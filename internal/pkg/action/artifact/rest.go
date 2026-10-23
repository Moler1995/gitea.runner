// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifact

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// restPageSize is Gitea's largest page, which reads `limit` and ignores the `per_page` the
// toolkit sends, so the toolkit misses every artifact past Gitea's default of 30.
const restPageSize = 50

// REST reaches artifacts of other runs through Gitea's API, with a token of the workflow's choice.
type REST struct {
	base  *url.URL
	token string
	api   *http.Client
	blob  *http.Client
	Retry Retry
	Logf  func(format string, args ...any)
}

// NewREST takes the instance as the runner reaches it, never the job's GITHUB_API_URL, so the
// token only goes to the instance.
func NewREST(instance, token string, transport http.RoundTripper) (*REST, error) {
	parsed, err := url.Parse(instance)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &REST{
		base:  parsed.JoinPath("api", "v1"),
		token: token,
		api: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // the redirect target is signed and must not get the token
		}},
		blob:  &http.Client{Transport: blobTransport{instance: parsed.Host, transport: transport}},
		Retry: DefaultRetry,
		Logf:  func(string, ...any) {},
	}, nil
}

func (r *REST) get(ctx context.Context, what string, endpoint *url.URL) (*http.Response, error) {
	return r.Retry.do(ctx, r.Logf, what, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+r.token)
		req.Header.Set("Accept", "application/json")
		return r.api.Do(req)
	})
}

// List returns the run's artifacts that have not expired, all of them or those named name.
func (r *REST) List(ctx context.Context, owner, repo string, runID int64, name string) ([]Artifact, error) {
	r.Logf("Fetching artifact list for workflow run %d in repository %s/%s", runID, owner, repo)
	var list []Artifact
	for page := 1; ; page++ {
		endpoint := r.base.JoinPath("repos", owner, repo, "actions", "runs", strconv.FormatInt(runID, 10), "artifacts")
		query := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(restPageSize)}}
		if name != "" {
			query.Set("name", name)
		}
		endpoint.RawQuery = query.Encode()
		resp, err := r.get(ctx, "list artifacts", endpoint)
		if err != nil {
			return nil, err
		}
		var body struct {
			Artifacts []struct {
				ID          int64     `json:"id"`
				Name        string    `json:"name"`
				SizeInBytes int64     `json:"size_in_bytes"`
				CreatedAt   time.Time `json:"created_at"`
				Expired     bool      `json:"expired"`
				Digest      string    `json:"digest"`
			} `json:"artifacts"`
		}
		err = json.UnmarshalRead(resp.Body, &body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list artifacts: %w", err)
		}
		for _, a := range body.Artifacts {
			if !a.Expired {
				list = append(list, Artifact{ID: a.ID, Name: a.Name, Size: a.SizeInBytes, CreatedAt: a.CreatedAt, Digest: a.Digest})
			}
		}
		if len(body.Artifacts) < restPageSize {
			r.Logf("Found %d artifact(s)", len(list))
			return list, nil
		}
	}
}

// DownloadURL returns the signed URL the artifact's zip redirects to.
func (r *REST) DownloadURL(ctx context.Context, owner, repo string, id int64) (string, error) {
	r.Logf("Downloading artifact '%d' from '%s/%s'", id, owner, repo)
	resp, err := r.get(ctx, "download artifact", r.base.JoinPath("repos", owner, repo, "actions", "artifacts", strconv.FormatInt(id, 10), "zip"))
	if err != nil {
		return "", fmt.Errorf("unable to download artifact: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", fmt.Errorf("unable to download artifact. Unexpected status: %d", resp.StatusCode)
	}
	location, err := resp.Location()
	if err != nil {
		return "", errors.New("unable to redirect to artifact download url")
	}
	return location.String(), nil
}

// Download GETs the URL DownloadURL returned. A body that sends nothing for stall fails the read.
func (r *REST) Download(ctx context.Context, signedURL string, stall time.Duration) (*http.Response, error) {
	return download(ctx, r.blob, signedURL, stall)
}
