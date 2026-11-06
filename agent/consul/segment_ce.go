// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/hashicorp/go-metrics/prometheus"

	"github.com/hashicorp/consul/agent/metadata"
)

var SegmentCESummaries = []prometheus.SummaryDefinition{
	{
		Name: []string{"leader", "reconcile"},
		Help: "Measures the time spent updating the raft store from the serf member information.",
	},
}

// LANSegmentAddr is used to return the address used for the given LAN segment.
func (s *Server) LANSegmentAddr(name string) string {
	segmentLan := s.LANSegments()[name]
	if segmentLan == nil {
		return ""
	}
	return segmentLan.LocalMember().Addr.String()
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

// floodSegments starts one flooder per segment pool. Every flooder keeps the
// servers known from the default LAN pool joined into its segment pool, so
// segment pools converge across servers no matter which shape retry_join
// took: a bare address, an explicit host:port, or none at all.
//
// This is variant B of P0-1: no sl_* tags are published, so nothing here is
// bounded by serf's 512-byte tag budget and any number of segments works.
// The price is the same assumption the bare-address expansion in JoinLAN
// already made: the segment must listen on the same port on every server and
// be reachable on the server's LAN address.
func (s *Server) floodSegments(config *Config) {
	for _, sc := range config.Segments {
		// sc is per-iteration (go 1.22+, see go.mod), so the closure may
		// capture it directly.
		addrFn := func(srv *metadata.Server) (string, error) {
			return segmentFloodAddr(srv, sc.Port)
		}
		go s.Flood(addrFn, s.segmentLan[sc.Name])
	}
}

// segmentFloodAddr returns the address of the given server's copy of a
// segment pool: the host the server is reachable on (its gossip address,
// carried in the RPC address) plus the segment's port. The RPC port itself
// is deliberately discarded.
func segmentFloodAddr(srv *metadata.Server, port int) (string, error) {
	if srv.Addr == nil {
		return "", fmt.Errorf("server %s has no address", srv.Name)
	}
	host, _, err := net.SplitHostPort(srv.Addr.String())
	if err != nil {
		return "", fmt.Errorf("server %s: cannot derive segment address: %w", srv.Name, err)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
