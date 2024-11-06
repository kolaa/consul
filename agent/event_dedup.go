// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package agent

import "sync"

// EventDedup remembers the last N keys so a user event that is delivered
// more than once — e.g. once per serf pool a server belongs to, all sharing
// one event channel — is only processed a single time. Safe for concurrent
// use.
type EventDedup struct {
	mu   sync.Mutex
	seen map[string]struct{}
	ring []string
	next int
}

// NewEventDedup returns a dedup that remembers up to size IDs.
func NewEventDedup(size int) *EventDedup {
	if size < 0 {
		size = 0
	}
	return &EventDedup{
		seen: make(map[string]struct{}, size),
		ring: make([]string, size),
	}
}

// FirstSeen returns true the first time key is observed. An empty key is
// never deduplicated, and a zero-size dedup never drops events.
func (d *EventDedup) FirstSeen(id string) bool {
	if d == nil || id == "" {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.ring) == 0 {
		return true
	}
	if _, dup := d.seen[id]; dup {
		return false
	}
	if old := d.ring[d.next]; old != "" {
		delete(d.seen, old)
	}
	d.ring[d.next] = id
	d.seen[id] = struct{}{}
	d.next = (d.next + 1) % len(d.ring)
	return true
}
