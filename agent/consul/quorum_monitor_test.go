// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package consul

import (
	"testing"

	"github.com/hashicorp/serf/serf"
	"github.com/stretchr/testify/require"
)

// The heartbeat has to count the same servers an election would: voting
// members of the default segment that are alive in the LAN ring. Read
// replicas, clients, servers parked in named segments and members that are
// leaving or failed all have to be left out, otherwise the alert would claim
// a quorum that raft cannot actually reach.
func TestCountAliveVotingServers(t *testing.T) {
	const (
		nodeID = "ee954a2f-80de-4b34-8780-97b942a50a99"
		other  = "32cb2c09-a673-48ea-9f4b-1b6f5f3b1b47"
	)

	cases := map[string]struct {
		members []serf.Member
		expect  int
	}{
		"empty ring": {
			members: nil,
			expect:  0,
		},
		"one alive server": {
			members: []serf.Member{
				serverMember("node1", nodeID, serf.StatusAlive),
			},
			expect: 1,
		},
		"a client is not a server": {
			members: []serf.Member{
				serverMember("node1", nodeID, serf.StatusAlive),
				{
					Name:   "client1",
					Tags:   map[string]string{"role": "client", "id": other, "dc": "dc1"},
					Status: serf.StatusAlive,
				},
			},
			expect: 1,
		},
		"a read replica does not vote": {
			members: []serf.Member{
				serverMember("node1", nodeID, serf.StatusAlive),
				serverMemberWith("node2", other, serf.StatusAlive, map[string]string{"read_replica": "true"}),
			},
			expect: 1,
		},
		"a server in a named segment is in another pool": {
			members: []serf.Member{
				serverMember("node1", nodeID, serf.StatusAlive),
				serverMemberWith("node2", other, serf.StatusAlive, map[string]string{"segment": "edge"}),
			},
			expect: 1,
		},
		"failed and leaving servers are not alive": {
			members: []serf.Member{
				serverMember("node1", nodeID, serf.StatusAlive),
				serverMember("node2", other, serf.StatusFailed),
				serverMember("node3", other, serf.StatusLeaving),
				serverMember("node4", other, serf.StatusLeft),
			},
			expect: 1,
		},
		"three healthy servers": {
			members: []serf.Member{
				serverMember("node1", nodeID, serf.StatusAlive),
				serverMember("node2", other, serf.StatusAlive),
				serverMember("node3", other, serf.StatusAlive),
			},
			expect: 3,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.expect, countAliveVotingServers(tc.members))
		})
	}
}

// A server that just booted must be able to see itself in its own ring: the
// heartbeat is worthless if it reports zero on the one node that is supposed
// to answer for the quorum.
func TestCountQuorumServers_SeesItself(t *testing.T) {
	_, s1 := testServer(t)

	alive, expected := s1.countQuorumServers()
	require.Equal(t, 1, alive)
	// testServer bootstraps, so bootstrap_expect is cleared once the cluster
	// is up; the heartbeat then reports the configured value for operators to
	// compare against.
	require.Zero(t, expected)
}

func serverMember(name, id string, status serf.MemberStatus) serf.Member {
	return serverMemberWith(name, id, status, nil)
}

func serverMemberWith(name, id string, status serf.MemberStatus, extraTags map[string]string) serf.Member {
	tags := map[string]string{
		"role":    "consul",
		"id":      id,
		"dc":      "dc1",
		"segment": "",
		"port":    "8300",
		"vsn":     "2",
		"build":   "1.22.7",
	}
	for k, v := range extraTags {
		tags[k] = v
	}

	return serf.Member{
		Name:   name,
		Tags:   tags,
		Status: status,
	}
}
