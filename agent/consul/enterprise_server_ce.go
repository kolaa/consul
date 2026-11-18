// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"context"
	"errors"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/memberlist"
	"github.com/hashicorp/serf/serf"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/pool"
	"github.com/hashicorp/consul/agent/structs"
)

var (
	// minMultiDCConnectVersion is the minimum version in order to support multi-DC Connect
	// features.
	minMultiDCConnectVersion = version.Must(version.NewVersion("1.6.0"))
)

type EnterpriseServer struct {
	segmentLan       map[string]*serf.Serf
	segmentListeners map[string]net.Listener

	// floodState carries the per-segment flood retry state and the set of
	// servers whose fallback join was refused (they do not run the segment).
	// One per configured segment; created in initEnterprise.
	floodState map[string]*segmentFloodState

	// poolEventCh receives the serf events of every LAN pool when segments
	// are configured; poolEventFilter forwards them to eventChLAN, dropping
	// events that a cross-pool view proves stale. Without segments the pools
	// deliver to eventChLAN directly and the filter sits idle.
	poolEventCh chan serf.Event

	// nodeSegments records, per lowercased client name, the segment of the
	// pool the node was last seen alive in. It backs the reap guard's
	// per-segment scoping for nodes whose catalog entry carries no segment
	// meta. Guarded by segmentMu; written from the event filter goroutine
	// and the reconcile loop.
	nodeSegments map[string]string
	segmentMu    sync.Mutex

	// unseenSince records since when a segment has been continuously
	// unconverged on this server, and segmentCapLogged whether the
	// extended-outage ERROR for the current episode went out. Reconcile runs
	// on the leader loop AND directly in tests, so both maps are mutex
	// guarded.
	guardMu          sync.Mutex
	unseenSince      map[string]time.Time
	segmentCapLogged map[string]bool
}

func (s *Server) initEnterprise(_ Deps) error {

	s.segmentLan = make(map[string]*serf.Serf, len(s.config.Segments))
	s.segmentListeners = make(map[string]net.Listener, len(s.config.Segments))
	s.floodState = make(map[string]*segmentFloodState, len(s.config.Segments))
	for _, segment := range s.config.Segments {
		s.floodState[segment.Name] = newSegmentFloodState()
	}
	s.poolEventCh = make(chan serf.Event, serfEventChSize)
	s.nodeSegments = make(map[string]string)
	s.unseenSince = make(map[string]time.Time)
	s.segmentCapLogged = make(map[string]bool)

	// The filter goroutine that forwards poolEventCh to eventChLAN starts in
	// floodSegments, after every pool exists: it reads s.segmentLan, which
	// setupSegments is still writing while this function runs.

	// Publish each segment's dialable gossip address as an sl_<name> tag on
	// the default LAN pool - but only when the segment is NOT reachable at
	// the LAN advertise IP. Every published tag spends serf's 512-byte tag
	// budget, and the built-in tags already leave room for only a handful of
	// them; a segment on the same IP as the LAN pool is reachable through
	// the fallback address (LAN IP + segment port), so omitting its tag
	// keeps the budget for the segments that need it. Omission is also safe
	// in mixed-version clusters: an unpatched flooder skips tag-less
	// servers, and a patched one falls back to the LAN address, so one side
	// always joins.
	if len(s.config.Segments) > 0 && s.config.SerfLANConfig != nil {
		lanIP := s.config.SerfLANConfig.MemberlistConfig.AdvertiseAddr
		if lanIP == "" {
			lanIP = s.config.SerfLANConfig.MemberlistConfig.BindAddr
		}
		for _, segment := range s.config.Segments {
			host, port := segmentAdvertiseHostPort(segment)
			if host == "" || host == lanIP {
				continue
			}
			if s.config.SerfLANConfig.Tags == nil {
				s.config.SerfLANConfig.Tags = make(map[string]string, len(s.config.Segments))
			}
			s.config.SerfLANConfig.Tags["sl_"+segment.Name] = net.JoinHostPort(host, strconv.Itoa(port))
		}
	}

	return nil
}

// segmentAdvertiseHostPort returns the address a segment's gossip pool
// advertises: the explicit advertise address when set, the bind address
// otherwise.
func segmentAdvertiseHostPort(segment NetworkSegment) (string, int) {
	if segment.SerfConfig == nil {
		return "", 0
	}
	mc := segment.SerfConfig.MemberlistConfig
	host := mc.AdvertiseAddr
	if host == "" {
		host = mc.BindAddr
	}
	port := mc.AdvertisePort
	if port <= 0 {
		port = mc.BindPort
	}
	return host, port
}

func (s *Server) startEnterprise() error {
	return nil
}

func (s *Server) handleEnterpriseUserEvents(event serf.UserEvent) bool {
	return false
}

func (s *Server) handleEnterpriseRPCConn(rtype pool.RPCType, conn net.Conn, isTLS bool) bool {
	return false
}

func (s *Server) handleEnterpriseNativeTLSConn(alpnProto string, conn net.Conn) bool {
	return false
}

func (s *Server) handleEnterpriseLeave() {
	// serf.Leave blocks for roughly BroadcastTimeout + LeavePropagateDelay;
	// leaving N pools sequentially can blow through systemd TimeoutStopSec or
	// a Kubernetes terminationGracePeriodSeconds, so leave them in parallel.
	var wg sync.WaitGroup
	for _, name := range s.segmentNames() {
		pool := s.segmentLan[name]
		wg.Go(func() {
			if err := pool.Leave(); err != nil {
				s.logger.Error("failed to leave segment LAN pool", "segment", name, "error", err)
			}
		})
	}
	wg.Wait()
}

func (s *Server) establishEnterpriseLeadership(_ context.Context) error {
	return nil
}

func (s *Server) revokeEnterpriseLeadership() error {
	return nil
}

func (s *Server) startTenancyDeferredDeletion(ctx context.Context) {
}

func (s *Server) stopTenancyDeferredDeletion() {
}

func (s *Server) validateEnterpriseRequest(entMeta *acl.EnterpriseMeta, write bool) error {
	return nil
}

func (s *Server) validateEnterpriseIntentionPartition(partition string) error {
	if partition == "" {
		return nil
	} else if strings.ToLower(partition) == "default" {
		return nil
	}

	// No special handling for wildcard partitions as they are pointless in CE.

	return errors.New("Partitions is a Consul Enterprise feature")
}

func (s *Server) validateEnterpriseIntentionNamespace(ns string, _ bool) error {
	if ns == "" {
		return nil
	} else if strings.ToLower(ns) == structs.IntentionDefaultNamespace {
		return nil
	}

	// No special handling for wildcard namespaces as they are pointless in CE.

	return errors.New("Namespaces is a Consul Enterprise feature")
}

// lanSegments returns a map of LAN segments by name
func (s *Server) lanSegments() map[string]*serf.Serf {
	return s.segmentLan
}

// segmentNames returns the segment pool names in sorted order. Map iteration
// order is random, so keyring responses, coordinate sets and logs would come
// out in a different order on every call without it.
func (s *Server) segmentNames() []string {
	return slices.Sorted(maps.Keys(s.segmentLan))
}

// setupSerfLAN is used to setup and initialize a Serf for the LAN
func (s *Server) setupSerfLAN(config *Config) error {
	var err error

	// With segments configured, the default pool delivers into poolEventCh
	// and poolEventFilter forwards to eventChLAN; without them the filter
	// would add nothing, so deliver directly.
	eventCh := s.eventChLAN
	if len(s.config.Segments) > 0 {
		eventCh = s.poolEventCh
	}

	// Initialize the LAN Serf for the default network segment.
	s.serfLAN, _, err = s.setupSerf(setupSerfOptions{
		Config:       config.SerfLANConfig,
		EventCh:      eventCh,
		SnapshotPath: serfLANSnapshot,
		Listener:     s.Listener,
		WAN:          false,
		Segment:      "",
		Partition:    "",
	})
	if err != nil {
		return err
	}
	return nil
}

const (
	// SerfTagLimit is the most bytes serf accepts for a member's encoded tags;
	// serf.Create and SetTags refuse anything larger.
	SerfTagLimit = memberlist.MetaMaxSize

	// SerfTagHeadroom is kept free below SerfTagLimit so a future Consul
	// release can add built-in tags without every server refusing to start.
	SerfTagHeadroom = 64

	// SerfBaseTagReserve is the room a server's default LAN pool tags other
	// than sl_ and the datacenter name take up; TestServer_BaseLANTags_FitReserve
	// keeps it honest.
	SerfBaseTagReserve = 352
)

// SegmentTagSize is the number of bytes one sl_<name>=host:port tag adds to
// the server's encoded default LAN pool tags.
func SegmentTagSize(name, host string, port int) int {
	key := len("sl_") + len(name)
	value := len(net.JoinHostPort(host, strconv.Itoa(port)))
	return msgpackStrSize(key) + msgpackStrSize(value)
}

// msgpackStrSize is the encoded size of an n-byte string in the msgpack
// dialect serf uses (raw headers, no str8).
func msgpackStrSize(n int) int {
	switch {
	case n < 32:
		return 1 + n
	case n < 1<<16:
		return 3 + n
	default:
		return 5 + n
	}
}

func (s *Server) shutdownSerfLAN() {
	if s.serfLAN != nil {
		s.serfLAN.Shutdown()
	}
	// Segment pools and their RPC listeners must go down too, otherwise an
	// in-process restart or a test cleanup hits "address already in use".
	for _, name := range s.segmentNames() {
		s.segmentLan[name].Shutdown()
	}
	for _, ln := range s.segmentListeners {
		_ = ln.Close()
	}
}

func addEnterpriseSerfTags(_ map[string]string, _ *acl.EnterpriseMeta) {
	// do nothing
}
