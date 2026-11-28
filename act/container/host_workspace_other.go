// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !linux

package container

import "errors"

func (e *HostEnvironment) UseWorkspace(_root, _repository string) error {
	return errors.ErrUnsupported
}
