// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package consul

import (
	"fmt"

	"github.com/hashicorp/serf/serf"
)

func (md *lanMergeDelegate) enterpriseNotifyMergeMember(m *serf.Member) error {

	if segment := m.Tags["segment"]; segment != "" {
		if md.segment != segment {
			return fmt.Errorf("Member '%s' trying to connect to wrong segment '%s'", m.Name, md.segment)
		}
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
