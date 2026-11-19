// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runHookTestHelper() int {
	var request hookRequest
	if err := json.UnmarshalRead(os.Stdin, &request); err != nil {
		return 2
	}
	mode := os.Getenv("GITEA_RUNNER_TEST_HOOK_MODE")
	if mode == "missing_response" {
		return 0
	}
	if mode == "cancel" && request.Command == "run_command_step" {
		if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
			return 2
		}
		<-time.After(time.Hour)
		return 0
	}
	result := map[string]any{}
	state := map[string]int{}
	if err := json.Unmarshal(request.State, &state); err != nil {
		return 2
	}
	state["calls"]++
	switch request.Command {
	case "prepare_job":
		result = map[string]any{"paths": HookPaths{Workspace: "/job/work", Act: "/job/act", Temp: "/job/tmp", ToolCache: "/job/tools"}, "runner": map[string]string{"os": "Linux", "arch": "X64"}, "container": HookContainerInfo{ID: request.JobID}}
	case "run_command_step", "run_container_step":
		result["exitCode"] = 0
		data, err := json.Marshal(request.Args)
		if err != nil {
			return 2
		}
		var args HookContainerStep
		if err := json.Unmarshal(data, &args); err != nil {
			return 2
		}
		if mode == "large_request" && (len(args.EntryPointArgs) != 1 || args.EntryPointArgs[0] != strings.Repeat("x", 128<<10)) {
			return 2
		}
		if mode == "build_context" {
			file, err := os.Open(args.BuildContextArchive)
			if err != nil {
				return 2
			}
			defer file.Close()
			reader := tar.NewReader(file)
			found := false
			for {
				header, err := reader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return 2
				}
				if header.Name == args.Dockerfile {
					body, err := io.ReadAll(reader)
					if err != nil || string(body) != "FROM scratch\n" {
						return 2
					}
					found = true
				}
			}
			if !found {
				return 2
			}
		}
		if mode == "command_failure" {
			result["exitCode"] = 17
		}
		if mode == "missing_exit" {
			delete(result, "exitCode")
		}
	case "copy_to_job":
		data, err := json.Marshal(request.Args)
		if err != nil {
			return 2
		}
		var args map[string]string
		if err := json.Unmarshal(data, &args); err != nil {
			return 2
		}
		file, err := os.Open(args["archiveFile"])
		if err != nil {
			return 2
		}
		reader := tar.NewReader(file)
		header, err := reader.Next()
		body, readErr := io.ReadAll(reader)
		file.Close()
		if err != nil || readErr != nil || header.Name != "script.sh" || string(body) != "echo hello" {
			return 2
		}
	case "copy_from_job":
		data, err := json.Marshal(request.Args)
		if err != nil {
			return 2
		}
		var args map[string]string
		if err := json.Unmarshal(data, &args); err != nil {
			return 2
		}
		file, err := os.Create(args["archiveFile"])
		if err != nil {
			return 2
		}
		writer := tar.NewWriter(file)
		if err := writer.WriteHeader(&tar.Header{Name: "outputcmd.txt", Size: 12, Mode: 0o600}); err != nil {
			return 2
		}
		if _, err := io.WriteString(writer, "answer=hello"); err != nil {
			return 2
		}
		if err := errors.Join(writer.Close(), file.Close()); err != nil {
			return 2
		}
	}
	response := map[string]any{"protocolVersion": 1, "requestId": request.RequestID, "state": state, "result": result}
	switch mode {
	case "wrong_request":
		response["requestId"] = "stale"
	case "wrong_version":
		response["protocolVersion"] = 9
	case "invalid_state":
		response["state"] = []string{"invalid"}
	case "null_state":
		response["state"] = nil
	case "large_state":
		response["state"] = map[string]string{"data": strings.Repeat("x", 64<<10)}
	case "policy_denied":
		response["error"] = map[string]string{"code": "policy_denied", "message": "not approved"}
	case "transfer_failure":
		response["error"] = map[string]string{"code": "transfer_failed", "message": "remote connection was lost"}
	case "not_found":
		response["error"] = map[string]string{"code": "not_found", "message": "no such source"}
	}
	data, err := json.Marshal(response)
	if err != nil {
		return 2
	}
	if mode == "invalid_json" {
		data = []byte("{")
	}
	if mode == "oversized" {
		data = bytes.Repeat([]byte(" "), hookResponseLimit+1)
	}
	if err := os.WriteFile(request.ResponseFile, data, 0o600); err != nil {
		return 2
	}
	if mode == "process_failure" {
		return 1
	}
	return 0
}

func testHookEnvironment(t *testing.T) *HookEnvironment {
	t.Helper()
	t.Setenv("GITEA_RUNNER_TEST_CONTAINER_HOOK", "1")
	program, err := os.Executable()
	require.NoError(t, err)
	environment, err := NewHookEnvironment(HookConfig{Enabled: true, Cmd: program}, HookJob{}, t.TempDir(), io.Discard, io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(environment.root)) })
	return environment
}

func TestContainerHookProtocol(t *testing.T) {
	for _, mode := range []string{"missing_response", "invalid_json", "wrong_request", "wrong_version", "invalid_state", "null_state", "large_state", "oversized", "policy_denied", "process_failure", "missing_exit"} {
		t.Run(mode, func(t *testing.T) {
			e := testHookEnvironment(t)
			t.Setenv("GITEA_RUNNER_TEST_HOOK_MODE", mode)
			err := e.Exec([]string{"echo", "hello"}, nil, "", "")(t.Context())
			require.Error(t, err)
			assert.NotContains(t, err.Error(), e.root)
			assert.NotContains(t, err.Error(), e.config.Cmd)
		})
	}
	t.Run("stdin crosses pipe buffer boundaries", func(t *testing.T) {
		e := testHookEnvironment(t)
		t.Setenv("GITEA_RUNNER_TEST_HOOK_MODE", "large_request")
		require.NoError(t, e.Exec([]string{"echo", strings.Repeat("x", 128<<10)}, nil, "", "")(t.Context()))
	})
	t.Run("failed step preserves state", func(t *testing.T) {
		e := testHookEnvironment(t)
		t.Setenv("GITEA_RUNNER_TEST_HOOK_MODE", "command_failure")
		require.ErrorIs(t, e.Exec([]string{"false"}, nil, "", "")(t.Context()), ExitCodeError(17))
		assert.JSONEq(t, `{"calls":1}`, string(e.state))
		t.Setenv("GITEA_RUNNER_TEST_HOOK_MODE", "")
		require.NoError(t, e.Remove()(t.Context()))
		assert.JSONEq(t, `{"calls":2}`, string(e.state))
		_, err := os.Stat(e.root)
		require.ErrorIs(t, err, os.ErrNotExist)
		require.NoError(t, e.Remove()(t.Context()))
	})
	t.Run("registry values stay out of JSON", func(t *testing.T) {
		e := testHookEnvironment(t)
		e.job.Container = HookContainer{Username: "user", Password: "private-password"}
		e.addRegistrySecrets(&e.job.Container, "JOB")
		data, err := json.Marshal(e.job)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "private-password")
		assert.Contains(t, string(data), "$env")
		assert.Contains(t, e.secrets, "GITEA_CONTAINER_HOOK_SECRET_JOB_PASSWORD=private-password")
	})
}

func TestContainerHookFiles(t *testing.T) {
	e := testHookEnvironment(t)
	require.NoError(t, e.Prepare(t.Context()))
	assert.Equal(t, "/job/work/file", e.ToContainerPath(filepath.Join(e.workdir, "file")))
	require.NoError(t, e.Copy("/job/act", &FileEntry{Name: "script.sh", Mode: 0o700, Body: "echo hello"})(t.Context()))
	archive, err := e.GetContainerArchive(t.Context(), "/job/act/outputcmd.txt")
	require.NoError(t, err)
	reader := tar.NewReader(archive)
	header, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, "outputcmd.txt", header.Name)
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "answer=hello", string(body))
	require.NoError(t, archive.Close())
	files, err := filepath.Glob(filepath.Join(e.root, "payload-*"))
	require.NoError(t, err)
	assert.Empty(t, files)
	contextDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600))
	t.Setenv("GITEA_RUNNER_TEST_HOOK_MODE", "build_context")
	require.NoError(t, e.RunContainerStep(t.Context(), HookContainerStep{Dockerfile: "Dockerfile"}, contextDir, false))
	for _, mode := range []string{"transfer_failure", "missing_response", "process_failure"} {
		t.Setenv("GITEA_RUNNER_TEST_HOOK_MODE", mode)
		require.ErrorIs(t, e.UpdateFromEnv("/job/act/envs.txt", &map[string]string{})(t.Context()), ErrArchiveUnavailable)
	}
	t.Setenv("GITEA_RUNNER_TEST_HOOK_MODE", "not_found")
	require.NoError(t, e.UpdateFromEnv("/job/act/envs.txt", &map[string]string{})(t.Context()))
}

type hookReadyWriter struct {
	ready chan struct{}
	once  sync.Once
}

func (w *hookReadyWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "ready") {
		w.once.Do(func() { close(w.ready) })
	}
	return len(data), nil
}

func TestContainerHookCancellation(t *testing.T) {
	e := testHookEnvironment(t)
	t.Setenv("GITEA_RUNNER_TEST_HOOK_MODE", "cancel")
	writer := &hookReadyWriter{ready: make(chan struct{})}
	e.stdout = writer
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Exec([]string{"wait"}, nil, "", "")(ctx) }()
	select {
	case <-writer.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("hook did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("hook did not stop on cancellation")
	}
	t.Setenv("GITEA_RUNNER_TEST_HOOK_MODE", "")
	require.NoError(t, e.Remove()(t.Context()))
}
