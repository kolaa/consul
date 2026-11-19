// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-metrics"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/serf/coordinate"
	"github.com/hashicorp/serf/serf"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/consul/reporting"
	"github.com/hashicorp/consul/agent/consul/state"
	resourcegrpc "github.com/hashicorp/consul/agent/grpc-external/services/resource"
	"github.com/hashicorp/consul/agent/metadata"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/internal/gossip/librtt"
	"github.com/hashicorp/consul/internal/resource"
	"github.com/hashicorp/consul/logging"
)

// runEnterpriseRateLimiterConfigEntryController start the rate limiter config controller
func (s *Server) runEnterpriseRateLimiterConfigEntryController() error {
	return nil
}

func (s *Server) setupEnterpriseGRPCServices(config *Config, deps Deps) error {
	return nil
}

func (s *Server) enterpriseValidateJoinWAN() error {
	return nil // no-op
}

// JoinLAN is used to have Consul join the inner-DC pool The target address
// should be another node inside the DC listening on the Serf LAN address.
//
// Only the default pool decides whether the join succeeded: retry_join
// treats a non-nil error as a failure and, with retry_max set, exits the
// agent with "max join retry exhausted" - so a segment pool that cannot be
// reached (segment port firewalled, first server of a rolling upgrade not
// yet listening) must not fail an otherwise successful LAN join. Segment
// pools are converged by the flooders anyway (see floodSegments); explicit
// host:<segment port> entries are honoured best-effort and only logged.
func (s *Server) JoinLAN(addrs []string, entMeta *acl.EnterpriseMeta) (int, error) {
	lanAddrs, segmentAddrs := segmentJoinAddrsExplicitOnly(addrs, s.config.Segments)

	// Segment joins run first, in parallel: each unreachable segment address
	// would otherwise block the next one for up to memberlist's TCPTimeout
	// (10s), and a manual `consul join host:<segPort>` must still repair a
	// split segment pool even when the address list has nothing for the
	// default pool (the error below says so).
	names := slices.Sorted(maps.Keys(segmentAddrs))
	segErrs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, segErrs[i] = s.segmentLan[name].Join(segmentAddrs[name], true)
		}()
	}
	wg.Wait()
	for i, name := range names {
		if segErrs[i] != nil {
			s.logger.Warn(
				"explicit segment join failed; only the segment flooder will retry, "+
					"against the advertised segment address",
				"segment", name, "error", segErrs[i])
		}
	}

	// Every address carrying a configured segment port means the default
	// pool has nothing to join. Returning (0, nil) here would let
	// retry_join log a successful join while the node stays out of the LAN
	// cluster, so fail loudly instead.
	if len(addrs) > 0 && len(lanAddrs) == 0 {
		return 0, fmt.Errorf(
			"cannot join the default LAN pool: every join address (%v) carries a configured segment port; use a bare address or one with the serf_lan port instead",
			addrs)
	}

	n := 0
	var err error
	if len(lanAddrs) > 0 {
		n, err = s.serfLAN.Join(lanAddrs, true)
	}
	return n, err
}

// segmentJoinAddrsExplicitOnly splits join addresses into the addresses that
// belong to the default serf LAN pool and, per segment name, the addresses
// that belong to that segment's pool.
//
//   - An address with an explicit port matching a configured segment port
//     goes to that segment's pool only.
//   - Everything else - a bare address or an explicit non-segment port -
//     goes to the default pool only. Bare addresses are deliberately not
//     expanded per segment any more: segment pools are converged by the
//     flooders (see floodSegments), so the default pool alone decides
//     whether a retry_join attempt succeeded.
//
// This assumes the (usual) deployment where every server in the datacenter is
// configured with the same segment names and ports.
func segmentJoinAddrsExplicitOnly(addrs []string, segments []NetworkSegment) ([]string, map[string][]string) {
	segmentByPort := make(map[int]string, len(segments))
	for _, segment := range segments {
		if segment.Port > 0 {
			segmentByPort[segment.Port] = segment.Name
		}
	}

	lanAddrs := make([]string, 0, len(addrs))
	segmentAddrs := make(map[string][]string)

	for _, addr := range addrs {
		if _, portStr, err := net.SplitHostPort(addr); err == nil {
			if port, perr := strconv.Atoi(portStr); perr == nil {
				if name, ok := segmentByPort[port]; ok {
					segmentAddrs[name] = append(segmentAddrs[name], addr)
					continue
				}
			}
		}
		lanAddrs = append(lanAddrs, addr)
	}

	return lanAddrs, segmentAddrs
}

// removeFailedNode is used to remove a failed node from the cluster
//
// if node is empty, just remove wanNode from the WAN
func (s *Server) removeFailedNode(
	removeFn func(*serf.Serf, string) error,
	node, wanNode string,
	entMeta *acl.EnterpriseMeta,
) error {
	maybeRemove := func(s *serf.Serf, node string) (bool, error) {
		if !isSerfMember(s, node) {
			return false, nil
		}
		return true, removeFn(s, node)
	}

	foundAny := false

	var merr error

	if node != "" {
		if found, err := maybeRemove(s.serfLAN, node); err != nil {
			merr = multierror.Append(merr, fmt.Errorf("could not remove failed node from LAN: %w", err))
		} else if found {
			foundAny = true
		}

		for _, segment := range s.segmentNames() {
			if found, err := maybeRemove(s.segmentLan[segment], node); err != nil {
				merr = multierror.Append(merr, fmt.Errorf("could not remove failed node from segment %s LAN: %w", segment, err))
			} else if found {
				foundAny = true
			}
		}
	}

	if s.serfWAN != nil {
		if found, err := maybeRemove(s.serfWAN, wanNode); err != nil {
			merr = multierror.Append(merr, fmt.Errorf("could not remove failed node from WAN: %w", err))
		} else if found {
			foundAny = true
		}
	}

	if merr != nil {
		return merr
	}

	if !foundAny {
		return fmt.Errorf("agent: No node found with name '%s'", node)
	}

	return nil
}

// lanPoolAllMembers returns all LAN members across the default segment and all configured network segments.
func (s *Server) lanPoolAllMembers() ([]serf.Member, error) {

	members := s.LANMembersInAgentPartition()

	for _, name := range s.segmentNames() {
		for _, member := range s.segmentLan[name].Members() {
			if member.Tags["role"] == "consul" {
				continue
			}
			members = append(members, member)
		}
	}

	return members, nil
}

// LANMembers returns the LAN members for one of:
//
// - the requested partition
// - the requested segment
// - all segments
//
// This is limited to segments and partitions that the node is a member of.
func (s *Server) LANMembers(filter LANMemberFilter) ([]serf.Member, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	if filter.AllSegments {
		return s.lanPoolAllMembers()
	}
	if filter.Segment != "" {
		pool, ok := s.lanSegments()[filter.Segment]
		if !ok {
			return nil, fmt.Errorf("network segment %q not found", filter.Segment)
		}
		return pool.Members(), nil
	}
	return s.LANMembersInAgentPartition(), nil
}

func (s *Server) GetMatchingLANCoordinate(_, segment string) (*coordinate.Coordinate, error) {
	if segment == "" {
		return s.serfLAN.GetCoordinate()
	}

	pool, ok := s.segmentLan[segment]
	if !ok {
		return nil, fmt.Errorf("network segment %q not found", segment)
	}

	return pool.GetCoordinate()
}

func (s *Server) addEnterpriseLANCoordinates(cs librtt.CoordinateSet) error {

	for _, name := range s.segmentNames() {
		c, err := s.segmentLan[name].GetCoordinate()
		if err != nil {
			return err
		}
		cs[name] = c
	}

	return nil
}

func (s *Server) LANSendUserEvent(name string, payload []byte, coalesce bool) error {
	// The event has to reach every pool the server belongs to: the default
	// LAN pool and every segment pool. Each pool delivers its own copy to
	// the shared event channel; the agent drops the repeats by UserEvent.ID.
	var errs []error
	if err := s.serfLAN.UserEvent(name, payload, coalesce); err != nil {
		errs = append(errs, fmt.Errorf("error broadcasting event on default LAN pool: %w", err))
	}
	for _, segmentName := range s.segmentNames() {
		if err := s.segmentLan[segmentName].UserEvent(name, payload, coalesce); err != nil {
			errs = append(errs, fmt.Errorf("error broadcasting event on segment %q: %w", segmentName, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("error broadcasting event: %w", err)
	}
	return nil
}

func (s *Server) DoWithLANSerfs(
	fn func(name, poolKind string, pool *serf.Serf) error,
	errorFn func(name, poolKind string, err error) error,
) error {
	if errorFn == nil {
		errorFn = func(_, _ string, err error) error { return err }
	}
	err := fn("", "", s.serfLAN)
	if err != nil {
		return errorFn("", "", err)
	}

	for _, name := range s.segmentNames() {
		err := fn(name, PoolKindSegment, s.segmentLan[name])
		if err != nil {
			return errorFn(name, PoolKindSegment, err)
		}
	}

	return nil
}

// reconcile is used to reconcile the differences between Serf membership and
// what is reflected in our strongly consistent store. Mainly we need to ensure
// all live nodes are registered, all failed nodes are marked as such, and all
// left nodes are deregistered.
func (s *Server) reconcile() (err error) {
	defer metrics.MeasureSince([]string{"leader", "reconcile"}, time.Now())

	// Collapse the member views of every pool into one decision per node
	// name: a node that shows up in several pools (a client that moved
	// segments, a hostname reused in two segments) must be reconciled once
	// per pass, with an alive member winning over a failed or left one, or
	// its serfHealth flips between passing and critical on every pass.
	members := s.lanMembersByNode()

	known := make(map[string]struct{}, len(members))
	for _, name := range slices.Sorted(maps.Keys(members)) {
		known[strings.ToLower(name)] = struct{}{}

		m := pickReconcileMember(members[name])
		if m == nil {
			// The same node name is alive in several pools with conflicting
			// node IDs. Registering either view would flap the catalog, so
			// log and touch nothing; the name stays in `known` so the entry
			// is not reaped either.
			s.logger.Error(
				"node name conflict across LAN pools: the name is alive with different node IDs; refusing to reconcile it",
				"node", name, "pools", memberPoolTags(members[name]))
			continue
		}
		if m.Status == serf.StatusAlive {
			s.recordNodeSegment(*m)
		}
		if err := s.reconcileMember(*m); err != nil {
			return err
		}
	}

	// A leader whose segment pools have not converged must not reap catalog
	// nodes it simply cannot see: reaping them is what makes segment clients
	// blink whenever leadership moves. Protect the affected nodes for this
	// pass instead.
	if err := s.protectUnconvergedSegments(known); err != nil {
		return err
	}

	// Reconcile any members that have been reaped while we were not the
	// leader.
	return s.reconcileReaped(known, nil)
}

// lanMembersByNode gathers the members of the default pool and of every
// segment pool, keyed by node name. Server copies in segment pools are
// excluded: servers are reconciled from the default pool.
func (s *Server) lanMembersByNode() map[string][]serf.Member {
	out := make(map[string][]serf.Member)
	for _, m := range s.serfLAN.Members() {
		out[m.Name] = append(out[m.Name], m)
	}
	for _, name := range s.segmentNames() {
		for _, m := range s.segmentLan[name].Members() {
			if m.Tags["role"] == "consul" {
				continue
			}
			out[m.Name] = append(out[m.Name], m)
		}
	}
	return out
}

// pickReconcileMember chooses the single member view to reconcile for a node
// name that appears in several pools. An alive member wins over any non-alive
// one; several alive members with different node IDs mean the name is used by
// different nodes in different pools, and nil is returned so the caller
// refuses to register either view.
func pickReconcileMember(members []serf.Member) *serf.Member {
	alive := -1
	aliveIDs := make(map[string]struct{})
	for i, m := range members {
		if m.Status != serf.StatusAlive {
			continue
		}
		aliveIDs[m.Tags["id"]] = struct{}{}
		if alive == -1 {
			alive = i
		}
	}
	if alive >= 0 {
		if len(aliveIDs) > 1 {
			return nil
		}
		return &members[alive]
	}

	// No alive view: take the least-terminal status (leaving < left < failed)
	// so a node that moved between pools settles on one registration.
	best := 0
	for i := range members {
		if members[i].Status < members[best].Status {
			best = i
		}
	}
	return &members[best]
}

// memberPoolTags describes where each member view of a conflicting node name
// was seen, for the conflict log message.
func memberPoolTags(members []serf.Member) []any {
	out := make([]any, 0, len(members))
	for _, m := range members {
		segment := m.Tags["segment"]
		if segment == "" {
			segment = "default"
		}
		out = append(out, fmt.Sprintf("%s(status=%s id=%s)", segment, m.Status, m.Tags["id"]))
	}
	return out
}

// poolEventFilter forwards the LAN pools' serf events to eventChLAN, dropping
// events that a cross-pool view proves stale. Without it the leader's
// event-driven reconcile (which sees one pool's view) marks a moved agent
// critical when the old pool's copy fails, and deregisters it by name once
// the old copy is reaped - the exact blinking the periodic pass deduplicates.
//
// The filter decides from current membership, not from the order events
// arrive in: pools deliver from different goroutines, and a graceful move can
// deliver the new pool's join and the old pool's leave in any order.
func (s *Server) poolEventFilter() {
	for {
		select {
		case e := <-s.poolEventCh:
			if me, ok := e.(serf.MemberEvent); ok {
				me.Members = s.filterShadowedMembers(me)
				if len(me.Members) == 0 {
					continue
				}
				e = me
			}
			select {
			case s.eventChLAN <- e:
			case <-s.shutdownCh:
				return
			}
		case <-s.shutdownCh:
			return
		}
	}
}

// filterShadowedMembers drops the members of one event whose view the
// cross-pool state proves stale, and records where every alive client lives
// for the reap guard. Server members always flow through: every pool holds a
// copy of every server, so filtering them could drop a real failure, and
// moved/duplicate hostnames are a client problem.
func (s *Server) filterShadowedMembers(me serf.MemberEvent) []serf.Member {
	// alive indexes, per LAN pool ("": the default pool), the node IDs alive
	// there. A member's own pool is its segment tag, so an event can be
	// judged against the OTHER pools.
	alive := s.aliveClientIndex()

	out := make([]serf.Member, 0, len(me.Members))
	for _, m := range me.Members {
		s.recordNodeSegment(m)

		if m.Tags["role"] == "consul" {
			out = append(out, m)
			continue
		}

		key := strings.ToLower(m.Name)
		ownPool := m.Tags["segment"]
		switch me.Type {
		case serf.EventMemberJoin, serf.EventMemberUpdate:
			// The same name alive with a DIFFERENT node ID in ANOTHER pool is
			// a duplicate hostname; registering either view flips the catalog
			// entry, so drop it and count it. The member's own pool is
			// expected to hold it alive with its own ID.
			conflicted := false
			for pool, ids := range alive[key] {
				if pool == ownPool {
					continue
				}
				if _, ok := ids[m.Tags["id"]]; !ok && len(ids) > 0 {
					conflicted = true
					break
				}
			}
			if m.Status == serf.StatusAlive && conflicted {
				metrics.IncrCounterWithLabels(
					[]string{"leader", "reconcile", "name_conflict"},
					1,
					[]metrics.Label{{Name: "segment", Value: m.Tags["segment"]}},
				)
				s.logger.Error(
					"node name conflict across LAN pools: the name is alive with different node IDs; dropping the event",
					"node", m.Name, "segment", m.Tags["segment"], "id", m.Tags["id"])
				continue
			}
		default:
			// failed, left, reap, leaving: drop the event when ANY pool holds
			// the same node (same ID) alive - it moved to another pool, or
			// the event is older than the node's current liveness.
			shadowed := false
			for _, ids := range alive[key] {
				if _, ok := ids[m.Tags["id"]]; ok {
					shadowed = true
					break
				}
			}
			if shadowed {
				s.logger.Debug(
					"dropping member event shadowed by a live copy in a LAN pool",
					"node", m.Name, "status", memberStatusString(m.Status), "id", m.Tags["id"])
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// memberStatusString renders a member status without panicking on the
// synthetic StatusReap value, whose String() serf does not handle.
func memberStatusString(status serf.MemberStatus) string {
	if status == StatusReap {
		return "reap"
	}
	return status.String()
}

// aliveClientIndex maps, per LAN pool ("" is the default pool), lowercased
// client names to the node IDs alive there. Server copies are excluded.
func (s *Server) aliveClientIndex() map[string]map[string]map[string]struct{} {
	out := make(map[string]map[string]map[string]struct{})
	index := func(pool string, members []serf.Member) {
		for _, m := range members {
			if m.Status != serf.StatusAlive || m.Tags["role"] == "consul" {
				continue
			}
			key := strings.ToLower(m.Name)
			byPool := out[key]
			if byPool == nil {
				byPool = make(map[string]map[string]struct{})
				out[key] = byPool
			}
			ids := byPool[pool]
			if ids == nil {
				ids = make(map[string]struct{})
				byPool[pool] = ids
			}
			ids[m.Tags["id"]] = struct{}{}
		}
	}
	// Guard against the startup window: the filter goroutine starts in
	// initEnterprise, while the segment pools are created before the default
	// LAN pool, so an event can arrive before serfLAN exists.
	if s.serfLAN != nil {
		index("", s.serfLAN.Members())
	}
	for _, name := range s.segmentNames() {
		if pool := s.segmentLan[name]; pool != nil {
			index(name, pool.Members())
		}
	}
	return out
}

// recordNodeSegment remembers, per lowercased client name, the segment of the
// pool the node was last seen alive in. It backs the reap guard's scoping for
// catalog nodes that carry no segment meta.
func (s *Server) recordNodeSegment(m serf.Member) {
	if m.Tags["role"] == "consul" || m.Status != serf.StatusAlive {
		return
	}
	s.segmentMu.Lock()
	defer s.segmentMu.Unlock()
	s.nodeSegments[strings.ToLower(m.Name)] = m.Tags["segment"]
}

// lastKnownSegment returns the segment a node was last seen alive in, for the
// reap guard's scoping when the catalog entry has no segment meta.
func (s *Server) lastKnownSegment(lowerName string) (string, bool) {
	s.segmentMu.Lock()
	defer s.segmentMu.Unlock()
	segment, ok := s.nodeSegments[lowerName]
	return segment, ok
}

// protectUnconvergedSegments adds catalog nodes that no local pool knows to
// `known` - protecting them from reconcileReaped - but only while the segment
// they belong to is unconverged on this server. Reaping such nodes is what
// makes segment clients blink on every leadership change while a pool is
// split. Nodes of converged segments and of the default pool keep the normal
// reap path, and protection for a segment ends after ReconnectTimeoutLAN of
// continuous non-convergence, so a permanent misconfiguration cannot keep
// ghosts forever.
func (s *Server) protectUnconvergedSegments(known map[string]struct{}) error {
	now := time.Now()
	lanMembers := s.serfLAN.Members()

	// seen reports, per segment this server knows about (configured pools
	// plus segments other servers advertise), whether its pool holds every
	// required server.
	seen := make(map[string]bool, len(s.segmentLan))
	for _, name := range s.segmentNames() {
		seen[name] = s.segmentPoolConverged(name, lanMembers)
	}
	for name := range advertisedSegments(s.config.Datacenter, lanMembers) {
		if _, ok := seen[name]; !ok {
			// Another server runs a segment this one has no pool for.
			seen[name] = false
		}
	}

	// Per-segment observability on every pass: one series per segment, back
	// to 0 when converged, plus an ERROR once per episode after
	// ReconnectTimeoutLAN of continuous non-convergence: the leader is then
	// long-term blind to that segment and the nodes it protects may be dead
	// without anyone noticing.
	for name, ok := range seen {
		var v float32
		if !ok {
			v = 1
		}
		metrics.SetGaugeWithLabels(
			[]string{"leader", "reconcile", "segment_unconverged"},
			v,
			[]metrics.Label{{Name: "segment", Value: name}},
		)
		if ok {
			s.clearUnseenSince(name)
			continue
		}
		age, started := s.unseenSinceAge(name, now)
		if !started {
			s.markUnseenSince(name, now)
			continue
		}
		if age > s.reapGuardTimeout() && !s.capLoggedOnce(name) {
			s.logger.Error(
				"segment pool has not converged for an extended period; "+
					"segment clients may be invisible to this leader and their health checks stale",
				"segment", name, "unconverged_for", age)
		}
	}

	// protect is the set of segments whose unknown nodes are guarded this
	// pass; the time-capped segments resume the normal reap path.
	protect := make(map[string]struct{})
	for name := range seen {
		if seen[name] {
			continue
		}
		if age, ok := s.unseenSinceAge(name, now); ok && age > s.reapGuardTimeout() {
			continue
		}
		protect[name] = struct{}{}
	}

	// reap_blocked: one series per configured segment, 1 only while nodes of
	// that segment are actually guarded.
	for _, name := range s.segmentNames() {
		var v float32
		if _, ok := protect[name]; ok {
			v = 1
		}
		metrics.SetGaugeWithLabels(
			[]string{"leader", "reconcile", "reap_blocked"},
			v,
			[]metrics.Label{{Name: "segment", Value: name}},
		)
	}

	if len(protect) == 0 {
		return nil
	}

	entMeta := structs.NodeEnterpriseMetaInDefaultPartition()
	state := s.fsm.State()
	_, checks, err := state.ChecksInState(nil, api.HealthAny, entMeta, structs.DefaultPeerKeyword)
	if err != nil {
		return err
	}

	var protected []string
	for _, check := range checks {
		if check.CheckID != structs.SerfCheckID {
			continue
		}
		nodeName := strings.ToLower(check.Node)
		if _, ok := known[nodeName]; ok {
			continue
		}

		// Scope the guard to the node's segment: the catalog meta when it
		// records one, this server's last observation otherwise. A node this
		// server cannot place keeps the normal reap path.
		segment, hasInfo := s.nodeSegmentOf(state, check.Node, nodeName, entMeta)
		if !hasInfo {
			continue
		}
		if _, guard := protect[segment]; !guard {
			continue
		}

		// Consul servers are reconciled from the default pool, which is not
		// affected by a segment split; leave them to the normal reap path.
		_, services, err := state.NodeServices(nil, check.Node, entMeta, structs.DefaultPeerKeyword)
		if err != nil {
			return err
		}
		isServer := false
		for _, svc := range services.Services {
			if svc.ID == structs.ConsulServiceID {
				isServer = true
				break
			}
		}
		if isServer {
			continue
		}

		known[nodeName] = struct{}{}
		protected = append(protected, check.Node)
	}

	if len(protected) > 0 {
		slices.Sort(protected)
		segments := slices.Sorted(maps.Keys(protect))
		s.logger.Warn(
			"segment pools have not converged; skipping the reap of nodes that no local pool can see, to keep segment clients from blinking in the catalog",
			"unconverged_segments", segments, "protected_nodes", protected)
	}
	return nil
}

// reapGuardTimeout is how long a segment may stay unconverged before the
// reap guard stops protecting its nodes and the ERROR goes out. It follows
// the LAN reconnect timeout: serf keeps a failed member that long, so a node
// invisible to this leader longer than that is a ghost, not a blink.
func (s *Server) reapGuardTimeout() time.Duration {
	if s.config.SerfLANConfig != nil && s.config.SerfLANConfig.ReconnectTimeout > 0 {
		return s.config.SerfLANConfig.ReconnectTimeout
	}
	return 72 * time.Hour
}

// unseenSinceAge returns how long the segment has been recorded as
// unconverged, and whether it is recorded at all.
func (s *Server) unseenSinceAge(name string, now time.Time) (time.Duration, bool) {
	s.guardMu.Lock()
	defer s.guardMu.Unlock()
	since, ok := s.unseenSince[name]
	if !ok {
		return 0, false
	}
	return now.Sub(since), true
}

// markUnseenSince records the segment as unconverged, unless it already is.
func (s *Server) markUnseenSince(name string, now time.Time) {
	s.guardMu.Lock()
	defer s.guardMu.Unlock()
	if _, ok := s.unseenSince[name]; !ok {
		s.unseenSince[name] = now
	}
}

// clearUnseenSince forgets the unconverged state of a converged segment.
func (s *Server) clearUnseenSince(name string) {
	s.guardMu.Lock()
	defer s.guardMu.Unlock()
	delete(s.unseenSince, name)
	delete(s.segmentCapLogged, name)
}

// capLoggedOnce reports whether the extended-outage ERROR went out for the
// current episode, and records that it has when it had not.
func (s *Server) capLoggedOnce(name string) bool {
	s.guardMu.Lock()
	defer s.guardMu.Unlock()
	if s.segmentCapLogged[name] {
		return true
	}
	s.segmentCapLogged[name] = true
	return false
}

// resetCapLogged re-arms the extended-outage ERROR for tests.
func (s *Server) resetCapLogged(name string) {
	s.guardMu.Lock()
	defer s.guardMu.Unlock()
	delete(s.segmentCapLogged, name)
}

// setUnseenSince records the segment as unconverged since t (tests).
func (s *Server) setUnseenSince(name string, since time.Time) {
	s.guardMu.Lock()
	defer s.guardMu.Unlock()
	s.unseenSince[name] = since
}

// nodeSegmentOf resolves which segment a catalog node belongs to: the catalog
// meta when it records one, this server's last observation otherwise.
func (s *Server) nodeSegmentOf(stateStore *state.Store, node, lowerName string, entMeta *acl.EnterpriseMeta) (string, bool) {
	_, n, err := stateStore.GetNode(node, entMeta, structs.DefaultPeerKeyword)
	if err == nil && n != nil {
		if segment, ok := n.Meta[structs.MetaSegmentKey]; ok {
			return segment, true
		}
	}
	return s.lastKnownSegment(lowerName)
}

// segmentPoolConverged reports whether the local pool of `segment` holds an
// alive copy of every required server: one that advertises the segment via
// its sl_ tag, or that publishes no tag and is reached through the LAN-IP
// fallback. A server whose fallback join was refused does not run the
// segment, so it is not required.
func (s *Server) segmentPoolConverged(segment string, lanMembers []serf.Member) bool {
	var refused map[string]struct{}
	if st := s.floodState[segment]; st != nil {
		refused = st.refusedNames()
	}

	alive := make(map[string]struct{})
	for _, m := range s.segmentLan[segment].Members() {
		if m.Status == serf.StatusAlive {
			alive[strings.ToLower(m.Name)] = struct{}{}
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
		if _, advertised := server.SegmentAddrs[segment]; !advertised {
			if _, skip := refused[strings.ToLower(server.Name)]; skip {
				continue
			}
		}
		if _, ok := alive[strings.ToLower(server.Name)]; !ok {
			return false
		}
	}
	return true
}

// advertisedSegments returns the segments that alive servers of the
// datacenter advertise in the default LAN pool through their sl_<name> tags.
// A segment nobody advertises has been removed from every running server.
func advertisedSegments(datacenter string, lanMembers []serf.Member) map[string]struct{} {
	segments := make(map[string]struct{})
	for _, m := range lanMembers {
		if m.Status != serf.StatusAlive {
			continue
		}
		ok, srv := metadata.IsConsulServer(m)
		if !ok || srv.Datacenter != datacenter {
			continue
		}
		for name := range srv.SegmentAddrs {
			segments[name] = struct{}{}
		}
	}
	return segments
}

func (s *Server) addEnterpriseStats(stats map[string]map[string]string) {
	// no-op
}

func getSerfMemberEnterpriseMeta(member serf.Member) *acl.EnterpriseMeta {
	return structs.NodeEnterpriseMetaInDefaultPartition()
}

func addSerfMetricsLabels(conf *serf.Config, wan bool, segment string, partition string, areaID string) {
	conf.MetricLabels = []metrics.Label{}

	networkMetric := metrics.Label{
		Name: "network",
	}
	if wan {
		networkMetric.Value = "wan"
	} else {
		networkMetric.Value = "lan"
	}
	conf.MetricLabels = append(conf.MetricLabels, networkMetric)

	segmentMetric := metrics.Label{
		Name:  "segment",
		Value: segment,
	}
	conf.MetricLabels = append(conf.MetricLabels, segmentMetric)

}

func (s *Server) updateReportingConfig(config ReloadableConfig) {
	// no-op
}

func getEnterpriseReportingDeps(deps Deps) reporting.EntDeps {
	// no-op
	return reporting.EntDeps{}
}

// CE version without LicenseManager
func (s *Server) newResourceServiceConfig(typeRegistry resource.Registry, resolver resourcegrpc.ACLResolver, tenancyBridge resourcegrpc.TenancyBridge) resourcegrpc.Config {
	return resourcegrpc.Config{
		Registry:      typeRegistry,
		Backend:       s.storageBackend,
		ACLResolver:   resolver,
		Logger:        s.loggers.Named(logging.GRPCAPI).Named(logging.Resource),
		TenancyBridge: tenancyBridge,
	}
}
