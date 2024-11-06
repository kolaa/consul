// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"net"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/agent/config"
	"github.com/hashicorp/consul/agent/structs"
)

// A network segment is the same gossip pool as the default LAN, only bound to
// another port, so it must inherit the tuned gossip_lan.* settings from the
// agent's SerfLANConfig instead of starting from library defaults.
func TestSegmentConfig_InheritsTunedGossipLANSettings(t *testing.T) {
	t.Parallel()

	_, cidr, err := net.ParseCIDR("10.0.0.0/8")
	require.NoError(t, err)

	lanAddr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:8301")
	require.NoError(t, err)
	segAddr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:8303")
	require.NoError(t, err)

	rt := &config.RuntimeConfig{
		SerfBindAddrLAN:      lanAddr,
		SerfAdvertiseAddrLAN: lanAddr,
		RPCBindAddr:          lanAddr,
		Segments: []structs.NetworkSegment{{
			Name:      "alpha",
			Bind:      segAddr,
			Advertise: segAddr,
		}},

		GossipLANGossipInterval: 51 * time.Millisecond,
		GossipLANGossipNodes:    4,
		GossipLANProbeInterval:  62 * time.Millisecond,
		GossipLANProbeTimeout:   73 * time.Millisecond,
		GossipLANSuspicionMult:  8,
		GossipLANRetransmitMult: 9,
		ReconnectTimeoutLAN:     42 * time.Second,
		SerfAllowedCIDRsLAN:     []net.IPNet{*cidr},
		StaticRuntimeConfig: config.StaticRuntimeConfig{
			EncryptVerifyIncoming: true,
			EncryptVerifyOutgoing: true,
		},
		DisableKeyringFile: true,
	}

	cfg, err := newConsulConfig(rt, hclog.NewNullLogger())
	require.NoError(t, err)
	require.Len(t, cfg.Segments, 1)

	seg := cfg.Segments[0].SerfConfig
	require.NotNil(t, seg)

	// The gossip_lan.* tuning must reach the segment's serf config...
	require.Equal(t, rt.GossipLANGossipInterval, seg.MemberlistConfig.GossipInterval,
		"gossip_lan.gossip_interval must be inherited from SerfLANConfig")
	require.Equal(t, rt.GossipLANGossipNodes, seg.MemberlistConfig.GossipNodes,
		"gossip_lan.gossip_nodes must be inherited from SerfLANConfig")
	require.Equal(t, rt.GossipLANProbeInterval, seg.MemberlistConfig.ProbeInterval,
		"gossip_lan.probe_interval must be inherited from SerfLANConfig")
	require.Equal(t, rt.GossipLANProbeTimeout, seg.MemberlistConfig.ProbeTimeout,
		"gossip_lan.probe_timeout must be inherited from SerfLANConfig")
	require.Equal(t, rt.GossipLANSuspicionMult, seg.MemberlistConfig.SuspicionMult,
		"gossip_lan.suspicion_mult must be inherited from SerfLANConfig")
	require.Equal(t, rt.GossipLANRetransmitMult, seg.MemberlistConfig.RetransmitMult,
		"gossip_lan.retransmit_mult must be inherited from SerfLANConfig")
	require.Equal(t, rt.ReconnectTimeoutLAN, seg.ReconnectTimeout,
		"reconnect_timeout must be inherited from SerfLANConfig")
	require.Equal(t, rt.SerfAllowedCIDRsLAN, seg.MemberlistConfig.CIDRsAllowed,
		"lan_allowed_cidrs must be inherited from SerfLANConfig")
	require.True(t, seg.MemberlistConfig.GossipVerifyIncoming)
	require.True(t, seg.MemberlistConfig.GossipVerifyOutgoing)

	// ...while the segment keeps its own bind address and stays a clone of the
	// LAN config, not an alias of it.
	require.Equal(t, "127.0.0.1", seg.MemberlistConfig.BindAddr)
	require.Equal(t, 8303, seg.MemberlistConfig.BindPort)
	require.NotSame(t, cfg.SerfLANConfig, seg)
	require.NotSame(t, cfg.SerfLANConfig.MemberlistConfig, seg.MemberlistConfig)
}
