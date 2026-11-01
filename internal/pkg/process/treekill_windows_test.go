// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestTreeKillWaitTrackingLeftoverOutput(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "tracked", true: "opted out"}[keep], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessHelper$")
			cmd.Env = append(os.Environ(), "GITEA_PROCESS_HELPER=orphan")
			cmd.SysProcAttr = SysProcAttr("", false)
			input, err := cmd.StdinPipe()
			require.NoError(t, err)
			reader, writer := io.Pipe()
			cmd.Stdout = writer
			stop := context.AfterFunc(ctx, func() {
				_ = input.Close()
				_ = reader.Close()
				_ = writer.Close()
			})
			t.Cleanup(func() {
				stop()
				_ = input.Close()
				_ = reader.Close()
				_ = writer.Close()
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			tree := NewTreeKill(cmd)
			tree.KeepLeftovers = keep
			cmd.WaitDelay = 50 * time.Millisecond
			require.NoError(t, cmd.Start())
			killer, err := tree.Capture(cmd.Process)
			require.NoError(t, err)
			defer killer.Close()
			_, err = input.Write([]byte{1})
			require.NoError(t, err)
			var childPID int
			_, err = fmt.Fscanln(reader, &childPID)
			require.NoError(t, err)
			handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(childPID))
			require.NoError(t, err)
			t.Cleanup(func() {
				_ = windows.TerminateProcess(handle, 1)
				_, _ = windows.WaitForSingleObject(handle, 5000)
				_ = windows.CloseHandle(handle)
			})
			require.NoError(t, tree.Wait(ctx, cmd))
			if keep {
				result, err := windows.WaitForSingleObject(handle, 0)
				require.NoError(t, err)
				require.EqualValues(t, windows.WAIT_TIMEOUT, result)
			} else {
				result, err := windows.WaitForSingleObject(handle, 5000)
				require.NoError(t, err)
				require.EqualValues(t, windows.WAIT_OBJECT_0, result)
			}
		})
	}
}
