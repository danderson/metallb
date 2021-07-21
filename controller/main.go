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
	"os"
	"reflect"
	"strings"

	"go.universe.tf/metallb/internal/allocator"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"
	"go.universe.tf/metallb/internal/logging"
	"go.universe.tf/metallb/internal/version"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	v1 "k8s.io/api/core/v1"
)

// Service offers methods to mutate a Kubernetes service object.
type service interface {
	UpdateStatus(svc *v1.Service) error
	Infof(svc *v1.Service, desc, msg string, args ...interface{})
	Errorf(svc *v1.Service, desc, msg string, args ...interface{})
}

type controller struct {
	client service
	synced bool
	config *config.Config
	ips    *allocator.Allocator
}

func (c *controller) SetBalancer(l log.Logger, name string, svcRo *v1.Service, _ k8s.EpsOrSlices) k8s.SyncState {
	level.Debug(l).Log("event", "startUpdate", "msg", "start of service update")
	defer level.Debug(l).Log("event", "endUpdate", "msg", "end of service update")

	if svcRo == nil {
		c.deleteBalancer(l, name)
		// There might be other LBs stuck waiting for an IP, so when
		// we delete a balancer we should reprocess all of them to
		// check for newly feasible balancers.
		return k8s.SyncStateReprocessAll
	}

	if c.config == nil {
		// Config hasn't been read, nothing we can do just yet.
		level.Debug(l).Log("event", "noConfig", "msg", "not processing, still waiting for config")
		return k8s.SyncStateSuccess
	}

	// Making a copy unconditionally is a bit wasteful, since we don't
	// always need to update the service. But, making an unconditional
	// copy makes the code much easier to follow, and we have a GC for
	// a reason.
	svc := svcRo.DeepCopy()
	if !c.convergeBalancer(l, name, svc) {
		return k8s.SyncStateError
	}
	if reflect.DeepEqual(svcRo, svc) {
		level.Debug(l).Log("event", "noChange", "msg", "service converged, no change")
		return k8s.SyncStateSuccess
	}

	if !reflect.DeepEqual(svcRo.Status, svc.Status) {
		var st v1.ServiceStatus
		st, svc = svc.Status, svcRo.DeepCopy()
		svc.Status = st
		if err := c.client.UpdateStatus(svc); err != nil {
			level.Error(l).Log("op", "updateServiceStatus", "error", err, "msg", "failed to update service status")
			return k8s.SyncStateError
		}
	}
	level.Info(l).Log("event", "serviceUpdated", "msg", "updated service object")

	return k8s.SyncStateSuccess
}

func (c *controller) deleteBalancer(l log.Logger, name string) {
	if c.ips.Unassign(name) {
		level.Info(l).Log("event", "serviceDeleted", "msg", "service deleted")
	}
}

func (c *controller) SetConfig(l log.Logger, cfg *config.Config) k8s.SyncState {
	level.Debug(l).Log("event", "startUpdate", "msg", "start of config update")
	defer level.Debug(l).Log("event", "endUpdate", "msg", "end of config update")

	if cfg == nil {
		level.Error(l).Log("op", "setConfig", "error", "no MetalLB configuration in cluster", "msg", "configuration is missing, MetalLB will not function")
		return k8s.SyncStateError
	}

	if err := c.ips.SetPools(cfg.Pools); err != nil {
		level.Error(l).Log("op", "setConfig", "error", err, "msg", "applying new configuration failed")
		return k8s.SyncStateError
	}
	c.config = cfg
	return k8s.SyncStateReprocessAll
}

func (c *controller) MarkSynced(l log.Logger) {
	c.synced = true
	level.Info(l).Log("event", "stateSynced", "msg", "controller synced, can allocate IPs now")
}

func main() {
	var (
		port       = flag.Int("port", 7472, "HTTP listening port for Prometheus metrics")
		config     = flag.String("config", "config", "Kubernetes ConfigMap containing MetalLB's configuration")
		namespace  = flag.String("namespace", os.Getenv("METALLB_NAMESPACE"), "config / memberlist secret namespace")
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file (only needed when running outside of k8s)")
		mlSecret   = flag.String("ml-secret-name", os.Getenv("METALLB_ML_SECRET_NAME"), "name of the memberlist secret to create")
		deployName = flag.String("deployment", os.Getenv("METALLB_DEPLOYMENT"), "name of the MetalLB controller Deployment")
		logLevel   = flag.String("log-level", "info", fmt.Sprintf("log level. must be one of: [%s]", strings.Join(logging.Levels, ", ")))
	)
	flag.Parse()

	logger, err := logging.Init(*logLevel)
	if err != nil {
		fmt.Printf("failed to initialize logging: %s\n", err)
		os.Exit(1)
	}

	level.Info(logger).Log("version", version.Version(), "commit", version.CommitHash(), "branch", version.Branch(), "goversion", version.GoString(), "msg", "MetalLB controller starting "+version.String())

	if *namespace == "" {
		bs, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			level.Error(logger).Log("op", "startup", "msg", "Unable to get namespace from pod service account data, please specify --namespace or METALLB_NAMESPACE", "error", err)
			os.Exit(1)
		}
		*namespace = string(bs)
	}

	c := &controller{
		ips: allocator.New(),
	}

	client, err := k8s.New(&k8s.Config{
		ProcessName:   "metallb-controller",
		ConfigMapName: *config,
		Namespace:     *namespace,
		MetricsPort:   *port,
		Logger:        logger,
		Kubeconfig:    *kubeconfig,

		ServiceChanged: c.SetBalancer,
		ConfigChanged:  c.SetConfig,
		Synced:         c.MarkSynced,
	})
	if err != nil {
		level.Error(logger).Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}

	if *mlSecret != "" {
		err = client.CreateMlSecret(*namespace, *deployName, *mlSecret)
		if err != nil {
			level.Error(logger).Log("op", "startup", "error", err, "msg", "failed to create memberlist secret")
			os.Exit(1)
		}
	}

	c.client = client
	if err := client.Run(nil); err != nil {
		level.Error(logger).Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}
