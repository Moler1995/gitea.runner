// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"gitea.dev/actionslib/pkg/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v4"
)

func TestJobExecutor(t *testing.T) {
	t.Parallel()
	tables := []TestJobFileInfo{
		{workdir, "uses-and-run-in-one-step", "push", "Invalid run/uses syntax for job:test step:Test", platforms, secrets},
		{workdir, "uses-github-empty", "push", "Expected format {org}/{repo}[/path]@ref", platforms, secrets},
		{workdir, "uses-github-noref", "push", "Expected format {org}/{repo}[/path]@ref", platforms, secrets},
		{workdir, "uses-github-root", "push", "", platforms, secrets},
		{workdir, "uses-docker-url", "push", "", platforms, secrets},
		{workdir, "job-nil-step", "push", "invalid Step 0: missing run or uses key", platforms, secrets},
	}
	ctx := common.WithDryrun(t.Context(), true)
	for _, table := range tables {
		t.Run(table.workflowPath, func(t *testing.T) {
			t.Parallel()
			table.runTest(ctx, t, &Config{})
		})
	}
}

type jobInfoMock struct {
	mock.Mock
}

func (jim *jobInfoMock) steps() []*model.Step {
	args := jim.Called()

	return args.Get(0).([]*model.Step)
}

func (jim *jobInfoMock) startContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) stopContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) closeContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) interpolateOutputs() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

type jobContainerMock struct {
	container.Container
	linuxEnvironment
}

func (jcm *jobContainerMock) ReplaceLogWriter(_, _ io.Writer) (io.Writer, io.Writer) {
	return nil, nil
}

type stepFactoryMock struct {
	mock.Mock
}

func (sfm *stepFactoryMock) newStep(model *model.Step, rc *RunContext) (step, error) {
	args := sfm.Called(model, rc)
	return args.Get(0).(step), args.Error(1)
}

type actionPreparerMock struct {
	reference string
	sha       string
	prepared  int
}

func (apm *actionPreparerMock) prepareActionExecutor() common.Executor {
	return func(context.Context) error {
		apm.prepared++
		return nil
	}
}

func (apm *actionPreparerMock) actionDownloadInfo() (string, string) {
	return apm.reference, apm.sha
}

func TestPrintPrepareActionsFetchesConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		buf := &bytes.Buffer{}
		ctx := common.WithLogger(t.Context(), logging.New(logging.NewLevelHandler(newJobLogHandler(buf, cyan), logging.LevelInfo)).With("job", "j1"))
		rc := newPrefetchRunContext(t)
		started, release := make(chan string), make(chan struct{})
		var fetched atomic.Int32
		setFetch(t, func(_ctx context.Context, input git.NewGitCloneExecutorInput) (string, string, error) {
			fetched.Add(1)
			started <- input.Ref
			<-release
			return filepath.Join(input.Dir, "commit"), "sha-" + input.Ref, nil
		})
		preparers := append(newRemotePreparers(rc, "org/repo@v1", "org/repo/sub@v1", "org/repo@v2", "org/repo@v3", "org/repo@v4", "org/repo@v5"), &actionPreparerMock{})
		finished := make(chan error)
		go func() { finished <- printPrepareActions(rc, preparers)(ctx) }()
		for range 4 {
			<-started
		}
		synctest.Wait()
		assert.EqualValues(t, 4, fetched.Load())
		close(release)
		<-started
		require.NoError(t, <-finished)
		assert.EqualValues(t, 5, fetched.Load())
		lines := []string{"[j1]   | Prepare all required actions"}
		for _, ref := range []string{"v1", "v2", "v3", "v4", "v5"} {
			lines = append(lines, fmt.Sprintf("[j1]   | Download action repository 'org/repo@%s' (SHA:sha-%s)", ref, ref))
		}
		assert.Equal(t, strings.Join(append(lines, ""), "\n"), buf.String())
	})
}

func TestPrintPrepareActionsFailsJobAndCancelsLaterDownloads(t *testing.T) {
	for name, panicFetch := range map[string]bool{"error": false, "panic": true} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rc := newPrefetchRunContext(t)
				setFetch(t, func(ctx context.Context, input git.NewGitCloneExecutorInput) (string, string, error) {
					if input.Ref == "v1" {
						if panicFetch {
							panic("broken archive")
						}
						return "", "", assert.AnError
					}
					<-ctx.Done()
					return "", "", ctx.Err()
				})
				ctx := common.WithJobErrorContainer(common.WithLogger(t.Context(), logging.New(logging.DiscardHandler)))
				remaining := &actionPreparerMock{}
				err := printPrepareActions(rc, append(newRemotePreparers(rc, "org/repo@v1", "org/repo@v2"), remaining))(ctx)
				if panicFetch {
					require.ErrorContains(t, err, "panic: broken archive")
				} else {
					require.ErrorIs(t, err, assert.AnError)
				}
				require.ErrorIs(t, err, common.JobError(ctx))
				assert.True(t, rc.jobFailed)
				assert.Zero(t, remaining.prepared)
			})
		})
	}
}

func newPrefetchRunContext(t *testing.T) *RunContext {
	return &RunContext{
		Config: &Config{ActionCacheDir: t.TempDir(), PresetGitHubContext: &model.GithubContext{}},
		Run:    &model.Run{JobID: "1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}}},
	}
}

func newRemotePreparers(rc *RunContext, uses ...string) []actionPreparer {
	preparers := make([]actionPreparer, 0, len(uses))
	for _, use := range uses {
		preparers = append(preparers, &stepActionRemote{Step: &model.Step{Uses: use}, RunContext: rc, readAction: func(_ctx context.Context, _step *model.Step, _actionDir, _actionPath string, _readFile actionYamlReader, _writeFile fileWriter) (*model.Action, error) {
			return &model.Action{}, nil
		}})
	}
	return preparers
}

func TestPrintPrepareActionsSkipsWithoutActions(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := logging.New(logging.NewLevelHandler(newJobLogHandler(buf, cyan), logging.LevelInfo))
	ctx := common.WithLogger(t.Context(), logger.With("job", "j1"))

	require.NoError(t, printPrepareActions(&RunContext{}, nil)(ctx))

	assert.Empty(t, buf.String())
}

func TestPrintCompleteJobName(t *testing.T) {
	for name, tt := range map[string]struct {
		rc   *RunContext
		want string
	}{
		"job name":            {rc: &RunContext{Run: &model.Run{JobID: "id"}, JobName: "lint", Name: "lint-1"}, want: "lint"},
		"falls back to name":  {rc: &RunContext{Run: &model.Run{JobID: "id"}, Name: "lint-1"}, want: "lint-1"},
		"falls back to jobID": {rc: &RunContext{Run: &model.Run{JobID: "lint"}}, want: "lint"},
	} {
		t.Run(name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			logger := logging.New(logging.NewLevelHandler(newJobLogHandler(buf, cyan), logging.LevelInfo))
			ctx := common.WithLogger(t.Context(), logger.With("job", "j1"))

			require.NoError(t, printCompleteJobName(tt.rc)(ctx))

			assert.Equal(t, "[j1]   | Complete job name: "+tt.want+"\n", buf.String())
		})
	}
}

type actionStepMock struct {
	*stepMock
	*actionPreparerMock
}

func TestNewJobExecutorDownloadsAllActionsBeforeTheFirstStep(t *testing.T) {
	ctx := common.WithJobErrorContainer(t.Context())
	jim := &jobInfoMock{}
	sfm := &stepFactoryMock{}
	rc := makeTestRC(t, "{}")
	rc.JobContainer = &jobContainerMock{}

	steps := []*model.Step{{ID: "1"}, {ID: "2"}}
	executorOrder := make([]string, 0)

	jim.On("steps").Return(steps)
	jim.On("startContainer").Return(func(context.Context) error { return nil })
	jim.On("stopContainer").Return(func(context.Context) error { return nil })
	jim.On("closeContainer").Return(func(context.Context) error { return nil })
	jim.On("interpolateOutputs").Return(func(context.Context) error { return nil })

	for _, stepModel := range steps {
		sm := &stepMock{}
		apm := &actionPreparerMock{reference: "actions/checkout@v" + stepModel.ID, sha: "sha" + stepModel.ID}
		sfm.On("newStep", stepModel, rc).Return(&actionStepMock{stepMock: sm, actionPreparerMock: apm}, nil)

		sm.On("pre").Return(func(ctx context.Context) error {
			executorOrder = append(executorOrder, "pre"+stepModel.ID)
			common.Logger(ctx).Infof("Run Pre %s", stepModel.ID)
			return nil
		})
		sm.On("main").Return(func(context.Context) error {
			executorOrder = append(executorOrder, "step"+stepModel.ID)
			return nil
		})
		sm.On("post").Return(func(context.Context) error { return nil })

		defer sm.AssertExpectations(t)
	}

	logger, hook := logtest.NewLogger()
	err := newJobExecutor(jim, sfm, rc)(common.WithLogger(ctx, logger.With("job", "test")))
	require.NoError(t, err)

	assert.Equal(t, "success", rc.Run.Job().Result)
	assert.Equal(t, []string{"pre1", "pre2", "step1", "step2"}, executorOrder)

	setup := make([]string, 0)
	for _, entry := range hook.AllRecords() {
		if strings.HasPrefix(entry.Message, "Prepare all required actions") || strings.HasPrefix(entry.Message, "Download action") ||
			strings.HasPrefix(entry.Message, "Complete job name") || strings.HasPrefix(entry.Message, "Run Pre") {
			setup = append(setup, entry.Message)
		}
		if _, ok := entry.Attr("jobResult"); ok || strings.HasPrefix(entry.Message, "Cleaning up container") {
			stage, _ := entry.Attr("stage")
			assert.Equal(t, StageComplete, stage, entry.Message)
		}
	}
	assert.Equal(t, []string{
		"Prepare all required actions",
		"Download action repository 'actions/checkout@v1' (SHA:sha1)",
		"Download action repository 'actions/checkout@v2' (SHA:sha2)",
		"Complete job name: job1",
		"Run Pre 1",
		"Run Pre 2",
	}, setup)
}

func TestNewJobExecutor(t *testing.T) {
	completed := []string{"startContainer", "step1", "interpolateOutputs", "stopContainer", "closeContainer"}
	completedWithPost := []string{"startContainer", "step1", "post1", "interpolateOutputs", "stopContainer", "closeContainer"}
	table := []struct {
		name          string
		steps         int
		preSteps      []int
		postSteps     []int
		executedSteps []string
		result        string
		stepFails     bool
		output        string
		startError    error
		cancelOnStart bool
		cancelOnClose bool
		timeout       bool
	}{
		{
			name:          "no steps leaves the result unset",
			executedSteps: []string{},
		},
		{
			name:          "step failure still cleans up",
			steps:         1,
			executedSteps: completed,
			result:        "failure",
			stepFails:     true,
		},
		{
			name:          "post steps run in reverse order",
			steps:         3,
			preSteps:      []int{1, 3},
			postSteps:     []int{2, 3},
			executedSteps: []string{"startContainer", "pre1", "pre3", "step1", "step2", "step3", "post3", "post2", "interpolateOutputs", "stopContainer", "closeContainer"},
			result:        "success",
		},
		{
			name:          "invalid job output fails the job",
			steps:         1,
			executedSteps: completed,
			result:        "failure",
			output:        "${{ 'test' != test }}",
		},
		{
			name:          "start failure still closes the container",
			steps:         1,
			executedSteps: []string{"startContainer", "closeContainer"},
			startError:    errors.New("start failed"),
			result:        "failure",
		},
		{
			name:          "cancelled while closing the container",
			steps:         1,
			executedSteps: completed,
			cancelOnClose: true,
			result:        "cancelled",
		},
		{
			name:          "cancelled at startup boundary",
			steps:         1,
			postSteps:     []int{1},
			executedSteps: completedWithPost,
			result:        "cancelled",
			cancelOnStart: true,
		},
		{
			name:          "post steps run after the parent deadline",
			steps:         1,
			postSteps:     []int{1},
			executedSteps: completedWithPost,
			result:        "failure",
			timeout:       true,
		},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			ctx := common.WithJobErrorContainer(t.Context())
			cancel := func() {}
			var expire func()
			if tt.timeout {
				deadline := newControllableDeadlineContext(ctx)
				ctx, expire = deadline, deadline.expire
			} else {
				ctx, cancel = context.WithCancel(ctx)
			}
			defer cancel()
			jim := &jobInfoMock{}
			sfm := &stepFactoryMock{}
			rc := makeTestRC(t, "{}")
			rc.JobContainer = &jobContainerMock{}
			if tt.output != "" {
				require.NoError(t, rc.Run.Job().RawOutputs.Encode(map[string]string{"bad": tt.output}))
			}
			executorOrder := make([]string, 0)

			steps := make([]*model.Step, tt.steps)
			for i := range steps {
				steps[i] = &model.Step{ID: strconv.Itoa(i + 1)}
			}
			jim.On("steps").Return(steps)

			if tt.steps > 0 {
				jim.On("startContainer").Return(func(_ context.Context) error {
					executorOrder = append(executorOrder, "startContainer")
					if tt.cancelOnStart {
						cancel()
					}
					return tt.startError
				})
			}

			for i, stepModel := range steps {
				sm := &stepMock{}

				sfm.On("newStep", stepModel, rc).Return(sm, nil)

				sm.On("pre").Return(func(_ctx context.Context) error {
					if slices.Contains(tt.preSteps, i+1) {
						executorOrder = append(executorOrder, "pre"+stepModel.ID)
					}
					return nil
				})

				sm.On("main").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "step"+stepModel.ID)
					if tt.timeout {
						expire()
						return ctx.Err()
					}
					if tt.stepFails {
						return errors.New("error")
					}
					return nil
				})

				sm.On("post").Return(func(ctx context.Context) error {
					if slices.Contains(tt.postSteps, i+1) {
						executorOrder = append(executorOrder, "post"+stepModel.ID)
					}
					if tt.timeout {
						require.NoError(t, ctx.Err(), "post step must run against a fresh, non-expired context")
					}
					return nil
				})

				defer sm.AssertExpectations(t)
			}

			if tt.steps > 0 && tt.startError == nil {
				jim.On("interpolateOutputs").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "interpolateOutputs")
					if tt.output != "" {
						return rc.interpolateOutputs()(ctx)
					}
					return nil
				})

				jim.On("stopContainer").Return(func(ctx context.Context) error {
					executorOrder = append(executorOrder, "stopContainer")
					require.NoError(t, ctx.Err())
					_, bounded := ctx.Deadline()
					require.True(t, bounded)
					return nil
				})
			}

			if tt.steps > 0 {
				jim.On("closeContainer").Return(func(_ctx context.Context) error {
					executorOrder = append(executorOrder, "closeContainer")
					if tt.cancelOnClose {
						cancel()
					}
					return nil
				})
			}

			err := newJobExecutor(jim, sfm, rc)(ctx)
			switch {
			case tt.startError != nil:
				require.ErrorIs(t, err, tt.startError)
			case tt.cancelOnStart:
				require.ErrorIs(t, err, context.Canceled)
			default:
				require.NoError(t, err)
			}
			assert.Equal(t, tt.result, rc.Run.Job().Result)
			if tt.output != "" {
				assert.Empty(t, rc.Run.Job().Outputs["bad"])
			}
			assert.Equal(t, tt.executedSteps, executorOrder)

			jim.AssertExpectations(t)
			sfm.AssertExpectations(t)
		})
	}
}

type controllableDeadlineContext struct {
	context.Context
	done chan struct{}
}

func newControllableDeadlineContext(parent context.Context) *controllableDeadlineContext {
	return &controllableDeadlineContext{Context: parent, done: make(chan struct{})}
}

func (ctx *controllableDeadlineContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *controllableDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *controllableDeadlineContext) expire() {
	close(ctx.done)
}

func TestSetJobResultMatrixContinueOnError(t *testing.T) {
	ctx := t.Context()
	for name, tolerated := range map[string]bool{
		"one tolerated and one firm failure fails the run and its caller": false,
		"all tolerated failures fail neither the run nor its caller":      true,
	} {
		t.Run(name, func(t *testing.T) {
			run := makeTestRC(t, "continue-on-error: ${{ matrix.experimental }}").Run
			job := run.Job()
			var siblings sync.WaitGroup
			for _, experimental := range []bool{true, tolerated} {
				rc := newTestRC(run.Workflow, map[string]any{"experimental": experimental})
				rc.Config.PresetGitHubContext = &model.GithubContext{}
				rc.jobTotal = 2
				siblings.Go(func() { rc.setJobResult(ctx, false) })
			}
			siblings.Wait()

			assert.Equal(t, "failure", job.Result)
			assert.Equal(t, tolerated, job.ContinueOnError)
			err := handleFailure(&model.Plan{Stages: []*model.Stage{{Runs: []*model.Run{run}}}})(ctx)
			if tolerated {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			callerRC := makeTestRC(t, "{}")
			callerRC.Config.PresetGitHubContext = &model.GithubContext{}
			(&caller{runContext: callerRC, reportedRuns: []*model.Run{run}}).finishWorkflowCall(ctx, nil)
			assert.Equal(t, map[bool]string{true: "success", false: "failure"}[tolerated], callerRC.Run.Job().Result)
		})
	}
}

func TestSkippedMatrixCombinationsPreserveJobResult(t *testing.T) {
	for _, test := range []struct{ prior, afterSkip, afterRun string }{
		{"", "skipped", "success"},
		{"failure", "failure", "failure"},
		{"cancelled", "cancelled", "cancelled"},
	} {
		t.Run(test.afterSkip, func(t *testing.T) {
			rc := makeTestRC(t, "if: false")
			rc.jobTotal = 3
			rc.caller = &caller{reportedRuns: []*model.Run{nil}}
			rc.Run.Job().Result = test.prior
			enabled, err := rc.isEnabled(t.Context())
			require.NoError(t, err)
			assert.False(t, enabled)
			require.Same(t, rc.Run, rc.caller.reportedRuns[0])
			assert.Equal(t, test.afterSkip, rc.Run.Job().Result)
			rc.setJobResult(t.Context(), test.prior != "cancelled")
			assert.Equal(t, test.afterRun, rc.Run.Job().Result)
		})
	}
}

func TestHasJobSummaryCapability(t *testing.T) {
	assert.True(t, hasJobSummaryCapability("cache,job-summary artifacts"))
	assert.True(t, hasJobSummaryCapability("cache,\njob-summary\tartifacts"))
	assert.False(t, hasJobSummaryCapability("not-job-summary,job-summary-v2"))
}

func fakeRuntimeToken(jobID int64) string {
	return "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"JobID":%d}`, jobID)) + ".c2ln"
}

func newJobSummaryRC(t *testing.T, transport summaryTransport) *RunContext {
	t.Helper()
	const runtimeURL = "https://runtime.example"
	rc := makeTestRC(t, "{}")
	rc.JobContainer = &container.HostEnvironment{ActPath: t.TempDir()}
	rc.Config.GitHubInstance, rc.Config.InstanceTransport = runtimeURL, transport
	rc.Env["GITEA_ACTIONS_CAPABILITIES"] = "job-summary"
	rc.Env["ACTIONS_RUNTIME_URL"] = runtimeURL
	rc.Env["ACTIONS_RUNTIME_TOKEN"] = fakeRuntimeToken(34)
	rc.Env["GITEA_RUN_ID"] = "12"
	return rc
}

type summaryTransport func(*http.Request) (*http.Response, error)

func (transport summaryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestStepSummaryRetriesWithinPhaseTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var attempts []string
		started := make(chan struct{})
		var finished time.Time
		rc := newJobSummaryRC(t, func(request *http.Request) (*http.Response, error) {
			defer request.Body.Close()
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			attempts = append(attempts, string(body))
			if len(attempts) == 1 {
				close(started)
			}
			<-request.Context().Done()
			finished = time.Now()
			return nil, request.Context().Err()
		})
		summaryPath := container.JoinPath(rc.JobContainer.GetActPath(), "summary.md")
		steps := []*model.Step{{ID: "first"}, {ID: "second"}}
		job := &jobInfoMock{}
		job.On("steps").Return(steps)
		for _, method := range []string{"startContainer", "interpolateOutputs", "stopContainer", "closeContainer"} {
			job.On(method).Return(noopExecutor)
		}
		step := &stepMock{}
		step.On("pre").Return(noopExecutor)
		step.On("post").Return(noopExecutor)
		step.On("main").Return(func(ctx context.Context) error {
			require.NoError(t, os.WriteFile(summaryPath, []byte(steps[rc.CurrentStepIndex].ID), 0o600))
			processStepSummary(ctx, rc, summaryPath)
			<-started
			assert.Zero(t, time.Since(start))
			return nil
		})
		factory := &stepFactoryMock{}
		factory.On("newStep", mock.Anything, rc).Return(step, nil)
		require.NoError(t, newJobExecutor(job, factory, rc)(t.Context()))
		assert.Equal(t, jobSummaryUploadPhaseTimeout, time.Since(start))
		synctest.Wait()
		assert.Equal(t, []string{"first", "first", "second"}, attempts)
		assert.Equal(t, "success", rc.Run.Job().Result)
		assert.Equal(t, time.Now(), finished)
	})
}

func TestStepSummaryUploadsAfterEachOuterPhase(t *testing.T) {
	type upload struct{ path, body string }
	var got []upload
	rc := newJobSummaryRC(t, func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "Bearer "+fakeRuntimeToken(34), r.Header.Get("Authorization"))
		assert.Equal(t, "text/markdown; charset=utf-8", r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		got = append(got, upload{r.URL.Path, string(body)})
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, err
	})
	ctx := t.Context()
	rc.summaryUploads = newStepSummaryUploads(ctx)
	outer := &stepRun{RunContext: rc, Step: &model.Step{ID: "outer"}, env: map[string]string{}}
	childRC := &RunContext{Parent: rc, Run: rc.Run, Config: rc.Config, JobContainer: rc.JobContainer}
	childRC.ExprEval = childRC.NewExpressionEvaluator(ctx)
	child := &stepRun{RunContext: childRC, Step: &model.Step{ID: "child"}, env: map[string]string{}}

	rc.CurrentStepIndex = 1
	require.NoError(t, runStepExecutor(outer, stepStagePre, runStepExecutor(child, stepStagePre, func(ctx context.Context) error {
		childRC.AddMask(ctx, "private")
		return os.WriteFile(child.env["GITHUB_STEP_SUMMARY"], []byte("pre private"), 0o600)
	}))(ctx))

	rc.CurrentStepIndex = 0
	first := &stepRun{RunContext: rc, Step: &model.Step{ID: "first"}, env: map[string]string{}}
	require.NoError(t, runStepExecutor(first, stepStageMain, func(context.Context) error {
		return os.WriteFile(first.env["GITHUB_STEP_SUMMARY"], []byte("first"), 0o600)
	})(ctx))

	rc.CurrentStepIndex = 1
	require.NoError(t, runStepExecutor(outer, stepStageMain, func(context.Context) error {
		content, err := os.ReadFile(outer.env["GITHUB_STEP_SUMMARY"])
		require.NoError(t, err)
		return os.WriteFile(outer.env["GITHUB_STEP_SUMMARY"], append(content, " main"...), 0o600)
	})(ctx))

	rc.CurrentStepIndex = 2
	empty := &stepRun{RunContext: rc, Step: &model.Step{ID: "empty"}, env: map[string]string{}}
	require.NoError(t, runStepExecutor(empty, stepStageMain, func(context.Context) error {
		return os.Remove(first.env["GITHUB_STEP_SUMMARY"])
	})(ctx))

	rc.CurrentStepIndex = 1
	postCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.ErrorIs(t, runStepExecutor(outer, stepStagePost, func(context.Context) error {
		require.NoError(t, os.WriteFile(outer.env["GITHUB_STEP_SUMMARY"], []byte("post private"), 0o600))
		cancel()
		return postCtx.Err()
	})(postCtx), context.Canceled)

	for key, invalid := range map[string]string{"GITEA_ACTIONS_CAPABILITIES": "not-job-summary,job-summary-v2", "ACTIONS_RUNTIME_TOKEN": "not-a-jwt"} {
		saved := rc.Env[key]
		rc.Env[key] = invalid
		processStepSummary(ctx, rc, outer.env["GITHUB_STEP_SUMMARY"])
		rc.Env[key] = saved
	}
	rc.summaryUploads.wait(ctx)
	const base = "/_apis/pipelines/workflows/12/jobs/34/steps/"
	assert.Equal(t, []upload{
		{base + "1/summary", "pre ***"},
		{base + "0/summary", "first"},
		{base + "1/summary", "pre *** main"},
		{base + "1/summary", "post ***"},
	}, got)
}

func TestExtractJobIDFromRuntimeToken(t *testing.T) {
	assert.Equal(t, int64(42), extractJobIDFromRuntimeToken(fakeRuntimeToken(42)))
	assert.Equal(t, int64(0), extractJobIDFromRuntimeToken("not-a-jwt"))
	assert.Equal(t, int64(0), extractJobIDFromRuntimeToken("a.b.c"))
	assert.Equal(t, int64(0), extractJobIDFromRuntimeToken(""))
}

func TestReadSingleFileFromContainerArchive(t *testing.T) {
	retainedBytes := 200 - len(jobSummaryTruncationMarker)
	for name, tt := range map[string]struct {
		content  string
		maxBytes int64
		want     string
	}{
		"truncates oversized summary without reading entire file": {strings.Repeat("a", 100000), 200, strings.Repeat("a", 200-len(jobSummaryTruncationMarker)) + jobSummaryTruncationMarker},
		"keeps exact limit":               {"abc", 3, "abc"},
		"strips UTF-8 BOM before limit":   {"\xef\xbb\xbfabc", 3, "abc"},
		"decodes UTF-16LE before limit":   {"\xff\xfea\x00b\x00c\x00", 3, "abc"},
		"decodes UTF-16BE before limit":   {"\xfe\xff\x00a\x00b\x00c", 3, "abc"},
		"limits expanded UTF-16":          {"\xff\xfe" + strings.Repeat("\x2d\x4e", 150), 200, strings.Repeat("中", retainedBytes/3) + jobSummaryTruncationMarker},
		"keeps complete UTF-8 characters": {strings.Repeat("a", retainedBytes-1) + "€" + strings.Repeat("a", 200), 200, strings.Repeat("a", retainedBytes-1) + jobSummaryTruncationMarker},
	} {
		t.Run(name, func(t *testing.T) {
			logger, hook := logtest.NewLogger()
			ctx := common.WithLogger(t.Context(), logger)
			cm := &containerMock{}
			archive := bytes.NewReader(tarArchive(t,
				tarEntry{name: "workflow", typeflag: tar.TypeDir},
				tarEntry{name: "other.md", body: "wrong"},
				tarEntry{name: "SUMMARY.md", body: tt.content},
			))
			cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/SUMMARY.md").Return(
				io.NopCloser(archive),
				nil,
			).Once()

			body, ok := readSingleFileFromContainerArchive(ctx, cm, "/var/run/act/workflow/SUMMARY.md", tt.maxBytes+1)
			assert.LessOrEqual(t, int64(len(body)), tt.maxBytes+1)
			body = truncateJobSummary(ctx, body, "/var/run/act/workflow/SUMMARY.md", int(tt.maxBytes), false)

			assert.True(t, ok)
			assert.Equal(t, []byte(tt.want), body)
			assert.LessOrEqual(t, int64(len(body)), tt.maxBytes)
			if strings.HasSuffix(tt.want, jobSummaryTruncationMarker) {
				if assert.Len(t, hook.AllRecords(), 1) {
					assert.Contains(t, hook.LastRecord().Message, "job summary truncated")
				}
			} else {
				assert.Empty(t, hook.AllRecords())
			}
			cm.AssertExpectations(t)
		})
	}
}

type tarEntry struct {
	name     string
	body     string
	typeflag byte
}

func tarArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()

	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for _, entry := range entries {
		buf.Grow(len(entry.body) + 3*512)
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.name,
			Typeflag: typeflag,
			Mode:     0o644,
			Size:     int64(len(entry.body)),
		}
		if typeflag == tar.TypeDir {
			header.Mode = 0o755
			header.Size = 0
		}
		require.NoError(t, tw.WriteHeader(header))
		if typeflag == tar.TypeReg {
			_, err := io.CopyN(tw, strings.NewReader(entry.body), int64(len(entry.body)))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func newTestRC(wf *model.Workflow, matrix map[string]any) *RunContext {
	return &RunContext{
		Config:      &Config{Workdir: ".", Env: testCheckoutEnv(), PlatformPicker: func([]string) string { return "ubuntu-latest" }},
		StepResults: map[string]*model.StepResult{},
		Env:         map[string]string{},
		Matrix:      matrix,
		Run:         &model.Run{JobID: "job1", Workflow: wf},
	}
}

func makeTestRC(t *testing.T, jobYAML string) *RunContext {
	t.Helper()
	var job *model.Job
	require.NoError(t, yaml.Unmarshal([]byte(jobYAML), &job))
	rc := newTestRC(&model.Workflow{
		Name: "workflow1",
		Jobs: map[string]*model.Job{"job1": job},
	}, nil)
	rc.Config.PresetGitHubContext = &model.GithubContext{}
	rc.ExprEval = rc.NewExpressionEvaluator(t.Context())
	return rc
}

func TestApplyJobTimeout(t *testing.T) {
	for input, want := range map[string]time.Duration{"": 0, "5": 5 * time.Minute, "0": 0} {
		synctest.Test(t, func(t *testing.T) {
			rc := makeTestRC(t, "runs-on: ubuntu-latest")
			rc.Run.Job().TimeoutMinutes = input
			logger, hook := logtest.NewLogger()
			ctx, cancel, err := applyJobTimeout(common.WithLogger(t.Context(), logger), rc, rc.Run.Job())
			require.NoError(t, err)
			defer cancel()
			deadline, ok := ctx.Deadline()
			require.Equal(t, want > 0, ok)
			if ok {
				assert.Equal(t, want, time.Until(deadline))
				<-ctx.Done()
				reportStepError(ctx, rc, ctx.Err())
				rc.markJobInterrupted(ctx)
				require.Len(t, hook.AllRecords(), 1)
				assert.Equal(t, "##[error]The job has exceeded the maximum execution time of 5m0s", hook.LastRecord().Message)
				rc.setJobResult(ctx, true)
				assert.Equal(t, "cancelled", rc.Run.Job().Result)
			}
		})
	}
}

func TestEvaluateJobContinueOnError(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"absent", "runs-on: ubuntu-latest", false},
		{"true", "continue-on-error: true\nruns-on: ubuntu-latest", true},
		{"false", "continue-on-error: false\nruns-on: ubuntu-latest", false},
		{"expression true", "continue-on-error: ${{ 'x' == 'x' }}\nruns-on: ubuntu-latest", true},
		{"expression false", "continue-on-error: ${{ 'x' != 'x' }}\nruns-on: ubuntu-latest", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := makeTestRC(t, tc.yaml)
			got := evaluateJobContinueOnError(t.Context(), rc, rc.Run.Job())
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestJobSetContinueOnError(t *testing.T) {
	for name, tt := range map[string]struct {
		values []bool
		want   bool
	}{
		"first call true":                {[]bool{true}, true},
		"first call false":               {[]bool{false}, false},
		"true then false locks to false": {[]bool{true, false}, false},
		"false then true stays false":    {[]bool{false, true}, false},
		"true then true stays true":      {[]bool{true, true}, true},
	} {
		t.Run(name, func(t *testing.T) {
			job := &model.Job{}
			for _, value := range tt.values {
				job.SetContinueOnError(value)
			}
			assert.Equal(t, tt.want, job.ContinueOnError)
		})
	}
}

func TestStepSummaryMasksSecrets(t *testing.T) {
	const prefix = "deployed true with s3cr3t and runtime-added via pr0xypw 😀"
	const maskedPrefix = "deployed true with *** and *** via *** 😀"
	padding := strings.Repeat("a", protocol.MaxStepSummarySize-len(jobSummaryTruncationMarker)-len(prefix)-2)
	content := prefix + padding + "s3cr3t" + strings.Repeat("中", 100)
	want := maskedPrefix + padding + "***" + strings.Repeat("中", (len(prefix)-len(maskedPrefix)-1)/3) + jobSummaryTruncationMarker
	longSecret := "PREFIX" + strings.Repeat("xy", 100) + "Z"
	readLimit := protocol.MaxStepSummarySize + base64.StdEncoding.EncodedLen(len(longSecret)) + 1
	shrinkingPrefix := strings.Repeat(longSecret, (readLimit-len(longSecret)+1)/len(longSecret))
	for name, testcase := range map[string]struct {
		content, want string
		masks         []string
	}{
		"mask before truncation":   {content, want, nil},
		"secret split by the read": {strings.Repeat("s3cr3t", protocol.MaxStepSummarySize/3), "", nil},
		"split read keeps every masked secret before the limit": {
			"# public\n" + strings.Repeat(longSecret, readLimit/len(longSecret)+1),
			"# public\n" + strings.Repeat("***", (protocol.MaxStepSummarySize-len("# public\n"))/len(longSecret)) + jobSummaryTruncationMarker,
			[]string{longSecret},
		},
		"nested secret expands the split tail": {
			shrinkingPrefix + strings.Repeat("*", readLimit-len(shrinkingPrefix)-len(longSecret)+1) + longSecret,
			"",
			[]string{"xy", longSecret},
		},
	} {
		t.Run(name, func(t *testing.T) {
			summaryPath := container.JoinPath(t.TempDir(), "summary.md")
			require.NoError(t, os.WriteFile(summaryPath, []byte(testcase.content), 0o600))
			rc := &RunContext{
				Config: &Config{
					Secrets:    map[string]string{"TOK": "s3cr3t", "ACTIONS_STEP_DEBUG": "true"},
					ExtraMasks: append([]string{"pr0xypw"}, testcase.masks...),
				},
				JobContainer: &container.HostEnvironment{},
				Masks:        []string{"runtime-added"},
			}
			got := string(readStepSummary(t.Context(), rc, summaryPath))
			if testcase.want == "" {
				assert.Equal(t, jobSummaryTruncationMarker, strings.TrimLeft(got, "*"))
			} else {
				assert.Equal(t, testcase.want, got)
			}
		})
	}
}
