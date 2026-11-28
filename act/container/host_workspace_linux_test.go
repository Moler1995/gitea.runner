// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestUseWorkspaceGivesConcurrentJobsOnePathAndHandsTheHostViewOn(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "workspaces")
	workspace := filepath.Join(root, "owner", "repo", "repo")
	require.ErrorContains(t, (&HostEnvironment{Root: dir}).UseWorkspace(root, "owner/.."), "unsafe")

	start := func(name string) *HostEnvironment {
		env := &HostEnvironment{Root: filepath.Join(dir, name), TrackingID: name}
		err := env.UseWorkspace(root, "owner/repo")
		if errors.Is(err, unix.EPERM) {
			t.Skip("needs CAP_SYS_ADMIN")
		}
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(env.HostPath(filepath.Join(env.Path, "name")), []byte(name), 0o644))
		return env
	}
	read := func(env *HostEnvironment) string {
		var out bytes.Buffer
		env.StdOut = &out
		require.NoError(t, env.Exec([]string{"cat", "name"}, map[string]string{"PATH": os.Getenv("PATH")}, "", "")(t.Context()))
		return out.String()
	}
	first, second := start("first"), start("second")
	assert.Equal(t, workspace, second.Path)
	assert.Equal(t, "first", read(first))
	assert.Equal(t, "second", read(second))
	shown, err := os.ReadFile(filepath.Join(workspace, "name"))
	require.NoError(t, err)
	assert.Equal(t, "first", string(shown))

	require.NoError(t, first.Remove()(t.Context()))
	assert.Equal(t, "second", read(second))
	assert.Eventually(t, func() bool {
		shown, _ := os.ReadFile(filepath.Join(workspace, "name"))
		return string(shown) == "second"
	}, 5*time.Second, 50*time.Millisecond)
	require.NoError(t, second.Remove()(t.Context()))
}
