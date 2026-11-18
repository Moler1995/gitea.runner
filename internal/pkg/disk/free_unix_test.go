// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build unix

package disk

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFreeBytes(t *testing.T) {
	free, err := FreeBytes(t.TempDir())
	require.NoError(t, err)
	assert.Positive(t, free)

	assert.Equal(t, uint64(1536), availableBytes(int64(3), uint32(512)))
	assert.Zero(t, availableBytes(int64(-3), uint32(512)))
}
