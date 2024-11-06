// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateEnterpriseConfigKeys(t *testing.T) {
	// ensure that all the enterprise configurations
	type testCase struct {
		config  Config
		badKeys []string
		check   func(t *testing.T, c *Config)
	}

	boolVal := true
	stringVal := "string"

	cases := map[string]testCase{
		"read_replica": {
			config: Config{
				ReadReplica: &boolVal,
			},
			badKeys: []string{"read_replica (or the deprecated non_voting_server)"},
		},
		"autopilot.redundancy_zone_tag": {
			config: Config{
				Autopilot: Autopilot{
					RedundancyZoneTag: &stringVal,
				},
			},
			badKeys: []string{"autopilot.redundancy_zone_tag"},
		},
		"autopilot.upgrade_version_tag": {
			config: Config{
				Autopilot: Autopilot{
					UpgradeVersionTag: &stringVal,
				},
			},
			badKeys: []string{"autopilot.upgrade_version_tag"},
		},
		"autopilot.disable_upgrade_migration": {
			config: Config{
				Autopilot: Autopilot{DisableUpgradeMigration: &boolVal},
			},
			badKeys: []string{"autopilot.disable_upgrade_migration"},
		},
		"dns_config.prefer_namespace": {
			config: Config{
				DNS: DNS{PreferNamespace: &boolVal},
			},
			badKeys: []string{"dns_config.prefer_namespace"},
			check: func(t *testing.T, c *Config) {
				require.Nil(t, c.DNS.PreferNamespace)
			},
		},
		"acl.msp_disable_bootstrap": {
			config: Config{
				ACL: ACL{MSPDisableBootstrap: &boolVal},
			},
			badKeys: []string{"acl.msp_disable_bootstrap"},
			check: func(t *testing.T, c *Config) {
				require.Nil(t, c.ACL.MSPDisableBootstrap)
			},
		},
		"acl.tokens.managed_service_provider": {
			config: Config{
				ACL: ACL{
					Tokens: Tokens{
						ManagedServiceProvider: []ServiceProviderToken{
							{
								AccessorID: &stringVal,
								SecretID:   &stringVal,
							},
						},
					},
				},
			},
			badKeys: []string{"acl.tokens.managed_service_provider"},
			check: func(t *testing.T, c *Config) {
				require.Empty(t, c.ACL.Tokens.ManagedServiceProvider)
				require.Nil(t, c.ACL.Tokens.ManagedServiceProvider)
			},
		},
		"license_path": {
			config: Config{
				LicensePath: &stringVal,
			},
			badKeys: []string{"license_path"},
			check: func(t *testing.T, c *Config) {
				require.Empty(t, c.LicensePath)
			},
		},
		"reporting.license.enabled": {
			config: Config{
				Reporting: Reporting{
					License: License{
						Enabled: &boolVal,
					},
				},
			},
			badKeys: []string{"reporting.license.enabled"},
			check: func(t *testing.T, c *Config) {
				require.Nil(t, c.Reporting.License.Enabled)
			},
		},
		"multi": {
			config: Config{
				ReadReplica: &boolVal,
				SegmentName: &stringVal,
				ACL: ACL{
					Tokens: Tokens{
						DeprecatedTokens: DeprecatedTokens{AgentMaster: &stringVal},
					},
				},
			},
			badKeys: []string{"read_replica (or the deprecated non_voting_server)"},
		},
	}

	for name, tcase := range cases {
		t.Run(name, func(t *testing.T) {
			errs := validateEnterpriseConfigKeys(&tcase.config)
			if len(tcase.badKeys) == 0 {
				require.Len(t, errs, 0)
				return
			}

			var expected []error
			for _, k := range tcase.badKeys {
				expected = append(expected, enterpriseConfigKeyError{key: k})
			}
			require.ElementsMatch(t, expected, errs)

			if tcase.check != nil {
				tcase.check(t, &tcase.config)
			}
		})
	}
}

// The documented Enterprise network-segment client pattern is
// `segment = "alpha"` + `ports.serf_lan = 8303` + a bare retry_join entry:
// memberlist then resolves the port-less address against the overridden
// serf_lan port, so the join lands in the right segment pool. The bare
// address must only be rejected while ports.serf_lan is still the default.
func TestValidateEnterpriseConfig_SegmentClientRetryJoin(t *testing.T) {
	t.Parallel()

	b := &builder{}

	cases := map[string]struct {
		rt      RuntimeConfig
		wantErr string
	}{
		"documented pattern: ports.serf_lan overridden, bare retry_join": {
			rt: RuntimeConfig{
				SegmentName:  "alpha",
				SerfPortLAN:  8303,
				RetryJoinLAN: []string{"10.0.0.5"},
			},
		},
		"bare retry_join with the default serf_lan port is rejected": {
			rt: RuntimeConfig{
				SegmentName:  "alpha",
				SerfPortLAN:  8301,
				RetryJoinLAN: []string{"10.0.0.5"},
			},
			wantErr: "has no port",
		},
		"retry_join with an explicit port is accepted": {
			rt: RuntimeConfig{
				SegmentName:  "alpha",
				SerfPortLAN:  8301,
				RetryJoinLAN: []string{"10.0.0.5:8303"},
			},
		},
		"cloud auto-join results are not validated": {
			rt: RuntimeConfig{
				SegmentName:  "alpha",
				SerfPortLAN:  8301,
				RetryJoinLAN: []string{"provider=aws tag_key=consul segment=alpha"},
			},
		},
		"servers are not validated": {
			rt: RuntimeConfig{
				ServerMode:   true,
				SerfPortLAN:  8301,
				RetryJoinLAN: []string{"10.0.0.5"},
			},
		},
		"clients without a segment are not validated": {
			rt: RuntimeConfig{
				SerfPortLAN:  8301,
				RetryJoinLAN: []string{"10.0.0.5"},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := b.validateEnterpriseConfig(tc.rt)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// loadSegmentClient loads an agent config through the full parse, build and
// validate path. The base pins the bind and advertise addresses so no network
// interface lookup happens; extra holds the per-case segment, port and
// retry_join lines.
func loadSegmentClient(t *testing.T, extra string) (LoadResult, error) {
	t.Helper()
	return Load(LoadOpts{
		HCL: []string{`
			data_dir = "` + filepath.ToSlash(t.TempDir()) + `"
			bind_addr = "127.0.0.1"
			advertise_addr = "127.0.0.1"
			node_name = "node1"
		`, extra},
	})
}

// ports.serf_lan = 0 never reaches validateEnterpriseConfig: the builder
// rejects the dynamic port first, so the segment check needs no case for it.
func TestLoad_SegmentClientDynamicLANPort(t *testing.T) {
	t.Parallel()

	_, err := loadSegmentClient(t, `
		segment = "alpha"
		ports { serf_lan = 0 }
	`)
	require.ErrorContains(t, err, "serf-lan-port must be greater than zero")
}

func TestLoad_SegmentClientRetryJoinPort(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		hcl     string
		wantErr string
	}{
		"bare address with the default serf_lan port is rejected": {
			hcl:     `segment = "alpha"` + "\n" + `retry_join = ["10.0.0.5"]`,
			wantErr: "has no port",
		},
		"address with an explicit port is accepted": {
			hcl: `segment = "alpha"` + "\n" + `retry_join = ["10.0.0.5:8303"]`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := loadSegmentClient(t, tc.hcl)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// A segment client that likely cannot join its segment still loads, but gets
// a warning. Each case counts the warnings that contain want; with want empty
// it asserts that no warning mentions a segment at all.
func TestLoad_SegmentClientWarnings(t *testing.T) {
	t.Parallel()

	const (
		cloudAutoJoin  = "cloud auto-join"
		noSegmentPool  = "will not join any segment pool"
		defaultLANJoin = "targets the default serf_lan port"
	)

	cases := map[string]struct {
		hcl  string
		want string
	}{
		"cloud auto-join with the default serf_lan port": {
			hcl:  `segment = "alpha"` + "\n" + `retry_join = ["provider=aws tag_key=consul"]`,
			want: cloudAutoJoin,
		},
		"empty retry_join with the default serf_lan port": {
			hcl:  `segment = "alpha"`,
			want: noSegmentPool,
		},
		"empty retry_join with an overridden serf_lan port": {
			hcl:  `segment = "alpha"` + "\n" + `ports { serf_lan = 8303 }`,
			want: noSegmentPool,
		},
		"documented pattern: cloud auto-join with an overridden serf_lan port": {
			hcl: `segment = "alpha"` + "\n" + `ports { serf_lan = 8303 }` + "\n" +
				`retry_join = ["provider=aws tag_key=consul"]`,
		},
		"client without a segment": {
			hcl: ``,
		},
		"server": {
			hcl: `server = true` + "\n" + `retry_join = ["10.0.0.5"]`,
		},
		"explicit default serf_lan port reaches the servers' default pool": {
			hcl:  `segment = "alpha"` + "\n" + `retry_join = ["10.0.0.5:8301"]`,
			want: defaultLANJoin,
		},
		"explicit default serf_lan port with an overridden local port": {
			hcl: `segment = "alpha"` + "\n" + `ports { serf_lan = 8303 }` + "\n" +
				`retry_join = ["10.0.0.5:8301"]`,
			want: defaultLANJoin,
		},
		"several cloud auto-join entries warn once": {
			hcl: `segment = "alpha"` + "\n" +
				`retry_join = ["provider=aws tag_key=consul", "provider=gce tag_value=consul"]`,
			want: cloudAutoJoin,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result, err := loadSegmentClient(t, tc.hcl)
			require.NoError(t, err)

			if tc.want == "" {
				for _, w := range result.Warnings {
					require.NotContains(t, w, "segment")
				}
				return
			}
			var n int
			for _, w := range result.Warnings {
				if strings.Contains(w, tc.want) {
					n++
				}
			}
			require.Equal(t, 1, n, "want exactly one warning containing %q, got %q", tc.want, result.Warnings)
		})
	}
}
