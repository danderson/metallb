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

package speakerlist

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"time"

	"go.universe.tf/metallb/internal/k8s"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/hashicorp/memberlist"
)

// SpeakerList represents a list of healthy speakers.
type SpeakerList struct {
	sync.Mutex  // Must be locked while accessing any of the internal SpeakerList maps or arrays.
	l           log.Logger
	k8sSpeakers map[string]string
	resyncSvcCh chan struct{}
	// The following fields are nil when memberlist is disabled.
	mlEventCh  chan memberlist.NodeEvent
	ml         *memberlist.Memberlist
	mlMembers  map[string]bool // Speakers that are alive according to memberlist.
	mlJoinCh   chan struct{}
	mlJoinList []string // A list of IPs of the ready speakers not yet members of the memberlist cluster.
	stopCh     chan struct{}
}

// New creates a new SpeakerList and returns a pointer to it.
func New(logger log.Logger, nodeName, bindAddr, bindPort, secret string, resyncSvcCh chan struct{}) (*SpeakerList, error) {
	sl := SpeakerList{
		l:           logger,
		k8sSpeakers: map[string]string{},
		resyncSvcCh: resyncSvcCh,
	}

	if bindAddr == "" {
		level.Info(logger).Log("op", "startup", "msg", "not starting fast dead node detection (memberlist), need ml-bindaddr config")
		return &sl, nil
	}

	mconfig := memberlist.DefaultLANConfig()

	// mconfig.Name MUST be equal to the spec.nodeName field of the speaker pod as we match it
	// against the nodeName field of Endpoint objects inside usableNodes().
	mconfig.Name = nodeName
	mconfig.BindAddr = bindAddr
	if bindPort != "" {
		mlport, err := strconv.Atoi(bindPort)
		if err != nil {
			level.Error(logger).Log("op", "startup", "error", "unable to parse ml-bindport", "msg", err)
			return nil, err
		}
		mconfig.BindPort = mlport
		mconfig.AdvertisePort = mlport
	}
	mconfig.Logger = newMemberlistLogger(sl.l)
	if secret == "" {
		level.Warn(logger).Log("op", "startup", "warning", "no ml-secret-key set, memberlist traffic will not be encrypted")
	} else {
		sha := sha256.New()
		mconfig.SecretKey = sha.Sum([]byte(secret))[:16]
	}

	// ChannelEventDelegate hints that it should not block, so make mlEventCh
	// 'big'.
	// TODO: See https://github.com/metallb/metallb/issues/716
	sl.mlEventCh = make(chan memberlist.NodeEvent, 1024)
	mconfig.Events = &memberlist.ChannelEventDelegate{Ch: sl.mlEventCh}

	ml, err := memberlist.Create(mconfig)
	if err != nil {
		level.Error(logger).Log("op", "startup", "error", err, "msg", "failed to create memberlist")
		return nil, err
	}

	sl.ml = ml
	sl.mlMembers = map[string]bool{}
	sl.mlJoinList = []string{}

	sl.mlJoinCh = make(chan struct{}, 1)
	sl.stopCh = make(chan struct{})
	go sl.memberlistWatchEvents()
	go sl.joinMembers()

	return &sl, nil
}

func (sl *SpeakerList) joinMembers() {
	for {
		select {
		case <-sl.mlJoinCh:
			sl.Lock()
			joinList := sl.mlJoinList
			sl.Unlock()
			if len(joinList) == 0 {
				// joinList is empty - wait for a new rejoin() call.
				break
			}
			// Join is sequential and TCPTimeout = 10s, so it can take time.
			n, err := sl.ml.Join(joinList)
			if err == nil {
				level.Info(sl.l).Log("op", "ml-join", "msg", "memberlist join", "no. joined", n, "join list", joinList)
			} else {
				level.Error(sl.l).Log("op", "ml-join", "msg", "memberlist join", "no. joined", n, "error", err, "join list", joinList)
			}
			select {
			case <-time.After(15 * time.Second):
				// Wait 15s between join attempts, by calling rejoin() we go to 'case <-sl.mlJoinCh',
				// so if mlJoinList is not empty after MemberList.Join() we will try joining again,
				// and if it's empty we will break right away.
				sl.rejoin()
			case <-sl.stopCh:
				return
			}
		case <-sl.stopCh:
			return
		}
	}
}

//nolint:godot
// updateJoinList updates mlJoinList with the IPs of all the ready speakers
// (i.e. present in k8sSpeakers) that are not alive members of the memberlist cluster
// (i.e. not present in mlMembers).
// Must be called with SpeakerList mutex locked.
func (sl *SpeakerList) updateJoinList() {
	joinList := []string{}
	for name, ip := range sl.k8sSpeakers {
		if !sl.mlMembers[name] {
			joinList = append(joinList, ip)
		}
	}
	sl.mlJoinList = joinList
	if len(joinList) > 0 {
		sl.rejoin()
	}
}

// SetSpeakers updates k8sSpeakers.
func (sl *SpeakerList) SetSpeakers(eps k8s.EpsOrSlices) {
	newmap := map[string]string{}

	switch eps.Type {
	case k8s.Eps:
		for _, subset := range eps.EpVal.Subsets {
			for _, ep := range subset.Addresses {
				if ep.NodeName == nil {
					continue
				}
				newmap[*ep.NodeName] = ep.IP
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
				// (v1beta.Endpoint).Addresses must contain at least one address according to the doc.
				newmap[nodeName] = ep.Addresses[0]
			}
		}
	}

	sl.Lock()
	oldmap := sl.k8sSpeakers
	sl.Unlock()
	if reflect.DeepEqual(oldmap, newmap) {
		return
	}

	added := map[string]string{}
	removed := map[string]string{}
	for k, v := range newmap {
		if _, ok := oldmap[k]; !ok {
			added[k] = v
		}
	}
	for k, v := range oldmap {
		if _, ok := newmap[k]; !ok {
			removed[k] = v
		}
	}
	level.Info(sl.l).Log("op", "SetSpeakers", "msg", "New K8s speakers list set", "added", added, "removed", removed)

	sl.Lock()
	sl.k8sSpeakers = newmap
	if sl.ml != nil {
		sl.updateJoinList()
	}
	sl.Unlock()

	if sl.ml == nil {
		// We normally resync all the services on memberlist events (member left/joined).
		// When memberlist is disabled, we need to resync the services as UsableSpeakers()
		// return value just changed.
		sl.forceSvcSync()
	}
}

func (sl *SpeakerList) rejoin() {
	select {
	case sl.mlJoinCh <- struct{}{}:
	default:
		// mlJoinCh has a capacity of 1. If this channel is full (i.e. blocks when we try to send),
		// there is no need to queue another join operation, therefore we drop it.
	}
}

// UsableSpeakers returns a map of usable nodes or an error.
func (sl *SpeakerList) UsableSpeakers() (map[string]bool, error) {
	sl.Lock()
	defer sl.Unlock()
	if len(sl.k8sSpeakers) == 0 {
		return nil, errors.New("k8sSpeakersNotSetYet")
	}
	activeNodes := map[string]bool{}
	if sl.ml == nil {
		// memberlist is disabled, return the ready speakers from k8s.
		for name := range sl.k8sSpeakers {
			activeNodes[name] = true
		}
	} else {
		// Return a copy with only the alive nodes.
		for name, alive := range sl.mlMembers {
			if alive {
				activeNodes[name] = true
			}
		}
	}

	return activeNodes, nil
}

// Stop stops the SpeakerList.
func (sl *SpeakerList) Stop() {
	if sl.ml == nil {
		return
	}

	close(sl.stopCh)
	level.Info(sl.l).Log("op", "shutdown", "msg", "leaving memberlist cluster")
	err := sl.ml.Leave(time.Second)
	level.Info(sl.l).Log("op", "shutdown", "msg", "left memberlist cluster", "error", err)
	err = sl.ml.Shutdown()
	level.Info(sl.l).Log("op", "shutdown", "msg", "memberlist shutdown", "error", err)
}

func event2String(e memberlist.NodeEventType) string {
	return []string{"NodeJoin", "NodeLeave", "NodeUpdate"}[e]
}

func (sl *SpeakerList) memberlistWatchEvents() {
	for {
		select {
		case e := <-sl.mlEventCh:
			level.Info(sl.l).Log("msg", "Node event", "node addr", e.Node.Addr, "node name", e.Node.Name, "node event", event2String(e.Event))
			sl.Lock()
			if e.Event == memberlist.NodeLeave {
				delete(sl.mlMembers, e.Node.Name)
			} else {
				// If the event is not NodeLeave, the node is alive.
				sl.mlMembers[e.Node.Name] = true
			}
			sl.updateJoinList()
			sl.Unlock()
			sl.forceSvcSync()
		case <-sl.stopCh:
			return
		}
	}
}

func (sl *SpeakerList) forceSvcSync() {
	select {
	case sl.resyncSvcCh <- struct{}{}:
		level.Debug(sl.l).Log("op", "forceSvcSync", "msg", "Queuing a resync")
	default:
		// resyncSvcCh has a capacity of 1, if it's full (it blocks when we try to produce)
		// there is no need to queue more than 1 resync order, so drop it
		level.Debug(sl.l).Log("op", "forceSvcSync", "msg", "Resync already queued, dropping")
	}
}
