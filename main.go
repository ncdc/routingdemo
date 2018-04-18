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
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"k8s.io/apimachinery/pkg/util/intstr"

	cli "gopkg.in/urfave/cli.v1"
	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const routingNamespace = "routing"

var routerKubeconfig string

func main() {
	app := cli.NewApp()

	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:        "kubeconfig",
			Destination: &routerKubeconfig,
		},
	}

	app.Commands = []cli.Command{
		{
			Name:   "add-backend",
			Action: addBackend,
		},
		{
			Name:   "delete-backend",
			Action: deleteBackend,
		},
		{
			Name:   "list-backends",
			Action: listBackends,
		},
		{
			Name:   "add-vhost",
			Action: addVhost,
		},
		{
			Name:   "delete-vhost",
			Action: deleteVhost,
		},
		{
			Name:   "list-vhosts",
			Action: listVhosts,
		},
		{
			Name:   "server",
			Action: runServer,
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatalln(err)
	}
}

func getClient(config string) (kubernetes.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = config
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	clientConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(clientConfig)
}

func addBackend(c *cli.Context) error {
	name := c.Args().Get(0)
	ip := c.Args().Get(1)
	kubeconfig := c.Args().Get(2)

	client, err := getClient(routerKubeconfig)
	if err != nil {
		return err
	}

	kubeconfigContents, err := ioutil.ReadFile(kubeconfig)
	if err != nil {
		return err
	}

	backend := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: routingNamespace,
			Name:      name,
		},
		Data: map[string]string{
			"ip":         ip,
			"kubeconfig": string(kubeconfigContents),
		},
	}

	_, err = client.CoreV1().ConfigMaps(routingNamespace).Create(&backend)

	return err
}

func deleteBackend(c *cli.Context) error {
	name := c.Args().Get(0)

	client, err := getClient(routerKubeconfig)
	if err != nil {
		return err
	}

	return client.CoreV1().ConfigMaps(routingNamespace).Delete(name, nil)
}

func listBackends(c *cli.Context) error {
	client, err := getClient(routerKubeconfig)
	if err != nil {
		return err
	}

	list, err := client.CoreV1().ConfigMaps(routingNamespace).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, be := range list.Items {
		fmt.Printf("%s: %s\n", be.Name, be.Data["ip"])
	}

	return nil
}

func addVhost(c *cli.Context) error {
	client, err := getClient(routerKubeconfig)
	if err != nil {
		return err
	}

	vhost := c.Args().Get(0)
	selector := c.Args().Get(1)

	ingress := v1beta1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: routingNamespace,
			Name:      vhost,
			Annotations: map[string]string{
				"routing.selector": selector,
			},
		},
		Spec: v1beta1.IngressSpec{
			Rules: []v1beta1.IngressRule{
				{
					Host: vhost + ".demo",
					IngressRuleValue: v1beta1.IngressRuleValue{
						HTTP: &v1beta1.HTTPIngressRuleValue{
							Paths: []v1beta1.HTTPIngressPath{
								{
									Path: "/",
									Backend: v1beta1.IngressBackend{
										ServiceName: vhost,
										ServicePort: intstr.FromInt(8080),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if _, err := client.ExtensionsV1beta1().Ingresses(routingNamespace).Create(&ingress); err != nil {
		return err
	}

	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: routingNamespace,
			Name:      vhost,
		},
		Spec: v1.ServiceSpec{
			Type:      v1.ServiceTypeClusterIP,
			ClusterIP: "None",
		},
	}

	if _, err := client.CoreV1().Services(routingNamespace).Create(&service); err != nil {
		return err
	}

	return nil
}

func deleteVhost(c *cli.Context) error {
	client, err := getClient(routerKubeconfig)
	if err != nil {
		return err
	}

	vhost := c.Args().Get(0)

	if err := client.ExtensionsV1beta1().Ingresses(routingNamespace).Delete(vhost, nil); err != nil {
		return err
	}

	if err := client.CoreV1().Services(routingNamespace).Delete(vhost, nil); err != nil {
		return err
	}

	return nil
}

func listVhosts(c *cli.Context) error {
	client, err := getClient(routerKubeconfig)
	if err != nil {
		return err
	}

	list, err := client.ExtensionsV1beta1().Ingresses(routingNamespace).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, vhost := range list.Items {
		fmt.Printf("%s: %s\n", vhost.Name, vhost.Annotations["routing.selector"])
	}

	return nil
}

func runServer(c *cli.Context) error {
	s := &server{
		backendControllers: make(map[string]*backendController),
	}

	s.run()

	return nil
}

type server struct {
	client kubernetes.Interface

	backendControllersLock sync.Mutex
	backendControllers     map[string]*backendController
}

func (s *server) run() error {
	// list/watch ConfigMaps in routing ns
	client, err := getClient(routerKubeconfig)
	if err != nil {
		return err
	}
	s.client = client

	log.Println("creating shared informer factory")
	sharedInfomers := informers.NewFilteredSharedInformerFactory(client, 0, routingNamespace, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	zoneController := newZoneController(client.CoreV1(), sharedInfomers.Core().V1().ConfigMaps())

	log.Println("starting shared informers")
	go sharedInfomers.Start(ctx.Done())

	log.Println("waiting for cache sync")
	sharedInfomers.WaitForCacheSync(ctx.Done())

	go zoneController.Run(ctx, 1)

	log.Println("waiting for term")
	<-ctx.Done()

	return nil
}
