// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"path/filepath"
	"testing"

	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/stretchr/testify/require"
)

// newHostTestConfig returns a default config whose container and host workdir parents are
// separate directories under t.TempDir(), so a host-mode test can never reach a real path.
func newHostTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	cfg.Container.WorkdirParent = t.TempDir()
	cfg.Host.WorkdirParent = t.TempDir()
	return cfg
}

func TestHostTestConfigPassesWorkdirGuard(t *testing.T) {
	cfg := newHostTestConfig(t)
	require.NoError(t, ValidateWorkdirConfig(cfg))
	require.NotEqual(t, cfg.Container.WorkdirParent, cfg.Host.WorkdirParent)
}

// The config of the 2026-10-30 incident: a hand-built host config with only the host parent set.
func TestValidateWorkdirConfigRefusesIncidentConfig(t *testing.T) {
	cfg := &config.Config{Host: config.Host{WorkdirParent: t.TempDir()}}
	require.EqualError(t, ValidateWorkdirConfig(cfg),
		"container.workdir_parent is empty; set it to a dedicated directory the runner may fill and delete")
}

// A relative host.workdir_parent is judged by, and rewritten to, the absolute path it names.
// No job runs; t.Chdir only moves the test process.
func TestHostWorkdirParentIsResolvedToAbsolute(t *testing.T) {
	fakeHome := func(t *testing.T) string {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		return home
	}
	root := func(*testing.T) string { return string(filepath.Separator) }
	scratch := func(t *testing.T) string { return t.TempDir() }
	for name, tc := range map[string]struct {
		cwd   func(t *testing.T) string
		value string
		want  string // error substring, or "" for accepted
	}{
		"dot in a scratch dir": {cwd: scratch, value: "."},
		"relative subdir":      {cwd: scratch, value: "cache/act"},
		"dot at the root":      {cwd: root, value: ".", want: "filesystem root"},
		"dotdot to the root":   {cwd: root, value: "..", want: "filesystem root"},
		"dot in the home dir":  {cwd: fakeHome, value: ".", want: "home directory"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(tc.cwd(t))
			cfg := &config.Config{Container: config.Container{WorkdirParent: "workspace"}, Host: config.Host{WorkdirParent: tc.value}}
			err := ResolveWorkdirConfig(cfg)
			if tc.want != "" {
				require.ErrorContains(t, err, "host.workdir_parent")
				require.ErrorContains(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			require.True(t, filepath.IsAbs(cfg.Host.WorkdirParent), cfg.Host.WorkdirParent)
		})
	}
}

func TestValidateWorkdirConfigRefusesHomeAsContainerParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := &config.Config{Container: config.Container{WorkdirParent: home}, Host: config.Host{WorkdirParent: t.TempDir()}}
	require.ErrorContains(t, ValidateWorkdirConfig(cfg), "container.workdir_parent")
}

func TestValidateWorkdirConfig(t *testing.T) {
	sep := string(filepath.Separator)
	for name, tc := range map[string]struct {
		container, host string
		want            string
	}{
		"both set":              {container: "workspace", host: sep + "h", want: ""},
		"container empty":       {container: "", host: sep + "h", want: "container.workdir_parent is empty"},
		"container root":        {container: sep, host: sep + "h", want: "container.workdir_parent"},
		"container dot":         {container: ".", host: sep + "h", want: "container.workdir_parent"},
		"container slash-dot":   {container: "/.", host: sep + "h", want: "container.workdir_parent"},
		"host empty":            {container: "workspace", host: "", want: "host.workdir_parent is empty"},
		"host root":             {container: "workspace", host: sep, want: "host.workdir_parent"},
		"host root with dotdot": {container: "workspace", host: sep + "x" + sep + "..", want: "host.workdir_parent"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{Container: config.Container{WorkdirParent: tc.container}, Host: config.Host{WorkdirParent: tc.host}}
			err := ValidateWorkdirConfig(cfg)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}
