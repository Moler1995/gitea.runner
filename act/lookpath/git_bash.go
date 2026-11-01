// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package lookpath

import (
	"path/filepath"
	"strings"
)

// lookPathWindows falls back to Git's bash when PATH has no bash or only the System32 WSL launcher.
func lookPathWindows(file string, env map[string]string, lookup func(string, map[string]string) (string, error)) (string, error) {
	found, err := lookup(file, env)
	isBash := strings.EqualFold(file, "bash") || strings.EqualFold(file, "bash.exe")
	if !isBash || err == nil && !strings.EqualFold(filepath.Base(filepath.Dir(found)), "System32") {
		return found, err
	}
	if git, gitErr := lookup("git.exe", env); gitErr == nil {
		for _, relative := range []string{"../bin/bash.exe", "../usr/bin/bash.exe", "bash.exe"} {
			if bash, bashErr := lookup(filepath.Join(filepath.Dir(git), relative), env); bashErr == nil {
				return bash, nil
			}
		}
	}
	return found, err
}
