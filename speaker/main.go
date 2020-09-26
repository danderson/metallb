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
	"crypto/sha256"
	"flag"
	"fmt"
	golog "log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.universe.tf/metallb/internal/bgp"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"
	"go.universe.tf/metallb/internal/layer2"
	"go.universe.tf/metallb/internal/logging"
	"go.universe.tf/metallb/internal/version"
	v1 "k8s.io/api/core/v1"

	gokitlog "github.com/go-kit/kit/log"
	"github.com/hashicorp/memberlist"
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
	Update(svc *v1.Service) (*v1.Service, error)
	UpdateStatus(svc *v1.Service) error
	Infof(svc *v1.Service, desc, msg string, args ...interface{})
	Errorf(svc *v1.Service, desc, msg string, args ...interface{})
}

func main() {
	prometheus.MustRegister(announcing)

	logger, err := logging.Init()
	if err != nil {
		fmt.Printf("failed to initialize logging: %s\n", err)
		os.Exit(1)
	}

	var (
		config      = flag.String("config", "config", "Kubernetes ConfigMap containing MetalLB's configuration")
		configNS    = flag.String("config-ns", "", "config file namespace (only needed when running outside of k8s)")
		kubeconfig  = flag.String("kubeconfig", "", "absolute path to the kubeconfig file (only needed when running outside of k8s)")
		host        = flag.String("host", os.Getenv("METALLB_HOST"), "HTTP host address")
		mlBindAddr  = flag.String("ml-bindaddr", os.Getenv("METALLB_ML_BIND_ADDR"), "Bind addr for MemberList (fast dead node detection)")
		mlBindPort  = flag.String("ml-bindport", os.Getenv("METALLB_ML_BIND_PORT"), "Bind port for MemberList (fast dead node detection)")
		mlLabels    = flag.String("ml-labels", os.Getenv("METALLB_ML_LABELS"), "Labels to match the speakers (for MemberList / fast dead node detection)")
		mlNamespace = flag.String("ml-namespace", os.Getenv("METALLB_ML_NAMESPACE"), "Namespace of the speakers (for MemberList / fast dead node detection)")
		mlSecret    = flag.String("ml-secret-key", os.Getenv("METALLB_ML_SECRET_KEY"), "Secret key for MemberList (fast dead node detection)")
		myNode      = flag.String("node-name", os.Getenv("METALLB_NODE_NAME"), "name of this Kubernetes node (spec.nodeName)")
		port        = flag.Int("port", 80, "HTTP listening port")
	)
	flag.Parse()

	logger.Log("version", version.Version(), "commit", version.CommitHash(), "branch", version.Branch(), "msg", "MetalLB speaker starting "+version.String())

	if *myNode == "" {
		logger.Log("op", "startup", "error", "must specify --node-name or METALLB_NODE_NAME", "msg", "missing configuration")
		os.Exit(1)
	}

	stopCh := make(chan struct{})
	go func() {
		c1 := make(chan os.Signal)
		signal.Notify(c1, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
		<-c1
		logger.Log("op", "shutdown", "msg", "starting shutdown")
		signal.Stop(c1)
		close(stopCh)
	}()
	defer logger.Log("op", "shutdown", "msg", "done")

	var mlist *memberlist.Memberlist
	var eventCh chan memberlist.NodeEvent
	if *mlNamespace == "" || *mlLabels == "" || *mlBindAddr == "" {
		logger.Log("op", "startup", "msg", "Not starting fast dead node detection (MemberList), need ml-bindaddr / ml-labels / ml-namespace config")
	} else {
		mconfig := memberlist.DefaultLANConfig()
		// mconfig.Name MUST be spec.nodeName, as we will match it against Enpoints nodeName in usableNodes()
		mconfig.Name = *myNode
		mconfig.BindAddr = *mlBindAddr
		if *mlBindPort != "" {
			mlport, err := strconv.Atoi(*mlBindPort)
			if err != nil {
				logger.Log("op", "startup", "error", "unable to parse ml-bindport", "msg", err)
				os.Exit(1)
			}
			mconfig.BindPort = mlport
			mconfig.AdvertisePort = mlport
		}
		loggerout := gokitlog.NewStdlibAdapter(gokitlog.With(logger, "component", "MemberList"))
		mconfig.Logger = golog.New(loggerout, "", golog.Lshortfile)
		if *mlSecret != "" {
			sha := sha256.New()
			mconfig.SecretKey = sha.Sum([]byte(*mlSecret))[:16]
		}
		eventCh = make(chan memberlist.NodeEvent, 16)
		mconfig.Events = &memberlist.ChannelEventDelegate{Ch: eventCh}
		mlist, err = memberlist.Create(mconfig)
		if err != nil {
			logger.Log("op", "startup", "error", err, "msg", "failed to create memberlist")
			os.Exit(1)
		}
	}

	// Setup all clients and speakers, config decides what is being done runtime.
	ctrl, err := newController(controllerConfig{
		MyNode: *myNode,
		Logger: logger,
		MList:  mlist,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create MetalLB controller")
		os.Exit(1)
	}

	client, err := k8s.New(&k8s.Config{
		ProcessName:   "metallb-speaker",
		ConfigMapName: *config,
		ConfigMapNS:   *configNS,
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
		logger.Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}
	ctrl.client = client

	if mlist != nil {
		go watchMemberListEvents(logger, eventCh, stopCh, client)

		iplist, err := client.GetPodsIPs(*mlNamespace, *mlLabels)
		if err != nil {
			logger.Log("op", "startup", "error", err, "msg", "failed to get PodsIPs")
			os.Exit(1)
		}
		n, err := mlist.Join(iplist)
		logger.Log("op", "startup", "msg", "Memberlist join", "nb joigned", n, "error ?", err)
		defer func() {
			logger.Log("op", "shutdown", "msg", "leaving MemberList cluster")
			err = mlist.Leave(time.Second)
			logger.Log("op", "shutdown", "msg", "left MemberList cluster", "error ?", err)
			mlist.Shutdown()
			logger.Log("op", "shutdown", "msg", "MemberList shutdown", "error ?", err)
		}()
	}

	if err := client.Run(stopCh); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}

func event2String(e memberlist.NodeEventType) string {
	return [...]string{"NodeJoin", "NodeLeave", "NodeUpdate"}[e]
}

func watchMemberListEvents(logger gokitlog.Logger, eventCh chan memberlist.NodeEvent, stopCh chan struct{}, client *k8s.Client) {
	for {
		select {
		case e := <-eventCh:
			logger.Log("msg", "Node event", "node addr", e.Node.Addr, "node name", e.Node.Name, "node event", event2String(e.Event))
			logger.Log("msg", "Call Force Sync")
			client.ForceSync()
		case <-stopCh:
			return
		}
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
	Logger gokitlog.Logger
	MList  *memberlist.Memberlist

	// For testing only, and will be removed in a future release.
	// See: https://github.com/google/metallb/issues/152.
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
			mList:     cfg.MList,
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

func (c *controller) SetBalancer(l gokitlog.Logger, name string, svc *v1.Service, eps *v1.Endpoints) k8s.SyncState {
	if svc == nil {
		return c.deleteBalancer(l, name, "serviceDeleted")
	}

	l.Log("event", "startUpdate", "msg", "start of service update")
	defer l.Log("event", "endUpdate", "msg", "end of service update")

	if svc.Spec.Type != "LoadBalancer" {
		return c.deleteBalancer(l, name, "notLoadBalancer")
	}

	if c.config == nil {
		l.Log("event", "noConfig", "msg", "not processing, still waiting for config")
		return k8s.SyncStateSuccess
	}

	if len(svc.Status.LoadBalancer.Ingress) != 1 {
		return c.deleteBalancer(l, name, "noIPAllocated")
	}

	lbIP := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	if lbIP == nil {
		l.Log("op", "setBalancer", "error", fmt.Sprintf("invalid LoadBalancer IP %q", svc.Status.LoadBalancer.Ingress[0].IP), "msg", "invalid IP allocated by controller")
		return c.deleteBalancer(l, name, "invalidIP")
	}

	l = gokitlog.With(l, "ip", lbIP)

	poolName := poolFor(c.config.Pools, lbIP)
	if poolName == "" {
		l.Log("op", "setBalancer", "error", "assigned IP not allowed by config", "msg", "IP allocated by controller not allowed by config")
		return c.deleteBalancer(l, name, "ipNotAllowed")
	}

	l = gokitlog.With(l, "pool", poolName)
	pool := c.config.Pools[poolName]
	if pool == nil {
		l.Log("bug", "true", "msg", "internal error: allocated IP has no matching address pool")
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

	l = gokitlog.With(l, "protocol", pool.Protocol)
	handler := c.protocols[pool.Protocol]
	if handler == nil {
		l.Log("bug", "true", "msg", "internal error: unknown balancer protocol!")
		return c.deleteBalancer(l, name, "internalError")
	}

	if deleteReason := handler.ShouldAnnounce(l, svc, eps, lbIP); deleteReason != "" {
		return c.deleteBalancer(l, name, deleteReason)
	}

	if err := handler.SetBalancer(l, name, lbIP, pool); err != nil {
		l.Log("op", "setBalancer", "error", err, "msg", "failed to announce service")
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
	l.Log("event", "serviceAnnounced", "msg", "service has IP, announcing")
	c.client.Infof(svc, "nodeAssigned", "announcing from node %q", c.myNode)

	return k8s.SyncStateSuccess
}

func (c *controller) deleteBalancer(l gokitlog.Logger, name, reason string) k8s.SyncState {
	proto, ok := c.announced[name]
	if !ok {
		return k8s.SyncStateSuccess
	}

	if err := c.protocols[proto].DeleteBalancer(l, name, reason); err != nil {
		l.Log("op", "deleteBalancer", "error", err, "msg", "failed to clear balancer state")
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

	l.Log("event", "serviceWithdrawn", "ip", c.svcIP[name], "reason", reason, "msg", "withdrawing service announcement")

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

func (c *controller) SetConfig(l gokitlog.Logger, cfg *config.Config) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of config update")
	defer l.Log("event", "endUpdate", "msg", "end of config update")

	if cfg == nil {
		l.Log("op", "setConfig", "error", "no MetalLB configuration in cluster", "msg", "configuration is missing, MetalLB will not function")
		return k8s.SyncStateError
	}

	for svc, ip := range c.svcIP {
		if pool := poolFor(cfg.Pools, ip); pool == "" {
			l.Log("op", "setConfig", "service", svc, "ip", ip, "error", "service has no configuration under new config", "msg", "new configuration rejected")
			return k8s.SyncStateError
		}
	}

	for proto, handler := range c.protocols {
		if err := handler.SetConfig(l, cfg); err != nil {
			l.Log("op", "setConfig", "protocol", proto, "error", err, "msg", "applying new configuration to protocol handler failed")
			return k8s.SyncStateError
		}
	}

	c.config = cfg

	return k8s.SyncStateReprocessAll
}

func (c *controller) SetNode(l gokitlog.Logger, node *v1.Node) k8s.SyncState {
	for proto, handler := range c.protocols {
		if err := handler.SetNode(l, node); err != nil {
			l.Log("op", "setNode", "error", err, "protocol", proto, "msg", "failed to propagate node info to protocol handler")
			return k8s.SyncStateError
		}
	}
	return k8s.SyncStateSuccess
}

// A Protocol can advertise an IP address.
type Protocol interface {
	SetConfig(gokitlog.Logger, *config.Config) error
	ShouldAnnounce(gokitlog.Logger, *v1.Service, *v1.Endpoints, net.IP) string
	SetBalancer(gokitlog.Logger, string, net.IP, *config.Pool) error
	DeleteBalancer(gokitlog.Logger, string, string) error
	SetNode(gokitlog.Logger, *v1.Node) error
}
