// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The v4 artifact service for `exec`, the subset of Gitea's that the toolkit and the built-in
// actions use. Blobs take the Azure block protocol Gitea also speaks, so blocks uploaded in
// parallel are joined in the order of the block list.
type serverV4 struct {
	dir    string
	key    []byte
	nextID atomic.Int64
	mu     sync.Mutex // serialises metadata changes
}

type artifactMeta struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	MimeType  string    `json:"mime_type"`
	Size      int64     `json:"size"`
	Digest    string    `json:"digest,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Finalized bool      `json:"finalized"`
}

const (
	zipMimeType   = "application/zip"
	signedURLTTL  = time.Hour
	maxTwirpBytes = 1 << 20
)

// RegisterV4 serves the v4 artifact service from baseDir on router.
func RegisterV4(router *http.ServeMux, baseDir string) {
	s := &serverV4{dir: filepath.Join(baseDir, "v4"), key: make([]byte, 32)}
	_, _ = rand.Read(s.key)
	s.nextID.Store(time.Now().UnixMilli())
	router.HandleFunc("POST "+ArtifactServicePath+"/CreateArtifact", twirp(s.createArtifact))
	router.HandleFunc("POST "+ArtifactServicePath+"/FinalizeArtifact", twirp(s.finalizeArtifact))
	router.HandleFunc("POST "+ArtifactServicePath+"/ListArtifacts", twirp(s.listArtifacts))
	router.HandleFunc("POST "+ArtifactServicePath+"/GetSignedArtifactURL", twirp(s.getSignedArtifactURL))
	router.HandleFunc("POST "+ArtifactServicePath+"/DeleteArtifact", twirp(s.deleteArtifact))
	router.HandleFunc("PUT "+ArtifactServicePath+"/UploadArtifact", s.uploadArtifact)
	router.HandleFunc("GET "+ArtifactServicePath+"/DownloadArtifact", s.downloadArtifact)
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
		err := json.UnmarshalRead(io.LimitReader(r.Body, maxTwirpBytes), &req)
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

func (s *serverV4) runDir(runID string) (string, error) {
	if _, err := strconv.ParseInt(runID, 10, 64); err != nil {
		return "", badRequest("run id %q is not a number", runID)
	}
	return filepath.Join(s.dir, runID), nil
}

// artifactDir encodes the name, which may hold anything a path must not.
func (s *serverV4) artifactDir(runID, name string) (string, error) {
	run, err := s.runDir(runID)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", badRequest("artifact name is empty")
	}
	return filepath.Join(run, base64.RawURLEncoding.EncodeToString([]byte(name))), nil
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

func (s *serverV4) signedURL(r *http.Request, endpoint, runID, name string) string {
	expires := strconv.FormatInt(time.Now().Add(signedURLTTL).Unix(), 10)
	query := url.Values{"run": {runID}, "name": {name}, "expires": {expires}, "sig": {s.sign(endpoint, runID, name, expires)}}
	return "http://" + r.Host + ArtifactServicePath + "/" + endpoint + "?" + query.Encode()
}

func (s *serverV4) sign(endpoint, runID, name, expires string) string {
	mac := hmac.New(sha256.New, s.key)
	for _, part := range []string{endpoint, runID, name, expires} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify returns the artifact directory a signed URL addresses.
func (s *serverV4) verify(w http.ResponseWriter, r *http.Request, endpoint string) (string, bool) {
	q := r.URL.Query()
	runID, name, expires := q.Get("run"), q.Get("name"), q.Get("expires")
	unix, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || !hmac.Equal([]byte(q.Get("sig")), []byte(s.sign(endpoint, runID, name, expires))) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return "", false
	}
	if time.Now().Unix() > unix {
		http.Error(w, "link expired", http.StatusUnauthorized)
		return "", false
	}
	dir, err := s.artifactDir(runID, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return dir, true
}

// createArtifact replaces an artifact of the same name, as Gitea does.
func (s *serverV4) createArtifact(r *http.Request, req CreateArtifactRequest) (CreateArtifactResponse, error) {
	dir, err := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	if err != nil {
		return CreateArtifactResponse{}, err
	}
	mimeType := zipMimeType
	if req.MimeType != nil && strings.Contains(*req.MimeType, "/") {
		mimeType = *req.MimeType
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		return CreateArtifactResponse{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		return CreateArtifactResponse{}, err
	}
	meta := &artifactMeta{ID: s.nextID.Add(1), Name: req.Name, MimeType: mimeType, CreatedAt: time.Now().UTC()}
	if err := writeMeta(dir, meta); err != nil {
		return CreateArtifactResponse{}, err
	}
	return CreateArtifactResponse{Ok: true, SignedUploadURL: s.signedURL(r, "UploadArtifact", req.WorkflowRunBackendID, req.Name)}, nil
}

type blockList struct {
	Blocks []string `xml:",any"` // Latest, Committed and Uncommitted all name a staged block
}

func (s *serverV4) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.verify(w, r, "UploadArtifact")
	if !ok {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		http.Error(w, "artifact not created", http.StatusNotFound)
		return
	}
	blob := filepath.Join(dir, "blob")
	var err error
	switch comp, blockID := r.URL.Query().Get("comp"), r.URL.Query().Get("blockid"); {
	case comp == "block" && blockID != "":
		err = writeFile(filepath.Join(dir, "blocks", base64.RawURLEncoding.EncodeToString([]byte(blockID))), r.Body, os.O_TRUNC)
	case comp == "block" || strings.EqualFold(comp, "appendBlock"):
		err = writeFile(blob, r.Body, os.O_APPEND)
	case strings.EqualFold(comp, "blocklist"):
		err = joinBlocks(dir, r.Body)
	case comp == "":
		err = writeFile(blob, r.Body, os.O_TRUNC)
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

func writeFile(name string, body io.Reader, flag int) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|flag, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, body)
	return errors.Join(err, f.Close())
}

func joinBlocks(dir string, body io.Reader) error {
	var list blockList
	if err := xml.NewDecoder(io.LimitReader(body, maxTwirpBytes)).Decode(&list); err != nil {
		return err
	}
	out, err := os.Create(filepath.Join(dir, "blob"))
	if err != nil {
		return err
	}
	for _, id := range list.Blocks {
		if err = appendBlock(out, filepath.Join(dir, "blocks", base64.RawURLEncoding.EncodeToString([]byte(id)))); err != nil {
			break
		}
	}
	return errors.Join(err, out.Close(), os.RemoveAll(filepath.Join(dir, "blocks")))
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

func (s *serverV4) finalizeArtifact(_ *http.Request, req FinalizeArtifactRequest) (FinalizeArtifactResponse, error) {
	dir, err := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	if err != nil {
		return FinalizeArtifactResponse{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readMeta(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return FinalizeArtifactResponse{}, notFound(req.Name)
	} else if err != nil {
		return FinalizeArtifactResponse{}, err
	}
	blob, err := os.Open(filepath.Join(dir, "blob"))
	if errors.Is(err, fs.ErrNotExist) {
		blob, err = os.Create(filepath.Join(dir, "blob")) // an empty upload sends no block
	}
	if err != nil {
		return FinalizeArtifactResponse{}, err
	}
	defer blob.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, blob)
	if err != nil {
		return FinalizeArtifactResponse{}, err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if size != int64(req.Size) {
		return FinalizeArtifactResponse{}, badRequest("size %d does not match the %d bytes uploaded", req.Size, size)
	}
	if req.Hash != nil && *req.Hash != "" && *req.Hash != digest {
		return FinalizeArtifactResponse{}, badRequest("hash %s does not match the uploaded %s", *req.Hash, digest)
	}
	meta.Size, meta.Digest, meta.Finalized = size, digest, true
	if err := writeMeta(dir, meta); err != nil {
		return FinalizeArtifactResponse{}, err
	}
	return FinalizeArtifactResponse{Ok: true, ArtifactID: Int64(meta.ID)}, nil
}

func (s *serverV4) listArtifacts(_ *http.Request, req ListArtifactsRequest) (ListArtifactsResponse, error) {
	run, err := s.runDir(req.WorkflowRunBackendID)
	if err != nil {
		return ListArtifactsResponse{}, err
	}
	entries, err := os.ReadDir(run)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ListArtifactsResponse{}, err
	}
	resp := ListArtifactsResponse{Artifacts: []MonolithArtifact{}}
	for _, entry := range entries {
		meta, err := readMeta(filepath.Join(run, entry.Name()))
		if err != nil || !meta.Finalized ||
			req.NameFilter != nil && *req.NameFilter != meta.Name || req.IDFilter != nil && int64(*req.IDFilter) != meta.ID {
			continue
		}
		resp.Artifacts = append(resp.Artifacts, MonolithArtifact{
			WorkflowRunBackendID:    req.WorkflowRunBackendID,
			WorkflowJobRunBackendID: req.WorkflowJobRunBackendID,
			DatabaseID:              Int64(meta.ID),
			Name:                    meta.Name,
			Size:                    Int64(meta.Size),
			CreatedAt:               &meta.CreatedAt,
			Digest:                  &meta.Digest,
		})
	}
	slices.SortFunc(resp.Artifacts, func(a, b MonolithArtifact) int { return int(a.DatabaseID - b.DatabaseID) })
	return resp, nil
}

func (s *serverV4) getSignedArtifactURL(r *http.Request, req GetSignedArtifactURLRequest) (GetSignedArtifactURLResponse, error) {
	dir, err := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	if err != nil {
		return GetSignedArtifactURLResponse{}, err
	}
	if meta, err := readMeta(dir); err != nil || !meta.Finalized {
		return GetSignedArtifactURLResponse{}, notFound(req.Name)
	}
	return GetSignedArtifactURLResponse{SignedURL: s.signedURL(r, "DownloadArtifact", req.WorkflowRunBackendID, req.Name)}, nil
}

func (s *serverV4) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.verify(w, r, "DownloadArtifact")
	if !ok {
		return
	}
	meta, err := readMeta(dir)
	if err != nil || !meta.Finalized {
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
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	http.ServeContent(w, r, "", meta.CreatedAt, blob)
}

func (s *serverV4) deleteArtifact(_ *http.Request, req DeleteArtifactRequest) (DeleteArtifactResponse, error) {
	dir, err := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	if err != nil {
		return DeleteArtifactResponse{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readMeta(dir)
	if err != nil {
		return DeleteArtifactResponse{}, notFound(req.Name)
	}
	return DeleteArtifactResponse{Ok: true, ArtifactID: Int64(meta.ID)}, os.RemoveAll(dir)
}
