// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build unix

package disk

func availableBytes[Count, Size ~int32 | ~int64 | ~uint32 | ~uint64](count Count, size Size) uint64 {
	return uint64(max(count, 0)) * uint64(size) // signed BSD counts go negative once root fills the reserved blocks
}
