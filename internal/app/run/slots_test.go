// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestStableHostPath(t *testing.T) {
	assert.Equal(t, filepath.Join("/p", "workspaces", "owner", "my.repo-1", "2"), stableHostPath("/p", "owner/my.repo-1", 2))
	for _, repository := range []string{"", "owner", "../repo", "owner/..", "owner/.", "owner/a/b", "owner/", "/repo", "owner/a\\b", "owner/repo."} {
		assert.Empty(t, stableHostPath("/p", repository, 0), repository)
	}
}

func TestCleanupStaleStableDirs(t *testing.T) {
	now := time.Date(2026, time.April, 29, 20, 0, 0, 0, time.UTC)
	root := t.TempDir()
	old := now.Add(-48 * time.Hour)
	stale := filepath.Join(root, "gone", "repo", "0")
	fresh := filepath.Join(root, "owner", "repo", "0")
	staleSibling := filepath.Join(root, "owner", "repo", "1")
	operatorData := filepath.Join(root, "other", "repo", "keep-me")
	for _, path := range []string{stale, fresh, staleSibling, operatorData} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	for _, path := range []string{stale, staleSibling, operatorData} {
		require.NoError(t, os.Chtimes(path, old, old))
	}
	require.NoError(t, os.Chtimes(fresh, now, now))
	strayFiles := []string{filepath.Join(root, "file"), filepath.Join(root, "other", "file")}
	for _, path := range strayFiles {
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}

	r := &Runner{cfg: &config.Config{Runner: config.Runner{WorkdirCleanupAge: 24 * time.Hour}}, now: func() time.Time { return now }}
	r.cleanupStaleStableDirs(t.Context(), root)

	assert.NoDirExists(t, filepath.Join(root, "gone"))
	assert.DirExists(t, fresh)
	assert.NoDirExists(t, staleSibling)
	assert.DirExists(t, operatorData)
	for _, path := range strayFiles {
		assert.FileExists(t, path)
	}
	r.cleanupStaleStableDirs(t.Context(), filepath.Join(root, "missing"))
}
