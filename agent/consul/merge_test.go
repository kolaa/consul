// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package consul

import (
	"io"
	"log"
	"net"
	"strconv"
	"testing"

	"github.com/hashicorp/go-hclog"
	uuid "github.com/hashicorp/go-uuid"
	"github.com/hashicorp/serf/serf"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/sdk/testutil"
	"github.com/hashicorp/consul/types"
	"github.com/hashicorp/consul/version"
)

func TestMerge_LAN(t *testing.T) {
	type testcase struct {
		members []*serf.Member
		expect  string
	}

	const thisNodeID = "ee954a2f-80de-4b34-8780-97b942a50a99"

	run := func(t *testing.T, tc testcase) {
		delegate := &lanMergeDelegate{
			dc:       "dc1",
			nodeID:   types.NodeID(thisNodeID),
			nodeName: "node0",
		}

		err := delegate.NotifyMerge(tc.members)

		if tc.expect == "" {
			require.NoError(t, err)
		} else {
			testutil.RequireErrorContains(t, err, tc.expect)
		}
	}

	cases := map[string]testcase{
		"client in the wrong datacenter": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc2",
					name:   "node1",
					server: false,
					build:  "0.7.5",
				}),
			},
			expect: "wrong datacenter",
		},
		"server in the wrong datacenter": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc2",
					name:   "node1",
					server: true,
					build:  "0.7.5",
				}),
			},
			expect: "wrong datacenter",
		},
		"node ID conflict with delegate's ID but same node name with same casing": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node0",
					id:     thisNodeID,
					server: true,
					build:  "0.7.5",
				}),
			},
			expect: "",
		},
		"node ID conflict with delegate's ID but same node name with different casing": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "NoDe0",
					id:     thisNodeID,
					server: true,
					build:  "0.7.5",
				}),
			},
			expect: "",
		},
		"node ID conflict with delegate's ID": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node1",
					id:     thisNodeID,
					server: true,
					build:  "0.7.5",
				}),
			},
			expect: "with this agent's ID",
		},
		"cluster with existing conflicting node IDs": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node1",
					id:     "6185913b-98d7-4441-bd8f-f7f7d854a4af",
					server: true,
					build:  "0.8.5",
				}),
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node2",
					id:     "6185913b-98d7-4441-bd8f-f7f7d854a4af",
					server: true,
					build:  "0.9.0",
				}),
			},
			expect: "with member",
		},
		"cluster with existing conflicting node IDs, but version is old enough to skip the check": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node1",
					id:     "6185913b-98d7-4441-bd8f-f7f7d854a4af",
					server: true,
					build:  "0.8.5",
				}),
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node2",
					id:     "6185913b-98d7-4441-bd8f-f7f7d854a4af",
					server: true,
					build:  "0.8.4",
				}),
			},
			expect: "with member",
		},
		"good cluster": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node1",
					server: true,
					build:  "0.8.5",
				}),
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node2",
					server: true,
					build:  "0.8.5",
				}),
			},
			expect: "",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run(t, tc)
		})
	}
}

// A node that is renamed, or moved to another segment, keeps its node ID but
// not its identity as the cluster knows it. The cluster keeps serving a copy
// of the old identity for a long time, and refusing to merge with that copy
// locks the node out of the cluster entirely.
func TestMerge_LAN_StaleSelf(t *testing.T) {
	const thisNodeID = "ee954a2f-80de-4b34-8780-97b942a50a99"

	type testcase struct {
		selfAddr    string
		segment     string
		members     []*serf.Member
		expect      string
		expectPrune string
	}

	run := func(t *testing.T, tc testcase) {
		delegate := &lanMergeDelegate{
			dc:       "dc1",
			nodeID:   types.NodeID(thisNodeID),
			nodeName: "node0",
			segment:  tc.segment,
		}
		delegate.setSelfAddr(tc.selfAddr)

		var pruned []string
		delegate.setStaleSelfHandler(func(m serf.Member) {
			pruned = append(pruned, m.Name)
		})

		err := delegate.NotifyMerge(tc.members)

		if tc.expect == "" {
			require.NoError(t, err)
		} else {
			testutil.RequireErrorContains(t, err, tc.expect)
		}
		if tc.expectPrune == "" {
			require.Empty(t, pruned)
		} else {
			require.Equal(t, []string{tc.expectPrune}, pruned)
		}
	}

	cases := map[string]testcase{
		"record of this agent from before a rename": {
			selfAddr: "10.0.0.1",
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node0-old",
					id:     thisNodeID,
					server: true,
					build:  "0.8.5",
					addr:   "10.0.0.1",
				}),
			},
			expectPrune: "node0-old",
		},
		"record of this agent from before a segment change": {
			selfAddr: "10.0.0.1",
			segment:  "lan",
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:      "dc1",
					name:    "node0",
					id:      thisNodeID,
					server:  true,
					build:   "0.8.5",
					addr:    "10.0.0.1",
					segment: "",
				}),
			},
			expectPrune: "node0",
		},
		"a different node ID on our address is a different agent, not us": {
			selfAddr: "10.0.0.1",
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node0-old",
					id:     "32cb2c09-a673-48ea-9f4b-1b6f5f3b1b47",
					server: true,
					build:  "0.8.5",
					addr:   "10.0.0.1",
				}),
			},
		},
		"a copy of our node ID on another address is still a conflict": {
			selfAddr: "10.0.0.1",
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node0-old",
					id:     thisNodeID,
					server: true,
					build:  "0.8.5",
					addr:   "10.0.0.99",
				}),
			},
			expect: "with this agent's ID",
		},
		"no self address configured keeps the strict check": {
			selfAddr: "",
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node0-old",
					id:     thisNodeID,
					server: true,
					build:  "0.8.5",
					addr:   "10.0.0.1",
				}),
			},
			expect: "with this agent's ID",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run(t, tc)
		})
	}
}

// Two records that share a node ID and an address are one machine that was
// renamed while the old record was still in the ring. That must not fail the
// merge, otherwise the renamed node can never join.
func TestMerge_LAN_ConflictingIDsOnSameAddress(t *testing.T) {
	const sharedNodeID = "6185913b-98d7-4441-bd8f-f7f7d854a4af"

	type testcase struct {
		members []*serf.Member
		expect  string
	}

	run := func(t *testing.T, tc testcase) {
		delegate := &lanMergeDelegate{
			dc:       "dc1",
			nodeID:   types.NodeID("ee954a2f-80de-4b34-8780-97b942a50a99"),
			nodeName: "node0",
		}

		err := delegate.NotifyMerge(tc.members)

		if tc.expect == "" {
			require.NoError(t, err)
		} else {
			testutil.RequireErrorContains(t, err, tc.expect)
		}
	}

	cases := map[string]testcase{
		"same address is a rename and is allowed": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node1-old",
					id:     sharedNodeID,
					server: true,
					build:  "0.8.5",
					addr:   "10.0.0.1",
				}),
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node1",
					id:     sharedNodeID,
					server: true,
					build:  "0.8.5",
					addr:   "10.0.0.1",
				}),
			},
		},
		"different addresses are still a real conflict": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node1",
					id:     sharedNodeID,
					server: true,
					build:  "0.8.5",
					addr:   "10.0.0.1",
				}),
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node2",
					id:     sharedNodeID,
					server: true,
					build:  "0.8.5",
					addr:   "10.0.0.2",
				}),
			},
			expect: "with member",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run(t, tc)
		})
	}
}

// A rename leaves a copy of this agent under its old name in the ring. That
// copy shares the node ID and address with the current record, so failure
// detection never resolves it on its own: it has to be pruned explicitly.
func TestPruneStaleSelfRecords(t *testing.T) {
	const nodeID = "ee954a2f-80de-4b34-8780-97b942a50a99"

	newCluster := func(t *testing.T, name string) *serf.Serf {
		t.Helper()

		conf := serf.DefaultConfig()
		conf.NodeName = name
		conf.DisableCoordinates = true
		conf.Logger = log.New(io.Discard, "", 0)
		conf.MemberlistConfig.BindAddr = "127.0.0.1"
		conf.MemberlistConfig.BindPort = 0
		conf.MemberlistConfig.AdvertiseAddr = "127.0.0.1"
		conf.MemberlistConfig.Logger = log.New(io.Discard, "", 0)
		conf.Tags = map[string]string{
			"id":      nodeID,
			"dc":      "dc1",
			"role":    "consul",
			"segment": "",
			"build":   "0.8.5",
		}

		cluster, err := serf.Create(conf)
		require.NoError(t, err)
		return cluster
	}

	stale := newCluster(t, "node0-old")
	current := newCluster(t, "node0")
	t.Cleanup(func() {
		_ = stale.Shutdown()
		_ = current.Shutdown()
	})

	addr := stale.LocalMember().Addr.String()
	port := stale.LocalMember().Port
	_, err := current.Join([]string{net.JoinHostPort(addr, strconv.Itoa(int(port)))}, true)
	require.NoError(t, err)

	// The ring now holds our own previous name next to our current one.
	staleFound := findStaleSelfRecords(current, types.NodeID(nodeID), "node0")
	require.Len(t, staleFound, 1)
	require.Equal(t, "node0-old", staleFound[0].Name)

	// Shut the stale instance down hard so that nothing can re-announce the
	// name while we assert on the prune.
	require.NoError(t, stale.Shutdown())

	pruneStaleSelfRecords(current, types.NodeID(nodeID), "node0", hclog.NewNullLogger())
	require.False(t, isSerfMember(current, "node0-old"))

	// The current name must never be considered stale.
	require.Empty(t, findStaleSelfRecords(current, types.NodeID(nodeID), "node0"))
}

func TestMerge_WAN(t *testing.T) {
	type testcase struct {
		members []*serf.Member
		expect  string
		setupFn func(t *testing.T, delegate *wanMergeDelegate)
	}

	run := func(t *testing.T, tc testcase) {
		delegate := &wanMergeDelegate{
			localDatacenter: "dc1",
		}
		if tc.setupFn != nil {
			tc.setupFn(t, delegate)
		}
		err := delegate.NotifyMerge(tc.members)
		if tc.expect == "" {
			require.NoError(t, err)
		} else {
			testutil.RequireErrorContains(t, err, tc.expect)
		}
	}

	cases := map[string]testcase{
		"not a server": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc2",
					name:   "node1",
					server: false,
					build:  "0.7.5",
				}),
			},
			expect: "not a server",
		},
		"good cluster": {
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc2",
					name:   "node1",
					server: true,
					build:  "0.7.5",
				}),
				makeTestNode(t, testMember{
					dc:     "dc3",
					name:   "node2",
					server: true,
					build:  "0.7.5",
				}),
			},
		},
		"federation disabled and local join allowed": {
			setupFn: func(t *testing.T, delegate *wanMergeDelegate) {
				delegate.SetWANFederationDisabled(true)
			},
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc1",
					name:   "node1",
					server: true,
					build:  "0.7.5",
				}),
			},
		},
		"federation disabled and remote join blocked": {
			setupFn: func(t *testing.T, delegate *wanMergeDelegate) {
				delegate.SetWANFederationDisabled(true)
			},
			members: []*serf.Member{
				makeTestNode(t, testMember{
					dc:     "dc2",
					name:   "node1",
					server: true,
					build:  "0.7.5",
				}),
			},
			expect: `WAN federation is disabled`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run(t, tc)
		})
	}
}

type testMember struct {
	dc        string
	name      string
	id        string
	server    bool
	build     string
	segment   string
	partition string
	addr      string
	status    serf.MemberStatus
}

func (tm testMember) role() string {
	if tm.server {
		return "consul"
	}
	return "node"
}

func makeTestNode(t *testing.T, tm testMember) *serf.Member {
	if tm.id == "" {
		uuid, err := uuid.GenerateUUID()
		require.NoError(t, err)
		tm.id = uuid
	}
	m := &serf.Member{
		Name:   tm.name,
		Status: tm.status,
		Tags: map[string]string{
			"role":    tm.role(),
			"dc":      tm.dc,
			"id":      tm.id,
			"port":    "8300",
			"segment": tm.segment,
			"build":   tm.build,
			"vsn":     "2",
			"vsn_max": "3",
			"vsn_min": "2",
			"fips":    version.GetFIPSInfo(),
		},
	}
	if tm.addr != "" {
		m.Addr = net.ParseIP(tm.addr)
	}
	if tm.partition != "" {
		m.Tags["ap"] = tm.partition
	}
	return m
}
