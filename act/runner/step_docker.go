// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/kballard/go-shellquote"
)

type stepDocker struct {
	Step       *model.Step
	RunContext *RunContext
	env        map[string]string
}

func (sd *stepDocker) pre() common.Executor { return common.NewPipelineExecutor() }

func (sd *stepDocker) main() common.Executor {
	sd.env = map[string]string{}

	return runStepExecutor(sd, stepStageMain, sd.runUsesContainer)
}

func (sd *stepDocker) post() common.Executor { return common.NewPipelineExecutor() }

func (sd *stepDocker) getRunContext() *RunContext {
	return sd.RunContext
}

func (sd *stepDocker) getStepModel() *model.Step {
	return sd.Step
}

func (sd *stepDocker) getEnv() *map[string]string {
	return &sd.env
}

func (sd *stepDocker) getIfExpression(_ context.Context, _ stepStage) string {
	return sd.Step.If.Value
}

func (sd *stepDocker) runUsesContainer(ctx context.Context) error {
	rc := sd.RunContext
	if rc.isWindowsHost() {
		return errWindowsHostContainer
	}
	if rc.kubernetes {
		return errKubernetesDocker
	}
	cmd, err := shellquote.Split(stepInput(sd.Step, "args"))
	if err != nil {
		return err
	}

	var entrypoint []string
	if entry := stepInput(sd.Step, "entrypoint"); entry != "" {
		entrypoint = []string{entry}
	}
	input := stepContainerInput(ctx, sd, strings.TrimPrefix(sd.Step.Uses, "docker://"), cmd, entrypoint, "")
	return rc.runStepContainer(ctx, ContainerNewContainer(input), nil, rc.Config.ForcePull)
}

func (rc *RunContext) runStepContainer(ctx context.Context, stepContainer container.Container, prepImage common.Executor, forcePull bool) error {
	if stepContainer == nil {
		return fmt.Errorf("running a Docker action: %w", errors.ErrUnsupported)
	}
	return common.NewPipelineExecutor(
		prepImage,
		stepContainer.Pull(forcePull),
		stepContainer.Remove(),
		stepContainer.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
		stepContainer.Start(true),
	).Finally(stepContainer.Close())(ctx)
}

var ContainerNewContainer = container.NewContainer
