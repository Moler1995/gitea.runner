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
