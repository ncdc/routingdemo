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

	routingEndpointsClient coreclient.EndpointsGetter
	routingIngressLister   extensionslisters.IngressLister

	backendClient        kubernetes.Interface
	backendServiceLister corelisters.ServiceLister

	ip string
}

func newBackendController(
	name, ip string,
	routingEndpointsClient coreclient.EndpointsGetter,
	routingIngressInformer extensionsinformers.IngressInformer,
	backendKubeconfig string,
) *backendController {
	getter := func() (*clientcmdapi.Config, error) {
		return clientcmd.Load([]byte(backendKubeconfig))
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

		routingEndpointsClient: routingEndpointsClient,
		routingIngressLister:   routingIngressInformer.Lister(),

		backendClient:        backendClient,
		backendServiceLister: backendInformers.Core().V1().Services().Lister(),

		ip: ip,
	}
	c.syncHandler = c.processBackendService

	backendInformers.Core().V1().Services().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueue,
			UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
			DeleteFunc: c.enqueue,
		},
	)

	c.additionalGoroutines = append(c.additionalGoroutines, func(done <-chan struct{}) {
		log.Println("Starting backend informers for", name)
		backendInformers.Start(done)
		log.Println("Waiting for cache sync for backend informers for", name)
		backendInformers.WaitForCacheSync(done)
		log.Println("Caches are synced for backend informers for", name)
	})

	return c
}

func (c *backendController) processBackendService(key string) error {
	log.Printf("processBackendService for %s\n", key)

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		log.Printf("error splitting key %q: %v\n", key, err)
		return nil
	}

	log.Println("Getting backend service", name)
	_, err = c.backendServiceLister.Services(ns).Get(name)
	if apierrors.IsNotFound(err) {
		log.Println("Couldn't find backend service, trying to remove related endpoint subset", name)
		endpoints, err := c.routingEndpointsClient.Endpoints(routingNamespace).Get(name, metav1.GetOptions{})
		if err != nil {
			log.Println("error getting endpoints for", name)
			return err
		}

		var newSubsets []v1.EndpointSubset
		for _, subset := range endpoints.Subsets {
			found := false
			for _, address := range subset.Addresses {
				log.Println("checking subset with ip", address.IP)
				if address.IP == c.ip {
					log.Println("found the one we want to remove")
					found = true
				}
			}
			if !found {
				newSubsets = append(newSubsets, subset)
			}
		}

		log.Printf("updating subsets to %#v\n", newSubsets)

		endpoints.Subsets = newSubsets
		_, err = c.routingEndpointsClient.Endpoints(routingNamespace).Update(endpoints)
		return err
	}
	if err != nil {
		log.Println("error getting backend service:", err)
	}

	log.Println("found backend service - checking if we need to create/update endpoints")
	endpoints, err := c.routingEndpointsClient.Endpoints(routingNamespace).Get(name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		log.Println("couldn't find endpoints for", name)
		// create endpoints
		endpoints = &v1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      name,
			},
			Subsets: []v1.EndpointSubset{
				subsetForIP(c.ip),
			},
		}

		log.Println("creating endpoints for", name)
		_, err = c.routingEndpointsClient.Endpoints(routingNamespace).Create(endpoints)
		return err
	}
	if err != nil {
		log.Printf("error getting endpoints %s: %v\n", name, err)
		return err
	}

	log.Println("found endpoints - checking if we need to add a subset")
	found := false
Loop:
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.IP == c.ip {
				log.Println("subset found")
				found = true
				break Loop
			}
		}
	}

	if !found {
		log.Println("need to add subset")
		endpoints.Subsets = append(endpoints.Subsets, subsetForIP(c.ip))
		_, err = c.routingEndpointsClient.Endpoints(routingNamespace).Update(endpoints)
		return err
	}

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
