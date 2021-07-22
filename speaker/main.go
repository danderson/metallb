// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"go.universe.tf/metallb/internal/bgp"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"
	"go.universe.tf/metallb/internal/layer2"
	"go.universe.tf/metallb/internal/logging"
	"go.universe.tf/metallb/internal/speakerlist"
	"go.universe.tf/metallb/internal/version"
	v1 "k8s.io/api/core/v1"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

var announcing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "metallb",
	Subsystem: "speaker",
	Name:      "announced",
	Help:      "Services being announced from this node. This is desired state, it does not guarantee that the routing protocols have converged.",
}, []string{
	"service",
	"protocol",
	"node",
	"ip",
})

// Service offers methods to mutate a Kubernetes service object.
type service interface {
	UpdateStatus(svc *v1.Service) error
	Infof(svc *v1.Service, desc, msg string, args ...interface{})
	Errorf(svc *v1.Service, desc, msg string, args ...interface{})
}

func main() {
	prometheus.MustRegister(announcing)

	var (
		config     = flag.String("config", "config", "Kubernetes ConfigMap containing MetalLB's configuration")
		namespace  = flag.String("namespace", os.Getenv("METALLB_NAMESPACE"), "config file and speakers namespace")
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file (only needed when running outside of k8s)")
		host       = flag.String("host", os.Getenv("METALLB_HOST"), "HTTP host address")
		mlBindAddr = flag.String("ml-bindaddr", os.Getenv("METALLB_ML_BIND_ADDR"), "Bind addr for MemberList (fast dead node detection)")
		mlBindPort = flag.String("ml-bindport", os.Getenv("METALLB_ML_BIND_PORT"), "Bind port for MemberList (fast dead node detection)")
		mlLabels   = flag.String("ml-labels", os.Getenv("METALLB_ML_LABELS"), "Labels to match the speakers (for MemberList / fast dead node detection)")
		mlSecret   = flag.String("ml-secret-key", os.Getenv("METALLB_ML_SECRET_KEY"), "Secret key for MemberList (fast dead node detection)")
		myNode     = flag.String("node-name", os.Getenv("METALLB_NODE_NAME"), "name of this Kubernetes node (spec.nodeName)")
		port       = flag.Int("port", 7472, "HTTP listening port")
		logLevel   = flag.String("log-level", "info", fmt.Sprintf("log level. must be one of: [%s]", strings.Join(logging.Levels, ", ")))
	)
	flag.Parse()

	logger, err := logging.Init(*logLevel)
	if err != nil {
		fmt.Printf("failed to initialize logging: %s\n", err)
		os.Exit(1)
	}

	level.Info(logger).Log("version", version.Version(), "commit", version.CommitHash(), "branch", version.Branch(), "goversion", version.GoString(), "msg", "MetalLB speaker starting "+version.String())

	if *namespace == "" {
		bs, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			level.Error(logger).Log("op", "startup", "msg", "Unable to get namespace from pod service account data, please specify --namespace or METALLB_NAMESPACE", "error", err)
			os.Exit(1)
		}
		*namespace = string(bs)
	}

	if *myNode == "" {
		level.Error(logger).Log("op", "startup", "error", "must specify --node-name or METALLB_NODE_NAME", "msg", "missing configuration")
		os.Exit(1)
	}

	stopCh := make(chan struct{})
	go func() {
		c1 := make(chan os.Signal, 1)
		signal.Notify(c1, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
		<-c1
		level.Info(logger).Log("op", "shutdown", "msg", "starting shutdown")
		signal.Stop(c1)
		close(stopCh)
	}()
	defer level.Info(logger).Log("op", "shutdown", "msg", "done")

	sList, err := speakerlist.New(logger, *myNode, *mlBindAddr, *mlBindPort, *mlSecret, *namespace, *mlLabels, stopCh)
	if err != nil {
		os.Exit(1)
	}

	// Setup all clients and speakers, config decides what is being done runtime.
	ctrl, err := newController(controllerConfig{
		MyNode: *myNode,
		Logger: logger,
		SList:  sList,
	})
	if err != nil {
		level.Error(logger).Log("op", "startup", "error", err, "msg", "failed to create MetalLB controller")
		os.Exit(1)
	}

	client, err := k8s.New(&k8s.Config{
		ProcessName:   "metallb-speaker",
		ConfigMapName: *config,
		ConfigMapNS:   *namespace,
		NodeName:      *myNode,
		Logger:        logger,
		Kubeconfig:    *kubeconfig,

		MetricsHost:   *host,
		MetricsPort:   *port,
		ReadEndpoints: true,

		ServiceChanged: ctrl.SetBalancer,
		ConfigChanged:  ctrl.SetConfig,
		NodeChanged:    ctrl.SetNode,
	})
	if err != nil {
		level.Error(logger).Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}
	ctrl.client = client

	sList.Start(client)
	defer sList.Stop()

	if err := client.Run(stopCh); err != nil {
		level.Error(logger).Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}

type controller struct {
	myNode string

	config *config.Config
	client service

	protocols map[config.Proto]Protocol
	announced map[string]config.Proto // service name -> protocol advertising it
	svcIP     map[string]net.IP       // service name -> assigned IP
}

type controllerConfig struct {
	MyNode string
	Logger log.Logger
	SList  SpeakerList

	// For testing only, and will be removed in a future release.
	// See: https://github.com/metallb/metallb/issues/152.
	DisableLayer2 bool
}

func newController(cfg controllerConfig) (*controller, error) {
	protocols := map[config.Proto]Protocol{
		config.BGP: &bgpController{
			logger: cfg.Logger,
			myNode: cfg.MyNode,
			svcAds: make(map[string][]*bgp.Advertisement),
		},
	}

	if !cfg.DisableLayer2 {
		a, err := layer2.New(cfg.Logger)
		if err != nil {
			return nil, fmt.Errorf("making layer2 announcer: %s", err)
		}
		protocols[config.Layer2] = &layer2Controller{
			announcer: a,
			myNode:    cfg.MyNode,
			sList:     cfg.SList,
		}
	}

	ret := &controller{
		myNode:    cfg.MyNode,
		protocols: protocols,
		announced: map[string]config.Proto{},
		svcIP:     map[string]net.IP{},
	}

	return ret, nil
}

func (c *controller) SetBalancer(l log.Logger, name string, svc *v1.Service, eps k8s.EpsOrSlices) k8s.SyncState {
	if svc == nil {
		return c.deleteBalancer(l, name, "serviceDeleted")
	}

	if svc.Spec.Type != "LoadBalancer" {
		return c.deleteBalancer(l, name, "notLoadBalancer")
	}

	level.Debug(l).Log("event", "startUpdate", "msg", "start of service update")
	defer level.Debug(l).Log("event", "endUpdate", "msg", "end of service update")

	if c.config == nil {
		level.Debug(l).Log("event", "noConfig", "msg", "not processing, still waiting for config")
		return k8s.SyncStateSuccess
	}

	if len(svc.Status.LoadBalancer.Ingress) != 1 {
		return c.deleteBalancer(l, name, "noIPAllocated")
	}

	lbIP := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	if lbIP == nil {
		level.Error(l).Log("op", "setBalancer", "error", fmt.Sprintf("invalid LoadBalancer IP %q", svc.Status.LoadBalancer.Ingress[0].IP), "msg", "invalid IP allocated by controller")
		return c.deleteBalancer(l, name, "invalidIP")
	}

	l = log.With(l, "ip", lbIP)

	poolName := poolFor(c.config.Pools, lbIP)
	if poolName == "" {
		level.Error(l).Log("op", "setBalancer", "error", "assigned IP not allowed by config", "msg", "IP allocated by controller not allowed by config")
		return c.deleteBalancer(l, name, "ipNotAllowed")
	}

	l = log.With(l, "pool", poolName)
	pool := c.config.Pools[poolName]
	if pool == nil {
		level.Error(l).Log("bug", "true", "msg", "internal error: allocated IP has no matching address pool")
		return c.deleteBalancer(l, name, "internalError")
	}

	if proto, ok := c.announced[name]; ok && proto != pool.Protocol {
		if st := c.deleteBalancer(l, name, "protocolChanged"); st == k8s.SyncStateError {
			return st
		}
	}

	if svcIP, ok := c.svcIP[name]; ok && !lbIP.Equal(svcIP) {
		if st := c.deleteBalancer(l, name, "loadBalancerIPChanged"); st == k8s.SyncStateError {
			return st
		}
	}

	l = log.With(l, "protocol", pool.Protocol)
	handler := c.protocols[pool.Protocol]
	if handler == nil {
		level.Error(l).Log("bug", "true", "msg", "internal error: unknown balancer protocol!")
		return c.deleteBalancer(l, name, "internalError")
	}

	if deleteReason := handler.ShouldAnnounce(l, name, svc, eps, lbIP); deleteReason != "" {
		return c.deleteBalancer(l, name, deleteReason)
	}

	if err := handler.SetBalancer(l, name, lbIP, pool); err != nil {
		level.Error(l).Log("op", "setBalancer", "error", err, "msg", "failed to announce service")
		return k8s.SyncStateError
	}

	if c.announced[name] == "" {
		c.announced[name] = pool.Protocol
		c.svcIP[name] = lbIP
	}

	announcing.With(prometheus.Labels{
		"protocol": string(pool.Protocol),
		"service":  name,
		"node":     c.myNode,
		"ip":       lbIP.String(),
	}).Set(1)
	level.Info(l).Log("event", "serviceAnnounced", "msg", "service has IP, announcing")
	c.client.Infof(svc, "nodeAssigned", "announcing from node %q", c.myNode)

	return k8s.SyncStateSuccess
}

func (c *controller) deleteBalancer(l log.Logger, name, reason string) k8s.SyncState {
	proto, ok := c.announced[name]
	if !ok {
		return k8s.SyncStateSuccess
	}

	if err := c.protocols[proto].DeleteBalancer(l, name, reason); err != nil {
		level.Error(l).Log("op", "deleteBalancer", "error", err, "msg", "failed to clear balancer state")
		return k8s.SyncStateError
	}

	announcing.Delete(prometheus.Labels{
		"protocol": string(proto),
		"service":  name,
		"node":     c.myNode,
		"ip":       c.svcIP[name].String(),
	})
	delete(c.announced, name)
	delete(c.svcIP, name)

	level.Info(l).Log("event", "serviceWithdrawn", "ip", c.svcIP[name], "reason", reason, "msg", "withdrawing service announcement")

	return k8s.SyncStateSuccess
}

func poolFor(pools map[string]*config.Pool, ip net.IP) string {
	for pname, p := range pools {
		for _, cidr := range p.CIDR {
			if cidr.Contains(ip) {
				return pname
			}
		}
	}
	return ""
}

func (c *controller) SetConfig(l log.Logger, cfg *config.Config) k8s.SyncState {
	level.Debug(l).Log("event", "startUpdate", "msg", "start of config update")
	defer level.Debug(l).Log("event", "endUpdate", "msg", "end of config update")

	if cfg == nil {
		level.Error(l).Log("op", "setConfig", "error", "no MetalLB configuration in cluster", "msg", "configuration is missing, MetalLB will not function")
		return k8s.SyncStateError
	}

	for svc, ip := range c.svcIP {
		if pool := poolFor(cfg.Pools, ip); pool == "" {
			level.Error(l).Log("op", "setConfig", "service", svc, "ip", ip, "error", "service has no configuration under new config", "msg", "new configuration rejected")
			return k8s.SyncStateError
		}
	}

	for proto, handler := range c.protocols {
		if err := handler.SetConfig(l, cfg); err != nil {
			level.Error(l).Log("op", "setConfig", "protocol", proto, "error", err, "msg", "applying new configuration to protocol handler failed")
			return k8s.SyncStateError
		}
	}

	c.config = cfg

	return k8s.SyncStateReprocessAll
}

func (c *controller) SetNode(l log.Logger, node *v1.Node) k8s.SyncState {
	for proto, handler := range c.protocols {
		if err := handler.SetNode(l, node); err != nil {
			level.Error(l).Log("op", "setNode", "error", err, "protocol", proto, "msg", "failed to propagate node info to protocol handler")
			return k8s.SyncStateError
		}
	}
	return k8s.SyncStateSuccess
}

// A Protocol can advertise an IP address.
type Protocol interface {
	SetConfig(log.Logger, *config.Config) error
	ShouldAnnounce(log.Logger, string, *v1.Service, k8s.EpsOrSlices, net.IP) string
	SetBalancer(log.Logger, string, net.IP, *config.Pool) error
	DeleteBalancer(log.Logger, string, string) error
	SetNode(log.Logger, *v1.Node) error
}

// Speakerlist represents a list of healthy speakers.
type SpeakerList interface {
	UsableSpeakers() map[string]bool
	Rejoin()
}
