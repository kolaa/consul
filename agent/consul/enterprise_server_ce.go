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
	"strings"
	"sync"

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
}

func (s *Server) initEnterprise(_ Deps) error {

	s.segmentLan = make(map[string]*serf.Serf, len(s.config.Segments))
	s.segmentListeners = make(map[string]net.Listener, len(s.config.Segments))

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

// LANSegments returns a map of LAN segments by name
func (s *Server) LANSegments() map[string]*serf.Serf {
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
		return err
	}
	return nil
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
