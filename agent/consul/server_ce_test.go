// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/serf/serf"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/agent/metadata"
	"github.com/hashicorp/consul/sdk/freeport"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	"github.com/hashicorp/consul/testrpc"
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

	require.NotEmpty(t, s.LANSegments())

	s.handleEnterpriseLeave()

	for name, pool := range s.LANSegments() {
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

// P0-1: segment pools between servers must converge through flooding,
// whatever shape retry_join took. Both servers live on this host and use the
// same segment port numbers, bound on different loopback addresses
// (127.0.0.1 vs 127.0.0.2). The join below carries an explicit LAN port,
// which reaches the default pool only: without flooding every segment pool
// keeps a single member, reconcileReaped then reaps the other server's
// segment clients out of the catalog, and they re-register forever.
//
// 21 segments (> 20) prove the design is not bounded by serf's 512-byte tag
// budget: variant B publishes no sl_* tags at all.
func TestServer_SegmentPools_ConvergeViaFlooding(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	skipIfCannotBindToIP(t, "127.0.0.2")

	const segmentCount = 21
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

// P1-3: only the default pool decides whether JoinLAN succeeded. An
// unreachable segment port has to be a WARN, not an error: retry_join treats
// err != nil as a failure and, with retry_max set, exits the agent with
// "max join retry exhausted" - even though the LAN join itself worked.
// Segment pools are converged by the flooders (floodSegments) anyway.
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

	require.Contains(t, logs.String(), "segment join failed",
		"the segment failure must be logged as a warning, not returned")
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

// segmentFloodAddr must replace the RPC port carried in the server's address
// with the segment's port, bracket IPv6 so memberlist can parse the result,
// and turn an unusable address into an error (FloodJoins logs it at debug and
// keeps going) instead of panicking.
func TestSegmentFloodAddr(t *testing.T) {
	srv := &metadata.Server{
		Name: "node1.dc1",
		Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 8300},
	}

	addr, err := segmentFloodAddr(srv, 8303)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.5:8303", addr,
		"the RPC port must be replaced by the segment port")

	srv.Addr = &net.TCPAddr{IP: net.ParseIP("fd00::1"), Port: 8300}
	addr, err = segmentFloodAddr(srv, 8303)
	require.NoError(t, err)
	require.Equal(t, "[fd00::1]:8303", addr)

	srv.Addr = nil
	_, err = segmentFloodAddr(srv, 8303)
	require.Error(t, err, "an unusable address must come back as an error, not a panic")
}
