// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ingress provides a read-only view of Kubernetes ingress resources
// as an ingress rule configuration type store
package ingress

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/go-multierror"
	ingress "k8s.io/api/networking/v1beta1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/informers/networking/v1beta1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	networkinglister "k8s.io/client-go/listers/networking/v1beta1"
	"k8s.io/client-go/tools/cache"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	kubecontroller "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/schema/collection"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/pkg/env"
	"istio.io/pkg/log"
)

// In 1.0, the Gateway is defined in the namespace where the actual controller runs, and needs to be managed by
// user.
// The gateway is named by appending "-istio-autogenerated-k8s-ingress" to the name of the ingress.
//
// Currently the gateway namespace is hardcoded to istio-system (model.IstioIngressNamespace)
//
// VirtualServices are also auto-generated in the model.IstioIngressNamespace.
//
// The sync of Ingress objects to IP is done by status.go
// the 'ingress service' name is used to get the IP of the Service
// If ingress service is empty, it falls back to NodeExternalIP list, selected using the labels.
// This is using 'namespace' of pilot - but seems to be broken (never worked), since it uses Pilot's pod labels
// instead of the ingress labels.

// Follows mesh.IngressControllerMode setting to enable - OFF|STRICT|DEFAULT.
// STRICT requires "kubernetes.io/ingress.class" == mesh.IngressClass
// DEFAULT allows Ingress without explicit class.

// In 1.1:
// - K8S_INGRESS_NS - namespace of the Gateway that will act as ingress.
// - labels of the gateway set to "app=ingressgateway" for node_port, service set to 'ingressgateway' (matching default install)
//   If we need more flexibility - we can add it (but likely we'll deprecate ingress support first)
// -

var schemas = collection.SchemasFor(
	collections.IstioNetworkingV1Alpha3Virtualservices,
	collections.IstioNetworkingV1Alpha3Gateways)

// Control needs RBAC permissions to write to Pods.

type controller struct {
	meshWatcher  mesh.Holder
	domainSuffix string

	queue                  controllers.Queue
	virtualServiceHandlers []model.EventHandler
	gatewayHandlers        []model.EventHandler

	mutex sync.RWMutex
	// processed ingresses
	ingresses map[types.NamespacedName]*ingress.Ingress

	ingressInformer cache.SharedInformer
	ingressLister   networkinglister.IngressLister
	serviceInformer cache.SharedInformer
	serviceLister   listerv1.ServiceLister
	// May be nil if ingress class is not supported in the cluster
	classes v1beta1.IngressClassInformer

	started atomic.Bool
}

// TODO: move to features ( and remove in 1.2 )
var ingressNamespace = env.RegisterStringVar("K8S_INGRESS_NS", "", "").Get()

var errUnsupportedOp = errors.New("unsupported operation: the ingress config store is a read-only view")

// Check if the "networking/v1" Ingress is available. Implementation borrowed from ingress-nginx
func V1Available(client kube.Client) bool {
	// check kubernetes version to use new ingress package or not
	version119, _ := version.ParseGeneric("v1.19.0")

	serverVersion, err := client.GetKubernetesVersion()
	if err != nil {
		return false
	}

	runningVersion, err := version.ParseGeneric(serverVersion.String())
	if err != nil {
		log.Errorf("unexpected error parsing running Kubernetes version: %v", err)
		return false
	}

	return runningVersion.AtLeast(version119)
}

// Check if the "networking" group Ingress is available. Implementation borrowed from ingress-nginx
func NetworkingIngressAvailable(client kube.Client) bool {
	// check kubernetes version to use new ingress package or not
	version118, _ := version.ParseGeneric("v1.18.0")

	serverVersion, err := client.GetKubernetesVersion()
	if err != nil {
		return false
	}

	runningVersion, err := version.ParseGeneric(serverVersion.String())
	if err != nil {
		log.Errorf("unexpected error parsing running Kubernetes version: %v", err)
		return false
	}

	return runningVersion.AtLeast(version118)
}

// NewController creates a new Kubernetes controller
func NewController(client kube.Client, meshWatcher mesh.Holder,
	options kubecontroller.Options,
) model.ConfigStoreController {
	if ingressNamespace == "" {
		ingressNamespace = constants.IstioIngressNamespace
	}

	ingressInformer := client.KubeInformer().Networking().V1beta1().Ingresses()
	_ = ingressInformer.Informer().SetTransform(kube.StripUnusedFields)
	serviceInformer := client.KubeInformer().Core().V1().Services()

	var classes v1beta1.IngressClassInformer
	if NetworkingIngressAvailable(client) {
		classes = client.KubeInformer().Networking().V1beta1().IngressClasses()
		// Register the informer now, so it will be properly started
		_ = classes.Informer().SetTransform(kube.StripUnusedFields)
	} else {
		log.Infof("Skipping IngressClass, resource not supported")
	}

	c := &controller{
		meshWatcher:     meshWatcher,
		domainSuffix:    options.DomainSuffix,
		ingresses:       make(map[types.NamespacedName]*ingress.Ingress),
		ingressInformer: ingressInformer.Informer(),
		ingressLister:   ingressInformer.Lister(),
		classes:         classes,
		serviceInformer: serviceInformer.Informer(),
		serviceLister:   serviceInformer.Lister(),
	}

	c.queue = controllers.NewQueue("ingress",
		controllers.WithReconciler(c.onEvent),
		controllers.WithMaxAttempts(5))
	c.ingressInformer.AddEventHandler(controllers.ObjectHandler(c.queue.AddObject))

	return c
}

func (c *controller) HasStarted() bool {
	return c.started.Load()
}

func (c *controller) Run(stop <-chan struct{}) {
	c.started.Store(true)
	c.queue.Run(stop)
}

func (c *controller) shouldProcessIngress(mesh *meshconfig.MeshConfig, i *ingress.Ingress) (bool, error) {
	var class *ingress.IngressClass
	if c.classes != nil && i.Spec.IngressClassName != nil {
		c, err := c.classes.Lister().Get(*i.Spec.IngressClassName)
		if err != nil && !kerrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to get ingress class %v: %v", i.Spec.IngressClassName, err)
		}
		class = c
	}
	return shouldProcessIngressWithClass(mesh, i, class), nil
}

// shouldProcessIngressUpdate checks whether we should renotify registered handlers about an update event
func (c *controller) shouldProcessIngressUpdate(ing *ingress.Ingress) (bool, error) {
	shouldProcess, err := c.shouldProcessIngress(c.meshWatcher.Mesh(), ing)
	if err != nil {
		return false, err
	}
	item := types.NamespacedName{Name: ing.Name, Namespace: ing.Namespace}
	if shouldProcess {
		// record processed ingress
		c.mutex.Lock()
		c.ingresses[item] = ing
		c.mutex.Unlock()
		return true, nil
	}

	c.mutex.Lock()
	_, preProcessed := c.ingresses[item]
	// previous processed but should not currently, delete it
	if preProcessed && !shouldProcess {
		delete(c.ingresses, item)
	} else {
		c.ingresses[item] = ing
	}
	c.mutex.Unlock()

	return preProcessed, nil
}

func (c *controller) onEvent(item types.NamespacedName) error {
	event := model.EventUpdate
	ing, err := c.ingressLister.Ingresses(item.Namespace).Get(item.Name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			event = model.EventDelete
			c.mutex.Lock()
			ing = c.ingresses[item]
			delete(c.ingresses, item)
			c.mutex.Unlock()
		} else {
			return err
		}
	}

	// ingress deleted, and it is not processed before
	if ing == nil {
		return nil
	}
	// we should check need process only when event is not delete,
	// if it is delete event, and previously processed, we need to process too.
	if event != model.EventDelete {
		shouldProcess, err := c.shouldProcessIngressUpdate(ing)
		if err != nil {
			return err
		}
		if !shouldProcess {
			return nil
		}
	}

	vsmetadata := config.Meta{
		Name:             item.Name + "-" + "virtualservice",
		Namespace:        item.Namespace,
		GroupVersionKind: gvk.VirtualService,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}
	gatewaymetadata := config.Meta{
		Name:             item.Name + "-" + "gateway",
		Namespace:        item.Namespace,
		GroupVersionKind: gvk.Gateway,
		// Set this label so that we do not compare configs and just push.
		Labels: map[string]string{constants.AlwaysPushLabel: "true"},
	}

	// Trigger updates for Gateway and VirtualService
	// TODO: we could be smarter here and only trigger when real changes were found
	for _, f := range c.virtualServiceHandlers {
		f(config.Config{Meta: vsmetadata}, config.Config{Meta: vsmetadata}, event)
	}
	for _, f := range c.gatewayHandlers {
		f(config.Config{Meta: gatewaymetadata}, config.Config{Meta: gatewaymetadata}, event)
	}

	return nil
}

func (c *controller) RegisterEventHandler(kind config.GroupVersionKind, f model.EventHandler) {
	switch kind {
	case gvk.VirtualService:
		c.virtualServiceHandlers = append(c.virtualServiceHandlers, f)
	case gvk.Gateway:
		c.gatewayHandlers = append(c.gatewayHandlers, f)
	}
}

func (c *controller) SetWatchErrorHandler(handler func(r *cache.Reflector, err error)) error {
	var errs error
	if err := c.serviceInformer.SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(err, errs)
	}
	if err := c.ingressInformer.SetWatchErrorHandler(handler); err != nil {
		errs = multierror.Append(err, errs)
	}
	if c.classes != nil {
		if err := c.classes.Informer().SetWatchErrorHandler(handler); err != nil {
			errs = multierror.Append(err, errs)
		}
	}
	return errs
}

func (c *controller) HasSynced() bool {
	// TODO: add c.queue.HasSynced() once #36332 is ready, ensuring Run is called before HasSynced
	return c.ingressInformer.HasSynced() && c.serviceInformer.HasSynced() &&
		(c.classes == nil || c.classes.Informer().HasSynced())
}

func (c *controller) Schemas() collection.Schemas {
	// TODO: are these two config descriptors right?
	return schemas
}

func (c *controller) Get(typ config.GroupVersionKind, name, namespace string) *config.Config {
	return nil
}

// sortIngressByCreationTime sorts the list of config objects in ascending order by their creation time (if available).
func sortIngressByCreationTime(configs []any) []*ingress.Ingress {
	ingr := make([]*ingress.Ingress, 0, len(configs))
	for _, i := range configs {
		ingr = append(ingr, i.(*ingress.Ingress))
	}
	sort.Slice(ingr, func(i, j int) bool {
		// If creation time is the same, then behavior is nondeterministic. In this case, we can
		// pick an arbitrary but consistent ordering based on name and namespace, which is unique.
		// CreationTimestamp is stored in seconds, so this is not uncommon.
		if ingr[i].CreationTimestamp == ingr[j].CreationTimestamp {
			in := ingr[i].Name + "." + ingr[i].Namespace
			jn := ingr[j].Name + "." + ingr[j].Namespace
			return in < jn
		}
		return ingr[i].CreationTimestamp.Before(&ingr[j].CreationTimestamp)
	})
	return ingr
}

func (c *controller) List(typ config.GroupVersionKind, namespace string) ([]config.Config, error) {
	if typ != gvk.Gateway &&
		typ != gvk.VirtualService {
		return nil, errUnsupportedOp
	}

	out := make([]config.Config, 0)

	ingressByHost := map[string]*config.Config{}

	for _, ingress := range sortIngressByCreationTime(c.ingressInformer.GetStore().List()) {
		if namespace != "" && namespace != ingress.Namespace {
			continue
		}
		process, err := c.shouldProcessIngress(c.meshWatcher.Mesh(), ingress)
		if err != nil {
			return nil, err
		}
		if !process {
			continue
		}

		switch typ {
		case gvk.VirtualService:
			ConvertIngressVirtualService(*ingress, c.domainSuffix, ingressByHost, c.serviceLister)
		case gvk.Gateway:
			gateways := ConvertIngressV1alpha3(*ingress, c.meshWatcher.Mesh(), c.domainSuffix)
			out = append(out, gateways)
		}
	}

	if typ == gvk.VirtualService {
		for _, obj := range ingressByHost {
			out = append(out, *obj)
		}
	}

	return out, nil
}

func (c *controller) Create(_ config.Config) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Update(_ config.Config) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) UpdateStatus(config.Config) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Patch(_ config.Config, _ config.PatchFunc) (string, error) {
	return "", errUnsupportedOp
}

func (c *controller) Delete(_ config.GroupVersionKind, _, _ string, _ *string) error {
	return errUnsupportedOp
}
