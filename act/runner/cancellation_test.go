// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"

	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.yaml.in/yaml/v4"
)

func TestStepsExecutorRunsMainStepsUnlessSetupFailed(t *testing.T) {
	for name, tt := range map[string]struct {
		stage, status, result string
		cause                 error
		ran                   []string
	}{
		"setup failure":        {"pre", "failure", "failure", setupError{assert.AnError}, []string{"pre"}},
		"pre step failure":     {"pre", "failure", "failure", assert.AnError, []string{"pre", "main", "always"}},
		"cancel during pre":    {"pre", "cancelled", "cancelled", context.Canceled, []string{"pre", "main", "always"}},
		"cancel during main":   {"main", "cancelled", "cancelled", context.Canceled, []string{"pre", "main", "always"}},
		"deadline during main": {"main", "cancelled", "failure", context.DeadlineExceeded, []string{"pre", "main", "always"}},
	} {
		t.Run(name, func(t *testing.T) {
			rc := makeTestRC(t, "{}")
			ctx, interrupt := context.WithCancel(common.WithJobErrorContainer(t.Context()))
			defer interrupt()
			if tt.cause == context.DeadlineExceeded {
				deadline := newControllableDeadlineContext(ctx)
				ctx, interrupt = deadline, deadline.expire
			}
			var ran []string
			phase := func(stage string) common.Executor {
				return func(stepCtx context.Context) error {
					ran = append(ran, stage)
					require.NoError(t, stepCtx.Err())
					switch {
					case stage != tt.stage:
					case tt.cause == context.Canceled || tt.cause == context.DeadlineExceeded:
						interrupt()
					default:
						reportStepError(stepCtx, rc, tt.cause)
						return tt.cause
					}
					return nil
				}
			}

			require.ErrorIs(t, newStepsExecutor(rc, []common.Executor{phase("pre")}, []common.Executor{phase("main"), phase("always")})(ctx), tt.cause)
			assert.Equal(t, tt.ran, ran)
			assert.Equal(t, tt.cause == context.Canceled, rc.jobCancelled)
			jobContext := rc.getJobContext()
			assert.Equal(t, tt.status, jobContext.Status)
			cancelled := tt.status == "cancelled"
			evaluator := exprparser.NewInterpeter(&exprparser.EvaluationEnvironment{Job: jobContext}, exprparser.Config{Context: "step"})
			for condition, want := range map[string]bool{"cancelled()": cancelled, "always()": true, "success()": false, "failure()": !cancelled, "!cancelled()": !cancelled, "": false} {
				got, err := evaluator.Evaluate(condition, exprparser.DefaultStatusCheckSuccess)
				require.NoError(t, err, condition)
				assert.Equal(t, want, got, condition)
			}
			rc.setJobResult(t.Context(), true)
			assert.Equal(t, tt.result, rc.Run.Job().Result)
		})
	}
}

// TestPreStepFailureAffectsMainStepIfStatus verifies the status path used by real
// main-step `if` evaluation. A pre-step failure is not present in StepResults, so
// recording only the context job error is not enough: getJobContext must also report
// failure so success()-default main steps skip and failure() steps run.
func TestPreStepFailureAffectsMainStepIfStatus(t *testing.T) {
	rc := createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, ""),
	})
	ctx := common.WithJobErrorContainer(t.Context())

	reportStepError(ctx, rc, assert.AnError)

	assert.Equal(t, "failure", rc.getJobContext().Status)
	require.ErrorIs(t, common.JobError(ctx), assert.AnError)

	defaultStep := &stepRun{
		RunContext: rc,
		Step:       &model.Step{ID: "default-step"},
		env:        map[string]string{},
	}
	defaultEnabled, err := isStepEnabled(ctx, defaultStep.getIfExpression(ctx, stepStageMain), defaultStep, stepStageMain)
	require.NoError(t, err)
	assert.False(t, defaultEnabled, "default success() main step must skip after a pre-step failure")

	failureStep := &stepRun{
		RunContext: rc,
		Step: &model.Step{
			ID: "failure-step",
			If: yaml.Node{Value: "failure()"},
		},
		env: map[string]string{},
	}
	failureEnabled, err := isStepEnabled(ctx, failureStep.getIfExpression(ctx, stepStageMain), failureStep, stepStageMain)
	require.NoError(t, err)
	assert.True(t, failureEnabled, "failure() main step must run after a pre-step failure")
}

// TestPostStepsContextCancelledIsUsableForFailingStep guards against a panic: post/cleanup
// steps run on a context derived from the cancelled job context, and a failing post step
// records its error via common.SetJobError. If that derived context lacks a job-error container,
// SetJobError dereferences a nil map and panics. The post context must therefore be detached
// from cancellation (so the steps run) yet still carry a usable error container.
func TestPostStepsContextCancelledIsUsableForFailingStep(t *testing.T) {
	jobSpan := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}})
	cancelled, cancel := context.WithCancel(trace.ContextWithSpanContext(common.WithJobErrorContainer(t.Context()), jobSpan))
	cancel()
	require.ErrorIs(t, cancelled.Err(), context.Canceled)

	postCtx, done := postStepsContext(cancelled)
	defer done()

	// Detached from cancellation, so the post steps actually run.
	require.NoError(t, postCtx.Err(), "post context must not be cancelled")
	assert.Equal(t, jobSpan, trace.SpanContextFromContext(postCtx), "post steps must stay in the job's trace")

	// A failing post step records its error instead of panicking.
	require.NotPanics(t, func() {
		common.SetJobError(postCtx, assert.AnError)
	}, "a failing post step must not panic on the cancel path")
	assert.ErrorIs(t, common.JobError(postCtx), assert.AnError)
}

// TestPostStepsContextDeadlinePreservesJobError verifies the job-timeout path keeps the original
// job-error container (via context.WithoutCancel), so the timeout failure and any post-step error
// survive into the post phase and the job is still reported as failed.
func TestPostStepsContextDeadlinePreservesJobError(t *testing.T) {
	base := common.WithJobErrorContainer(t.Context())
	common.SetJobError(base, assert.AnError)
	expired, cancel := context.WithDeadline(base, time.Now().Add(-time.Hour))
	defer cancel()
	require.ErrorIs(t, expired.Err(), context.DeadlineExceeded)

	postCtx, done := postStepsContext(expired)
	defer done()

	require.NoError(t, postCtx.Err(), "post context must not carry the expired deadline")
	assert.ErrorIs(t, common.JobError(postCtx), assert.AnError, "the timeout job error must be preserved")
}

// reportStepError must treat a context.Canceled (e.g. a teardown-cancelled read) as an
// interruption, never a job failure.
func TestReportStepErrorTreatsCancelAsInterruption(t *testing.T) {
	rc := &RunContext{}

	// stray read cancellation while the job context is live: ignored, not a failure
	live := common.WithJobErrorContainer(t.Context())
	reportStepError(live, rc, context.Canceled)
	require.NoError(t, common.JobError(live))
	assert.False(t, rc.jobFailed)
	assert.False(t, rc.jobCancelled)

	// genuine job cancellation: recorded as cancelled, still not a failure
	cancelled, cancel := context.WithCancel(common.WithJobErrorContainer(t.Context()))
	cancel()
	reportStepError(cancelled, rc, context.Canceled)
	require.NoError(t, common.JobError(cancelled))
	assert.False(t, rc.jobFailed)
	assert.True(t, rc.jobCancelled)

	// a real error still fails the job
	for _, stepErr := range []error{assert.AnError, context.DeadlineExceeded} {
		rc := &RunContext{}
		failed := common.WithJobErrorContainer(t.Context())
		reportStepError(failed, rc, stepErr)
		require.ErrorIs(t, common.JobError(failed), stepErr)
		assert.Equal(t, "failure", rc.getJobContext().Status)
	}
}
