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

package config // import "go.universe.tf/metallb/internal/config"

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/mikioh/ipaddr"
	yaml "gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	// DefaultBGPHoldTime is the default hold time value (in seconds) used for
	// BGP peers.
	DefaultBGPHoldTime = 90
	// DefautBGPPeerPort is the default remote port used for BGP peers.
	DefautBGPPeerPort = 179
)

// configFile is the configuration as parsed out of the ConfigMap,
// without validation or useful high level types.
type configFile struct {
	Peers             []peer
	PeerAutodiscovery *peerAutodiscovery `yaml:"peer-autodiscovery"`
	BGPCommunities    map[string]string  `yaml:"bgp-communities"`
	Pools             []addressPool      `yaml:"address-pools"`
}

type peer struct {
	MyASN         uint32         `yaml:"my-asn"`
	ASN           uint32         `yaml:"peer-asn"`
	Addr          string         `yaml:"peer-address"`
	SrcAddr       string         `yaml:"source-address"`
	Port          uint16         `yaml:"peer-port"`
	HoldTime      string         `yaml:"hold-time"`
	RouterID      string         `yaml:"router-id"`
	NodeSelectors []nodeSelector `yaml:"node-selectors"`
	Password      string         `yaml:"password"`
}

type peerAutodiscovery struct {
	Defaults        *peerAutodiscoveryDefaults  `yaml:"defaults"`
	NodeSelectors   []nodeSelector              `yaml:"node-selectors"`
	FromAnnotations []*peerAutodiscoveryMapping `yaml:"from-annotations"`
	FromLabels      []*peerAutodiscoveryMapping `yaml:"from-labels"`
}

type peerAutodiscoveryDefaults struct {
	MyASN    uint32 `yaml:"my-asn"`
	ASN      uint32 `yaml:"peer-asn"`
	Addr     string `yaml:"peer-address"`
	SrcAddr  string `yaml:"source-address"`
	Port     uint16 `yaml:"peer-port"`
	HoldTime string `yaml:"hold-time"`
	RouterID string `yaml:"router-id"`
}

type peerAutodiscoveryMapping struct {
	MyASN    string `yaml:"my-asn"`
	ASN      string `yaml:"peer-asn"`
	Addr     string `yaml:"peer-address"`
	SrcAddr  string `yaml:"source-address"`
	Port     string `yaml:"peer-port"`
	HoldTime string `yaml:"hold-time"`
	RouterID string `yaml:"router-id"`
}

type nodeSelector struct {
	MatchLabels      map[string]string      `yaml:"match-labels"`
	MatchExpressions []selectorRequirements `yaml:"match-expressions"`
}

type selectorRequirements struct {
	Key      string   `yaml:"key"`
	Operator string   `yaml:"operator"`
	Values   []string `yaml:"values"`
}

type addressPool struct {
	Protocol          Proto
	Name              string
	Addresses         []string
	AvoidBuggyIPs     bool               `yaml:"avoid-buggy-ips"`
	AutoAssign        *bool              `yaml:"auto-assign"`
	BGPAdvertisements []bgpAdvertisement `yaml:"bgp-advertisements"`
}

type bgpAdvertisement struct {
	AggregationLength *int `yaml:"aggregation-length"`
	LocalPref         *uint32
	Communities       []string
}

// Config is a parsed MetalLB configuration.
type Config struct {
	// Routers that MetalLB should peer with.
	Peers []*Peer
	// Peer autodiscovery configuration.
	PeerAutodiscovery *PeerAutodiscovery
	// Address pools from which to allocate load balancer IPs.
	Pools map[string]*Pool
}

// Proto holds the protocol we are speaking.
type Proto string

// MetalLB supported protocols.
const (
	BGP    Proto = "bgp"
	Layer2 Proto = "layer2"
)

// Peer is the configuration of a BGP peering session.
type Peer struct {
	// AS number to use for the local end of the session.
	MyASN uint32
	// AS number to expect from the remote end of the session.
	ASN uint32
	// Address to dial when establishing the session.
	Addr net.IP
	// Source address to use when establishing the session.
	SrcAddr net.IP
	// Port to dial when establishing the session.
	Port uint16
	// Requested BGP hold time, per RFC4271.
	HoldTime time.Duration
	// BGP router ID to advertise to the peer
	RouterID net.IP
	// Only connect to this peer on nodes that match one of these
	// selectors.
	NodeSelectors []labels.Selector
	// Authentication password for routers enforcing TCP MD5 authenticated sessions
	Password string
	// TODO: more BGP session settings
}

// PeerAutodiscoveryDefaults specifies BGP peering parameters which can be set
// globally for all autodiscovered peers.
type PeerAutodiscoveryDefaults struct {
	// AS number to use for the local end of the session.
	MyASN uint32
	// AS number to expect from the remote end of the session.
	ASN uint32
	// Address to dial when establishing the session.
	Addr net.IP
	// Source address to use when establishing the session.
	SrcAddr net.IP
	// Port to dial when establishing the session.
	Port uint16
	// Requested BGP hold time, per RFC4271.
	HoldTime time.Duration
	// BGP router ID to advertise to the peer.
	RouterID net.IP
}

// PeerAutodiscoveryMapping maps BGP peering configuration parameters to node
// annotations or labels to allow automatic discovery of BGP configuration
// from Node objects.
//
// All the fields are strings because the values of a PeerAutodiscoveryMapping
// are annotation/label keys rather than the BGP parameters themselves. The
// controller uses the PeerAutodiscoveryMapping to figure out which annotations
// or labels to get the BGP parameters from for a given node peer.
//
// For example, setting MyASN to example.com/my-asn means "look for an
// annotation or label with the key example.com/my-asn on a Node object and use
// its value as the local ASN when constructing a BGP peer".
//
// Whether to use annotations or labels is out of scope for this type: since
// both annotations and labels use string keys, a PeerAutodiscoveryMapping can
// be used to map BGP parameters to either annotations or labels.
type PeerAutodiscoveryMapping struct {
	MyASN    string
	ASN      string
	Addr     string
	SrcAddr  string
	Port     string
	HoldTime string
	RouterID string
}

// PeerAutodiscovery defines automatic discovery of BGP peers using annotations
// and/or labels. It allows the user to tell MetalLB to retrieve BGP peering
// configuration dynamically rather than from a static configuration file.
type PeerAutodiscovery struct {
	// Defaults specifies BGP peering parameters which should be used for all
	// autodiscovered peers. The default value of a parameter is used when the
	// same parameter isn't specified in annotations/labels on the Node object.
	Defaults *PeerAutodiscoveryDefaults
	// FromAnnotations tells MetalLB to retrieve BGP peering configuration for
	// a node by looking up specific annotations on the corresponding Node
	// object. Multiple mappings can be specified in order to discover multiple
	// BGP peers.
	FromAnnotations []*PeerAutodiscoveryMapping
	// FromLabels tells MetalLB to retrieve BGP peering configuration for
	// a node by looking up specific labels on the corresponding Node object.
	// Multiple mappings can be specified in order to discover multiple BGP
	// peers.
	FromLabels []*PeerAutodiscoveryMapping
	// NodeSelectors indicates the nodes for which peer autodiscovery should be
	// enabled. Autodiscovery is performed only for nodes whose label set
	// matches the specified selectors. When no selectors are specified,
	// autodiscovery is performed for all nodes.
	NodeSelectors []labels.Selector
}

// Pool is the configuration of an IP address pool.
type Pool struct {
	// Protocol for this pool.
	Protocol Proto
	// The addresses that are part of this pool, expressed as CIDR
	// prefixes. config.Parse guarantees that these are
	// non-overlapping, both within and between pools.
	CIDR []*net.IPNet
	// Some buggy consumer devices mistakenly drop IPv4 traffic for IP
	// addresses ending in .0 or .255, due to poor implementations of
	// smurf protection. This setting marks such addresses as
	// unusable, for maximum compatibility with ancient parts of the
	// internet.
	AvoidBuggyIPs bool
	// If false, prevents IP addresses to be automatically assigned
	// from this pool.
	AutoAssign bool
	// When an IP is allocated from this pool, how should it be
	// translated into BGP announcements?
	BGPAdvertisements []*BGPAdvertisement
}

// BGPAdvertisement describes one translation from an IP address to a BGP advertisement.
type BGPAdvertisement struct {
	// Roll up the IP address into a CIDR prefix of this
	// length. Optional, defaults to 32 (i.e. no aggregation) if not
	// specified.
	AggregationLength int
	// Value of the LOCAL_PREF BGP path attribute. Used only when
	// advertising to IBGP peers (i.e. Peer.MyASN == Peer.ASN).
	LocalPref uint32
	// Value of the COMMUNITIES path attribute.
	Communities map[uint32]bool
}

func parseNodeSelector(ns *nodeSelector) (labels.Selector, error) {
	if len(ns.MatchLabels)+len(ns.MatchExpressions) == 0 {
		return labels.Everything(), nil
	}

	// Convert to a metav1.LabelSelector so we can use the fancy
	// parsing function to create a Selector.
	//
	// Why not use metav1.LabelSelector in the raw config object?
	// Because metav1.LabelSelector doesn't have yaml tag
	// specifications.
	sel := &metav1.LabelSelector{
		MatchLabels: ns.MatchLabels,
	}
	for _, req := range ns.MatchExpressions {
		sel.MatchExpressions = append(sel.MatchExpressions, metav1.LabelSelectorRequirement{
			Key:      req.Key,
			Operator: metav1.LabelSelectorOperator(req.Operator),
			Values:   req.Values,
		})
	}

	return metav1.LabelSelectorAsSelector(sel)
}

// parseNodeSelectors parses the provided nodeSelector slice ns and returns a
// slice of Selectors which can be used to match against labels. If ns is
// empty, a "match everything" Selector is returned.
func parseNodeSelectors(ns []nodeSelector) ([]labels.Selector, error) {
	if len(ns) == 0 {
		return []labels.Selector{labels.Everything()}, nil
	}

	nodeSels := []labels.Selector{}
	for _, sel := range ns {
		nodeSel, err := parseNodeSelector(&sel)
		if err != nil {
			return nil, fmt.Errorf("parsing node selector: %w", err)
		}
		nodeSels = append(nodeSels, nodeSel)
	}

	return nodeSels, nil
}

// ParseHoldTime parses the provided string into a BGP hold time value
// represented as a time.Duration.
func ParseHoldTime(ht string) (time.Duration, error) {
	d, err := time.ParseDuration(ht)
	if err != nil {
		return 0, fmt.Errorf("invalid hold time %q: %s", ht, err)
	}
	rounded := time.Duration(int(d.Seconds())) * time.Second
	if rounded != 0 && rounded < 3*time.Second {
		return 0, fmt.Errorf("invalid hold time %q: must be 0 or >=3s", ht)
	}
	return rounded, nil
}

// Parse loads and validates a Config from bs.
func Parse(bs []byte) (*Config, error) {
	var raw configFile
	if err := yaml.UnmarshalStrict(bs, &raw); err != nil {
		return nil, fmt.Errorf("could not parse config: %s", err)
	}

	cfg := &Config{Pools: map[string]*Pool{}}
	for i, p := range raw.Peers {
		peer, err := parsePeer(p)
		if err != nil {
			return nil, fmt.Errorf("parsing peer #%d: %s", i+1, err)
		}
		for _, ep := range cfg.Peers {
			// TODO: Be smarter regarding conflicting peers. For example, two
			// peers could have a different hold time but they'd still result
			// in two BGP sessions between the speaker and the remote host.
			if reflect.DeepEqual(peer, ep) {
				return nil, fmt.Errorf("peer #%d already exists", i+1)
			}
		}
		cfg.Peers = append(cfg.Peers, peer)
	}

	if raw.PeerAutodiscovery != nil {
		pad, err := parsePeerAutodiscovery(*raw.PeerAutodiscovery)
		if err != nil {
			return nil, fmt.Errorf("parsing peer autodiscovery: %w", err)
		}
		cfg.PeerAutodiscovery = pad
	}

	communities := map[string]uint32{}
	for n, v := range raw.BGPCommunities {
		c, err := parseCommunity(v)
		if err != nil {
			return nil, fmt.Errorf("parsing community %q: %s", n, err)
		}
		communities[n] = c
	}

	var allCIDRs []*net.IPNet
	for i, p := range raw.Pools {
		pool, err := parseAddressPool(p, communities)
		if err != nil {
			return nil, fmt.Errorf("parsing address pool #%d: %s", i+1, err)
		}

		// Check that the pool isn't already defined
		if cfg.Pools[p.Name] != nil {
			return nil, fmt.Errorf("duplicate definition of pool %q", p.Name)
		}

		// Check that all specified CIDR ranges are non-overlapping.
		for _, cidr := range pool.CIDR {
			for _, m := range allCIDRs {
				if cidrsOverlap(cidr, m) {
					return nil, fmt.Errorf("CIDR %q in pool %q overlaps with already defined CIDR %q", cidr, p.Name, m)
				}
			}
			allCIDRs = append(allCIDRs, cidr)
		}

		cfg.Pools[p.Name] = pool
	}

	return cfg, nil
}

func parsePeer(p peer) (*Peer, error) {
	if p.MyASN == 0 {
		return nil, errors.New("missing local ASN")
	}
	if p.ASN == 0 {
		return nil, errors.New("missing peer ASN")
	}
	ip := net.ParseIP(p.Addr)
	if ip == nil {
		return nil, fmt.Errorf("invalid peer IP %q", p.Addr)
	}
	holdTime := DefaultBGPHoldTime * time.Second
	if p.HoldTime != "" {
		ht, err := ParseHoldTime(p.HoldTime)
		if err != nil {
			return nil, err
		}
		holdTime = ht
	}
	port := uint16(DefautBGPPeerPort)
	if p.Port != 0 {
		port = p.Port
	}
	// Ideally we would set a default RouterID here, instead of having
	// to do it elsewhere in the code. Unfortunately, we don't know
	// the node IP here.
	var routerID net.IP
	if p.RouterID != "" {
		routerID = net.ParseIP(p.RouterID)
		if routerID == nil {
			return nil, fmt.Errorf("invalid router ID %q", p.RouterID)
		}
	}
	src := net.ParseIP(p.SrcAddr)
	if p.SrcAddr != "" && src == nil {
		return nil, fmt.Errorf("invalid source IP %q", p.SrcAddr)
	}

	nodeSels, err := parseNodeSelectors(p.NodeSelectors)
	if err != nil {
		return nil, err
	}

	var password string
	if p.Password != "" {
		password = p.Password
	}
	return &Peer{
		MyASN:         p.MyASN,
		ASN:           p.ASN,
		Addr:          ip,
		SrcAddr:       src,
		Port:          port,
		HoldTime:      holdTime,
		RouterID:      routerID,
		NodeSelectors: nodeSels,
		Password:      password,
	}, nil
}

// parsePeerAutodiscovery parses peer autodiscovery configuration, constructs
// a PeerAutodiscovery and returns a pointer to it.
func parsePeerAutodiscovery(p peerAutodiscovery) (*PeerAutodiscovery, error) {
	pad := &PeerAutodiscovery{
		Defaults: &PeerAutodiscoveryDefaults{},
	}

	if p.FromAnnotations == nil && p.FromLabels == nil {
		return nil, errors.New("missing from-annotations or from-labels")
	}

	if p.Defaults != nil {
		pad.Defaults.ASN = p.Defaults.ASN
		pad.Defaults.MyASN = p.Defaults.MyASN
		pad.Defaults.Addr = net.ParseIP(p.Defaults.Addr)
		pad.Defaults.SrcAddr = net.ParseIP(p.Defaults.SrcAddr)
		pad.Defaults.Port = p.Defaults.Port
		pad.Defaults.RouterID = net.ParseIP(p.Defaults.RouterID)

		if p.Defaults.HoldTime != "" {
			ht, err := ParseHoldTime(p.Defaults.HoldTime)
			if err != nil {
				return nil, fmt.Errorf("parsing default hold time: %w", err)
			}
			pad.Defaults.HoldTime = ht
		}
	}

	if p.FromAnnotations != nil {
		for _, pam := range p.FromAnnotations {
			pad.FromAnnotations = append(pad.FromAnnotations, &PeerAutodiscoveryMapping{
				ASN:      pam.ASN,
				Addr:     pam.Addr,
				SrcAddr:  pam.SrcAddr,
				HoldTime: pam.HoldTime,
				MyASN:    pam.MyASN,
				Port:     pam.Port,
				RouterID: pam.RouterID,
			})
		}
	}

	if p.FromLabels != nil {
		for _, pam := range p.FromLabels {
			pad.FromLabels = append(pad.FromLabels, &PeerAutodiscoveryMapping{
				ASN:      pam.ASN,
				Addr:     pam.Addr,
				SrcAddr:  pam.SrcAddr,
				HoldTime: pam.HoldTime,
				MyASN:    pam.MyASN,
				Port:     pam.Port,
				RouterID: pam.RouterID,
			})
		}
	}

	nodeSels, err := parseNodeSelectors(p.NodeSelectors)
	if err != nil {
		return nil, err
	}
	pad.NodeSelectors = nodeSels

	if err := validatePeerAutodiscovery(*pad); err != nil {
		return nil, fmt.Errorf("validating peer autodiscovery: %w", err)
	}

	return pad, nil
}

func parseAddressPool(p addressPool, bgpCommunities map[string]uint32) (*Pool, error) {
	if p.Name == "" {
		return nil, errors.New("missing pool name")
	}

	ret := &Pool{
		Protocol:      p.Protocol,
		AvoidBuggyIPs: p.AvoidBuggyIPs,
		AutoAssign:    true,
	}

	if p.AutoAssign != nil {
		ret.AutoAssign = *p.AutoAssign
	}

	if len(p.Addresses) == 0 {
		return nil, errors.New("pool has no prefixes defined")
	}
	for _, cidr := range p.Addresses {
		nets, err := parseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q in pool %q: %s", cidr, p.Name, err)
		}
		ret.CIDR = append(ret.CIDR, nets...)
	}

	switch ret.Protocol {
	case Layer2:
		if len(p.BGPAdvertisements) > 0 {
			return nil, errors.New("cannot have bgp-advertisements configuration element in a layer2 address pool")
		}
	case BGP:
		ads, err := parseBGPAdvertisements(p.BGPAdvertisements, ret.CIDR, bgpCommunities)
		if err != nil {
			return nil, fmt.Errorf("parsing BGP communities: %s", err)
		}
		ret.BGPAdvertisements = ads
	case "":
		return nil, errors.New("address pool is missing the protocol field")
	default:
		return nil, fmt.Errorf("unknown protocol %q", ret.Protocol)
	}

	return ret, nil
}

func parseBGPAdvertisements(ads []bgpAdvertisement, cidrs []*net.IPNet, communities map[string]uint32) ([]*BGPAdvertisement, error) {
	if len(ads) == 0 {
		return []*BGPAdvertisement{
			{
				AggregationLength: 32,
				LocalPref:         0,
				Communities:       map[uint32]bool{},
			},
		}, nil
	}

	var ret []*BGPAdvertisement
	for _, rawAd := range ads {
		ad := &BGPAdvertisement{
			AggregationLength: 32,
			LocalPref:         0,
			Communities:       map[uint32]bool{},
		}

		if rawAd.AggregationLength != nil {
			ad.AggregationLength = *rawAd.AggregationLength
		}
		if ad.AggregationLength > 32 {
			return nil, fmt.Errorf("invalid aggregation length %q", ad.AggregationLength)
		}
		for _, cidr := range cidrs {
			o, _ := cidr.Mask.Size()
			if ad.AggregationLength < o {
				return nil, fmt.Errorf("invalid aggregation length %d: prefix %q in this pool is more specific than the aggregation length", ad.AggregationLength, cidr)
			}
		}

		if rawAd.LocalPref != nil {
			ad.LocalPref = *rawAd.LocalPref
		}

		for _, c := range rawAd.Communities {
			if v, ok := communities[c]; ok {
				ad.Communities[v] = true
			} else {
				v, err := parseCommunity(c)
				if err != nil {
					return nil, fmt.Errorf("invalid community %q in BGP advertisement: %s", c, err)
				}
				ad.Communities[v] = true
			}
		}

		ret = append(ret, ad)
	}

	return ret, nil
}

func parseCommunity(c string) (uint32, error) {
	fs := strings.Split(c, ":")
	if len(fs) != 2 {
		return 0, fmt.Errorf("invalid community string %q", c)
	}
	a, err := strconv.ParseUint(fs[0], 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid first section of community %q: %s", fs[0], err)
	}
	b, err := strconv.ParseUint(fs[1], 10, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid second section of community %q: %s", fs[0], err)
	}

	return (uint32(a) << 16) + uint32(b), nil
}

func parseCIDR(cidr string) ([]*net.IPNet, error) {
	if !strings.Contains(cidr, "-") {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", cidr)
		}
		return []*net.IPNet{n}, nil
	}

	fs := strings.SplitN(cidr, "-", 2)
	if len(fs) != 2 {
		return nil, fmt.Errorf("invalid IP range %q", cidr)
	}
	start := net.ParseIP(strings.TrimSpace(fs[0]))
	if start == nil {
		return nil, fmt.Errorf("invalid IP range %q: invalid start IP %q", cidr, fs[0])
	}
	end := net.ParseIP(strings.TrimSpace(fs[1]))
	if end == nil {
		return nil, fmt.Errorf("invalid IP range %q: invalid end IP %q", cidr, fs[1])
	}

	var ret []*net.IPNet
	for _, pfx := range ipaddr.Summarize(start, end) {
		n := &net.IPNet{
			IP:   pfx.IP,
			Mask: pfx.Mask,
		}
		ret = append(ret, n)
	}
	return ret, nil
}

func cidrsOverlap(a, b *net.IPNet) bool {
	return cidrContainsCIDR(a, b) || cidrContainsCIDR(b, a)
}

func cidrContainsCIDR(outer, inner *net.IPNet) bool {
	ol, _ := outer.Mask.Size()
	il, _ := inner.Mask.Size()
	if ol == il && outer.IP.Equal(inner.IP) {
		return true
	}
	if ol < il && outer.Contains(inner.IP) {
		return true
	}
	return false
}

// validatePeerAutodiscovery verifies that peer autodiscovery config is
// specified for all required BGP params, or that default values are in place.
func validatePeerAutodiscovery(p PeerAutodiscovery) error {
	if p.FromAnnotations != nil {
		for i, m := range p.FromAnnotations {
			if m.MyASN == "" && p.Defaults.MyASN == 0 {
				return fmt.Errorf("peer autodiscovery annotations mapping %d: local ASN missing and no default specified", i)
			}
			if m.ASN == "" && p.Defaults.ASN == 0 {
				return fmt.Errorf("peer autodiscovery annotations mapping %d: peer ASN missing and no default specified", i)
			}
			if m.Addr == "" && p.Defaults.Addr == nil {
				return fmt.Errorf("peer autodiscovery annotations mapping %d: peer address missing and no default specified", i)
			}
		}
	}

	if p.FromLabels != nil {
		for i, m := range p.FromLabels {
			if m.MyASN == "" && p.Defaults.MyASN == 0 {
				return fmt.Errorf("peer autodiscovery labels mapping %d: local ASN missing and no default specified", i)
			}
			if m.ASN == "" && p.Defaults.ASN == 0 {
				return fmt.Errorf("peer autodiscovery labels mapping %d: peer ASN missing and no default specified", i)
			}
			if m.Addr == "" && p.Defaults.Addr == nil {
				return fmt.Errorf("peer autodiscovery labels mapping %d: peer address missing and no default specified", i)
			}
		}
	}

	return nil
}
