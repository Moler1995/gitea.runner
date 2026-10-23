// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package downloadartifact

import (
	"cmp"
	"context"
	"net/http"
	"strings"

	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/artifact"
)

func newSource(c *action.Context, token string) (source, error) {
	if token == "" {
		client, err := artifact.New(c.ResultsURL, c.RuntimeToken, c.Transport)
		if err != nil {
			return nil, err
		}
		client.Logf = c.Infof
		return &runSource{c: c, client: client}, nil
	}
	repository := cmp.Or(c.Input("repository"), c.Github.Repository)
	owner, repo, _ := strings.Cut(repository, "/")
	if owner == "" || repo == "" {
		return nil, action.Failf("Invalid repository: '%s'. Must be in format owner/repo", repository)
	}
	runID, ok := parseInt(cmp.Or(c.Input("run-id"), c.Github.RunID))
	if !ok {
		return nil, action.Failf("Invalid run-id: '%s'. Must be a number.", c.Input("run-id"))
	}
	rest, err := artifact.NewREST(c.ResultsURL, token, c.Transport)
	if err != nil {
		return nil, err
	}
	rest.Logf = c.Infof
	return &otherRunSource{c: c, rest: rest, owner: owner, repo: repo, runID: runID}, nil
}

// runSource is the job's own run, through the results service.
type runSource struct {
	c      *action.Context
	client *artifact.Client
}

func (s *runSource) list(ctx context.Context, name string) ([]artifact.Artifact, error) {
	var filter *string
	if name != "" {
		filter = &name
	}
	list, err := s.client.List(ctx, filter, nil)
	if err == nil {
		s.c.Infof("Found %d artifact(s)", len(list))
	}
	return list, err
}

func (s *runSource) open(ctx context.Context, a artifact.Artifact) (*http.Response, error) {
	signed, err := s.client.SignedURL(ctx, a.Name)
	if err != nil {
		return nil, err
	}
	s.c.Infof("Redirecting to blob download url: %s", artifact.ScrubQuery(signed))
	return s.client.Download(ctx, signed, downloadStall)
}

// otherRunSource is any run the token can read, through Gitea's API.
type otherRunSource struct {
	c           *action.Context
	rest        *artifact.REST
	owner, repo string
	runID       int64
}

func (s *otherRunSource) list(ctx context.Context, name string) ([]artifact.Artifact, error) {
	return s.rest.List(ctx, s.owner, s.repo, s.runID, name)
}

func (s *otherRunSource) open(ctx context.Context, a artifact.Artifact) (*http.Response, error) {
	location, err := s.rest.DownloadURL(ctx, s.owner, s.repo, a.ID)
	if err != nil {
		return nil, err
	}
	s.c.Infof("Redirecting to blob download url: %s", artifact.ScrubQuery(location))
	return s.rest.Download(ctx, location, downloadStall)
}
