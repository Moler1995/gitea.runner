// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	if os.Getenv(transferHelperEnv) == "1" && len(os.Args) == 4 && os.Args[1] == KubernetesTransferCommand && os.Args[2] == "fetch" {
		if err := FetchKubernetesTransfer(context.Background(), os.Stdin, os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fetch runs the helper side against a request, returning what it extracted into a fresh directory.
func fetch(t *testing.T, request transferRequest) (string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "act")
	body, err := json.Marshal(request)
	require.NoError(t, err)
	return dest, FetchKubernetesTransfer(t.Context(), bytes.NewReader(body), dest)
}

func eventArchive(t *testing.T) func(io.Writer) error {
	return func(w io.Writer) error {
		return writeFilesTar(t.Context(), w, 0, 0, &FileEntry{Name: "workflow/event.json", Mode: 0o644, Body: `{"ref":"main"}`})
	}
}

func TestTransferServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("extracts with tar")
	}
	server, err := startTransferServer("127.0.0.1", 0)
	require.NoError(t, err)
	again, err := startTransferServer("127.0.0.1", 0)
	require.NoError(t, err)
	assert.Same(t, server, again, "one server per runner")

	t.Run("serves an offer once over TLS to the pod it was offered to", func(t *testing.T) {
		request, done := server.offer([]string{"127.0.0.1"}, eventArchive(t))
		defer done()
		assert.True(t, strings.HasPrefix(request.URL, "https://127.0.0.1:"))
		dest, err := fetch(t, request)
		require.NoError(t, err)
		content, err := os.ReadFile(filepath.Join(dest, "workflow", "event.json"))
		require.NoError(t, err)
		assert.JSONEq(t, `{"ref":"main"}`, string(content))
		_, err = fetch(t, request)
		require.ErrorContains(t, err, "404", "a token is good for one fetch")
	})

	t.Run("another pod can neither take nor block a copy", func(t *testing.T) {
		request, done := server.offer([]string{"10.1.2.3"}, eventArchive(t))
		defer done()
		_, err := fetch(t, request) // from 127.0.0.1, not the job pod
		require.ErrorContains(t, err, "404")
		server.mu.Lock()
		_, pending := server.pending[strings.TrimPrefix(request.URL, server.base)]
		server.mu.Unlock()
		assert.True(t, pending, "the job pod can still fetch it")
	})

	t.Run("a dual-stack job pod may fetch from either address", func(t *testing.T) {
		request, done := server.offer([]string{"fd00::1", "127.0.0.1"}, eventArchive(t))
		defer done()
		_, err := fetch(t, request)
		require.NoError(t, err)
	})

	t.Run("a finished copy is no longer served", func(t *testing.T) {
		request, done := server.offer([]string{"127.0.0.1"}, eventArchive(t))
		done()
		_, err := fetch(t, request)
		require.ErrorContains(t, err, "404")
	})

	t.Run("a failed write cuts the response short", func(t *testing.T) {
		request, done := server.offer([]string{"127.0.0.1"}, func(w io.Writer) error {
			_, _ = w.Write(make([]byte, 64<<10))
			return errors.New("source gone")
		})
		defer done()
		_, err := fetch(t, request)
		require.Error(t, err)
	})

	t.Run("the helper only talks to the runner whose pin it got", func(t *testing.T) {
		other, err := startTransferServer("127.0.0.1", freePort(t))
		require.NoError(t, err)
		require.NotEqual(t, server.pin, other.pin, "every runner has its own certificate")
		request, done := other.offer([]string{"127.0.0.1"}, eventArchive(t))
		defer done()
		request.Pin = server.pin
		_, err = fetch(t, request)
		require.ErrorContains(t, err, "does not match the pin")

		request.Pin = ""
		_, err = fetch(t, request)
		require.ErrorContains(t, err, "no certificate pin")
	})

	t.Run("the pin is the served certificate", func(t *testing.T) {
		connection, err := tls.Dial("tcp", strings.TrimSuffix(strings.TrimPrefix(server.base, "https://"), transferPath), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // inspecting the self-signed certificate
		require.NoError(t, err)
		defer connection.Close()
		sum := sha256.Sum256(connection.ConnectionState().PeerCertificates[0].Raw)
		assert.Equal(t, hex.EncodeToString(sum[:]), server.pin)
		assert.Equal(t, uint16(tls.VersionTLS13), connection.ConnectionState().Version)
	})
}

func freePort(t *testing.T) uint16 {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return uint16(listener.Addr().(*net.TCPAddr).Port)
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
	newPod := func(image string, fallback bool) *KubernetesPod {
		pod := &KubernetesPod{options: KubernetesOptions{Transfer: &KubernetesTransfer{Host: "127.0.0.1", Image: image, Binary: "/usr/local/bin/gitea-runner", Fallback: fallback}}}
		pod.JobContainer(&NewContainerInput{})
		return pod
	}

	pod := newPod("", false)
	require.NoError(t, pod.setupTransfer(t.Context(), []string{"gitea/runner:latest"}))
	assert.Equal(t, "gitea/runner:latest", pod.transferImage, "a single-container runner pod's image holds the binary")
	assert.Equal(t, "/var/run/act/bin/gitea-runner", pod.transferHelper)

	pod = newPod("registry.example/runner:1", false)
	require.NoError(t, pod.setupTransfer(t.Context(), []string{"gitea/runner:latest", "sidecar"}))
	assert.Equal(t, "registry.example/runner:1", pod.transferImage)

	pod = newPod("", true)
	require.NoError(t, pod.setupTransfer(t.Context(), []string{"gitea/runner:latest", "sidecar"}))
	assert.Empty(t, pod.transferHelper, "auto leaves a runner pod with several containers and no image on the API server")

	pod = newPod("", false)
	require.ErrorContains(t, pod.setupTransfer(t.Context(), []string{"gitea/runner:latest", "sidecar"}), "kubernetes.transfer.image",
		"direct fails the job rather than quietly using the API server")
	assert.Empty(t, pod.transferHelper)
}

func TestKubernetesPodCopiesOverPodNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	ctx := t.Context()
	workdir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	kubeconfig, cluster := fakeKubernetes(t, `{"spec":{"containers":[{}]},"status":{"phase":"Running","podIP":"127.0.0.1","containerStatuses":[
		{"name":"job","ready":true,"state":{"running":{}}}]}}`, transferHelperEnv+"=1")
	pod, err := NewKubernetesPod(KubernetesOptions{Kubeconfig: kubeconfig, Transfer: &KubernetesTransfer{
		Host: "127.0.0.1", Image: "gitea/runner:latest", Binary: "/usr/local/bin/gitea-runner", Fallback: true,
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
	assert.NotContains(t, execs[len(execs)-1], "transfer%2F", "the token goes over stdin, not into the command line and audit log")

	unreachable := filepath.Join(workdir, "unreachable")
	require.NoError(t, os.WriteFile(unreachable, []byte("#!/bin/sh\necho 'connection refused' >&2\nexit 1\n"), 0o755))
	pod.transferHelper = unreachable
	pod.options.Transfer.Fallback = false
	err = job.Copy(workdir+"/act/", &FileEntry{Name: "workflow/other.json", Mode: 0o644, Body: `{}`})(ctx)
	require.ErrorContains(t, err, "connection refused", "direct fails the copy")
	assert.Equal(t, unreachable, pod.transferHelper)
	_, err = os.Stat(filepath.Join(workdir, "act", "workflow", "other.json"))
	require.ErrorIs(t, err, os.ErrNotExist)

	pod.options.Transfer.Fallback = true
	require.NoError(t, job.Copy(workdir+"/act/", &FileEntry{Name: "workflow/other.json", Mode: 0o644, Body: `{}`})(ctx))
	_, err = os.Stat(filepath.Join(workdir, "act", "workflow", "other.json"))
	require.NoError(t, err, "auto falls back to the API server")
	assert.Empty(t, pod.transferHelper, "and the pod stays on it")
	execs = cluster.requests["exec"]
	assert.NotContains(t, execs[len(execs)-1], "command="+KubernetesTransferCommand)

	pod.transferHelper, pod.ips = os.Args[0], nil
	pod.options.Transfer.Fallback = false
	require.ErrorContains(t, job.Copy(workdir+"/act/", &FileEntry{Name: "workflow/third.json", Mode: 0o644, Body: `{}`})(ctx), "no IP",
		"without the pod IP there is no one to serve the copy to")
}
