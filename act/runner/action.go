// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/act/container"

	_ "embed"

	"gitea.dev/actionslib/pkg/expreval"
	"gitea.dev/actionslib/pkg/exprparser"
	"gitea.dev/actionslib/pkg/model"
	"github.com/kballard/go-shellquote"
)

type actionStep interface {
	step

	getActionModel() *model.Action
	getCompositeRunContext(ctx context.Context, inputs map[string]string) *RunContext
	getCompositeSteps() *compositeSteps
}

type readAction func(ctx context.Context, step *model.Step, actionDir, actionPath string, readFile actionYamlReader, writeFile fileWriter) (*model.Action, error)

type actionYamlReader func(filename string) (io.Reader, io.Closer, error)

type fileWriter func(filename string, data []byte, perm fs.FileMode) error

type runAction func(step actionStep) common.Executor

//go:embed res/trampoline.js
var trampoline string

var (
	ContainerImageExistsLocally     = container.ImageExistsLocally
	ContainerNewDockerBuildExecutor = container.NewDockerBuildExecutor
	dockerActionNamePattern         = regexp.MustCompile("[^a-zA-Z0-9]")
	inputEnvPattern                 = regexp.MustCompile("[^A-Z0-9-]")
)

func readActionImpl(ctx context.Context, step *model.Step, actionDir, actionPath string, readFile actionYamlReader, writeFile fileWriter) (*model.Action, error) {
	var readErrors []error
	for _, filename := range []string{"action.yml", "action.yaml", "Dockerfile", "dockerfile"} {
		manifest := strings.HasPrefix(filename, "action.")
		reader, closer, err := readFile(filename)
		if err != nil {
			readErrors = append(readErrors, fmt.Errorf("failed to read '%s' from action '%s' with path '%s' of step %w", filename, step.String(), actionPath, err))
			if os.IsNotExist(err) {
				continue
			}
			if manifest {
				return nil, errors.Join(readErrors...)
			}
			break
		}
		defer closer.Close()
		if manifest {
			action, err := model.ReadAction(reader)
			if err != nil {
				return nil, &fs.PathError{Op: "Failed to load", Path: filepath.Join(actionDir, actionPath, filename), Err: err}
			}
			return action, nil
		}
		action := &model.Action{Name: "(Synthetic)", Runs: model.ActionRuns{Using: model.ActionRunsUsingDocker, Image: filename}}
		common.Logger(ctx).Debugf("Using synthetic action %v for %s", action, filename)
		return action, nil
	}
	if val, ok := step.With["args"]; ok {
		if err := writeFile(filepath.Join(actionDir, actionPath, "trampoline.js"), []byte(trampoline), 0o644); err != nil {
			return nil, err
		}
		action := &model.Action{
			Name: "(Synthetic)",
			Inputs: map[string]model.Input{
				"args":    {},
				"cwd":     {Description: "(Actual working directory)", Default: filepath.Join(actionDir, actionPath)},
				"command": {Description: "(Actual program)", Default: val},
			},
			Runs: model.ActionRuns{Using: model.ActionRunsUsingNode12, Main: "trampoline.js"},
		}
		common.Logger(ctx).Debugf("Using synthetic action %v", action)
		return action, nil
	}
	return nil, errors.Join(readErrors...)
}

func maybeCopyToActionDir(ctx context.Context, step actionStep, actionDir, actionPath, containerActionDir string) error {
	logger := common.Logger(ctx)
	rc := step.getRunContext()
	stepModel := step.getStepModel()

	if stepModel.Type() != model.StepTypeUsesActionRemote {
		return nil
	}

	containerActionDirCopy := strings.TrimSuffix(containerActionDir, actionPath)
	logger.Debug(containerActionDirCopy)

	if !strings.HasSuffix(containerActionDirCopy, `/`) {
		containerActionDirCopy += `/`
	}

	unlock, err := git.AcquireCloneLock(ctx, actionDir)
	if err != nil {
		return err
	}
	defer unlock()

	if err := rc.JobContainer.CopyDir(containerActionDirCopy, actionDir+"/", false, true)(ctx); err != nil { // an exported commit has no .git to apply gitignore with
		return err
	}
	if rc.Config.NoActionPatch {
		return nil
	}
	patched := patchedScripts(ctx, actionDir, actionScriptPaths(actionDir, filepath.Join(actionDir, actionPath), step.getActionModel()))
	if len(patched) == 0 {
		return nil
	}
	return rc.JobContainer.Copy(containerActionDirCopy, patched...)(ctx)
}

func runActionImpl(step actionStep) common.Executor {
	return func(ctx context.Context) error {
		rc := step.getRunContext()
		rc.withGithubEnv(ctx, getStepGithubContext(ctx, step), *step.getEnv())
		populateEnvsFromSavedState(step.getEnv(), step, rc)
		return runActionStage(ctx, step, stepStageMain)
	}
}

func runActionStage(ctx context.Context, step actionStep, stage stepStage) error {
	rc := step.getRunContext()
	stepModel := step.getStepModel()
	action := step.getActionModel()
	inputs, err := rc.resolveActionInputs(ctx, step, action)
	if err != nil {
		return err
	}
	applyActionInputs(step, inputs)
	if stage == stepStageMain {
		warnUnexpectedInputs(ctx, action, stepModel.With)
	}
	if stage == stepStagePost {
		populateEnvsFromSavedState(step.getEnv(), step, rc)
	} else {
		printRunActionHeader(ctx, stepModel, action, *step.getEnv(), rc)
	}

	actionDir, actionPath := actionSourcePaths(step)
	actionName, containerActionDir := getContainerActionPaths(step)
	if stage == stepStageMain {
		common.Logger(ctx).Debugf("type=%v actionDir=%s actionPath=%s workdir=%s actionCacheDir=%s actionName=%s containerActionDir=%s", stepModel.Type(), actionDir, actionPath, rc.Config.Workdir, rc.ActionCacheDir(), actionName, containerActionDir)
	}
	using := action.Runs.Using
	switch {
	case using.IsDocker():
		return execAsDocker(ctx, step, stage, inputs, actionName, containerActionDir)
	case using.IsNode(), using.IsComposite(), using == model.ActionRunsUsingGo:
		if stage != stepStagePost {
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}
		}
		if using.IsComposite() {
			return execAsComposite(ctx, step, inputs)
		}
		return runActionScript(ctx, step, containerActionDir, [...]string{action.Runs.Pre, action.Runs.Main, action.Runs.Post}[stage])
	default:
		return fmt.Errorf("the runs.using key must be one of: %v, got %s", []string{
			model.ActionRunsUsingDocker,
			model.ActionRunsUsingNode12,
			model.ActionRunsUsingNode16,
			model.ActionRunsUsingNode20,
			model.ActionRunsUsingNode24,
			model.ActionRunsUsingComposite,
			model.ActionRunsUsingGo,
		}, using)
	}
}

func runActionScript(ctx context.Context, step actionStep, actionDir, script string) error {
	rc := step.getRunContext()
	env := step.getEnv()
	rc.ApplyExtraPath(ctx, env)
	if step.getActionModel().Runs.Using.IsNode() {
		args := nodeActionCommand(rc.node(ctx), container.JoinPath(actionDir, script))
		common.Logger(ctx).Debugf("executing remote job container: %s", args)
		return rc.JobContainer.Exec(args, *env, "", "")(ctx)
	}
	executable := script + ".out"
	return common.NewPipelineExecutor(
		rc.JobContainer.Exec([]string{"go", "build", "-o", executable, script}, *env, "", actionDir),
		rc.JobContainer.Exec([]string{container.JoinPath(actionDir, executable)}, *env, "", ""),
	)(ctx)
}

// /var/run is a symlink, so without the flag node's import.meta.url differs from argv[1], which ESM actions compare.
func nodeActionCommand(node, script string) []string {
	return []string{node, "--preserve-symlinks-main", script}
}

// dockerActionImageTag derives the local docker image tag used when an action
// is built from a Dockerfile.
//
// For Gitea: a local action (`uses: ./` or `uses: ./path`) has an actionName
// that is the workspace-relative path of the action. That path is identical
// across repositories (e.g. "./" for a self-referencing action), so without
// namespacing, every repository's local docker action would build and reuse the
// same `act-dockeraction:latest` image on a shared docker daemon. A subsequent
// repository would then silently run the image built for an earlier one.
// Including the repository keeps the tag stable for caching within a repository
// while preventing cross-repository collisions.
// See https://gitea.com/gitea/runner/issues/1039.
func dockerActionImageTag(repository, actionName string, localAction bool) string {
	name := actionName
	if localAction {
		name = path.Join(repository, actionName)
	}
	// The human-readable name is sanitized by collapsing every non-alphanumeric character to "-".
	sanitized := dockerActionNamePattern.ReplaceAllString(name, "-")
	if localAction {
		// For local actions a short hash of the raw repository and action path is appended so the tag stays unique per repository.
		sum := sha256.Sum256([]byte(repository + "\x00" + actionName))
		sanitized += "-" + hex.EncodeToString(sum[:])[:12]
	}
	// "-dockeraction" ensures that "./", "./test " won't get converted to "act-:latest", "act-test-:latest" which are invalid docker image names
	return strings.ToLower("act-" + strings.TrimLeft(sanitized+"-dockeraction:latest", "-"))
}

// One image per step and job: its stages share a build, and concurrent jobs never run each other's.
func (rc *RunContext) dockerActionImage(ctx context.Context, step actionStep, actionName string, localAction bool) string {
	job := rc.topLevelRunContext()
	if job.dockerActionImages == nil {
		job.dockerActionImages = map[*model.Step]string{}
	}
	image, ok := job.dockerActionImages[step.getStepModel()]
	if !ok {
		image = dockerActionImageTag(getStepGithubContext(ctx, step).Repository, actionName, localAction) + "-" + strings.ToLower(rand.Text())
		if !common.Dryrun(ctx) { // a dry run builds nothing to remove
			job.dockerActionImages[step.getStepModel()] = image
		}
	}
	return image
}

func execAsDocker(ctx context.Context, step actionStep, stage stepStage, inputs map[string]string, actionName, buildDir string) error {
	rc := step.getRunContext()
	if rc.isWindowsHost() {
		return errWindowsHostContainer
	}
	if rc.kubernetes {
		return errKubernetesDocker
	}

	action := step.getActionModel()
	remote, isRemote := step.(*stepActionRemote)
	image, prebuilt := strings.CutPrefix(action.Runs.Image, "docker://")
	if !prebuilt {
		image = rc.dockerActionImage(ctx, step, actionName, !isRemote)
	}
	cmd, err := evalDockerArgsAndEnv(ctx, step, action, inputs)
	if err != nil {
		return err
	}
	entrypoint, err := dockerEntrypoint(step, stage)
	if err != nil {
		return err
	}
	containerInput := stepContainerInput(ctx, step, image, cmd, entrypoint, rc.Config.ContainerOptions)
	stepContainer := ContainerNewContainer(containerInput)
	if prebuilt {
		return rc.runStepContainer(ctx, stepContainer, nil, rc.Config.ForcePull)
	}

	platform := containerInput.Platform
	exists, err := ContainerImageExistsLocally(ctx, image, platform)
	if err != nil {
		return err
	}
	if exists && !rc.Config.ForceRebuild {
		common.Logger(ctx).Debugf("image '%s' for architecture '%s' already exists", image, platform)
		return rc.runStepContainer(ctx, stepContainer, nil, false)
	}
	if isRemote {
		buildDir = path.Join(remote.actionDir, remote.remoteAction.Path)
	}
	contextDir, filename := filepath.Split(filepath.Join(buildDir, action.Runs.Image))
	common.Logger(ctx).Debugf("image '%s' for architecture '%s' will be built from context '%s'", image, platform, contextDir)
	buildImage := common.Executor(func(ctx context.Context) error {
		input := container.NewDockerBuildExecutorInput{
			ContextDir: contextDir,
			Dockerfile: filename,
			ImageTag:   image,
			Platform:   platform,
			BuildArgs:  rc.proxyBuildArgs(),
			Stdout:     common.NewLineWriter(rawLogLine(ctx)),
		}
		if isRemote {
			unlock, err := git.AcquireCloneLock(ctx, remote.actionDir) // held across the whole build, the daemon drains contextDir lazily
			if err != nil {
				return err
			}
			defer unlock()
		} else {
			input.Dockerignore, _ = readSingleFileFromContainerArchive(ctx, rc.JobContainer, contextDir+"/.dockerignore", 1<<20)
			buildContext, err := rc.JobContainer.GetContainerArchive(ctx, contextDir+"/.")
			if err != nil {
				return err
			}
			defer buildContext.Close()
			input.BuildContext = buildContext
		}
		return ContainerNewDockerBuildExecutor(input)(ctx)
	})
	return rc.runStepContainer(ctx, stepContainer, buildImage.Retry(3), false)
}

// dockerEntrypoint returns the entrypoint the action's image runs with for the given
// stage. Only the main stage honours the `entrypoint` input.
func dockerEntrypoint(step actionStep, stage stepStage) ([]string, error) {
	runs := step.getActionModel().Runs

	var entrypoint string
	switch stage {
	case stepStagePre:
		entrypoint = runs.PreEntrypoint
	case stepStagePost:
		entrypoint = runs.PostEntrypoint
	default:
		entrypoint = runs.Entrypoint
		if entrypoint == "" {
			if fields := strings.Fields(stepInput(step.getStepModel(), "entrypoint")); len(fields) > 0 {
				return fields, nil
			}
		}
	}

	if entrypoint == "" {
		return nil, nil
	}
	return shellquote.Split(entrypoint)
}

func evalDockerArgsAndEnv(ctx context.Context, step step, action *model.Action, inputs map[string]string) ([]string, error) {
	env := step.getEnv()
	ee := newActionInputsExpressionEvaluator(inputs)
	runsEnv := make(map[string]string, len(action.Runs.Env))
	var err error
	for name, expression := range action.Runs.Env {
		if runsEnv[name], err = ee.Interpolate(ctx, expression); err != nil {
			return nil, fmt.Errorf("unable to interpolate env %s: %w", name, err)
		}
	}
	*env = step.getRunContext().mergeEnv(runsEnv, *env, inputs)
	aliasGiteaEnv(*env)
	if action.Runs.Args == nil {
		return shellquote.Split(stepInput(step.getStepModel(), "args"))
	}
	cmd := make([]string, len(action.Runs.Args))
	for i, expression := range action.Runs.Args {
		if cmd[i], err = ee.Interpolate(ctx, expression); err != nil {
			return nil, fmt.Errorf("unable to interpolate runs.args: %w", err)
		}
	}
	return cmd, nil
}

func stepContainerInput(ctx context.Context, step step, image string, cmd, entrypoint []string, runnerOptions string) *container.NewContainerInput {
	rc := step.getRunContext()
	logWriter := rc.commandLogWriter(ctx)
	envList := make([]string, 0)
	for k, v := range *step.getEnv() {
		if rc.IsHostEnv() && strings.EqualFold(k, rc.JobContainer.GetPathVariableName()) && v == rc.JobContainer.DefaultPathVariable() { // the runner's own PATH stays out, as on GitHub
			continue
		}
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	envList = append(envList, rc.runnerEnv(ctx)...)

	binds, mounts, _ := rc.GetBindsAndMounts(ctx)
	networkMode := rc.topLevelRunContext().jobNetworkName
	if rc.IsHostEnv() {
		networkMode = "default"
	}
	return &container.NewContainerInput{
		Cmd:           cmd,
		Entrypoint:    entrypoint,
		WorkingDir:    rc.JobContainer.ToContainerPath(rc.Config.Workdir),
		Image:         image,
		Name:          createContainerName(rc.jobContainerName(), "STEP-"+step.getStepModel().ID),
		Env:           envList,
		Mounts:        mounts,
		NetworkMode:   networkMode,
		Binds:         binds,
		Stdout:        logWriter,
		Stderr:        logWriter,
		Privileged:    rc.Config.Privileged,
		UsernsMode:    rc.Config.UsernsMode,
		Platform:      rc.Config.ContainerArchitecture,
		RunnerOptions: runnerOptions,
		AutoRemove:    true,
		ValidVolumes:  rc.validVolumes(),
		AllocatePTY:   rc.Config.AllocatePTY,
	}
}

func populateEnvsFromSavedState(env *map[string]string, step actionStep, rc *RunContext) {
	for name, value := range rc.IntraActionState[step.getStepModel().ID] {
		(*env)["STATE_"+name] = value
	}
}

func inputEnvKey(inputID string) string {
	return "INPUT_" + inputEnvPattern.ReplaceAllString(strings.ToUpper(inputID), "_")
}

func (rc *RunContext) resolveActionInputs(ctx context.Context, step step, action *model.Action) (map[string]string, error) {
	provided := step.getStepModel().With
	inputs := mergeMaps(provided)
	var evaluator *expressionEvaluator
	eval := expreval.New(func(expression string, statusCheck exprparser.DefaultStatusCheck) (any, error) {
		if evaluator == nil {
			evaluator = &expressionEvaluator{interpreter: exprparser.NewInterpeter(
				rc.actionDefaultEnvironment(ctx, getStepGithubContext(ctx, step), rc.getJobContext()), exprparser.Config{Context: "step"})}
		}
		return evaluator.evaluate(ctx, expression, statusCheck)
	})
	for name, input := range action.Inputs {
		if _, ok := common.LookupFold(provided, name, true); !ok {
			var err error
			if inputs[name], err = eval.Interpolate(input.Default); err != nil {
				return nil, fmt.Errorf("unable to interpolate the default of input %s: %w", name, err)
			}
		}
	}
	return inputs, nil
}

func applyActionInputs(step step, inputs map[string]string) {
	mergeIntoMap(step, step.getEnv(), (&model.Step{With: inputs}).GetEnv())
}

func stepInput(step *model.Step, name string) string {
	value, _ := common.LookupFold(step.With, name, true)
	return value
}

func warnUnexpectedInputs(ctx context.Context, action *model.Action, inputs map[string]string) {
	docker := action.Runs.Using.IsDocker()
	var unexpectedInputs []string
	for input := range inputs {
		_, declared := common.LookupFold(action.Inputs, input, true)
		containerInput := docker && (strings.EqualFold(input, "entryPoint") || strings.EqualFold(input, "args"))
		if !declared && !containerInput {
			unexpectedInputs = append(unexpectedInputs, input)
		}
	}
	if len(unexpectedInputs) == 0 {
		return
	}
	validInputs := slices.Sorted(maps.Keys(action.Inputs))
	if docker {
		validInputs = append([]string{"entryPoint", "args"}, validInputs...)
	}
	slices.Sort(unexpectedInputs)
	common.Logger(ctx).With(rawOutputField, true).Warnf("##[warning]%s", EscapeCommandData(fmt.Sprintf(
		"Unexpected input(s) '%s', valid inputs are ['%s']", strings.Join(unexpectedInputs, "', '"), strings.Join(validInputs, "', '"))))
}

func getContainerActionPaths(step actionStep) (string, string) {
	rc := step.getRunContext()
	if sar, ok := step.(*stepActionRemote); ok { // a copy per action, as its repository's directory is shared
		actionName := path.Join(sar.containerKey(), sar.remoteAction.Path)
		return actionName, rc.JobContainer.GetActPath() + "/actions/" + actionName
	}
	actionName := getOsSafeRelativePath(filepath.Join(rc.Config.Workdir, step.getStepModel().Uses), rc.Config.Workdir)
	return "./" + actionName, rc.JobContainer.ToContainerPath(rc.Config.Workdir) + "/" + actionName
}

func getOsSafeRelativePath(s, prefix string) string {
	actionName := strings.TrimPrefix(s, prefix)
	if runtime.GOOS == "windows" {
		actionName = strings.ReplaceAll(actionName, "\\", "/")
	}
	actionName = strings.TrimPrefix(actionName, "/")

	return actionName
}

func shouldRunPreStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		log := common.Logger(ctx)

		if step.getActionModel() == nil {
			log.Debugf("skip pre step for '%s': no action model available", step.getStepModel())
			return false
		}

		return true
	}
}

func hasPreStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		runs := &step.getActionModel().Runs
		return (runs.Using.IsComposite() && slices.ContainsFunc(runs.Steps, func(child model.Step) bool { return child.Uses != "" })) || // pre evaluates with before preceding steps produce outputs
			(runs.Using.IsDocker() && runs.PreEntrypoint != "") ||
			((runs.Using.IsNode() || runs.Using == model.ActionRunsUsingGo) && runs.Pre != "")
	}
}

func actionSourcePaths(step actionStep) (actionDir, actionPath string) {
	if sar, ok := step.(*stepActionRemote); ok {
		return sar.actionDir, sar.remoteAction.Path
	} else {
		return filepath.Join(step.getRunContext().Config.Workdir, step.getStepModel().Uses), ""
	}
}

func shouldRunPostStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		if remote, ok := step.(*stepActionRemote); ok && remote.preStepStarted && !remote.action.Runs.Using.IsComposite() {
			return true
		}
		log := common.Logger(ctx)
		stepResults := step.getRunContext().StepResults
		stepResult := stepResults[step.getStepModel().ID]

		if stepResult == nil {
			log.With("stepResult", model.StepStatusSkipped).Debugf("skipping post step for '%s'; step was not executed", step.getStepModel())
			return false
		}

		if stepResult.Conclusion == model.StepStatusSkipped {
			log.With("stepResult", model.StepStatusSkipped).Debugf("skipping post step for '%s'; main step was skipped", step.getStepModel())
			return false
		}

		if step.getActionModel() == nil {
			log.With("stepResult", model.StepStatusSkipped).Debugf("skipping post step for '%s': no action model available", step.getStepModel())
			return false
		}

		return true
	}
}

func hasPostStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		runs := &step.getActionModel().Runs
		return runs.Using.IsComposite() || (runs.Using.IsDocker() && runs.PostEntrypoint != "") ||
			((runs.Using.IsNode() || runs.Using == model.ActionRunsUsingGo) && runs.Post != "")
	}
}

func runActionHook(step actionStep, stage stepStage) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		logger.Debugf("run %s step for '%s'", strings.ToLower(stage.String()), step.getStepModel())

		if !step.getActionModel().Runs.Using.IsComposite() {
			if stage == stepStagePost {
				logger.With(rawOutputField, true).Info("Post job cleanup.")
			}
			return runActionStage(ctx, step, stage)
		}

		if stage == stepStagePre {
			rc := step.getRunContext()
			printRunActionHeader(ctx, step.getStepModel(), step.getActionModel(), *step.getEnv(), rc)
			inputs, err := rc.resolveActionInputs(ctx, step, step.getActionModel())
			if err != nil {
				return err
			}
			step.getCompositeRunContext(ctx, inputs)
			if pre := step.getCompositeSteps().pre; pre != nil {
				return pre(ctx)
			}
		} else {
			actionDir, actionPath := actionSourcePaths(step)
			_, containerActionDir := getContainerActionPaths(step)
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}
			if steps := step.getCompositeSteps(); steps != nil && steps.post != nil {
				return steps.post(ctx)
			}
		}
		return errors.New("missing steps in composite action")
	}
}
