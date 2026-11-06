// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"slices"
	"strconv"
	"sync"

	"gitea.com/gitea/runner/internal/pkg/config"
)

// slotAllocator numbers running tasks with the lowest free slot, so at most capacity tasks stay within 0..capacity-1.
type slotAllocator struct {
	mu   sync.Mutex
	used []bool
}

func (a *slotAllocator) acquire() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	slot := slices.Index(a.used, false)
	if slot < 0 {
		slot = len(a.used)
		a.used = append(a.used, false)
	}
	a.used[slot] = true
	return slot
}

func (a *slotAllocator) release(slot int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used[slot] = false
}

func toolCacheSuffix(mode string, slot int) string {
	if mode != config.ToolCacheModePerSlot {
		return ""
	}
	return "-" + strconv.Itoa(slot)
}
