// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package consul

import (
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/serf/serf"

	"github.com/hashicorp/consul/agent/metadata"
)

// monitorQuorumHealth is a cheap, always-on signal that a server is still part
// of a raft group that can elect a leader.
//
// Every incident in which a restarted server failed to come back was a server
// that kept running happily while its raft group was actually broken: the LAN
// serf ring looked full, no error was printed, and the only visible symptom was
// that every request blocked waiting for a leader. What was missing was a
// heartbeat tying the two states together, so this loop publishes it:
//
//	consul_quorum_alive_servers   - servers alive in the LAN ring, voters only
//	consul_quorum_bootstrap_expect - the bootstrap_expect this server expects
//	consul_quorum_leader_known    - 1 when raft reports a leader, else 0
//
// A healthy cluster keeps alive_servers >= bootstrap_expect and
// leader_known == 1. The difference between the two is exactly what to alert
// on: alive_servers low means the gossip ring lost servers (or they were
// renamed into a segment they never came back from), leader_known == 0 with a
// healthy ring means raft lost its leader while gossip still thinks everything
// is fine - the classic symptom of a node that restarted into a stale serf
// snapshot and rejoined a pool it no longer belongs to.
//
// The gauges are emitted unconditionally so that dashboards and alerts keep
// working; only the error logs are rate limited, one line per interval at most
// per condition.
func (s *Server) monitorQuorumHealth() {
	interval := s.config.ReconcileInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Rate limit: only report a condition again after a full interval of it
	// persisting, so a flapping condition cannot flood the logs.
	var (
		reportedLowServers time.Time
		reportedNoLeader   time.Time
	)

	for {
		select {
		case <-s.shutdownCh:
			return
		case now := <-ticker.C:
			aliveServers, expected := s.countQuorumServers()
			leaderAddr, _ := s.raft.LeaderWithID()
			leaderKnown := leaderAddr != ""

			metrics.SetGauge([]string{"quorum", "alive_servers"}, float32(aliveServers))
			metrics.SetGauge([]string{"quorum", "bootstrap_expect"}, float32(expected))
			if leaderKnown {
				metrics.SetGauge([]string{"quorum", "leader_known"}, 1)
			} else {
				metrics.SetGauge([]string{"quorum", "leader_known"}, 0)
			}

			// bootstrap_expect is only meaningful before the cluster is
			// bootstrapped; afterwards it is zeroed out and the raft
			// configuration is the source of truth.
			if expected > 0 && aliveServers < expected && now.Sub(reportedLowServers) >= interval {
				reportedLowServers = now
				s.logger.Error("quorum: not enough alive servers for bootstrap_expect",
					"alive_servers", aliveServers,
					"bootstrap_expect", expected,
				)
			}

			if !leaderKnown && now.Sub(reportedNoLeader) >= interval {
				reportedNoLeader = now
				s.logger.Error("quorum: raft has no leader",
					"alive_servers", aliveServers,
					"bootstrap_expect", expected,
					"raft_state", s.raft.State().String(),
				)
			}
		}
	}
}

// countQuorumServers reports how many voting servers are alive in the LAN ring
// along with the bootstrap_expect this server was configured with. Read
// replicas and servers in named segments are not counted: neither of them
// participates in the raft quorum of the default segment.
func (s *Server) countQuorumServers() (aliveServers int, expected int) {
	return countAliveVotingServers(s.serfLAN.Members()), s.config.BootstrapExpect
}

func countAliveVotingServers(members []serf.Member) int {
	var alive int

	for _, member := range members {
		ok, serverMeta := metadata.IsConsulServer(member)
		if !ok || serverMeta.Segment != "" {
			continue
		}
		if serverMeta.ReadReplica {
			continue
		}
		if member.Status != serf.StatusAlive {
			continue
		}
		alive++
	}

	return alive
}
