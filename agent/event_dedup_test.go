// Copyright IBM Corp. 2024, 2026
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEventDedup_FirstSeen(t *testing.T) {
	t.Parallel()

	d := NewEventDedup(4)

	require.True(t, d.FirstSeen("a"), "first observation wins")
	require.False(t, d.FirstSeen("a"), "second observation is a duplicate")
	require.True(t, d.FirstSeen("b"), "distinct IDs are independent")
	require.True(t, d.FirstSeen(""), "empty IDs are never deduplicated")
	require.True(t, d.FirstSeen(""), "empty IDs stay non-deduplicated")
}

func TestEventDedup_RingEviction(t *testing.T) {
	t.Parallel()

	d := NewEventDedup(2)
	require.True(t, d.FirstSeen("a"))
	require.True(t, d.FirstSeen("b"))
	require.False(t, d.FirstSeen("a"), "still inside the ring")

	// "c" evicts "a" from a size-2 ring.
	require.True(t, d.FirstSeen("c"))
	require.True(t, d.FirstSeen("a"), "evicted ID is forgotten")
	require.False(t, d.FirstSeen("c"))
}

func TestEventDedup_ZeroSize(t *testing.T) {
	t.Parallel()

	d := NewEventDedup(0)
	require.True(t, d.FirstSeen("a"))
	require.True(t, d.FirstSeen("a"), "a zero-size dedup never drops events")
}

func TestEventDedup_ConcurrentFirstSeen(t *testing.T) {
	t.Parallel()

	const (
		workers  = 8
		idCount  = 50
		dedupCap = 256
	)
	d := NewEventDedup(dedupCap)

	var wins atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < idCount; j++ {
				id := string(rune('a'+j%26)) + string(rune('0'+j%10))
				if d.FirstSeen(id) {
					wins.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	// Every distinct ID must win exactly once across all goroutines.
	require.Equal(t, int64(idCount), wins.Load())
}
