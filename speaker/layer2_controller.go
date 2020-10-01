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
	"github.com/hashicorp/memberlist"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/layer2"
	"k8s.io/api/core/v1"
)

type layer2Controller struct {
	announcer *layer2.Announce
	myNode    string
	mList     *memberlist.Memberlist
}

func (c *layer2Controller) SetConfig(log.Logger, *config.Config) error {
	return nil
}

func activeNodes(mList *memberlist.Memberlist) []string {
	var activeNodes []string
	if mList != nil {
		for _, n := range mList.Members() {
		    activeNodes = append(activeNodes, n.Name)
		}
	}
    return activeNodes
}

// usableNodes returns all nodes that have at least one fully ready
// endpoint on them.
func usableNodes(eps *v1.Endpoints, mList *memberlist.Memberlist) []string {
	var activeNodes map[string]bool
	if mList != nil {
		activeNodes = map[string]bool{}
		for _, n := range mList.Members() {
			activeNodes[n.Name] = true
		}
	}

	usable := map[string]bool{}
	for _, subset := range eps.Subsets {
		for _, ep := range subset.Addresses {
			if ep.NodeName == nil {
				continue
			}
			if activeNodes != nil {
				if _, ok := activeNodes[*ep.NodeName]; !ok {
					continue
				}
			}
			if _, ok := usable[*ep.NodeName]; !ok {
				usable[*ep.NodeName] = true
			}
		}
	}

	var ret []string
	for node, ok := range usable {
		if ok {
			ret = append(ret, node)
		}
	}

	return ret
}

func (c *layer2Controller) ShouldAnnounce(l log.Logger, name string, svc *v1.Service, eps *v1.Endpoints) string {
	nodes := usableNodes(eps, c.mList)
		
	if len(nodes) == 0 && svc.Spec.ExternalTrafficPolicy == "Cluster" {
		nodes = activeNodes(c.mList)
	}
	
	// Sort the slice by the hash of node + service name. This
	// produces an ordering of ready nodes that is unique to this
	// service.
	sort.Slice(nodes, func(i, j int) bool {
		hi := sha256.Sum256([]byte(nodes[i] + "#" + name))
		hj := sha256.Sum256([]byte(nodes[j] + "#" + name))

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
	return nil
}
