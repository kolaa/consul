// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"testing"

	"github.com/hashicorp/memberlist"
	"github.com/hashicorp/serf/serf"
	"github.com/stretchr/testify/require"
)

type testConflictDelegate struct{}

func (testConflictDelegate) NotifyConflict(_, _ *memberlist.Node) {}

// serf.Create overwrites MemberlistConfig.Conflict with its own delegate, so
// the old "clear Conflict for segment clients" assignment was dead code: had
// it run, it would have hidden node name conflicts from the operator.
func TestEnterpriseModifyClientSerfConfigLAN_PreservesConflictDelegate(t *testing.T) {
	conf := serf.DefaultConfig()
	conf.Tags = map[string]string{"segment": "dmz"}
	delegate := &testConflictDelegate{}
	conf.MemberlistConfig.Conflict = delegate

	enterpriseModifyClientSerfConfigLAN(&Config{}, conf)

	require.Same(t, delegate, conf.MemberlistConfig.Conflict,
		"the hook must not touch MemberlistConfig.Conflict; serf.Create decides it")
}
