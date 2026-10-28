// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package uploadartifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
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

// dockerArchives archives like the Docker API: entries named below the base name of the path,
// symlinks and hard links kept as links, the path itself not followed.
type dockerArchives struct {
	*container.HostEnvironment
}

func (d dockerArchives) GetContainerArchive(_ context.Context, srcPath string) (io.ReadCloser, error) {
	if _, err := os.Lstat(srcPath); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	type file struct {
		info fs.FileInfo
		name string
	}
	var files []file
	err := filepath.WalkDir(srcPath, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(srcPath, p)
		hdr.Name = filepath.ToSlash(filepath.Join(filepath.Base(srcPath), rel))
		if i := slices.IndexFunc(files, func(f file) bool { return os.SameFile(f.info, info) }); i >= 0 {
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, files[i].name, 0
		} else if info.Mode().IsRegular() {
			files = append(files, file{info, hdr.Name})
		}
		if err := tw.WriteHeader(hdr); err != nil || hdr.Typeflag != tar.TypeReg {
			return err
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = tw.Write(content)
		return err
	})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(&buf), tw.Close()
}

type harness struct {
	t                *testing.T
	server           string
	client           *artifact.Client
	workspace        string
	env              container.ExecutionsEnvironment
	logs, annotation []string
	retentionDays    string
	output           string
}

func newHarness(t *testing.T, env container.ExecutionsEnvironment) *harness {
	t.Helper()
	router := http.NewServeMux()
	artifacts.RegisterV4(router, t.TempDir())
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	client, err := artifact.New(server.URL, artifacts.LocalRuntimeToken("7", time.Hour), nil)
	require.NoError(t, err)
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return &harness{t: t, server: server.URL, client: client, workspace: workspace, env: env}
}

func (h *harness) write(rel, content string, mode os.FileMode) {
	h.t.Helper()
	p := filepath.Join(h.workspace, filepath.FromSlash(rel))
	require.NoError(h.t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(h.t, os.WriteFile(p, []byte(content), mode))
}

func (h *harness) run(inputs map[string]string) error {
	h.logs, h.annotation = nil, nil
	h.output = filepath.Join(h.t.TempDir(), "output")
	return Main(h.t.Context(), &action.Context{
		Container:    h.env,
		Github:       &model.GithubContext{ServerURL: "https://gitea.example", Repository: "owner/repo", RunID: "7", RunAttempt: "2", RetentionDays: h.retentionDays},
		Inputs:       inputs,
		Workspace:    h.workspace,
		Env:          map[string]string{"GITHUB_OUTPUT": h.output},
		ResultsURL:   h.server,
		RuntimeToken: artifacts.LocalRuntimeToken("7", time.Hour),
		Infof:        func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) },
		Annotate:     func(level, message string) { h.annotation = append(h.annotation, level+": "+message) },
	})
}

// fetch returns the artifact's content, and its zip entries with their mode when it is a zip.
func (h *harness) fetch(name string) ([]byte, map[string]string) {
	h.t.Helper()
	signed, err := h.client.SignedURL(h.t.Context(), name)
	require.NoError(h.t, err)
	resp, err := h.client.Download(h.t.Context(), signed, time.Minute)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return content, nil
	}
	entries := map[string]string{}
	for _, f := range reader.File {
		body, err := f.Open()
		require.NoError(h.t, err)
		data, err := io.ReadAll(body)
		require.NoError(h.t, err)
		entries[f.Name] = string(data)
	}
	return content, entries
}

func environments() map[string]func() container.ExecutionsEnvironment {
	envs := map[string]func() container.ExecutionsEnvironment{
		"host": func() container.ExecutionsEnvironment { return &container.HostEnvironment{} },
	}
	if runtime.GOOS != "windows" {
		envs["container"] = func() container.ExecutionsEnvironment { return dockerArchives{&container.HostEnvironment{}} }
	}
	return envs
}

func TestUploadSelectsWhatTheToolkitSelects(t *testing.T) {
	for envName, newEnv := range environments() {
		t.Run(envName, func(t *testing.T) {
			h := newHarness(t, newEnv())
			h.write("dist/a.txt", "a", 0o644)
			h.write("dist/run.sh", "#!/bin/sh\n", 0o755)
			h.write("dist/sub/b.txt", "b", 0o644)
			h.write("dist/.hidden", "h", 0o644)
			h.write("dist/.git/config", "c", 0o644)
			h.write("logs/x.log", "x", 0o644)
			if runtime.GOOS != "windows" {
				require.NoError(t, os.Symlink("a.txt", filepath.Join(h.workspace, "dist", "link.txt")))
				require.NoError(t, os.Symlink("sub", filepath.Join(h.workspace, "dist", "linkdir")))
				require.NoError(t, os.Symlink("missing", filepath.Join(h.workspace, "dist", "broken")))
				require.NoError(t, os.Symlink("..", filepath.Join(h.workspace, "dist", "loop")))
				require.NoError(t, os.Link(filepath.Join(h.workspace, "dist", "a.txt"), filepath.Join(h.workspace, "dist", "hard.txt")))
			}

			for _, test := range []struct {
				name     string
				inputs   map[string]string
				expected map[string]string
				links    map[string]string
			}{
				{
					name:     "a directory",
					inputs:   map[string]string{"path": "dist"},
					expected: map[string]string{"a.txt": "a", "run.sh": "#!/bin/sh\n", "sub/b.txt": "b"},
					// the loop resolves to the workspace, which the toolkit enters once, as only the
					// real path of an ancestor counts as a cycle
					links: map[string]string{"link.txt": "a", "hard.txt": "a", "linkdir/b.txt": "b", "loop/logs/x.log": "x"},
				},
				{
					name:     "hidden files included",
					inputs:   map[string]string{"path": "dist/", "include-hidden-files": "true"},
					expected: map[string]string{"a.txt": "a", "run.sh": "#!/bin/sh\n", "sub/b.txt": "b", ".hidden": "h", ".git/config": "c"},
					links:    map[string]string{"link.txt": "a", "hard.txt": "a", "linkdir/b.txt": "b", "loop/logs/x.log": "x"},
				},
				{
					name:     "several patterns share their common ancestor as root",
					inputs:   map[string]string{"path": "dist/**/*.txt\n!dist/sub/**\n# a comment\nlogs/x.log"},
					expected: map[string]string{"dist/a.txt": "a", "logs/x.log": "x"},
					links:    map[string]string{"dist/link.txt": "a", "dist/hard.txt": "a", "dist/linkdir/b.txt": "b"},
				},
				{
					name:     "a single file keeps no directory",
					inputs:   map[string]string{"path": h.workspace + "/dist/sub/b.txt"},
					expected: map[string]string{"b.txt": "b"},
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					inputs := map[string]string{"name": "art", "overwrite": "true"}
					maps.Copy(inputs, test.inputs)
					require.NoError(t, h.run(inputs))
					expected := test.expected
					if runtime.GOOS != "windows" {
						maps.Copy(expected, test.links)
					}
					_, entries := h.fetch("art")
					assert.Equal(t, expected, entries)
				})
			}
		})
	}
}

func TestUploadZipAndOutputs(t *testing.T) {
	h := newHarness(t, &container.HostEnvironment{})
	h.write("out/run.sh", "#!/bin/sh\n", 0o755)
	h.write("out/data.txt", strings.Repeat("data", 1000), 0o644)
	h.retentionDays = "3"
	require.NoError(t, h.run(map[string]string{"name": "packed", "path": "out", "compression-level": "0", "retention-days": "10"}))
	assert.Equal(t, []string{"warning: Retention days cannot be greater than the maximum allowed retention set within the repository. Using 3 instead."}, h.annotation)
	assert.Contains(t, h.logs, "With the provided path, there will be 2 files uploaded")

	content, _ := h.fetch("packed")
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	require.NoError(t, err)
	for _, f := range reader.File {
		assert.Equal(t, zip.Store, f.Method, f.Name)
		if runtime.GOOS != "windows" && f.Name == "run.sh" {
			assert.Equal(t, os.FileMode(0o755), f.Mode().Perm())
		}
	}

	list, err := h.client.List(t.Context(), nil, nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	outputs := h.outputs()
	assert.Equal(t, strconv.FormatInt(list[0].ID, 10), outputs["artifact-id"])
	assert.Equal(t, strings.TrimPrefix(list[0].Digest, "sha256:"), outputs["artifact-digest"])
	assert.Equal(t, "https://gitea.example/owner/repo/actions/runs/7/artifacts/packed?attempt=2", outputs["artifact-url"])

	require.NoError(t, h.run(map[string]string{"name": "packed", "path": "out/data.txt", "overwrite": "true"}))
	list, err = h.client.List(t.Context(), nil, nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, entries := h.fetch("packed")
	assert.Equal(t, []string{"data.txt"}, keys(entries))
}

func (h *harness) outputs() map[string]string {
	h.t.Helper()
	content, err := os.ReadFile(h.output)
	require.NoError(h.t, err)
	outputs := map[string]string{}
	for line := range strings.Lines(string(content)) {
		name, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		outputs[name] = value
	}
	return outputs
}

func keys(m map[string]string) []string {
	var list []string
	for k := range m {
		list = append(list, k)
	}
	return list
}

func TestUploadWithoutArchiving(t *testing.T) {
	h := newHarness(t, &container.HostEnvironment{})
	h.write("report.html", "<html></html>", 0o644)
	h.write("two/a.txt", "a", 0o644)
	h.write("two/b.txt", "b", 0o644)

	require.NoError(t, h.run(map[string]string{"name": "ignored", "path": "report.html", "archive": "false"}))
	content, entries := h.fetch("report.html")
	assert.Nil(t, entries)
	assert.Equal(t, "<html></html>", string(content))
	assert.Contains(t, h.logs, "Uploading artifact: report.html")

	require.ErrorContains(t, h.run(map[string]string{"path": "two", "archive": "false"}), "When 'archive' is set to false, only a single file can be uploaded. Found 2 files to upload.")
}

func TestUploadFailures(t *testing.T) {
	h := newHarness(t, &container.HostEnvironment{})
	h.write("ok.txt", "x", 0o644)

	require.NoError(t, h.run(map[string]string{"path": "nothing/here"}))
	assert.Equal(t, []string{"warning: No files were found with the provided path: nothing/here. No artifacts will be uploaded."}, h.annotation)
	require.NoError(t, h.run(map[string]string{"path": "nothing", "if-no-files-found": "ignore"}))
	assert.Contains(t, h.logs, "No files were found with the provided path: nothing. No artifacts will be uploaded.")
	require.ErrorContains(t, h.run(map[string]string{"path": "nothing", "if-no-files-found": "error"}), "No files were found")

	require.ErrorContains(t, h.run(map[string]string{"name": "a/b", "path": "ok.txt"}), "The artifact name is not valid: a/b. Contains the following character:  Forward slash /")
	require.ErrorContains(t, h.run(map[string]string{}), "Input required and not supplied: path")
	require.ErrorContains(t, h.run(map[string]string{"name": "", "path": "ok.txt"}), "Provided artifact name input during validation is empty")
	require.ErrorContains(t, h.run(map[string]string{"path": "ok.txt", "overwrite": ""}), "Core Schema")
	require.NoError(t, h.run(map[string]string{"path": "ok.txt", "retention": "1", "overwrite": "true"}))
	assert.Equal(t, []string{"warning: Unexpected input(s) 'retention', valid inputs are ['name', 'path', 'if-no-files-found', 'retention-days', 'compression-level', 'overwrite', 'include-hidden-files', 'archive']"}, h.annotation)
	require.ErrorContains(t, h.run(map[string]string{"path": "ok.txt", "compression-level": "10"}), "Invalid compression-level. Valid values are 0-9")
	require.ErrorContains(t, h.run(map[string]string{"path": "ok.txt", "if-no-files-found": "fail"}), "Unrecognized if-no-files-found input. Provided: fail. Available options: warn,error,ignore")
	require.ErrorContains(t, h.run(map[string]string{"path": "ok.txt", "archive": "no"}), "Core Schema")
	if runtime.GOOS != "windows" {
		h.write("bad/a:b.txt", "x", 0o644)
		require.ErrorContains(t, h.run(map[string]string{"path": "bad"}), "The path for one of the files in artifact is not valid: /a:b.txt. Contains the following character:  Colon :")
	}
}
