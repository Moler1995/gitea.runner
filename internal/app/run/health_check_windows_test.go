// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExecuteHealthCheckPathWithSpace(t *testing.T) {
	whoami, err := os.ReadFile(filepath.Join(os.Getenv("SystemRoot"), "System32", "whoami.exe"))
	require.NoError(t, err)
	script := filepath.Join(t.TempDir(), "health check", "probe.exe")
	require.NoError(t, os.MkdirAll(filepath.Dir(script), 0o755))
	require.NoError(t, os.WriteFile(script, whoami, 0o755))
	require.NoError(t, executeHealthCheck(t.Context(), script, time.Minute, nil))
}
