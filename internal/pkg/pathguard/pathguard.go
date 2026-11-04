// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package pathguard holds the pure checks that keep a job workdir, and what teardown deletes, inside a directory the runner owns.
package pathguard

import (
	"errors"
	"fmt"
	"os"
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

// CheckParent rejects a workdir parent setting that is empty or resolves to the filesystem root
// or the home directory. setting names the config key, raw is the configured value and resolved
// the absolute path the runner uses. An empty home skips the home check.
func CheckParent(setting, raw, resolved, home string) error {
	if raw == "" {
		return fmt.Errorf("%s is empty; set it to a dedicated directory the runner may fill and delete", setting)
	}
	if IsFilesystemRoot(resolved) {
		return fmt.Errorf("%s %q is the filesystem root; set it to a dedicated directory the runner may fill and delete", setting, raw)
	}
	if home != "" && filepath.Clean(resolved) == filepath.Clean(home) {
		return fmt.Errorf("%s %q is the home directory; set it to a dedicated directory the runner may fill and delete", setting, raw)
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
// The checks run on the path as given and again with symlinks resolved, so a link
// that points at the root, home or outside owner is refused too.
func CheckRemovable(path, owner, home string) error {
	if path == "" {
		return errors.New("refusing to delete an empty path")
	}
	if err := checkRemovable(path, "", home); err != nil {
		return err
	}
	resolved := resolve(path)
	if err := checkRemovable(resolved, resolveIfSet(owner), resolveIfSet(home)); err != nil {
		if resolved == filepath.Clean(path) {
			return err
		}
		return fmt.Errorf("%w (%q resolves through a symlink to %q)", err, path, resolved)
	}
	return nil
}

func checkRemovable(path, owner, home string) error {
	switch {
	case IsFilesystemRoot(path):
		return fmt.Errorf("refusing to delete the filesystem root %q", path)
	case home != "" && filepath.Clean(path) == filepath.Clean(home):
		return fmt.Errorf("refusing to delete the home directory %q", path)
	case owner != "" && !strictlyInside(owner, path):
		return fmt.Errorf("refusing to delete %q: it is not strictly inside %q", path, owner)
	}
	return nil
}

func resolveIfSet(path string) string {
	if path == "" {
		return ""
	}
	return resolve(path)
}

// resolve returns path with symlinks resolved. A path that does not exist yet is resolved
// through its nearest existing ancestor, and the missing tail is appended unchanged.
func resolve(path string) string {
	return resolveDepth(path, 0)
}

// maxLinkDepth bounds dangling-link chains, which EvalSymlinks cannot follow.
const maxLinkDepth = 40

func resolveDepth(path string, depth int) string {
	clean := filepath.Clean(path)
	tail := ""
	for current := clean; ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, tail)
		}
		// A dangling link still names where it points, and a later write through it would go there.
		if target, err := os.Readlink(current); err == nil && depth < maxLinkDepth {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(current), target)
			}
			return filepath.Join(resolveDepth(target, depth+1), tail)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return clean
		}
		tail = filepath.Join(filepath.Base(current), tail)
		current = parent
	}
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
