package main

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"go.universe.tf/metallb/internal/bgp"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"

	"github.com/go-kit/kit/log"
	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func strptr(s string) *string {
	return &s
}

func mustSelector(s string) labels.Selector {
	res, err := labels.Parse(s)
	if err != nil {
		panic(err)
	}
	return res
}

func ipnet(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func statusAssigned(ip string) v1.ServiceStatus {
	return v1.ServiceStatus{
		LoadBalancer: v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{
				{
					IP: ip,
				},
			},
		},
	}
}

func sortAds(ads map[string][]*bgp.Advertisement) {
	if len(ads) == 0 {
		return
	}

	for _, v := range ads {
		if len(v) == 0 {
			continue
		}
		sort.Slice(v, func(i, j int) bool {
			a, b := v[i], v[j]
			if a.Prefix.String() != b.Prefix.String() {
				return a.Prefix.String() < b.Prefix.String()
			}
			if a.LocalPref != b.LocalPref {
				return a.LocalPref < b.LocalPref
			}
			if a.NextHop.String() != b.NextHop.String() {
				return a.NextHop.String() < b.NextHop.String()
			}
			if len(a.Communities) != len(b.Communities) {
				return len(a.Communities) < len(b.Communities)
			}
			sort.Slice(a.Communities, func(i, j int) bool { return a.Communities[i] < a.Communities[j] })
			sort.Slice(b.Communities, func(i, j int) bool { return b.Communities[i] < b.Communities[j] })
			for k := range a.Communities {
				if a.Communities[k] != b.Communities[k] {
					return a.Communities[k] < b.Communities[k]
				}
			}
			return false
		})
	}
}

type fakeBGP struct {
	t *testing.T

	sync.Mutex
	// peer IP -> advertisements
	gotAds map[string][]*bgp.Advertisement
}

func (f *fakeBGP) New(_ log.Logger, addr string, _ net.IP, _ uint32, _ net.IP, _ uint32, _ time.Duration, _, _ string) (session, error) {
	f.Lock()
	defer f.Unlock()

	if _, ok := f.gotAds[addr]; ok {
		f.t.Errorf("Tried to create already existing BGP session to %q", addr)
		return nil, errors.New("invariant violation")
	}
	// Nil because we haven't programmed any routes for it yet, but
	// the key now exists in the map.
	f.gotAds[addr] = nil
	return &fakeSession{
		f:    f,
		addr: addr,
	}, nil
}

func (f *fakeBGP) Ads() map[string][]*bgp.Advertisement {
	ret := map[string][]*bgp.Advertisement{}

	f.Lock()
	defer f.Unlock()

	// Make a deep copy so that we can release the lock.
	for k, v := range f.gotAds {
		if v == nil {
			ret[k] = nil
			continue
		}
		s := []*bgp.Advertisement{}
		for _, ad := range v {
			adCopy := new(bgp.Advertisement)
			*adCopy = *ad
			s = append(s, adCopy)
		}
		ret[k] = s
	}

	return ret
}

type fakeSession struct {
	f    *fakeBGP
	addr string
}

func (f *fakeSession) Close() error {
	f.f.Lock()
	defer f.f.Unlock()

	if _, ok := f.f.gotAds[f.addr]; !ok {
		f.f.t.Errorf("Tried to close non-existent session to %q", f.addr)
		return errors.New("invariant violation")
	}

	delete(f.f.gotAds, f.addr)
	return nil
}

func (f *fakeSession) Set(ads ...*bgp.Advertisement) error {
	f.f.Lock()
	defer f.f.Unlock()

	if _, ok := f.f.gotAds[f.addr]; !ok {
		f.f.t.Errorf("Tried to set ads on non-existent session to %q", f.addr)
		return errors.New("invariant violation")
	}

	f.f.gotAds[f.addr] = ads
	return nil
}

// testK8S implements service by recording what the controller wants
// to do to k8s.
type testK8S struct {
	loggedWarning bool
	t             *testing.T
}

func (s *testK8S) UpdateStatus(svc *v1.Service) error {
	panic("never called")
}

func (s *testK8S) Infof(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Info event %q: %s", evtType, fmt.Sprintf(msg, args...))
}

func (s *testK8S) Errorf(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Warning event %q: %s", evtType, fmt.Sprintf(msg, args...))
	s.loggedWarning = true
}

func TestBGPSpeaker(t *testing.T) {
	b := &fakeBGP{
		t:      t,
		gotAds: map[string][]*bgp.Advertisement{},
	}
	newBGP = b.New
	c, err := newController(controllerConfig{
		MyNode:        "pandora",
		DisableLayer2: true,
	})
	if err != nil {
		t.Fatalf("creating controller: %s", err)
	}
	c.client = &testK8S{t: t}

	tests := []struct {
		desc string

		balancer string
		config   *config.Config
		svc      *v1.Service
		eps      k8s.EpsOrSlices

		wantAds map[string][]*bgp.Advertisement
	}{
		{
			desc:     "Service ignored, no config",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{},
		},

		{
			desc: "One peer, no services",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				Pools: map[string]*config.Pool{
					"default": {
						Protocol: config.BGP,
						CIDR:     []*net.IPNet{ipnet("10.20.30.0/24")},
						BGPAdvertisements: []*config.BGPAdvertisement{
							{
								AggregationLength: 32,
							},
						},
					},
				},
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "Add service, not an LB",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "ClusterIP",
					ExternalTrafficPolicy: "Cluster",
				},
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("pandora"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "Add service, it's an LB!",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "LB switches to local traffic policy, endpoint isn't on our node",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Local",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "New endpoint, on our node",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Local",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
								{
									IP:       "2.3.4.6",
									NodeName: strptr("pandora"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Endpoint on our node has some unready ports",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Local",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
								{
									IP:       "2.3.4.6",
									NodeName: strptr("pandora"),
								},
							},
						},
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
							NotReadyAddresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.6",
									NodeName: strptr("pandora"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "Endpoint list is empty",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "Endpoint list contains only unhealthy endpoints",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							NotReadyAddresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "Endpoint list contains some unhealthy endpoints",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
							NotReadyAddresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.6",
									NodeName: strptr("pandora"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc: "Multiple advertisement config",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				Pools: map[string]*config.Pool{
					"default": {
						Protocol: config.BGP,
						CIDR:     []*net.IPNet{ipnet("10.20.30.0/24")},
						BGPAdvertisements: []*config.BGPAdvertisement{
							{
								AggregationLength: 32,
								LocalPref:         100,
								Communities:       map[uint32]bool{1234: true, 2345: true},
							},
							{
								AggregationLength: 24,
								LocalPref:         1000,
							},
						},
					},
				},
			},
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix:      ipnet("10.20.30.1/32"),
						LocalPref:   100,
						Communities: []uint32{1234, 2345},
					},
					{
						Prefix:    ipnet("10.20.30.0/24"),
						LocalPref: 1000,
					},
				},
			},
		},

		{
			desc: "Multiple peers",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
					{
						Addr:          net.ParseIP("1.2.3.5"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				Pools: map[string]*config.Pool{
					"default": {
						Protocol: config.BGP,
						CIDR:     []*net.IPNet{ipnet("10.20.30.0/24")},
						BGPAdvertisements: []*config.BGPAdvertisement{
							{
								AggregationLength: 32,
							},
						},
					},
				},
			},
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Second balancer, no ingress assigned",
			balancer: "test2",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Second balancer, ingress gets assigned",
			balancer: "test2",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.5"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
					{
						Prefix: ipnet("10.20.30.5/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
					{
						Prefix: ipnet("10.20.30.5/32"),
					},
				},
			},
		},

		{
			desc:     "Second balancer, ingress shared with first",
			balancer: "test2",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					// Prefixes duplicated because the dedupe happens
					// inside the real BGP session.
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Delete svc",
			balancer: "test1",
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc: "Delete peer",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.5"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				Pools: map[string]*config.Pool{
					"default": {
						Protocol: config.BGP,
						CIDR:     []*net.IPNet{ipnet("10.20.30.0/24")},
						BGPAdvertisements: []*config.BGPAdvertisement{
							{
								AggregationLength: 32,
							},
						},
					},
				},
			},
			balancer: "test2",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				EpVal: &v1.Endpoints{
					Subsets: []v1.EndpointSubset{
						{
							Addresses: []v1.EndpointAddress{
								{
									IP:       "2.3.4.5",
									NodeName: strptr("iris"),
								},
							},
						},
					},
				},
				Type: k8s.Eps,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Delete second svc",
			balancer: "test2",
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.5:0": nil,
			},
		},
	}

	l := log.NewNopLogger()
	for _, test := range tests {
		if test.config != nil {
			if c.SetConfig(l, test.config) == k8s.SyncStateError {
				t.Errorf("%q: SetConfig failed", test.desc)
			}
		}
		if test.balancer != "" {
			if c.SetBalancer(l, test.balancer, test.svc, test.eps) == k8s.SyncStateError {
				t.Errorf("%q: SetBalancer failed", test.desc)
			}
		}

		gotAds := b.Ads()
		sortAds(test.wantAds)
		sortAds(gotAds)
		if diff := cmp.Diff(test.wantAds, gotAds); diff != "" {
			t.Errorf("%q: unexpected advertisement state (-want +got)\n%s", test.desc, diff)
		}
	}
}

func TestBGPSpeakerEPSlices(t *testing.T) {
	b := &fakeBGP{
		t:      t,
		gotAds: map[string][]*bgp.Advertisement{},
	}
	newBGP = b.New
	c, err := newController(controllerConfig{
		MyNode:        "pandora",
		DisableLayer2: true,
	})
	if err != nil {
		t.Fatalf("creating controller: %s", err)
	}
	c.client = &testK8S{t: t}

	tests := []struct {
		desc string

		balancer string
		config   *config.Config
		svc      *v1.Service
		eps      k8s.EpsOrSlices

		wantAds map[string][]*bgp.Advertisement
	}{
		{
			desc:     "Service ignored, no config",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{},
		},
		{
			desc: "One peer, no services",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				Pools: map[string]*config.Pool{
					"default": {
						Protocol: config.BGP,
						CIDR:     []*net.IPNet{ipnet("10.20.30.0/24")},
						BGPAdvertisements: []*config.BGPAdvertisement{
							{
								AggregationLength: 32,
							},
						},
					},
				},
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},
		{
			desc:     "Add service, not an LB",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "ClusterIP",
					ExternalTrafficPolicy: "Cluster",
				},
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "pandora",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "Add service, it's an LB!",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "LB switches to local traffic policy, endpoint isn't on our node",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Local",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "New endpoint, on our node",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Local",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.6",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "pandora",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Endpoint on our node has some unready ports",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Local",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.6",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "pandora",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(false),
								},
							},
						},
					},
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.7",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.6",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "pandora",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "Endpoint list is empty",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "Endpoint list contains only unhealthy endpoints",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(false),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc:     "Endpoint list contains some unhealthy endpoints",
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
							{
								Addresses: []string{
									"2.3.4.6",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "pandora",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(false),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc: "Multiple advertisement config",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				Pools: map[string]*config.Pool{
					"default": {
						Protocol: config.BGP,
						CIDR:     []*net.IPNet{ipnet("10.20.30.0/24")},
						BGPAdvertisements: []*config.BGPAdvertisement{
							{
								AggregationLength: 32,
								LocalPref:         100,
								Communities:       map[uint32]bool{1234: true, 2345: true},
							},
							{
								AggregationLength: 24,
								LocalPref:         1000,
							},
						},
					},
				},
			},
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix:      ipnet("10.20.30.1/32"),
						LocalPref:   100,
						Communities: []uint32{1234, 2345},
					},
					{
						Prefix:    ipnet("10.20.30.0/24"),
						LocalPref: 1000,
					},
				},
			},
		},

		{
			desc: "Multiple peers",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
					{
						Addr:          net.ParseIP("1.2.3.5"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				Pools: map[string]*config.Pool{
					"default": {
						Protocol: config.BGP,
						CIDR:     []*net.IPNet{ipnet("10.20.30.0/24")},
						BGPAdvertisements: []*config.BGPAdvertisement{
							{
								AggregationLength: 32,
							},
						},
					},
				},
			},
			balancer: "test1",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Second balancer, no ingress assigned",
			balancer: "test2",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Second balancer, ingress gets assigned",
			balancer: "test2",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.5"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
					{
						Prefix: ipnet("10.20.30.5/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
					{
						Prefix: ipnet("10.20.30.5/32"),
					},
				},
			},
		},

		{
			desc:     "Second balancer, ingress shared with first",
			balancer: "test2",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					// Prefixes duplicated because the dedupe happens
					// inside the real BGP session.
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Delete svc",
			balancer: "test1",
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc: "Delete peer",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.5"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				Pools: map[string]*config.Pool{
					"default": {
						Protocol: config.BGP,
						CIDR:     []*net.IPNet{ipnet("10.20.30.0/24")},
						BGPAdvertisements: []*config.BGPAdvertisement{
							{
								AggregationLength: 32,
							},
						},
					},
				},
			},
			balancer: "test2",
			svc: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					ExternalTrafficPolicy: "Cluster",
				},
				Status: statusAssigned("10.20.30.1"),
			},
			eps: k8s.EpsOrSlices{
				SlicesVal: []*discovery.EndpointSlice{
					{
						Endpoints: []discovery.Endpoint{
							{
								Addresses: []string{
									"2.3.4.5",
								},
								Topology: map[string]string{
									"kubernetes.io/hostname": "iris",
								},
								Conditions: discovery.EndpointConditions{
									Ready: boolPtr(true),
								},
							},
						},
					},
				},
				Type: k8s.Slices,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.5:0": {
					{
						Prefix: ipnet("10.20.30.1/32"),
					},
				},
			},
		},

		{
			desc:     "Delete second svc",
			balancer: "test2",
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.5:0": nil,
			},
		},
	}

	l := log.NewNopLogger()
	for _, test := range tests {
		if test.config != nil {
			if c.SetConfig(l, test.config) == k8s.SyncStateError {
				t.Errorf("%q: SetConfig failed", test.desc)
			}
		}
		if test.balancer != "" {
			if c.SetBalancer(l, test.balancer, test.svc, test.eps) == k8s.SyncStateError {
				t.Errorf("%q: SetBalancer failed", test.desc)
			}
		}

		gotAds := b.Ads()
		sortAds(test.wantAds)
		sortAds(gotAds)
		if diff := cmp.Diff(test.wantAds, gotAds); diff != "" {
			t.Errorf("%q: unexpected advertisement state (-want +got)\n%s", test.desc, diff)
		}
	}
}

func TestNodeSelectors(t *testing.T) {
	b := &fakeBGP{
		t:      t,
		gotAds: map[string][]*bgp.Advertisement{},
	}
	newBGP = b.New
	c, err := newController(controllerConfig{
		MyNode:        "pandora",
		DisableLayer2: true,
	})
	if err != nil {
		t.Fatalf("creating controller: %s", err)
	}
	c.client = &testK8S{t: t}

	pools := map[string]*config.Pool{
		"default": {
			Protocol: config.BGP,
			CIDR:     []*net.IPNet{ipnet("1.2.3.0/24")},
			BGPAdvertisements: []*config.BGPAdvertisement{
				{
					AggregationLength: 32,
				},
			},
		},
	}

	tests := []struct {
		desc    string
		config  *config.Config
		node    *v1.Node
		wantAds map[string][]*bgp.Advertisement
	}{
		{
			desc:    "No config, no advertisements",
			wantAds: map[string][]*bgp.Advertisement{},
		},

		{
			desc: "One peer, default node selector, no node labels",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				Pools: pools,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc: "Second peer, non-matching node selector",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
					{
						Addr: net.ParseIP("2.3.4.5"),
						NodeSelectors: []labels.Selector{
							mustSelector("foo=bar"),
						},
					},
				},
				Pools: pools,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc: "Add node label that matches",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"foo": "bar",
					},
				},
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
				"2.3.4.5:0": nil,
			},
		},

		{
			desc: "Change node label so it no longer matches",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"foo": "baz",
					},
				},
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
			},
		},

		{
			desc: "Change node selector so it matches again",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
					{
						Addr: net.ParseIP("2.3.4.5"),
						NodeSelectors: []labels.Selector{
							mustSelector("foo in (bar, baz)"),
						},
					},
				},
				Pools: pools,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
				"2.3.4.5:0": nil,
			},
		},

		{
			desc: "Change node label back, still matches",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"foo": "bar",
					},
				},
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
				"2.3.4.5:0": nil,
			},
		},

		{
			desc: "Multiple node selectors, only one matches",
			config: &config.Config{
				Peers: []*config.Peer{
					{
						Addr:          net.ParseIP("1.2.3.4"),
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
					{
						Addr: net.ParseIP("2.3.4.5"),
						NodeSelectors: []labels.Selector{
							mustSelector("host=frontend"),
							mustSelector("foo in (bar, baz)"),
						},
					},
				},
				Pools: pools,
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
				"2.3.4.5:0": nil,
			},
		},

		{
			desc: "Change node labels to match the other selector",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"host": "frontend",
					},
				},
			},
			wantAds: map[string][]*bgp.Advertisement{
				"1.2.3.4:0": nil,
				"2.3.4.5:0": nil,
			},
		},
	}

	l := log.NewNopLogger()
	for _, test := range tests {
		if test.config != nil {
			if c.SetConfig(l, test.config) == k8s.SyncStateError {
				t.Errorf("%q: SetConfig failed", test.desc)
			}
		}

		if test.node != nil {
			if c.SetNode(l, test.node) == k8s.SyncStateError {
				t.Errorf("%q: SetNode failed", test.desc)
			}
		}

		gotAds := b.Ads()
		sortAds(test.wantAds)
		sortAds(gotAds)
		if diff := cmp.Diff(test.wantAds, gotAds); diff != "" {
			t.Errorf("%q: unexpected advertisement state (-want +got)\n%s", test.desc, diff)
		}
	}
}

func TestParseNodePeer(t *testing.T) {
	pam := &config.PeerAutodiscoveryMapping{
		MyASN:    "example.com/my-asn",
		ASN:      "example.com/asn",
		Addr:     "example.com/addr",
		SrcAddr:  "example.com/srcaddr",
		Port:     "example.com/port",
		HoldTime: "example.com/hold-time",
		RouterID: "example.com/router-id",
	}

	tests := []struct {
		desc     string
		ls       labels.Set
		defaults *config.PeerAutodiscoveryDefaults
		mapping  *config.PeerAutodiscoveryMapping
		wantPeer *config.Peer
	}{
		{
			desc: "Full config",
			ls: labels.Set(map[string]string{
				"example.com/my-asn":    "65000",
				"example.com/asn":       "65001",
				"example.com/addr":      "10.0.0.1",
				"example.com/srcaddr":   "10.0.0.2",
				"example.com/port":      "1179",
				"example.com/hold-time": "30s",
				"example.com/router-id": "10.0.0.2",
			}),
			mapping: pam,
			wantPeer: &config.Peer{
				ASN:      65001,
				MyASN:    65000,
				Addr:     net.ParseIP("10.0.0.1"),
				SrcAddr:  net.ParseIP("10.0.0.2"),
				HoldTime: 30 * time.Second,
				Port:     1179,
				RouterID: net.ParseIP("10.0.0.2"),
			},
		},
		{
			desc: "Use all defaults",
			defaults: &config.PeerAutodiscoveryDefaults{
				ASN:      65001,
				MyASN:    65000,
				Addr:     net.ParseIP("10.0.0.1"),
				SrcAddr:  net.ParseIP("10.0.0.2"),
				Port:     1179,
				HoldTime: 30 * time.Second,
			},
			mapping: pam,
			wantPeer: &config.Peer{
				ASN:      65001,
				MyASN:    65000,
				Addr:     net.ParseIP("10.0.0.1"),
				SrcAddr:  net.ParseIP("10.0.0.2"),
				HoldTime: 30 * time.Second,
				Port:     1179,
			},
		},
		{
			desc: "Verify defaults get overridden by annotations",
			ls: labels.Set(map[string]string{
				"example.com/my-asn":    "65000",
				"example.com/asn":       "65001",
				"example.com/addr":      "10.0.0.1",
				"example.com/srcaddr":   "10.0.0.2",
				"example.com/port":      "1180",
				"example.com/hold-time": "60s",
			}),
			defaults: &config.PeerAutodiscoveryDefaults{
				MyASN:    100,
				ASN:      200,
				Addr:     net.ParseIP("1.1.1.1"),
				SrcAddr:  net.ParseIP("1.1.1.2"),
				HoldTime: 30 * time.Second,
				Port:     1179,
			},
			mapping: pam,
			wantPeer: &config.Peer{
				ASN:      65001,
				MyASN:    65000,
				Addr:     net.ParseIP("10.0.0.1"),
				SrcAddr:  net.ParseIP("10.0.0.2"),
				HoldTime: 60 * time.Second,
				Port:     1180,
			},
		},
		{
			desc: "Nil peer autodiscovery mapping",
			ls: labels.Set(map[string]string{
				"example.com/my-asn": "65000",
				"example.com/asn":    "65001",
				"example.com/addr":   "10.0.0.1",
			}),
		},
		{
			desc: "Empty labels",
			ls: labels.Set(map[string]string{
				"example.com/my-asn": "",
				"example.com/asn":    "",
				"example.com/addr":   "",
			}),
			mapping: pam,
		},
		{
			desc: "Malformed local ASN",
			ls: labels.Set(map[string]string{
				"example.com/my-asn": "oops",
				"example.com/asn":    "65001",
				"example.com/addr":   "10.0.0.1",
			}),
			mapping: pam,
		},
		{
			desc: "Malformed peer ASN",
			ls: labels.Set(map[string]string{
				"example.com/my-asn": "65000",
				"example.com/asn":    "oops",
				"example.com/addr":   "10.0.0.1",
			}),
			mapping: pam,
		},
		{
			desc: "Malformed peer address",
			ls: labels.Set(map[string]string{
				"example.com/my-asn": "65000",
				"example.com/asn":    "65001",
				"example.com/addr":   "oops",
			}),
			mapping: pam,
		},
		{
			desc: "Malformed source address",
			ls: labels.Set(map[string]string{
				"example.com/my-asn":  "65000",
				"example.com/asn":     "65001",
				"example.com/addr":    "10.0.0.1",
				"example.com/srcaddr": "oops",
			}),
			mapping: pam,
		},
		{
			desc: "Malformed port",
			ls: labels.Set(map[string]string{
				"example.com/my-asn": "65000",
				"example.com/asn":    "65001",
				"example.com/addr":   "10.0.0.1",
				"example.com/port":   "oops",
			}),
			mapping: pam,
		},
		{
			desc: "Malformed hold time",
			ls: labels.Set(map[string]string{
				"example.com/my-asn":    "65000",
				"example.com/asn":       "65001",
				"example.com/addr":      "10.0.0.1",
				"example.com/hold-time": "oops",
			}),
			mapping: pam,
		},
		{
			desc: "Malformed router ID",
			ls: labels.Set(map[string]string{
				"example.com/my-asn":    "65000",
				"example.com/asn":       "65001",
				"example.com/addr":      "10.0.0.1",
				"example.com/router-id": "oops",
			}),
			mapping: pam,
		},
	}

	l := log.NewNopLogger()
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			gotPeer, err := parseNodePeer(l, test.mapping, test.defaults, test.ls)
			if err != nil && test.wantPeer != nil {
				t.Errorf("%q: Expected no error but got %q", test.desc, err.Error())
			}
			if diff := cmp.Diff(test.wantPeer, gotPeer); diff != "" {
				t.Errorf("%q: Unexpected peer (-want +got)\n%s", test.desc, diff)
			}
		})
	}
}

func TestDiscoverNodePeers(t *testing.T) {
	pad := &config.PeerAutodiscovery{
		FromAnnotations: []*config.PeerAutodiscoveryMapping{
			{
				MyASN:   "example.com/p1-my-asn",
				ASN:     "example.com/p1-peer-asn",
				Addr:    "example.com/p1-peer-address",
				SrcAddr: "example.com/p1-source-address",
			},
			{
				MyASN: "example.com/p2-my-asn",
				ASN:   "example.com/p2-peer-asn",
				Addr:  "example.com/p2-peer-address",
			},
		},
		FromLabels: []*config.PeerAutodiscoveryMapping{
			{
				MyASN: "example.com/p1-my-asn",
				ASN:   "example.com/p1-peer-asn",
				Addr:  "example.com/p1-peer-address",
			},
		},
		NodeSelectors: []labels.Selector{
			mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "test")),
		},
	}

	tests := []struct {
		desc        string
		annotations map[string]string
		labels      map[string]string
		pad         *config.PeerAutodiscovery
		wantPeers   []*config.Peer
	}{
		{
			desc: "One peer discovered",
			annotations: map[string]string{
				"example.com/p1-my-asn":       "100",
				"example.com/p1-peer-asn":     "200",
				"example.com/p1-peer-address": "10.0.0.1",
			},
			labels: map[string]string{
				"kubernetes.io/hostname": "test",
			},
			pad: pad,
			wantPeers: []*config.Peer{
				{
					MyASN:         100,
					ASN:           200,
					Addr:          net.ParseIP("10.0.0.1"),
					Port:          179,
					HoldTime:      90 * time.Second,
					NodeSelectors: []labels.Selector{mustSelector("kubernetes.io/hostname=test")},
				},
			},
		},
		{
			desc: "Multiple peers discovered",
			annotations: map[string]string{
				"example.com/p1-my-asn":       "100",
				"example.com/p1-peer-asn":     "200",
				"example.com/p1-peer-address": "10.0.0.1",
				"example.com/p2-my-asn":       "100",
				"example.com/p2-peer-asn":     "200",
				"example.com/p2-peer-address": "10.0.0.2",
			},
			labels: map[string]string{
				"kubernetes.io/hostname": "test",
			},
			pad: pad,
			wantPeers: []*config.Peer{
				{
					MyASN:         100,
					ASN:           200,
					Addr:          net.ParseIP("10.0.0.1"),
					Port:          179,
					HoldTime:      90 * time.Second,
					NodeSelectors: []labels.Selector{mustSelector("kubernetes.io/hostname=test")},
				},
				{

					MyASN:         100,
					ASN:           200,
					Addr:          net.ParseIP("10.0.0.2"),
					Port:          179,
					HoldTime:      90 * time.Second,
					NodeSelectors: []labels.Selector{mustSelector("kubernetes.io/hostname=test")},
				},
			},
		},
		{
			desc:        "No peers discovered",
			annotations: map[string]string{},
			labels: map[string]string{
				"kubernetes.io/hostname": "test",
			},
			pad:       pad,
			wantPeers: []*config.Peer{},
		},
		{
			desc: "Duplicate peers",
			annotations: map[string]string{
				"example.com/p1-my-asn":       "100",
				"example.com/p1-peer-asn":     "200",
				"example.com/p1-peer-address": "10.0.0.1",
				"example.com/p2-my-asn":       "100",
				"example.com/p2-peer-asn":     "200",
				"example.com/p2-peer-address": "10.0.0.1",
			},
			labels: map[string]string{
				"kubernetes.io/hostname": "test",
			},
			pad: pad,
			wantPeers: []*config.Peer{
				{
					MyASN:         100,
					ASN:           200,
					Addr:          net.ParseIP("10.0.0.1"),
					Port:          179,
					HoldTime:      90 * time.Second,
					NodeSelectors: []labels.Selector{mustSelector("kubernetes.io/hostname=test")},
				},
			},
		},
		{
			desc: "Node labels don't match selector",
			annotations: map[string]string{
				"example.com/p1-my-asn":       "100",
				"example.com/p1-peer-asn":     "200",
				"example.com/p1-peer-address": "10.0.0.1",
			},
			labels: labels.Set(map[string]string{
				"kubernetes.io/hostname": "foo",
			}),
			pad:       pad,
			wantPeers: []*config.Peer{},
		},
		{
			desc: "Config in labels",
			labels: map[string]string{
				"kubernetes.io/hostname":      "test",
				"example.com/p1-my-asn":       "100",
				"example.com/p1-peer-asn":     "200",
				"example.com/p1-peer-address": "10.0.0.1",
			},
			pad: pad,
			wantPeers: []*config.Peer{
				{
					MyASN:         100,
					ASN:           200,
					Addr:          net.ParseIP("10.0.0.1"),
					Port:          179,
					HoldTime:      90 * time.Second,
					NodeSelectors: []labels.Selector{mustSelector("kubernetes.io/hostname=test")},
				},
			},
		},
		{
			desc: "Config in labels and annotations",
			annotations: map[string]string{
				"example.com/p1-my-asn":       "100",
				"example.com/p1-peer-asn":     "200",
				"example.com/p1-peer-address": "10.0.0.1",
			},
			labels: map[string]string{
				"kubernetes.io/hostname":      "test",
				"example.com/p1-my-asn":       "100",
				"example.com/p1-peer-asn":     "200",
				"example.com/p1-peer-address": "10.0.0.2",
			},
			pad: pad,
			wantPeers: []*config.Peer{
				{
					MyASN:         100,
					ASN:           200,
					Addr:          net.ParseIP("10.0.0.1"),
					Port:          179,
					HoldTime:      90 * time.Second,
					NodeSelectors: []labels.Selector{mustSelector("kubernetes.io/hostname=test")},
				},
				{

					MyASN:         100,
					ASN:           200,
					Addr:          net.ParseIP("10.0.0.2"),
					Port:          179,
					HoldTime:      90 * time.Second,
					NodeSelectors: []labels.Selector{mustSelector("kubernetes.io/hostname=test")},
				},
			},
		},
	}

	comparer := func(a, b *config.Peer) bool {
		return reflect.DeepEqual(a, b)
	}

	l := log.NewNopLogger()
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			c := &bgpController{
				logger: l,
				myNode: "pandora",
				svcAds: make(map[string][]*bgp.Advertisement),
				cfg: &config.Config{
					PeerAutodiscovery: pad,
				},
				nodeAnnotations: labels.Set(test.annotations),
				nodeLabels:      labels.Set(test.labels),
			}

			discovered := c.discoverNodePeers(l)

			if diff := cmp.Diff(test.wantPeers, discovered, cmp.Comparer(comparer)); diff != "" {
				t.Errorf("%q: Unexpected peers (-want +got)\n%s", test.desc, diff)
			}
		})
	}
}

// Verify correct interaction between regular peers and node peers.
func TestNodePeers(t *testing.T) {
	np1 := &config.Peer{
		MyASN:    100,
		ASN:      200,
		Addr:     net.ParseIP("10.0.0.1"),
		Port:     179,
		HoldTime: 90 * time.Second,
		NodeSelectors: []labels.Selector{
			mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "test")),
		},
	}
	np2 := &config.Peer{
		MyASN:    100,
		ASN:      200,
		Addr:     net.ParseIP("10.0.0.2"),
		Port:     179,
		HoldTime: 90 * time.Second,
		NodeSelectors: []labels.Selector{
			mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "test")),
		},
	}
	p1 := &config.Peer{
		MyASN:         100,
		ASN:           200,
		Addr:          net.ParseIP("10.0.0.3"),
		Port:          179,
		HoldTime:      90 * time.Second,
		NodeSelectors: []labels.Selector{labels.Everything()},
	}
	p2 := &config.Peer{
		MyASN:         100,
		ASN:           200,
		Addr:          net.ParseIP("10.0.0.4"),
		Port:          179,
		HoldTime:      90 * time.Second,
		NodeSelectors: []labels.Selector{labels.Everything()},
	}
	pad := &config.PeerAutodiscovery{
		FromAnnotations: []*config.PeerAutodiscoveryMapping{
			{
				MyASN: "example.com/np1-my-asn",
				ASN:   "example.com/np1-asn",
				Addr:  "example.com/np1-addr",
			},
		},
		NodeSelectors: []labels.Selector{labels.Everything()},
	}
	padMulti := &config.PeerAutodiscovery{
		FromAnnotations: []*config.PeerAutodiscoveryMapping{
			{
				MyASN:   "example.com/np1-my-asn",
				ASN:     "example.com/np1-asn",
				Addr:    "example.com/np1-addr",
				SrcAddr: "example.com/np1-srcaddr",
			},
			{
				MyASN:   "example.com/np2-my-asn",
				ASN:     "example.com/np2-asn",
				Addr:    "example.com/np2-addr",
				SrcAddr: "example.com/np2-srcaddr",
			},
		},
		NodeSelectors: []labels.Selector{labels.Everything()},
	}
	anns := map[string]string{
		"example.com/np1-my-asn": "100",
		"example.com/np1-asn":    "200",
		"example.com/np1-addr":   "10.0.0.1",
		"example.com/np2-my-asn": "100",
		"example.com/np2-asn":    "200",
		"example.com/np2-addr":   "10.0.0.2",
	}

	tests := []struct {
		desc        string
		peers       []*config.Peer
		cfg         *config.Config
		annotations map[string]string
		wantPeers   []*config.Peer
	}{
		{
			desc: "No peers, node peer discovered",
			cfg: &config.Config{
				PeerAutodiscovery: pad,
			},
			annotations: anns,
			wantPeers:   []*config.Peer{np1},
		},
		{
			desc: "No peers, multiple node peers discovered",
			cfg: &config.Config{
				PeerAutodiscovery: padMulti,
			},
			annotations: anns,
			wantPeers:   []*config.Peer{np1, np2},
		},
		{
			desc:  "Existing peer, node peer discovered",
			peers: []*config.Peer{p1},
			cfg: &config.Config{
				Peers:             []*config.Peer{p1},
				PeerAutodiscovery: pad,
			},
			annotations: anns,
			wantPeers:   []*config.Peer{p1, np1},
		},
		{
			desc:  "Existing peer, no node peer discovered",
			peers: []*config.Peer{p1},
			cfg: &config.Config{
				Peers: []*config.Peer{p1},
			},
			annotations: anns,
			wantPeers:   []*config.Peer{p1},
		},
		{
			desc:  "Peer autodiscovery disabled, node peers removed",
			peers: []*config.Peer{p1, np1, np2},
			cfg: &config.Config{
				Peers: []*config.Peer{p1},
			},
			annotations: anns,
			wantPeers:   []*config.Peer{p1},
		},
		{
			desc:  "No peers in config, node peer remains intact",
			peers: []*config.Peer{np1},
			cfg: &config.Config{
				PeerAutodiscovery: pad,
			},
			annotations: anns,
			wantPeers:   []*config.Peer{np1},
		},
		{
			desc:  "Regular peer modified, node peer remains intact",
			peers: []*config.Peer{p1, np1},
			cfg: &config.Config{
				Peers:             []*config.Peer{p2},
				PeerAutodiscovery: pad,
			},
			annotations: anns,
			wantPeers:   []*config.Peer{p2, np1},
		},
		{
			desc:  "Regular peer modified to be identical to node peer",
			peers: []*config.Peer{p1, p2, np1},
			cfg: &config.Config{
				Peers: []*config.Peer{
					{
						MyASN:         np1.MyASN,
						ASN:           np1.ASN,
						Addr:          np1.Addr,
						Port:          np1.Port,
						HoldTime:      np1.HoldTime,
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
					p2,
				},
				PeerAutodiscovery: pad,
			},
			annotations: anns,
			wantPeers: []*config.Peer{{
				MyASN:         np1.MyASN,
				ASN:           np1.ASN,
				Addr:          np1.Addr,
				Port:          np1.Port,
				HoldTime:      np1.HoldTime,
				NodeSelectors: []labels.Selector{labels.Everything()},
			}, p2},
		},
		{
			desc: "Regular peer identical to node peer, selectors match",
			peers: []*config.Peer{
				{
					MyASN:         np1.MyASN,
					ASN:           np1.ASN,
					Addr:          np1.Addr,
					Port:          np1.Port,
					HoldTime:      np1.HoldTime,
					NodeSelectors: []labels.Selector{labels.Everything()},
				},
			},
			cfg: &config.Config{
				Peers: []*config.Peer{
					{
						MyASN:         np1.MyASN,
						ASN:           np1.ASN,
						Addr:          np1.Addr,
						Port:          np1.Port,
						HoldTime:      np1.HoldTime,
						NodeSelectors: []labels.Selector{labels.Everything()},
					},
				},
				PeerAutodiscovery: pad,
			},
			annotations: anns,
			wantPeers: []*config.Peer{
				{
					MyASN:         np1.MyASN,
					ASN:           np1.ASN,
					Addr:          np1.Addr,
					Port:          np1.Port,
					HoldTime:      np1.HoldTime,
					NodeSelectors: []labels.Selector{labels.Everything()},
				},
			},
		},
		{
			desc:  "Regular peer identical to node peer, selectors don't match",
			peers: []*config.Peer{np1},
			cfg: &config.Config{
				Peers: []*config.Peer{
					{
						MyASN:    np1.MyASN,
						ASN:      np1.ASN,
						Addr:     np1.Addr,
						Port:     np1.Port,
						HoldTime: np1.HoldTime,
						NodeSelectors: []labels.Selector{
							mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "foo")),
						},
					},
				},
				PeerAutodiscovery: pad,
			},
			annotations: anns,
			wantPeers: []*config.Peer{
				{
					MyASN:    np1.MyASN,
					ASN:      np1.ASN,
					Addr:     np1.Addr,
					Port:     np1.Port,
					HoldTime: np1.HoldTime,
					NodeSelectors: []labels.Selector{
						mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "foo")),
					},
				},
				np1,
			},
		},
		{
			desc: "Duplicate node peers",
			cfg: &config.Config{
				PeerAutodiscovery: padMulti,
			},
			annotations: labels.Set(
				map[string]string{
					"example.com/np1-my-asn": "100",
					"example.com/np1-asn":    "200",
					"example.com/np1-addr":   "10.0.0.1",
					"example.com/np2-my-asn": "100",
					"example.com/np2-asn":    "200",
					"example.com/np2-addr":   "10.0.0.1",
				},
			),
			wantPeers: []*config.Peer{np1},
		},
		{
			desc: "Peer config identical except source address",
			cfg: &config.Config{
				PeerAutodiscovery: padMulti,
			},
			annotations: labels.Set(
				map[string]string{
					"example.com/np1-my-asn":  "100",
					"example.com/np1-asn":     "200",
					"example.com/np1-addr":    "10.0.0.1",
					"example.com/np2-my-asn":  "100",
					"example.com/np2-asn":     "200",
					"example.com/np2-addr":    "10.0.0.1",
					"example.com/np2-srcaddr": "10.0.0.2",
				},
			),
			wantPeers: []*config.Peer{
				np1,
				{
					MyASN:    100,
					ASN:      200,
					Addr:     net.ParseIP("10.0.0.1"),
					SrcAddr:  net.ParseIP("10.0.0.2"),
					Port:     179,
					HoldTime: 90 * time.Second,
					NodeSelectors: []labels.Selector{
						mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "test")),
					},
				},
			},
		},
		{
			desc:  "Multiple node peers, one node peer removed",
			peers: []*config.Peer{np1, np2},
			cfg: &config.Config{
				PeerAutodiscovery: padMulti,
			},
			annotations: labels.Set(
				map[string]string{
					"example.com/np1-my-asn": "100",
					"example.com/np1-asn":    "200",
					"example.com/np1-addr":   "10.0.0.1",
				},
			),
			wantPeers: []*config.Peer{np1},
		},
	}

	comparer := func(a, b *config.Peer) bool {
		return reflect.DeepEqual(a, b)
	}

	l := log.NewNopLogger()
	c := &bgpController{
		logger: l,
		myNode: "pandora",
		svcAds: make(map[string][]*bgp.Advertisement),
		nodeLabels: labels.Set(map[string]string{
			"kubernetes.io/hostname": "test",
		}),
	}
	for _, test := range tests {
		// Reset the BGP session status before each test. The fakeBGP type
		// preserves BGP session state between tests, which leads to unexpected
		// results.
		b := &fakeBGP{
			t:      t,
			gotAds: map[string][]*bgp.Advertisement{},
		}
		newBGP = b.New

		c.cfg = nil

		for _, p := range test.peers {
			c.peers = append(c.peers, &peer{Cfg: p})
		}
		c.nodeAnnotations = labels.Set(test.annotations)

		if err := c.SetConfig(l, test.cfg); err != nil {
			t.Error("SetConfig failed")
		}

		var pcs []*config.Peer
		for _, pc := range c.peers {
			pcs = append(pcs, pc.Cfg)
		}

		if diff := cmp.Diff(test.wantPeers, pcs, cmp.Comparer(comparer)); diff != "" {
			t.Errorf("%q: Unexpected peers (-want +got)\n%s", test.desc, diff)
		}
	}
}

func TestSetNode(t *testing.T) {
	tests := []struct {
		desc        string
		annotations map[string]string
		labels      map[string]string
		peers       []*peer
		node        *v1.Node
		wantPeers   []*peer
	}{
		{
			desc: "Node labels change, peer is updated",
			labels: map[string]string{
				"example.com/my-asn":       "100",
				"example.com/peer-asn":     "200",
				"example.com/peer-address": "10.0.0.1",
				"kubernetes.io/hostname":   "test",
			},
			peers: []*peer{
				{
					Cfg: &config.Peer{
						MyASN:    100,
						ASN:      200,
						Addr:     net.ParseIP("10.0.0.1"),
						Port:     179,
						HoldTime: 90 * time.Second,
						NodeSelectors: []labels.Selector{
							mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "test")),
						},
					},
				},
			},
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"example.com/my-asn":       "100",
						"example.com/peer-asn":     "200",
						"example.com/peer-address": "10.0.0.2",
						"kubernetes.io/hostname":   "test",
					},
				},
			},
			wantPeers: []*peer{
				{
					Cfg: &config.Peer{
						MyASN:    100,
						ASN:      200,
						Addr:     net.ParseIP("10.0.0.2"),
						Port:     179,
						HoldTime: 90 * time.Second,
						NodeSelectors: []labels.Selector{
							mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "test")),
						},
					},
				},
			},
		},
		{
			desc: "Node annotations change, peer is updated",
			annotations: map[string]string{
				"example.com/my-asn":       "100",
				"example.com/peer-asn":     "200",
				"example.com/peer-address": "10.0.0.1",
			},
			labels: map[string]string{
				"kubernetes.io/hostname": "test",
			},
			peers: []*peer{
				{
					Cfg: &config.Peer{
						MyASN:    100,
						ASN:      200,
						Addr:     net.ParseIP("10.0.0.1"),
						Port:     179,
						HoldTime: 90 * time.Second,
						NodeSelectors: []labels.Selector{
							mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "test")),
						},
					},
				},
			},
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"example.com/my-asn":       "100",
						"example.com/peer-asn":     "200",
						"example.com/peer-address": "10.0.0.2",
					},
					Labels: map[string]string{
						"kubernetes.io/hostname": "test",
					},
				},
			},
			wantPeers: []*peer{
				{
					Cfg: &config.Peer{
						MyASN:    100,
						ASN:      200,
						Addr:     net.ParseIP("10.0.0.2"),
						Port:     179,
						HoldTime: 90 * time.Second,
						NodeSelectors: []labels.Selector{
							mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "test")),
						},
					},
				},
			},
		},
		{
			desc: "Required annotation is removed, peer is removed",
			annotations: map[string]string{
				"example.com/my-asn":       "100",
				"example.com/peer-asn":     "200",
				"example.com/peer-address": "10.0.0.1",
			},
			labels: map[string]string{
				"kubernetes.io/hostname": "test",
			},
			peers: []*peer{
				{
					Cfg: &config.Peer{
						MyASN:    100,
						ASN:      200,
						Addr:     net.ParseIP("10.0.0.1"),
						Port:     179,
						HoldTime: 90 * time.Second,
						NodeSelectors: []labels.Selector{
							mustSelector(fmt.Sprintf("%s=%s", v1.LabelHostname, "test")),
						},
					},
				},
			},
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"example.com/my-asn":   "100",
						"example.com/peer-asn": "200",
					},
					Labels: map[string]string{
						"kubernetes.io/hostname": "test",
					},
				},
			},
			wantPeers: []*peer{},
		},
	}

	comparer := func(a, b *peer) bool {
		return reflect.DeepEqual(a.Cfg, b.Cfg)
	}

	l := log.NewNopLogger()
	c := &bgpController{
		cfg: &config.Config{
			PeerAutodiscovery: &config.PeerAutodiscovery{
				FromAnnotations: []*config.PeerAutodiscoveryMapping{
					{
						MyASN: "example.com/my-asn",
						ASN:   "example.com/peer-asn",
						Addr:  "example.com/peer-address",
					},
				},
				FromLabels: []*config.PeerAutodiscoveryMapping{
					{
						MyASN: "example.com/my-asn",
						ASN:   "example.com/peer-asn",
						Addr:  "example.com/peer-address",
					},
				},
				NodeSelectors: []labels.Selector{labels.Everything()},
			},
		},
		logger: l,
		myNode: "pandora",
		svcAds: make(map[string][]*bgp.Advertisement),
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			// Reset the BGP session status before each test. The fakeBGP type
			// preserves BGP session state between tests, which leads to unexpected
			// results.
			b := &fakeBGP{
				t:      t,
				gotAds: map[string][]*bgp.Advertisement{},
			}
			newBGP = b.New

			c.nodeAnnotations = test.annotations
			c.nodeLabels = test.labels
			c.peers = test.peers

			if err := c.SetNode(l, test.node); err != nil {
				t.Error("SetNode failed")
			}

			if diff := cmp.Diff(test.wantPeers, c.peers, cmp.Comparer(comparer)); diff != "" {
				t.Errorf("%q: Unexpected peers (-want +got)\n%s", test.desc, diff)
			}
		})
	}
}
