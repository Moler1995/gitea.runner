// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type serverV4 struct { // the subset of Gitea's v4 artifact service that the toolkit uses, unauthenticated like the v3 routes beside it
	dir     string
	mu      sync.Mutex
	startID int64
	lastID  int64
}

type artifactMeta struct {
	monolithArtifact // its Digest stays empty until the upload is finalized
	MimeType         string
}

const (
	zipMimeType   = "application/zip"
	maxTwirpBytes = 1 << 20
)

func registerV4(router *http.ServeMux, baseDir string) {
	startID := time.Now().UnixMilli() // so artifacts of earlier runs in baseDir have lower IDs
	server := &serverV4{dir: filepath.Join(baseDir, "v4"), startID: startID, lastID: startID}
	router.HandleFunc("POST "+artifactServicePath+"/CreateArtifact", twirp(server.createArtifact))
	router.HandleFunc("POST "+artifactServicePath+"/FinalizeArtifact", twirp(server.finalizeArtifact))
	router.HandleFunc("POST "+artifactServicePath+"/ListArtifacts", twirp(server.listArtifacts))
	router.HandleFunc("POST "+artifactServicePath+"/GetSignedArtifactURL", twirp(server.getSignedArtifactURL))
	router.HandleFunc("POST "+artifactServicePath+"/DeleteArtifact", twirp(server.deleteArtifact))
	router.HandleFunc("PUT "+artifactServicePath+"/UploadArtifact", server.uploadArtifact)
	router.HandleFunc("GET "+artifactServicePath+"/DownloadArtifact", server.downloadArtifact)
}

type twirpError struct {
	status int
	code   string
	msg    string
}

func (e *twirpError) Error() string { return e.msg }

func notFound(name string) error {
	return &twirpError{http.StatusNotFound, "not_found", fmt.Sprintf("artifact %q not found", name)}
}

func badRequest(format string, args ...any) error {
	return &twirpError{http.StatusBadRequest, "invalid_argument", fmt.Sprintf(format, args...)}
}

func twirp[Req, Resp any](handle func(*http.Request, Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		var resp Resp
		err := json.UnmarshalRead(io.LimitReader(r.Body, maxTwirpBytes), &req, json.MatchCaseInsensitiveNames(true)) // also matches the toolkit's proto field names
		if err != nil {
			err = badRequest("malformed request: %v", err)
		} else {
			resp, err = handle(r, req)
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			terr := &twirpError{http.StatusInternalServerError, "internal", err.Error()}
			errors.As(err, &terr)
			w.WriteHeader(terr.status)
			_ = json.MarshalWrite(w, map[string]string{"code": terr.code, "msg": terr.msg})
			return
		}
		_ = json.MarshalWrite(w, resp)
	}
}

func fileName(name string) string { // safe on any filesystem for any client-chosen name, case-insensitive ones included
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func (s *serverV4) runDir(runID string) string {
	return filepath.Join(s.dir, fileName(runID))
}

func (s *serverV4) artifactDir(runID, name string) string {
	return filepath.Join(s.runDir(runID), fileName(name))
}

func (s *serverV4) urlDir(r *http.Request) string {
	return s.artifactDir(r.URL.Query().Get("run"), r.URL.Query().Get("name"))
}

func readMeta(dir string) (*artifactMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta artifactMeta
	return &meta, json.Unmarshal(data, &meta)
}

func writeMeta(dir string, meta *artifactMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644)
}

func blobURL(r *http.Request, endpoint, runID, name string) string {
	return "http://" + r.Host + artifactServicePath + "/" + endpoint + "?" + url.Values{"run": {runID}, "name": {name}}.Encode()
}

func (s *serverV4) createArtifact(r *http.Request, req createArtifactRequest) (createArtifactResponse, error) { // replaces a finalized artifact of the same name, as Gitea does
	dir := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if meta, err := readMeta(dir); err == nil && meta.Digest == "" && meta.DatabaseID > s.startID {
		return createArtifactResponse{}, &twirpError{http.StatusConflict, "already_exists", fmt.Sprintf("artifact %q is still being uploaded", req.Name)}
	}
	if err := os.RemoveAll(dir); err != nil {
		return createArtifactResponse{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		return createArtifactResponse{}, err
	}
	s.lastID++
	if err := writeMeta(dir, &artifactMeta{
		WorkflowRunBackendID: req.WorkflowRunBackendID,
		DatabaseID:           s.lastID,
		Name:                 req.Name,
		CreatedAt:            time.Now().UTC().Truncate(time.Second),
		MimeType:             cmp.Or(req.MimeType, zipMimeType),
	}); err != nil {
		return createArtifactResponse{}, err
	}
	return createArtifactResponse{Ok: true, SignedUploadURL: blobURL(r, "UploadArtifact", req.WorkflowRunBackendID, req.Name)}, nil
}

type blockList struct {
	Blocks []string `xml:",any"` // Latest, Committed and Uncommitted all name a staged block
}

func (s *serverV4) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	dir := s.urlDir(r)
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		http.Error(w, "artifact not created", http.StatusNotFound)
		return
	}
	var err error
	switch comp := r.URL.Query().Get("comp"); comp {
	case "block":
		err = writeFile(filepath.Join(dir, "blocks", fileName(r.URL.Query().Get("blockid"))), r.Body)
	case "blocklist":
		err = joinBlocks(dir, r.Body)
	default:
		http.Error(w, "unsupported comp "+comp, http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func writeFile(name string, body io.Reader) error {
	file, err := os.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, body)
	return errors.Join(err, file.Close())
}

func joinBlocks(dir string, body io.Reader) error { // keeps the blocks for finalizeArtifact, so that a retried commit can join them again
	var list blockList
	if err := xml.NewDecoder(io.LimitReader(body, maxTwirpBytes)).Decode(&list); err != nil {
		return err
	}
	out, err := os.CreateTemp(dir, "blob")
	if err != nil {
		return err
	}
	for _, id := range list.Blocks {
		if err = appendBlock(out, filepath.Join(dir, "blocks", fileName(id))); err != nil {
			break
		}
	}
	if err = errors.Join(err, out.Close()); err != nil {
		return errors.Join(err, os.Remove(out.Name()))
	}
	return os.Rename(out.Name(), filepath.Join(dir, "blob"))
}

func appendBlock(out io.Writer, name string) error {
	in, err := os.Open(name)
	if err != nil {
		return err
	}
	defer in.Close()
	_, err = io.Copy(out, in)
	return err
}

func (s *serverV4) finalizeArtifact(_ *http.Request, req finalizeArtifactRequest) (finalizeArtifactResponse, error) {
	dir := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readMeta(dir)
	if err != nil {
		return finalizeArtifactResponse{}, notFound(req.Name)
	}
	blob, err := os.Open(filepath.Join(dir, "blob"))
	if err != nil {
		return finalizeArtifactResponse{}, err
	}
	defer blob.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, blob)
	if err != nil {
		return finalizeArtifactResponse{}, err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if size != req.Size {
		return finalizeArtifactResponse{}, badRequest("size %d does not match the %d bytes uploaded", req.Size, size)
	}
	if req.Hash != "" && req.Hash != digest {
		return finalizeArtifactResponse{}, badRequest("hash %s does not match the uploaded %s", req.Hash, digest)
	}
	meta.Size, meta.Digest = size, digest
	if err := writeMeta(dir, meta); err != nil {
		return finalizeArtifactResponse{}, err
	}
	return finalizeArtifactResponse{Ok: true, ArtifactID: meta.DatabaseID}, os.RemoveAll(filepath.Join(dir, "blocks"))
}

func (s *serverV4) listArtifacts(_ *http.Request, req listArtifactsRequest) (listArtifactsResponse, error) {
	run := s.runDir(req.WorkflowRunBackendID)
	entries, err := os.ReadDir(run)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return listArtifactsResponse{}, err
	}
	var resp listArtifactsResponse
	for _, entry := range entries {
		meta, err := readMeta(filepath.Join(run, entry.Name()))
		if err != nil || meta.Digest == "" ||
			req.NameFilter != "" && req.NameFilter != meta.Name || req.IDFilter != 0 && req.IDFilter != meta.DatabaseID {
			continue
		}
		resp.Artifacts = append(resp.Artifacts, meta.monolithArtifact)
	}
	slices.SortFunc(resp.Artifacts, func(left, right monolithArtifact) int { return cmp.Compare(left.DatabaseID, right.DatabaseID) })
	return resp, nil
}

func (s *serverV4) getSignedArtifactURL(r *http.Request, req getSignedArtifactURLRequest) (getSignedArtifactURLResponse, error) {
	if meta, err := readMeta(s.artifactDir(req.WorkflowRunBackendID, req.Name)); err != nil || meta.Digest == "" {
		return getSignedArtifactURLResponse{}, notFound(req.Name)
	}
	return getSignedArtifactURLResponse{SignedURL: blobURL(r, "DownloadArtifact", req.WorkflowRunBackendID, req.Name)}, nil
}

func (s *serverV4) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	dir := s.urlDir(r)
	meta, err := readMeta(dir)
	if err != nil || meta.Digest == "" {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	blob, err := os.Open(filepath.Join(dir, "blob"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer blob.Close()
	filename := meta.Name
	if meta.MimeType == zipMimeType {
		filename += ".zip"
	}
	w.Header().Set("Content-Type", meta.MimeType)
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filename)) // the toolkit URI-decodes any filename
	http.ServeContent(w, r, "", meta.CreatedAt, blob)
}

func (s *serverV4) deleteArtifact(_ *http.Request, req deleteArtifactRequest) (deleteArtifactResponse, error) {
	dir := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readMeta(dir)
	if err != nil {
		return deleteArtifactResponse{}, notFound(req.Name)
	}
	return deleteArtifactResponse{Ok: true, ArtifactID: meta.DatabaseID}, os.RemoveAll(dir)
}
