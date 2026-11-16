// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-metrics"
	"github.com/hashicorp/go-metrics/prometheus"
	"github.com/hashicorp/serf/serf"

	"github.com/hashicorp/consul/agent/metadata"
)

var SegmentCESummaries = []prometheus.SummaryDefinition{
	{
		Name: []string{"leader", "reconcile"},
		Help: "Measures the time spent updating the raft store from the serf member information.",
	},
}

// LANSegmentAddr is used to return the address used for the given LAN segment.
// The result is a dialable host:port - joinLAN and TestAgent.SegmentAddr hand
// it straight to memberlist as a join target, and a bare IP would silently
// resolve against the joiner's own bind port instead of the segment pool.
func (s *Server) LANSegmentAddr(name string) string {
	segmentLan := s.lanSegments()[name]
	if segmentLan == nil {
		return ""
	}
	member := segmentLan.LocalMember()
	return net.JoinHostPort(member.Addr.String(), strconv.Itoa(int(member.Port)))
}

// setupSegmentRPC sets up separate RPC listeners for network segments.
func (s *Server) setupSegmentRPC() (map[string]net.Listener, error) {
	for _, segment := range s.config.Segments {

		if segment.RPCAddr != nil {
			ln, err := net.ListenTCP("tcp", segment.RPCAddr)
			if err != nil {
				return nil, fmt.Errorf("segment %q: listen on %s: %w", segment.Name, segment.RPCAddr, err)
			}
			// Register as we go, so the s.Shutdown() that NewServer calls on
			// any startup error also releases listeners created so far.
			s.segmentListeners[segment.Name] = ln
		}
	}

	return s.segmentListeners, nil
}

// setupSegments initializes Serf instances for each configured network segment.
func (s *Server) setupSegments(config *Config, rpcListeners map[string]net.Listener) error {

	for _, segment := range config.Segments {

		listener := rpcListeners[segment.Name]

		if listener == nil {
			listener = s.Listener
		}

		ln, _, err := s.setupSerf(setupSerfOptions{
			Config:       segment.SerfConfig,
			EventCh:      s.eventChLAN,
			SnapshotPath: fmt.Sprintf("serf/%s.snapshot", strings.ToLower(segment.Name)),
			Listener:     listener,
			WAN:          false,
			Segment:      segment.Name,
			Partition:    "",
		})

		if err != nil {
			return fmt.Errorf("segment %q: start LAN serf: %w", segment.Name, err)
		}

		s.segmentLan[segment.Name] = ln
	}

	return nil
}

// floodSegments starts one convergence loop per segment pool. Every flooder
// keeps the servers known from the default LAN pool joined into its segment
// pool, so segment pools converge across servers no matter which shape
// retry_join took: a bare address, an explicit host:port, or none at all.
//
// The join address is the server's own advertised segment address, published
// as an sl_<segment> tag on the default LAN pool, so a segment that lives on
// its own NIC/VLAN converges even though its address differs from the LAN
// address. Servers without the tag (for example while a new segment is being
// rolled out server-by-server) are skipped without dialing anything.
func (s *Server) floodSegments(config *Config) {
	for _, sc := range config.Segments {
		go s.segmentFloodLoop(sc.Name, s.segmentLan[sc.Name], s.config.SerfFloodInterval)
	}
}

// segmentFlapBackoffPasses is how many flood intervals a server that stays
// unreachable after a flood-join attempt is skipped before the next attempt.
// A server whose segment port passes TCP but drops UDP (a common firewall
// mistake) would otherwise be resurrected by every pass and flap between
// alive and failed for every client of the segment.
const segmentFlapBackoffPasses = 5

// segmentFloodState tracks, per server, how the previous flood passes went.
type segmentFloodState struct {
	mu       sync.Mutex
	cooldown map[string]int  // passes to skip before the next join attempt
	pending  map[string]bool // joined on the previous pass; watch if it stays missing
}

func newSegmentFloodState() *segmentFloodState {
	return &segmentFloodState{
		cooldown: make(map[string]int),
		pending:  make(map[string]bool),
	}
}

// clear forgets any backoff for a server that is alive in the segment pool.
func (st *segmentFloodState) clear(name string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.cooldown, name)
	delete(st.pending, name)
}

// joinedRecently reports whether the given server was joined on the previous
// pass, consuming the marker.
func (st *segmentFloodState) joinedRecently(name string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pending[name] {
		st.pending[name] = false
		return true
	}
	return false
}

// setBackoff skips the next n passes for the given server.
func (st *segmentFloodState) setBackoff(name string, n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.cooldown[name] = n
}

// takeBackoff consumes one backoff pass; it returns true while the server is
// still backed off.
func (st *segmentFloodState) takeBackoff(name string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cooldown[name] > 0 {
		st.cooldown[name]--
		return true
	}
	return false
}

// markPending records that the given server was joined just now, so the next
// pass can tell "joined and still missing" (flapping) apart from "never
// tried".
func (st *segmentFloodState) markPending(name string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pending[name] = true
}

// segmentFloodLoop is the long-running per-segment convergence loop. Unlike
// Flood (the LAN→WAN flooder), member names are compared exactly: LAN pool
// member names carry no datacenter suffix, so a node named "consul01.prod"
// in datacenter "prod" must not be flood-joined forever.
func (s *Server) segmentFloodLoop(segment string, dstSerf *serf.Serf, interval time.Duration) {
	// Register in floodCh so FloodNotify wakes the loop on LAN joins, the
	// same way it wakes the WAN flooder.
	s.floodLock.Lock()
	floodCh := make(chan struct{})
	s.floodCh = append(s.floodCh, floodCh)
	s.floodLock.Unlock()
	defer func() {
		s.floodLock.Lock()
		defer s.floodLock.Unlock()

		for i, ch := range s.floodCh {
			if ch == floodCh {
				s.floodCh = append(s.floodCh[:i], s.floodCh[i+1:]...)
				return
			}
		}
		panic("flood channels out of sync")
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	state := newSegmentFloodState()
	warned := false
	for {
		select {
		case <-s.serfLAN.ShutdownCh():
			return

		case <-dstSerf.ShutdownCh():
			return

		case <-ticker.C:
		case <-floodCh:
		}

		missing := s.segmentFloodJoins(segment, dstSerf, state)
		metrics.SetGaugeWithLabels(
			[]string{"serf", "flood", "segment_missing_servers"},
			float32(len(missing)),
			[]metrics.Label{{Name: "segment", Value: segment}},
		)

		if len(missing) > 0 {
			if !warned {
				s.logger.Warn(
					"segment pool has not converged: some servers are missing or flapping "+
						"(reachable over TCP but failing UDP probes?); clients of this segment "+
						"may blink in the catalog until it converges",
					"segment", segment, "missing_servers", missing)
				warned = true
			}
		} else if warned {
			s.logger.Info("segment pool converged", "segment", segment)
			warned = false
		}
	}
}

// segmentFloodPlan decides which servers one flood pass must try to join
// into the segment pool: alive default-LAN servers that advertise the segment
// but are not alive in its pool. It is a pure function so the convergence
// logic can be unit-tested without serf instances. `missing` lists every
// advertised server that is not alive in the pool (including ones the caller
// may back off from joining); `targets` is the subset due a join attempt.
func segmentFloodPlan(segment string, lanMembers, segmentMembers []serf.Member) (targets []metadata.Server, missing []string) {
	// Names are compared exactly: LAN pool member names carry no datacenter
	// suffix, so a node named "consul01.prod" in datacenter "prod" matches
	// its own pool copy instead of being flood-joined forever.
	alive := make(map[string]struct{})
	for _, m := range segmentMembers {
		if m.Status == serf.StatusAlive {
			alive[m.Name] = struct{}{}
		}
	}

	for _, m := range lanMembers {
		if m.Status != serf.StatusAlive {
			continue
		}
		ok, server := metadata.IsConsulServer(m)
		if !ok {
			continue
		}
		if _, hasSegment := server.SegmentAddrs[segment]; !hasSegment {
			// The server does not run this segment (for example during the
			// rolling addition of a new one); never dial a guessed address.
			continue
		}
		if _, ok := alive[server.Name]; ok {
			continue
		}
		missing = append(missing, server.Name)
		targets = append(targets, *server)
	}
	return targets, missing
}

// segmentFloodJoins performs one flood pass: it joins the planned servers
// into the segment pool and returns the names of the advertised servers that
// are still not alive there.
func (s *Server) segmentFloodJoins(segment string, dstSerf *serf.Serf, state *segmentFloodState) []string {
	lanMembers := s.serfLAN.Members()
	segmentMembers := dstSerf.Members()

	targets, missing := segmentFloodPlan(segment, lanMembers, segmentMembers)

	for _, m := range segmentMembers {
		if m.Status == serf.StatusAlive {
			state.clear(m.Name)
		}
	}

	for _, server := range targets {
		if state.joinedRecently(server.Name) {
			// Joined on the previous pass and still not alive: it is
			// flapping. Back off instead of resurrecting it every interval.
			state.setBackoff(server.Name, segmentFlapBackoffPasses)
			continue
		}
		if state.takeBackoff(server.Name) {
			continue
		}

		state.markPending(server.Name)
		joinAddr := net.JoinHostPort(server.SegmentAddrs[segment], strconv.Itoa(server.SegmentPorts[segment]))
		if _, err := dstSerf.Join([]string{server.Name + "/" + joinAddr}, true); err != nil {
			s.logger.Debug("segment flood-join failed", "segment", segment, "server", server.Name, "address", joinAddr, "error", err)
			state.setBackoff(server.Name, 1)
		}
	}
	return missing
}

// IsCloudAutoJoin reports whether a join address is a go-discover expression
// ("provider=aws tag_key=...") rather than a host or host:port. It is the one
// test shared by join routing and config validation, and matches the check
// retry_join uses to resolve such entries (agent/retry_join.go).
func IsCloudAutoJoin(addr string) bool {
	return strings.Contains(addr, "provider=")
}
