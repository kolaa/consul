// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package consul

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/serf/serf"

	"github.com/hashicorp/consul/agent/metadata"
	"github.com/hashicorp/consul/types"
)

// lanMergeDelegate is used to handle a cluster merge on the LAN gossip
// ring. We check that the peers are in the same datacenter and abort the
// merge if there is a mis-match.
type lanMergeDelegate struct {
	dc        string
	nodeID    types.NodeID
	nodeName  string
	segment   string
	server    bool
	partition string

	// identityMu guards the fields below. They describe this agent's own
	// serf pool and are only known once serf.Create has run, while
	// NotifyMerge can already be invoked by memberlist goroutines that
	// handle the snapshot rejoin performed inside serf.Create.
	identityMu       sync.Mutex
	selfAddr         net.IP
	staleSelfHandler func(m serf.Member)
	pendingStaleSelf []serf.Member
}

// uniqueIDMinVersion is the lowest version where we insist that nodes
// have a unique ID.
var uniqueIDMinVersion = version.Must(version.NewVersion("0.8.5"))

const (
	// staleSelfPruneInterval is how often we re-check whether a stale copy
	// of this agent has made it into our serf ring before pruning it.
	staleSelfPruneInterval = 100 * time.Millisecond

	// staleSelfPruneTimeout bounds how long we wait for that copy to show up.
	// If it never does there is nothing to clean up.
	staleSelfPruneTimeout = 30 * time.Second
)

// setSelfAddr records the address this agent advertises on this pool.
//
// It must be called before serf.Create so that merges triggered by the
// snapshot rejoin inside serf.Create can already be recognized.
func (md *lanMergeDelegate) setSelfAddr(selfAddr string) {
	addr := net.ParseIP(selfAddr)
	if addr == nil || addr.IsUnspecified() {
		return
	}
	md.identityMu.Lock()
	defer md.identityMu.Unlock()
	md.selfAddr = addr
}

// setStaleSelfHandler installs the callback used to evict stale copies of
// this agent from the pool and replays everything that was already observed
// before the handler was available.
func (md *lanMergeDelegate) setStaleSelfHandler(handler func(m serf.Member)) {
	md.identityMu.Lock()
	md.staleSelfHandler = handler
	pending := md.pendingStaleSelf
	md.pendingStaleSelf = nil
	md.identityMu.Unlock()

	for _, m := range pending {
		handler(m)
	}
}

// isStaleSelf reports whether m is a leftover record of this very agent: it
// carries our node ID but no longer matches our current identity - either a
// previous node name after a rename, or a previous segment tag after a
// segment change - and it lives at the address we advertise ourselves.
//
// Such records are produced by our own history, never by a genuinely
// conflicting agent, so they must not fail the merge. Anything else that
// reuses our node ID (for example a data_dir copied onto another host) still
// has a different address and keeps failing loudly.
func (md *lanMergeDelegate) isStaleSelf(m *serf.Member) bool {
	md.identityMu.Lock()
	selfAddr := md.selfAddr
	md.identityMu.Unlock()

	if selfAddr == nil || m.Addr == nil || !m.Addr.Equal(selfAddr) {
		return false
	}
	if strings.EqualFold(m.Name, md.nodeName) && m.Tags["segment"] == md.segment {
		return false
	}
	return true
}

// pruneStaleSelf asks this pool to evict m and remembers it until the pool is
// available to do so.
func (md *lanMergeDelegate) pruneStaleSelf(m serf.Member) {
	md.identityMu.Lock()
	handler := md.staleSelfHandler
	if handler == nil {
		md.pendingStaleSelf = append(md.pendingStaleSelf, m)
		md.identityMu.Unlock()
		return
	}
	md.identityMu.Unlock()

	handler(m)
}

// pruneStaleSelfRecordsEvictor returns a handler that removes a stale copy of
// this agent from the given serf pool. The removal is deferred until the copy
// is actually present in the ring, because NotifyMerge runs before the remote
// state is merged locally: pruning earlier would only be undone by the merge
// that is already in flight.
func pruneStaleSelfRecordsEvictor(cluster *serf.Serf, logger hclog.Logger) func(m serf.Member) {
	return func(m serf.Member) {
		go func() {
			deadline := time.Now().Add(staleSelfPruneTimeout)
			for !isSerfMember(cluster, m.Name) {
				if time.Now().After(deadline) {
					logger.Debug("stale copy of this agent never joined the ring, skipping prune",
						"member", m.Name)
					return
				}
				time.Sleep(staleSelfPruneInterval)
			}
			if err := cluster.RemoveFailedNodePrune(m.Name); err != nil {
				logger.Warn("failed to prune stale copy of this agent left behind by a rename or segment change",
					"member", m.Name, "error", err)
				return
			}
			logger.Info("pruned stale copy of this agent left behind by a rename or segment change",
				"member", m.Name)
		}()
	}
}

// pruneStaleSelfRecords evicts every record that carries this agent's node ID
// under a different name from the given pool. A rename leaves such records
// behind in each pool the node used to belong to, and they are normally only
// reaped long after the node has rejoined - which keeps the old name visible
// in "consul members" and confuses servers that still route to it.
//
// This is the explicit counterpart to the merge-time handling above: the
// member being evicted really is this agent, only under a name it no longer
// uses.
func pruneStaleSelfRecords(cluster *serf.Serf, nodeID types.NodeID, nodeName string, logger hclog.Logger) {
	for _, m := range findStaleSelfRecords(cluster, nodeID, nodeName) {
		if err := cluster.RemoveFailedNodePrune(m.Name); err != nil {
			logger.Warn("failed to prune stale copy of this agent left behind by a rename",
				"member", m.Name, "error", err)
			continue
		}
		logger.Info("pruned stale copy of this agent left behind by a rename",
			"member", m.Name)
	}
}

// findStaleSelfRecords returns the members of the given pool that carry this
// agent's node ID under some other name, which is what a rename leaves behind.
func findStaleSelfRecords(cluster *serf.Serf, nodeID types.NodeID, nodeName string) []serf.Member {
	var stale []serf.Member
	if cluster == nil {
		return stale
	}
	for _, m := range cluster.Members() {
		if m.Status == serf.StatusLeft {
			continue
		}
		if rawID, ok := m.Tags["id"]; !ok || rawID != string(nodeID) {
			continue
		}
		if strings.EqualFold(m.Name, nodeName) {
			continue
		}
		stale = append(stale, m)
	}
	return stale
}

func (md *lanMergeDelegate) NotifyMerge(members []*serf.Member) error {
	nodeMap := make(map[types.NodeID]serf.Member)
	for _, m := range members {
		if rawID, ok := m.Tags["id"]; ok && rawID != "" {
			if m.Status == serf.StatusLeft {
				continue
			}

			nodeID := types.NodeID(rawID)

			// A record of this very agent from before a rename or a segment
			// change. Accept the merge and clean the record up afterwards
			// instead of refusing to talk to the whole cluster because of
			// our own leftovers.
			if nodeID == md.nodeID && md.isStaleSelf(m) {
				md.pruneStaleSelf(*m)
				continue
			}

			// See if there's another node that conflicts with us.
			if (nodeID == md.nodeID) && !strings.EqualFold(m.Name, md.nodeName) {
				return fmt.Errorf("Member '%s' has conflicting node ID '%s' with this agent's ID",
					m.Name, nodeID)
			}

			// See if there are any two nodes that conflict with each
			// other. This lets us only do joins into a hygienic
			// cluster now that node IDs are critical for operation.
			if other, ok := nodeMap[nodeID]; ok {
				// The same address means this is one machine seen under two
				// names, which is exactly what a rename looks like while the
				// old record is still being reaped. Rejecting here would keep
				// the renamed node out of the cluster for as long as the stale
				// record lives, so accept instead. Records that share a node
				// ID but not an address are still a real conflict.
				if other.Addr == nil || m.Addr == nil || !other.Addr.Equal(m.Addr) {
					return fmt.Errorf("Member '%s' has conflicting node ID '%s' with member '%s'",
						m.Name, nodeID, other.Name)
				}
				continue
			}

			// Only map nodes with a version that's >= than when
			// we made host-based IDs opt-in, which helps prevent
			// chaos when upgrading older clusters. See #3070 for
			// more details.
			if ver, err := metadata.Build(m); err == nil {
				if ver.Compare(uniqueIDMinVersion) >= 0 {
					nodeMap[nodeID] = *m
				}
			}
		}

		if ok, dc := isConsulNode(*m); ok {
			if dc != md.dc {
				return fmt.Errorf("Member '%s' part of wrong datacenter '%s'",
					m.Name, dc)
			}
		}

		if ok, parts := metadata.IsConsulServer(*m); ok {
			if parts.Datacenter != md.dc {
				return fmt.Errorf("Member '%s' part of wrong datacenter '%s'",
					m.Name, parts.Datacenter)
			}
		}

		if err := md.enterpriseNotifyMergeMember(m); err != nil {
			return err
		}
	}
	return nil
}

// wanMergeDelegate is used to handle a cluster merge on the WAN gossip
// ring. We check that the peers are server nodes and abort the merge
// otherwise.
type wanMergeDelegate struct {
	localDatacenter string

	federationDisabledLock sync.Mutex
	federationDisabled     bool
}

// SetWANFederationDisabled selectively disables the wan pool from accepting
// non-local members. If the toggle changed the current value it returns true.
func (md *wanMergeDelegate) SetWANFederationDisabled(disabled bool) bool {
	md.federationDisabledLock.Lock()
	prior := md.federationDisabled
	md.federationDisabled = disabled
	md.federationDisabledLock.Unlock()

	return prior != disabled
}

func (md *wanMergeDelegate) NotifyMerge(members []*serf.Member) error {
	// Deliberately hold this lock during the entire merge so calls to
	// SetWANFederationDisabled returning immediately imply that the flag takes
	// effect for all future merges.
	md.federationDisabledLock.Lock()
	defer md.federationDisabledLock.Unlock()

	for _, m := range members {
		ok, srv := metadata.IsConsulServer(*m)
		if !ok {
			return fmt.Errorf("Member '%s' is not a server", m.Name)
		}

		if md.federationDisabled {
			if srv.Datacenter != md.localDatacenter {
				return fmt.Errorf("Member '%s' part of wrong datacenter '%s'; WAN federation is disabled", m.Name, srv.Datacenter)
			}
		}
	}
	return nil
}
