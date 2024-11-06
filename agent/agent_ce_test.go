// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package agent

import (
	"testing"

	"github.com/hashicorp/consul/agent/config"
	"github.com/hashicorp/consul/agent/consul"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/serf/serf"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/agent/structs"
)

func TestAgent_consulConfig_Reporting(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()
	hcl := `
		reporting {
			license {
				enabled = true
			}
		}
	`
	a := NewTestAgent(t, hcl)
	defer a.Shutdown()
	require.Equal(t, false, a.consulConfig().Reporting.License.Enabled)
}

func TestAgent_consulConfig_Reporting_Default(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()
	hcl := `
		reporting {
		}
	`
	a := NewTestAgent(t, hcl)
	defer a.Shutdown()
	require.Equal(t, false, a.consulConfig().Reporting.License.Enabled)
}

func TestValidateEnterpriseMeshPortConfig(t *testing.T) {
	t.Parallel()

	t.Run("plain multiport service stays valid", func(t *testing.T) {
		err := validateEnterpriseMeshPortConfig(&structs.NodeService{
			Service: "api",
			Ports:   structs.ServicePorts{{Name: "http", Port: 8080, Default: true}},
		})
		require.NoError(t, err)
	})

	t.Run("multiport sidecar registration is rejected", func(t *testing.T) {
		err := validateEnterpriseMeshPortConfig(&structs.NodeService{
			Service: "api",
			Ports:   structs.ServicePorts{{Name: "http", Port: 8080, Default: true}},
			Connect: structs.ServiceConnect{SidecarService: &structs.ServiceDefinition{}},
		})
		require.ErrorContains(t, err, "named service ports in the service mesh require Consul Enterprise")
	})

	t.Run("destination port upstreams are rejected", func(t *testing.T) {
		err := validateEnterpriseMeshPortConfig(&structs.NodeService{
			Kind: structs.ServiceKindConnectProxy,
			Proxy: structs.ConnectProxyConfig{
				Upstreams: structs.Upstreams{{
					DestinationName: "db",
					DestinationPort: "admin",
					LocalBindPort:   7000,
				}},
			},
		})
		require.ErrorContains(t, err, "destination port routing requires Consul Enterprise")
	})
}

// A server is a member of the default LAN pool and of every segment pool, and
// LANSendUserEvent broadcasts the same event to all of them: each pool
// delivers its own copy over one shared channel, so without dedup a fanned-out
// event would arrive at the agent once per pool. The copies must collapse to
// one by payload bytes (every pool carries the identical []byte), otherwise
// e.g. `consul exec` targeting a server runs the command N+1 times. The dedup
// lives in the UserEventHandler wrapper that initEnterprise installs, so the
// copies are driven through that handler the same way the server does.
func TestHandleEvents_DeduplicatesFannedOutUserEvents(t *testing.T) {
	t.Parallel()

	// A bare Agent is enough: the wrapper only touches the dedup ring, and
	// handleEvents only touches eventCh, the event ring buffer and the
	// logger — no RPC, no cluster leader.
	a := &Agent{
		eventCh:    make(chan serf.UserEvent, 16),
		shutdownCh: make(chan struct{}),
		eventBuf:   make([]*UserEvent, 256),
		config:     &config.RuntimeConfig{NodeName: "test"},
		logger:     hclog.NewInterceptLogger(nil),
	}

	// This is the seam agent.go wires up before initEnterprise runs: the
	// handler that funnels every pool's copies into the agent's channel.
	consulCfg := &consul.Config{
		UserEventHandler: func(e serf.UserEvent) {
			select {
			case a.eventCh <- e:
			case <-a.shutdownCh:
			}
		},
	}
	require.NoError(t, a.initEnterprise(consulCfg))

	go a.handleEvents()
	defer close(a.shutdownCh)

	payload, err := encodeMsgPackUserEvent(&UserEvent{
		ID:      "fanned-out-event-id",
		Name:    "dedup-test",
		Version: userEventMaxVersion,
	})
	require.NoError(t, err)

	sentinel, err := encodeMsgPackUserEvent(&UserEvent{
		ID:      "dedup-sentinel-id",
		Name:    "dedup-sentinel",
		Version: userEventMaxVersion,
	})
	require.NoError(t, err)

	// Two copies of the same event with different LTimes (each pool has its
	// own Lamport clock, so only the payload bytes identify the event), then
	// a sentinel with a distinct payload: once the sentinel is buffered both
	// copies have been processed, because handleEvents is a single FIFO
	// consumer.
	consulCfg.UserEventHandler(serf.UserEvent{LTime: 10, Name: "dedup-test", Payload: payload})
	consulCfg.UserEventHandler(serf.UserEvent{LTime: 12, Name: "dedup-test", Payload: payload})
	consulCfg.UserEventHandler(serf.UserEvent{LTime: 11, Name: "dedup-sentinel", Payload: sentinel})

	countByName := func(name string) int {
		n := 0
		for _, e := range a.UserEvents() {
			if e.Name == name {
				n++
			}
		}
		return n
	}

	retry.Run(t, func(r *retry.R) {
		require.Equal(r, 1, countByName("dedup-sentinel"), "sentinel not processed yet")
		require.Equal(r, 1, countByName("dedup-test"),
			"the duplicate copy must be dropped by payload bytes")
	})
}
