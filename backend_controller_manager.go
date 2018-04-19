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

	"k8s.io/apimachinery/pkg/util/sets"
	extensionsinformers "k8s.io/client-go/informers/extensions/v1beta1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

type backendControllerManager struct {
	backends           <-chan []*backend
	backendControllers map[string]*backendController
	cancels            map[string]func()

	routingEndpointsClient coreclient.EndpointsGetter
	routingIngressInformer extensionsinformers.IngressInformer
}

func newBackendControllerManager(
	backends <-chan []*backend,
	routingEndpointsClient coreclient.EndpointsGetter,
	routingIngressInformer extensionsinformers.IngressInformer,
) *backendControllerManager {
	m := &backendControllerManager{
		backends:               backends,
		backendControllers:     make(map[string]*backendController),
		cancels:                make(map[string]func()),
		routingEndpointsClient: routingEndpointsClient,
		routingIngressInformer: routingIngressInformer,
	}
	// make sure the routing ingress informer gets started
	_ = routingIngressInformer.Informer()

	return m
}

func (m *backendControllerManager) run(ctx context.Context) {
	for {
		log.Println("backendControllerManager waiting for updates")
		select {
		case <-ctx.Done():
			return
		case backends := <-m.backends:
			m.handleUpdate(ctx, backends)
		}
	}
}

func (m *backendControllerManager) handleUpdate(parentCtx context.Context, backends []*backend) {
	log.Println("backendControllerManager.handleUpdate start")
	defer log.Println("backendControllerManager.handleUpdate end")

	namesToKeep := sets.NewString()

	for _, be := range backends {
		namesToKeep.Insert(be.name)

		if _, exists := m.backendControllers[be.name]; !exists {
			ctx, cancel := context.WithCancel(parentCtx)
			m.cancels[be.name] = cancel

			c := newBackendController(be, m.routingEndpointsClient, m.routingIngressInformer)
			m.backendControllers[be.name] = c

			go c.Run(ctx, 1)
		}
	}

	log.Println("namesToKeep", namesToKeep)

	allNames := sets.StringKeySet(m.backendControllers)
	log.Println("allNames", allNames)

	namesToDelete := allNames.Difference(namesToKeep)
	log.Println("namesToDelete", namesToDelete)

	for _, name := range namesToDelete.List() {
		log.Println("canceling backendController", name)
		cancel := m.cancels[name]
		cancel()

		log.Println("removing backendController", name)
		delete(m.cancels, name)
		delete(m.backendControllers, name)
	}
}
