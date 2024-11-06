// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package config

import (
	"fmt"
	"regexp"

	"github.com/hashicorp/consul/ipaddr"
)

var validSegmentName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_\-]*[A-Za-z0-9]$`)

func (b *builder) validateSegments(rt RuntimeConfig) error {

	if len(rt.Segments) > rt.SegmentLimit {
		return fmt.Errorf("Cannot exceed network segment limit of %d", rt.SegmentLimit)
	}

	takenPorts := make(map[int]string, len(rt.Segments))

	for _, segment := range rt.Segments {

		if segment.Name == "" {
			return fmt.Errorf("Segment name cannot be blank")
		}

		if !validSegmentName.MatchString(segment.Name) {
			return fmt.Errorf("Segment name must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character")
		}

		if len(segment.Name) > rt.SegmentNameLimit {
			return fmt.Errorf("Segment name %q exceeds maximum length of %d", segment.Name, rt.SegmentNameLimit)
		}

		previous, ok := takenPorts[segment.Bind.Port]
		if ok {
			return fmt.Errorf("Segment %q port %d overlaps with segment %q", segment.Name, segment.Bind.Port, previous)
		}

		takenPorts[segment.Bind.Port] = segment.Name

		if ipaddr.IsAny(segment.Advertise.IP) {
			return fmt.Errorf("Advertise WAN address for segment &%q cannot be 0.0.0.0, :: or [::]", segment.Name)
		}

		if segment.Bind.Port <= 0 {
			return fmt.Errorf("Port for segment %q cannot be 0", segment.Name)
		}
	}

	return nil
}
