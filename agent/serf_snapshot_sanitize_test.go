// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/agent/config"
	"github.com/hashicorp/consul/agent/structs"
)

// newSanitizeTestSetup creates a data dir that already looks like a data dir
// of a running agent: a serf/ directory holding one snapshot per pool, but no
// fingerprint because the option was never enabled here before.
func newSanitizeTestSetup(t *testing.T) (*config.RuntimeConfig, string) {
	t.Helper()

	dataDir := t.TempDir()
	serfDir := filepath.Join(dataDir, "serf")
	require.NoError(t, os.MkdirAll(serfDir, 0o755))
	for _, name := range []string{"local.snapshot", "remote.snapshot", "local-lan.snapshot"} {
		require.NoError(t, os.WriteFile(filepath.Join(serfDir, name), []byte("snapshot"), 0o644))
	}

	return &config.RuntimeConfig{
		DataDir:                            dataDir,
		NodeName:                           "node1",
		Datacenter:                         "dc1",
		SanitizeSerfSnapshotOnConfigChange: true,
	}, serfDir
}

func remainingSnapshots(t *testing.T, serfDir string) []string {
	t.Helper()

	entries, err := filepath.Glob(filepath.Join(serfDir, "*.snapshot"))
	require.NoError(t, err)
	return entries
}

func runSanitize(t *testing.T, cfg *config.RuntimeConfig) {
	t.Helper()
	require.NoError(t, sanitizeSerfSnapshots(cfg, hclog.NewNullLogger()))
}

func TestSanitizeSerfSnapshots_DisabledByDefault(t *testing.T) {
	cfg, serfDir := newSanitizeTestSetup(t)
	cfg.SanitizeSerfSnapshotOnConfigChange = false

	// Rename the node: without the option nothing may be touched.
	cfg.NodeName = "renamed"

	runSanitize(t, cfg)

	require.Len(t, remainingSnapshots(t, serfDir), 3)
	require.NoFileExists(t, filepath.Join(serfDir, serfSnapshotFingerprintFile))
}

func TestSanitizeSerfSnapshots_FirstStartRemovesEverything(t *testing.T) {
	cfg, serfDir := newSanitizeTestSetup(t)

	runSanitize(t, cfg)

	require.Empty(t, remainingSnapshots(t, serfDir))
	require.FileExists(t, filepath.Join(serfDir, serfSnapshotFingerprintFile))
}

func TestSanitizeSerfSnapshots_UnchangedIdentityKeepsSnapshots(t *testing.T) {
	cfg, serfDir := newSanitizeTestSetup(t)

	runSanitize(t, cfg)

	// Recreate the snapshots as if the agent had been running for a while.
	for _, name := range []string{"local.snapshot", "remote.snapshot"} {
		require.NoError(t, os.WriteFile(filepath.Join(serfDir, name), []byte("snapshot"), 0o644))
	}

	runSanitize(t, cfg)

	require.Len(t, remainingSnapshots(t, serfDir), 2)
}

func TestSanitizeSerfSnapshots_RemovesOnIdentityChange(t *testing.T) {
	type testcase struct {
		// before runs before the first start, which is what records the
		// fingerprint the second start is compared against.
		before func(cfg *config.RuntimeConfig)
		change func(cfg *config.RuntimeConfig)
	}

	testcases := map[string]testcase{
		"node renamed": {
			change: func(cfg *config.RuntimeConfig) {
				cfg.NodeName = "node1.prod.example.com"
			},
		},
		"datacenter changed": {
			change: func(cfg *config.RuntimeConfig) {
				cfg.Datacenter = "dc2"
			},
		},
		"client moved into a segment": {
			change: func(cfg *config.RuntimeConfig) {
				cfg.SegmentName = "lan"
			},
		},
		"client moved out of a segment": {
			before: func(cfg *config.RuntimeConfig) {
				cfg.SegmentName = "lan"
			},
			change: func(cfg *config.RuntimeConfig) {
				cfg.SegmentName = ""
			},
		},
		"a segment changed its port": {
			before: func(cfg *config.RuntimeConfig) {
				cfg.Segments = []structs.NetworkSegment{
					{Name: "wan", Bind: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8303}},
				}
			},
			change: func(cfg *config.RuntimeConfig) {
				cfg.Segments = []structs.NetworkSegment{
					{Name: "wan", Bind: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8302}},
				}
			},
		},
		"a segment was renamed": {
			before: func(cfg *config.RuntimeConfig) {
				cfg.Segments = []structs.NetworkSegment{
					{Name: "wan", Bind: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8302}},
				}
			},
			change: func(cfg *config.RuntimeConfig) {
				cfg.Segments = []structs.NetworkSegment{
					{Name: "renamed", Bind: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8302}},
				}
			},
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			cfg, serfDir := newSanitizeTestSetup(t)
			if tc.before != nil {
				tc.before(cfg)
			}

			runSanitize(t, cfg)

			// Now the agent has been running and re-created its snapshots.
			require.NoError(t, os.WriteFile(filepath.Join(serfDir, "local.snapshot"), []byte("snapshot"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(serfDir, "local-lan.snapshot"), []byte("snapshot"), 0o644))
			require.Len(t, remainingSnapshots(t, serfDir), 2)

			tc.change(cfg)
			runSanitize(t, cfg)

			require.Empty(t, remainingSnapshots(t, serfDir))
		})
	}
}

func TestSanitizeSerfSnapshots_IgnoresAddressChanges(t *testing.T) {
	cfg, serfDir := newSanitizeTestSetup(t)
	cfg.AdvertiseAddrLAN = &net.IPAddr{IP: net.ParseIP("10.0.0.1")}

	runSanitize(t, cfg)
	require.NoError(t, os.WriteFile(filepath.Join(serfDir, "local.snapshot"), []byte("snapshot"), 0o644))

	// Moving the agent to a different address does not invalidate the peers
	// it knows, so the snapshots must survive.
	cfg.AdvertiseAddrLAN = &net.IPAddr{IP: net.ParseIP("10.0.0.99")}
	cfg.BindAddr = &net.IPAddr{IP: net.ParseIP("10.0.0.99")}

	runSanitize(t, cfg)

	require.Len(t, remainingSnapshots(t, serfDir), 1)
}

func TestSanitizeSerfSnapshots_SegmentOrderIsNotAChange(t *testing.T) {
	cfg, serfDir := newSanitizeTestSetup(t)
	cfg.Segments = []structs.NetworkSegment{
		{Name: "alpha", Bind: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8302}},
		{Name: "beta", Bind: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8303}},
	}

	runSanitize(t, cfg)
	require.NoError(t, os.WriteFile(filepath.Join(serfDir, "local.snapshot"), []byte("snapshot"), 0o644))

	cfg.Segments = []structs.NetworkSegment{
		{Name: "beta", Bind: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8303}},
		{Name: "alpha", Bind: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8302}},
	}

	runSanitize(t, cfg)

	require.Len(t, remainingSnapshots(t, serfDir), 1)
}

func TestSanitizeSerfSnapshots_DevModeIsSkipped(t *testing.T) {
	cfg, serfDir := newSanitizeTestSetup(t)
	cfg.DevMode = true
	cfg.NodeName = "renamed"

	runSanitize(t, cfg)

	require.Len(t, remainingSnapshots(t, serfDir), 3)
	require.NoFileExists(t, filepath.Join(serfDir, serfSnapshotFingerprintFile))
}

func TestSerfSnapshotFingerprint_IgnoresAddresses(t *testing.T) {
	base := &config.RuntimeConfig{
		NodeName:   "node1",
		Datacenter: "dc1",
	}

	moved := &config.RuntimeConfig{
		NodeName:             "node1",
		Datacenter:           "dc1",
		BindAddr:             &net.IPAddr{IP: net.ParseIP("10.0.0.2")},
		AdvertiseAddrLAN:     &net.IPAddr{IP: net.ParseIP("10.0.0.2")},
		SerfAdvertiseAddrLAN: &net.TCPAddr{IP: net.ParseIP("10.0.0.2"), Port: 8301},
	}

	require.Equal(t, serfSnapshotFingerprint(base), serfSnapshotFingerprint(moved))
	require.NotEqual(t,
		serfSnapshotFingerprint(base),
		serfSnapshotFingerprint(&config.RuntimeConfig{NodeName: "node2", Datacenter: "dc1"}),
	)
	require.NotEqual(t,
		serfSnapshotFingerprint(base),
		serfSnapshotFingerprint(&config.RuntimeConfig{NodeName: "node1", Datacenter: "dc2"}),
	)
	require.NotEqual(t,
		serfSnapshotFingerprint(base),
		serfSnapshotFingerprint(&config.RuntimeConfig{NodeName: "node1", Datacenter: "dc1", SegmentName: "lan"}),
	)
}
