// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package consul

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/serf/serf"

	"github.com/hashicorp/consul/agent/metadata"
	"github.com/hashicorp/consul/internal/gossip/libserf"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/consul/logging"
	"github.com/hashicorp/consul/types"
)

// clientSerfSnapshotPath returns the serf snapshot file used by a client's
// single LAN pool.
//
// A client belongs to exactly one pool: the default one, or one named
// segment. The snapshot has to follow it, because a serf snapshot records
// member names and addresses only and never the segment tags. With a shared
// file an agent that is moved from the default pool into a segment (or the
// other way round) would silently replay its previous pool's members as
// valid join targets on the next start, dragging it back into the segment it
// was moved out of - which is what produced the endless leave/join churn and
// the wrong-segment join errors.
//
// Servers keep one snapshot per pool already (serf/local.snapshot for the
// default pool and serf/<segment>.snapshot for every segment).
func clientSerfSnapshotPath(segment string) string {
	if segment == "" {
		return serfLANSnapshot
	}
	return fmt.Sprintf("serf/local-%s.snapshot", strings.ToLower(segment))
}

// setupSerf is used to setup and initialize a Serf
func (c *Client) setupSerf(conf *serf.Config, ch chan serf.Event, path string) (*serf.Serf, error) {
	conf.Init()

	conf.NodeName = c.config.NodeName
	conf.Tags["role"] = "node"
	conf.Tags["dc"] = c.config.Datacenter
	conf.Tags["segment"] = c.config.Segment
	conf.Tags["id"] = string(c.config.NodeID)
	conf.Tags["vsn"] = fmt.Sprintf("%d", c.config.ProtocolVersion)
	conf.Tags["vsn_min"] = fmt.Sprintf("%d", ProtocolVersionMin)
	conf.Tags["vsn_max"] = fmt.Sprintf("%d", ProtocolVersionMax)
	conf.Tags["build"] = c.config.Build
	if c.config.AdvertiseReconnectTimeout != 0 {
		conf.Tags[libserf.ReconnectTimeoutTag] = c.config.AdvertiseReconnectTimeout.String()
	}

	// We use the Intercept variant here to ensure that serf and memberlist logs
	// can be streamed via the monitor endpoint
	serfLogger := c.logger.
		NamedIntercept(logging.Serf).
		NamedIntercept(logging.LAN).
		StandardLogger(&hclog.StandardLoggerOptions{InferLevels: true})
	memberlistLogger := c.logger.
		NamedIntercept(logging.Memberlist).
		NamedIntercept(logging.LAN).
		StandardLogger(&hclog.StandardLoggerOptions{InferLevels: true})

	conf.MemberlistConfig.Logger = memberlistLogger
	conf.Logger = serfLogger
	conf.EventCh = ch
	conf.ProtocolVersion = protocolVersionMap[c.config.ProtocolVersion]
	conf.RejoinAfterLeave = c.config.RejoinAfterLeave
	mergeDelegate := &lanMergeDelegate{
		dc:        c.config.Datacenter,
		nodeID:    c.config.NodeID,
		nodeName:  c.config.NodeName,
		segment:   c.config.Segment,
		server:    false,
		partition: c.config.AgentEnterpriseMeta().PartitionOrDefault(),
	}
	conf.Merge = mergeDelegate

	conf.SnapshotPath = filepath.Join(c.config.DataDir, path)
	if err := lib.EnsurePath(conf.SnapshotPath, false); err != nil {
		return nil, err
	}

	addSerfMetricsLabels(conf, false, c.config.Segment, c.config.AgentEnterpriseMeta().PartitionOrDefault())

	addEnterpriseSerfTags(conf.Tags, c.config.AgentEnterpriseMeta())

	conf.ReconnectTimeoutOverride = libserf.NewReconnectOverride(c.logger)

	enterpriseModifyClientSerfConfigLAN(c.config, conf)

	// Know our own address before serf.Create, so that the snapshot rejoin it
	// performs can already be recognized as a merge with ourselves under a
	// previous identity.
	mergeDelegate.setSelfAddr(conf.MemberlistConfig.AdvertiseAddr)

	cluster, err := serf.Create(conf)
	if err != nil {
		return nil, err
	}

	mergeDelegate.setStaleSelfHandler(pruneStaleSelfRecordsEvictor(cluster, c.logger))

	return cluster, nil
}

// lanEventHandler is used to handle events from the lan Serf cluster
func (c *Client) lanEventHandler() {
	var numQueuedEvents int
	for {
		numQueuedEvents = len(c.eventCh)
		if numQueuedEvents > serfEventBacklogWarning {
			c.logger.Warn("number of queued serf events above warning threshold",
				"queued_events", numQueuedEvents,
				"warning_threshold", serfEventBacklogWarning,
			)
		}

		select {
		case e := <-c.eventCh:
			switch e.EventType() {
			case serf.EventMemberJoin:
				c.nodeJoin(e.(serf.MemberEvent))
			case serf.EventMemberLeave, serf.EventMemberFailed, serf.EventMemberReap:
				c.nodeFail(e.(serf.MemberEvent))
			case serf.EventUser:
				c.localEvent(e.(serf.UserEvent))
			case serf.EventMemberUpdate: // Ignore
				c.nodeUpdate(e.(serf.MemberEvent))
			case serf.EventQuery: // Ignore
			default:
				c.logger.Warn("unhandled LAN Serf Event", "event", e)
			}
		case <-c.shutdownCh:
			return
		}
	}
}

// nodeJoin is used to handle join events on the serf cluster
func (c *Client) nodeJoin(me serf.MemberEvent) {
	for _, m := range me.Members {
		ok, parts := metadata.IsConsulServer(m)
		if !ok {
			continue
		}
		if parts.Datacenter != c.config.Datacenter {
			c.logger.Warn("server has joined the wrong cluster: wrong datacenter",
				"server", m.Name,
				"datacenter", parts.Datacenter,
			)
			continue
		}
		c.logger.Info("adding server", "server", parts)
		c.router.AddServer(types.AreaLAN, parts)

		// Trigger the callback
		if c.config.ServerUp != nil {
			c.config.ServerUp()
		}
	}
}

// nodeUpdate is used to handle update events on the serf cluster
func (c *Client) nodeUpdate(me serf.MemberEvent) {
	for _, m := range me.Members {
		ok, parts := metadata.IsConsulServer(m)
		if !ok {
			continue
		}
		if parts.Datacenter != c.config.Datacenter {
			c.logger.Warn("server has joined the wrong cluster: wrong datacenter",
				"server", m.Name,
				"datacenter", parts.Datacenter,
			)
			continue
		}
		c.logger.Info("updating server", "server", parts.String())
		c.router.AddServer(types.AreaLAN, parts)
	}
}

// nodeFail is used to handle fail events on the serf cluster
func (c *Client) nodeFail(me serf.MemberEvent) {
	for _, m := range me.Members {
		ok, parts := metadata.IsConsulServer(m)
		if !ok {
			continue
		}
		c.logger.Info("removing server", "server", parts.String())
		c.router.RemoveServer(types.AreaLAN, parts)
	}
}

// localEvent is called when we receive an event on the local Serf
func (c *Client) localEvent(event serf.UserEvent) {
	// Handle only consul events
	if !strings.HasPrefix(event.Name, "consul:") {
		return
	}

	switch name := event.Name; {
	case name == newLeaderEvent:
		c.logger.Info("New leader elected", "payload", string(event.Payload))

		// Trigger the callback
		if c.config.ServerUp != nil {
			c.config.ServerUp()
		}
	case isUserEvent(name):
		event.Name = rawUserEventName(name)
		c.logger.Debug("user event", "name", event.Name)

		// Trigger the callback
		if c.config.UserEventHandler != nil {
			c.config.UserEventHandler(event)
		}
	default:
		if !c.handleEnterpriseUserEvents(event) {
			c.logger.Warn("Unhandled local event", "event", event)
		}
	}
}
