// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const transferHelperEnv = "GITEA_RUNNER_TEST_TRANSFER_HELPER"

// TestMain lets the test binary stand in for the runner binary a job pod runs the transfer helper with.
func TestMain(m *testing.M) {
	if os.Getenv(transferHelperEnv) == "1" && len(os.Args) == 5 && os.Args[1] == KubernetesTransferCommand && os.Args[2] == "fetch" {
		if err := FetchKubernetesTransfer(context.Background(), os.Args[3], os.Args[4]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestTransferServerServesEachOfferOnce(t *testing.T) {
	server, err := startTransferServer("127.0.0.1", 0)
	require.NoError(t, err)
	get := func(url string) (int, string, error) {
		response, err := http.Get(url) //nolint:noctx // test against a local server
		if err != nil {
			return 0, "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return response.StatusCode, string(body), err
	}

	url, done := server.offer(func(w io.Writer) error { _, err := io.WriteString(w, "content"); return err })
	assert.True(t, strings.HasPrefix(url, "http://127.0.0.1:"))
	status, body, err := get(url)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "content", body)
	status, _, err = get(url)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, status, "a token is good for one fetch")
	done()

	url, done = server.offer(func(io.Writer) error { return nil })
	done()
	status, _, err = get(url)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, status, "a finished copy is no longer served")

	url, done = server.offer(func(w io.Writer) error {
		_, _ = w.Write(make([]byte, 64<<10))
		return errors.New("source gone")
	})
	defer done()
	_, _, err = get(url)
	require.Error(t, err, "a failed write cuts the response short")

	again, err := startTransferServer("127.0.0.1", 0)
	require.NoError(t, err)
	assert.Same(t, server, again)
}

func TestFetchKubernetesTransfer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("extracts with tar")
	}
	archive := func(w io.Writer) error {
		return writeFilesTar(t.Context(), w, 0, 0, &FileEntry{Name: "workflow/event.json", Mode: 0o644, Body: `{"ref":"main"}`})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_ = archive(w)
		case "/cut":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, 700)) // a header block and part of the next
			panic(http.ErrAbortHandler)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dest := filepath.Join(t.TempDir(), "act")

	require.NoError(t, FetchKubernetesTransfer(t.Context(), server.URL+"/ok", dest))
	content, err := os.ReadFile(filepath.Join(dest, "workflow", "event.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"ref":"main"}`, string(content))

	require.ErrorContains(t, FetchKubernetesTransfer(t.Context(), server.URL+"/gone", dest), "404")
	require.Error(t, FetchKubernetesTransfer(t.Context(), server.URL+"/cut", dest))
}

func TestInstallKubernetesTransferHelper(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, InstallKubernetesTransferHelper(dir))
	executable, err := os.Executable()
	require.NoError(t, err)
	installed, err := os.Stat(filepath.Join(dir, filepath.Base(executable)))
	require.NoError(t, err)
	original, err := os.Stat(executable)
	require.NoError(t, err)
	assert.Equal(t, original.Size(), installed.Size())
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o755), installed.Mode().Perm())
	}
}

func TestKubernetesPodTransferImage(t *testing.T) {
	newPod := func(image string) *KubernetesPod {
		pod := &KubernetesPod{options: KubernetesOptions{Transfer: &KubernetesTransfer{Host: "127.0.0.1", Image: image, Binary: "/usr/local/bin/gitea-runner"}}}
		pod.JobContainer(&NewContainerInput{})
		return pod
	}

	pod := newPod("")
	pod.setupTransfer(t.Context(), []string{"gitea/runner:latest"})
	assert.Equal(t, "gitea/runner:latest", pod.transferImage, "a single-container runner pod's image holds the binary")
	assert.Equal(t, "/var/run/act/bin/gitea-runner", pod.transferHelper)

	pod = newPod("")
	pod.setupTransfer(t.Context(), []string{"gitea/runner:latest", "sidecar"})
	assert.Empty(t, pod.transferHelper, "with several containers the image must be configured")

	pod = newPod("registry.example/runner:1")
	pod.setupTransfer(t.Context(), []string{"gitea/runner:latest", "sidecar"})
	assert.Equal(t, "registry.example/runner:1", pod.transferImage)
}

func TestKubernetesPodCopiesOverPodNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	ctx := t.Context()
	workdir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	kubeconfig, cluster := fakeKubernetes(t, `{"spec":{"containers":[{}]},"status":{"phase":"Running","containerStatuses":[
		{"name":"job","ready":true,"state":{"running":{}}}]}}`, transferHelperEnv+"=1")
	pod, err := NewKubernetesPod(KubernetesOptions{Kubeconfig: kubeconfig, Transfer: &KubernetesTransfer{
		Host: "127.0.0.1", Image: "gitea/runner:latest", Binary: "/usr/local/bin/gitea-runner",
	}})
	require.NoError(t, err)
	job := pod.JobContainer(&NewContainerInput{Name: "job", Image: "node", WorkingDir: workdir, Stdout: io.Discard, Stderr: io.Discard})
	require.NoError(t, job.Create(nil, nil)(ctx))

	var manifest struct {
		Spec struct {
			InitContainers []struct {
				Name, Image  string
				Command      []string
				VolumeMounts []struct{ Name, MountPath string }
			}
		}
	}
	require.NoError(t, json.Unmarshal([]byte(cluster.requests["POST /api/v1/namespaces/ci/pods"][0]), &manifest))
	require.Len(t, manifest.Spec.InitContainers, 1)
	initContainer := manifest.Spec.InitContainers[0]
	assert.Equal(t, "gitea/runner:latest", initContainer.Image)
	assert.Equal(t, []string{"/usr/local/bin/gitea-runner", KubernetesTransferCommand, "install", "/var/run/act/bin"}, initContainer.Command)
	assert.Equal(t, []struct{ Name, MountPath string }{{"act", "/var/run/act"}}, initContainer.VolumeMounts)
	require.NoError(t, job.Start(false)(ctx))

	pod.transferHelper = os.Args[0] // stands in for the binary the init container installs
	require.NoError(t, job.Copy(workdir+"/act/", &FileEntry{Name: "workflow/event.json", Mode: 0o644, Body: `{"ref":"main"}`})(ctx))
	content, err := os.ReadFile(filepath.Join(workdir, "act", "workflow", "event.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"ref":"main"}`, string(content))
	execs := cluster.requests["exec"]
	assert.Contains(t, execs[len(execs)-1], "command="+KubernetesTransferCommand)
	assert.NotContains(t, execs[len(execs)-1], "stdin=true", "the files come over the pod network, not the exec stream")

	unreachable := filepath.Join(workdir, "unreachable")
	require.NoError(t, os.WriteFile(unreachable, []byte("#!/bin/sh\necho 'connection refused' >&2\nexit 1\n"), 0o755))
	pod.transferHelper = unreachable
	require.NoError(t, job.Copy(workdir+"/act/", &FileEntry{Name: "workflow/other.json", Mode: 0o644, Body: `{}`})(ctx))
	_, err = os.Stat(filepath.Join(workdir, "act", "workflow", "other.json"))
	require.NoError(t, err, "a failed transfer falls back to the API server")
	assert.Empty(t, pod.transferHelper, "and the pod stays on it")
	execs = cluster.requests["exec"]
	assert.Contains(t, execs[len(execs)-1], "stdin=true")
}
