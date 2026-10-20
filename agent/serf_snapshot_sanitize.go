// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/go-hclog"

	"github.com/hashicorp/consul/agent/config"
)

// serfSnapshotFingerprintFile records which gossip identity the serf
// snapshots next to it were written for.
const serfSnapshotFingerprintFile = "serf-snapshot-fingerprint"

// sanitizeSerfSnapshots drops the serf gossip snapshots of an agent whose
// gossip identity changed since they were written.
//
// A serf snapshot only ever records member names and addresses. It does not
// record which network segment a member belongs to, so once an agent is
// renamed or moved between segments the snapshot turns from "rejoin the
// cluster I was part of" into a list of peers that are no longer valid join
// targets: on start serf replays it inside Create, before any merge delegate
// gets a chance to see the members, and the agent either flaps
// leave/join forever or refuses to join with a wrong-segment error.
//
// The change is detected with a fingerprint of everything that defines
// which pools this agent is allowed to talk to: its node name, its
// datacenter, its segment, and the set of segments it serves together with
// their ports. Addresses and IPs are deliberately not part of it - moving an
// agent to another address is a normal occurrence and does not invalidate
// the peers it knows.
//
// This is opt-in: the very first start with the option enabled has no
// fingerprint on disk and therefore removes all snapshots, which disables
// serf's built-in rejoin behavior until retry_join or an explicit join
// repopulates it. That behavior is documented in RUNBOOK-1.22.7.md.
func sanitizeSerfSnapshots(cfg *config.RuntimeConfig, logger hclog.Logger) error {
	if !cfg.SanitizeSerfSnapshotOnConfigChange {
		return nil
	}
	if cfg.DevMode || cfg.DataDir == "" {
		return nil
	}

	serfDir := filepath.Join(cfg.DataDir, "serf")
	if err := os.MkdirAll(serfDir, 0o755); err != nil {
		return fmt.Errorf("failed to create serf data directory %s: %w", serfDir, err)
	}

	fingerprintPath := filepath.Join(serfDir, serfSnapshotFingerprintFile)
	want := serfSnapshotFingerprint(cfg)

	reason, changed, err := serfSnapshotReason(fingerprintPath, want)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	snapshots, err := filepath.Glob(filepath.Join(serfDir, "*.snapshot"))
	if err != nil {
		return fmt.Errorf("failed to list serf snapshots in %s: %w", serfDir, err)
	}

	for _, snapshot := range snapshots {
		if err := os.Remove(snapshot); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("failed to remove stale serf snapshot %s: %w", snapshot, err)
		}
		logger.Info("removed serf gossip snapshot because the agent's gossip identity changed",
			"file", snapshot, "reason", reason)
	}

	if err := os.WriteFile(fingerprintPath, []byte(want+"\n"), 0o644); err != nil {
		return fmt.Errorf("failed to write serf snapshot fingerprint %s: %w", fingerprintPath, err)
	}
	return nil
}

// serfSnapshotReason compares the recorded fingerprint with the current one
// and reports whether the snapshots on disk are still valid for this
// identity.
func serfSnapshotReason(fingerprintPath, want string) (reason string, changed bool, err error) {
	prev, err := os.ReadFile(fingerprintPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The very first start with the option enabled. There is nothing to
		// compare against, so the snapshots - whatever identity they were
		// written for - are removed.
		return "no fingerprint recorded yet", true, nil
	case err != nil:
		return "", false, fmt.Errorf("failed to read serf snapshot fingerprint %s: %w", fingerprintPath, err)
	}

	if strings.TrimSpace(string(prev)) == want {
		return "", false, nil
	}
	return "gossip identity changed since the snapshot was written", true, nil
}

// serfSnapshotFingerprint builds the identity the snapshots are validated
// against. The parts are length-delimited so that no combination of values
// can collide with another one.
func serfSnapshotFingerprint(cfg *config.RuntimeConfig) string {
	var b strings.Builder

	write := func(key, value string) {
		fmt.Fprintf(&b, "%s\x00%s\x00", key, value)
	}

	write("node_name", cfg.NodeName)
	write("datacenter", cfg.Datacenter)
	write("segment", cfg.SegmentName)

	// Order the segments by name so that a reordering of the config block is
	// not mistaken for a change of the segment set itself. The port lives on
	// the bind address; it decides which remote pool this agent's segment
	// pools line up with, so it is part of the identity.
	segments := make([]string, 0, len(cfg.Segments))
	for _, segment := range cfg.Segments {
		port := 0
		if segment.Bind != nil {
			port = segment.Bind.Port
		}
		segments = append(segments, fmt.Sprintf("%s=%d", segment.Name, port))
	}
	sort.Strings(segments)
	for _, segment := range segments {
		write("segment_config", segment)
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
