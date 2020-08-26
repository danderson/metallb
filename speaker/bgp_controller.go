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
	"io"
	"net"
	"reflect"
	"sort"
	"strconv"
	"time"

	"go.universe.tf/metallb/internal/bgp"
	"go.universe.tf/metallb/internal/config"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/go-kit/kit/log"
)

type peer struct {
	cfg *config.Peer
	bgp session
}

type advertisementData struct {
	Adv      *bgp.Advertisement
	PoolName string
}

type bgpController struct {
	logger     log.Logger
	myNode     string
	nodeLabels labels.Set
	peers      []*peer
	svcAds     map[string][]*advertisementData
}

func (c *bgpController) SetConfig(l log.Logger, cfg *config.Config) error {
	newPeers := make([]*peer, 0, len(cfg.Peers))
newPeers:
	for _, p := range cfg.Peers {
		for i, ep := range c.peers {
			if ep == nil {
				continue
			}
			if reflect.DeepEqual(p, ep.cfg) {
				newPeers = append(newPeers, ep)
				c.peers[i] = nil
				continue newPeers
			}
		}
		// No existing peers match, create a new one.
		newPeers = append(newPeers, &peer{
			cfg: p,
		})
	}

	oldPeers := c.peers
	c.peers = newPeers

	for _, p := range oldPeers {
		if p == nil {
			continue
		}
		l.Log("event", "peerRemoved", "peer", p.cfg.Addr, "reason", "removedFromConfig", "msg", "peer deconfigured, closing BGP session")
		if p.bgp != nil {
			if err := p.bgp.Close(); err != nil {
				l.Log("op", "setConfig", "error", err, "peer", p.cfg.Addr, "msg", "failed to shut down BGP session")
			}
		}
	}

	return c.syncPeers(l)
}

// nodeHasHealthyEndpoint return true if this node has at least one healthy endpoint.
func nodeHasHealthyEndpoint(eps *v1.Endpoints, node string) bool {
	ready := map[string]bool{}
	for _, subset := range eps.Subsets {
		for _, ep := range subset.Addresses {
			if ep.NodeName == nil || *ep.NodeName != node {
				continue
			}
			if _, ok := ready[ep.IP]; !ok {
				// Only set true if nothing else has expressed an
				// opinion. This means that false will take precedence
				// if there's any unready ports for a given endpoint.
				ready[ep.IP] = true
			}
		}
		for _, ep := range subset.NotReadyAddresses {
			ready[ep.IP] = false
		}
	}

	for _, r := range ready {
		if r {
			// At least one fully healthy endpoint on this machine.
			return true
		}
	}
	return false
}

func healthyEndpointExists(eps *v1.Endpoints) bool {
	ready := map[string]bool{}
	for _, subset := range eps.Subsets {
		for _, ep := range subset.Addresses {
			if _, ok := ready[ep.IP]; !ok {
				// Only set true if nothing else has expressed an
				// opinion. This means that false will take precedence
				// if there's any unready ports for a given endpoint.
				ready[ep.IP] = true
			}
		}
		for _, ep := range subset.NotReadyAddresses {
			ready[ep.IP] = false
		}
	}

	for _, r := range ready {
		if r {
			// At least one fully healthy endpoint on this machine.
			return true
		}
	}
	return false
}

func (c *bgpController) ShouldAnnounce(l log.Logger, name string, svc *v1.Service, eps *v1.Endpoints) string {
	// Should we advertise?
	// Yes, if externalTrafficPolicy is
	//  Cluster && any healthy endpoint exists
	// or
	//  Local && there's a ready local endpoint.
	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal && !nodeHasHealthyEndpoint(eps, c.myNode) {
		return "noLocalEndpoints"
	} else if !healthyEndpointExists(eps) {
		return "noEndpoints"
	}
	return ""
}

// Called when either the peer list or node labels have changed,
// implying that the set of running BGP sessions may need tweaking.
func (c *bgpController) syncPeers(l log.Logger) error {
	var (
		errs          int
		needUpdateAds bool
	)
	for _, p := range c.peers {
		// First, determine if the peering should be active for this
		// node.
		shouldRun := false
		for _, ns := range p.cfg.NodeSelectors {
			if ns.Matches(c.nodeLabels) {
				shouldRun = true
				break
			}
		}

		// Now, compare current state to intended state, and correct.
		if p.bgp != nil && !shouldRun {
			// Oops, session is running but shouldn't be. Shut it down.
			l.Log("event", "peerRemoved", "peer", p.cfg.Addr, "reason", "filteredByNodeSelector", "msg", "peer deconfigured, closing BGP session")
			if err := p.bgp.Close(); err != nil {
				l.Log("op", "syncPeers", "error", err, "peer", p.cfg.Addr, "msg", "failed to shut down BGP session")
			}
			p.bgp = nil
		} else if p.bgp == nil && shouldRun {
			// Session doesn't exist, but should be running. Create
			// it.
			l.Log("event", "peerAdded", "peer", p.cfg.Addr, "msg", "peer configured, starting BGP session")
			var routerID net.IP
			if p.cfg.RouterID != nil {
				routerID = p.cfg.RouterID
			}
			s, err := newBGP(c.logger, net.JoinHostPort(p.cfg.Addr.String(), strconv.Itoa(int(p.cfg.Port))), p.cfg.MyASN, routerID, p.cfg.ASN, p.cfg.HoldTime, p.cfg.Password, c.myNode)
			if err != nil {
				l.Log("op", "syncPeers", "error", err, "peer", p.cfg.Addr, "msg", "failed to create BGP session")
				errs++
			} else {
				p.bgp = s
				needUpdateAds = true
			}
		}
	}
	if needUpdateAds {
		// Some new sessions came up, resync advertisement state.
		if err := c.updateAds(); err != nil {
			l.Log("op", "updateAds", "error", err, "msg", "failed to update BGP advertisements")
			return err
		}
	}
	if errs > 0 {
		return fmt.Errorf("%d BGP sessions failed to start", errs)
	}
	return nil
}

func (c *bgpController) SetBalancer(l log.Logger, name string, lbIP net.IP, pool *config.Pool) error {
	c.svcAds[name] = nil
	for _, adCfg := range pool.BGPAdvertisements {
		m := net.CIDRMask(adCfg.AggregationLength, 32)
		ad := &bgp.Advertisement{
			Prefix: &net.IPNet{
				IP:   lbIP.Mask(m),
				Mask: m,
			},
			LocalPref: adCfg.LocalPref,
		}
		for comm := range adCfg.Communities {
			ad.Communities = append(ad.Communities, comm)
		}
		sort.Slice(ad.Communities, func(i, j int) bool { return ad.Communities[i] < ad.Communities[j] })

		c.svcAds[name] = append(c.svcAds[name], &advertisementData{Adv: ad, PoolName: pool.Name})
	}

	if err := c.updateAds(); err != nil {
		return err
	}

	l.Log("event", "updatedAdvertisements", "numAds", len(c.svcAds[name]), "msg", "making advertisements using BGP")

	return nil
}

func (c *bgpController) updateAds() error {
	var allAds []*advertisementData
	for _, ads := range c.svcAds {
		// This list might contain duplicates, but that's fine,
		// they'll get compacted by the session code when it's
		// calculating advertisements.
		//
		// TODO: be more intelligent about compacting advertisements
		// and detecting conflicting advertisements.
		allAds = append(allAds, ads...)
	}
	for _, peer := range c.peers {
		if peer.bgp == nil {
			continue
		}
		var peerAds []*bgp.Advertisement
		if peer.cfg.AddressPools == nil {
			for _, adv := range allAds {
				peerAds = append(peerAds, adv.Adv)
			}
		} else {
			for _, adv := range allAds {
				if _, ok := peer.cfg.AddressPools[adv.PoolName]; ok {
					peerAds = append(peerAds, adv.Adv)
				}
			}
		}
		if err := peer.bgp.Set(peerAds...); err != nil {
			return err
		}
	}
	return nil
}

func (c *bgpController) DeleteBalancer(l log.Logger, name, reason string) error {
	if _, ok := c.svcAds[name]; !ok {
		return nil
	}
	delete(c.svcAds, name)
	return c.updateAds()
}

type session interface {
	io.Closer
	Set(advs ...*bgp.Advertisement) error
}

func (c *bgpController) SetNode(l log.Logger, node *v1.Node) error {
	nodeLabels := node.Labels
	if nodeLabels == nil {
		nodeLabels = map[string]string{}
	}
	ns := labels.Set(nodeLabels)
	if c.nodeLabels != nil && labels.Equals(c.nodeLabels, ns) {
		// Node labels unchanged, no action required.
		return nil
	}
	c.nodeLabels = ns
	l.Log("event", "nodeLabelsChanged", "msg", "Node labels changed, resyncing BGP peers")
	return c.syncPeers(l)
}

var newBGP = func(logger log.Logger, addr string, myASN uint32, routerID net.IP, asn uint32, hold time.Duration, password string, myNode string) (session, error) {
	return bgp.New(logger, addr, myASN, routerID, asn, hold, password, myNode)
}
