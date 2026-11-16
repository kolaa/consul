// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/hashicorp/go-version"
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

	// tagSizeErr records a clear error when the serf tags assembled for the
	// default LAN pool exceed memberlist's encoded-tag budget, so that
	// setupSerfLAN can fail startup with the segment names instead of the
	// opaque error serf.Create would return.
	tagSizeErr error
}

func (s *Server) initEnterprise(_ Deps) error {

	s.segmentLan = make(map[string]*serf.Serf, len(s.config.Segments))
	s.segmentListeners = make(map[string]net.Listener, len(s.config.Segments))

	// Publish each segment's dialable gossip address as an sl_<name> tag on
	// the default LAN pool. Peers build their flood-join address from these
	// tags, so a segment that lives on its own NIC/VLAN converges even though
	// its address differs from the LAN address. Servers without the tag (for
	// example while a new segment is being rolled out) are skipped by the
	// flooder instead of being dialed at a guessed address.
	if len(s.config.Segments) > 0 && s.config.SerfLANConfig != nil {
		if s.config.SerfLANConfig.Tags == nil {
			s.config.SerfLANConfig.Tags = make(map[string]string, len(s.config.Segments))
		}
		for _, segment := range s.config.Segments {
			if segment.SerfConfig == nil {
				continue
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
			s.config.SerfLANConfig.Tags["sl_"+segment.Name] = net.JoinHostPort(host, strconv.Itoa(port))
		}
	}

	// Measure the fully assembled tag set of the default LAN pool (this hook
	// runs at the end of setupSerfConfig, after every built-in tag and the
	// published sl_<segment> tags are in place) and remember a clear error if
	// it does not fit memberlist's budget. Segment pools publish no sl_ tags,
	// and the WAN pool cannot, so only the default pool is measured.
	prev := s.config.OverrideInitialSerfTags
	s.config.OverrideInitialSerfTags = func(tags map[string]string) {
		if prev != nil {
			prev(tags)
		}
		if tags["segment"] != "" {
			return
		}
		if size := encodedSerfTagsSize(tags); size > serfTagsBudget {
			s.tagSizeErr = fmt.Errorf(
				"serf tags for the default LAN pool encode to %d bytes, over the %d-byte limit: "+
					"the sl_<segment> tags published for network segments leave too little room; "+
					"shorten segment names/advertise addresses or reduce the segment count",
				size, serfTagsBudget)
		}
	}

	return nil
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

	// Initialize the LAN Serf for the default network segment.
	s.serfLAN, _, err = s.setupSerf(setupSerfOptions{
		Config:       config.SerfLANConfig,
		EventCh:      s.eventChLAN,
		SnapshotPath: serfLANSnapshot,
		Listener:     s.Listener,
		WAN:          false,
		Segment:      "",
		Partition:    "",
	})
	if err != nil {
		// serf.Create rejects an oversized tag set with a generic message;
		// the tag budget check in initEnterprise names the actual cause.
		if s.tagSizeErr != nil {
			return s.tagSizeErr
		}
		return err
	}
	if s.tagSizeErr != nil {
		return s.tagSizeErr
	}
	return nil
}

// serfTagsBudget mirrors memberlist.MetaMaxSize: the encoded size limit for
// the tag set carried in a serf member's metadata.
const serfTagsBudget = 512

// encodedSerfTagsSize replicates serf's encodeTags so the CE tag budget
// check measures exactly what serf.Create will.
func encodedSerfTagsSize(tags map[string]string) int {
	var buf bytes.Buffer
	buf.WriteByte(255) // serf tagMagicByte
	enc := codec.NewEncoder(&buf, &codec.MsgpackHandle{})
	if err := enc.Encode(tags); err != nil {
		return len(buf.Bytes()) + 1 // unreachable for a string map; treat as over budget
	}
	return buf.Len()
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
