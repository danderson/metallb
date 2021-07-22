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
	"bytes"
	"crypto/sha256"
	"net"
	"sort"

	"github.com/go-kit/kit/log"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"
	"go.universe.tf/metallb/internal/layer2"
	v1 "k8s.io/api/core/v1"
)

type layer2Controller struct {
	announcer *layer2.Announce
	myNode    string
	sList     SpeakerList
}

func (c *layer2Controller) SetConfig(log.Logger, *config.Config) error {
	return nil
}

// usableNodes returns all nodes that have at least one fully ready
// endpoint on them.
// The speakers parameter is a map with the node name as key and the readiness
// status as value (true means ready, false means not ready).
// If the speakers map is nil, it is ignored.
func usableNodes(isTrafficLocal bool, eps k8s.EpsOrSlices, speakers map[string]bool) []string {
	usable := map[string]bool{}
	if isTrafficLocal {
		switch eps.Type {
		case k8s.Eps:
			for _, subset := range eps.EpVal.Subsets {
				for _, ep := range subset.Addresses {
					if ep.NodeName == nil {
						continue
					}
					if speakers != nil {
						if ready, ok := speakers[*ep.NodeName]; !ok || !ready {
							continue
						}
					}
					if _, ok := usable[*ep.NodeName]; !ok {
						usable[*ep.NodeName] = true
					}
				}
			}
		case k8s.Slices:
			for _, slice := range eps.SlicesVal {
				for _, ep := range slice.Endpoints {
					if !k8s.IsConditionReady(ep.Conditions) {
						continue
					}
					nodeName := ep.Topology["kubernetes.io/hostname"]
					if nodeName == "" {
						continue
					}
					if speakers != nil {
						if ready, ok := speakers[nodeName]; !ok || !ready {
							continue
						}
					}
					if _, ok := usable[nodeName]; !ok {
						usable[nodeName] = true
					}
				}
			}
		}

	} else {
		// For non traffic local services, all nodes are usable.
		usable = speakers
	}

	var ret []string
	for node, ok := range usable {
		if ok {
			ret = append(ret, node)
		}
	}

	return ret
}

func (c *layer2Controller) ShouldAnnounce(l log.Logger, name string, svc *v1.Service, eps k8s.EpsOrSlices, lbIP net.IP) string {
	nodes := usableNodes(svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal, eps, c.sList.UsableSpeakers())
	// Sort the slice by the hash of node + service IP. This
	// produces an ordering of ready nodes that is unique to the IP
	// so that all services sharing the same IP are annonced from the same node.
	sort.Slice(nodes, func(i, j int) bool {
		hi := sha256.Sum256([]byte(nodes[i] + "#" + lbIP.String()))
		hj := sha256.Sum256([]byte(nodes[j] + "#" + lbIP.String()))

		return bytes.Compare(hi[:], hj[:]) < 0
	})

	// Are we first in the list? If so, we win and should announce.
	if len(nodes) > 0 && nodes[0] == c.myNode {
		return ""
	}

	// Either not eligible, or lost the election entirely.
	return "notOwner"
}

func (c *layer2Controller) SetBalancer(l log.Logger, name string, lbIP net.IP, pool *config.Pool) error {
	c.announcer.SetBalancer(name, lbIP)
	return nil
}

func (c *layer2Controller) DeleteBalancer(l log.Logger, name, reason string) error {
	if !c.announcer.AnnounceName(name) {
		return nil
	}
	c.announcer.DeleteBalancer(name)
	return nil
}

func (c *layer2Controller) SetNode(log.Logger, *v1.Node) error {
	c.sList.Rejoin()
	return nil
}
