// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type tarCopyingContainer struct{ containerMock }

func (*tarCopyingContainer) ExtractTarGz(_destPath, _archive string) common.Executor { return nil }

func TestNodeUsesPATHWhenDisabledOrUnsupported(t *testing.T) {
	noLoader := &tarCopyingContainer{}
	noLoader.On("GetContainerArchive", mock.Anything, mock.Anything).Return(nil, os.ErrNotExist)
	for _, rc := range []*RunContext{
		{Config: &Config{NoNodeDownload: true}},
		{Config: &Config{}, JobContainer: noLoader},
	} {
		assert.Equal(t, "node", rc.node(t.Context()))
	}
	noLoader.AssertCalled(t, "GetContainerArchive", mock.Anything, mock.Anything)
}

func TestDownloadNodeVerifiesChecksumPrunesAndArchivesOnlyTheBinary(t *testing.T) {
	name := "node-v24.0.0-linux-x64"
	var archive bytes.Buffer
	gz, err := gzip.NewWriterLevel(&archive, gzip.NoCompression)
	require.NoError(t, err)
	_, err = gz.Write(tarArchive(t, tarEntry{name: name + "/bin/node", body: "node binary"}, tarEntry{name: name + "/README.md", body: strings.Repeat("tail", 1<<15)}))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	sum, exeSum := sha256.Sum256(archive.Bytes()), sha256.Sum256([]byte("node.exe binary"))
	linux := nodeBuild{goos: "linux", name: "linux-x64", version: "24.0.0", sha256: hex.EncodeToString(sum[:])}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v24.0.0/" + name + ".tar.gz":
			_, _ = response.Write(archive.Bytes())
		case "/v24.0.0/win-x64/node.exe":
			_, _ = io.WriteString(response, "node.exe binary")
		case "/v24.0.0/SHASUMS256.txt":
			_, _ = io.WriteString(response, hex.EncodeToString(exeSum[:])+"  win-x64/node.exe\n")
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("NODEJS_ORG_MIRROR", server.URL+"/")

	dir := t.TempDir()
	stale, abandoned, unrelated := filepath.Join(dir, "node-v1.0.0-linux-x64"), filepath.Join(dir, ".node-123"), filepath.Join(dir, "reports")
	for _, old := range []string{stale, abandoned, unrelated} {
		require.NoError(t, os.Mkdir(old, 0o755))
		require.NoError(t, os.Chtimes(old, time.Time{}, time.Now().Add(-8*24*time.Hour)))
	}
	binary, err := downloadNode(t.Context(), dir, linux)
	require.NoError(t, err)
	assert.NoDirExists(t, stale)
	assert.NoDirExists(t, abandoned)
	assert.DirExists(t, unrelated)
	assert.Equal(t, filepath.Join(dir, name, "bin", "node"), binary)
	archived, err := nodeArchive(binary, name)
	require.NoError(t, err)
	cached, err := os.Open(archived)
	require.NoError(t, err)
	defer cached.Close()
	unzipped, err := gzip.NewReader(cached)
	require.NoError(t, err)
	reader := tar.NewReader(unzipped)
	header, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, name+"/bin/node", header.Name)
	assert.Equal(t, int64(0o755), header.Mode)
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "node binary", string(body))
	header, err = reader.Next()
	require.NoError(t, err)
	assert.Equal(t, name+"/"+nodeComplete, header.Name)
	_, err = reader.Next()
	require.ErrorIs(t, err, io.EOF)

	binary, err = downloadNode(t.Context(), dir, nodeBuild{goos: "windows", name: "win-x64", version: "24.0.0"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "node-v24.0.0-win-x64", "bin", "node.exe"), binary)
	body, err = os.ReadFile(binary)
	require.NoError(t, err)
	assert.Equal(t, "node.exe binary", string(body))

	linux.sha256 = strings.Repeat("0", 64)
	dir = t.TempDir()
	_, err = downloadNode(t.Context(), dir, linux)
	require.ErrorContains(t, err, "checksum")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
