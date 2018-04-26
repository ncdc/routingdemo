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
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"

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

	if ingress.Annotations["weightedCluster"] != "true" {
		c.log("%s isn't a weightedCluster ingress", key)
		return nil
	}

	original := ingress
	ingress = ingress.DeepCopy()

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

	serviceNames := sets.NewString()

	if len(services) == 0 {
		c.log("No services matching %s=%s for ingress %s", vhostLabel, vhost, key)

		for k := range ingress.Annotations {
			if strings.HasPrefix(k, "weight.") {
				delete(ingress.Annotations, k)
			}
		}

		rule.HTTP.Paths = []v1beta1.HTTPIngressPath{
			{
				Path: "/",
				Backend: v1beta1.IngressBackend{
					ServiceName: "temporary-placeholder",
					ServicePort: intstr.FromInt(8080),
				},
			},
		}
	} else {
		c.log("Found %d services matching %s=%s for ingress %s", len(services), vhostLabel, vhost, key)
		haveWeights := false
		for _, service := range services {
			if ingress.Annotations["weight."+service.Name] != "" {
				haveWeights = true
				break
			}
		}

		var paths []v1beta1.HTTPIngressPath
		for i, service := range services {
			c.log("Found routable service %s/%s", service.Namespace, service.Name)
			serviceNames.Insert(service.Name)

			if !haveWeights && i == 0 {
				ingress.Annotations["weight."+service.Name] = "100"
			}
			if ingress.Annotations["weight."+service.Name] == "" {
				ingress.Annotations["weight."+service.Name] = "0"
			}
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
	}

	for k := range ingress.Annotations {
		if strings.HasPrefix(k, "weight.") {
			parts := strings.Split(k, ".")
			if !serviceNames.Has(parts[1]) {
				c.log("Removing weight annotation %s", k)
				delete(ingress.Annotations, k)
			}
		}
	}

	if reflect.DeepEqual(original.Spec, ingress.Spec) && reflect.DeepEqual(original.Annotations, ingress.Annotations) {
		c.log("No changes for routing ingress %s", key)
		return nil
	}

	if len(services) > 0 {
		// see if we need to adjust any weights
		c.log("Checking if we need to adjust weights")
		total := 0
		firstWeight := 0
		for i, service := range services {
			weight, err := strconv.Atoi(ingress.Annotations["weight."+service.Name])
			if err != nil {
				c.log("error parsing %s: %v", ingress.Annotations["weight."+service.Name], err)
				return nil
			}
			total += weight
			if i == 0 {
				firstWeight = weight
			}
		}
		remainder := 100 - total
		if remainder > 0 {
			firstWeight += remainder
			c.log("Adjusting weight for %s from %d to %d", services[0].Name, firstWeight-remainder, firstWeight)
			ingress.Annotations["weight."+services[0].Name] = fmt.Sprintf("%d", firstWeight)
		}
	}

	c.log("Trying to update ingress %s to %#v", key, ingress)

	oldData, err := json.Marshal(original)
	if err != nil {
		c.log("Error converting original to json: %v", err)
		return nil
	}

	newData, err := json.Marshal(ingress)
	if err != nil {
		c.log("Error converting updated to json: %v", err)
		return nil
	}

	patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
	if err != nil {
		c.log("Error creating patch: %v", err)
		return nil
	}

	_, err = c.routingIngressClient.Ingresses(ns).Patch(ingress.Name, types.MergePatchType, patchBytes)

	return err
}
