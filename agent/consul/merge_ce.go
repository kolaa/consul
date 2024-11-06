// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"fmt"

	"github.com/hashicorp/serf/serf"
)

func (md *lanMergeDelegate) enterpriseNotifyMergeMember(m *serf.Member) error {

	// Segments are strictly isolated: the member's segment must match the
	// segment of the pool we are merging into. The check is deliberately
	// symmetric (unlike a check that only fires when the *member* carries a
	// non-empty tag) so that a default-pool member can never be absorbed by
	// a segment pool and vice versa. Without this, agents from one segment
	// leak into other segments' gossip rings, which shows up as duplicate
	// entries in "consul members" and endless leave/join flapping.
	if segment := m.Tags["segment"]; segment != md.segment {
		return fmt.Errorf("Member '%s' part of wrong segment '%s' (expected '%s')",
			m.Name, segment, md.segment)
	}

	if memberFIPS := m.Tags["fips"]; memberFIPS != "" {
		return fmt.Errorf("Member '%s' is FIPS Consul; FIPS Consul is only available in Consul Enterprise",
			m.Name)
	}
	if memberPartition := m.Tags["ap"]; memberPartition != "" {
		return fmt.Errorf("Member '%s' part of partition '%s'; Partitions are a Consul Enterprise feature",
			m.Name, memberPartition)
	}
	return nil
}
