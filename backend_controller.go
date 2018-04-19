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
	"log"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	extensionsinformers "k8s.io/client-go/informers/extensions/v1beta1"
	"k8s.io/client-go/kubernetes"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	extensionslisters "k8s.io/client-go/listers/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type backendController struct {
	*genericController

	backend              *backend
	backendClient        kubernetes.Interface
	backendServiceLister corelisters.ServiceLister
	backendIngressLister extensionslisters.IngressLister

	routingEndpointsClient coreclient.EndpointsGetter
	routingIngressLister   extensionslisters.IngressLister

	ready chan struct{}
}

func newBackendController(
	backend *backend,
	routingEndpointsClient coreclient.EndpointsGetter,
	routingIngressInformer extensionsinformers.IngressInformer,
) *backendController {
	getter := func() (*clientcmdapi.Config, error) {
		return clientcmd.Load([]byte(backend.kubeconfig))
	}
	clientConfig, err := clientcmd.BuildConfigFromKubeconfigGetter("", getter)
	if err != nil {
		panic(err)
	}
	backendClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		panic(err)
	}

	backendInformers := informers.NewSharedInformerFactory(backendClient, 0)

	c := &backendController{
		genericController: newGenericController("backend-handler"),

		backend:              backend,
		backendClient:        backendClient,
		backendServiceLister: backendInformers.Core().V1().Services().Lister(),
		backendIngressLister: backendInformers.Extensions().V1beta1().Ingresses().Lister(),

		routingEndpointsClient: routingEndpointsClient,
		routingIngressLister:   routingIngressInformer.Lister(),

		ready: make(chan struct{}),
	}
	c.syncHandler = c.processBackendService
	c.stopHandler = c.onStop

	backendInformers.Core().V1().Services().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueue,
			UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
			DeleteFunc: c.enqueue,
		},
	)

	backendInformers.Extensions().V1beta1().Ingresses().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueue,
			UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
			DeleteFunc: c.enqueue,
		},
	)

	routingIngressInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueue,
			UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
			DeleteFunc: c.enqueue,
		},
	)

	c.additionalGoroutines = append(c.additionalGoroutines, func(done <-chan struct{}) {
		c.log("Starting backend informers")
		backendInformers.Start(done)
		c.log("Waiting for cache sync for backend informers")
		backendInformers.WaitForCacheSync(done)
		c.log("Caches are synced for backend informers")
		close(c.ready)
	})

	return c
}

func (c *backendController) log(msg string, args ...interface{}) {
	log.Printf("["+c.backend.name+"] "+msg+"\n", args...)
}

func (c *backendController) processBackendService(key string) error {
	c.log("processBackendService for %s", key)
	c.log("waiting for ready")
	<-c.ready
	c.log("ready")

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.log("error splitting key %q: %v", key, err)
		return nil
	}

	c.log("Checking for routing ingress for %s/%s", ns, name)
	_, err = c.routingIngressLister.Ingresses(ns).Get(name)
	if apierrors.IsNotFound(err) {
		c.log("No routing ingress for %s/%s, skipping", ns, name)
		return nil
	}
	if err != nil {
		c.log("Error getting routing ingress %s: %v", name, err)
		return err
	}

	missingBackendIngress := false
	missingBackendService := false

	c.log("Checking for backend ingress for %s/%s", ns, name)
	_, err = c.backendIngressLister.Ingresses(ns).Get(name)
	if apierrors.IsNotFound(err) {
		c.log("No backend ingress for %s/%s", ns, name)
		missingBackendIngress = true
	} else if err != nil {
		c.log("Error getting backend ingress %s: %v", name, err)
		return err
	}

	c.log("Getting backend service %s/%s", ns, name)
	_, err = c.backendServiceLister.Services(ns).Get(name)
	if apierrors.IsNotFound(err) {
		c.log("No backend service for %s/%s", ns, name)
		missingBackendService = true
	} else if err != nil {
		c.log("error getting backend service: %v", err)
		return err
	}

	if missingBackendIngress || missingBackendService {
		c.log("Backend ingress or service missing - trying to remove related endpoint subset address %s/%s %s", ns, name, c.backend.ip)
		err = c.deleteEndpointAddressForBackend(ns, name)
		return err
	}

	c.log("found backend service - checking if we need to create/update endpoints")
	endpoints, err := c.routingEndpointsClient.Endpoints(ns).Get(name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.log("couldn't find endpoints for %s/%s", ns, name)
		// create endpoints
		endpoints = &v1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      name,
			},
			Subsets: []v1.EndpointSubset{
				subsetForIP(c.backend.ip),
			},
		}

		c.log("creating endpoints for %s/%s", ns, name)
		_, err = c.routingEndpointsClient.Endpoints(ns).Create(endpoints)
		return err
	}
	if err != nil {
		c.log("error getting endpoints %s/%s: %v", ns, name, err)
		return err
	}

	c.log("found endpoints for %s/%s - checking if we need to add an address for %s:%s", ns, name, c.backend.name, c.backend.ip)
	found := false
	if len(endpoints.Subsets) > 0 {
		subset := endpoints.Subsets[0]
		for _, address := range subset.Addresses {
			if address.IP == c.backend.ip {
				c.log("address found for %s/%s %s:%s", ns, name, c.backend.name, c.backend.ip)
				found = true
				break
			}
		}
	}

	if !found {
		if len(endpoints.Subsets) == 0 {
			endpoints.Subsets = append(endpoints.Subsets, subsetForIP(c.backend.ip))
		} else {
			c.log("need to add address for %s/%s %s:%s", ns, name, c.backend.name, c.backend.ip)
			endpoints.Subsets[0].Addresses = append(endpoints.Subsets[0].Addresses, v1.EndpointAddress{IP: c.backend.ip})
		}
		_, err = c.routingEndpointsClient.Endpoints(ns).Update(endpoints)
		return err
	}

	return nil
}

func (c *backendController) onStop() {
	c.log("onStop begin")

	routingIngresses, err := c.routingIngressLister.List(labels.Everything())
	if err != nil {
		c.log("error listing routing ingresses: %v", err)
		return
	}

	for _, routingIngress := range routingIngresses {
		c.log("evaluating routing ingress %s/%s", routingIngress.Namespace, routingIngress.Name)

		// if there's not a corresponding backend ingress, skip
		_, err := c.backendIngressLister.Ingresses(routingIngress.Namespace).Get(routingIngress.Name)
		if apierrors.IsNotFound(err) {
			c.log("no backend ingress %s/%s - skipping", routingIngress.Namespace, routingIngress.Name)
			continue
		}
		if err != nil {
			c.log("error getting backend ingress %s/%s: %v", routingIngress.Namespace, routingIngress.Name, err)
			continue
		}

		c.log("trying to delete %s from endpoints %s/%s", c.backend.ip, routingIngress.Namespace, routingIngress.Name)
		err = c.deleteEndpointAddressForBackend(routingIngress.Namespace, routingIngress.Name)
		if err != nil {
			c.log("error deleting %s from endpoints %s/%s: %v", c.backend.ip, routingIngress.Namespace, routingIngress.Name, err)
		}
	}
}

func (c *backendController) deleteEndpointAddressForBackend(ns, name string) error {
	endpoints, err := c.routingEndpointsClient.Endpoints(ns).Get(name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		c.log("Endpoints for %s/%s don't exist - no-op", ns, name)
		return nil
	}
	if err != nil {
		c.log("error getting endpoints for %s/%s", ns, name)
		return err
	}
	if len(endpoints.Subsets) == 0 {
		c.log("no subsets for %s/%s - no-op", ns, name)
		return nil
	}

	var newAddresses []v1.EndpointAddress

	subset := &endpoints.Subsets[0]
	for i := range subset.Addresses {
		address := subset.Addresses[i]
		c.log("checking subset with ip %s", address.IP)
		if address.IP == c.backend.ip {
			c.log("found the one we want to remove")
		} else {
			c.log("keeping %s", address.IP)
			newAddresses = append(newAddresses, address)
		}
	}
	subset.Addresses = newAddresses
	c.log("updating subset to %#v", subset)

	_, err = c.routingEndpointsClient.Endpoints(ns).Update(endpoints)
	return nil
}

func subsetForIP(ip string) v1.EndpointSubset {
	return v1.EndpointSubset{
		Addresses: []v1.EndpointAddress{
			{
				IP: ip,
			},
		},
		Ports: []v1.EndpointPort{
			{
				Name:     "http",
				Port:     80,
				Protocol: "TCP",
			},
		},
	}
}
