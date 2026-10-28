// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startedJob creates and starts a job container on a fake cluster whose execs run locally with env.
func startedJob(t *testing.T, env ...string) (*kubernetesContainer, *fakeCluster, string) {
	t.Helper()
	workdir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	kubeconfig, cluster := fakeKubernetes(t, `{"spec":{"containers":[{}]},"status":{"phase":"Running","containerStatuses":[
		{"name":"job","ready":true,"state":{"running":{}}}]}}`, env...)
	pod, err := NewKubernetesPod(KubernetesOptions{Kubeconfig: kubeconfig})
	require.NoError(t, err)
	job := pod.JobContainer(&NewContainerInput{Name: "job", Image: "node", WorkingDir: workdir, Stdout: io.Discard, Stderr: io.Discard}).(*kubernetesContainer)
	require.NoError(t, job.Create(nil, nil)(t.Context()))
	require.NoError(t, job.Start(false)(t.Context()))
	return job, cluster, workdir
}

// lastExtract returns the script and stdin size of the last exec, the extract just run.
func lastExtract(t *testing.T, cluster *fakeCluster) (string, int) {
	t.Helper()
	execs, sizes := cluster.requests["exec"], cluster.requests["exec stdin bytes"]
	query, err := url.ParseQuery(execs[len(execs)-1])
	require.NoError(t, err)
	size, err := strconv.Atoi(sizes[len(sizes)-1])
	require.NoError(t, err)
	return query["command"][2], size
}

// compressible is source-like content, as actions are, of about size bytes.
func compressible(size int) string {
	line := "export function handler(event, context) { return { statusCode: 200, body: JSON.stringify(event) }; }\n"
	return strings.Repeat(line, size/len(line)+1)
}

func TestKubernetesCopiesCompressed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("needs gzip")
	}
	t.Parallel()
	ctx := t.Context()
	job, cluster, workdir := startedJob(t)
	require.True(t, job.gzip, "the probe finds gzip in the container")

	t.Run("a directory goes gzipped", func(t *testing.T) {
		action := filepath.Join(t.TempDir(), "action")
		require.NoError(t, os.MkdirAll(filepath.Join(action, "dist"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(action, "dist", "index.js"), []byte(compressible(1<<20)), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(action, "action.yml"), []byte("runs:\n  using: node20\n  main: dist/index.js\n"), 0o644))
		require.NoError(t, job.CopyDir(workdir+"/act/actions/sha/", action+"/", false, true)(ctx))

		content, err := os.ReadFile(filepath.Join(workdir, "act", "actions", "sha", "dist", "index.js"))
		require.NoError(t, err)
		assert.Equal(t, compressible(1<<20), string(content))
		script, size := lastExtract(t, cluster)
		assert.Contains(t, script, "gzip -dc |")
		assert.Less(t, size, 100<<10, "a megabyte of sources crosses the API server as a fraction of it")
	})

	t.Run("large files go gzipped", func(t *testing.T) {
		require.NoError(t, job.Copy(workdir+"/act/", &FileEntry{Name: "workflow/event.json", Mode: 0o644, Body: compressible(compressCopyMin)})(ctx))
		content, err := os.ReadFile(filepath.Join(workdir, "act", "workflow", "event.json"))
		require.NoError(t, err)
		assert.Equal(t, compressible(compressCopyMin), string(content))
		script, size := lastExtract(t, cluster)
		assert.Contains(t, script, "gzip -dc |")
		assert.Less(t, size, compressCopyMin/2)
	})

	t.Run("small files go as they are", func(t *testing.T) {
		require.NoError(t, job.Copy(workdir+"/act/", &FileEntry{Name: "workflow/1", Mode: 0o755, Body: "echo hi"})(ctx))
		content, err := os.ReadFile(filepath.Join(workdir, "act", "workflow", "1"))
		require.NoError(t, err)
		assert.Equal(t, "echo hi", string(content))
		script, _ := lastExtract(t, cluster)
		assert.NotContains(t, script, "gzip")
	})
}

func TestKubernetesCopiesUncompressedWithoutGzip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	t.Parallel()
	// A PATH with what the container needs except gzip, as on a minimal image.
	bin := t.TempDir()
	for _, tool := range []string{"uname", "tar", "mkdir"} {
		found, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(found, filepath.Join(bin, tool)))
	}
	job, cluster, workdir := startedJob(t, "PATH="+bin)
	require.False(t, job.gzip)
	assert.Equal(t, bin, job.pod.path, "the PATH still comes through the probe")

	action := filepath.Join(t.TempDir(), "action")
	require.NoError(t, os.MkdirAll(action, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(action, "index.js"), []byte(compressible(256<<10)), 0o644))
	require.NoError(t, job.CopyDir(workdir+"/act/actions/sha/", action+"/", false, true)(t.Context()))
	content, err := os.ReadFile(filepath.Join(workdir, "act", "actions", "sha", "index.js"))
	require.NoError(t, err)
	assert.Equal(t, compressible(256<<10), string(content))
	script, size := lastExtract(t, cluster)
	assert.NotContains(t, script, "gzip")
	assert.Greater(t, size, 256<<10)
}
