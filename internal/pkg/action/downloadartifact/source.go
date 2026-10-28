// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package downloadartifact

import (
	"context"
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
		return runSource{client}, nil
	}
	parts := append(strings.Split(c.Input("repository"), "/"), "")
	owner, repo := parts[0], parts[1]
	if owner == "" || repo == "" {
		return nil, action.Failf("Invalid repository: '%s'. Must be in format owner/repo", c.Input("repository"))
	}
	runID, err := action.ParseInt(c.Input("run-id"))
	if err != nil || runID <= 0 { // Gitea lists the artifacts of every run for those
		return nil, action.Failf("Invalid run-id: '%s'. Must be a number.", c.Input("run-id"))
	}
	rest, err := artifact.NewREST(c.ResultsURL, token, c.Transport)
	if err != nil {
		return nil, err
	}
	rest.Logf = c.Infof
	return otherRunSource{rest, owner, repo, int64(runID)}, nil
}

type runSource struct{ *artifact.Client }

func (s runSource) list(ctx context.Context, name string) ([]artifact.Artifact, error) {
	list, err := s.List(ctx, name)
	if err == nil {
		s.Logf("Found %d artifact(s)", len(list))
	}
	return list, err
}

func (s runSource) signedURL(ctx context.Context, art artifact.Artifact) (string, error) {
	return s.SignedURL(ctx, art.Name)
}

type otherRunSource struct {
	*artifact.REST
	owner, repo string
	runID       int64
}

func (s otherRunSource) list(ctx context.Context, name string) ([]artifact.Artifact, error) {
	return s.List(ctx, s.owner, s.repo, s.runID, name)
}

func (s otherRunSource) signedURL(ctx context.Context, art artifact.Artifact) (string, error) {
	return s.DownloadURL(ctx, s.owner, s.repo, art.ID)
}
