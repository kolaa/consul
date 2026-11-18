// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/serf/serf"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul-net-rpc/go-msgpack/codec"
	"github.com/hashicorp/consul/agent/metadata"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/sdk/freeport"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	"github.com/hashicorp/consul/testrpc"
	"github.com/hashicorp/consul/types"
)

func TestSegmentJoinAddrsExplicitOnly(t *testing.T) {
	segments := []NetworkSegment{
		{Name: "wan-ish", Port: 8302},
		{Name: "second", Port: 8303},
	}

	cases := map[string]struct {
		segments       []NetworkSegment
		addrs          []string
		expectLAN      []string
		expectSegments map[string][]string
	}{
		"bare addresses reach the default pool only": {
			segments:       segments,
			addrs:          []string{"10.0.0.1", "server.example.com"},
			expectLAN:      []string{"10.0.0.1", "server.example.com"},
			expectSegments: map[string][]string{},
		},
		"mixed bare and explicit-port addresses": {
			segments:  segments,
			addrs:     []string{"10.0.0.1", "server.example.com:8301", "10.0.0.2:8303"},
			expectLAN: []string{"10.0.0.1", "server.example.com:8301"},
			expectSegments: map[string][]string{
				"second": {"10.0.0.2:8303"},
			},
		},
		"an explicit segment port only reaches that segment": {
			segments:  segments,
			addrs:     []string{"10.0.0.1:8303"},
			expectLAN: []string{},
			expectSegments: map[string][]string{
				"second": {"10.0.0.1:8303"},
			},
		},
		"an explicit non-segment port only reaches the default pool": {
			segments:       segments,
			addrs:          []string{"10.0.0.1:1234"},
			expectLAN:      []string{"10.0.0.1:1234"},
			expectSegments: map[string][]string{},
		},
		"bare IPv6 reaches the default pool only": {
			segments:       segments,
			addrs:          []string{"fd00::1"},
			expectLAN:      []string{"fd00::1"},
			expectSegments: map[string][]string{},
		},
		"bracketed IPv6 without a port reaches the default pool only": {
			segments:       segments,
			addrs:          []string{"[fd00::1]"},
			expectLAN:      []string{"[fd00::1]"},
			expectSegments: map[string][]string{},
		},
		"no segments configured": {
			addrs:          []string{"10.0.0.1"},
			expectLAN:      []string{"10.0.0.1"},
			expectSegments: map[string][]string{},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			lan, perSegment := segmentJoinAddrsExplicitOnly(tc.addrs, tc.segments)
			require.Equal(t, tc.expectLAN, lan)
			require.Equal(t, tc.expectSegments, perSegment)
		})
	}
}

// segmentForTest builds a network segment bound to a free loopback port, the
// same way agent.newConsulConfig builds segments in production.
func segmentForTest(name string, port int) NetworkSegment {
	sc := DefaultConfig().SerfLANConfig
	sc.MemberlistConfig.BindAddr = "127.0.0.1"
	sc.MemberlistConfig.BindPort = port
	sc.MemberlistConfig.AdvertiseAddr = "127.0.0.1"
	sc.MemberlistConfig.AdvertisePort = port
	return NetworkSegment{
		Name:       name,
		Bind:       "127.0.0.1",
		Advertise:  "127.0.0.1",
		Port:       port,
		SerfConfig: sc,
	}
}

// A user event fired through the server must reach the local agent once per
// pool the server belongs to: the default LAN pool and every segment pool
// share s.eventChLAN, so LANSendUserEvent has to fan out explicitly or
// segment members never see the event at all.
func TestServer_LANSendUserEvent_FansOutToSegmentPools(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	events := make(chan serf.UserEvent, 16)
	_, s := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{segmentForTest("alpha", freeport.GetOne(t))}
		c.UserEventHandler = func(e serf.UserEvent) {
			events <- e
		}
	})
	defer s.Shutdown()

	payload := []byte("fanout-payload")
	require.NoError(t, s.LANSendUserEvent("consul:event:fanout-test", payload, false))

	deadline := time.After(10 * time.Second)
	var got []serf.UserEvent
	for len(got) < 2 {
		select {
		case e := <-events:
			if e.Name == "fanout-test" {
				got = append(got, e)
			}
		case <-deadline:
			t.Fatalf("want 2 deliveries of the user event (default LAN + segment pool), got %d", len(got))
		}
	}
	for _, e := range got {
		require.Equal(t, payload, e.Payload)
	}
}

// A filter for a segment this server is not a member of must come back as an
// error: today the nil *serf.Serf from the map lookup panics inside the HTTP
// handler, so `consul members -segment=typo` yields a 500 plus a stack trace.
func TestServer_LANMembers_UnknownSegment(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	_, s := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{segmentForTest("alpha", freeport.GetOne(t))}
	})
	defer s.Shutdown()

	_, err := s.LANMembers(LANMemberFilter{Segment: "typo"})
	require.ErrorContains(t, err, `network segment "typo" not found`)

	// The real segment still resolves.
	members, err := s.LANMembers(LANMemberFilter{Segment: "alpha"})
	require.NoError(t, err)
	require.NotEmpty(t, members)

	// And so does the default (no segment) filter.
	members, err = s.LANMembers(LANMemberFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, members)
}

func TestAgent_ReloadConfig_Reporting(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	dir1, s := testServerWithConfig(t, func(c *Config) {
		c.Reporting.License.Enabled = false
	})
	defer os.RemoveAll(dir1)
	defer s.Shutdown()

	testrpc.WaitForTestAgent(t, s.RPC, "dc1")

	require.Equal(t, false, s.config.Reporting.License.Enabled)

	rc := ReloadableConfig{
		Reporting: Reporting{
			License: License{
				Enabled: true,
			},
		},
	}

	require.NoError(t, s.ReloadConfig(rc))

	// Check config reload is no-op
	require.Equal(t, false, s.config.Reporting.License.Enabled)
}

// consul leave must gracefully leave every segment pool too: otherwise the
// segment clients keep seeing this server as "failed" (not "left") and keep
// sending RPC at it until probes notice or reconnect_timeout expires.
func TestServer_HandleEnterpriseLeave_LeavesSegmentPools(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	_, s := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{
			segmentForTest("alpha", freeport.GetOne(t)),
			segmentForTest("beta", freeport.GetOne(t)),
		}
	})
	defer s.Shutdown()

	require.NotEmpty(t, s.lanSegments())

	s.handleEnterpriseLeave()

	for name, pool := range s.lanSegments() {
		require.Equal(t, serf.SerfLeft, pool.State(),
			"segment pool %q must be left by handleEnterpriseLeave", name)
	}
}

// The segment RPC listeners only live in a local variable inside NewServer
// today, so Shutdown() never closes them and an in-process restart (or a test
// cleanup) hits "address already in use".
func TestServer_Shutdown_ReleasesSegmentListeners(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	rpcAddr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: freeport.GetOne(t)}

	_, s := testServerWithConfig(t, func(c *Config) {
		seg := segmentForTest("alpha", freeport.GetOne(t))
		seg.RPCAddr = rpcAddr
		c.Segments = []NetworkSegment{seg}
	})
	defer s.Shutdown()

	// Sanity check: the segment RPC listener exists while the server runs.
	_, err := net.Listen("tcp", rpcAddr.String())
	require.Error(t, err, "segment RPC port must be bound while the server is up")

	require.NoError(t, s.Shutdown())

	ln, err := net.Listen("tcp", rpcAddr.String())
	require.NoError(t, err, "Shutdown must close the segment RPC listener")
	ln.Close()
}

// A listen failure has to say which segment failed (and stay unwrappable to
// the network error), otherwise a multi-segment config is undebuggable.
func TestServer_SetupSegmentRPC_ErrorIdentifiesSegment(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	// Occupy a port so the segment's listen fails.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()

	_, s := testServerWithConfig(t)
	defer s.Shutdown()

	s.config.Segments = []NetworkSegment{{
		Name:    "alpha",
		RPCAddr: blocker.Addr().(*net.TCPAddr),
	}}

	_, err = s.setupSegmentRPC()
	require.Error(t, err, "listen on a busy port must fail")
	require.ErrorContains(t, err, `segment "alpha"`)

	var opErr *net.OpError
	require.ErrorAs(t, err, &opErr, "the wrapped network error must stay reachable via errors.As")
}

// NewServer calls s.Shutdown() when setupSegmentRPC fails; listeners created
// for the earlier segments must already be registered at that point so the
// error path releases them, otherwise partial startup leaks the ports.
func TestServer_SetupSegmentRPC_ErrorPathReleasesEarlierListeners(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()

	goodAddr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: freeport.GetOne(t)}

	_, s := testServerWithConfig(t)
	defer s.Shutdown()

	s.config.Segments = []NetworkSegment{
		{Name: "good", RPCAddr: goodAddr},
		{Name: "bad", RPCAddr: blocker.Addr().(*net.TCPAddr)},
	}

	_, err = s.setupSegmentRPC()
	require.Error(t, err, "the busy segment must fail the setup")

	// This is what NewServer does on the error path.
	require.NoError(t, s.Shutdown())

	ln, err := net.Listen("tcp", goodAddr.String())
	require.NoError(t, err,
		"the listener created for the earlier segment must be released when setup fails")
	ln.Close()
}

// Consul builds the WAN memberlist transport itself (the mesh gateway
// federation transport wraps it), so memberlist never writes the auto-bound
// port back into the config: a dynamic (0) WAN bind port would trip the
// dynamic-port check in NewServer anyway. setupSerf has to report it instead
// — before the transport is created, so no socket is ever bound — and the
// error comes back through setupSerf's caller, so NewServer takes its
// ordinary error path and shuts down, releasing the segment RPC listeners
// registered by setupSegmentRPC.
func TestSetupSerf_WANDynamicBindPortFails(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	_, s := testServerWithConfig(t)
	defer s.Shutdown()

	conf := DefaultConfig().SerfWANConfig
	conf.MemberlistConfig.BindAddr = "127.0.0.1"
	conf.MemberlistConfig.BindPort = 0

	_, _, err := s.setupSerf(setupSerfOptions{
		Config:       conf,
		EventCh:      make(chan serf.Event, 16),
		SnapshotPath: serfWANSnapshot,
		Listener:     s.Listener,
		WAN:          true,
	})
	require.ErrorContains(t, err, "Failed to get dynamic bind port for WAN Serf")
}

// lockedBuffer collects log output written by concurrent goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The server's own copies in its segment pools carry role=consul plus a
// non-empty segment tag, so they fail shouldHandleMember and every reconcile
// cycle used to log one WARN per segment per server for members the default
// pool already handles.
func TestServer_Reconcile_SkipsSegmentServerCopies(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	logs := &lockedBuffer{}
	dir, s := testServerWithDepsAndConfig(t, func(d *Deps) {
		d.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{
			Output: logs,
			Level:  hclog.Warn,
		})
	}, func(c *Config) {
		c.Segments = []NetworkSegment{segmentForTest("alpha", freeport.GetOne(t))}
	})
	defer os.RemoveAll(dir)
	defer s.Shutdown()

	testrpc.WaitForLeader(t, s.RPC, "dc1")

	require.NoError(t, s.reconcile())

	require.NotContains(t, logs.String(), "skipping reconcile of node",
		"segment copies of servers must not warn on every reconcile cycle")
}

// The event path has to apply the same filter as the periodic pass above:
// a server's copy in a segment pool fails shouldHandleMember, so queueing it
// logged one WARN per segment per server on the leader for every flood join,
// failure and update (review finding F9). A real client of the same segment
// must still reach reconcileMember and be registered.
func TestServer_LocalMemberEvent_SkipsSegmentServerCopies(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	logs := &lockedBuffer{}
	segPort := freeport.GetOne(t)
	_, s := testServerWithDepsAndConfig(t, func(d *Deps) {
		d.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{
			Output: logs,
			Level:  hclog.Warn,
		})
	}, func(c *Config) {
		c.Bootstrap = true
		c.Segments = []NetworkSegment{segmentForTest("alpha", segPort)}
	})
	defer s.Shutdown()

	testrpc.WaitForLeader(t, s.RPC, "dc1")
	baseline := strings.Count(logs.String(), "skipping reconcile of node")

	// A server's copy in the segment pool: must not be queued at all.
	s.localMemberEvent(serf.MemberEvent{
		Type: serf.EventMemberJoin,
		Members: []serf.Member{{
			Name:   "segment-server-copy",
			Status: serf.StatusAlive,
			Tags: map[string]string{
				"role": "consul", "dc": "dc1", "segment": "alpha",
				"id": "7d3f4a10-0000-4000-8000-000000000001", "port": "8300",
			},
		}},
	})

	// A real client of the same segment: must still be queued, reconciled and
	// registered. Waiting for it in the catalog also proves the leader loop
	// already consumed everything pushed above (reconcileCh is FIFO), so the
	// WARN assertion below needs no sleep.
	client := serf.Member{
		Name:   uniqueNodeName(t.Name()) + "-web",
		Addr:   net.ParseIP("127.0.0.1"),
		Status: serf.StatusAlive,
		Tags: map[string]string{
			"role": "node", "dc": "dc1", "segment": "alpha",
			"id": "7d3f4a10-0000-4000-8000-000000000002", "build": "1.2.3",
		},
	}
	s.localMemberEvent(serf.MemberEvent{
		Type:    serf.EventMemberJoin,
		Members: []serf.Member{client},
	})

	retry.Run(t, func(r *retry.R) {
		_, node, err := s.fsm.State().GetNode(client.Name, nil, "")
		require.NoError(r, err)
		require.NotNil(r, node, "a segment client must still be queued and registered")
	})

	require.Equal(t, baseline, strings.Count(logs.String(), "skipping reconcile of node"),
		"a server's segment copy must not reach reconcileMember on the event path (F9)")
}

// LANSegmentAddr is a dialable join target: joinLAN hands it straight to
// memberlist, so it must carry the segment pool's port, not just its IP.
func TestServer_LANSegmentAddr_IncludesPort(t *testing.T) {
	segPort := freeport.GetOne(t)
	_, s := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{segmentForTest("alpha", segPort)}
	})
	defer s.Shutdown()

	require.Equal(t, fmt.Sprintf("127.0.0.1:%d", segPort), s.LANSegmentAddr("alpha"))
	require.Equal(t, "", s.LANSegmentAddr("typo"))
}

// The role=consul skip must only exclude the server's own segment copies: a
// real client that lives only in a segment pool (it never appears in the
// default LAN pool) has to be registered by reconcile, or it stays out of the
// catalog until some serf event happens to fire. The node is deleted from the
// state store right before reconcile so only reconcile() itself can bring it
// back - the earlier serf memberJoin event has already been consumed by then.
func TestServer_Reconcile_RegistersSegmentClient(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	dir1, s := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{segmentForTest("alpha", freeport.GetOne(t))}
	})
	defer os.RemoveAll(dir1)
	defer s.Shutdown()

	testrpc.WaitForLeader(t, s.RPC, "dc1")

	dir2, client := testClientWithConfig(t, func(c *Config) {
		c.Datacenter = "dc1"
		c.NodeName = uniqueNodeName(t.Name())
		c.Segment = "alpha"
	})
	defer os.RemoveAll(dir2)
	defer client.Shutdown()

	// joinLAN targets the server's segment pool because the client carries
	// segment=alpha; the membership check confirms both sides see each other
	// there, so the client is in the segment pool and nowhere else.
	joinLAN(t, client, s)

	state := s.fsm.State()
	idx, node, err := state.GetNode(client.config.NodeName, nil, "")
	require.NoError(t, err)
	if node != nil {
		require.NoError(t, state.DeleteNode(idx, client.config.NodeName, nil, ""))
	}

	require.NoError(t, s.reconcile())

	_, node, err = state.GetNode(client.config.NodeName, nil, "")
	require.NoError(t, err)
	require.NotNil(t, node,
		"a segment client must be registered by reconcile; only the server's own segment copies are skipped")
}

// GetMatchingLANCoordinate must resolve the default pool, the real segment
// pools and reject an unknown segment with the same wording LANMembers uses.
func TestServer_GetMatchingLANCoordinate_UnknownSegment(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	_, s := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{segmentForTest("alpha", freeport.GetOne(t))}
	})
	defer s.Shutdown()

	coord, err := s.GetMatchingLANCoordinate("", "")
	require.NoError(t, err)
	require.NotNil(t, coord)

	coord, err = s.GetMatchingLANCoordinate("", "alpha")
	require.NoError(t, err)
	require.NotNil(t, coord)

	_, err = s.GetMatchingLANCoordinate("", "typo")
	require.ErrorContains(t, err, `network segment "typo" not found`)
}

// Map iteration order is random, so keyring responses, coordinate sets and
// logs must come from a sorted list of pool names. Six names make a random
// order pass this test only with probability 1/720.
func TestServer_SegmentNames_Sorted(t *testing.T) {
	s := &Server{}
	s.segmentLan = make(map[string]*serf.Serf)
	for _, name := range []string{"gamma", "alpha", "echo", "delta", "bravo", "charlie"} {
		s.segmentLan[name] = nil // segmentNames only reads the keys
	}

	require.Equal(t,
		[]string{"alpha", "bravo", "charlie", "delta", "echo", "gamma"},
		s.segmentNames())
}

// Segment pools between servers must converge through flooding, whatever
// shape retry_join took. Both servers live on this host and use the same
// segment port numbers, bound on different loopback addresses (127.0.0.1 vs
// 127.0.0.2). The join below carries an explicit LAN port, which reaches the
// default pool only: without flooding every segment pool keeps a single
// member, reconcileReaped then reaps the other server's segment clients out
// of the catalog, and they re-register forever.
//
// The flood address comes from the sl_<segment> tags each server publishes on
// its default LAN pool, so convergence works even though the two servers use
// different addresses for LAN and segments alike.
func TestServer_SegmentPools_ConvergeViaFlooding(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	skipIfCannotBindToIP(t, "127.0.0.2")

	const segmentCount = 4
	segPorts := freeport.GetN(t, segmentCount)

	segmentsOn := func(ip string) []NetworkSegment {
		segs := make([]NetworkSegment, 0, segmentCount)
		for i, port := range segPorts {
			sc := DefaultConfig().SerfLANConfig
			sc.MemberlistConfig.BindAddr = ip
			sc.MemberlistConfig.BindPort = port
			sc.MemberlistConfig.AdvertiseAddr = ip
			sc.MemberlistConfig.AdvertisePort = port
			segs = append(segs, NetworkSegment{
				Name:       fmt.Sprintf("seg%02d", i),
				Bind:       ip,
				Advertise:  ip,
				Port:       port,
				SerfConfig: sc,
			})
		}
		return segs
	}

	_, s1 := testServerWithConfig(t, func(c *Config) {
		c.Bootstrap = true
		c.SerfFloodInterval = 100 * time.Millisecond
		c.Segments = segmentsOn("127.0.0.1")
	})
	_, s2 := testServerWithConfig(t, func(c *Config) {
		c.Bootstrap = false
		c.SerfFloodInterval = 100 * time.Millisecond
		c.SerfLANConfig.MemberlistConfig.BindAddr = "127.0.0.2"
		c.SerfLANConfig.MemberlistConfig.AdvertiseAddr = "127.0.0.2"
		c.Segments = segmentsOn("127.0.0.2")
	})

	// The join address carries an explicit port, so it reaches the default
	// pool only; the segment pools have to be converged by the flooders.
	n, err := s2.JoinLAN([]string{joinAddrLAN(s1)}, s2.AgentEnterpriseMeta())
	require.NoError(t, err)
	require.Equal(t, 1, n)

	retry.Run(t, func(r *retry.R) {
		for _, s := range []*Server{s1, s2} {
			members, err := s.LANMembers(LANMemberFilter{})
			require.NoError(r, err)
			require.Len(r, members, 2,
				"default LAN pool of %s must see both servers", s.config.NodeName)

			for i := range segPorts {
				segName := fmt.Sprintf("seg%02d", i)
				members, err = s.LANMembers(LANMemberFilter{Segment: segName})
				require.NoError(r, err)
				require.Len(r, members, 2,
					"segment pool %q of %s must see both servers via flooding",
					segName, s.config.NodeName)
			}
		}
	})
}

// Only the default pool decides whether JoinLAN succeeded. An unreachable
// segment port has to be a WARN, not an error: retry_join treats err != nil
// as a failure and, with retry_max set, exits the agent with "max join retry
// exhausted" - even though the LAN join itself worked. Segment pools are
// converged by the flooders (floodSegments) anyway.
func TestServer_JoinLAN_SegmentJoinFailureIsBestEffort(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	logs := &lockedBuffer{}
	segPort := freeport.GetOne(t)

	_, leader := testServerWithConfig(t)

	_, s := testServerWithDepsAndConfig(t, func(d *Deps) {
		d.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{
			Output: logs,
			Level:  hclog.Warn,
		})
	}, func(c *Config) {
		c.Segments = []NetworkSegment{segmentForTest("alpha", segPort)}
	})

	// The first address reaches the leader's default pool. The second
	// carries the configured segment port but has no listener anywhere
	// (the segment binds 127.0.0.1, this targets 127.0.0.2), so that
	// segment join fails deterministically.
	addrs := []string{
		joinAddrLAN(leader),
		fmt.Sprintf("127.0.0.2:%d", segPort),
	}
	n, err := s.JoinLAN(addrs, s.AgentEnterpriseMeta())
	require.NoError(t, err, "a failed segment join must not fail JoinLAN")
	require.Equal(t, 1, n, "the default pool join must still report its success")

	require.Contains(t, logs.String(), "explicit segment join failed",
		"the segment failure must be logged as a warning, not returned")
}

// A segment-only join list must still perform the segment join before it
// reports the error: `consul join host:<segPort>` is the manual repair for a
// split segment pool and must not error out without trying.
func TestServer_JoinLAN_SegmentOnlyAddrsStillJoins(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	skipIfCannotBindToIP(t, "127.0.0.2")

	segPort := freeport.GetOne(t)
	segOn := func(ip string) NetworkSegment {
		sc := DefaultConfig().SerfLANConfig
		sc.MemberlistConfig.BindAddr = ip
		sc.MemberlistConfig.BindPort = segPort
		sc.MemberlistConfig.AdvertiseAddr = ip
		sc.MemberlistConfig.AdvertisePort = segPort
		return NetworkSegment{
			Name:       "alpha",
			Bind:       ip,
			Advertise:  ip,
			Port:       segPort,
			SerfConfig: sc,
		}
	}

	_, s1 := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{segOn("127.0.0.1")}
	})
	defer s1.Shutdown()

	_, s2 := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{segOn("127.0.0.2")}
	})
	defer s2.Shutdown()

	// Only s1's segment address is given: the call must fail (the default
	// pool got nothing) but the join into s1's alpha pool must have happened.
	_, err := s2.JoinLAN([]string{fmt.Sprintf("127.0.0.1:%d", segPort)}, s2.AgentEnterpriseMeta())
	require.Error(t, err)

	retry.Run(t, func(r *retry.R) {
		members, err := s1.LANMembers(LANMemberFilter{Segment: "alpha"})
		require.NoError(r, err)
		names := make([]string, 0, len(members))
		for _, m := range members {
			names = append(names, m.Name)
		}
		require.Contains(r, names, s2.config.NodeName,
			"the segment join must run even though JoinLAN returned an error")
	})
}

// If every join address carries a configured segment port, no address can
// reach the default pool. Reporting (0, nil) would let retry_join log a
// successful join while the node never joins the LAN cluster at all; the
// contract is "success = default pool joined", so this shape is an error.
func TestServer_JoinLAN_SegmentOnlyAddrsIsAnError(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	segPort := freeport.GetOne(t)
	_, s := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{segmentForTest("alpha", segPort)}
	})

	// The address matches the configured segment port, so it would be routed
	// to the segment pool only - the default pool gets nothing to join.
	_, err := s.JoinLAN([]string{fmt.Sprintf("127.0.0.1:%d", segPort)}, s.AgentEnterpriseMeta())
	require.Error(t, err,
		"an address that cannot reach the default pool must not report success")
}

// segmentFloodTarget must prefer the advertised sl_<segment> address and
// fall back to the LAN IP plus the local segment port when the server
// publishes no tag (same-IP segments omit their tag to save tag budget).
func TestSegmentFloodTarget(t *testing.T) {
	srv := &metadata.Server{
		Name:         "node1",
		Addr:         &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 8300},
		SegmentAddrs: map[string]string{"dmz": "172.16.0.5"},
		SegmentPorts: map[string]int{"dmz": 8310},
	}

	addr, advertised := segmentFloodTarget(srv, "dmz", 8303)
	require.True(t, advertised)
	require.Equal(t, "172.16.0.5:8310", addr,
		"the advertised tag address must win over the fallback")

	addr, advertised = segmentFloodTarget(srv, "alpha", 8303)
	require.False(t, advertised)
	require.Equal(t, "10.0.0.5:8303", addr,
		"a tag-less server is dialed at the LAN IP plus the local segment port")

	srv.Addr = nil
	_, advertised = segmentFloodTarget(srv, "alpha", 8303)
	require.False(t, advertised, "a server without any address must not be dialed")
}

// floodServerMember builds a LAN-pool member for a server, as the segment
// flooder sees it: role/dc/port/vsn/build is what metadata.IsConsulServer
// needs to parse it.
func floodServerMember(name, dc string, status serf.MemberStatus) serf.Member {
	return serf.Member{
		Name:   name,
		Status: status,
		Tags: map[string]string{
			"role": "consul", "dc": dc, "port": "8300",
			"vsn": "2", "build": "1.17.0",
		},
	}
}

// floodClientMember builds a LAN-pool member for a client node: it must never
// become a flood candidate (the flooder only joins servers).
func floodClientMember(name, dc string, status serf.MemberStatus) serf.Member {
	return serf.Member{
		Name:   name,
		Status: status,
		Tags: map[string]string{
			"role": "node", "dc": dc,
		},
	}
}

func floodCandidateNames(candidates []metadata.Server) []string {
	names := make([]string, 0, len(candidates))
	for _, srv := range candidates {
		names = append(names, srv.Name)
	}
	return names
}

// Candidate selection must match member names exactly: LAN pool member names
// carry no datacenter suffix, so a node named "consul01.prod" in datacenter
// "prod" has to compare equal to its own copy in the segment pool. Trimming
// the ".<dc>" suffix (as the WAN flooder's naming convention invites) would
// report it missing forever and flood-join it every pass (review F2/F13).
func TestSegmentFloodCandidates_ExactNameMatching(t *testing.T) {
	lan := []serf.Member{
		floodServerMember("consul01.prod", "prod", serf.StatusAlive),
		floodServerMember("consul02", "prod", serf.StatusAlive),
		floodServerMember("consul03", "prod", serf.StatusFailed),
		floodClientMember("client-1", "prod", serf.StatusAlive),
	}
	segment := []serf.Member{
		floodServerMember("consul01.prod", "prod", serf.StatusAlive),
	}

	alive, candidates := segmentFloodCandidates("prod", lan, segment)

	require.Contains(t, alive, "consul01.prod",
		"the segment pool member must be recognised under its exact name")
	require.Equal(t, []string{"consul02"}, floodCandidateNames(candidates),
		"only the alive LAN server missing from the segment pool is a candidate")

	// A client in the segment pool must not shield a server of the same name,
	// and a failed LAN server must not be dialed at all.
	segment = []serf.Member{
		floodClientMember("consul02", "prod", serf.StatusAlive),
	}
	_, candidates = segmentFloodCandidates("prod", lan, segment)
	require.Equal(t, []string{"consul01.prod", "consul02"}, floodCandidateNames(candidates),
		"clients in the pool are not servers, and failed LAN members are skipped")
}

// encodedSerfTagsSize measures a tag map exactly the way serf does before it
// hands it to memberlist: magic byte prefix, then the msgpack encoding of the
// map (serf/serf.go encodeTags). Serf refuses anything over
// memberlist.MetaMaxSize, so this is the byte count that matters. Map
// iteration order changes the bytes but not their length.
func encodedSerfTagsSize(tags map[string]string) int {
	var buf bytes.Buffer
	buf.WriteByte(255) // serf tagMagicByte
	if err := codec.NewEncoder(&buf, &codec.MsgpackHandle{}).Encode(tags); err != nil {
		panic(fmt.Sprintf("failed to encode serf tags: %s", err))
	}
	return buf.Len()
}

// SerfBaseTagReserve is only honest if a real server's default LAN pool tags
// fit inside it: the config-time budget check (agent/config/segment_ce.go)
// adds it to the datacenter name and the sl_ tags to decide whether a segment
// list fits in serf's 512-byte tag limit. If the built-in tags ever grow past
// the reserve, every server with many segments would be refused at config
// time for no visible reason - or worse, serf itself would refuse to start.
func TestServer_BaseLANTags_FitReserve(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	// A segment on a different IP from the LAN advertise address publishes an
	// sl_<name> tag, so the test measures the tag set an operator with
	// segments actually runs.
	skipIfCannotBindToIP(t, "127.0.0.2")

	segPort := freeport.GetOne(t)
	seg := segmentForTest("alpha", segPort)
	seg.Bind, seg.Advertise = "127.0.0.2", "127.0.0.2"
	seg.SerfConfig.MemberlistConfig.BindAddr = "127.0.0.2"
	seg.SerfConfig.MemberlistConfig.AdvertiseAddr = "127.0.0.2"

	_, s := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{seg}
	})
	defer s.Shutdown()

	tags := s.serfLAN.LocalMember().Tags
	require.Contains(t, tags, "sl_alpha",
		"the segment tag must be published for this test to measure it")

	// Everything except the sl_ tags and the datacenter name must fit the
	// reserve the config-time check assumes.
	base := make(map[string]string, len(tags))
	for k, v := range tags {
		if strings.HasPrefix(k, "sl_") || k == "dc" {
			continue
		}
		base[k] = v
	}
	baseSize := encodedSerfTagsSize(base) - 1 // minus the magic byte
	require.LessOrEqual(t, baseSize, SerfBaseTagReserve,
		"default LAN pool tags (minus sl_ and dc) grew past SerfBaseTagReserve; "+
			"raise the constant and update the config-time budget check")

	// And the whole set must leave SerfTagHeadroom free below serf's limit.
	total := encodedSerfTagsSize(tags)
	require.LessOrEqual(t, total, SerfTagLimit-SerfTagHeadroom,
		"a server with one segment must not eat into SerfTagHeadroom")
}

// The retry state must back failures off exponentially, skip refused
// fallbacks, and reset a server that became alive.
func TestSegmentFloodState_TimeBackoff(t *testing.T) {
	st := newSegmentFloodState()
	st.baseDelay = time.Second
	st.maxDelay = 8 * time.Second
	now := time.Now()

	require.True(t, st.due("s1", now), "a server never dialed is due")

	st.reschedule("s1", now)
	require.False(t, st.due("s1", now.Add(500*time.Millisecond)))
	require.True(t, st.due("s1", now.Add(time.Second)), "first retry after the base delay")

	st.reschedule("s1", now.Add(time.Second))
	require.False(t, st.due("s1", now.Add(2*time.Second)))
	require.True(t, st.due("s1", now.Add(3*time.Second)), "backoff doubles per attempt")

	// Refused fallbacks are skipped by the flooder and by the reap guard.
	st.markRefused("s2")
	require.True(t, st.refusedFallback("s2"))
	require.Contains(t, st.refusedNames(), "s2")

	// A server that became alive is fully reset.
	st.reschedule("s1", now.Add(3*time.Second))
	st.clear("s1")
	require.True(t, st.due("s1", now.Add(4*time.Second)))
	st.markRefused("s2")
	st.clear("s2")
	require.False(t, st.refusedFallback("s2"))
	require.Empty(t, st.refusedNames())
}

// The cross-pool event filter must drop a non-alive event whose node is
// alive with the same ID in another pool (a moved agent), keep events for
// nodes that are really gone, and drop alive events whose name is alive
// with a different node ID elsewhere (a duplicate hostname).
func TestServer_PoolEventFilter_ShadowedMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	logs := &lockedBuffer{}
	segPort := freeport.GetOne(t)
	_, s := testServerWithDepsAndConfig(t, func(d *Deps) {
		d.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{
			Output: logs,
			Level:  hclog.Error,
		})
	}, func(c *Config) {
		c.Segments = []NetworkSegment{segmentForTest("alpha", segPort)}
	})
	defer s.Shutdown()

	// web-7 moved to alpha: its old (default pool) copy is failed.
	_, web7 := testClientWithConfig(t, func(c *Config) {
		c.NodeName = uniqueNodeName(t.Name()) + "-web7"
		c.NodeID = types.NodeID(uniqueNodeName(t.Name()) + "-web7")
		c.Segment = "alpha"
	})
	defer web7.Shutdown()
	joinLAN(t, web7, s)

	// web-9 exists twice with different node IDs: once in the default pool,
	// once in alpha.
	_, web9Default := testClientWithConfig(t, func(c *Config) {
		c.NodeName = uniqueNodeName(t.Name()) + "-web9"
		c.NodeID = types.NodeID(uniqueNodeName(t.Name()) + "-web9d")
	})
	defer web9Default.Shutdown()
	joinLAN(t, web9Default, s)

	_, web9Alpha := testClientWithConfig(t, func(c *Config) {
		c.NodeName = web9Default.config.NodeName
		c.NodeID = types.NodeID(uniqueNodeName(t.Name()) + "-web9a")
		c.Segment = "alpha"
	})
	defer web9Alpha.Shutdown()
	joinLAN(t, web9Alpha, s)

	// Wait until every client is where the test expects it.
	retry.Run(t, func(r *retry.R) {
		alpha, err := s.LANMembers(LANMemberFilter{Segment: "alpha"})
		require.NoError(r, err)
		require.Len(r, alpha, 3, "alpha pool: server + web-7 + web-9")
	})

	idOf := func(c *Client) string { return string(c.config.NodeID) }

	// 1. A failed event for web-7 from its old pool is shadowed by the live
	// alpha copy with the same ID.
	failedWeb7 := serf.Member{
		Name:   web7.config.NodeName,
		Status: serf.StatusFailed,
		Tags:   map[string]string{"role": "node", "segment": "", "id": idOf(web7)},
	}
	kept := s.filterShadowedMembers(serf.MemberEvent{
		Type:    serf.EventMemberFailed,
		Members: []serf.Member{failedWeb7},
	})
	require.Empty(t, kept, "a failed event shadowed by a live copy in another pool must be dropped")

	// 2. A failed event for a node no pool holds alive flows through.
	ghost := serf.Member{
		Name:   "ghost-" + uniqueNodeName(t.Name()),
		Status: serf.StatusFailed,
		Tags:   map[string]string{"role": "node", "segment": "", "id": "id-ghost"},
	}
	kept = s.filterShadowedMembers(serf.MemberEvent{
		Type:    serf.EventMemberFailed,
		Members: []serf.Member{ghost},
	})
	require.Len(t, kept, 1, "a real failure must flow through")

	// 3. An alive event for web-9 is dropped: another pool holds the same
	// name with a different live node.
	aliveWeb9 := serf.Member{
		Name:   web9Default.config.NodeName,
		Status: serf.StatusAlive,
		Tags:   map[string]string{"role": "node", "segment": "", "id": idOf(web9Default)},
	}
	kept = s.filterShadowedMembers(serf.MemberEvent{
		Type:    serf.EventMemberJoin,
		Members: []serf.Member{aliveWeb9},
	})
	require.Empty(t, kept, "an alive event for a name conflicted across pools must be dropped")
	require.Contains(t, logs.String(), "node name conflict across LAN pools")
}

// The regression the advertised sl_<segment> tags exist for: the segment
// pool lives on a different address than the LAN pool (a separate NIC/VLAN),
// so a flood address guessed from the LAN address can never work. Both
// servers keep their default LAN on 127.0.0.1 and put segment "dmz" on
// 127.0.0.2; the pools must still converge.
func TestServer_SegmentPools_ConvergeWithSeparateSegmentIP(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	skipIfCannotBindToIP(t, "127.0.0.2")

	segPort := freeport.GetOne(t)
	dmzOn := func(ip string) NetworkSegment {
		sc := DefaultConfig().SerfLANConfig
		sc.MemberlistConfig.BindAddr = ip
		sc.MemberlistConfig.BindPort = segPort
		sc.MemberlistConfig.AdvertiseAddr = ip
		sc.MemberlistConfig.AdvertisePort = segPort
		return NetworkSegment{
			Name:       "dmz",
			Bind:       ip,
			Advertise:  ip,
			Port:       segPort,
			SerfConfig: sc,
		}
	}

	_, s1 := testServerWithConfig(t, func(c *Config) {
		c.Bootstrap = true
		c.SerfFloodInterval = 100 * time.Millisecond
		c.Segments = []NetworkSegment{dmzOn("127.0.0.1")}
	})
	defer s1.Shutdown()

	// s2's LAN stays on 127.0.0.1 (same as s1) while its dmz pool is only
	// reachable on 127.0.0.2 - the guessed LAN address would miss it.
	_, s2 := testServerWithConfig(t, func(c *Config) {
		c.Bootstrap = false
		c.SerfFloodInterval = 100 * time.Millisecond
		c.Segments = []NetworkSegment{dmzOn("127.0.0.2")}
	})
	defer s2.Shutdown()

	n, err := s2.JoinLAN([]string{joinAddrLAN(s1)}, s2.AgentEnterpriseMeta())
	require.NoError(t, err)
	require.Equal(t, 1, n)

	retry.Run(t, func(r *retry.R) {
		for _, s := range []*Server{s1, s2} {
			members, err := s.LANMembers(LANMemberFilter{Segment: "dmz"})
			require.NoError(r, err)
			require.Len(r, members, 2,
				"dmz pool of %s must see both servers via the advertised addresses", s.config.NodeName)
		}
	})
}

// pickReconcileMember collapses the per-pool views of a node into one
// decision: alive anywhere wins over failed/left, and the same name alive
// with different node IDs in two pools is a conflict that must not be
// registered at all.
func TestPickReconcileMember(t *testing.T) {
	member := func(name, id string, status serf.MemberStatus) serf.Member {
		return serf.Member{
			Name:   name,
			Status: status,
			Tags:   map[string]string{"role": "node", "dc": "dc1", "id": id},
		}
	}

	t.Run("alive wins over failed across pools", func(t *testing.T) {
		got := pickReconcileMember([]serf.Member{
			member("c1", "id-1", serf.StatusFailed),
			member("c1", "id-1", serf.StatusAlive),
		})
		require.NotNil(t, got)
		require.Equal(t, serf.StatusAlive, got.Status)
	})

	t.Run("no alive view picks the least terminal status", func(t *testing.T) {
		got := pickReconcileMember([]serf.Member{
			member("c1", "id-1", serf.StatusFailed),
			member("c1", "id-1", serf.StatusLeft),
		})
		require.NotNil(t, got)
		require.Equal(t, serf.StatusLeft, got.Status)
	})

	t.Run("same name alive with different node IDs is a conflict", func(t *testing.T) {
		require.Nil(t, pickReconcileMember([]serf.Member{
			member("web-1", "id-dmz", serf.StatusAlive),
			member("web-1", "id-pci", serf.StatusAlive),
		}))
	})
}

// Segments reachable at the LAN advertise IP must NOT spend serf's tag
// budget: their sl_ tags are omitted and the flooder's LAN-IP fallback
// covers them. Only a segment on a different address publishes a tag.
func TestServer_SegmentTags_OmittedForSameIP(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	skipIfCannotBindToIP(t, "127.0.0.2")

	sameIPPort := freeport.GetOne(t)
	otherIPPort := freeport.GetOne(t)

	other := segmentForTest("dmz", otherIPPort)
	other.SerfConfig.MemberlistConfig.BindAddr = "127.0.0.2"
	other.SerfConfig.MemberlistConfig.AdvertiseAddr = "127.0.0.2"

	_, s := testServerWithConfig(t, func(c *Config) {
		c.Segments = []NetworkSegment{
			segmentForTest("alpha", sameIPPort), // same IP as the LAN pool
			other,                               // separate NIC
		}
	})
	defer s.Shutdown()

	tags := s.serfLAN.LocalMember().Tags
	require.NotContains(t, tags, "sl_alpha",
		"a segment reachable at the LAN IP must not spend tag budget")
	require.Equal(t, net.JoinHostPort("127.0.0.2", strconv.Itoa(otherIPPort)), tags["sl_dmz"],
		"a segment on another address must be advertised")
}

// A leader whose segment pool has not converged must not reap catalog nodes
// it cannot see: that reap is what makes segment clients blink on every
// leadership change. The node stays registered while the pool is split (with
// a WARN), and normal reaping resumes once the pool converges.
func TestServer_Reconcile_SkipsReapWhileSegmentUnconverged(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	logs := &lockedBuffer{}
	segPort := freeport.GetOne(t)

	// The leader. Its flooder is slowed to a crawl so the test fully
	// controls when the alpha pool converges. Its advertised segment
	// address is a dead port, so neither flooder can merge the alpha pools:
	// a serf join merges both pools at once, so a single reachable side
	// would be enough to converge them.
	_, s1 := testServerWithDepsAndConfig(t, func(d *Deps) {
		d.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{
			Output: logs,
			Level:  hclog.Warn,
		})
	}, func(c *Config) {
		c.Bootstrap = true
		c.SerfFloodInterval = time.Hour
		seg := segmentForTest("alpha", segPort)
		seg.SerfConfig.MemberlistConfig.AdvertiseAddr = "127.0.0.2"
		seg.SerfConfig.MemberlistConfig.AdvertisePort = 1
		c.Segments = []NetworkSegment{seg}
	})
	defer s1.Shutdown()

	// s2 runs alpha but also advertises an address nothing listens on, so
	// s1's alpha pool can never converge; s2 still shows up in s1's default
	// pool with its sl_alpha tag, which makes alpha "unconverged" on s1.
	_, s2 := testServerWithConfig(t, func(c *Config) {
		c.Bootstrap = false
		c.SerfFloodInterval = time.Hour
		seg := segmentForTest("alpha", freeport.GetOne(t))
		seg.SerfConfig.MemberlistConfig.AdvertiseAddr = "127.0.0.2"
		seg.SerfConfig.MemberlistConfig.AdvertisePort = 1
		c.Segments = []NetworkSegment{seg}
	})
	defer s2.Shutdown()

	n, err := s2.JoinLAN([]string{joinAddrLAN(s1)}, s2.AgentEnterpriseMeta())
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Register a catalog node with a serfHealth check that no local pool
	// knows about - exactly the state a split pool leaves segment clients in.
	// The segment meta is what real clients register (agent.go) and what
	// scopes the reap guard to alpha.
	ghost := structs.RegisterRequest{
		Datacenter: s1.config.Datacenter,
		Node:       "ghost",
		Address:    "127.1.1.1",
		NodeMeta:   map[string]string{structs.MetaSegmentKey: "alpha"},
		Check: &structs.HealthCheck{
			Node:    "ghost",
			CheckID: structs.SerfCheckID,
			Name:    structs.SerfCheckName,
			Status:  api.HealthCritical,
		},
	}
	var out struct{}
	require.NoError(t, s1.RPC(context.Background(), "Catalog.Register", &ghost, &out))

	// Wait until s1's default pool actually sees s2, so that alpha counts as
	// an existing-but-unconverged segment rather than a missing one.
	retry.Run(t, func(r *retry.R) {
		members, err := s1.LANMembers(LANMemberFilter{})
		require.NoError(r, err)
		require.Len(r, members, 2)
	})

	require.False(t, s1.segmentPoolConverged("alpha", s1.serfLAN.Members()),
		"alpha must be unconverged on s1 before the first reconcile")

	require.NoError(t, s1.reconcile())

	_, node, err := s1.fsm.State().GetNode("ghost", nil, "")
	require.NoError(t, err)
	require.NotNil(t, node, "an unconverged segment pool must not let reconcile reap nodes it cannot see")
	require.Contains(t, logs.String(), "skipping the reap")

	// Converge s1's alpha pool by hand; the next pass must reap the ghost
	// again, proving the guard only holds while the pool is split.
	s2SegAddr := net.JoinHostPort("127.0.0.1",
		strconv.Itoa(s2.config.Segments[0].SerfConfig.MemberlistConfig.BindPort))
	_, err = s1.segmentLan["alpha"].Join([]string{s2SegAddr}, true)
	require.NoError(t, err)

	retry.Run(t, func(r *retry.R) {
		members, err := s1.LANMembers(LANMemberFilter{Segment: "alpha"})
		require.NoError(r, err)
		require.Len(r, members, 2, "alpha pool must be converged before the reap resumes")
	})

	require.NoError(t, s1.reconcile())

	_, node, err = s1.fsm.State().GetNode("ghost", nil, "")
	require.NoError(t, err)
	require.Nil(t, node, "once the segment pool converged, the ghost node must be reaped")
}

// splitAlphaCluster starts a two-server cluster whose alpha pools cannot
// converge: both servers advertise alpha at a dead 127.0.0.2 address, and
// neither alpha bind address is reachable from the other side, so no
// flood join can merge the pools (a serf join merges both directions at
// once). s1 is the leader.
func splitAlphaCluster(t *testing.T, depOpts func(*Deps)) (*Server, *Server) {
	skipIfCannotBindToIP(t, "127.0.0.2")

	dead := func(c *Config, bootstrap bool) {
		c.Bootstrap = bootstrap
		c.SerfFloodInterval = time.Hour
		seg := segmentForTest("alpha", freeport.GetOne(t))
		seg.SerfConfig.MemberlistConfig.AdvertiseAddr = "127.0.0.2"
		seg.SerfConfig.MemberlistConfig.AdvertisePort = 1
		c.Segments = []NetworkSegment{seg}
	}
	_, s1 := testServerWithDepsAndConfig(t, depOpts, func(c *Config) { dead(c, true) })
	_, s2 := testServerWithConfig(t, func(c *Config) { dead(c, false) })
	joinLAN(t, s2, s1)

	retry.Run(t, func(r *retry.R) {
		members, err := s1.LANMembers(LANMemberFilter{})
		require.NoError(r, err)
		require.Len(r, members, 2, "s1 must see s2 in the default pool")
	})
	require.False(t, s1.segmentPoolConverged("alpha", s1.serfLAN.Members()),
		"alpha must be unconverged on the leader")
	return s1, s2
}

// After the guard's time cap (ReconnectTimeoutLAN of continuous
// non-convergence), a segment's unknown nodes must be reaped again: a
// permanent split must not keep ghosts forever, and the extended outage must
// be logged once as an ERROR.
func TestServer_Reconcile_ReapGuardTimeCap(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	logs := &lockedBuffer{}
	s1, s2 := splitAlphaCluster(t, func(d *Deps) {
		d.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{
			Output: logs,
			Level:  hclog.Error,
		})
	})
	defer s1.Shutdown()
	defer s2.Shutdown()

	// A short cap keeps the test fast.
	s1.config.SerfLANConfig.ReconnectTimeout = 500 * time.Millisecond

	// A dead segment client the leader cannot see, registered in the catalog
	// with segment meta so the guard scopes to alpha.
	ghost := structs.RegisterRequest{
		Datacenter: s1.config.Datacenter,
		Node:       "ghost",
		Address:    "127.1.1.1",
		NodeMeta:   map[string]string{structs.MetaSegmentKey: "alpha"},
		Check: &structs.HealthCheck{
			Node:    "ghost",
			CheckID: structs.SerfCheckID,
			Name:    structs.SerfCheckName,
			Status:  api.HealthCritical,
		},
	}
	var out struct{}
	require.NoError(t, s1.RPC(context.Background(), "Catalog.Register", &ghost, &out))

	// While the segment is freshly unconverged, the ghost is protected.
	s1.setUnseenSince("alpha", time.Now())
	require.NoError(t, s1.reconcile())
	_, node, err := s1.fsm.State().GetNode("ghost", nil, "")
	require.NoError(t, err)
	require.NotNil(t, node, "inside the time cap the ghost must be protected")

	// Past the cap the ghost is reaped and the outage logged once.
	s1.setUnseenSince("alpha", time.Now().Add(-time.Hour))
	s1.resetCapLogged("alpha")
	require.NoError(t, s1.reconcile())
	_, node, err = s1.fsm.State().GetNode("ghost", nil, "")
	require.NoError(t, err)
	require.Nil(t, node, "past the time cap the ghost must be reaped")
	require.Contains(t, logs.String(), "segment pool has not converged for an extended period")
}

// The reap guard only protects nodes of the UNCONVERGED segment: a ghost of
// the default pool is reaped even while a segment pool is split.
func TestServer_Reconcile_ReapGuardScopedToSegment(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	s1, s2 := splitAlphaCluster(t, nil)
	defer s1.Shutdown()
	defer s2.Shutdown()

	register := func(name, segment string) {
		req := structs.RegisterRequest{
			Datacenter: s1.config.Datacenter,
			Node:       name,
			Address:    "127.1.1.1",
			Check: &structs.HealthCheck{
				Node:    name,
				CheckID: structs.SerfCheckID,
				Name:    structs.SerfCheckName,
				Status:  api.HealthCritical,
			},
		}
		if segment != "" {
			req.NodeMeta = map[string]string{structs.MetaSegmentKey: segment}
		}
		var out struct{}
		require.NoError(t, s1.RPC(context.Background(), "Catalog.Register", &req, &out))
	}
	register("seg-ghost", "alpha")
	register("default-ghost", "")

	s1.setUnseenSince("alpha", time.Now())
	require.NoError(t, s1.reconcile())

	_, node, err := s1.fsm.State().GetNode("seg-ghost", nil, "")
	require.NoError(t, err)
	require.NotNil(t, node, "a node of the unconverged segment is protected")

	_, node, err = s1.fsm.State().GetNode("default-ghost", nil, "")
	require.NoError(t, err)
	require.Nil(t, node, "a default-pool node must keep the normal reap path")
}

// IsCloudAutoJoin must match go-discover expressions exactly: the provider
// key must be the first key, so hosts that merely contain "provider=" are
// routed to serf instead of the discover path.
func TestIsCloudAutoJoin(t *testing.T) {
	require.True(t, IsCloudAutoJoin("provider=aws tag_key=consul"))
	require.True(t, IsCloudAutoJoin("provider=k8s label_selector=app:consul"))
	require.False(t, IsCloudAutoJoin("10.0.0.1"))
	require.False(t, IsCloudAutoJoin("10.0.0.1:8301"))
	require.False(t, IsCloudAutoJoin("provider-aware-host"))     // no "=" after the key
	require.False(t, IsCloudAutoJoin("host=x provider=aws"))     // provider not first
	require.False(t, IsCloudAutoJoin("provider="))               // empty provider value
	require.False(t, IsCloudAutoJoin("xprovider=aws tag_key=k")) // key is not exactly "provider"
}
