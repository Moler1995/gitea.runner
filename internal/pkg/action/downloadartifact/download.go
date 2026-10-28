// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package downloadartifact works like actions/download-artifact v8.
package downloadartifact

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/artifact"

	"golang.org/x/sync/errgroup"
)

var digestBehaviors = []string{"ignore", "info", "warn", "error"}

const (
	parallelDownloads = 5
	downloadAttempts  = 5
)

// Overridable in tests.
var (
	retryDelay    = 5 * time.Second
	downloadStall = 30 * time.Second
)

// source is where the artifacts come from, the job's own run or another one.
type source interface {
	list(ctx context.Context, name string) ([]artifact.Artifact, error)
	open(ctx context.Context, a artifact.Artifact) (*http.Response, error)
}

func Main(ctx context.Context, c *action.Context) error {
	if err := run(ctx, c); err != nil {
		return action.Failf("Unable to download artifact(s): %w", err)
	}
	return nil
}

func run(ctx context.Context, c *action.Context) error {
	c.ApplyDefaults("name", "", "artifact-ids", "", "path", "", "pattern", "", "merge-multiple", "false", "github-token", "",
		"repository", c.Github.Repository, "run-id", c.Github.RunID, "skip-decompress", "false", "digest-mismatch", "error")
	name, ids, pattern, token := c.Input("name"), c.Input("artifact-ids"), c.Input("pattern"), c.Input("github-token")
	mergeMultiple, err := c.BoolInput("merge-multiple")
	if err != nil {
		return err
	}
	skipDecompress, err := c.BoolInput("skip-decompress")
	if err != nil {
		return err
	}
	digestMismatch := c.Input("digest-mismatch")
	if !slices.Contains(digestBehaviors, digestMismatch) {
		return action.Failf("Invalid value for 'digest-mismatch': '%s'. Valid options are: %s", digestMismatch, strings.Join(digestBehaviors, ", "))
	}
	dest := cmp.Or(c.Input("path"), c.Workspace)
	if strings.HasPrefix(dest, "~") {
		home, err := c.Home(ctx)
		if err != nil {
			return err
		}
		dest = home + dest[1:]
	}
	dest = c.Resolve(dest)
	if name != "" && ids != "" {
		return action.Failf("Inputs 'name' and 'artifact-ids' cannot be used together. Please specify only one.")
	}

	src, err := newSource(c, token)
	if err != nil {
		return err
	}
	var artifacts []artifact.Artifact
	switch {
	case name != "":
		c.Infof("Downloading single artifact")
		found, err := src.list(ctx, name)
		if err != nil {
			return err
		}
		found = latest(found)
		if len(found) == 0 {
			return action.Failf("Artifact not found for name: %s\nPlease ensure that your artifact is not expired and the artifact was uploaded using a compatible version of toolkit/upload-artifact.", name)
		}
		artifacts = found[:1]
	case ids != "":
		c.Infof("Downloading artifacts by ID")
		if artifacts, err = byIDs(ctx, c, src, ids); err != nil {
			return err
		}
	default:
		all, err := src.list(ctx, "")
		if err != nil {
			return err
		}
		artifacts = latest(all)
		if pattern != "" {
			c.Infof("Filtering artifacts by pattern '%s'", pattern)
			artifacts = slices.DeleteFunc(artifacts, func(a artifact.Artifact) bool { return !matchName(pattern, a.Name) })
		} else {
			c.Infof("No input name, artifact-ids or pattern filtered specified, downloading all artifacts")
			if !mergeMultiple {
				c.Infof("An extra directory with the artifact name will be created for each download")
			}
		}
	}
	if len(artifacts) > 0 {
		c.Infof("Preparing to download the following artifacts:")
		for _, a := range artifacts {
			c.Infof("- %s (ID: %d, Size: %d, Expected Digest: %s)", a.Name, a.ID, a.Size, cmp.Or(a.Digest, "undefined"))
		}
	}

	mismatched := make([]bool, len(artifacts))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(parallelDownloads)
	for i, a := range artifacts {
		target := dest
		if name == "" && !mergeMultiple && len(artifacts) > 1 {
			target = c.Join(dest, a.Name)
		}
		group.Go(func() error {
			mismatch, err := fetch(groupCtx, c, src, a, target, skipDecompress)
			if err != nil {
				return err
			}
			if mismatch {
				mismatched[i] = true
				message := fmt.Sprintf("Artifact '%s' digest validation failed. Please verify the integrity of the artifact.", a.Name)
				switch digestMismatch {
				case "info":
					c.Infof("%s", message)
				case "warn":
					c.Warning("%s", message)
				}
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	var mismatches []string
	for i, a := range artifacts {
		if mismatched[i] {
			mismatches = append(mismatches, a.Name)
		}
	}
	if len(mismatches) > 0 && digestMismatch == "error" {
		return action.Failf("Digest validation failed for artifact(s): %s. Use 'digest-mismatch: warn' to continue on mismatch.", strings.Join(mismatches, ", "))
	}
	c.Infof("Total of %d artifact(s) downloaded", len(artifacts))
	if err := c.SetOutputs(ctx, "download-path", dest); err != nil {
		return err
	}
	c.Infof("Download artifact has finished successfully")
	return nil
}

func byIDs(ctx context.Context, c *action.Context, src source, input string) ([]artifact.Artifact, error) {
	var wanted []int64
	for id := range strings.SplitSeq(input, ",") {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		parsed, ok := parseInt(id)
		if !ok {
			return nil, action.Failf("Invalid artifact ID: '%s'. Must be a number.", id)
		}
		wanted = append(wanted, parsed)
	}
	if len(wanted) == 0 {
		return nil, action.Failf("No valid artifact IDs provided in 'artifact-ids' input")
	}
	all, err := src.list(ctx, "")
	if err != nil {
		return nil, err
	}
	found := slices.DeleteFunc(latest(all), func(a artifact.Artifact) bool { return !slices.Contains(wanted, a.ID) })
	if len(found) == 0 {
		return nil, action.Failf("None of the provided artifact IDs were found")
	}
	if len(found) < len(wanted) {
		var missing []string
		for _, id := range wanted {
			if !slices.ContainsFunc(found, func(a artifact.Artifact) bool { return a.ID == id }) {
				missing = append(missing, strconv.FormatInt(id, 10))
			}
		}
		c.Warning("Could not find the following artifact IDs: %s", strings.Join(missing, ", "))
	}
	return found, nil
}

// latest keeps the newest artifact of each name, the one with the highest ID, newest first.
func latest(artifacts []artifact.Artifact) []artifact.Artifact {
	sorted := slices.Clone(artifacts)
	slices.SortStableFunc(sorted, func(a, b artifact.Artifact) int { return cmp.Compare(b.ID, a.ID) })
	seen := map[string]bool{}
	return slices.DeleteFunc(sorted, func(a artifact.Artifact) bool {
		duplicate := seen[a.Name]
		seen[a.Name] = true
		return duplicate
	})
}

// parseInt reads a leading integer the way JavaScript's parseInt(s, 10) does.
func parseInt(s string) (int64, bool) {
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	digits := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == digits {
		return 0, false
	}
	parsed, err := strconv.ParseInt(s[:end], 10, 64)
	return parsed, err == nil
}

// fetch downloads one artifact into target, retrying as the toolkit does, and reports a digest
// that does not match the one the service listed.
func fetch(ctx context.Context, c *action.Context, src source, a artifact.Artifact, target string, skipDecompress bool) (bool, error) {
	var digest string
	var err error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		c.Infof("Starting download of artifact to: %s", target)
		if digest, err = fetchOnce(ctx, c, src, a, target, skipDecompress); err == nil {
			break
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if _, api := errors.AsType[*artifact.StatusError](err); api {
			return false, err // the service gave up already, only the blob download retries
		}
		if attempt == downloadAttempts {
			return false, action.Failf("Unable to download and extract artifact: Artifact download failed after %d retries: %w", attempt, err)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(retryDelay):
		}
	}
	c.Infof("Artifact download completed successfully.")
	return a.Digest != "" && a.Digest != digest, nil
}

func fetchOnce(ctx context.Context, c *action.Context, src source, a artifact.Artifact, target string, skipDecompress bool) (string, error) {
	resp, err := src.open(ctx, a)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	spooled, err := spool(resp.Body)
	if err != nil {
		return "", err
	}
	defer spooled.remove()
	c.Infof("SHA256 digest of downloaded artifact is %s", strings.TrimPrefix(spooled.digest, "sha256:"))
	if isZip(resp) && !skipDecompress {
		return spooled.digest, extractZip(ctx, c, spooled, target)
	}
	fileName := rawFileName(resp.Header.Get("Content-Disposition"))
	c.Infof("Downloading raw file (non-zip) to: %s", c.Join(target, fileName))
	return spooled.digest, writeRaw(ctx, c, spooled, target, fileName)
}
