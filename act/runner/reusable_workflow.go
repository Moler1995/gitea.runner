// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"

	"gitea.dev/actionslib/pkg/expreval"
	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
)

var absoluteReusableWorkflowURLRegex = regexp.MustCompile(`^(https?://.*)/([^/]+/[^/]+/\.[^/]+/workflows/[^@]+@.*)$`)

func (c *caller) loadReusableWorkflow(ctx context.Context, instance, uses string) (*model.Plan, error) {
	rc := c.runContext
	if matches := absoluteReusableWorkflowURLRegex.FindStringSubmatch(uses); matches != nil {
		instance, uses = matches[1], matches[2]
	}
	workflow, err := model.ParseReusableWorkflowUses(uses)
	if err != nil {
		return nil, err
	}
	depth := 1
	for parent := rc.caller; parent != nil; parent = parent.runContext.caller {
		depth++
	}
	if depth >= model.MaxWorkflowCallDepth {
		return nil, fmt.Errorf("reusable workflow %q exceeds the limit of %d workflows including the root", workflow.Path, model.MaxWorkflowCallDepth)
	}

	if rc.caller != nil {
		c.checkout = rc.caller.checkout
	}
	cacheDir := "workflows"
	if preset := rc.Config.PresetGitHubContext; workflow.IsLocal() && c.checkout.Dir == "" && preset != nil {
		cacheDir = "local-workflows" // a same-repository checkout must never answer another repository's call unauthenticated
		workflow, err = model.ParseReusableWorkflowUses(fmt.Sprintf("%s/%s@%s", preset.Repository, workflow.Path, preset.Sha))
		if err != nil {
			return nil, err
		}
	}
	if !workflow.IsLocal() {
		cloneURL, err := rc.ExprEval.Interpolate(ctx, withHTTPS(instance)+"/"+workflow.Owner+"/"+workflow.Repo)
		if err != nil {
			return nil, fmt.Errorf("unable to interpolate the workflow clone URL: %w", err)
		}
		c.checkout = rc.Config.withInstanceAuth(git.NewGitCloneExecutorInput{
			URL: cloneURL,
			Ref: workflow.Ref,
			Dir: filepath.Join(rc.ActionCacheDir(), cacheDir, model.UsesHash(cloneURL+"@"+workflow.Ref), "git"),
		}, rc.Config.GetToken())
	}

	if c.checkout.Dir != "" {
		unlock, err := git.AcquireCloneLock(ctx, filepath.Dir(c.checkout.Dir))
		if err != nil {
			return nil, err
		}
		defer unlock()
		if err := git.NewGitCloneExecutor(c.checkout)(ctx); err != nil {
			return nil, err
		}
		if !workflow.IsLocal() {
			c.checkout.Ref, err = git.FindGitRevision(ctx, c.checkout.Dir)
			if err != nil {
				return nil, err
			}
			c.checkout.OfflineMode = true
		}
	}
	file, err := os.Open(filepath.Join(cmp.Or(c.checkout.Dir, rc.Config.Workdir), workflow.Path))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	planner, err := model.NewSingleWorkflowPlanner(filepath.Base(workflow.Path), file)
	if err != nil {
		return nil, err
	}
	return planner.PlanEvent("workflow_call")
}

func (rc *RunContext) runReusableWorkflow(ctx context.Context) error {
	call := &caller{runContext: rc}
	plan, err := call.loadReusableWorkflow(ctx, rc.Config.GitHubInstance, rc.Run.Job().Uses)
	if err == nil && len(plan.Stages) > 0 {
		if err = call.resolveInputs(ctx, plan.Stages[0].Runs[0].Workflow); err == nil {
			err = (&runnerImpl{config: rc.Config, eventJSON: rc.EventJSON, caller: call}).NewPlanExecutor(plan)(ctx)
		}
	}
	call.finishWorkflowCall(ctx, err)
	return cmp.Or(ctx.Err(), err)
}

func (c *caller) finishWorkflowCall(ctx context.Context, planErr error) {
	rc := c.runContext
	config := c.workflowCallConfig
	var jobs map[string]*model.WorkflowCallResult
	if config != nil && len(config.Outputs) > 0 && (planErr == nil || len(c.reportedRuns) > 0) {
		jobs = make(map[string]*model.WorkflowCallResult, len(c.reportedRuns))
	}
	failed := planErr != nil
	for _, run := range c.reportedRuns {
		if run == nil {
			failed = true
			continue
		}
		job := run.Job()
		switch job.NeedsResult() {
		case "failure":
			failed = true
		case "cancelled":
			rc.markCancelled()
		}
		if jobs != nil {
			jobs[run.JobID] = &model.WorkflowCallResult{Outputs: job.Outputs}
		}
	}
	if jobs != nil {
		interpreter := exprparser.NewInterpeter(&exprparser.EvaluationEnvironment{
			Github: rc.getGithubContext(ctx),
			Vars:   rc.Config.Vars,
			Jobs:   &jobs,
			Inputs: c.inputs,
		}, exprparser.Config{})
		outputs, err := expreval.New(interpreter.Evaluate).EvaluateWorkflowCallOutputs(config) // unlogged, the caller's log lacks the called jobs' masks
		if err != nil {
			common.Logger(ctx).Errorf("##[error]unable to interpolate workflow %s", EscapeCommandData(err.Error()))
			failed = true
		} else if len(rc.Matrix) == 0 || (!failed && !rc.jobCancelled) {
			mergeJobOutputs(rc.Run.Job(), outputs)
		}
	}
	rc.markJobInterrupted(ctx)
	rc.setJobResult(ctx, !failed)
}

// For Gitea
// withInstanceAuth passes instance credentials only to clones from this Gitea instance, and runner.insecure only to its registered address.
func (c Config) withInstanceAuth(input git.NewGitCloneExecutorInput, token string) git.NewGitCloneExecutorInput {
	input.Depth, input.OfflineMode = c.ActionCloneDepth, c.ActionOfflineMode
	u, err := url.Parse(input.URL)
	if err != nil || !onInstanceHost(u.Host, c.GitHubInstance, c.trustedActionInstance()) {
		return input
	}
	input.ClientCertFile, input.ClientKeyFile = c.ClientCertFile, c.ClientKeyFile
	if onInstanceHost(u.Host, c.GitHubInstance) {
		input.InsecureSkipTLS, input.CAFile, input.Transport = c.InsecureSkipTLS, c.CAFile, c.InstanceTransport
	}
	if u.User == nil {
		input.Token = token
	}
	return input
}

// For Gitea
// trustedActionInstance returns the self-hosted DEFAULT_ACTIONS_URL host that may carry the
// task token, or "" when actions resolve to github.com / a GithubMirror (never trusted).
func (c Config) trustedActionInstance() string {
	if c.DefaultActionInstanceIsSelfHosted {
		return c.DefaultActionInstance
	}
	return ""
}

// For Gitea
// onInstanceHost reports whether host is that of one of the instance URLs, which may lack a scheme.
func onInstanceHost(host string, instanceURLs ...string) bool {
	for _, candidate := range instanceURLs {
		if candidate == "" {
			continue
		}
		if u, err := url.Parse(withHTTPS(candidate)); err == nil && u.Host == host {
			return true
		}
	}
	return false
}
