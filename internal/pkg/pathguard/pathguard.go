// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package pathguard holds the pure checks that keep a job workdir, and what teardown deletes, inside a directory the runner owns.
package pathguard

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// IsFilesystemRoot reports whether path is "/" or a volume root such as `C:\`.
func IsFilesystemRoot(path string) bool {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, volume)
	return rest == string(filepath.Separator) || (rest == "" && volume != "")
}

// CheckParent rejects a workdir parent setting that is empty or resolves to the filesystem root.
// setting names the config key, raw is the configured value and resolved the path the runner uses.
func CheckParent(setting, raw, resolved string) error {
	if raw == "" {
		return fmt.Errorf("%s is empty; set it to a dedicated directory the runner may fill and delete", setting)
	}
	if IsFilesystemRoot(resolved) {
		return fmt.Errorf("%s %q is the filesystem root; set it to a dedicated directory the runner may fill and delete", setting, raw)
	}
	return nil
}

// CheckRepository rejects a repository that is not a plain relative path.
func CheckRepository(repository string) error {
	slashed := filepath.ToSlash(repository)
	switch {
	case repository == "":
		return errors.New("the task has no repository, so the job workdir would be the workdir parent itself")
	case strings.HasPrefix(slashed, "/") || filepath.IsAbs(repository) || filepath.VolumeName(repository) != "":
		return fmt.Errorf("the task repository %q is an absolute path; expected a relative <owner>/<repo>", repository)
	case strings.Contains("/"+slashed+"/", "/../"):
		return fmt.Errorf("the task repository %q contains a '..' segment; expected a relative <owner>/<repo>", repository)
	case filepath.Clean(filepath.FromSlash(repository)) == ".":
		return fmt.Errorf("the task repository %q resolves to the workdir parent itself; expected a relative <owner>/<repo>", repository)
	}
	return nil
}

// ValidateWorkdir requires workdir to be strictly below parent, never equal to it.
func ValidateWorkdir(parent, workdir string) error {
	if !strictlyInside(parent, workdir) {
		return fmt.Errorf("the job workdir %q is not strictly inside its parent %q; check container.workdir_parent and the task repository", workdir, parent)
	}
	return nil
}

// CheckRemovable returns an error unless path is safe for teardown to delete: not empty,
// not the filesystem root, not home, and, when owner is set, strictly inside owner.
// An empty owner skips the containment check, for a path the caller made itself.
func CheckRemovable(path, owner, home string) error {
	switch {
	case path == "":
		return errors.New("refusing to delete an empty path")
	case IsFilesystemRoot(path):
		return fmt.Errorf("refusing to delete the filesystem root %q", path)
	case home != "" && filepath.Clean(path) == filepath.Clean(home):
		return fmt.Errorf("refusing to delete the home directory %q", path)
	case owner != "" && !strictlyInside(owner, path):
		return fmt.Errorf("refusing to delete %q: it is not strictly inside %q", path, owner)
	}
	return nil
}

func strictlyInside(parent, path string) bool {
	if parent == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(path))
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
