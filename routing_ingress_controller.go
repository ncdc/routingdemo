/*
Copyright 2018 Heptio.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"reflect"

	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	extensionsclient "k8s.io/client-go/kubernetes/typed/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"

	coreinformers "k8s.io/client-go/informers/core/v1"
	extensionsinformers "k8s.io/client-go/informers/extensions/v1beta1"
	corelisters "k8s.io/client-go/listers/core/v1"
	extensionslisters "k8s.io/client-go/listers/extensions/v1beta1"
)

type routingIngressController struct {
	*genericController

	routingServiceLister corelisters.ServiceLister
	routingIngressClient extensionsclient.IngressesGetter
	routingIngressLister extensionslisters.IngressLister
}

func newRoutingIngressController(
	routingServiceInformer coreinformers.ServiceInformer,
	routingIngressClient extensionsclient.IngressesGetter,
	routingIngressInformer extensionsinformers.IngressInformer,
) *routingIngressController {
	c := &routingIngressController{
		genericController: newGenericController("routing-ingress"),

		routingServiceLister: routingServiceInformer.Lister(),
		routingIngressClient: routingIngressClient,
		routingIngressLister: routingIngressInformer.Lister(),
	}

	c.syncHandler = c.processIngress

	routingServiceInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(_ interface{}) { c.resync() },
			UpdateFunc: func(_, _ interface{}) { c.resync() },
			DeleteFunc: func(_ interface{}) { c.resync() },
		},
	)

	routingIngressInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueue,
			UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
			DeleteFunc: c.enqueue,
		},
	)

	return c
}

func (c *routingIngressController) resync() {
	ingresses, err := c.routingIngressLister.List(labels.Everything())
	if err != nil {
		c.log("Error resyncing: %v", err)
		return
	}
	for i := range ingresses {
		c.enqueue(ingresses[i])
	}
}

func (c *routingIngressController) processIngress(key string) error {
	c.log("routingIngressController.processIngress start for key %s", key)
	defer c.log("routingIngressController.processIngress end for key %s", key)

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.log("error splitting key %s: %v", key, err)
		return nil
	}

	ingress, err := c.routingIngressLister.Ingresses(ns).Get(name)
	if apierrors.IsNotFound(err) {
		c.log("Couldn't find ingress %s, must have been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	// var observedGeneration int64
	// s := ingress.Annotations["observedGeneration"]
	// if s != "" {
	// 	i, err := strconv.ParseInt(s, 10, 64)
	// 	if err != nil {
	// 		c.log("Error parsing observedGeneration %s: %v", s, err)
	// 	} else {
	// 		observedGeneration = i
	// 	}
	// }
	// if observedGeneration == ingress.Generation {
	// 	c.log("observedGeneration %d == generation %d for %s", observedGeneration, ingress.Generation, key)
	// 	return nil
	// }
	original := ingress
	ingress = ingress.DeepCopy()

	//ingress.Annotations["observedGeneration"] = strconv.FormatInt(ingress.Generation, 10)

	if len(ingress.Spec.Rules) == 0 {
		c.log("ingress %s has 0 rules", key)
		return nil
	}

	rule := &ingress.Spec.Rules[0]
	if rule.Host == "" {
		c.log("ingress %s rule has no host", key)
		return nil
	}
	if rule.HTTP == nil {
		c.log("ingress %s rule has no http", key)
		return nil
	}
	vhost := rule.Host

	services, err := c.routingServiceLister.Services(ns).List(labels.SelectorFromSet(labels.Set{vhostLabel: vhost}))
	if err != nil {
		c.log("error listing services for vhost %s: %v", vhost, err)
		return err
	}
	if len(services) == 0 {
		c.log("No services matching %s=%s for ingress %s", vhostLabel, vhost, key)
		return nil
	}

	var paths []v1beta1.HTTPIngressPath
	for _, service := range services {
		path := v1beta1.HTTPIngressPath{
			Path: "/",
			Backend: v1beta1.IngressBackend{
				ServiceName: service.Name,
				ServicePort: intstr.FromInt(8080),
			},
		}

		paths = append(paths, path)
	}

	rule.HTTP.Paths = paths

	if reflect.DeepEqual(original.Spec, ingress.Spec) && reflect.DeepEqual(original.Annotations, ingress.Annotations) {
		c.log("No changes for routing ingress %s", key)
		return nil
	}

	c.log("Trying to update ingress %s to %#v", key, ingress)

	_, err = c.routingIngressClient.Ingresses(ns).Update(ingress)

	return err
}
