// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package downloadartifact

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/artifacts"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/artifact"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type zipEntry struct {
	name, body string
	mode       os.FileMode
}

func zipOf(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		header := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		header.SetMode(e.mode)
		w, err := zw.CreateHeader(header)
		require.NoError(t, err)
		_, err = io.WriteString(w, e.body)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

type harness struct {
	t                *testing.T
	server, store    string
	client           *artifact.Client
	workspace        string
	mu               sync.Mutex
	logs, annotation []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	retryDelay = time.Millisecond
	store := t.TempDir()
	router := http.NewServeMux()
	artifacts.RegisterV4(router, store)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client, err := artifact.New(server.URL, artifacts.LocalRuntimeToken("7", time.Hour), nil)
	require.NoError(t, err)
	return &harness{t: t, server: server.URL, store: store, client: client, workspace: t.TempDir()}
}

func (h *harness) upload(name, mimeType string, content []byte) int64 {
	h.t.Helper()
	ctx := h.t.Context()
	signed, err := h.client.Create(ctx, name, mimeType, nil)
	require.NoError(h.t, err)
	uploaded, err := h.client.Upload(ctx, signed, mimeType, bytes.NewReader(content), 2, time.Minute)
	require.NoError(h.t, err)
	id, err := h.client.Finalize(ctx, name, uploaded.Size, uploaded.Digest)
	require.NoError(h.t, err)
	return id
}

func (h *harness) run(inputs map[string]string) error {
	h.logs, h.annotation = nil, nil
	return Main(h.t.Context(), &action.Context{
		Container:    &container.HostEnvironment{},
		Github:       &model.GithubContext{Repository: "owner/repo", RunID: "7"},
		Inputs:       inputs,
		Workspace:    h.workspace,
		Env:          map[string]string{"GITHUB_OUTPUT": filepath.Join(h.workspace, "output")},
		ResultsURL:   h.server,
		RuntimeToken: artifacts.LocalRuntimeToken("7", time.Hour),
		Infof: func(format string, args ...any) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.logs = append(h.logs, fmt.Sprintf(format, args...))
		},
		Warning: func(format string, args ...any) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.annotation = append(h.annotation, "warning: "+fmt.Sprintf(format, args...))
		},
	})
}

func (h *harness) read(rel string) string {
	h.t.Helper()
	content, err := os.ReadFile(filepath.Join(h.workspace, filepath.FromSlash(rel)))
	require.NoError(h.t, err)
	return string(content)
}

func TestDownloadByNameExtractsLikeUnzipStream(t *testing.T) {
	h := newHarness(t)
	h.upload("build", "", zipOf(t,
		zipEntry{name: "run.sh", body: "#!/bin/sh\n", mode: 0o755},
		zipEntry{name: "../escape.txt", body: "kept inside", mode: 0o644},
		zipEntry{name: "a/../../nested.txt", body: "nested", mode: 0o644},
		zipEntry{name: "link", body: "/etc/hosts", mode: os.ModeSymlink | 0o777},
		zipEntry{name: "empty/", mode: os.ModeDir | 0o755},
	))
	require.NoError(t, h.run(map[string]string{"name": "build", "path": "out"}))

	assert.Equal(t, "#!/bin/sh\n", h.read("out/run.sh"))
	assert.Equal(t, "kept inside", h.read("out/escape.txt"))
	assert.Equal(t, "nested", h.read("out/a/nested.txt"))
	assert.Equal(t, "/etc/hosts", h.read("out/link"), "a symlink entry becomes a file holding its target")
	assert.DirExists(t, filepath.Join(h.workspace, "out", "empty"))
	assert.NoFileExists(t, filepath.Join(h.workspace, "escape.txt"))
	if runtime.GOOS != "windows" {
		info, err := os.Lstat(filepath.Join(h.workspace, "out", "run.sh"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), info.Mode(), "unzip-stream restores no mode")
	}
	assert.Equal(t, "download-path="+filepath.Join(h.workspace, "out")+"\n", h.read("output"))
	assert.Contains(t, h.logs, "Total of 1 artifact(s) downloaded")
}

func TestDownloadSelection(t *testing.T) {
	h := newHarness(t)
	first := h.upload("alpha", "", zipOf(t, zipEntry{name: "a.txt", body: "a1", mode: 0o644}))
	h.upload("beta", "", zipOf(t, zipEntry{name: "b.txt", body: "b", mode: 0o644}))
	h.upload(".hidden", "", zipOf(t, zipEntry{name: "h.txt", body: "h", mode: 0o644}))
	_, err := h.client.Delete(t.Context(), "alpha")
	require.NoError(t, err)
	second := h.upload("alpha", "", zipOf(t, zipEntry{name: "a.txt", body: "a2", mode: 0o644}))

	require.NoError(t, h.run(map[string]string{"path": "all"}))
	assert.Equal(t, "a2", h.read("all/alpha/a.txt"))
	assert.Equal(t, "b", h.read("all/beta/b.txt"))
	assert.Equal(t, "h", h.read("all/.hidden/h.txt"))

	require.NoError(t, h.run(map[string]string{"path": "merged", "merge-multiple": "true"}))
	assert.Equal(t, "b", h.read("merged/b.txt"))

	require.NoError(t, h.run(map[string]string{"path": "pattern", "pattern": "*"}))
	assert.NoDirExists(t, filepath.Join(h.workspace, "pattern", ".hidden"), "a wildcard does not match a leading dot")
	assert.Equal(t, "b", h.read("pattern/beta/b.txt"))

	require.NoError(t, h.run(map[string]string{"path": "none", "pattern": "nope-*"}))
	assert.Contains(t, h.logs, "Total of 0 artifact(s) downloaded")

	require.NoError(t, h.run(map[string]string{"path": "ids", "artifact-ids": fmt.Sprintf("%d, %d", second, first)}))
	assert.Equal(t, "a2", h.read("ids/a.txt"), "a single match lands in path itself")
	assert.Equal(t, []string{fmt.Sprintf("warning: Could not find the following artifact IDs: %d", first)}, h.annotation)

	require.ErrorContains(t, h.run(map[string]string{"artifact-ids": strconv.FormatInt(first, 10)}), "None of the provided artifact IDs were found")
	require.ErrorContains(t, h.run(map[string]string{"name": "gone"}), "Unable to download artifact(s): Artifact not found for name: gone")
	require.ErrorContains(t, h.run(map[string]string{"name": "alpha", "artifact-ids": "1"}), "cannot be used together")
	require.ErrorContains(t, h.run(map[string]string{"artifact-ids": "x"}), "Invalid artifact ID: 'x'. Must be a number.")
	require.ErrorContains(t, h.run(map[string]string{"digest-mismatch": "loud"}), "Valid options are: ignore, info, warn, error")
	require.ErrorContains(t, h.run(map[string]string{"merge-multiple": "yes"}), "Core Schema")
	h.upload("..", "", zipOf(t, zipEntry{name: "escaped.txt", body: "x", mode: 0o644}))
	require.ErrorContains(t, h.run(map[string]string{"path": "all"}), "Artifact name is not valid: ..")
	require.NoError(t, h.run(map[string]string{"name": "beta", "path": "warned", "retention-days": "1"}))
	assert.Equal(t, []string{"warning: Unexpected input(s) 'retention-days', valid inputs are ['name', 'artifact-ids', 'path', 'pattern', 'merge-multiple', 'github-token', 'repository', 'run-id', 'skip-decompress', 'digest-mismatch']"}, h.annotation)
}

func TestDownloadRawFilesAndSkipDecompress(t *testing.T) {
	h := newHarness(t)
	h.upload("notes ü.txt", "text/plain", []byte("plain text"))
	archive := zipOf(t, zipEntry{name: "x.txt", body: "x", mode: 0o644})
	h.upload("bundle", "", archive)

	require.NoError(t, h.run(map[string]string{"name": "notes ü.txt", "path": "raw"}))
	assert.Equal(t, "plain text", h.read("raw/notes ü.txt"))

	require.NoError(t, h.run(map[string]string{"name": "bundle", "path": "kept", "skip-decompress": "true"}))
	assert.Equal(t, string(archive), h.read("kept/bundle.zip"))
}

func TestDownloadDigestMismatch(t *testing.T) {
	h := newHarness(t)
	h.upload("art", "", zipOf(t, zipEntry{name: "f.txt", body: "original", mode: 0o644}))
	blobs, err := filepath.Glob(filepath.Join(h.store, "v4", "*", "*", "blob"))
	require.NoError(t, err)
	require.Len(t, blobs, 1)
	tampered := zipOf(t, zipEntry{name: "f.txt", body: "tampered", mode: 0o644})
	require.NoError(t, os.WriteFile(blobs[0], tampered, 0o644))

	err = h.run(map[string]string{"name": "art", "path": "strict"})
	require.ErrorContains(t, err, "Digest validation failed for artifact(s): art. Use 'digest-mismatch: warn' to continue on mismatch.")
	assert.Equal(t, "tampered", h.read("strict/f.txt"), "the files stay, as with the JavaScript action")

	require.NoError(t, h.run(map[string]string{"name": "art", "path": "lenient", "digest-mismatch": "warn"}))
	assert.Equal(t, []string{"warning: Artifact 'art' digest validation failed. Please verify the integrity of the artifact."}, h.annotation)
}

func TestDownloadFromAnotherRun(t *testing.T) {
	h := newHarness(t)
	archive := zipOf(t, zipEntry{name: "other.txt", body: "from run 12", mode: 0o644})
	var expired atomic.Bool
	var zipCalls atomic.Int32
	var gitea *httptest.Server
	gitea = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/someone/else/actions/runs/12/artifacts":
			assert.Equal(t, "token pat", r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"total_count":2,"artifacts":[
				{"id":30,"name":"dist","size_in_bytes":1,"expired":true},
				{"id":20,"name":"dist","size_in_bytes":1,"expired":false}]}`)
		case "/api/v1/repos/someone/else/actions/artifacts/20/zip":
			zipCalls.Add(1)
			if expired.Load() {
				http.Error(w, "Artifact has expired", http.StatusNotFound)
				return
			}
			http.Redirect(w, r, gitea.URL+"/raw/20?sig=abc", http.StatusFound)
		case "/raw/20":
			assert.Empty(t, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gitea.Close()
	h.server = gitea.URL

	inputs := map[string]string{"name": "dist", "github-token": "pat", "repository": "someone/else/ignored", "run-id": "12", "path": "other"}
	require.NoError(t, h.run(inputs))
	assert.Equal(t, "from run 12", h.read("other/other.txt"), "the expired newer copy is skipped")
	assert.Contains(t, strings.Join(h.logs, "\n"), "Redirecting to blob download url: "+gitea.URL+"/raw/20\n")

	expired.Store(true)
	require.ErrorContains(t, h.run(inputs), "Artifact has expired")
	assert.Equal(t, int32(2), zipCalls.Load(), "an API error is not retried")
	require.ErrorContains(t, h.run(map[string]string{"github-token": "pat", "repository": "nobody"}), "Invalid repository: 'nobody'. Must be in format owner/repo")
	require.ErrorContains(t, h.run(map[string]string{"github-token": "pat", "run-id": "0"}), "Invalid run-id: '0'")
}
