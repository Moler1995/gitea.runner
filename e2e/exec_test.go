// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testExecArtifacts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	bin := filepath.Join(t.TempDir(), "gitea-runner")
	if out, err := exec.CommandContext(ctx, cmp.Or(os.Getenv("GO"), "go"), "build", "-o", bin, "..").CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}

	// upload-artifact needs GITHUB_REPOSITORY, which exec derives from the origin remote
	workdir := t.TempDir()
	workflow, err := os.ReadFile("testdata/workflows/exec_artifact.yml")
	if err != nil {
		t.Fatalf("read workflow fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workdir, ".gitea/workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, ".gitea/workflows/exec_artifact.yml"), workflow, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://example.com/e2e/exec.git"},
		{"-c", "user.name=e2e", "-c", "user.email=e2e@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	artifacts := t.TempDir()
	cmd := exec.CommandContext(ctx, bin, "exec", "-C", workdir,
		"-i", os.Getenv("E2E_JOB_IMAGE"), "--artifact-server-path", artifacts)
	// exec reuses the artifact service of a surrounding CI job
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "ACTIONS_") })
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exec: %v\n%s", err, out)
	}
	if blobs, _ := filepath.Glob(filepath.Join(artifacts, "v4", "*", "*", "blob")); len(blobs) != 1 {
		t.Fatalf("want one v4 artifact blob, got %v", blobs)
	}
}
