// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !consulent

package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/hashicorp/consul/ipaddr"
)

// Single-character names are allowed; '_' is allowed in the middle, which is
// what the error message now says.
var validSegmentName = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_-]*[A-Za-z0-9])?$`)

// "local" and "remote" collide with serf/local.snapshot (LAN) and
// serf/remote.snapshot (WAN) because segment snapshots are named
// serf/<lower(name)>.snapshot.
var reservedSegmentNames = map[string]struct{}{"local": {}, "remote": {}}

// ValidateSegmentName checks a segment name for blankness, length, syntax and
// the reserved words shared by a server's segments = [...] list and a
// client's segment = "..." option.
func ValidateSegmentName(name string, limit int) error {
	switch {
	case name == "":
		return errors.New("segment name cannot be blank")
	case len(name) > limit:
		return fmt.Errorf("segment name %q exceeds maximum length of %d", name, limit)
	case !validSegmentName.MatchString(name):
		return fmt.Errorf("segment name %q must start with a letter, end with a letter or digit, "+
			"and contain only letters, digits, '-' or '_'", name)
	}
	if _, ok := reservedSegmentNames[strings.ToLower(name)]; ok {
		return fmt.Errorf("segment name %q is reserved", name)
	}
	return nil
}

func isAny(ip net.IP) bool { return ip == nil || ip.IsUnspecified() }

// BindConflict reports whether two listeners would fight over the same
// socket: same port and either the same IP or one of them is a wildcard.
// Serf binds TCP and UDP on the same port, so protocol is not compared.
func BindConflict(a, b *net.TCPAddr) bool {
	if a == nil || b == nil || a.Port != b.Port {
		return false
	}
	return a.IP.Equal(b.IP) || isAny(a.IP) || isAny(b.IP)
}

// validateSegments collects every problem with the segment configuration in
// a single pass so the operator sees all of them at once, instead of fixing
// them one restart at a time. The builder guarantees non-nil Bind/Advertise
// addresses (port <= 0 is rejected before this runs).
func (b *builder) validateSegments(rt RuntimeConfig) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	// `segment` names the single segment a client belongs to; servers are
	// members of every segment in `segments = [...]`, so reject the option
	// where it can only be silently ignored.
	if rt.SegmentName != "" {
		if rt.ServerMode {
			fail("segment = %q is a client-only option; servers join every segment in segments = [...]",
				rt.SegmentName)
		}
		if err := ValidateSegmentName(rt.SegmentName, rt.SegmentNameLimit); err != nil {
			errs = append(errs, err)
		}
	}
	if len(rt.Segments) == 0 {
		return errors.Join(errs...)
	}
	if !rt.ServerMode {
		fail("segments = [...] is a server-only option")
	}
	if len(rt.Segments) > rt.SegmentLimit {
		fail("cannot exceed network segment limit of %d", rt.SegmentLimit)
	}

	type bound struct {
		owner string
		addr  *net.TCPAddr
	}
	taken := []bound{{"serf_lan", rt.SerfBindAddrLAN}, {"server (RPC)", rt.RPCBindAddr}}
	if rt.SerfBindAddrWAN != nil {
		taken = append(taken, bound{"serf_wan", rt.SerfBindAddrWAN})
	}
	names := make(map[string]string, len(rt.Segments)) // lower(name) -> name
	ports := make(map[int]string, len(rt.Segments))    // JoinLAN routes explicit ports by number

	for _, seg := range rt.Segments {
		if err := ValidateSegmentName(seg.Name, rt.SegmentNameLimit); err != nil {
			errs = append(errs, err)
			continue
		}
		key := strings.ToLower(seg.Name)
		if prev, dup := names[key]; dup {
			fail("segment %q duplicates %q (case-insensitive: both use serf/%s.snapshot)",
				seg.Name, prev, key)
		}
		names[key] = seg.Name

		if prev, dup := ports[seg.Bind.Port]; dup {
			fail("segment %q port %d is already used by segment %q", seg.Name, seg.Bind.Port, prev)
		}
		if seg.Bind.Port == rt.SerfPortLAN {
			fail("segment %q port %d equals serf_lan; explicit host:port joins would be ambiguous",
				seg.Name, seg.Bind.Port)
		}
		ports[seg.Bind.Port] = seg.Name

		if ipaddr.IsAny(seg.Advertise.IP) {
			fail("advertise address for segment %q cannot be 0.0.0.0, :: or [::]", seg.Name)
		}
		for _, t := range taken {
			if BindConflict(seg.Bind, t.addr) {
				fail("segment %q bind %s conflicts with %s bind %s", seg.Name, seg.Bind, t.owner, t.addr)
			}
		}
		taken = append(taken, bound{"segment " + strconv.Quote(seg.Name), seg.Bind})
		if seg.RPCListener {
			// rpc_listener binds <segment bind IP>:server_port, so it must
			// clear the main RPC listener, every pool bound so far and this
			// segment's own serf bind.
			rpc := &net.TCPAddr{IP: seg.Bind.IP, Port: rt.ServerPort}
			if BindConflict(rpc, rt.RPCBindAddr) {
				fail("segment %q: rpc_listener would bind %s, already used by the main RPC listener %s",
					seg.Name, rpc, rt.RPCBindAddr)
			}
			for _, t := range taken {
				if t.owner == "server (RPC)" {
					continue // already reported with the main-RPC wording above
				}
				if BindConflict(rpc, t.addr) {
					fail("segment %q: rpc_listener would bind %s, already used by %s bind %s",
						seg.Name, rpc, t.owner, t.addr)
				}
			}
			taken = append(taken, bound{fmt.Sprintf("segment %q rpc_listener", seg.Name), rpc})
		}
	}
	return errors.Join(errs...)
}
