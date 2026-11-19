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

	"github.com/hashicorp/go-hclog"
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
			EventCh:      s.poolEventCh,
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
// as an sl_<segment> tag on the default LAN pool - but only when the segment
// is not reachable at the LAN advertise IP (see initEnterprise). For a
// tag-less server the flooder falls back to the LAN IP plus the segment
// port; a refused connection there means the server does not run the
// segment, so it is skipped quietly from then on.
func (s *Server) floodSegments(config *Config) {
	if len(config.Segments) > 0 {
		// Forward the pooled events to eventChLAN. The pools buffered their
		// events since setupSegments; the filter drains the backlog on start
		// (a single channel keeps each pool's order).
		go s.poolEventFilter()
	}
	for _, sc := range config.Segments {
		_, port := segmentAdvertiseHostPort(sc)
		flooder := &segmentFlooder{
			logger:  s.logger,
			segment: sc.Name,
			port:    port,
			state:   s.floodState[sc.Name],
		}
		pool := s.segmentLan[sc.Name]
		warner := &segmentFloodWarner{}
		warned := false
		go s.segmentFloodLoop(pool, func() {
			now := time.Now()
			missing := flooder.pass(s.config.Datacenter, s.serfLAN, pool, now)
			metrics.SetGaugeWithLabels(
				[]string{"serf", "flood", "segment_missing_servers"},
				float32(len(missing)),
				[]metrics.Label{{Name: "segment", Value: flooder.segment}},
			)

			if warner.observe(len(missing) > 0, now) {
				s.logger.Warn(
					"segment pool has not converged: some servers are missing "+
						"(unreachable, or refusing connections); clients of this segment "+
						"may blink in the catalog until it converges",
					"segment", flooder.segment, "missing_servers", missing)
				warned = true
			} else if warned && len(missing) == 0 {
				s.logger.Info("segment pool converged", "segment", flooder.segment)
				warned = false
			}
		})
	}
}

// segmentFloodWarnInterval rate-limits the warning for servers the segment
// flooder keeps failing to join; every attempt still logs at Debug.
const segmentFloodWarnInterval = 5 * time.Minute

// segmentFloodWarnAfter is how many flood passes in a row must fail before
// the flooder warns: a peer that is still starting fails a pass or two.
const segmentFloodWarnAfter = 3

// segmentFloodWarner decides when one segment's flooder reports servers it
// cannot join. It is only touched by that segment's flood goroutine.
type segmentFloodWarner struct {
	fails   int
	limiter warnLimiter
}

// observe records one flood pass and reports whether to warn about it: the
// failure has lasted segmentFloodWarnAfter passes in a row and no warning
// went out within segmentFloodWarnInterval.
func (w *segmentFloodWarner) observe(failed bool, now time.Time) bool {
	if !failed {
		w.fails = 0
		return false
	}
	w.fails++
	return w.fails >= segmentFloodWarnAfter && w.limiter.allow(now, segmentFloodWarnInterval)
}

// warnLimiter lets a repeating condition warn at most once per interval. The
// zero value is ready to use and allows the first warning.
type warnLimiter struct {
	mu   sync.Mutex
	last time.Time
}

// allow reports whether a warning may go out at now, and records it if so.
func (w *warnLimiter) allow(now time.Time, every time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.last.IsZero() && now.Sub(w.last) < every {
		return false
	}
	w.last = now
	return true
}

// segmentFloodLoop runs the pass on every flood interval and on every
// FloodNotify until the default LAN pool or the segment pool shuts down. It
// mirrors Flood (the LAN→WAN flooder); the pass is the segment joiner with
// its exact name matching and retry backoff.
func (s *Server) segmentFloodLoop(dstSerf *serf.Serf, pass func()) {
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

	ticker := time.NewTicker(s.config.SerfFloodInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.serfLAN.ShutdownCh():
			return

		case <-dstSerf.ShutdownCh():
			return

		case <-ticker.C:
		case <-floodCh:
		}

		pass()
	}
}

const (
	// segmentFloodRetryBase is the first retry delay after a failed or
	// ineffective flood join; segmentFloodRetryMax caps the exponential
	// growth. A join that keeps "succeeding" over TCP while the member
	// never becomes alive (UDP filtered) is backed off the same way, so the
	// flooder cannot resurrect a half-reachable server every interval.
	segmentFloodRetryBase = 30 * time.Second
	segmentFloodRetryMax  = 10 * time.Minute
)

// segmentFloodState tracks, per default-pool server, when the flooder may
// dial it again and whether a fallback join was refused (which means the
// server does not run the segment). The flooder's goroutine and the leader's
// reconcile loop (which reads the refused set) both touch it.
type segmentFloodState struct {
	mu          sync.Mutex
	nextAttempt map[string]time.Time
	retries     map[string]int  // consecutive attempts while the server stays missing
	refused     map[string]bool // fallback join was refused: server does not run the segment

	baseDelay time.Duration
	maxDelay  time.Duration
}

func newSegmentFloodState() *segmentFloodState {
	return &segmentFloodState{
		nextAttempt: make(map[string]time.Time),
		retries:     make(map[string]int),
		refused:     make(map[string]bool),
		baseDelay:   segmentFloodRetryBase,
		maxDelay:    segmentFloodRetryMax,
	}
}

// clear forgets all state for a server that is alive in the segment pool.
func (st *segmentFloodState) clear(name string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.nextAttempt, name)
	delete(st.retries, name)
	delete(st.refused, name)
}

// due reports whether the server may be dialed at now.
func (st *segmentFloodState) due(name string, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	next, ok := st.nextAttempt[name]
	return !ok || !now.Before(next)
}

// reschedule sets the next attempt after one just happened, backing off
// exponentially while the server stays missing.
func (st *segmentFloodState) reschedule(name string, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delay := st.baseDelay << st.retries[name]
	if delay > st.maxDelay || delay <= 0 {
		delay = st.maxDelay
	}
	st.retries[name]++
	st.nextAttempt[name] = now.Add(delay)
}

// markRefused records that the server's fallback join was refused.
func (st *segmentFloodState) markRefused(name string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.refused[name] = true
	delete(st.nextAttempt, name)
	delete(st.retries, name)
}

// refusedFallback reports whether the server's fallback join was refused, so
// the flooder skips it and the reap guard stops expecting it in the pool.
func (st *segmentFloodState) refusedFallback(name string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.refused[name]
}

// refusedNames returns the names of every server whose fallback join was
// refused. The reap guard excludes them from the servers a converged pool
// must contain.
func (st *segmentFloodState) refusedNames() map[string]struct{} {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]struct{}, len(st.refused))
	for name := range st.refused {
		out[name] = struct{}{}
	}
	return out
}

// segmentFlooder joins the default pool's servers into one segment pool.
type segmentFlooder struct {
	logger  hclog.Logger
	segment string
	port    int // this server's configured port for the segment (fallback)
	state   *segmentFloodState
}

// segmentFloodTarget returns the address to dial for a server: the advertised
// sl_<segment> address when the server publishes one, and the LAN IP plus
// the local segment port otherwise.
func segmentFloodTarget(srv *metadata.Server, segment string, fallbackPort int) (string, bool) {
	if host, ok := srv.SegmentAddrs[segment]; ok {
		return net.JoinHostPort(host, strconv.Itoa(srv.SegmentPorts[segment])), true
	}
	tcpAddr, ok := srv.Addr.(*net.TCPAddr)
	if !ok || tcpAddr == nil || tcpAddr.IP == nil {
		return "", false
	}
	return net.JoinHostPort(tcpAddr.IP.String(), strconv.Itoa(fallbackPort)), false
}

// segmentFloodCandidates performs the selection half of a flood pass: it
// returns the servers alive in the segment pool (keyed by exact member name,
// so the flooder can clear their retry state) and the LAN pool servers that
// are alive there but missing from the segment pool. Member names are
// compared exactly: LAN pool member names carry no datacenter suffix, so a
// node named "consul01.prod" in datacenter "prod" must not be matched against
// a suffix-trimmed "consul01" and flood-joined forever.
func segmentFloodCandidates(datacenter string, lanMembers, segmentMembers []serf.Member) (map[string]struct{}, []metadata.Server) {
	alive := make(map[string]struct{})
	for _, m := range segmentMembers {
		if m.Status != serf.StatusAlive {
			continue
		}
		if ok, srv := metadata.IsConsulServer(m); ok && srv.Datacenter == datacenter {
			alive[srv.Name] = struct{}{}
		}
	}

	var candidates []metadata.Server
	for _, m := range lanMembers {
		if m.Status != serf.StatusAlive {
			continue
		}
		ok, srv := metadata.IsConsulServer(m)
		if !ok {
			continue
		}
		if _, ok := alive[srv.Name]; ok {
			continue
		}
		candidates = append(candidates, *srv)
	}
	return alive, candidates
}

// pass performs one flood pass and returns the servers that advertise (or
// should serve) this segment but are not alive in its pool. Refused
// fallbacks are skipped silently; servers inside their retry backoff are
// reported but not dialed. Candidate selection lives in
// segmentFloodCandidates; this half owns the join side and its retry state.
func (f *segmentFlooder) pass(datacenter string, srcSerf, dstSerf *serf.Serf, now time.Time) (missing []string) {
	alive, candidates := segmentFloodCandidates(datacenter, srcSerf.Members(), dstSerf.Members())
	for name := range alive {
		f.state.clear(name)
	}

	for i := range candidates {
		srv := &candidates[i]
		if f.state.refusedFallback(srv.Name) {
			continue
		}

		missing = append(missing, srv.Name)
		if !f.state.due(srv.Name, now) {
			continue
		}

		joinAddr, advertised := segmentFloodTarget(srv, f.segment, f.port)
		f.state.reschedule(srv.Name, now)
		if _, err := dstSerf.Join([]string{srv.Name + "/" + joinAddr}, true); err != nil {
			f.logger.Debug("segment flood-join failed", "segment", f.segment, "server", srv.Name, "address", joinAddr, "error", err)
			if !advertised && isConnectionRefused(err) {
				// Nothing listens at the fallback address: the server does
				// not run this segment. Skip it quietly instead of warning
				// about a port that was never opened.
				f.state.markRefused(srv.Name)
				missing = missing[:len(missing)-1]
			}
		}
	}
	return missing
}

// isConnectionRefused reports whether err is a refused TCP connection.
// memberlist formats join errors without wrapping, so the errno cannot be
// recovered with errors.Is; the message is checked instead ("connection
// refused" on unix, "actively refused it" on Windows).
func isConnectionRefused(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "refused")
}

// IsCloudAutoJoin reports whether a join address is a go-discover expression
// ("provider=aws tag_key=...") rather than a host or host:port. It is the one
// test shared by join routing and config validation, and matches the check
// retry_join uses to resolve such entries (agent/retry_join.go). The
// provider key must be the first key of the expression, matched exactly, so
// hosts that merely contain "provider=" are not misclassified.
func IsCloudAutoJoin(addr string) bool {
	first, _, _ := strings.Cut(addr, " ")
	key, value, ok := strings.Cut(first, "=")
	return ok && key == "provider" && value != ""
}
