// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package config

import (
	"fmt"
	"net"
	"strconv"

	"github.com/hashicorp/consul/agent/consul"
)

// validateEnterpriseConfig is a function to validate the enterprise specific
// configuration items after Parsing but before merging into the overall
// configuration. The original intent is to use it to ensure that we warn
// for enterprise configurations used in CE.
func validateEnterpriseConfigKeys(config *Config) []error {
	var result []error
	add := func(k string) {
		result = append(result, enterpriseConfigKeyError{key: k})
	}

	if config.ReadReplica != nil {
		add(`read_replica (or the deprecated non_voting_server)`)
	}
	if stringVal(config.Partition) != "" {
		add("partition")
	}
	if stringVal(config.Autopilot.RedundancyZoneTag) != "" {
		add("autopilot.redundancy_zone_tag")
	}
	if stringVal(config.Autopilot.UpgradeVersionTag) != "" {
		add("autopilot.upgrade_version_tag")
	}
	if config.Autopilot.DisableUpgradeMigration != nil {
		add("autopilot.disable_upgrade_migration")
	}
	if config.DNS.PreferNamespace != nil {
		add("dns_config.prefer_namespace")
		config.DNS.PreferNamespace = nil
	}
	if config.ACL.MSPDisableBootstrap != nil {
		add("acl.msp_disable_bootstrap")
		config.ACL.MSPDisableBootstrap = nil
	}
	if len(config.ACL.Tokens.ManagedServiceProvider) > 0 {
		add("acl.tokens.managed_service_provider")
		config.ACL.Tokens.ManagedServiceProvider = nil
	}
	if boolVal(config.Audit.Enabled) || len(config.Audit.Sinks) > 0 {
		add("audit")
	}
	if config.LicensePath != nil {
		add("license_path")
		config.LicensePath = nil
	}
	if config.Reporting.License.Enabled != nil {
		add("reporting.license.enabled")
		config.Reporting.License.Enabled = nil
	}

	return result
}

type enterpriseConfigKeyError struct {
	key string
}

func (e enterpriseConfigKeyError) Error() string {
	return fmt.Sprintf("%q is a Consul Enterprise configuration and will have no effect", e.key)
}

func (*builder) BuildEnterpriseRuntimeConfig(_ *RuntimeConfig, _ *Config) error {
	return nil
}

// defaultSerfLANPort is the default port of the LAN gossip pool
// (ports.serf_lan), where memberlist resolves retry_join addresses that carry
// no explicit port.
const defaultSerfLANPort = consul.DefaultLANSerfPort

// validateEnterpriseConfig checks that a client in a network segment can join
// its segment's serf pool. A setup that can never join is an error; one that
// likely cannot join, but may be valid, is a warning.
func (b *builder) validateEnterpriseConfig(rt RuntimeConfig) error {
	if rt.ServerMode || rt.SegmentName == "" {
		return nil
	}
	// Clients have no flooder and no other discovery, so without retry_join
	// the client only joins if an operator runs `consul join` by hand.
	if len(rt.RetryJoinLAN) == 0 {
		b.warn("segment = %q is set but retry_join is empty, so this client will not join any "+
			"segment pool: add retry_join with host:<segment port>, or a bare host with "+
			"ports.serf_lan set to the segment port", rt.SegmentName)
		return nil
	}
	// An explicit default serf_lan port reaches the servers' default LAN pool,
	// whatever the local ports.serf_lan is, and that pool rejects segment
	// members.
	for _, addr := range rt.RetryJoinLAN {
		if _, port, err := net.SplitHostPort(addr); err == nil && port == strconv.Itoa(defaultSerfLANPort) {
			b.warn("retry_join %q targets the default serf_lan port %d while segment = %q, so the "+
				"servers' default LAN pool will reject this client: use host:<segment port>",
				addr, defaultSerfLANPort, rt.SegmentName)
		}
	}
	// Documented pattern: ports.serf_lan = <segment port> + bare retry_join.
	// memberlist then resolves the port-less address against the overridden
	// serf_lan port, which is the segment's port, so the join lands in the
	// right pool.
	if rt.SerfPortLAN != defaultSerfLANPort {
		return nil
	}
	// A client in a network segment gossips only with that segment's serf pool
	// on the servers. With the default serf_lan port, memberlist resolves a
	// retry_join address that carries no explicit port against the default LAN
	// pool instead, so the join is rejected as a wrong-segment merge. A literal
	// address is an error. Cloud auto-join (provider=...) is only a warning:
	// most providers return bare IPs, but some (k8s with a port annotation)
	// return ip:port, which joins the segment pool correctly.
	var cloudAutoJoin bool
	for _, addr := range rt.RetryJoinLAN {
		if consul.IsCloudAutoJoin(addr) {
			cloudAutoJoin = true
			continue
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("retry_join %q has no port while segment = %q and ports.serf_lan "+
				"is the default %d: use host:<segment port> or set ports.serf_lan to the segment port",
				addr, rt.SegmentName, defaultSerfLANPort)
		}
	}
	if cloudAutoJoin {
		b.warn("segment = %q uses cloud auto-join with the default ports.serf_lan %d: unless the "+
			"provider returns ip:<segment port>, the join reaches the default LAN pool and is "+
			"rejected; set ports.serf_lan to the segment port", rt.SegmentName, defaultSerfLANPort)
	}

	return nil
}
