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

	"github.com/hashicorp/consul/agent/consul"
	"github.com/hashicorp/consul/ipaddr"
)

// Single-character names are allowed; '_' is allowed in the middle, which is
// what the error message now says.
var validSegmentName = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_-]*[A-Za-z0-9])?$`)

// "local" and "remote" collide with serf/local.snapshot (LAN) and
// serf/remote.snapshot (WAN) because segment snapshots are named
// serf/<lower(name)>.snapshot. The Windows device names are reserved too:
// on Windows serf/con.snapshot opens the console device, not a file, so the
// segment's rejoin state would be lost.
var reservedSegmentNames = func() map[string]struct{} {
	names := map[string]struct{}{
		"local": {}, "remote": {},
		"con": {}, "prn": {}, "aux": {}, "nul": {},
	}
	for i := 1; i <= 9; i++ {
		names["com"+strconv.Itoa(i)] = struct{}{}
		names["lpt"+strconv.Itoa(i)] = struct{}{}
	}
	return names
}()

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

// segmentPortsOnly reports whether addrs is non-empty and every entry is a
// host:port whose port belongs to a segment. Bare hosts and cloud auto-join
// expressions reach the default LAN pool, so any of them makes it false.
func segmentPortsOnly(addrs []string, segmentPorts map[int]string) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		if consul.IsCloudAutoJoin(addr) {
			return false
		}
		_, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return false
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return false
		}
		if _, ok := segmentPorts[port]; !ok {
			return false
		}
	}
	return true
}

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
	// Serf binds TCP and UDP, so a client listener on either protocol
	// collides. Unix sockets cannot; DNS lists one address per protocol, so
	// keep one entry per IP:port to report a collision once.
	clientAddrs := []struct {
		owner string
		addrs []net.Addr
	}{
		{"HTTP", rt.HTTPAddrs}, {"HTTPS", rt.HTTPSAddrs},
		{"gRPC", rt.GRPCAddrs}, {"gRPC TLS", rt.GRPCTLSAddrs}, {"DNS", rt.DNSAddrs},
	}
	for _, c := range clientAddrs {
		seen := make(map[string]struct{}, len(c.addrs))
		for _, a := range c.addrs {
			var addr *net.TCPAddr
			switch a := a.(type) {
			case *net.TCPAddr:
				addr = a
			case *net.UDPAddr:
				addr = &net.TCPAddr{IP: a.IP, Port: a.Port, Zone: a.Zone}
			default:
				continue
			}
			if _, dup := seen[addr.String()]; dup {
				continue
			}
			seen[addr.String()] = struct{}{}
			taken = append(taken, bound{c.owner, addr})
		}
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
		} else if !isAny(rt.RPCBindAddr.IP) && !rt.RPCBindAddr.IP.Equal(seg.Advertise.IP) &&
			(rt.RPCAdvertiseAddr == nil || !rt.RPCAdvertiseAddr.IP.Equal(seg.Advertise.IP)) {
			// Segment clients dial the segment advertise IP on the server
			// RPC port, which only the main RPC listener can answer. The RPC
			// advertise IP is exempt: default-pool clients already dial it,
			// so it reaches that listener (directly or through NAT).
			b.warn("segment %q: clients dial %s for RPC, but the RPC listener binds %s; "+
				"set rpc_listener = true or bind the server RPC to that IP or a wildcard",
				seg.Name, &net.TCPAddr{IP: seg.Advertise.IP, Port: rt.ServerPort}, rt.RPCBindAddr)
		}
	}

	// JoinLAN routes a retry_join entry with a segment port only to that
	// segment's pool, so a server whose entries all name segment ports never
	// joins the default LAN pool, and the flooder (which starts from the
	// default pool) cannot repair that.
	if rt.ServerMode && segmentPortsOnly(rt.RetryJoinLAN, ports) {
		b.warn("every retry_join entry targets a segment port, so this server will not join the " +
			"default LAN pool: add the servers' serf_lan address (or a bare host) to retry_join")
	}

	// Servers gossip the endpoint of every segment that is NOT reachable at
	// the LAN address as an sl_ tag on the default LAN pool, and serf refuses
	// to start once the encoded tags pass its limit. The check keeps
	// SerfTagHeadroom free below the limit so a future Consul release can add
	// built-in tags without every server refusing to start after an upgrade.
	if rt.ServerMode {
		lanAdvertise := rt.SerfAdvertiseAddrLAN
		if lanAdvertise == nil {
			lanAdvertise = rt.SerfBindAddrLAN
		}
		need := 0
		for _, seg := range rt.Segments {
			if lanAdvertise != nil && seg.Advertise.IP.Equal(lanAdvertise.IP) {
				continue // reachable through the LAN-IP fallback: no tag published
			}
			need += consul.SegmentTagSize(seg.Name, seg.Advertise.IP.String(), seg.Advertise.Port)
		}
		if used := consul.SerfBaseTagReserve + len(rt.Datacenter) + need; used > consul.SerfTagLimit-consul.SerfTagHeadroom {
			fail("%d segments need %d bytes of serf tags; the limit with headroom is %d bytes (%d reserved for other server tags, %d kept free for future releases)",
				len(rt.Segments), need, consul.SerfTagLimit-consul.SerfTagHeadroom,
				consul.SerfBaseTagReserve+len(rt.Datacenter), consul.SerfTagHeadroom)
		}
	}
	return errors.Join(errs...)
}
