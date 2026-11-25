// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	_ "embed"

	"gitea.dev/actionslib/pkg/expreval"
	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"go.yaml.in/yaml/v4"
)

// NewExpressionEvaluator creates a new evaluator
func (rc *RunContext) NewExpressionEvaluator(ctx context.Context) *ExpressionEvaluator {
	return rc.NewExpressionEvaluatorWithEnv(ctx, rc.GetEnv())
}

func (rc *RunContext) NewExpressionEvaluatorWithEnv(ctx context.Context, env map[string]string) *ExpressionEvaluator {
	return rc.newExpressionEvaluator(ctx, env, rc.getJobContext(), rc.getGithubContext(ctx), "job")
}

func (rc *RunContext) newExpressionEvaluator(ctx context.Context, env map[string]string, jobContext *model.JobContext, github *model.GithubContext, scope string) *ExpressionEvaluator {
	ee := rc.actionDefaultEnvironment(ctx, github, jobContext)
	if scope == "job" {
		ee.Jobs = new(map[string]*model.WorkflowCallResult)
	}
	ee.Env = env
	ee.Steps = rc.StepResults
	ee.Secrets = getWorkflowSecrets(rc)
	ee.Vars = rc.Config.Vars
	ee.Needs = exprparser.NeedsContext(rc.Run)
	ee.Inputs = rc.getEvaluatorInputs(github)
	return &expressionEvaluator{
		interpreter: exprparser.NewInterpeter(ee, exprparser.Config{
			Run:        rc.Run,
			WorkingDir: rc.Config.Workdir,
			Context:    scope,
		}),
	}
}

//go:embed hashfiles/index.js
var hashfiles string

// NewStepExpressionEvaluator creates a new evaluator with the `inputs` of the enclosing workflow or composite action
func (rc *RunContext) NewStepExpressionEvaluator(ctx context.Context, step step) *ExpressionEvaluator {
	return rc.newExpressionEvaluator(ctx, *step.getEnv(), rc.getJobContext(), getStepGithubContext(ctx, step), "step")
}

func (rc *RunContext) actionDefaultEnvironment(ctx context.Context, github *model.GithubContext, job *model.JobContext) *exprparser.EvaluationEnvironment {
	return &exprparser.EvaluationEnvironment{
		Github:    github,
		Strategy:  exprparser.StrategyContext(rc.Run.Job().Strategy, rc.jobIndex, rc.jobTotal),
		Matrix:    rc.Matrix,
		Job:       job,
		Runner:    rc.getRunnerContext(ctx),
		HashFiles: getHashFilesFunction(ctx, rc),
	}
}

func newActionInputsExpressionEvaluator(inputs map[string]string) *ExpressionEvaluator {
	values := make(map[string]any, len(inputs))
	for name, value := range inputs {
		values[name] = value
	}
	return &expressionEvaluator{interpreter: exprparser.NewInterpeter(&exprparser.EvaluationEnvironment{Inputs: values}, exprparser.Config{Context: "step"})}
}

func getHashFilesFunction(ctx context.Context, rc *RunContext) func(v []reflect.Value) (any, error) {
	return func(v []reflect.Value) (any, error) {
		if rc.JobContainer != nil {
			timeoutCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()
			name := "workflow/hashfiles/index.js"
			hout := &bytes.Buffer{}
			herr := &bytes.Buffer{}
			patterns := []string{}
			followSymlink := false

			for i, p := range v {
				s := exprparser.CoerceToString(p)
				if i == 0 && strings.HasPrefix(s, "--") {
					if strings.EqualFold(s, "--follow-symbolic-links") {
						followSymlink = true
						continue
					}
					return "", fmt.Errorf("invalid glob option %s, available option: '--follow-symbolic-links'", s)
				}
				patterns = append(patterns, s)
			}
			env := map[string]string{}
			maps.Copy(env, rc.Env)
			env["patterns"] = strings.Join(patterns, "\n")
			if followSymlink {
				env["followSymbolicLinks"] = "true"
			}

			stdout, stderr := rc.JobContainer.ReplaceLogWriter(hout, herr)
			defer rc.JobContainer.ReplaceLogWriter(stdout, stderr)
			err := rc.JobContainer.Copy(rc.JobContainer.GetActPath(), &container.FileEntry{
				Name: name,
				Mode: 0o644,
				Body: hashfiles,
			}).Then(rc.JobContainer.Exec([]string{"node", container.JoinPath(rc.JobContainer.GetActPath(), name)}, env, "", ""))(timeoutCtx)
			switch {
			case err == nil:
			case ctx.Err() != nil:
				return "", ctx.Err()
			case timeoutCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
				return "", fmt.Errorf("hashFiles('%s') couldn't finish within 120 seconds", expressionStringEscape(strings.Join(patterns, ", ")))
			case hashFilesNodeMissing(err):
				common.Logger(ctx).With(rawOutputField, true).Warnf("##[warning]hashFiles could not run because node is unavailable in the job environment. Returning an empty string.")
				return "", nil
			default:
				return "", fmt.Errorf("hashFiles('%s') failed. Fail to hash files under directory '%s'", expressionStringEscape(strings.Join(patterns, ", ")), rc.githubWorkspace())
			}
			for line := range strings.Lines(hout.String() + "\n" + herr.String()) {
				if hash, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "__OUTPUT__"); ok {
					if hash, ok = strings.CutSuffix(hash, "__OUTPUT__"); ok {
						return hash, nil
					}
				}
			}
		}
		return "", nil
	}
}

func hashFilesNodeMissing(err error) bool {
	exitErr, ok := errors.AsType[container.ExitCodeError](err)
	return ok && (exitErr == 126 || exitErr == 127) || err.Error() == "Cannot find: node in PATH"
}

type expressionEvaluator struct {
	interpreter exprparser.Interpreter
}

type ExpressionEvaluator = expressionEvaluator

var addMaskExpression = regexp.MustCompile(`::add-mask::.*`)

func (ee expressionEvaluator) evaluate(ctx context.Context, in string, defaultStatusCheck exprparser.DefaultStatusCheck) (any, error) {
	logger := common.Logger(ctx)
	logger.Debugf("evaluating expression '%s'", in)
	evaluated, err := ee.interpreter.Evaluate(in, defaultStatusCheck)

	// evaluated is an any: %t renders everything but a bool as "%!t(string=...)"
	logger.Debugf("expression '%s' evaluated to '%s'", in, addMaskExpression.ReplaceAllString(fmt.Sprintf("%v", evaluated), "::add-mask::***)"))

	return evaluated, err
}

// shared returns the evaluation layer of the shared library, bound to this context so the
// evaluation of every single expression is still logged and masked here.
func (ee expressionEvaluator) shared(ctx context.Context) expreval.Evaluator {
	return expreval.New(func(in string, defaultStatusCheck exprparser.DefaultStatusCheck) (any, error) {
		return ee.evaluate(ctx, in, defaultStatusCheck)
	})
}

func (ee expressionEvaluator) EvaluateYamlNode(ctx context.Context, node *yaml.Node) error {
	return ee.shared(ctx).EvaluateYamlNode(node)
}

func (ee expressionEvaluator) Interpolate(ctx context.Context, in string) (string, error) {
	return ee.shared(ctx).Interpolate(in)
}

// InterpolateName keeps the source text of a job or step name that cannot be evaluated, as GitHub does.
func (ee expressionEvaluator) InterpolateName(ctx context.Context, in string) string {
	out, err := ee.Interpolate(ctx, in)
	if err != nil {
		common.Logger(ctx).Warnf("Unable to evaluate the display name '%s': %s", in, err)
		return in
	}
	return out
}

// EvalBool evaluates an expression against given evaluator. An `if:` is an expression even without
// `${{ }}`, while literal text around one makes the whole value a string.
func EvalBool(ctx context.Context, evaluator *expressionEvaluator, expr string, defaultStatusCheck exprparser.DefaultStatusCheck) (bool, error) {
	return expreval.New(func(in string, dsc exprparser.DefaultStatusCheck) (any, error) {
		return evaluator.evaluate(ctx, in, dsc)
	}).EvalBool(expr, defaultStatusCheck)
}

func inputsFromEnv(env map[string]string) map[string]any {
	inputs := map[string]any{}
	for k, v := range env {
		if after, ok := strings.CutPrefix(k, "INPUT_"); ok {
			inputs[strings.ToLower(after)] = v
		}
	}
	return inputs
}

func (rc *RunContext) getEvaluatorInputs(ghc *model.GithubContext) map[string]any {
	if rc.actionInputs != nil {
		return maps.Clone(rc.actionInputs)
	}
	if rc.caller != nil {
		return maps.Clone(rc.caller.inputs)
	}
	// only a called workflow's job itself takes the inputs Gitea resolved,
	// composite actions copy its Config but have their own `inputs`
	if rc.Config.WorkflowCallInputs != nil && rc.Parent == nil {
		return maps.Clone(rc.Config.WorkflowCallInputs)
	}

	inputs := map[string]any{}

	switch ghc.EventName {
	case "workflow_dispatch":
		if config := rc.Run.Workflow.WorkflowDispatchConfig(); config != nil {
			for name, input := range config.Inputs {
				if input.Type == "number" {
					input.Type = "string" // boolean is the only typed dispatch input, as in Gitea
				}
				setEvaluatorInput(inputs, ghc, name, model.WorkflowCallInput{Type: input.Type, Default: input.Default})
			}
		}
	case "workflow_call":
		for name, input := range rc.Run.Workflow.WorkflowCallConfig().Inputs {
			setEvaluatorInput(inputs, ghc, name, input)
		}
	}
	return inputs
}

func setEvaluatorInput(inputs map[string]any, ghc *model.GithubContext, name string, input model.WorkflowCallInput) {
	value := model.NestedMapLookup(ghc.Event, "inputs", name)
	if value == nil {
		value, _ = input.DefaultValue()
	}
	inputs[name] = model.CoerceInputValue(value, input.Type)
}

func (c *caller) resolveInputs(ctx context.Context, workflow *model.Workflow) error {
	rc := c.runContext
	callerEval := rc.ExprEval.shared(ctx)
	var callerWith map[string]any
	if err := model.DecodeEvaluated("workflow inputs", rc.Run.Job().RawWith, callerEval.EvaluateYamlNode, &callerWith); err != nil {
		return err
	}
	c.workflowCallConfig = workflow.WorkflowCallConfig()
	var err error
	if c.inputs, err = callerEval.ResolveWorkflowCallInputs(c.workflowCallConfig, callerWith); err != nil {
		common.Logger(ctx).Warn(err)
	}
	return nil
}

func (rc *RunContext) resolveWorkflowCall(ctx context.Context) error {
	if rc.caller == nil {
		return nil
	}
	logger := common.Logger(ctx)
	caller := rc.caller.runContext
	callerJob := caller.Run.Job()

	if callerJob.InheritSecrets() {
		rc.workflowCallSecrets = getWorkflowSecrets(caller)
		return nil
	}
	secrets := callerJob.Secrets()
	if err := rc.caller.workflowCallConfig.ValidateSecrets(secrets); err != nil {
		logger.Warn(err)
	}
	var err error
	if rc.workflowCallSecrets, err = caller.ExprEval.shared(ctx).EvaluateCallerSecrets(secrets); err != nil {
		return fmt.Errorf("unable to interpolate %w", err)
	}
	for _, name := range []string{"GITHUB_TOKEN", "GITEA_TOKEN"} {
		token, ok := rc.Config.Secrets[name]
		if _, mapped := common.LookupFold(rc.workflowCallSecrets, name, true); ok && !mapped {
			rc.workflowCallSecrets[name] = token
		}
	}
	return nil
}

func getWorkflowSecrets(rc *RunContext) map[string]string {
	if rc.caller != nil {
		return rc.workflowCallSecrets
	}
	if rc.runtimeToken == "" {
		return rc.Config.Secrets
	}
	secrets := map[string]string{"ACTIONS_RUNTIME_TOKEN": rc.runtimeToken} // as the daemon exposes its token
	maps.Copy(secrets, rc.Config.Secrets)
	return secrets
}
