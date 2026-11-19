// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/act/container/hooks"

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

	return runStepExecutor(sd, stepStageMain, sd.runUsesContainer())
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

// resolveContainerStepInputs extracts the docker image, args, and entrypoint
// from a uses: docker://... step, interpolating ${{ }} expressions with eval.
func resolveContainerStepInputs(eval ExpressionEvaluator, step *model.Step) (image string, args, entrypoint []string, err error) {
	image = strings.TrimPrefix(step.Uses, "docker://")

	args, err = shellquote.Split(eval.Interpolate(context.Background(), step.With["args"]))
	if err != nil {
		return "", nil, nil, fmt.Errorf("step %q: failed to parse args: %w", step.ID, err)
	}

	if entry := eval.Interpolate(context.Background(), step.With["entrypoint"]); entry != "" {
		entrypoint = []string{entry}
	}
	return image, args, entrypoint, nil
}

func (sd *stepDocker) prepareContainerStep() (*hooks.ContainerStepSpec, error) {
	rc := sd.RunContext
	step := sd.Step

	image, args, entrypoint, err := resolveContainerStepInputs(rc.ExprEval, step)
	if err != nil {
		return nil, err
	}

	return &hooks.ContainerStepSpec{
		StepNumber: step.Number,
		StepID:     step.ID,
		StepName:   step.Name,
		Image:      image,
		Entrypoint: entrypoint,
		Args:       args,
		WorkingDir: rc.Config.Workdir,
	}, nil
}

func (sd *stepDocker) runUsesContainer() common.Executor {
	rc := sd.RunContext
	step := sd.Step

	return func(ctx context.Context) error {
		if rc.containerHooks.Enabled {
			spec, ok := rc.containerHooks.StepSpecs[step.Number]
			if !ok {
				return fmt.Errorf("container step #%d was not described during collection", step.Number)
			}
			specWithEnv := *spec
			specWithEnv.Env = maps.Clone(sd.env)
			input := rc.buildContainerExecuteHookStepInput(&specWithEnv)
			return rc.runContainerExecuteHook(ctx, input)
		}

		if rc.isWindowsHost() {
			return errWindowsHostContainer
		}
		if rc.kubernetes {
			return errKubernetesDocker
		}
		image := strings.TrimPrefix(step.Uses, "docker://")
		cmd, err := shellquote.Split(stepInput(step, "args"))
		if err != nil {
			return err
		}

		var entrypoint []string
		if entry := stepInput(step, "entrypoint"); entry != "" {
			entrypoint = []string{entry}
		}

		stepContainer := newStepContainer(ctx, sd, image, cmd, entrypoint, "")

		return common.NewPipelineExecutor(
			stepContainer.Pull(rc.Config.ForcePull),
			stepContainer.Remove(),
			stepContainer.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
			stepContainer.Start(true),
		).Finally(stepContainer.Close())(ctx)
	}
}

var ContainerNewContainer = container.NewContainer
