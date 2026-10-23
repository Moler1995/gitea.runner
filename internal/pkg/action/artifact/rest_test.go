// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifact

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRESTListsEveryPageAndSkipsExpired(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/repos/own%20er/repo/actions/runs/12/artifacts", r.URL.EscapedPath())
		assert.Equal(t, "token secret", r.Header.Get("Authorization"))
		assert.Empty(t, r.URL.Query().Get("per_page"))
		pages = append(pages, r.URL.Query().Get("page")+"/"+r.URL.Query().Get("limit"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		count := restPageSize
		if page == 2 {
			count = 3
		}
		var entries []string
		for i := range count {
			id := (page-1)*restPageSize + i + 1
			entries = append(entries, fmt.Sprintf(`{"id":%d,"name":"a%d","size_in_bytes":%d,"created_at":"2026-01-02T03:04:05Z","expired":%t}`, id, id, id, id == 2))
		}
		_, _ = io.WriteString(w, `{"total_count":53,"artifacts":[`+strings.Join(entries, ",")+`]}`)
	}))
	defer server.Close()
	rest, err := NewREST(server.URL, "secret", nil)
	require.NoError(t, err)
	list, err := rest.List(t.Context(), "own er", "repo", 12, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"1/50", "2/50"}, pages)
	assert.Len(t, list, 52)
	assert.Equal(t, Artifact{ID: 3, Name: "a3", Size: 3, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, list[1])
}

func TestRESTDownloadKeepsTheTokenOffTheBlob(t *testing.T) {
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "zip bytes")
	}))
	defer blob.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/o/r/actions/artifacts/5/zip":
			http.Redirect(w, r, blob.URL+"/raw?sig=x", http.StatusFound)
		case "/api/v1/repos/o/r/actions/artifacts/6/zip":
			http.Error(w, "Artifact has expired", http.StatusNotFound)
		}
	}))
	defer api.Close()
	rest, err := NewREST(api.URL, "secret", nil)
	require.NoError(t, err)
	rest.Retry = fastRetry

	location, err := rest.DownloadURL(t.Context(), "o", "r", 5)
	require.NoError(t, err)
	assert.Equal(t, blob.URL+"/raw?sig=x", location)
	resp, err := rest.Download(t.Context(), location, time.Minute)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "zip bytes", string(body))

	_, err = rest.DownloadURL(t.Context(), "o", "r", 6)
	require.ErrorContains(t, err, "Artifact has expired")
}
