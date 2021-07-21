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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.universe.tf/metallb/internal/bgp"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
)

type peer struct {
	Cfg *config.Peer
	BGP session
}

type bgpController struct {
	logger          log.Logger
	myNode          string
	nodeAnnotations labels.Set
	nodeLabels      labels.Set
	cfg             *config.Config
	peers           []*peer
	peersLock       sync.Mutex
	svcAds          map[string][]*bgp.Advertisement
}

func (c *bgpController) SetConfig(l log.Logger, cfg *config.Config) error {
	if reflect.DeepEqual(c.cfg, cfg) {
		return nil
	}

	c.cfg = cfg

	level.Info(l).Log("event", "configChanged", "msg", "config changed, resyncing BGP peers")
	return c.syncPeers(l)
}

// hasHealthyEndpoint return true if this node has at least one healthy endpoint.
// It only checks nodes matching the given filterNode function.
func hasHealthyEndpoint(eps k8s.EpsOrSlices, filterNode func(*string) bool) bool {
	ready := map[string]bool{}
	switch eps.Type {
	case k8s.Eps:
		for _, subset := range eps.EpVal.Subsets {
			for _, ep := range subset.Addresses {
				if filterNode(ep.NodeName) {
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
	case k8s.Slices:
		for _, slice := range eps.SlicesVal {
			for _, ep := range slice.Endpoints {
				node := ep.Topology["kubernetes.io/hostname"]
				if filterNode(&node) {
					continue
				}
				for _, addr := range ep.Addresses {
					if _, ok := ready[addr]; !ok && k8s.IsConditionReady(ep.Conditions) {
						// Only set true if nothing else has expressed an
						// opinion. This means that false will take precedence
						// if there's any unready ports for a given endpoint.
						ready[addr] = true
					}
					if !k8s.IsConditionReady(ep.Conditions) {
						ready[addr] = false
					}
				}
			}
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

func (c *bgpController) ShouldAnnounce(l log.Logger, name string, svc *v1.Service, eps k8s.EpsOrSlices) string {
	// Should we advertise?
	// Yes, if externalTrafficPolicy is
	//  Cluster && any healthy endpoint exists
	// or
	//  Local && there's a ready local endpoint.
	filterNode := func(toFilter *string) bool {
		if toFilter == nil || *toFilter != c.myNode {
			return true
		}
		return false
	}

	if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal && !hasHealthyEndpoint(eps, filterNode) {
		return "noLocalEndpoints"
	} else if !hasHealthyEndpoint(eps, func(toFilter *string) bool { return false }) {
		return "noEndpoints"
	}
	return ""
}

// syncPeers updates the BGP peer list based on peers defined statically in the
// configuration as well as any autodiscovered node peers.
func (c *bgpController) syncPeers(l log.Logger) error {
	if c.cfg == nil {
		return nil
	}

	c.peersLock.Lock()
	defer c.peersLock.Unlock()

	// Merge static peers and discovered peers into a single slice representing
	// the desired set of peers while ensuring we don't have two peers which
	// would result in the same BGP session.
	var wantPeers []*config.Peer
	for _, p := range append(c.cfg.Peers, c.discoverNodePeers(l)...) {
		var duplicate bool
		for _, wp := range wantPeers {
			if isSameSession(wp, p, c.nodeLabels) {
				level.Warn(l).Log(
					"event", "peerSkipped",
					"localASN", p.MyASN,
					"peerASN", p.ASN,
					"peerAddress", p.Addr,
					"port", p.Port,
					"holdTime", p.HoldTime,
					"routerID", p.RouterID,
					"msg", fmt.Sprintf("skipping peer to avoid a duplicate BGP session to %s:%d", p.Addr, p.Port),
				)
				duplicate = true
				break
			}
		}
		if !duplicate {
			wantPeers = append(wantPeers, p)
		}
	}

	// Construct a slice with the new peer state.
	newPeers := []*peer{}
newPeers:
	for _, p := range wantPeers {
		for i, ep := range c.peers {
			if ep == nil {
				continue
			}
			if reflect.DeepEqual(p, ep.Cfg) {
				newPeers = append(newPeers, ep)
				// Remove the peer from the slice containing the old peers
				// because later we iterate over the slice and shut down old
				// peers which are no longer configured.
				c.peers[i] = nil
				// Move to the next peer in the outer loop because we don't
				// need to create a new peer (rather, we're preserving an
				// existing peer).
				continue newPeers
			}
		}
		// No existing peers match, create a new one.
		level.Info(l).Log(
			"event", "peerConfigured",
			"localASN", p.MyASN,
			"peerASN", p.ASN,
			"peerAddress", p.Addr,
			"port", p.Port,
			"holdTime", p.HoldTime,
			"routerID", p.RouterID,
			"msg", "adding new peer configuration",
		)
		newPeers = append(newPeers, &peer{
			Cfg: p,
		})
	}

	// Keep the old peers in a new variable and set the in-memory peers to the
	// new, desired state.
	oldPeers := c.peers
	c.peers = newPeers

	// Shut down outdated peers.
	for _, p := range oldPeers {
		if p == nil {
			continue
		}
		level.Info(l).Log("event", "peerDeconfigured", "peer", p.Cfg.Addr, "reason", "removedFromConfig", "msg", "peer deconfigured, closing BGP session")
		if p.BGP != nil {
			if err := p.BGP.Close(); err != nil {
				level.Info(l).Log("op", "setConfig", "error", err, "peer", p.Cfg.Addr, "msg", "failed to shut down BGP session")
			}
		}
	}

	return c.updatePeers(l)
}

// updatePeers is called when either the peer list or node annotations/labels
// have changed, implying that the set of running BGP sessions may need
// tweaking.
func (c *bgpController) updatePeers(l log.Logger) error {
	var totalErrs int
	var needUpdateAds int

	// Update peer BGP sessions.
	update, errs := c.syncBGPSessions(l, c.peers)
	needUpdateAds += update
	totalErrs += errs
	level.Info(l).Log("op", "syncBGPSessions", "needUpdate", update, "errs", errs, "msg", "done syncing peer BGP sessions")

	if needUpdateAds > 0 {
		// Some new sessions came up, resync advertisement state.
		if err := c.updateAds(); err != nil {
			level.Error(l).Log("op", "updateAds", "error", err, "msg", "failed to update BGP advertisements")
			return err
		}
	}
	if totalErrs > 0 {
		return fmt.Errorf("%d BGP sessions failed to start", errs)
	}
	return nil
}

// discoverNodePeers attempts to create BGP peer configurations from node
// annotations and/or labels if peer autodiscovery is configured. Any
// discovered peer configs are returned, and a zero-length slice is returned
// otherwise. Duplicate peer configs are discarded.
func (c *bgpController) discoverNodePeers(l log.Logger) []*config.Peer {
	discovered := []*config.Peer{}

	pad := c.cfg.PeerAutodiscovery
	if pad == nil {
		level.Info(l).Log("op", "discoverNodePeer", "msg", "peer autodiscovery disabled")
		return discovered
	}

	// If the node labels don't match any peer autodiscovery node selector, we
	// shouldn't try to discover peers for this node.
	if !selectorMatches(c.nodeLabels, pad.NodeSelectors) {
		level.Info(l).Log("op", "discoverNodePeer", "msg", "node labels don't match autodiscovery selectors")
		return discovered
	}

	// We need to limit any discovered peers to the relevant node only, so
	// ensure we have a valid hostname label on the Node object.
	h := c.nodeLabels[v1.LabelHostname]
	if h == "" {
		level.Info(l).Log("op", "discoverNodePeer", "msg", fmt.Sprintf("label %s not found on node", v1.LabelHostname))
		return discovered
	}
	selector, err := labels.Parse(fmt.Sprintf("%s=%s", v1.LabelHostname, h))
	if err != nil {
		level.Error(l).Log("op", "discoverNodePeer", "msg", fmt.Sprintf("parsing node selector: %v", err))
		return discovered
	}

	// Parse node peer configuration from annotations.
	if pad.FromAnnotations != nil {
		for _, pam := range pad.FromAnnotations {
			np, err := parseNodePeer(l, pam, pad.Defaults, c.nodeAnnotations)
			if err != nil {
				level.Error(l).Log("op", "discoverNodePeer", "error", err, "msg", "no peer discovered", "mappingType", "fromAnnotations", "mapping", pam)
				continue
			}
			if peerConfigExists(discovered, np) {
				level.Warn(l).Log("op", "discoverNodePeer", "error", err, "msg", "duplicate peer discovered", "mappingType", "fromAnnotations", "mapping", pam)
				continue
			}
			discovered = append(discovered, np)
		}
	}

	// Parse node peer configuration from labels.
	if pad.FromLabels != nil {
		for _, pam := range pad.FromLabels {
			np, err := parseNodePeer(l, pam, pad.Defaults, c.nodeLabels)
			if err != nil {
				level.Error(l).Log("op", "discoverNodePeer", "error", err, "msg", "no peer discovered", "mappingType", "fromLabels", "mapping", pam)
				continue
			}
			if peerConfigExists(discovered, np) {
				level.Warn(l).Log("op", "discoverNodePeer", "error", err, "msg", "duplicate peer discovered", "mappingType", "fromLabels", "mapping", pam)
				continue
			}
			discovered = append(discovered, np)
		}
	}

	// Limit discovered peers to the relevant node only.
	for _, p := range discovered {
		p.NodeSelectors = []labels.Selector{selector}
	}

	return discovered
}

// peerConfigExists returns true if peer config p exists in slice s.
func peerConfigExists(s []*config.Peer, p *config.Peer) bool {
	for _, ep := range s {
		if reflect.DeepEqual(ep, p) {
			return true
		}
	}

	return false
}

func (c *bgpController) syncBGPSessions(l log.Logger, peers []*peer) (needUpdateAds int, errs int) {
	for _, p := range peers {
		// First, determine if the peering should be active for this
		// node.
		shouldRun := false
		for _, ns := range p.Cfg.NodeSelectors {
			if ns.Matches(c.nodeLabels) {
				shouldRun = true
				break
			}
		}

		// Now, compare current state to intended state, and correct.
		if p.BGP != nil && !shouldRun {
			// Oops, session is running but shouldn't be. Shut it down.
			level.Info(l).Log("event", "peerRemoved", "peer", p.Cfg.Addr, "reason", "filteredByNodeSelector", "msg", "peer deconfigured, closing BGP session")
			if err := p.BGP.Close(); err != nil {
				level.Error(l).Log("op", "syncBGPSessions", "error", err, "peer", p.Cfg.Addr, "msg", "failed to shut down BGP session")
			}
			p.BGP = nil
		} else if p.BGP == nil && shouldRun {
			// Session doesn't exist, but should be running. Create
			// it.
			level.Info(l).Log(
				"event", "peerAdded",
				"localASN", p.Cfg.MyASN,
				"peerASN", p.Cfg.ASN,
				"peerAddress", p.Cfg.Addr,
				"port", p.Cfg.Port,
				"holdTime", p.Cfg.HoldTime,
				"routerID", p.Cfg.RouterID,
				"msg", "peer added, starting BGP session",
			)
			var routerID net.IP
			if p.Cfg.RouterID != nil {
				routerID = p.Cfg.RouterID
			}
			s, err := newBGP(c.logger, net.JoinHostPort(p.Cfg.Addr.String(), strconv.Itoa(int(p.Cfg.Port))), p.Cfg.SrcAddr, p.Cfg.MyASN, routerID, p.Cfg.ASN, p.Cfg.HoldTime, p.Cfg.Password, c.myNode)
			if err != nil {
				level.Error(l).Log("op", "syncBGPSessions", "error", err, "peer", p.Cfg.Addr, "msg", "failed to create BGP session")
				errs++
			} else {
				p.BGP = s
				needUpdateAds++
			}
		}
	}
	return
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
		c.svcAds[name] = append(c.svcAds[name], ad)
	}

	if err := c.updateAds(); err != nil {
		return err
	}

	level.Info(l).Log("event", "updatedAdvertisements", "numAds", len(c.svcAds[name]), "msg", "making advertisements using BGP")

	return nil
}

func (c *bgpController) updateAds() error {
	var allAds []*bgp.Advertisement
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
		if peer.BGP == nil {
			continue
		}
		if err := peer.BGP.Set(allAds...); err != nil {
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
	nodeAnnotations := node.Annotations
	if nodeAnnotations == nil {
		nodeAnnotations = map[string]string{}
	}
	nodeLabels := node.Labels
	if nodeLabels == nil {
		nodeLabels = map[string]string{}
	}

	anns := labels.Set(nodeAnnotations)
	ls := labels.Set(nodeLabels)
	annotationsUnchanged := c.nodeAnnotations != nil && labels.Equals(c.nodeAnnotations, anns)
	labelsUnchanged := c.nodeLabels != nil && labels.Equals(c.nodeLabels, ls)
	if labelsUnchanged && annotationsUnchanged {
		// Node labels and annotations unchanged, no action required.
		return nil
	}
	c.nodeAnnotations = anns
	c.nodeLabels = ls

	level.Info(l).Log("event", "nodeChanged", "msg", "node changed, resyncing BGP peers")
	return c.syncPeers(l)
}

// statusPeer represents a BGP peer in a format suitable for publishing in a
// status endpoint.
type statusPeer struct {
	MyASN         uint32
	ASN           uint32
	Addr          net.IP
	Port          uint16
	HoldTime      string
	RouterID      net.IP
	NodeSelectors []string
	Password      string
}

func (c *bgpController) StatusHandler() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		c.peersLock.Lock()
		defer c.peersLock.Unlock()

		// Copy peers slice. We want to redact BGP passwords without modifying
		// the actual peers.
		peers := []statusPeer{}

		for _, p := range c.peers {
			// Peers can be nil temporarily during reconciliation.
			if p == nil {
				continue
			}

			sp := statusPeer{
				MyASN:    p.Cfg.MyASN,
				ASN:      p.Cfg.ASN,
				Addr:     p.Cfg.Addr,
				Port:     p.Cfg.Port,
				HoldTime: p.Cfg.HoldTime.String(),
				RouterID: p.Cfg.RouterID,
			}

			for _, ns := range p.Cfg.NodeSelectors {
				sp.NodeSelectors = append(sp.NodeSelectors, ns.String())
			}

			// Don't expose BGP passwords over the status endpoint.
			if p.Cfg.Password != "" {
				sp.Password = "REDACTED"
			}

			peers = append(peers, sp)
		}

		res := struct {
			Peers []statusPeer
		}{
			Peers: peers,
		}

		j, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get status: %s", err), 500)
			return
		}
		fmt.Fprint(w, string(j))
	}
}

// parseNodePeer attempts to construct a BGP peer configuration from
// information conveyed in node annotations or labels using the given
// autodiscovery mapping and defaults and a set of labels/annotations.
func parseNodePeer(l log.Logger, pam *config.PeerAutodiscoveryMapping, d *config.PeerAutodiscoveryDefaults, ls labels.Set) (*config.Peer, error) {
	// Method called with a nil or empty peer autodiscovery.
	if pam == nil {
		return nil, errors.New("nil peer autodiscovery mapping")
	}

	var (
		myASN       uint32
		peerASN     uint32
		peerAddr    net.IP
		srcAddr     net.IP
		peerPort    uint16
		holdTime    time.Duration
		holdTimeRaw string
		routerID    net.IP
	)

	// Hardcoded defaults. Used only if a parameter isn't specified in the peer
	// autodiscovery defaults and also not in annotations/labels.
	peerPort = config.DefautBGPPeerPort
	holdTime = config.DefaultBGPHoldTime * time.Second

	// Use user-provided defaults. Parameter values read from
	// labels/annotations override the values set here.
	if d != nil {
		if d.ASN != 0 {
			peerASN = d.ASN
		}
		if d.MyASN != 0 {
			myASN = d.MyASN
		}
		if d.Addr != nil {
			peerAddr = d.Addr
		}
		if d.SrcAddr != nil {
			srcAddr = d.SrcAddr
		}
		if d.Port != 0 {
			peerPort = d.Port
		}
		if d.HoldTime != 0 {
			holdTime = d.HoldTime
		}
	}

	for k, v := range ls {
		switch k {
		case pam.MyASN:
			asn, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("parsing local ASN: %v", err)
			}
			myASN = uint32(asn)
		case pam.ASN:
			asn, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("parsing peer ASN: %v", err)
			}
			peerASN = uint32(asn)
		case pam.Addr:
			peerAddr = net.ParseIP(v)
			if peerAddr == nil {
				return nil, fmt.Errorf("invalid peer IP %q", v)
			}
		case pam.SrcAddr:
			srcAddr = net.ParseIP(v)
			if srcAddr == nil {
				return nil, fmt.Errorf("invalid source IP %q", v)
			}
		case pam.Port:
			port, err := strconv.ParseUint(v, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("parsing peer port: %v", err)
			}
			peerPort = uint16(port)
		case pam.HoldTime:
			holdTimeRaw = v
		case pam.RouterID:
			routerID = net.ParseIP(v)
			if routerID == nil {
				return nil, fmt.Errorf("invalid router ID %q", v)
			}
		}
	}

	if myASN == 0 {
		return nil, errors.New("missing or invalid local ASN")
	}
	if peerASN == 0 {
		return nil, errors.New("missing or invalid peer ASN")
	}
	if peerAddr == nil {
		return nil, errors.New("missing or invalid peer address")
	}

	if holdTimeRaw != "" {
		ht, err := config.ParseHoldTime(holdTimeRaw)
		if err != nil {
			return nil, fmt.Errorf("parsing hold time: %v", err)
		}
		holdTime = ht
	}

	p := &config.Peer{
		MyASN:    myASN,
		ASN:      peerASN,
		Addr:     peerAddr,
		SrcAddr:  srcAddr,
		Port:     peerPort,
		HoldTime: holdTime,
		RouterID: routerID,
		// BGP passwords aren't supported for node peers.
		Password: "",
	}

	return p, nil
}

// isSameSession returns true if the peer configurations a and b would result
// in the same TCP connection on a node.
//
// Two configs would result in the same TCP connection if all of the following
// conditions are true:
//
// - The node selectors of both a and b match the label set ls.
// - The peer address of a and b is identical.
// - The source address of a and b is identical.
// - The port of a and b is identical.
func isSameSession(a *config.Peer, b *config.Peer, ls labels.Set) bool {
	if !selectorMatches(ls, a.NodeSelectors) || !selectorMatches(ls, b.NodeSelectors) {
		return false
	}
	if !a.Addr.Equal(b.Addr) {
		return false
	}
	if !a.SrcAddr.Equal(b.SrcAddr) {
		return false
	}
	if a.Port != b.Port {
		return false
	}

	return true
}

// selectorMatches returns true if the label set ls matches any of the
// selectors specified in sel.
func selectorMatches(ls labels.Set, sel []labels.Selector) bool {
	for _, s := range sel {
		if s.Matches(ls) {
			return true
		}
	}

	return false
}

var newBGP = func(logger log.Logger, addr string, srcAddr net.IP, myASN uint32, routerID net.IP, asn uint32, hold time.Duration, password string, myNode string) (session, error) {
	return bgp.New(logger, addr, srcAddr, myASN, routerID, asn, hold, password, myNode)
}
