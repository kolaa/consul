// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package config

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/sdk/testutil"
)

func TestSegments(t *testing.T) {
	dataDir := testutil.TempDir(t, "consul")

	tests := []testCase{
		{
			desc: "segment on a server is rejected",
			args: []string{
				`-data-dir=` + dataDir,
			},
			json:        []string{`{ "server": true, "segment": "a" }`},
			hcl:         []string{` server = true segment = "a" `},
			expectedErr: `is a client-only option`,
		},
		{
			desc: "segments on a client are rejected",
			args: []string{
				`-data-dir=` + dataDir,
			},
			json:        []string{`{ "segments":[{ "name":"alpha", "port": 8303 }] }`},
			hcl:         []string{`segments = [{ name = "alpha", port = 8303 }]`},
			expectedErr: `is a server-only option`,
		},
		{
			desc: "segments on a server",
			args: []string{
				`-data-dir=` + dataDir,
				`-server`,
			},
			json: []string{`{ "segments":[{ "name":"alpha", "port": 8303 }] }`},
			hcl:  []string{`segments = [{ name = "alpha", port = 8303 }]`},
			expected: func(rt *RuntimeConfig) {
				rt.DataDir = dataDir
				rt.ServerMode = true
				rt.TLS.ServerMode = true
				rt.LeaveOnTerm = false
				rt.SkipLeaveOnInt = true
				rt.RPCConfig.EnableStreaming = true
				rt.GRPCTLSPort = 8503
				rt.GRPCTLSAddrs = []net.Addr{defaultGrpcTlsAddr}
				rt.Segments = []structs.NetworkSegment{{
					Name:      "alpha",
					Bind:      &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 8303},
					Advertise: &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8303},
				}}
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
		},
	}

	for _, tc := range tests {
		for _, format := range []string{"json", "hcl"} {
			name := fmt.Sprintf("%v_%v", tc.desc, format)
			t.Run(name, tc.run(format, dataDir))
		}
	}
}

func segTCPAddr(ip string, port int) *net.TCPAddr {
	return &net.TCPAddr{IP: net.ParseIP(ip), Port: port}
}

// segmentValidationFixture returns a RuntimeConfig as the builder produces it
// for a healthy server, so cases only vary in the segments under test.
func segmentValidationFixture(segs ...structs.NetworkSegment) RuntimeConfig {
	return RuntimeConfig{
		ServerMode:       true,
		SegmentLimit:     64,
		SegmentNameLimit: 64,
		SerfPortLAN:      8301,
		ServerPort:       8300,
		SerfBindAddrLAN:  segTCPAddr("127.0.0.1", 8301),
		SerfBindAddrWAN:  segTCPAddr("127.0.0.1", 8302),
		RPCBindAddr:      segTCPAddr("127.0.0.1", 8300),
		Segments:         segs,
	}
}

func segmentCase(name string, port int, opts ...func(*structs.NetworkSegment)) structs.NetworkSegment {
	seg := structs.NetworkSegment{
		Name:      name,
		Bind:      segTCPAddr("10.0.0.1", port),
		Advertise: segTCPAddr("10.0.0.1", port),
	}
	for _, opt := range opts {
		opt(&seg)
	}
	return seg
}

// TestValidateSegments pins the single-pass validator: every problem in the
// segments config must be reported at once (errors.Join), including the
// conflicts the old validator never checked (bind collisions, reserved and
// duplicate names, the serf_lan port).
func TestValidateSegments(t *testing.T) {
	longName := "a" + strings.Repeat("b", 64) // 65 chars with the 64 limit

	cases := map[string]struct {
		rt       RuntimeConfig
		wantErrs []string // every substring must appear in the joined error
	}{
		"valid server segments": {
			rt: segmentValidationFixture(
				segmentCase("alpha", 8303),
				segmentCase("beta", 8304),
			),
		},
		"single character segment name is valid": {
			rt: segmentValidationFixture(segmentCase("a", 8303)),
		},
		"underscore in segment name is valid": {
			rt: segmentValidationFixture(segmentCase("a_b", 8303)),
		},
		"duplicate segment names": {
			rt: segmentValidationFixture(
				segmentCase("alpha", 8303),
				segmentCase("alpha", 8304),
			),
			wantErrs: []string{`segment "alpha" duplicates "alpha"`},
		},
		"case-insensitive duplicate names share a snapshot": {
			rt: segmentValidationFixture(
				segmentCase("Prod", 8303),
				segmentCase("prod", 8304),
			),
			wantErrs: []string{`case-insensitive: both use serf/prod.snapshot`},
		},
		"reserved segment name local": {
			rt:       segmentValidationFixture(segmentCase("local", 8303)),
			wantErrs: []string{`segment name "local" is reserved`},
		},
		"reserved segment name is case-insensitive": {
			rt:       segmentValidationFixture(segmentCase("ReMoTe", 8303)),
			wantErrs: []string{`segment name "ReMoTe" is reserved`},
		},
		"windows device name con is reserved": {
			rt:       segmentValidationFixture(segmentCase("con", 8303)),
			wantErrs: []string{`segment name "con" is reserved`},
		},
		"windows device names are reserved case-insensitively": {
			rt: segmentValidationFixture(
				segmentCase("NUL", 8303),
				segmentCase("Aux", 8304),
				segmentCase("prn", 8305),
			),
			wantErrs: []string{
				`segment name "NUL" is reserved`,
				`segment name "Aux" is reserved`,
				`segment name "prn" is reserved`,
			},
		},
		"windows numbered device names are reserved": {
			rt: segmentValidationFixture(
				segmentCase("com1", 8303),
				segmentCase("COM9", 8304),
				segmentCase("lpt1", 8305),
				segmentCase("Lpt9", 8306),
			),
			wantErrs: []string{
				`segment name "com1" is reserved`,
				`segment name "COM9" is reserved`,
				`segment name "lpt1" is reserved`,
				`segment name "Lpt9" is reserved`,
			},
		},
		"names that only start like a windows device name are valid": {
			rt: segmentValidationFixture(
				segmentCase("console", 8303),
				segmentCase("com10", 8304),
				segmentCase("lpt", 8305),
				segmentCase("aux1", 8306),
			),
		},
		"client segment name is checked for windows device names": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture()
				rt.ServerMode = false
				rt.SegmentName = "com3"
				return rt
			}(),
			wantErrs: []string{`segment name "com3" is reserved`},
		},
		"blank segment name": {
			rt:       segmentValidationFixture(segmentCase("", 8303)),
			wantErrs: []string{`segment name cannot be blank`},
		},
		"name must start with a letter": {
			rt:       segmentValidationFixture(segmentCase("1bad", 8303)),
			wantErrs: []string{`segment name "1bad" must start with a letter, end with a letter or digit`},
		},
		"name must end with a letter or digit": {
			rt:       segmentValidationFixture(segmentCase("bad-", 8303)),
			wantErrs: []string{`segment name "bad-" must start with a letter`},
		},
		"name exceeds the limit": {
			rt:       segmentValidationFixture(segmentCase(longName, 8303)),
			wantErrs: []string{`exceeds maximum length of 64`},
		},
		"duplicate ports are rejected": {
			rt: segmentValidationFixture(
				segmentCase("alpha", 8303),
				segmentCase("beta", 8303),
			),
			wantErrs: []string{`segment "beta" port 8303 is already used by segment "alpha"`},
		},
		"segment port equal to serf_lan": {
			rt:       segmentValidationFixture(segmentCase("alpha", 8301)),
			wantErrs: []string{`equals serf_lan`},
		},
		"wildcard bind conflicts with a specific serf_lan bind": {
			rt: segmentValidationFixture(
				segmentCase("alpha", 8301, func(s *structs.NetworkSegment) {
					s.Bind = segTCPAddr("0.0.0.0", 8301)
				}),
			),
			wantErrs: []string{`conflicts with serf_lan bind`},
		},
		"wildcard server RPC bind conflicts with a segment bind": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(segmentCase("alpha", 8300))
				rt.RPCBindAddr = segTCPAddr("0.0.0.0", 8300)
				return rt
			}(),
			wantErrs: []string{`conflicts with server (RPC) bind`},
		},
		"rpc_listener collides with the main RPC listener": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(segmentCase("alpha", 8305, func(s *structs.NetworkSegment) {
					s.Bind = segTCPAddr("10.0.0.6", 8305)
					s.RPCListener = true
				}))
				rt.RPCBindAddr = segTCPAddr("10.0.0.6", 8300)
				return rt
			}(),
			wantErrs: []string{`rpc_listener would bind 10.0.0.6:8300, already used by the main RPC listener`},
		},
		"two rpc_listeners on the same IP collide": {
			// The main RPC listener lives on a different IP, so only the
			// pairwise check between the segments themselves can catch this:
			// both rpc_listeners target <bind IP>:server_port.
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(
					segmentCase("alpha", 8305, func(s *structs.NetworkSegment) {
						s.Bind = segTCPAddr("10.0.0.6", 8305)
						s.RPCListener = true
					}),
					segmentCase("beta", 8306, func(s *structs.NetworkSegment) {
						s.Bind = segTCPAddr("10.0.0.6", 8306)
						s.RPCListener = true
					}),
				)
				rt.RPCBindAddr = segTCPAddr("10.0.0.5", 8300)
				return rt
			}(),
			wantErrs: []string{`segment "beta": rpc_listener would bind 10.0.0.6:8300, already used by segment "alpha" rpc_listener`},
		},
		"rpc_listener collides with the segment's own serf bind": {
			// Bind port == server_port on the segment IP: the rpc_listener
			// target is already taken by this segment's serf pool, and the
			// main RPC listener lives elsewhere, so only the loop over the
			// pools bound so far can catch it.
			rt: segmentValidationFixture(segmentCase("alpha", 8300, func(s *structs.NetworkSegment) {
				s.Bind = segTCPAddr("10.0.0.6", 8300)
				s.RPCListener = true
			})),
			wantErrs: []string{`segment "alpha": rpc_listener would bind 10.0.0.6:8300, already used by segment "alpha" bind 10.0.0.6:8300`},
		},
		"wildcard bind conflicts with serf_wan bind": {
			rt: segmentValidationFixture(segmentCase("alpha", 8302, func(s *structs.NetworkSegment) {
				s.Bind = segTCPAddr("0.0.0.0", 8302)
			})),
			wantErrs: []string{`conflicts with serf_wan bind`},
		},
		"advertise address cannot be a wildcard": {
			rt: segmentValidationFixture(segmentCase("alpha", 8303, func(s *structs.NetworkSegment) {
				s.Advertise = segTCPAddr("0.0.0.0", 8303)
			})),
			wantErrs: []string{`advertise address for segment "alpha" cannot be 0.0.0.0, :: or [::]`},
		},
		"segment limit": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(
					segmentCase("a", 8303),
					segmentCase("b", 8304),
					segmentCase("c", 8305),
				)
				rt.SegmentLimit = 2
				return rt
			}(),
			wantErrs: []string{`cannot exceed network segment limit of 2`},
		},
		"all problems are reported in one pass": {
			rt: segmentValidationFixture(
				segmentCase("alpha", 8303),
				segmentCase("alpha", 8303),
				segmentCase("Prod", 8305),
				segmentCase("prod", 8306),
				segmentCase("beta", 8301),
			),
			wantErrs: []string{
				`case-insensitive: both use serf/prod.snapshot`,
				`is already used by segment`,
				`equals serf_lan`,
			},
		},
		"segment on a server is rejected": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture()
				rt.SegmentName = "dmz"
				return rt
			}(),
			wantErrs: []string{`is a client-only option`},
		},
		"segments on a client are rejected": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(segmentCase("alpha", 8303))
				rt.ServerMode = false
				return rt
			}(),
			wantErrs: []string{`is a server-only option`},
		},
		"client segment name is validated": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture()
				rt.ServerMode = false
				rt.SegmentName = "1dmz"
				return rt
			}(),
			wantErrs: []string{`segment name "1dmz" must start with a letter`},
		},
		"client segment name cannot be reserved": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture()
				rt.ServerMode = false
				rt.SegmentName = "Remote"
				return rt
			}(),
			wantErrs: []string{`segment name "Remote" is reserved`},
		},
		"segment tags beyond the serf tag budget": {
			rt: func() RuntimeConfig {
				var segs []structs.NetworkSegment
				for i := range 20 {
					name := fmt.Sprintf("seg%02d", i) + strings.Repeat("x", 25) // 30 chars
					segs = append(segs, segmentCase(name, 9000+i))
				}
				rt := segmentValidationFixture(segs...)
				rt.Datacenter = "dc1"
				return rt
			}(),
			wantErrs: []string{`20 segments need`, `serf allows 512 bytes`},
		},
		"small segment set fits the serf tag budget": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(
					segmentCase("alpha", 8303),
					segmentCase("bravo", 8304),
					segmentCase("delta", 8305),
				)
				rt.Datacenter = "dc1"
				return rt
			}(),
		},
		"segment bind conflicts with the HTTP listener": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(segmentCase("alpha", 8500))
				rt.HTTPAddrs = []net.Addr{segTCPAddr("10.0.0.1", 8500)}
				return rt
			}(),
			wantErrs: []string{`segment "alpha" bind 10.0.0.1:8500 conflicts with HTTP bind 10.0.0.1:8500`},
		},
		"segment bind conflicts with a wildcard HTTPS listener": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(segmentCase("alpha", 8501))
				rt.HTTPSAddrs = []net.Addr{segTCPAddr("0.0.0.0", 8501)}
				return rt
			}(),
			wantErrs: []string{`conflicts with HTTPS bind 0.0.0.0:8501`},
		},
		"segment bind conflicts with the gRPC listener": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(segmentCase("alpha", 8502))
				rt.GRPCAddrs = []net.Addr{segTCPAddr("10.0.0.1", 8502)}
				return rt
			}(),
			wantErrs: []string{`conflicts with gRPC bind 10.0.0.1:8502`},
		},
		"segment bind conflicts with the gRPC TLS listener": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(segmentCase("alpha", 8503))
				rt.GRPCTLSAddrs = []net.Addr{segTCPAddr("10.0.0.1", 8503)}
				return rt
			}(),
			wantErrs: []string{`conflicts with gRPC TLS bind 10.0.0.1:8503`},
		},
		"segment bind conflicts with the DNS listener": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(segmentCase("alpha", 8600))
				rt.DNSAddrs = []net.Addr{
					segTCPAddr("10.0.0.1", 8600),
					&net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8600},
				}
				return rt
			}(),
			wantErrs: []string{`conflicts with DNS bind 10.0.0.1:8600`},
		},
		"client listeners on other IPs or unix sockets do not conflict": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture(segmentCase("alpha", 8500))
				rt.HTTPAddrs = []net.Addr{
					segTCPAddr("10.0.0.2", 8500),
					&net.UnixAddr{Name: "/tmp/consul-http.sock", Net: "unix"},
				}
				return rt
			}(),
		},
		"valid client segment name": {
			rt: func() RuntimeConfig {
				rt := segmentValidationFixture()
				rt.ServerMode = false
				rt.SegmentName = "dmz"
				return rt
			}(),
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := (&builder{}).validateSegments(tc.rt)
			if len(tc.wantErrs) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err, "expected the segment config to be rejected")
			for _, want := range tc.wantErrs {
				require.ErrorContains(t, err, want)
			}
		})
	}
}

// JoinLAN sends a retry_join entry with a segment port only to that segment's
// pool, so a server listing nothing but segment ports never joins the default
// LAN pool. That still loads, but must warn.
func TestValidateSegments_SegmentOnlyRetryJoinWarns(t *testing.T) {
	cases := map[string]struct {
		retryJoin []string
		wantWarn  bool
	}{
		"only segment ports":        {retryJoin: []string{"10.0.0.5:8303", "10.0.0.6:8304"}, wantWarn: true},
		"a serf_lan port alongside": {retryJoin: []string{"10.0.0.5:8303", "10.0.0.6:8301"}},
		"a bare host alongside":     {retryJoin: []string{"10.0.0.5:8303", "10.0.0.6"}},
		"cloud auto-join alongside": {retryJoin: []string{"10.0.0.5:8303", "provider=aws tag_key=consul"}},
		"no retry_join":             {},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rt := segmentValidationFixture(segmentCase("alpha", 8303), segmentCase("beta", 8304))
			rt.RetryJoinLAN = tc.retryJoin
			b := &builder{}
			require.NoError(t, b.validateSegments(rt))

			var n int
			for _, w := range b.Warnings {
				if strings.Contains(w, "every retry_join entry targets a segment port") {
					n++
				}
			}
			if tc.wantWarn {
				require.Equal(t, 1, n, "warnings: %q", b.Warnings)
			} else {
				require.Zero(t, n, "warnings: %q", b.Warnings)
			}
		})
	}
}

// Without rpc_listener, a segment's clients dial the segment advertise IP on
// the server RPC port, which the main RPC listener only answers when it binds
// a wildcard or that same IP. Load must warn about the unreachable case.
func TestLoad_SegmentRPCUnreachableWarns(t *testing.T) {
	cases := map[string]struct {
		hcl      string
		wantWarn bool
	}{
		"specific RPC bind, other advertise IP": {
			hcl:      `bind_addr = "127.0.0.1" segments = [{ name = "alpha", port = 8303, advertise = "10.0.0.2" }]`,
			wantWarn: true,
		},
		"rpc_listener on the segment IP": {
			hcl: `bind_addr = "127.0.0.1" segments = [{ name = "alpha", port = 8303, bind = "10.0.0.2", advertise = "10.0.0.2", rpc_listener = true }]`,
		},
		"advertise IP equals the RPC bind IP": {
			hcl: `bind_addr = "127.0.0.1" segments = [{ name = "alpha", port = 8303, advertise = "127.0.0.1" }]`,
		},
		"wildcard RPC bind": {
			hcl: `bind_addr = "0.0.0.0" segments = [{ name = "alpha", port = 8303, advertise = "10.0.0.2" }]`,
		},
		// Behind NAT the segment inherits advertise_addr, which every client
		// already dials on the server port.
		"advertise IP equals the RPC advertise IP": {
			hcl: `bind_addr = "127.0.0.1" advertise_addr = "10.0.0.9" segments = [{ name = "alpha", port = 8303 }]`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hcl := fmt.Sprintf("server = true\ndata_dir = %q\n%s", testutil.TempDir(t, "consul"), tc.hcl)
			opts := LoadOpts{sources: []Source{FileSource{Name: "segments.hcl", Format: "hcl", Data: hcl}}}
			patchLoadOptsShims(&opts)
			result, err := Load(opts)
			require.NoError(t, err)

			var n int
			for _, w := range result.Warnings {
				if strings.Contains(w, `segment "alpha": clients dial`) {
					n++
				}
			}
			if tc.wantWarn {
				require.Equal(t, 1, n, "warnings: %q", result.Warnings)
			} else {
				require.Zero(t, n, "warnings: %q", result.Warnings)
			}
		})
	}
}
