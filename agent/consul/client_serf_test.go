// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package consul

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A client keeps exactly one snapshot for its single pool, and that snapshot
// has to follow the segment the client belongs to: serf snapshots record
// names and addresses but never segment tags, so sharing one file between the
// default pool and a named segment lets an agent that was moved between them
// rejoin the pool it was moved out of.
func TestClientSerfSnapshotPath(t *testing.T) {
	cases := map[string]struct {
		segment string
		expect  string
	}{
		"default pool": {
			segment: "",
			expect:  "serf/local.snapshot",
		},
		"named segment": {
			segment: "lan",
			expect:  "serf/local-lan.snapshot",
		},
		"segment names are lowercased like on servers": {
			segment: "Edge-A",
			expect:  "serf/local-edge-a.snapshot",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.expect, clientSerfSnapshotPath(tc.segment))
		})
	}
}
