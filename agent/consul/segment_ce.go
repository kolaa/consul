// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"fmt"
	"net"
	"strings"

	"github.com/armon/go-metrics/prometheus"
)

var SegmentCESummaries = []prometheus.SummaryDefinition{
	{
		Name: []string{"leader", "reconcile"},
		Help: "Measures the time spent updating the raft store from the serf member information.",
	},
}

// LANSegmentAddr is used to return the address used for the given LAN segment.
func (s *Server) LANSegmentAddr(name string) (string, error) {
	segmentLan := s.LANSegments()[name]
	if segmentLan == nil {
		return "", fmt.Errorf("segment not found: '%s'", name)
	}
	return segmentLan.LocalMember().Addr.String(), nil
}

// setupSegmentRPC sets up separate RPC listeners for network segments.
func (s *Server) setupSegmentRPC() (map[string]net.Listener, error) {
	listeners := map[string]net.Listener{}

	for _, segment := range s.config.Segments {

		if segment.RPCAddr != nil {
			ln, err := net.ListenTCP("tcp", segment.RPCAddr)
			if err != nil {
				return nil, err
			}
			listeners[segment.Name] = ln
		}
	}

	return listeners, nil
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
			return fmt.Errorf("Failed to start LAN Serf: %v for segment %s", err, segment.Name)
		}

		s.segmentLan[segment.Name] = ln
	}

	return nil
}

// floodSegments is a NOP in the CE version of Consul.
func (s *Server) floodSegments(config *Config) {

	//for _, sc := range config.Segments {
	//	//Fire up the join flooder.
	//	addrFn := func(s *metadata.Server) (string, error) {
	//
	//		addr := s.SegmentAddrs[sc.Name]
	//		port := s.SegmentPorts[sc.Name]
	//
	//		return fmt.Sprintf("%s:%d", addr, port), nil
	//	}
	//
	//	go s.Flood(addrFn, s.segmentLan[sc.Name])
	//}
}
