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
	"context"
	"log"

	"k8s.io/api/core/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	coreinformers "k8s.io/client-go/informers/core/v1"
	extensionsinformers "k8s.io/client-go/informers/extensions/v1beta1"
	corelisters "k8s.io/client-go/listers/core/v1"
	extensionslisters "k8s.io/client-go/listers/extensions/v1beta1"
)

type backendIngressControllerManager struct {
	*genericController

	secretsLister          corelisters.SecretLister
	routingServiceClient   coreclient.ServicesGetter
	routingServiceLister   corelisters.ServiceLister
	routingEndpointsClient coreclient.EndpointsGetter
	routingEndpointsLister corelisters.EndpointsLister
	routingIngressLister   extensionslisters.IngressLister
	routingNamespaceClient coreclient.NamespacesGetter

	backendIngressControllers map[string]*backendIngressController
	cancels                   map[string]func()
}

func newBackendIngressControllerManager(
	secretsInformer coreinformers.SecretInformer,
	routingServiceClient coreclient.ServicesGetter,
	routingServiceInformer coreinformers.ServiceInformer,
	routingEndpointsClient coreclient.EndpointsGetter,
	routingEndpointsInformer coreinformers.EndpointsInformer,
	routingIngressInformer extensionsinformers.IngressInformer,
	routingNamespaceClient coreclient.NamespacesGetter,
) *backendIngressControllerManager {
	m := &backendIngressControllerManager{
		genericController: newGenericController("backends"),

		secretsLister:          secretsInformer.Lister(),
		routingServiceClient:   routingServiceClient,
		routingServiceLister:   routingServiceInformer.Lister(),
		routingEndpointsClient: routingEndpointsClient,
		routingEndpointsLister: routingEndpointsInformer.Lister(),
		routingIngressLister:   routingIngressInformer.Lister(),
		routingNamespaceClient: routingNamespaceClient,

		backendIngressControllers: make(map[string]*backendIngressController),
		cancels:                   make(map[string]func()),
	}
	m.syncHandler = m.processSecret

	secretsInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				secret := obj.(*v1.Secret)
				return secret.Labels[backendLabel] == "true"
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    m.enqueue,
				UpdateFunc: func(_, obj interface{}) { m.enqueue(obj) },
				DeleteFunc: m.enqueue,
			},
		},
	)

	return m
}

func (m *backendIngressControllerManager) processSecret(key string) error {
	log.Println("backendIngressControllerManager.processSecret start")
	defer log.Println("backendIngressControllerManager.processSecret end")

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		m.log("error splitting key %s: %v", key, err)
		return nil
	}

	secret, err := m.secretsLister.Secrets(ns).Get(name)
	if apierrors.IsNotFound(err) {
		m.log("Couldn't find %s/%s - stopping backend ingress controller for it", ns, name)
		m.log("canceling backendIngressController %s", name)
		cancel := m.cancels[name]
		cancel()

		m.log("removing backendIngressController %s", name)
		delete(m.cancels, name)
		delete(m.backendIngressControllers, name)
	}
	if err != nil {
		return err
	}

	if _, exists := m.backendIngressControllers[name]; !exists {
		ctx, cancel := context.WithCancel(context.Background())
		m.cancels[name] = cancel

		be := &backend{
			name:       name,
			ip:         string(secret.Data["ip"]),
			kubeconfig: string(secret.Data["kubeconfig"]),
		}

		c := newBackendIngressController(
			be,
			m.routingServiceClient,
			m.routingServiceLister,
			m.routingEndpointsClient,
			m.routingEndpointsLister,
			m.routingIngressLister,
			m.routingNamespaceClient,
		)
		m.backendIngressControllers[name] = c

		go c.Run(ctx, 1)
	}

	return nil

	// namesToKeep := sets.NewString()

	// for _, be := range backends {
	// 	namesToKeep.Insert(be.name)

	// }

	// log.Println("namesToKeep", namesToKeep)

	// allNames := sets.StringKeySet(m.backendIngressControllers)
	// log.Println("allNames", allNames)

	// namesToDelete := allNames.Difference(namesToKeep)
	// log.Println("namesToDelete", namesToDelete)

	// for _, name := range namesToDelete.List() {

	// }
}
