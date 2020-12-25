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
	"fmt"
	"net"
	"strings"

	"github.com/go-kit/kit/log"
	v1 "k8s.io/api/core/v1"

	"go.universe.tf/metallb/internal/allocator"
	"go.universe.tf/metallb/internal/allocator/k8salloc"
)

func (c *controller) convergeBalancer(l log.Logger, key string, svc *v1.Service) bool {
	var lbIP net.IP

	// Not a LoadBalancer, early exit. It might have been a balancer
	// in the past, so we still need to clear LB state.
	if svc.Spec.Type != "LoadBalancer" {
		l.Log("event", "clearAssignment", "reason", "notLoadBalancer", "msg", "not a LoadBalancer")
		c.clearServiceState(key, svc)
		// Early return, we explicitly do *not* want to reallocate
		// an IP.
		return true
	}

	// If the ClusterIP is malformed or not set we can't determine the
	// ipFamily to use.
	clusterIP := net.ParseIP(svc.Spec.ClusterIP)
	if clusterIP == nil {
		l.Log("event", "clearAssignment", "reason", "noClusterIP", "msg", "No ClusterIP")
		c.clearServiceState(key, svc)
		return true
	}

	var iptype allocator.IPType
	if svc.Spec.ClusterIPs != nil {
		iptype, _, _ = c.ips.ParseIPs(svc.Spec.ClusterIPs)
	} else {
		iptype, _, _ = c.ips.ParseIPs([]string{svc.Spec.ClusterIP})
	}
	if iptype == allocator.DualStack {
		return c.convergeBalancerDual(l, key, svc)
	}

	// The assigned LB IP is the end state of convergence. If there's
	// none or a malformed one, nuke all controlled state so that we
	// start converging from a clean slate.
	if len(svc.Status.LoadBalancer.Ingress) == 1 {
		lbIP = net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	}
	if lbIP == nil {
		c.clearServiceState(key, svc)
	}

	// Clear the lbIP if it has a different ipFamily compared to the clusterIP.
	// (this should not happen since the "ipFamily" of a service is immutable)
	if (clusterIP.To4() == nil) != (lbIP.To4() == nil) {
		c.clearServiceState(key, svc)
		lbIP = nil
	}

	// It's possible the config mutated and the IP we have no longer
	// makes sense. If so, clear it out and give the rest of the logic
	// a chance to allocate again.
	if lbIP != nil {
		// This assign is idempotent if the config is consistent,
		// otherwise it'll fail and tell us why.
		if err := c.ips.Assign(key, []net.IP{lbIP}, k8salloc.Ports(svc), k8salloc.SharingKey(svc), k8salloc.BackendKey(svc)); err != nil {
			l.Log("event", "clearAssignment", "reason", "notAllowedByConfig", "msg", "current IP not allowed by config, clearing")
			c.clearServiceState(key, svc)
			lbIP = nil
		}

		// The user might also have changed the pool annotation, and
		// requested a different pool than the one that is currently
		// allocated.
		desiredPool := svc.Annotations["metallb.universe.tf/address-pool"]
		if lbIP != nil && desiredPool != "" && c.ips.Pool(key) != desiredPool {
			l.Log("event", "clearAssignment", "reason", "differentPoolRequested", "msg", "user requested a different pool than the one currently assigned")
			c.clearServiceState(key, svc)
			lbIP = nil
		}
	}

	// User set or changed the desired LB IP, nuke the
	// state. allocateIP will pay attention to LoadBalancerIP and try
	// to meet the user's demands.
	if svc.Spec.LoadBalancerIP != "" && svc.Spec.LoadBalancerIP != lbIP.String() {
		l.Log("event", "clearAssignment", "reason", "differentIPRequested", "msg", "user requested a different IP than the one currently assigned")
		c.clearServiceState(key, svc)
		lbIP = nil
	}

	// If lbIP is still nil at this point, try to allocate.
	if lbIP == nil {
		if !c.synced {
			l.Log("op", "allocateIP", "error", "controller not synced", "msg", "controller not synced yet, cannot allocate IP; will retry after sync")
			return false
		}
		ips, err := c.allocateIPs(key, svc)
		if err != nil {
			l.Log("op", "allocateIP", "error", err, "msg", "IP allocation failed")
			c.client.Errorf(svc, "AllocationFailed", "Failed to allocate IP for %q: %s", key, err)
			// The outer controller loop will retry converging this
			// service when another service gets deleted, so there's
			// nothing to do here but wait to get called again later.
			return true
		}
		lbIP = ips[0]
		l.Log("event", "ipAllocated", "ip", lbIP, "msg", "IP address assigned by controller")
		c.client.Infof(svc, "IPAllocated", "Assigned IP %q", lbIP)
	}

	if lbIP == nil {
		l.Log("bug", "true", "msg", "internal error: failed to allocate an IP, but did not exit convergeService early!")
		c.client.Errorf(svc, "InternalError", "didn't allocate an IP but also did not fail")
		c.clearServiceState(key, svc)
		return true
	}

	pool := c.ips.Pool(key)
	if pool == "" || c.config.Pools[pool] == nil {
		l.Log("bug", "true", "ip", lbIP, "msg", "internal error: allocated IP has no matching address pool")
		c.client.Errorf(svc, "InternalError", "allocated an IP that has no pool")
		c.clearServiceState(key, svc)
		return true
	}

	// At this point, we have an IP selected somehow, all that remains
	// is to program the data plane.
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: lbIP.String()}}
	return true
}

// clearServiceState clears all fields that are actively managed by
// this controller.
func (c *controller) clearServiceState(key string, svc *v1.Service) {
	c.ips.Unassign(key)
	svc.Status.LoadBalancer = v1.LoadBalancerStatus{}
}

func (c *controller) allocateIPs(key string, svc *v1.Service) ([]net.IP, error) {
	var iptype allocator.IPType
	var err error
	if svc.Spec.ClusterIPs != nil {
		iptype, _, err = c.ips.ParseIPs(svc.Spec.ClusterIPs)
	} else {
		iptype, _, err = c.ips.ParseIPs([]string{svc.Spec.ClusterIP})
	}
	if err != nil {
		// (we should never get here because the caller ensured that Spec.ClusterIP != nil and valid)
		return nil, fmt.Errorf("invalid ClusterIP [%s], can't determine family", svc.Spec.ClusterIP)
	}

	// If the user asked for a specific IP, try that.
	if svc.Spec.LoadBalancerIP != "" {
		if iptype != allocator.DualStack {
			lbiptype, ips, err := c.ips.ParseIPs([]string{svc.Spec.LoadBalancerIP})
			if err != nil {
				return nil, err
			}
			if iptype != lbiptype {
				return nil, fmt.Errorf("requested spec.loadBalancerIP %q does not match the ipFamily of the service", svc.Spec.LoadBalancerIP)
			}
			if err := c.ips.Assign(key, ips, k8salloc.Ports(svc), k8salloc.SharingKey(svc), k8salloc.BackendKey(svc)); err != nil {
				return nil, err
			}
			return ips, nil
		}
	}

	// Otherwise, did the user ask for a specific pool?
	desiredPool := svc.Annotations["metallb.universe.tf/address-pool"]
	if desiredPool != "" {
		ips, err := c.ips.AllocateFromPool(key, iptype, desiredPool, k8salloc.Ports(svc), k8salloc.SharingKey(svc), k8salloc.BackendKey(svc))
		if err != nil {
			return nil, err
		}
		return ips, nil
	}

	// Okay, in that case just bruteforce across all pools.
	return c.ips.Allocate(key, iptype, k8salloc.Ports(svc), k8salloc.SharingKey(svc), k8salloc.BackendKey(svc))
}

// ======================================== Dual-stack

func (c *controller) convergeBalancerDual(l log.Logger, key string, svc *v1.Service) bool {
	var lbIP, lbIP2 net.IP

	// The assigned LB IP is the end state of convergence. If there's
	// none or a malformed one, nuke all controlled state so that we
	// start converging from a clean slate.
	var iptype allocator.IPType
	var lbips []net.IP
	if len(svc.Status.LoadBalancer.Ingress) > 1 {
		var err error
		iptype, lbips, err = c.ips.ParseIPs([]string{svc.Status.LoadBalancer.Ingress[0].IP, svc.Status.LoadBalancer.Ingress[1].IP})
		if err != nil {
			iptype = allocator.Invalid
		}
	}

	// It's possible the config mutated and the IP we have no longer
	// makes sense. If so, clear it out and give the rest of the logic
	// a chance to allocate again.
	if iptype == allocator.DualStack {
		// This assign is idempotent if the config is consistent,
		// otherwise it'll fail and tell us why.
		if err := c.ips.Assign(key, lbips, k8salloc.Ports(svc), k8salloc.SharingKey(svc), k8salloc.BackendKey(svc)); err != nil {
			l.Log("event", "clearAssignment", "reason", "notAllowedByConfig", "msg", "current IP not allowed by config, clearing")
			c.clearServiceState(key, svc)
		} else {
			// The user might also have changed the pool annotation, and
			// requested a different pool than the one that is currently
			// allocated.
			desiredPool := svc.Annotations["metallb.universe.tf/address-pool"]
			if desiredPool != "" && c.ips.Pool(key) != desiredPool {
				l.Log("event", "clearAssignment", "reason", "differentPoolRequested", "msg", "user requested a different pool than the one currently assigned")
				c.clearServiceState(key, svc)
				lbIP = nil
			}
		}
	} else {
		c.clearServiceState(key, svc)
		lbIP = nil
	}

	// The (singular) svc.Spec.LoadBalancerIP is ignored for dual-stack
	if svc.Spec.LoadBalancerIP != "" {
		l.Log("event", "loadBalancerIP", "reason", "N/A", "msg", "loadBalancerIP ignored for dual-stack")
	}

	if requestedIPs := svc.Annotations["metallb.universe.tf/load-balancer-ips"]; requestedIPs != "" {
		// Until a svc.Spec.LoadBalancerIPs exists we use an annotation.
		// requestedIPs must be a comma-separated list of 2 addresses, one from each family.
		ips := strings.Split(requestedIPs, ",")
		if len(ips) != 2 {
			l.Log("op", "allocateIP", "load-balancer-ips", len(ips), "msg", "Must be two addresses")
			return true
		}
		if lbIP = net.ParseIP(strings.TrimSpace(ips[0])); lbIP == nil {
			l.Log("op", "allocateIP", "load-balancer-ips", ips[0], "msg", "Invalid addresses")
			return true
		}
		if lbIP2 = net.ParseIP(strings.TrimSpace(ips[1])); lbIP2 == nil {
			l.Log("op", "allocateIP", "load-balancer-ips", ips[1], "msg", "Invalid addresses")
			return true
		}
		if (lbIP.To4() == nil) == (lbIP2.To4() == nil) {
			l.Log("op", "allocateIP", "load-balancer-ips", requestedIPs, "msg", "Same family")
		}

		// Try to assign the requested IPs
		if err := c.ips.Assign(key, []net.IP{lbIP, lbIP2}, k8salloc.Ports(svc), k8salloc.SharingKey(svc), k8salloc.BackendKey(svc)); err != nil {
			l.Log("op", "allocateIP", "error", err, "msg", "Can't assign requested IPs")
			return true
		}
	}

	// If lbIP's is still nil at this point, try to allocate.
	if lbIP == nil {
		if !c.synced {
			l.Log("op", "allocateIP", "error", "controller not synced", "msg", "controller not synced yet, cannot allocate IP; will retry after sync")
			return false
		}
		ips, err := c.allocateIPs(key, svc)
		if err != nil {
			l.Log("op", "allocateIP", "error", err, "msg", "IP allocation failed")
			c.client.Errorf(svc, "AllocationFailed", "Failed to allocate IP for %q: %s", key, err)
			// The outer controller loop will retry converging this
			// service when another service gets deleted, so there's
			// nothing to do here but wait to get called again later.
			return true
		}
		lbIP = ips[0]
		lbIP2 = ips[1]
		l.Log("event", "ipAllocated", "ip", lbIP, "ip2", lbIP2, "msg", "IP address assigned by controller")
		c.client.Infof(svc, "IPAllocated", "Assigned IP %q %q", lbIP, lbIP2)
	}

	if lbIP == nil || lbIP2 == nil {
		l.Log("bug", "true", "msg", "internal error: failed to allocate an IP, but did not exit convergeService early!")
		c.client.Errorf(svc, "InternalError", "didn't allocate an IP but also did not fail")
		c.clearServiceState(key, svc)
		return true
	}

	pool := c.ips.Pool(key)
	if pool == "" || c.config.Pools[pool] == nil {
		l.Log("bug", "true", "ip", lbIP, "msg", "internal error: allocated IP has no matching address pool")
		c.client.Errorf(svc, "InternalError", "allocated an IP that has no pool")
		c.clearServiceState(key, svc)
		return true
	}

	// At this point, we have an IP selected somehow, all that remains
	// is to program the data plane.
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: lbIP.String()}, {IP: lbIP2.String()}}
	return true
}
