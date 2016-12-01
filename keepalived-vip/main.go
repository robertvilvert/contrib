/*
Copyright 2015 The Kubernetes Authors.

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
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/spf13/pflag"

	"k8s.io/kubernetes/pkg/api"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/client/restclient"
	kubectl_util "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/util/wait"
)

var (
	flags = pflag.NewFlagSet("", pflag.ContinueOnError)

	cluster = flags.Bool("use-kubernetes-cluster-service", true, `If true, use the built in kubernetes
        cluster for creating the client`)

	useUnicast = flags.Bool("use-unicast", false, `use unicast instead of multicast for communication
		with other keepalived instances`)

	configMapName = flags.String("services-configmap", "",
		`Name of the ConfigMap that contains the definition of the services to expose.
		The key in the map indicates the external IP to use. The value is the name of the 
		service with the format namespace/serviceName and the port of the service could be a number or the
		name of the port.`)

	proxyMode = flags.Bool("proxy-protocol-mode", false, `If true, it will use keepalived to announce the virtual
		IP address/es and HAProxy with proxy protocol to forward traffic to the endpoints.
		Please check http://blog.haproxy.com/haproxy/proxy-protocol
		Be sure that both endpoints of the connection support proxy protocol.
		`)

	// sysctl changes required by keepalived
	sysctlAdjustments = map[string]int{
		// allows processes to bind() to non-local IP addresses
		"net/ipv4/ip_nonlocal_bind": 1,
		// enable connection tracking for LVS connections
		"net/ipv4/vs/conntrack": 1,
	}
)

func main() {
	flags.AddGoFlagSet(flag.CommandLine)
	flags.Parse(os.Args)

	flag.Set("logtostderr", "true")

	clientConfig := kubectl_util.DefaultClientConfig(flags)

	if *configMapName == "" {
		glog.Fatalf("Please specify --services-configmap")
	}

	kubeconfig, err := restclient.InClusterConfig()
	if err != nil {
		kubeconfig, err = clientConfig.ClientConfig()
		if err != nil {
			glog.Fatalf("error configuring the client: %v", err)
		}
	}

	kubeClient, err := clientset.NewForConfig(kubeconfig)
	if err != nil {
		glog.Fatalf("failed to create client: %v", err)
	}

	namespace, specified, err := clientConfig.Namespace()
	if err != nil {
		glog.Fatalf("unexpected error: %v", err)
	}

	if !specified {
		namespace = api.NamespaceAll
	}

	err = loadIPVModule()
	if err != nil {
		glog.Fatalf("unexpected error: %v", err)
	}

	err = changeSysctl()
	if err != nil {
		glog.Fatalf("unexpected error: %v", err)
	}

	err = resetIPVS()
	if err != nil {
		glog.Fatalf("unexpected error: %v", err)
	}

	if *proxyMode {
		copyHaproxyCfg()
	}

	glog.Info("starting LVS configuration")
	if *useUnicast {
		glog.Info("keepalived will use unicast to sync the nodes")
	}
	ipvsc := newIPVSController(kubeClient, namespace, *useUnicast, *configMapName, *proxyMode)
	go ipvsc.epController.Run(wait.NeverStop)
	go ipvsc.svcController.Run(wait.NeverStop)

	go ipvsc.syncQueue.run(time.Second, ipvsc.stopCh)

	go handleSigterm(ipvsc)

	glog.Info("starting keepalived to announce VIPs")
	ipvsc.keepalived.Start()
}

func handleSigterm(ipvsc *ipvsControllerController) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM)
	<-signalChan
	glog.Infof("Received SIGTERM, shutting down")

	exitCode := 0
	if err := ipvsc.Stop(); err != nil {
		glog.Infof("Error during shutdown %v", err)
		exitCode = 1
	}

	glog.Infof("Exiting with %v", exitCode)
	os.Exit(exitCode)
}
