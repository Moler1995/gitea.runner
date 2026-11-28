// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"gitea.com/gitea/runner/act/common"

	"gitea.dev/actionslib/pkg/model"
)

func (rc *RunContext) setCompositeActionEnv(stepEnv, inputs map[string]string) {
	isInput := func(key, _value string) bool { return strings.HasPrefix(key, "INPUT_") }
	env := maps.Clone(stepEnv)
	maps.DeleteFunc(env, isInput)
	for name, value := range inputs {
		env[inputEnvKey(name)] = value
	}
	rc.setActionEnv(env)
	maps.DeleteFunc(rc.Env, isInput)
}

func newCompositeRunContext(ctx context.Context, parent *RunContext, step actionStep, actionPath string, inputs map[string]string) *RunContext {
	// run with the global config but without secrets
	configCopy := *parent.Config
	configCopy.Secrets = nil

	// create a run context for the composite action to run in
	compositerc := &RunContext{
		Name:    parent.Name,
		JobName: parent.JobName,
		Matrix:  parent.Matrix,
		Run: &model.Run{
			JobID: parent.Run.JobID,
			Workflow: &model.Workflow{
				Name: parent.Run.Workflow.Name,
				Jobs: map[string]*model.Job{
					parent.Run.JobID: {Strategy: parent.Run.Job().Strategy},
				},
			},
		},
		Config:        &configCopy,
		StepResults:   map[string]*model.StepResult{},
		JobContainer:  parent.JobContainer,
		ActionPath:    actionPath,
		GlobalEnv:     parent.GlobalEnv,
		Masks:         slices.Clip(parent.Masks),
		ExtraPath:     parent.ExtraPath,
		Parent:        parent,
		EventJSON:     parent.EventJSON,
		platformImage: parent.platformImage,
		kubernetes:    parent.kubernetes,
		jobIndex:      parent.jobIndex,
		jobTotal:      parent.jobTotal,
		runtimeToken:  parent.runtimeToken,
	}
	compositerc.setCompositeActionEnv(*step.getEnv(), inputs)
	compositerc.ExprEval = compositerc.NewExpressionEvaluator(ctx)

	return compositerc
}

// appendUniqueMasks appends the masks from src to dst, skipping any mask that
// is already present in dst. This prevents the parent RunContext's Masks slice
// from growing exponentially when composite actions are nested or repeated,
// since each composite RunContext is seeded with its parent's masks.
func appendUniqueMasks(dst, src []string) []string {
	for _, m := range src {
		if !slices.Contains(dst, m) {
			dst = append(dst, m)
		}
	}
	return dst
}

func execAsComposite(ctx context.Context, step actionStep, inputs map[string]string) error {
	compositeRC := step.getCompositeRunContext(ctx, inputs)
	ctx = WithCompositeLogger(ctx, &compositeRC.Masks)
	rc := step.getRunContext()
	err := step.getCompositeSteps().main(ctx)

	// Map outputs from composite RunContext to job RunContext
	eval := compositeRC.NewExpressionEvaluator(ctx)
	for outputName, output := range step.getActionModel().Outputs {
		value, outputErr := eval.Interpolate(ctx, output.Value)
		if outputErr != nil {
			err = errors.Join(err, fmt.Errorf("unable to interpolate output %s: %w", outputName, outputErr))
			continue
		}
		rc.setOutput(ctx, map[string]string{"name": outputName}, value)
	}

	rc.ExtraPath = compositeRC.ExtraPath
	// Propagate GlobalEnv only, so composite inputs and step-local values do not escape.
	mergeIntoMap := mergeIntoMapCaseSensitive
	if rc.JobContainer.IsEnvironmentCaseInsensitive() {
		mergeIntoMap = mergeIntoMapCaseInsensitive
	}
	if rc.GlobalEnv == nil {
		rc.GlobalEnv = map[string]string{}
	}
	mergeIntoMap(rc.GlobalEnv, compositeRC.GlobalEnv)
	mergeIntoMap(rc.Env, compositeRC.GlobalEnv)

	return err
}

type compositeSteps struct {
	pre  common.Executor
	main common.Executor
	post common.Executor
}

// Executor returns a pipeline executor for all the steps in the job
func (rc *RunContext) compositeExecutor(action *model.Action) *compositeSteps {
	steps := make([]common.Executor, 0)
	preSteps := make([]common.Executor, 0)
	var postExecutor common.Executor

	sf := &stepFactoryImpl{}

	for i, step := range action.Runs.Steps {
		if step.ID == "" {
			step.ID = strconv.Itoa(i)
		}
		step.Number = i

		// create a copy of the step, since this composite action could
		// run multiple times and we might modify the instance
		stepcopy := step

		step, err := sf.newStep(&stepcopy, rc)
		if err != nil {
			return &compositeSteps{
				main: common.NewErrorExecutor(err),
			}
		}

		stepID := step.getStepModel().ID
		preSteps = append(preSteps, newCompositeStepLogExecutor(rc.newCompositeCommandExecutor(step.pre()), stepID))

		steps = append(steps, newCompositeStepLogExecutor(rc.newCompositeCommandExecutor(step.main()), stepID))

		// run the post executor in reverse order
		stepPost := newCompositeStepLogExecutor(rc.newCompositeCommandExecutor(step.post()), stepID)
		if postExecutor != nil {
			stepPost = stepPost.Finally(postExecutor)
		}
		postExecutor = stepPost
	}

	steps = append(steps, common.JobError)
	preSteps = append(preSteps, common.JobError)
	return &compositeSteps{
		pre: func(ctx context.Context) error {
			return common.NewPipelineExecutor(preSteps...)(common.WithJobErrorContainer(ctx))
		},
		main: func(ctx context.Context) error {
			return common.NewPipelineExecutor(steps...)(common.WithJobErrorContainer(ctx))
		},
		post: postExecutor,
	}
}

func (rc *RunContext) newCompositeCommandExecutor(executor common.Executor) common.Executor {
	return func(ctx context.Context) error {
		if parent := rc.Parent; parent != nil { // a composite context outlives the stage it was made in, so sync masks around every step
			rc.Masks = appendUniqueMasks(rc.Masks, parent.Masks)
			defer func() { parent.Masks = appendUniqueMasks(parent.Masks, rc.Masks) }()
		}
		return rc.withCommandLogWriter(WithCompositeLogger(ctx, &rc.Masks), executor)
	}
}

func newCompositeStepLogExecutor(runStep common.Executor, stepID string) common.Executor {
	return func(ctx context.Context) error {
		ctx = WithCompositeStepLogger(ctx, stepID)
		logger := common.Logger(ctx)
		err := runStep(ctx)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			logger.Errorf("##[error]%s", EscapeCommandData(err.Error()))
			common.SetJobError(ctx, err)
		}
		return nil
	}
}
