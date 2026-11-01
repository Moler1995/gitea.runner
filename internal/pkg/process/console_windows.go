// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"sync"

	"golang.org/x/sys/windows"
)

var utf8Console sync.Once

// UTF8Console switches the console steps share to UTF-8 once, as GitHub's runner runs chcp 65001 before its steps.
func UTF8Console() {
	utf8Console.Do(func() {
		_ = windows.SetConsoleCP(65001)
		_ = windows.SetConsoleOutputCP(65001)
	})
}
