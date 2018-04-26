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

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	extensionslisters "k8s.io/client-go/listers/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type backendIngressController struct {
	*genericController

	backend              *backend
	backendIngressLister extensionslisters.IngressLister

	routingServiceClient   coreclient.ServicesGetter
	routingServiceLister   corelisters.ServiceLister
	routingEndpointsClient coreclient.EndpointsGetter
	routingEndpointsLister corelisters.EndpointsLister
	routingIngressLister   extensionslisters.IngressLister
	routingNamespaceClient coreclient.NamespacesGetter

	ready chan struct{}
}

const ingressRouteLabel = "route"
const vhostLabel = "vhost"

func newBackendIngressController(
	backend *backend,
	routingServiceClient coreclient.ServicesGetter,
	routingServiceLister corelisters.ServiceLister,
	routingEndpointsClient coreclient.EndpointsGetter,
	routingEndpointsLister corelisters.EndpointsLister,
	routingIngressLister extensionslisters.IngressLister,
	routingNamespaceClient coreclient.NamespacesGetter,
) *backendIngressController {
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

	c := &backendIngressController{
		genericController: newGenericController("backend-handler"),

		backend:              backend,
		backendIngressLister: backendInformers.Extensions().V1beta1().Ingresses().Lister(),

		routingServiceClient:   routingServiceClient,
		routingServiceLister:   routingServiceLister,
		routingEndpointsClient: routingEndpointsClient,
		routingEndpointsLister: routingEndpointsLister,
		routingIngressLister:   routingIngressLister,
		routingNamespaceClient: routingNamespaceClient,

		ready: make(chan struct{}),
	}
	c.syncHandler = c.processBackendIngress
	c.stopHandler = c.onStop

	backendInformers.Extensions().V1beta1().Ingresses().Informer().AddEventHandler(
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

func (c *backendIngressController) log(msg string, args ...interface{}) {
	log.Printf("["+c.backend.name+"] "+msg+"\n", args...)
}

func nameWithBackend(name, backend string) string {
	return name + "-" + backend
}

func (c *backendIngressController) processBackendIngress(key string) error {
	c.log("processBackendIngress for %s", key)
	c.log("waiting for ready")
	<-c.ready
	c.log("ready")

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.log("error splitting key %q: %v", key, err)
		return nil
	}

	routingServiceName := nameWithBackend(name, c.backend.name)

	c.log("Getting backend ingress %s/%s", ns, name)
	backendIngress, err := c.backendIngressLister.Ingresses(ns).Get(name)
	if apierrors.IsNotFound(err) {
		c.log("NOT FOUND backend ingress %s/%s", ns, name)
		if err := c.deleteRoutingService(ns, routingServiceName); err != nil {
			return err
		}
		return c.deleteRoutingEndpoints(ns, routingServiceName)
	}
	if err != nil {
		return err
	}

	if backendIngress.Labels[ingressRouteLabel] != "true" {
		c.log("MISSING ROUTE LABEL backend ingress %s/%s", ns, name)
		if err := c.deleteRoutingService(ns, routingServiceName); err != nil {
			return err
		}
		return c.deleteRoutingEndpoints(ns, routingServiceName)
	}

	vhost := backendIngress.Labels[vhostLabel]
	if vhost == "" {
		c.log("No vhost for ingress %s/%s", ns, name)
		return nil
	}

	if err := c.ensureRoutingNamespace(ns); err != nil {
		c.log("Error ensuring namespace %s in routing cluster: %v", ns, err)
		return err
	}

	c.log("Checking for routing service %s/%s", ns, routingServiceName)
	_, err = c.routingServiceLister.Services(ns).Get(routingServiceName)
	if apierrors.IsNotFound(err) {
		c.log("NOT FOUND routing service %s/%s", ns, routingServiceName)
		if err := c.createRoutingService(ns, name, c.backend.name, vhost); err != nil {
			return err
		}
		return c.createRoutingEndpoints(ns, name, c.backend.name)
	}
	if err != nil {
		return err
	}

	c.log("FOUND routing service %s/%s", ns, routingServiceName)
	//c.reconcileRoutingService(ns, routingServiceName, routingService)

	c.log("Checking for routing endpoints %s/%s", ns, routingServiceName)
	_, err = c.routingEndpointsLister.Endpoints(ns).Get(routingServiceName)
	if apierrors.IsNotFound(err) {
		c.log("NOT FOUND routing endpoints %s/%s", ns, routingServiceName)
		return c.createRoutingEndpoints(ns, name, c.backend.name)
	}
	if err != nil {
		return err
	}

	c.log("FOUND routing endpoints %s/%s", ns, routingServiceName)
	//return c.reconcileRoutingEndpoints(ns, routingServiceName, routingEndpoints)
	return nil
}

func (c *backendIngressController) createRoutingService(ns, name, backend, vhost string) error {
	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      nameWithBackend(name, backend),
			Labels: map[string]string{
				vhostLabel:   vhost,
				backendLabel: backend,
			},
		},
		Spec: v1.ServiceSpec{
			Type:      v1.ServiceTypeClusterIP,
			ClusterIP: "None",
			Ports: []v1.ServicePort{
				{
					Name:       "http",
					Protocol:   "TCP",
					Port:       8080,
					TargetPort: intstr.FromInt(8080),
				},
			},
		},
	}
	_, err := c.routingServiceClient.Services(ns).Create(&service)
	return err
}

func (c *backendIngressController) deleteRoutingService(ns, name string) error {
	err := c.routingServiceClient.Services(ns).Delete(name, nil)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *backendIngressController) createRoutingEndpoints(ns, name, backend string) error {
	endpoints := v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      nameWithBackend(name, backend),
			Labels: map[string]string{
				backendLabel: backend,
			},
		},
		Subsets: []v1.EndpointSubset{
			subsetForIP(c.backend.ip),
		},
	}

	_, err := c.routingEndpointsClient.Endpoints(ns).Create(&endpoints)
	return err
}

func (c *backendIngressController) deleteRoutingEndpoints(ns, name string) error {
	err := c.routingEndpointsClient.Endpoints(ns).Delete(name, nil)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *backendIngressController) onStop() {
	c.log("onStop begin")
	c.log("onStop end")

	// delete all services labeled backend=x, which also deletes endpoints
	c.log("listing services for backend")
	services, err := c.routingServiceLister.List(labels.SelectorFromSet(labels.Set{backendLabel: c.backend.name}))
	if err != nil {
		c.log("onStop error listing services: %v", err)
	} else {
		for _, service := range services {
			c.log("deleting service %s/%s", service.Namespace, service.Name)
			if err = c.routingServiceClient.Services(service.Namespace).Delete(service.Name, nil); err != nil {
				c.log("onStop error deleting service %s/%s: %v", service.Namespace, service.Name, err)
			}
		}
	}
}

func (c *backendIngressController) ensureRoutingNamespace(ns string) error {
	_, err := c.routingNamespaceClient.Namespaces().Get(ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		n := v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: ns,
			},
		}
		_, err = c.routingNamespaceClient.Namespaces().Create(&n)
		return err
	}
	return err
}
