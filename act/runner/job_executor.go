// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"gitea.dev/actionslib/pkg/protocol"
	unicodeencoding "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

const (
	jobCleanupTimeout            = time.Minute
	jobSummaryUploadPhaseTimeout = 15 * time.Second
)

// jobSummaryTruncationMarker is appended to a summary that exceeded the size limit
// so the rendered output makes the truncation visible instead of silently cutting off.
const jobSummaryTruncationMarker = "\n\n---\n\n*Job summary truncated: it exceeded the maximum allowed size.*\n"

type jobInfo interface {
	steps() []*model.Step
	startContainer() common.Executor
	stopContainer() common.Executor
	closeContainer() common.Executor
	interpolateOutputs() common.Executor
}

func reportStepError(ctx context.Context, rc *RunContext, err error) {
	if ctx.Err() != nil {
		rc.markJobInterrupted(ctx)
	} else if err != nil && !errors.Is(err, context.Canceled) { // A live job can receive cancellation from step teardown.
		common.Logger(ctx).Errorf("##[error]%s", EscapeCommandData(err.Error()))
		common.SetJobError(ctx, err)
		rc.markFailed()
	}
}

// actionPreparer is implemented by steps that download an action before they run, so the job
// executor can fetch all of them up front.
type actionPreparer interface {
	prepareActionExecutor() common.Executor
	actionDownloadInfo() (reference, sha string)
}

// printPrepareActions downloads every action the job uses before its first step runs and reports
// them as actions/runner's "Prepare all required actions" section does. The steps still call
// prepareActionExecutor themselves; it is a no-op once the action is resolved here.
func printPrepareActions(rc *RunContext, preparers []actionPreparer) common.Executor {
	return func(ctx context.Context) error {
		if len(preparers) == 0 {
			return nil
		}

		rawLogger := common.Logger(ctx).With(rawOutputField, true)
		rawLogger.Infof("Prepare all required actions")

		fetchCtx, cancelFetches := context.WithCancel(ctx)
		var fetches sync.WaitGroup
		defer func() { cancelFetches(); fetches.Wait() }()
		slots := make(chan struct{}, 4)
		job := rc.topLevelRunContext()
		if job.actionDownloads == nil {
			job.actionDownloads = map[string]*actionDownload{}
		}
		for _, preparer := range preparers {
			action, ok := preparer.(*stepActionRemote)
			if !ok || strings.Contains(action.Step.Uses, "${{") || action.action != nil {
				continue
			}
			github := action.RunContext.getGithubContext(common.WithLogger(ctx, logging.New(logging.DiscardHandler)))
			remote, err := newRemoteAction(action.Step.Uses, github, action.RunContext.Config.GitHubInstance)
			if err != nil || action.skipsLocalCheckout(remote, github) {
				continue
			}
			key := remote.downloadKey(action.RunContext.Config.DefaultActionURL())
			if job.actionDownloads[key] != nil {
				continue
			}
			download := &actionDownload{fetched: make(chan struct{})}
			job.actionDownloads[key] = download
			fetches.Go(func() {
				defer close(download.fetched)
				defer common.RecoverPanic(&download.err)
				slots <- struct{}{}
				defer func() { <-slots }()
				download.dir, download.sha, download.err = action.fetchAction(fetchCtx, remote, github.Token)
			})
		}

		for _, preparer := range preparers {
			if err := preparer.prepareActionExecutor()(ctx); err != nil {
				// No step has run yet, so the failure belongs to the job.
				reportStepError(ctx, rc, err)
				return setupError{err}
			}
			reference, sha := preparer.actionDownloadInfo()
			if sha == "" {
				continue
			}
			rawLogger.Infof("Download action repository '%s' (SHA:%s)", reference, sha)
		}
		return nil
	}
}

// setupError is a failure of the job setup, such as an action that cannot be downloaded.
type setupError struct{ error }

func (e setupError) Unwrap() error { return e.error }

// printCompleteJobName closes the setup section the way actions/runner ends its "Set up job" step.
func printCompleteJobName(rc *RunContext) common.Executor {
	return func(ctx context.Context) error {
		common.Logger(ctx).With(rawOutputField, true).Infof("Complete job name: %s", cmp.Or(rc.JobName, rc.Name, rc.Run.JobID)) // Name holds a matrix combination; JobName is the shared name GitHub reports.
		return nil
	}
}

func newJobExecutor(info jobInfo, sf stepFactory, rc *RunContext) common.Executor {
	var steps, preSteps, stepPreSteps []common.Executor // Collected separately: every action is downloaded before the first pre step runs.
	var preparers []actionPreparer
	var postExecutor common.Executor

	infoSteps := info.steps()

	if len(infoSteps) == 0 {
		return common.NewDebugExecutor("No steps found")
	}

	preSteps = append(preSteps, func(ctx context.Context) error {
		if len(rc.Matrix) > 0 {
			common.Logger(ctx).Infof("Matrix: %v", rc.Matrix)
		}
		return nil
	})
	preSteps = append(preSteps, func(ctx context.Context) error {
		if err := evaluateJobEnvAndDefaults(ctx, rc); err != nil {
			reportStepError(ctx, rc, err)
			return err
		}
		return nil
	})

	for i, stepModel := range infoSteps {
		if stepModel == nil {
			return common.NewErrorExecutor(fmt.Errorf("invalid Step %v: missing run or uses key", i))
		}
		if stepModel.ID == "" {
			stepModel.ID = strconv.Itoa(i)
		}
		stepModel.Number = i

		step, err := sf.newStep(stepModel, rc)
		if err != nil {
			return common.NewErrorExecutor(err)
		}

		if preparer, ok := step.(actionPreparer); ok {
			preparers = append(preparers, preparer)
		}

		runPhase := func(stage stepStage, executor common.Executor) common.Executor {
			return useStepLogger(rc, stepModel, stage, func(ctx context.Context) error {
				rc.CurrentStepIndex = i
				err := executor(ctx)
				reportStepError(ctx, rc, err)
				return err
			})
		}
		stepPreSteps = append(stepPreSteps, runPhase(stepStagePre, step.pre()))
		steps = append(steps, runPhase(stepStageMain, step.main()))
		postExec := runPhase(stepStagePost, step.post())
		if postExecutor != nil {
			// run the post executor in reverse order
			postExecutor = postExec.Finally(postExecutor)
		} else {
			postExecutor = postExec
		}
	}

	// The setup section of the job log. The started hook goes first, so what it sets up is
	// in place for the first action download and the first step.
	// The pre steps come after it, as steps of their own.
	preSteps = append(preSteps, rc.runJobStartedHook, printPrepareActions(rc, preparers), printCompleteJobName(rc))
	runSteps := newStepsExecutor(rc, append(preSteps, stepPreSteps...), steps)

	// Ahead of the teardown below, while the job environment is still up.
	postExecutor = postExecutor.Finally(useJobStageLogger(StageComplete, rc.runJobCompletedHook))
	postExecutor = postExecutor.Finally(useJobStageLogger(StageComplete, func(ctx context.Context) error {
		reportStepError(ctx, rc, info.interpolateOutputs()(ctx))                          // A bad output fails the job without abandoning the plan.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobCleanupTimeout) // Teardown needs time even after cancellation.
		defer cancel()

		rc.summaryUploads.wait(ctx)
		logger := common.Logger(ctx)
		logger.Infof("Cleaning up container for job %s", rc.JobName)
		err := info.stopContainer()(ctx)
		if err != nil {
			logger.Errorf("##[error]%s", EscapeCommandData("Error while stop job container: "+err.Error()))
		}
		rc.removeDockerActionImages(ctx)

		return err
	}))

	return common.Executor(func(ctx context.Context) error {
		if err := info.startContainer()(ctx); err != nil {
			rc.markFailed()
			return err
		}
		rc.summaryUploads = newStepSummaryUploads(ctx)
		return runSteps.Finally(func(ctx context.Context) error {
			rc.markJobInterrupted(ctx) // Post steps must see interrupts that landed after the last main step.
			postCtx, cancel := postStepsContext(ctx)
			defer cancel()
			return postExecutor(postCtx)
		})(ctx)
	}).Finally(info.closeContainer()).Finally(useJobStageLogger(StageComplete, func(ctx context.Context) error {
		rc.markJobInterrupted(ctx)
		rc.setJobResult(ctx, common.JobError(ctx) == nil)
		return nil
	}))
}

func postStepsContext(ctx context.Context) (context.Context, context.CancelFunc) {
	switch ctx.Err() {
	case context.Canceled:
		return context.WithTimeout(common.WithJobErrorContainer(context.WithoutCancel(ctx)), 5*time.Minute) // a failed post step cannot replace a server cancellation
	case context.DeadlineExceeded:
		return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	default:
		return ctx, func() {}
	}
}

func newStepsExecutor(rc *RunContext, preSteps, steps []common.Executor) common.Executor {
	preExecutor := common.NewPipelineExecutor(preSteps...)
	return func(ctx context.Context) error {
		preErr := preExecutor(ctx)
		if _, setupFailed := errors.AsType[setupError](preErr); setupFailed {
			return preErr // as on GitHub, no step runs after the job setup failed
		}
		for i, step := range steps {
			if err := ctx.Err(); err != nil {
				rc.markJobInterrupted(ctx)
				freshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
				defer cancel()
				for _, remaining := range steps[i:] {
					_ = remaining(freshCtx)
				}
				return cmp.Or(preErr, err)
			}
			_ = step(ctx)
		}
		rc.markJobInterrupted(ctx)
		return preErr
	}
}

func (rc *RunContext) setJobResult(ctx context.Context, success bool) {
	success = success && !rc.jobFailed

	job := rc.Run.Job()
	var continueOnError bool
	if !success && !rc.jobCancelled {
		continueOnError = evaluateJobContinueOnError(context.WithoutCancel(ctx), rc, job)
	}
	defer lockJob(job)() // matrix combinations share this result and its caller notification
	jobResult := "success"
	if rc.jobTotal != 1 && job.Result != "" && job.Result != "skipped" {
		jobResult = job.Result
	}
	switch {
	case rc.jobCancelled:
		jobResult = "cancelled"
	case !success && jobResult != "cancelled":
		jobResult = "failure"
		job.SetContinueOnError(continueOnError)
	}
	job.Result = jobResult

	if rc.caller != nil {
		rc.caller.reportedRuns[rc.callerRunIndex] = rc.Run
		return
	}

	jobResultMessage := "failed"
	switch jobResult {
	case "success":
		jobResultMessage = "succeeded"
	case "cancelled":
		jobResultMessage = "cancelled"
	}

	common.Logger(ctx).With("jobResult", jobResult).Infof("Job %s", jobResultMessage)
}

// evaluateJobEnv returns the workflow and job env evaluated, as far as it got on error, leaving rc.Env raw.
func evaluateJobEnv(ctx context.Context, rc *RunContext) (map[string]string, error) {
	env := rc.GetEnv()
	evaluated := maps.Clone(env)
	evaluator := rc.NewExpressionEvaluatorWithEnv(ctx, evaluated)
	var workflowEnv map[string]string
	if rc.Run.Workflow.Env == nil {
		if err := model.DecodeEvaluated("workflow env", rc.Run.Workflow.RawEnv, evaluator.shared(ctx).EvaluateYamlNode, &workflowEnv); err != nil {
			return evaluated, err
		}
	}
	if workflowEnv != nil {
		evaluated = rc.mergeEnv(workflowEnv, env)
		evaluator = rc.NewExpressionEvaluatorWithEnv(ctx, evaluated)
	}
	var err error
	for k, v := range env {
		if evaluated[k], err = evaluator.Interpolate(ctx, v); err != nil {
			return evaluated, fmt.Errorf("unable to interpolate env %s: %w", k, err)
		}
	}
	return evaluated, nil
}

// evaluateJobEnvAndDefaults resolves the job's env, defaults.run and container env once, as GitHub does at job setup.
func evaluateJobEnvAndDefaults(ctx context.Context, rc *RunContext) error {
	env, err := evaluateJobEnv(ctx, rc)
	rc.Env = env
	rc.ExprEval = rc.NewExpressionEvaluator(ctx)
	if err != nil {
		return err
	}
	var defaults model.Defaults
	if err := model.DecodeEvaluated("defaults", rc.Run.Job().RawDefaults, rc.ExprEval.shared(ctx).EvaluateYamlNode, &defaults); err != nil {
		return err
	}
	rc.jobRunDefaults = defaults.Run
	return model.DecodeEvaluated("container env", rc.containerEnv, rc.ExprEval.shared(ctx).EvaluateYamlNode, &rc.containerSpec.Env)
}

type jobTimeout time.Duration

func (timeout jobTimeout) Error() string {
	return fmt.Sprintf("The job has exceeded the maximum execution time of %s", time.Duration(timeout))
}

func (jobTimeout) Unwrap() error { return context.DeadlineExceeded }

type jobTimeoutCancellation struct{}

func (jobTimeoutCancellation) Error() string { return "The operation was canceled." }
func (jobTimeoutCancellation) Unwrap() error { return context.Canceled }

func (rc *RunContext) markJobInterrupted(ctx context.Context) {
	switch ctx.Err() {
	case context.Canceled:
		rc.markCancelled()
	case context.DeadlineExceeded:
		if timeout, ok := context.Cause(ctx).(jobTimeout); ok {
			if !rc.jobCancelled {
				common.Logger(ctx).Errorf("##[error]%s", timeout)
			}
			rc.markCancelled()
		} else {
			rc.jobTimedOut = true
			rc.markFailed()
		}
	}
}

func applyJobTimeout(ctx context.Context, rc *RunContext, job *model.Job) (context.Context, context.CancelFunc, error) {
	if raw := job.TimeoutMinutes; raw != "" {
		minutes, err := rc.ExprEval.shared(ctx).EvaluateTimeoutMinutes(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("Error when evaluating 'timeout-minutes' for job '%s'. %w", rc.Run.JobID, err)
		}
		if minutes > 0 {
			timeout := time.Duration(minutes) * time.Minute
			ctx, cancel := context.WithTimeoutCause(ctx, timeout, jobTimeout(timeout))
			return ctx, cancel, nil
		}
	}
	return ctx, func() {}, nil
}

// evaluateJobContinueOnError evaluates the job-level continue-on-error expression.
func evaluateJobContinueOnError(ctx context.Context, rc *RunContext, job *model.Job) bool {
	expr := strings.TrimSpace(job.RawContinueOnError)
	if expr == "" {
		return false
	}
	continueOnError, err := EvalBool(ctx, rc.NewExpressionEvaluator(ctx), expr, exprparser.DefaultStatusCheckNone)
	if err != nil {
		common.Logger(ctx).Warnf("continue-on-error expression %q evaluation failed: %v", expr, err)
		return false
	}
	return continueOnError
}

type stepSummaryUpload struct {
	done   chan struct{}
	status int
	err    error
}

type stepSummaryUploads struct {
	ctx    context.Context
	cancel context.CancelFunc
	steps  []*stepSummaryUpload
}

func newStepSummaryUploads(ctx context.Context) *stepSummaryUploads {
	uploadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &stepSummaryUploads{ctx: uploadCtx, cancel: cancel}
}

func (uploads *stepSummaryUploads) wait(ctx context.Context) {
	defer uploads.cancel()
	if len(uploads.steps) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, jobSummaryUploadPhaseTimeout) // A slow endpoint must leave time for teardown.
	defer cancel()
	logger := common.Logger(ctx)
	for _, upload := range uploads.steps {
		select {
		case <-upload.done:
			if upload.err != nil {
				logger.WithError(upload.err).Warn("job summary upload failed")
			} else if upload.status/100 != 2 {
				logger.Warnf("job summary upload failed: status=%d", upload.status)
			}
		case <-ctx.Done():
			logger.WithError(ctx.Err()).Warn("job summary upload timed out")
			return
		}
	}
}

func processStepSummary(ctx context.Context, rc *RunContext, summaryPath string) {
	job := rc.topLevelRunContext()
	uploads := job.summaryUploads
	if uploads == nil {
		return
	}
	env := job.GetEnv()
	if !hasJobSummaryCapability(env["GITEA_ACTIONS_CAPABILITIES"]) {
		return
	}
	if rc != job {
		job.Masks = appendUniqueMasks(slices.Clip(job.Masks), rc.Masks) // composite masks must survive later writes to the child's slice
		return
	}
	runtimeURL := strings.TrimSpace(env["ACTIONS_RUNTIME_URL"])
	runtimeToken := strings.TrimSpace(env["ACTIONS_RUNTIME_TOKEN"])
	runID := strings.TrimSpace(env["GITEA_RUN_ID"])
	jobID := extractJobIDFromRuntimeToken(runtimeToken)
	if runtimeURL == "" || runID == "" || jobID <= 0 {
		return
	}

	body := readStepSummary(ctx, rc, summaryPath)
	if len(body) == 0 {
		return
	}
	url := fmt.Sprintf("%s/_apis/pipelines/workflows/%s/jobs/%d/steps/%d/summary", strings.TrimRight(runtimeURL, "/"), runID, jobID, rc.CurrentStepIndex)
	transport := rc.Config.instanceTransportFor(url)
	var previousDone <-chan struct{}
	if last := len(uploads.steps) - 1; last >= 0 {
		previousDone = uploads.steps[last].done
	}
	upload := &stepSummaryUpload{done: make(chan struct{})}
	uploads.steps = append(uploads.steps, upload)
	uploadCtx := uploads.ctx
	go func() {
		defer close(upload.done)
		defer common.RecoverPanic(&upload.err)
		if previousDone != nil {
			select {
			case <-previousDone:
			case <-uploadCtx.Done():
				return
			}
		}
		if uploadCtx.Err() == nil {
			upload.status, upload.err = uploadJobSummary(uploadCtx, &http.Client{Timeout: 5 * time.Second, Transport: transport}, url, runtimeToken, body)
		}
	}()
}

func readStepSummary(ctx context.Context, rc *RunContext, summaryPath string) []byte {
	ctx, cancel := context.WithTimeout(ctx, jobSummaryUploadPhaseTimeout)
	defer cancel()
	var maskers []string
	limit := protocol.MaxStepSummarySize
	body, _ := readContainerFile(ctx, rc.JobContainer, summaryPath, func() int64 {
		maskers = rc.secretMaskers()
		for i := 0; i < len(maskers); i += 2 {
			limit = max(limit, protocol.MaxStepSummarySize+len(maskers[i]))
		}
		return int64(limit + 1)
	})
	if len(body) == 0 {
		return nil
	}
	masker := NewSecretReplacer(maskers)
	masked := masker.Replace(string(body))
	split := len(body) > limit
	if split { // the read can end inside a secret, so keep only what masks alike without the lookahead
		unsplit := masker.Replace(string(body[:protocol.MaxStepSummarySize]))
		kept := 0
		for kept < len(unsplit) && kept < len(masked) && unsplit[kept] == masked[kept] {
			kept++
		}
		for kept > 0 && kept < len(masked) && !utf8.RuneStart(masked[kept]) {
			kept--
		}
		masked = masked[:kept]
	}
	return truncateJobSummary(ctx, []byte(masked), summaryPath, protocol.MaxStepSummarySize, split)
}

// For Gitea
// instanceTransportFor returns InstanceTransport only for URLs on the registered address, as steps can rewrite the summary URL through GITHUB_ENV.
func (c Config) instanceTransportFor(rawURL string) http.RoundTripper {
	if !strings.HasPrefix(rawURL, c.GitHubInstance+"/") {
		return nil
	}
	return c.InstanceTransport
}

// extractJobIDFromRuntimeToken returns the JobID claim from an ACTIONS_RUNTIME_TOKEN JWT
// without verifying its signature. Returns 0 if the token is unparseable or has no JobID.
func extractJobIDFromRuntimeToken(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		JobID int64 `json:"JobID"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return claims.JobID
}

func hasJobSummaryCapability(caps string) bool {
	return slices.Contains(strings.FieldsFunc(caps, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	}), "job-summary")
}

func uploadJobSummary(ctx context.Context, client *http.Client, url, runtimeToken string, body []byte) (int, error) {
	status, err := putJobSummary(ctx, client, url, runtimeToken, body)
	if !isTransientJobSummaryUploadFailure(status, err) {
		return status, err
	}
	select {
	case <-ctx.Done():
		return status, ctx.Err()
	case <-time.After(time.Second):
		return putJobSummary(ctx, client, url, runtimeToken, body)
	}
}

func putJobSummary(ctx context.Context, client *http.Client, url, runtimeToken string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+runtimeToken)
	req.Header.Set("Content-Type", "text/markdown; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func isTransientJobSummaryUploadFailure(status int, err error) bool {
	return err != nil || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status/100 == 5
}

func readSingleFileFromContainerArchive(ctx context.Context, env container.ExecutionsEnvironment, p string, maxBytes int64) ([]byte, bool) {
	return readContainerFile(ctx, env, p, func() int64 { return maxBytes })
}

func readContainerFile(ctx context.Context, env container.ExecutionsEnvironment, p string, limit func() int64) ([]byte, bool) {
	rc, err := env.GetContainerArchive(ctx, p)
	if err != nil {
		return nil, false
	}
	defer rc.Close()

	requestedPath := path.Clean(strings.TrimPrefix(p, "/"))
	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if err != nil {
			return nil, false
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		entryName := path.Clean(strings.TrimPrefix(header.Name, "/"))
		if entryName != requestedPath && entryName != path.Base(requestedPath) {
			continue
		}
		if header.Size == 0 {
			return []byte{}, true
		}
		b, err := io.ReadAll(io.LimitReader(transform.NewReader(tr, unicodeencoding.BOMOverride(unicodeencoding.UTF8.NewDecoder())), limit()))
		if err != nil {
			return nil, false
		}
		return b, true
	}
}

func truncateJobSummary(ctx context.Context, body []byte, summaryPath string, maxBytes int, split bool) []byte {
	if !split && len(body) <= maxBytes {
		return body
	}
	keep := max(min(len(body), maxBytes-len(jobSummaryTruncationMarker)), 0)
	for keep > 0 && keep < len(body) && !utf8.RuneStart(body[keep]) {
		keep--
	}
	common.Logger(ctx).Warnf("job summary truncated: path=%s max=%d", summaryPath, maxBytes)
	return append(body[:keep], jobSummaryTruncationMarker...)
}

// useJobStageLogger tags the log entries of a job level executor with the stage they belong to.
func useJobStageLogger(stage string, executor common.Executor) common.Executor {
	return func(ctx context.Context) error {
		return executor(common.WithLogger(ctx, common.Logger(ctx).With("stage", stage)))
	}
}

func useStepLogger(rc *RunContext, stepModel *model.Step, stage stepStage, executor common.Executor) common.Executor {
	return func(ctx context.Context) error {
		ctx = withStepLogger(ctx, stepModel.Number, stepModel.ID, rc.ExprEval.InterpolateName(ctx, stepModel.String()), stage.String())
		return rc.withCommandLogWriter(ctx, executor)
	}
}
