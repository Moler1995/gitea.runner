// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package artifact talks to the artifact services the built-in artifact actions use: the v4
// results service of the job's own run, and Gitea's REST API for other runs.
package artifact

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Artifact is an artifact as either service lists it.
type Artifact struct {
	ID        int64
	Name      string
	Size      int64
	CreatedAt time.Time
	Digest    string // empty when the service reports none, Gitea never does
}

// Retry is how requests retry, the toolkit's schedule by default.
type Retry struct {
	Attempts   int
	Base       time.Duration
	Multiplier float64
	MaxWait    time.Duration
}

var DefaultRetry = Retry{Attempts: 5, Base: 8 * time.Second, Multiplier: 1.5, MaxWait: 2 * time.Minute}

// Client is the v4 results service of one job.
type Client struct {
	base         *url.URL
	token        string
	runID, jobID string
	api, blob    *http.Client
	Retry        Retry
	Logf         func(format string, args ...any)
}

// New reaches base, the results service as the runner reaches it, through transport, which
// carries the instance's TLS settings. Signed URLs elsewhere use the default transport.
func New(base, token string, transport http.RoundTripper) (*Client, error) {
	if base == "" {
		return nil, errors.New("no artifact service is configured for this runner")
	}
	parsed, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("ACTIONS_RUNTIME_TOKEN is not set")
	}
	runID, jobID, err := backendIDs(token)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{
		base: parsed, token: token, runID: runID, jobID: jobID,
		api:   &http.Client{Transport: transport},
		blob:  &http.Client{Transport: blobTransport{instance: parsed.Host, transport: transport}},
		Retry: DefaultRetry,
		Logf:  func(string, ...any) {},
	}, nil
}

// backendIDs reads the run and job IDs the results service expects from the unverified claims of
// a runtime token, the same way the toolkit does.
func backendIDs(token string) (runID, jobID string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", errors.New("the runtime token is not a JWT, artifacts v4 need Gitea 1.22 or later")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", "", err
	}
	var claims struct {
		Scp string `json:"scp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", err
	}
	for scope := range strings.FieldsSeq(claims.Scp) {
		if fields := strings.Split(scope, ":"); fields[0] == "Actions.Results" && len(fields) == 3 {
			return fields[1], fields[2], nil
		}
	}
	return "", "", errors.New("the runtime token has no Actions.Results scope")
}

// blobTransport only offers the instance's transport to the instance, a signed URL can point at
// object storage.
type blobTransport struct {
	instance  string
	transport http.RoundTripper
}

func (t blobTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.instance {
		return t.transport.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// StatusError is a response the service answered with.
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("(%d) %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("(%d) %s: %s", e.Status, http.StatusText(e.Status), e.Msg)
}

// IsNotFound reports whether err is a 404 from the service.
func IsNotFound(err error) bool {
	var status *StatusError
	return errors.As(err, &status) && status.Status == http.StatusNotFound
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// do sends the request send builds until it succeeds or fails for good, and returns the
// successful response for the caller to close.
func (r Retry) do(ctx context.Context, logf func(string, ...any), what string, send func() (*http.Response, error)) (*http.Response, error) {
	var waited time.Duration
	for attempt := 0; ; attempt++ {
		resp, err := send()
		var wait time.Duration
		if err == nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return resp, nil
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			err = &StatusError{Status: resp.StatusCode, Msg: twirpMessage(body)}
			if !retryable(resp.StatusCode) {
				return nil, err
			}
			if seconds, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil && resp.StatusCode == http.StatusTooManyRequests {
				wait = time.Duration(seconds) * time.Second
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt+1 >= r.Attempts {
			return nil, fmt.Errorf("%s failed after %d attempts: %w", what, r.Attempts, err)
		}
		if wait == 0 {
			wait = time.Duration(float64(r.Base) * pow(r.Multiplier, attempt))
		}
		if waited+wait > r.MaxWait {
			return nil, fmt.Errorf("%s: a retry wait of %s would exceed %s: %w", what, wait, r.MaxWait, err)
		}
		logf("Attempt %d of %d failed with error: %v. Retrying request in %d ms...", attempt+1, r.Attempts, err, wait.Milliseconds())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		waited += wait
	}
}

func pow(base float64, exp int) float64 {
	result := 1.0
	for range exp {
		result *= base
	}
	return result
}

// twirpMessage is the msg of a twirp error, or the body Gitea answers in plain text.
func twirpMessage(body []byte) string {
	var twirpErr struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal(body, &twirpErr) == nil && twirpErr.Msg != "" {
		return twirpErr.Msg
	}
	return strings.TrimSpace(string(body))
}

type ( // the ArtifactService messages in the protobuf JSON mapping, requests with the proto names the toolkit sends
	backendIDsMessage struct {
		WorkflowRunBackendID    string `json:"workflow_run_backend_id"`
		WorkflowJobRunBackendID string `json:"workflow_job_run_backend_id"`
	}
	namedRequest struct {
		backendIDsMessage
		Name string `json:"name"`
	}
	createRequest struct {
		namedRequest
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
		Version   int32      `json:"version"`
		MimeType  string     `json:"mime_type,omitempty"` // Gitea before 1.26 rejects the field
	}
	finalizeRequest struct {
		namedRequest
		Size int64  `json:"size,string"`
		Hash string `json:"hash"`
	}
	listRequest struct {
		backendIDsMessage
		NameFilter *string `json:"name_filter,omitempty"`
		IDFilter   *int64  `json:"id_filter,omitempty,string"`
	}
	idResponse struct {
		Ok         bool  `json:"ok"`
		ArtifactID int64 `json:"artifactId,string"`
	}
)

func call[Req, Resp any](ctx context.Context, c *Client, method string, req Req) (Resp, error) {
	var resp Resp
	body, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	endpoint := c.base.JoinPath("/twirp/github.actions.results.api.v1.ArtifactService", method).String()
	httpResp, err := c.Retry.do(ctx, c.Logf, method, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
		return c.api.Do(httpReq)
	})
	if err != nil {
		return resp, fmt.Errorf("failed to %s: %w", method, err)
	}
	defer httpResp.Body.Close()
	if err := json.UnmarshalRead(httpResp.Body, &resp); err != nil {
		return resp, fmt.Errorf("failed to %s: %w", method, err)
	}
	return resp, nil
}

func (c *Client) named(name string) namedRequest {
	return namedRequest{backendIDsMessage{c.runID, c.jobID}, name}
}

// Create starts an upload and returns where the content goes.
func (c *Client) Create(ctx context.Context, name, mimeType string, expiresAt *time.Time) (string, error) {
	resp, err := call[createRequest, struct {
		Ok              bool   `json:"ok"`
		SignedUploadURL string `json:"signedUploadUrl"`
	}](ctx, c, "CreateArtifact", createRequest{c.named(name), expiresAt, 7, mimeType})
	if err == nil && !resp.Ok {
		err = errors.New("CreateArtifact: response from backend was not ok")
	}
	return resp.SignedUploadURL, err
}

func (c *Client) Finalize(ctx context.Context, name string, size int64, digest string) (int64, error) {
	resp, err := call[finalizeRequest, idResponse](ctx, c, "FinalizeArtifact", finalizeRequest{c.named(name), size, digest})
	if err == nil && !resp.Ok {
		err = errors.New("FinalizeArtifact: response from backend was not ok")
	}
	return resp.ArtifactID, err
}

// List returns the run's artifacts, filtered by name or ID when given.
func (c *Client) List(ctx context.Context, name *string, id *int64) ([]Artifact, error) {
	resp, err := call[listRequest, struct {
		Artifacts []struct {
			DatabaseID int64     `json:"databaseId,string"`
			Name       string    `json:"name"`
			Size       int64     `json:"size,string"`
			CreatedAt  time.Time `json:"createdAt"`
			Digest     string    `json:"digest"`
		} `json:"artifacts"`
	}](ctx, c, "ListArtifacts", listRequest{backendIDsMessage{c.runID, c.jobID}, name, id})
	if err != nil {
		return nil, err
	}
	list := make([]Artifact, 0, len(resp.Artifacts))
	for _, a := range resp.Artifacts {
		list = append(list, Artifact{ID: a.DatabaseID, Name: a.Name, Size: a.Size, CreatedAt: a.CreatedAt, Digest: a.Digest})
	}
	return list, nil
}

func (c *Client) SignedURL(ctx context.Context, name string) (string, error) {
	resp, err := call[namedRequest, struct {
		SignedURL string `json:"signedUrl"`
	}](ctx, c, "GetSignedArtifactURL", c.named(name))
	return resp.SignedURL, err
}

func (c *Client) Delete(ctx context.Context, name string) (int64, error) {
	resp, err := call[namedRequest, idResponse](ctx, c, "DeleteArtifact", c.named(name))
	return resp.ArtifactID, err
}

// ScrubQuery drops the signature from a URL before it is logged.
func ScrubQuery(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "<unparsable URL>"
	}
	parsed.RawQuery = ""
	return parsed.String()
}
