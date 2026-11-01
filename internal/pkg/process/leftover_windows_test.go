// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func startSleeperIn(t *testing.T, dir, trackingID string) *exec.Cmd {
	t.Helper()
	ping := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	cmd := exec.Command(ping, "-n", "60", "127.0.0.1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RUNNER_TRACKING_ID="+trackingID)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func processCWD(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return processCWDHandle(h)
}

func awaitCWD(t *testing.T, pid int) string {
	t.Helper()
	var cwd string
	require.Eventually(t, func() bool {
		var err error
		cwd, err = processCWD(uint32(pid))
		return err == nil && cwd != ""
	}, 5*time.Second, 50*time.Millisecond, "process cwd should become readable")
	return cwd
}

func TestProcessCWDReadsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	cmd := startSleeperIn(t, dir, "leftover-test")
	cwd := awaitCWD(t, cmd.Process.Pid)
	require.True(t, dirContainsPath(dir, cwd), "cwd %q should be within %q", cwd, dir)
}

func TestKillProcessesWithPathsTracking(t *testing.T) {
	for _, trackingID := range []string{"leftover-test", "", "another-job"} {
		t.Run("tracking="+trackingID, func(t *testing.T) {
			dir := t.TempDir()
			cmd := startSleeperIn(t, dir, trackingID)
			awaitCWD(t, cmd.Process.Pid)
			killed, err := KillProcessesWithPaths(t.Context(), []string{dir}, "leftover-test")
			require.NoError(t, err)
			if trackingID == "leftover-test" {
				require.Equal(t, 1, killed)
				require.Eventually(t, func() bool {
					return !processAlive(cmd.Process.Pid)
				}, 5*time.Second, 50*time.Millisecond)
			} else {
				require.Zero(t, killed)
				require.True(t, processAlive(cmd.Process.Pid))
			}
		})
	}
}

func TestKillProcessesWithPathsSparesUnrelated(t *testing.T) {
	workspace := t.TempDir()
	elsewhere := t.TempDir()
	cmd := startSleeperIn(t, elsewhere, "leftover-test")
	awaitCWD(t, cmd.Process.Pid)

	_, err := KillProcessesWithPaths(t.Context(), []string{workspace}, "leftover-test")
	require.NoError(t, err)
	require.True(t, processAlive(cmd.Process.Pid), "process outside owned dirs must be spared")
}

func TestKillProcessesWithPathsCommandLine(t *testing.T) {
	workspace := t.TempDir()
	cmd, ready := newTestProcess(t, "child")
	cmd.Dir = t.TempDir()
	cmd.Args = append(cmd.Args, filepath.Join(workspace, "reference"))
	cmd.Env = append(cmd.Env, "RUNNER_TRACKING_ID=command-test")
	require.NoError(t, cmd.Start())
	ready()
	killed, err := KillProcessesWithPaths(t.Context(), []string{workspace}, "command-test")
	require.NoError(t, err)
	require.Equal(t, 1, killed)
	require.Eventually(t, func() bool {
		return !processAlive(cmd.Process.Pid)
	}, 5*time.Second, 50*time.Millisecond)
}

func TestKillProcessesWithPathsWOW64(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("needs a 64-bit runner")
	}
	ping := filepath.Join(os.Getenv("SystemRoot"), "SysWOW64", "ping.exe")
	if _, err := os.Stat(ping); err != nil {
		t.Skip("32-bit Windows tools unavailable")
	}
	for _, trackingID := range []string{"wow64-test", ""} {
		t.Run("tracking="+trackingID, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(ping, "-n", "60", "127.0.0.1")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "RUNNER_TRACKING_ID="+trackingID)
			require.NoError(t, cmd.Start())
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			})
			awaitCWD(t, cmd.Process.Pid)
			killed, err := KillProcessesWithPaths(t.Context(), []string{dir}, "wow64-test")
			require.NoError(t, err)
			if trackingID == "wow64-test" {
				require.Equal(t, 1, killed)
			} else {
				require.Zero(t, killed)
				require.True(t, processAlive(cmd.Process.Pid))
			}
		})
	}
}
