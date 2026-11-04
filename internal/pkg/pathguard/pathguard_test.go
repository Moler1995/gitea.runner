// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pathguard

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsFilesystemRoot(t *testing.T) {
	assert.True(t, IsFilesystemRoot(string(filepath.Separator)))
	assert.False(t, IsFilesystemRoot(filepath.Join(string(filepath.Separator), "workspace")))
	assert.False(t, IsFilesystemRoot(""))
	if runtime.GOOS == "windows" {
		assert.True(t, IsFilesystemRoot(`C:\`))
		assert.False(t, IsFilesystemRoot(`C:\work`))
	}
}

func TestCheckParent(t *testing.T) {
	sep := string(filepath.Separator)
	require.NoError(t, CheckParent("host.workdir_parent", "x", filepath.Join(sep, "x")))
	require.EqualError(t, CheckParent("container.workdir_parent", "", sep),
		"container.workdir_parent is empty; set it to a dedicated directory the runner may fill and delete")
	require.EqualError(t, CheckParent("host.workdir_parent", sep, sep),
		`host.workdir_parent "`+sep+`" is the filesystem root; set it to a dedicated directory the runner may fill and delete`)
	// A relative container parent is resolved under "/", so "." is the root too.
	require.ErrorContains(t, CheckParent("container.workdir_parent", ".", sep), "filesystem root")
}

func TestCheckRepository(t *testing.T) {
	for _, ok := range []string{"owner/repo", "owner/re..po", "a/b/c"} {
		require.NoError(t, CheckRepository(ok), ok)
	}
	for repo, want := range map[string]string{
		"":            "no repository",
		"/":           "absolute",
		"/etc/passwd": "absolute",
		"../x":        "'..' segment",
		"owner/../..": "'..' segment",
		"owner/..":    "'..' segment",
		".":           "workdir parent itself",
		"./":          "workdir parent itself",
	} {
		require.ErrorContains(t, CheckRepository(repo), want, repo)
	}
}

func TestValidateWorkdir(t *testing.T) {
	parent := filepath.Join(string(filepath.Separator), "ws")
	require.NoError(t, ValidateWorkdir(parent, filepath.Join(parent, "o", "r")))
	for _, bad := range []string{parent, filepath.Join(parent, ".."), filepath.Join(parent, "..", "x"), filepath.Join(string(filepath.Separator), "wsx"), ""} {
		require.ErrorContains(t, ValidateWorkdir(parent, bad), "not strictly inside", bad)
	}
	require.Error(t, ValidateWorkdir("", filepath.Join(parent, "o")))
}

func TestCheckRemovable(t *testing.T) {
	sep := string(filepath.Separator)
	owner := filepath.Join(sep, "ws")
	home := filepath.Join(sep, "home", "me")
	require.NoError(t, CheckRemovable(filepath.Join(owner, "o", "r"), owner, home))
	require.NoError(t, CheckRemovable(filepath.Join(sep, "tmp", "misc"), "", home))
	require.ErrorContains(t, CheckRemovable("", owner, home), "empty path")
	require.ErrorContains(t, CheckRemovable(sep, owner, home), "filesystem root")
	require.ErrorContains(t, CheckRemovable(sep, "", home), "filesystem root")
	require.ErrorContains(t, CheckRemovable(home+sep, "", home), "home directory")
	require.ErrorContains(t, CheckRemovable(owner, owner, home), "not strictly inside")
	require.ErrorContains(t, CheckRemovable(filepath.Join(sep, "bait"), owner, home), "not strictly inside")
	require.ErrorContains(t, CheckRemovable(filepath.Join(owner, "..", "bait"), owner, home), "not strictly inside")
}
