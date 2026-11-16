// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"bytes"
	"errors"
	"log"
	"testing"

	discover "github.com/hashicorp/go-discover"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/sdk/testutil"
)

func TestAgentRetryNewDiscover(t *testing.T) {
	d, err := newDiscover()
	require.NoError(t, err)
	expected := []string{
		"aliyun", "aws", "azure", "digitalocean", "gce", "k8s", "linode",
		"mdns", "os", "packet", "scaleway", "softlayer", "srv", "tencentcloud",
		"triton", "vsphere",
	}
	require.Equal(t, expected, d.Names())
}

func TestAgentRetryJoinAddrs(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	d, err := newDiscover()
	require.NoError(t, err)

	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{"handles nil", nil, []string{}},
		{"handles empty input", []string{}, []string{}},
		{"handles one element",
			[]string{"192.168.0.12"},
			[]string{"192.168.0.12"},
		},
		{"handles two elements",
			[]string{"192.168.0.12", "192.168.0.13"},
			[]string{"192.168.0.12", "192.168.0.13"},
		},
		{"tries to resolve aws things, which fails but that is fine",
			[]string{"192.168.0.12", "provider=aws region=eu-west-1 tag_key=consul tag_value=tag access_key_id=a secret_access_key=a"},
			[]string{"192.168.0.12"},
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := testutil.LoggerWithOutput(t, &buf)

			output := retryJoinAddrs(d, retryJoinSerfVariant, "LAN", test.input, logger)
			bufout := buf.String()
			require.Equal(t, test.expected, output, bufout)
			if i == 4 {
				require.Contains(t, bufout, `Using provider "aws"`)
			}
		})
	}
	t.Run("handles nil discover", func(t *testing.T) {
		require.Equal(t, []string{}, retryJoinAddrs(nil, retryJoinSerfVariant, "LAN", []string{"a"}, nil))
	})
}

// fakeProvider is a go-discover provider that returns fixed addresses or a
// fixed error without touching the network.
type fakeProvider struct {
	addrs []string
	err   error
}

func (p fakeProvider) Addrs(map[string]string, *log.Logger) ([]string, error) {
	return p.addrs, p.err
}

func (fakeProvider) Help() string { return "fake" }

// Segment pools rely on retry_join resolving discover expressions before
// JoinLAN: the server's per-segment port expansion only handles addresses.
func TestAgentRetryJoinAddrs_FakeProvider(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		provider fakeProvider
		expected []string
		wantLog  string
	}{
		"resolved addresses replace the expression": {
			provider: fakeProvider{addrs: []string{"10.0.0.9", "10.0.0.10"}},
			expected: []string{"192.168.0.12", "10.0.0.9", "10.0.0.10"},
		},
		"a failing provider is dropped": {
			provider: fakeProvider{err: errors.New("boom")},
			expected: []string{"192.168.0.12"},
			wantLog:  "Cannot discover address",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			d, err := discover.New(discover.WithProviders(map[string]discover.Provider{"fake": tc.provider}))
			require.NoError(t, err)

			// retryJoinAddrs needs a non-nil logger whenever it resolves an
			// expression.
			var buf bytes.Buffer
			logger := testutil.LoggerWithOutput(t, &buf)

			output := retryJoinAddrs(d, retryJoinSerfVariant, "LAN", []string{"192.168.0.12", "provider=fake"}, logger)
			require.Equal(t, tc.expected, output, buf.String())
			for _, addr := range output {
				require.NotContains(t, addr, "provider=")
			}
			if tc.wantLog != "" {
				require.Contains(t, buf.String(), tc.wantLog)
			}
		})
	}
}
