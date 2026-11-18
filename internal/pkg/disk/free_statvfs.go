// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build netbsd || solaris

package disk

import "golang.org/x/sys/unix"

// FreeBytes reports the space available to an unprivileged user on the volume holding path.
func FreeBytes(path string) (uint64, error) {
	var stat unix.Statvfs_t
	if err := unix.Statvfs(path, &stat); err != nil {
		return 0, err
	}
	// statvfs(2) counts blocks in f_frsize units, f_bsize is the preferred I/O size and differs on ZFS
	return availableBytes(stat.Bavail, stat.Frsize), nil
}
