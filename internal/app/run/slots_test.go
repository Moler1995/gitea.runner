// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"sync"
	"sync/atomic"
	"testing"

	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlotAllocatorReusesLowestFree(t *testing.T) {
	var slots slotAllocator
	require.Equal(t, 0, slots.acquire())
	require.Equal(t, 1, slots.acquire())
	require.Equal(t, 2, slots.acquire())
	slots.release(1)
	assert.Equal(t, 1, slots.acquire())
	assert.Equal(t, 3, slots.acquire())

	assert.Equal(t, "-3", toolCacheSuffix(config.ToolCacheModePerSlot, 3))
	assert.Empty(t, toolCacheSuffix(config.ToolCacheModeShared, 3))
}

func TestSlotAllocatorConcurrent(t *testing.T) {
	const capacity = 4
	var slots slotAllocator
	var holders [capacity]atomic.Int32
	sem := make(chan struct{}, capacity)
	var wg sync.WaitGroup
	for range 500 {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			slot := slots.acquire()
			defer slots.release(slot)
			if !assert.Less(t, slot, capacity) {
				return
			}
			assert.Equal(t, int32(1), holders[slot].Add(1), "slot %d held twice", slot)
			holders[slot].Add(-1)
		})
	}
	wg.Wait()
}
