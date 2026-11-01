// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gitea.com/gitea/runner/act/common/logging"

	"golang.org/x/sys/windows/svc"
)

func runService(ctx context.Context, run func(context.Context) error) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service: %w", err)
	}
	if !isService {
		return run(ctx)
	}

	handler := &daemonService{ctx: ctx, run: func(ctx context.Context) error {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if err := os.Chdir(filepath.Dir(executable)); err != nil {
			return err
		}
		logFile, err := os.OpenFile("gitea-runner.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open service log: %w", err)
		}
		defer func() { _ = logFile.Close() }()
		previousOutput := logging.Output()
		logging.SetOutput(logFile)
		defer logging.SetOutput(previousOutput)
		err = run(ctx)
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		if err != nil {
			logging.WithError(err).Error("runner daemon failed")
		}
		return err
	}}
	if err := svc.Run("", handler); err != nil { // SCM supplies the name for an own-process service.
		return fmt.Errorf("run Windows service: %w", err)
	}
	return handler.err
}

type daemonService struct {
	ctx context.Context
	run func(context.Context) error
	err error
}

func (s *daemonService) Execute(_args []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	status := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	changes <- status
	for {
		select {
		case s.err = <-done:
			if status.State != svc.StopPending {
				changes <- svc.Status{State: svc.StopPending}
			}
			if s.err != nil {
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- status
			case svc.Stop, svc.Shutdown:
				if status.State != svc.StopPending {
					status = svc.Status{State: svc.StopPending}
					changes <- status
					cancel()
				}
			}
		}
	}
}
