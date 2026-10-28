// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifact

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/artifacts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var fastRetry = Retry{Attempts: 3, Base: time.Millisecond, Multiplier: 1, MaxWait: time.Second}

func TestClientRoundTrip(t *testing.T) {
	ctx := t.Context()
	router := http.NewServeMux()
	artifacts.RegisterV4(router, t.TempDir())
	server := httptest.NewServer(router)
	defer server.Close()
	client, err := New(server.URL, artifacts.LocalRuntimeToken("7", time.Hour), nil)
	require.NoError(t, err)
	client.Retry = fastRetry
	content := make([]byte, 2*BlockSize+123)
	_, _ = rand.Read(content)
	sum := sha256.Sum256(content)

	uploadURL, err := client.Create(ctx, "art", "", nil)
	require.NoError(t, err)
	uploaded, err := client.Upload(ctx, uploadURL, "application/zip", bytes.NewReader(content), 3, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, Uploaded{Size: int64(len(content)), Digest: "sha256:" + hex.EncodeToString(sum[:])}, uploaded)
	id, err := client.Finalize(ctx, "art", uploaded.Size, uploaded.Digest)
	require.NoError(t, err)

	listed, err := client.List(ctx, "art")
	require.NoError(t, err)
	assert.Equal(t, []Artifact{{ID: id, Name: "art", Size: int64(len(content)), Digest: uploaded.Digest}}, listed)

	signed, err := client.SignedURL(ctx, "art")
	require.NoError(t, err)
	resp, err := client.Download(ctx, signed, time.Minute)
	require.NoError(t, err)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.True(t, bytes.Equal(content, got))

	deleted, err := client.Delete(ctx, "art")
	require.NoError(t, err)
	assert.Equal(t, id, deleted)
	_, err = client.SignedURL(ctx, "art")
	assert.True(t, IsNotFound(err), err)

	uploadURL, err = client.Create(ctx, "empty.txt", "text/plain", nil)
	require.NoError(t, err)
	uploaded, err = client.Upload(ctx, uploadURL, "text/plain", strings.NewReader(""), 2, time.Minute)
	require.NoError(t, err)
	_, err = client.Finalize(ctx, "empty.txt", uploaded.Size, uploaded.Digest)
	require.NoError(t, err)
}

func TestClientRetries(t *testing.T) {
	var calls atomic.Int32
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey"), "the runtime token is the bearer")
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			http.Error(w, "Error create or get artifact", http.StatusInternalServerError)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"signedUploadUrl":"http://blob/u?sig=x"}`)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, artifacts.LocalRuntimeToken("7", time.Hour), nil)
	require.NoError(t, err)
	client.Retry = fastRetry
	var logged []string
	client.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	uploadURL, err := client.Create(t.Context(), "art", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "http://blob/u?sig=x", uploadURL)
	assert.NotContains(t, bodies[2], "mime_type", "older Gitea rejects the field")
	assert.Contains(t, logged[1], "Error create or get artifact")

	calls.Store(10)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"invalid_argument","msg":"bad name"}`)
	}))
	defer failing.Close()
	client.base.Host = strings.TrimPrefix(failing.URL, "http://")
	_, err = client.Create(t.Context(), "art", "", nil)
	require.ErrorContains(t, err, "(400) Bad Request: bad name")
	assert.Equal(t, int32(11), calls.Load(), "a 400 is not retried")

	var blockIDs []string
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockIDs = append(blockIDs, r.URL.Query().Get("blockid"))
		w.WriteHeader(http.StatusCreated)
	}))
	for range 2 {
		_, err = client.Upload(t.Context(), blob.URL+"/u?sig=secret", "application/zip", strings.NewReader("x"), 1, time.Minute)
		require.NoError(t, err)
	}
	assert.NotEqual(t, blockIDs[0], blockIDs[2], "each upload names its blocks anew")
	blob.Close()
	_, err = client.Upload(t.Context(), blob.URL+"/u?sig=secret", "application/zip", strings.NewReader("x"), 1, time.Minute)
	require.Error(t, err)
	assert.NotContains(t, err.Error()+strings.Join(logged, ""), "secret")
}

func TestDownloadStalls(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	resp, err := download(t.Context(), http.DefaultClient, server.URL, 50*time.Millisecond)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	require.ErrorContains(t, err, "did not respond in 50ms")
}
