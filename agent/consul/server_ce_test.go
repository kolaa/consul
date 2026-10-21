// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/testrpc"
)

func TestSegmentJoinAddrs(t *testing.T) {
	segments := []NetworkSegment{
		{Name: "wan-ish", Port: 8302},
		{Name: "second", Port: 8303},
	}

	cases := map[string]struct {
		segments       []NetworkSegment
		addrs          []string
		expectLAN      []string
		expectSegments map[string][]string
	}{
		"bare addresses reach the default pool and every segment": {
			segments:  segments,
			addrs:     []string{"10.0.0.1", "server.example.com"},
			expectLAN: []string{"10.0.0.1", "server.example.com"},
			expectSegments: map[string][]string{
				"wan-ish": {"10.0.0.1:8302", "server.example.com:8302"},
				"second":  {"10.0.0.1:8303", "server.example.com:8303"},
			},
		},
		"mixed bare and explicit-port addresses": {
			segments:  segments,
			addrs:     []string{"10.0.0.1", "server.example.com:8301", "10.0.0.2:8303"},
			expectLAN: []string{"10.0.0.1", "server.example.com:8301"},
			expectSegments: map[string][]string{
				"wan-ish": {"10.0.0.1:8302"},
				"second":  {"10.0.0.1:8303", "10.0.0.2:8303"},
			},
		},
		"an explicit segment port only reaches that segment": {
			segments:  segments,
			addrs:     []string{"10.0.0.1:8303"},
			expectLAN: []string{},
			expectSegments: map[string][]string{
				"second": {"10.0.0.1:8303"},
			},
		},
		"an explicit non-segment port only reaches the default pool": {
			segments:       segments,
			addrs:          []string{"10.0.0.1:1234"},
			expectLAN:      []string{"10.0.0.1:1234"},
			expectSegments: map[string][]string{},
		},
		"bare IPv6 gets bracketed for each segment": {
			segments:  segments,
			addrs:     []string{"fd00::1"},
			expectLAN: []string{"fd00::1"},
			expectSegments: map[string][]string{
				"wan-ish": {"[fd00::1]:8302"},
				"second":  {"[fd00::1]:8303"},
			},
		},
		"no segments configured": {
			addrs:          []string{"10.0.0.1"},
			expectLAN:      []string{"10.0.0.1"},
			expectSegments: map[string][]string{},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			lan, perSegment := segmentJoinAddrs(tc.addrs, tc.segments)
			require.Equal(t, tc.expectLAN, lan)
			require.Equal(t, tc.expectSegments, perSegment)
		})
	}
}

func TestAgent_ReloadConfig_Reporting(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}
	t.Parallel()

	dir1, s := testServerWithConfig(t, func(c *Config) {
		c.Reporting.License.Enabled = false
	})
	defer os.RemoveAll(dir1)
	defer s.Shutdown()

	testrpc.WaitForTestAgent(t, s.RPC, "dc1")

	require.Equal(t, false, s.config.Reporting.License.Enabled)

	rc := ReloadableConfig{
		Reporting: Reporting{
			License: License{
				Enabled: true,
			},
		},
	}

	require.NoError(t, s.ReloadConfig(rc))

	// Check config reload is no-op
	require.Equal(t, false, s.config.Reporting.License.Enabled)
}
