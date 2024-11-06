// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package agent

import (
	"context"
	"fmt"

	lru "github.com/hashicorp/golang-lru"
	"github.com/hashicorp/serf/serf"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/config"
	"github.com/hashicorp/consul/agent/consul"
	"github.com/hashicorp/consul/agent/proxycfg"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/api"
)

// enterpriseAgent embeds fields that we only access in consul-enterprise builds
type enterpriseAgent struct{}

// fillAgentServiceEnterpriseMeta is a noop stub for the func defined agent_ent.go
func fillAgentServiceEnterpriseMeta(_ *api.AgentService, _ *acl.EnterpriseMeta) {}

// fillHealthCheckEnterpriseMeta is a noop stub for the func defined agent_ent.go
func fillHealthCheckEnterpriseMeta(_ *api.HealthCheck, _ *acl.EnterpriseMeta) {}

// initEnterprise is the community edition counterpart of the func defined in
// agent_ent.go.
//
// It runs in Agent.Start after agent.go installs UserEventHandler and before
// the server or client is created, which is where the community edition can
// adjust agent state that agent.go declares no field for. Here the handler is
// wrapped to drop repeated deliveries of a fanned-out user event: a server
// belongs to the default LAN pool plus every segment pool, and
// LANSendUserEvent broadcasts the same payload bytes to all of them over one
// shared channel, so without dedup the event would be ingested once per pool.
// Keying on the payload bytes is enough — every pool carries the identical
// []byte — and spares a second msgpack decode. The window grows with the pool
// count, so a burst of events across N+1 pools cannot evict an event's first
// copy before its last one arrives.
func (a *Agent) initEnterprise(consulCfg *consul.Config) error {
	if consulCfg.UserEventHandler == nil {
		return nil
	}
	seen, err := lru.New(userEventDedupPerPool * (1 + len(consulCfg.Segments)))
	if err != nil {
		return fmt.Errorf("failed to create user event dedup: %w", err)
	}
	inner := consulCfg.UserEventHandler
	consulCfg.UserEventHandler = func(e serf.UserEvent) {
		if len(e.Payload) > 0 {
			if dup, _ := seen.ContainsOrAdd(string(e.Payload), struct{}{}); dup {
				return
			}
		}
		inner(e)
	}
	return nil
}

// userEventDedupPerPool is how many recent user events the dedup remembers
// for each LAN pool the agent belongs to.
const userEventDedupPerPool = 256

// reloadEnterprise is a noop stub for the func defined agent_ent.go
func (a *Agent) reloadEnterprise(conf *config.RuntimeConfig) error {
	return nil
}

// enterpriseConsulConfig applies the community edition adjustments to the
// consul config at the end of newConsulConfig.
//
// agent.segmentConfig() builds segment pools from library defaults plus a few
// LAN settings (CIDRs, reconnect timeout, verify flags), because agent.go has
// no enterprise hook at the point where it runs. Now that
// newConsulConfig has applied every gossip_lan.* tuning knob to
// cfg.SerfLANConfig and setupKeyrings has handed each segment its own keyring,
// re-clone each segment pool from the final LAN config so the tuned settings
// (intervals, probes, CIDRs, verify flags, reconnect timeout) reach every
// segment, not just the default pool.
func enterpriseConsulConfig(cfg *consul.Config, _ *config.RuntimeConfig) {
	if cfg.SerfLANConfig == nil {
		return
	}

	for i := range cfg.Segments {
		segment := &cfg.Segments[i]
		if segment.SerfConfig == nil {
			continue
		}

		// Only the segment specific values survive from the config that
		// segmentConfig built: the bind/advertise addresses it was given, the
		// keyring copy setupKeyrings created for it (pools must not share one
		// keyring object) and the fact that segments have no keyring file.
		previous := segment.SerfConfig
		serfConf := consul.CloneSerfLANConfig(cfg.SerfLANConfig)
		serfConf.MemberlistConfig.BindAddr = previous.MemberlistConfig.BindAddr
		serfConf.MemberlistConfig.BindPort = previous.MemberlistConfig.BindPort
		serfConf.MemberlistConfig.AdvertiseAddr = previous.MemberlistConfig.AdvertiseAddr
		serfConf.MemberlistConfig.AdvertisePort = previous.MemberlistConfig.AdvertisePort
		serfConf.MemberlistConfig.Keyring = previous.MemberlistConfig.Keyring
		serfConf.KeyringFile = previous.KeyringFile

		segment.SerfConfig = serfConf
	}
}

// validateFIPSConfig is a noop stub for the func defined in agent_ent.go
func validateFIPSConfig(_ *config.RuntimeConfig) error {
	return nil
}

// WriteEvent is a noop stub for the func defined agent_ent.go
func (a *Agent) WriteEvent(eventType string, payload interface{}) {
}

// startLicenseManager is used to start the license management process
func (a *Agent) startLicenseManager(_ context.Context) error {
	return nil
}

// stopLicenseManager is used to stop the license management go routines
func (a *Agent) stopLicenseManager() {}

// enterpriseStats outputs all the Agent stats specific to Consul Enterprise
func (a *Agent) enterpriseStats() map[string]map[string]string {
	return nil
}

func (a *Agent) AgentEnterpriseMeta() *acl.EnterpriseMeta {
	return structs.NodeEnterpriseMetaInDefaultPartition()
}

func (a *Agent) registerEntCache() {}

func (*Agent) fillEnterpriseProxyDataSources(*proxycfg.DataSources) {}

func (a *Agent) writeAuditRPCEvent(_ string, _ string) interface{} {
	return nil
}

func validateEnterpriseMeshPortConfig(service *structs.NodeService) error {
	if service == nil {
		return nil
	}

	if len(service.Ports) > 0 {
		if service.LocallyRegisteredAsSidecar || service.Kind == structs.ServiceKindConnectProxy || service.Connect.SidecarService != nil {
			return fmt.Errorf("named service ports in the service mesh require Consul Enterprise")
		}
	}

	if service.Kind == structs.ServiceKindConnectProxy {
		if len(service.Proxy.LocalServicePorts) > 0 {
			return fmt.Errorf("named service ports in the service mesh require Consul Enterprise")
		}
		for _, upstream := range service.Proxy.Upstreams {
			if upstream.DestinationPort != "" {
				return fmt.Errorf("destination port routing requires Consul Enterprise")
			}
		}
	}

	return nil
}

func copyEnterpriseSidecarServicePorts(_, _ *structs.NodeService) {}
