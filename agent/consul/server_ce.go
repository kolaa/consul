// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/consul/agent/structs"

	"github.com/armon/go-metrics"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/serf/coordinate"
	"github.com/hashicorp/serf/serf"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/consul/reporting"
	resourcegrpc "github.com/hashicorp/consul/agent/grpc-external/services/resource"
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
// should be another node inside the DC listening on the Serf LAN address
func (s *Server) JoinLAN(addrs []string, entMeta *acl.EnterpriseMeta) (int, error) {
	// retry_join is shared by the default LAN pool and every segment pool.
	// memberlist resolves addresses that carry no explicit port against the
	// *local* bind port of the pool doing the join, so a bare address would
	// otherwise only ever reach the default LAN pool (and, when used from a
	// segment pool, would silently target the wrong remote pool). Expand the
	// addresses per pool so a single retry_join entry reaches all of them.
	lanAddrs, segmentAddrs := segmentJoinAddrs(addrs, s.config.Segments)

	var merr error
	total := 0

	if len(lanAddrs) > 0 {
		n, err := s.serfLAN.Join(lanAddrs, true)
		total += n
		if err != nil {
			merr = multierror.Append(merr, err)
		}
	}

	for name, segAddrs := range segmentAddrs {
		pool := s.segmentLan[name]
		if pool == nil {
			merr = multierror.Append(merr, fmt.Errorf("cannot join unknown network segment '%s'", name))
			continue
		}
		n, err := pool.Join(segAddrs, true)
		total += n
		if err != nil {
			merr = multierror.Append(merr, fmt.Errorf("failed to join segment '%s': %w", name, err))
		}
	}

	// Whatever we just pulled in may include a copy of this very agent under
	// the name it used before a rename. Clean those up right away: otherwise
	// the old name stays visible in "consul members" until serf reaps it,
	// which can take days and keeps the router pointing at a node that no
	// longer exists.
	s.pruneStaleSelf()

	if merr != nil {
		return total, merr
	}
	return total, nil
}

// pruneStaleSelf drops records that carry this agent's node ID under a name
// this agent no longer uses from every LAN pool.
func (s *Server) pruneStaleSelf() {
	logger := s.logger
	nodeID := s.config.NodeID
	nodeName := s.config.NodeName

	pruneStaleSelfRecords(s.serfLAN, nodeID, nodeName, logger)
	for name, pool := range s.segmentLan {
		pruneStaleSelfRecords(pool, nodeID, nodeName, logger.With("segment", name))
	}
}

// segmentJoinAddrs splits join addresses into the addresses that belong to
// the default serf LAN pool and, per segment name, the addresses that belong
// to that segment's pool.
//
//   - An address without an explicit port is joined into the default pool
//     unchanged (memberlist resolves it against the local serf LAN bind port)
//     and additionally into every configured segment pool using that segment's
//     port.
//   - An address with an explicit port is joined only into the pool that
//     listens on that port: a configured segment if the port matches one, and
//     the default LAN pool otherwise.
//
// This assumes the (usual) deployment where every server in the datacenter is
// configured with the same segment names and ports.
func segmentJoinAddrs(addrs []string, segments []NetworkSegment) ([]string, map[string][]string) {
	segmentByPort := make(map[string]string, len(segments))
	for _, segment := range segments {
		if segment.Port == 0 {
			continue
		}
		segmentByPort[strconv.Itoa(segment.Port)] = segment.Name
	}

	lanAddrs := make([]string, 0, len(addrs))
	segmentAddrs := make(map[string][]string)

	for _, addr := range addrs {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
			port = ""
		}

		if port == "" {
			lanAddrs = append(lanAddrs, host)
			for _, segment := range segments {
				if segment.Port == 0 {
					continue
				}
				segmentAddrs[segment.Name] = append(
					segmentAddrs[segment.Name],
					net.JoinHostPort(host, strconv.Itoa(segment.Port)))
			}
			continue
		}

		if name, ok := segmentByPort[port]; ok {
			segmentAddrs[name] = append(segmentAddrs[name], addr)
			continue
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

		for segment, segmentLan := range s.segmentLan {
			if found, err := maybeRemove(segmentLan, node); err != nil {
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

	for _, lan := range s.segmentLan {
		for _, member := range lan.Members() {
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
		return s.LANSegments()[filter.Segment].Members(), nil
	}
	return s.LANMembersInAgentPartition(), nil
}

func (s *Server) GetMatchingLANCoordinate(_, segment string) (*coordinate.Coordinate, error) {

	if segment == "" || s.config.Segment == segment {
		return s.serfLAN.GetCoordinate()
	}

	segmentLan := s.segmentLan[segment]
	if segmentLan == nil {
		return nil, fmt.Errorf("segment not found: '%s'", segment)
	}

	return s.segmentLan[segment].GetCoordinate()
}

func (s *Server) addEnterpriseLANCoordinates(cs librtt.CoordinateSet) error {

	for name, segment := range s.segmentLan {
		c, err := segment.GetCoordinate()
		if err != nil {
			return err
		}
		cs[name] = c
	}

	return nil
}

func (s *Server) LANSendUserEvent(name string, payload []byte, coalesce bool) error {
	err := s.serfLAN.UserEvent(name, payload, coalesce)
	if err != nil {
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

	for name, segmentSerf := range s.segmentLan {
		err := fn(name, PoolKindSegment, segmentSerf)
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

	members := s.serfLAN.Members()
	knownMembers := make(map[string]struct{})
	for _, member := range members {
		if err := s.reconcileMember(member); err != nil {
			return err
		}
		knownMembers[strings.ToLower(member.Name)] = struct{}{}
	}

	for _, segment := range s.segmentLan {
		members = segment.Members()

		for _, member := range members {
			if err := s.reconcileMember(member); err != nil {
				return err
			}

			if _, ok := knownMembers[strings.ToLower(member.Name)]; !ok {
				knownMembers[strings.ToLower(member.Name)] = struct{}{}
			}
		}
	}

	// Reconcile any members that have been reaped while we were not the
	// leader.
	return s.reconcileReaped(knownMembers, nil)
}

func (s *Server) addEnterpriseStats(stats map[string]map[string]string) {
	// no-op
}

func getSerfMemberEnterpriseMeta(member serf.Member) *acl.EnterpriseMeta {
	return structs.NodeEnterpriseMetaInDefaultPartition()
}

func addSerfMetricsLabels(conf *serf.Config, wan bool, segment string, partition string) {
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
