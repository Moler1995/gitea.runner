// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactV4Flow(t *testing.T) {
	router := http.NewServeMux()
	RegisterV4(router, t.TempDir())
	server := httptest.NewServer(router)
	defer server.Close()

	call := func(t *testing.T, method, body string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Post(server.URL+ArtifactServicePath+"/"+method, "application/json", strings.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()
		var out map[string]any
		require.NoError(t, json.UnmarshalRead(resp.Body, &out))
		return resp.StatusCode, out
	}
	put := func(t *testing.T, rawURL, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, rawURL, strings.NewReader(body))
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	// the toolkit's spelling: proto field names and int64 as strings
	status, created := call(t, "CreateArtifact", `{"workflow_run_backend_id":"7","workflow_job_run_backend_id":"9","name":"my art","version":7,"mime_type":"application/zip"}`)
	require.Equal(t, http.StatusOK, status, created)
	upload := created["signedUploadUrl"].(string)
	require.Equal(t, http.StatusCreated, put(t, upload+"&comp=block&blockid=Yg", "world"))
	require.Equal(t, http.StatusCreated, put(t, upload+"&comp=block&blockid=YQ", "hello "))
	require.Equal(t, http.StatusCreated, put(t, upload+"&comp=blocklist", `<?xml version="1.0"?><BlockList><Latest>YQ</Latest><Latest>Yg</Latest></BlockList>`))
	tampered, err := url.Parse(upload)
	require.NoError(t, err)
	q := tampered.Query()
	q.Set("name", "other")
	tampered.RawQuery = q.Encode()
	assert.Equal(t, http.StatusUnauthorized, put(t, tampered.String()+"&comp=block", "x"))

	sum := sha256.Sum256([]byte("hello world"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	status, _ = call(t, "FinalizeArtifact", `{"workflow_run_backend_id":"7","name":"my art","size":"10"}`)
	assert.Equal(t, http.StatusBadRequest, status)
	status, finalized := call(t, "FinalizeArtifact", `{"workflow_run_backend_id":"7","name":"my art","size":"11","hash":"`+digest+`"}`)
	require.Equal(t, http.StatusOK, status, finalized)
	id := finalized["artifactId"].(string)

	status, listed := call(t, "ListArtifacts", `{"workflowRunBackendId":"7","idFilter":"`+id+`"}`)
	require.Equal(t, http.StatusOK, status)
	artifacts := listed["artifacts"].([]any)
	require.Len(t, artifacts, 1)
	assert.Equal(t, map[string]any{"name": "my art", "size": "11", "digest": digest, "databaseId": id}, map[string]any{
		"name": artifacts[0].(map[string]any)["name"], "size": artifacts[0].(map[string]any)["size"],
		"digest": artifacts[0].(map[string]any)["digest"], "databaseId": artifacts[0].(map[string]any)["databaseId"],
	})
	_, other := call(t, "ListArtifacts", `{"workflow_run_backend_id":"8"}`)
	assert.Empty(t, other["artifacts"])

	status, signed := call(t, "GetSignedArtifactURL", `{"workflow_run_backend_id":"7","name":"my art"}`)
	require.Equal(t, http.StatusOK, status)
	resp, err := http.Get(signed["signedUrl"].(string))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(body))
	assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
	assert.Equal(t, `attachment; filename="my art.zip"`, resp.Header.Get("Content-Disposition"))

	status, _ = call(t, "DeleteArtifact", `{"workflow_run_backend_id":"7","name":"my art"}`)
	require.Equal(t, http.StatusOK, status)
	status, missing := call(t, "GetSignedArtifactURL", `{"workflow_run_backend_id":"7","name":"my art"}`)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "not_found", missing["code"])

	status, _ = call(t, "CreateArtifact", `{"workflow_run_backend_id":"../7","name":"x"}`)
	assert.Equal(t, http.StatusBadRequest, status)
}
