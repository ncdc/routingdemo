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
	"errors"
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
)

type zoneController struct {
	*genericController

	configMapsClient coreclient.ConfigMapsGetter
}

func newZoneController(
	configMapsClient coreclient.ConfigMapsGetter,
	configMapsInformer coreinformers.ConfigMapInformer,
) *zoneController {
	c := &zoneController{
		genericController: newGenericController("zone"),

		configMapsClient: configMapsClient,
	}

	c.syncHandler = c.rebuildZone

	configMapsInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueue,
			UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
			DeleteFunc: c.enqueue,
		},
	)

	return c
}

func (c *zoneController) rebuildZone(key string) error {
	/*
			x := `backend.            IN      SOA     ns.backend. andy.heptio.com. 2015082541 7200 3600 1209600 3600
		backend.            IN      NS      ns
		ns IN A 10.96.0.10
		*.cluster1.backend.          IN      A       10.211.55.9`
	*/
	log.Println("rebuildZone start")
	defer log.Println("rebuildZone done")

	log.Println("getting coredns-zones configmap")
	zonesCM, err := c.configMapsClient.ConfigMaps("kube-system").Get("coredns-zones", metav1.GetOptions{})
	if err != nil {
		log.Printf("error getting coredns-zones configmap: %v\n", err)
		return err
	}

	zone := zonesCM.Data["db.backend"]

	log.Println("parsing zone")
	token := <-dns.ParseZone(strings.NewReader(zone), "", "")
	if token.Error != nil {
		log.Printf("error parsing zone: %v\n", err)
		return token.Error
	}

	soa, ok := token.RR.(*dns.SOA)
	if !ok {
		msg := fmt.Sprintf("%T is not a *dns.SOA: %#v\n", token.RR, token.RR)
		log.Println(msg)
		return errors.New(msg)
	}

	soa.Serial++
	zone = soa.String() + "\n"

	log.Println("getting backend configmaps")
	list, err := c.configMapsClient.ConfigMaps(routingNamespace).List(metav1.ListOptions{})
	if err != nil {
		log.Printf("error listing backend configmaps: %v\n", err)
		return err
	}

	for _, be := range list.Items {
		log.Printf("adding %s=%s\n", be.Name, be.Data["ip"])
		a := dns.A{
			Hdr: dns.RR_Header{
				Name:   fmt.Sprintf("*.%s.backend.", be.Name),
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
			},
			A: net.ParseIP(be.Data["ip"]),
		}

		zone += a.String() + "\n"
	}

	zonesCM.Data["db.backend"] = zone

	log.Println("updating coredns-zones with")
	log.Println(zone)

	_, err = c.configMapsClient.ConfigMaps("kube-system").Update(zonesCM)
	if err != nil {
		log.Printf("error updating coredns-zones configmap: %v\n", err)
		return err
	}

	return nil
}
