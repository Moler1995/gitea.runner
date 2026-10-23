// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package artifact talks to the artifact services the built-in artifact actions use: the v4
// results service of the job's own run, and Gitea's REST API for other runs.
package artifact

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/artifacts"
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
	runID, jobID, err := artifacts.BackendIDs(token)
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

func call[Req, Resp any](ctx context.Context, c *Client, method string, req Req) (Resp, error) {
	var resp Resp
	body, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	endpoint := c.base.JoinPath(artifacts.ArtifactServicePath, method).String()
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

// Create starts an upload and returns where the content goes. mimeType is only sent when set, as
// Gitea before 1.26 rejects the field.
func (c *Client) Create(ctx context.Context, name, mimeType string, expiresAt *time.Time) (string, error) {
	req := artifacts.CreateArtifactRequest{WorkflowRunBackendID: c.runID, WorkflowJobRunBackendID: c.jobID, Name: name, ExpiresAt: expiresAt, Version: 7}
	if mimeType != "" {
		req.MimeType = &mimeType
	}
	resp, err := call[artifacts.CreateArtifactRequest, artifacts.CreateArtifactResponse](ctx, c, "CreateArtifact", req)
	if err == nil && !resp.Ok {
		err = errors.New("CreateArtifact: response from backend was not ok")
	}
	return resp.SignedUploadURL, err
}

func (c *Client) Finalize(ctx context.Context, name string, size int64, digest string) (int64, error) {
	req := artifacts.FinalizeArtifactRequest{WorkflowRunBackendID: c.runID, WorkflowJobRunBackendID: c.jobID, Name: name, Size: artifacts.Int64(size), Hash: &digest}
	resp, err := call[artifacts.FinalizeArtifactRequest, artifacts.FinalizeArtifactResponse](ctx, c, "FinalizeArtifact", req)
	if err == nil && !resp.Ok {
		err = errors.New("FinalizeArtifact: response from backend was not ok")
	}
	return int64(resp.ArtifactID), err
}

// List returns the run's artifacts, filtered by name or ID when given.
func (c *Client) List(ctx context.Context, name *string, id *int64) ([]Artifact, error) {
	req := artifacts.ListArtifactsRequest{WorkflowRunBackendID: c.runID, WorkflowJobRunBackendID: c.jobID, NameFilter: name}
	if id != nil {
		filter := artifacts.Int64(*id)
		req.IDFilter = &filter
	}
	resp, err := call[artifacts.ListArtifactsRequest, artifacts.ListArtifactsResponse](ctx, c, "ListArtifacts", req)
	if err != nil {
		return nil, err
	}
	list := make([]Artifact, 0, len(resp.Artifacts))
	for _, a := range resp.Artifacts {
		item := Artifact{ID: int64(a.DatabaseID), Name: a.Name, Size: int64(a.Size)}
		if a.CreatedAt != nil {
			item.CreatedAt = *a.CreatedAt
		}
		if a.Digest != nil {
			item.Digest = *a.Digest
		}
		list = append(list, item)
	}
	return list, nil
}

func (c *Client) SignedURL(ctx context.Context, name string) (string, error) {
	req := artifacts.GetSignedArtifactURLRequest{WorkflowRunBackendID: c.runID, WorkflowJobRunBackendID: c.jobID, Name: name}
	resp, err := call[artifacts.GetSignedArtifactURLRequest, artifacts.GetSignedArtifactURLResponse](ctx, c, "GetSignedArtifactURL", req)
	return resp.SignedURL, err
}

func (c *Client) Delete(ctx context.Context, name string) (int64, error) {
	req := artifacts.DeleteArtifactRequest{WorkflowRunBackendID: c.runID, WorkflowJobRunBackendID: c.jobID, Name: name}
	resp, err := call[artifacts.DeleteArtifactRequest, artifacts.DeleteArtifactResponse](ctx, c, "DeleteArtifact", req)
	return int64(resp.ArtifactID), err
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
