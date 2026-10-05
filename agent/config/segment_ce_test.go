// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package config

import (
	"fmt"
	"net"
	"testing"

	"github.com/hashicorp/consul/sdk/testutil"
)

func TestSegments(t *testing.T) {
	dataDir := testutil.TempDir(t, "consul")

	tests := []testCase{
		{
			desc: "segment name",
			args: []string{
				`-data-dir=` + dataDir,
			},
			json: []string{`{ "server": true, "segment": "a" }`},
			hcl:  []string{` server = true segment = "a" `},
			expected: func(rt *RuntimeConfig) {
				rt.SegmentName = "a"
				rt.ServerMode = true
				rt.TLS.ServerMode = true
				rt.LeaveOnTerm = false
				rt.SkipLeaveOnInt = true
				rt.DataDir = dataDir
				rt.RPCConfig.EnableStreaming = true
				rt.GRPCTLSPort = 8503
				rt.GRPCTLSAddrs = []net.Addr{defaultGrpcTlsAddr}
			},
		},
		{
			desc: "segment port must be set",
			args: []string{
				`-data-dir=` + dataDir,
			},
			json:        []string{`{ "segments":[{ "name":"x" }] }`},
			hcl:         []string{`segments = [{ name = "x" }]`},
			expectedErr: `Port for segment "x" cannot be <= 0`,
			//expectedWarnings: []string{
			//	enterpriseConfigKeyError{key: "segments"}.Error(),
			//},
		},
		// {
		// 	desc: "segments not in CE",
		// 	args: []string{
		// 		`-data-dir=` + dataDir,
		// 	},
		// 	json:        []string{`{ "segments":[{ "name":"x", "port": 123 }] }`},
		// 	hcl:         []string{`segments = [{ name = "x" port = 123 }]`},
		// 	expected: func(rt *RuntimeConfig) {
		// 		rt.Segments = []NetworkSegment("x", 123)
		// 	},
		// },
	}

	for _, tc := range tests {
		for _, format := range []string{"json", "hcl"} {
			name := fmt.Sprintf("%v_%v", tc.desc, format)
			t.Run(name, tc.run(format, dataDir))
		}
	}
}
