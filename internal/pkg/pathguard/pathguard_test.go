// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pathguard

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	home := filepath.Join(sep, "home", "me")
	require.NoError(t, CheckParent("host.workdir_parent", "x", filepath.Join(sep, "x"), home))
	require.EqualError(t, CheckParent("container.workdir_parent", "", sep, home),
		"container.workdir_parent is empty; set it to a dedicated directory the runner may fill and delete")
	require.EqualError(t, CheckParent("host.workdir_parent", sep, sep, home),
		"host.workdir_parent "+strconv.Quote(sep)+" is the filesystem root; set it to a dedicated directory the runner may fill and delete")
	// A relative container parent is resolved under "/", so "." is the root too.
	require.ErrorContains(t, CheckParent("container.workdir_parent", ".", sep, home), "filesystem root")
	require.ErrorContains(t, CheckParent("host.workdir_parent", ".", home+sep, home), "is the home directory")
	require.NoError(t, CheckParent("host.workdir_parent", "x", home, ""), "an unknown home skips the home check")
}

// Every path below is made inside t.TempDir(); nothing outside it is named or touched.
func TestCheckRemovableResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	owner := filepath.Join(base, "root")
	bait := filepath.Join(base, "bait")
	fakeHome := filepath.Join(base, "home")
	for _, dir := range []string{filepath.Join(owner, "inside"), bait, fakeHome} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	for link, target := range map[string]string{
		filepath.Join(owner, "to-bait"):      bait,
		filepath.Join(owner, "to-home"):      fakeHome,
		filepath.Join(owner, "to-owner"):     owner,
		filepath.Join(owner, "to-bait-deep"): filepath.Join(bait, "gone"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("cannot create symlinks: %v", err)
		}
	}
	require.NoError(t, CheckRemovable(filepath.Join(owner, "inside"), owner, fakeHome))
	require.NoError(t, CheckRemovable(filepath.Join(owner, "not-yet", "made"), owner, fakeHome), "a missing path resolves through its existing ancestor")

	require.ErrorContains(t, CheckRemovable(filepath.Join(owner, "to-bait"), owner, fakeHome), "not strictly inside")
	require.ErrorContains(t, CheckRemovable(filepath.Join(owner, "to-bait", "x"), owner, fakeHome), "not strictly inside")
	require.ErrorContains(t, CheckRemovable(filepath.Join(owner, "to-bait-deep"), owner, fakeHome), "not strictly inside")
	require.ErrorContains(t, CheckRemovable(filepath.Join(owner, "to-owner"), owner, fakeHome), "not strictly inside")
	require.ErrorContains(t, CheckRemovable(filepath.Join(owner, "to-home"), "", fakeHome), "home directory")
	// The owner may itself be reached through a link: the same directory, so still inside.
	ownerLink := filepath.Join(base, "owner-link")
	require.NoError(t, os.Symlink(owner, ownerLink))
	require.NoError(t, CheckRemovable(filepath.Join(owner, "inside"), ownerLink, fakeHome))
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
