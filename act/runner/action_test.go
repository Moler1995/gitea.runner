// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/lock"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type closerFunc func()

func (close closerFunc) Close() error {
	close()
	return nil
}

func TestActionReader(t *testing.T) {
	yaml := strings.ReplaceAll(`
name: 'name'
runs:
	using: 'node16'
	main: 'main.js'
`, "\t", "  ")
	yamlAction := &model.Action{
		Name: "name",
		Runs: model.ActionRuns{
			Using:  "node16",
			Main:   "main.js",
			PreIf:  "always()",
			PostIf: "always()",
		},
	}

	type readerCase struct {
		name        string
		step        model.Step
		filename    string
		fileContent string
		expected    *model.Action
	}
	table := []readerCase{
		{
			name: "readWithArgs",
			step: model.Step{With: map[string]string{"args": "cmd"}},
			expected: &model.Action{
				Name: "(Synthetic)",
				Inputs: map[string]model.Input{
					"args": {},
					"cwd": {
						Description: "(Actual working directory)",
						Default:     filepath.Join("actionDir", "actionPath"),
					},
					"command": {
						Description: "(Actual program)",
						Default:     "cmd",
					},
				},
				Runs: model.ActionRuns{
					Using: "node12",
					Main:  "trampoline.js",
				},
			},
		},
		{
			name:        "invalid manifest",
			filename:    "action.yaml",
			fileContent: "runs: {using: node99}\n",
		},
	}

	for _, filename := range []string{"action.yml", "action.yaml"} {
		table = append(table, readerCase{name: filename, filename: filename, fileContent: yaml, expected: yamlAction})
	}
	for _, filename := range []string{"Dockerfile", "dockerfile"} {
		table = append(table, readerCase{name: filename, filename: filename, expected: &model.Action{
			Name: "(Synthetic)", Runs: model.ActionRuns{Using: "docker", Image: filename},
		}})
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			closed := false

			readFile := func(filename string) (io.Reader, io.Closer, error) {
				if tt.filename != filename {
					return nil, nil, fs.ErrNotExist
				}

				return strings.NewReader(tt.fileContent), closerFunc(func() { closed = true }), nil
			}

			writeFile := func(filename string, _data []byte, perm fs.FileMode) error {
				assert.Equal(t, filepath.Join("actionDir", "actionPath", "trampoline.js"), filename)
				assert.Equal(t, fs.FileMode(0o644), perm)
				return nil
			}

			action, err := readActionImpl(t.Context(), &tt.step, "actionDir", "actionPath", readFile, writeFile)

			assert.Equal(t, tt.filename != "", closed)
			if tt.expected == nil {
				require.ErrorContains(t, err, "Failed to load "+filepath.Join("actionDir", "actionPath", tt.filename))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, action)
		})
	}
}

func TestActionRunner(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tt := range []struct {
		name        string
		inputs      map[string]model.Input
		with        map[string]string
		inherited   map[string]string
		state       map[string]string
		expectedEnv map[string]string
		warning     string
	}{
		{
			name:        "input precedence and saved state across stages",
			inputs:      map[string]model.Input{"foo": {Default: "default"}, "key": {Default: "${{ fromJSON('') }}"}},
			with:        map[string]string{"KEY": "provided"},
			inherited:   map[string]string{"INPUT_FOO": "inherited", "INPUT_KEY": "inherited"},
			state:       map[string]string{"name": "state value"},
			expectedEnv: map[string]string{"INPUT_FOO": "default", "INPUT_KEY": "provided", "STATE_name": "state value"},
		},
		{
			name:    "unexpected inputs warn once across stages",
			inputs:  map[string]model.Input{"key": {}},
			with:    map[string]string{"KEY": "provided value", "UNKNOWN": "value", "other%0A\r\n": "value"},
			warning: "##[warning]Unexpected input(s) 'UNKNOWN', 'other%250A%0D%0A', valid inputs are ['key']",
		},
		{
			name:    "no declared inputs",
			with:    map[string]string{"unknown": "value"},
			warning: "##[warning]Unexpected input(s) 'unknown', valid inputs are ['']",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hook := logtest.NewWithLevel(logging.LevelWarn)
			ctx := common.WithLogger(t.Context(), logging.New(hook))
			cm := &containerMock{}
			containerActionDir := "/var/run/act/actions/" + model.UsesHash("/org/repo/path@ref") + "/"
			cm.On("CopyDir", containerActionDir, "dir/", false, true).Return(noopExecutor)

			for _, script := range []string{"path/pre.js", "path/main.js", "path/post.js"} {
				cm.On("Exec", nodeActionCommand("node", containerActionDir+script), mock.Anything, "", "").Return(noopExecutor).Once().Run(func(args mock.Arguments) {
					env := args.Get(1).(map[string]string)
					for key, value := range tt.expectedEnv {
						if script == "path/pre.js" && strings.HasPrefix(key, "STATE_") {
							assert.NotContains(t, env, key)
						} else {
							assert.Equal(t, value, env[key], "%s in %s", key, script)
						}
					}
				})
			}

			remoteAction := mustNewRemoteAction(t, "org/repo/path@ref")
			step := &stepActionRemote{
				Step: &model.Step{ID: "step", Uses: "org/repo/path@ref", With: tt.with},
				RunContext: &RunContext{
					Config:           &Config{PresetGitHubContext: &model.GithubContext{}},
					Run:              &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
					IntraActionState: map[string]map[string]string{"step": tt.state},
					JobContainer:     cm,
				},
				action:       &model.Action{Inputs: tt.inputs, Runs: model.ActionRuns{Using: "node16", Pre: "pre.js", Main: "main.js", Post: "post.js"}},
				actionDir:    "dir",
				remoteAction: remoteAction,
			}
			for index, execute := range []common.Executor{runActionHook(step, stepStagePre), runActionImpl(step), runActionHook(step, stepStagePost)} {
				step.env = mergeMaps(tt.inherited)
				require.NoError(t, execute(ctx))
				if index == 0 {
					assert.Empty(t, hook.AllRecords())
				}
			}
			if tt.warning == "" {
				assert.Empty(t, hook.AllRecords())
			} else {
				require.Len(t, hook.AllRecords(), 1)
				assert.Equal(t, tt.warning, hook.LastRecord().Message)
				rawOutput, ok := hook.LastRecord().Attr(rawOutputField)
				require.True(t, ok)
				assert.Equal(t, true, rawOutput)
			}
			cm.AssertExpectations(t)
		})
	}
}

func TestGoActionUsesLinuxExecutablePaths(t *testing.T) {
	actionRoot := t.TempDir()
	for _, stage := range []string{"main", "pre", "post"} {
		t.Run(stage, func(t *testing.T) {
			cm := &containerMock{}
			containerActionDir := "/var/run/act/actions/" + model.UsesHash("/org/repo@ref")
			if stage != "post" {
				cm.On("CopyDir", containerActionDir+"/", mock.Anything, false, true).Return(noopExecutor).Once()
			}
			cm.On("Exec", []string{"go", "build", "-o", stage + ".go.out", stage + ".go"}, mock.Anything, "", containerActionDir).Return(noopExecutor).Once()
			cm.On("Exec", []string{containerActionDir + "/" + stage + ".go.out"}, mock.Anything, "", "").Return(noopExecutor).Once()
			step := &stepActionRemote{
				Step: &model.Step{ID: "step", Uses: "org/repo@ref"},
				RunContext: &RunContext{
					Config:       &Config{NoActionPatch: true, PresetGitHubContext: &model.GithubContext{}},
					Run:          &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
					JobContainer: cm,
				},
				action:       &model.Action{Runs: model.ActionRuns{Using: model.ActionRunsUsingGo, Main: "main.go", Pre: "pre.go", Post: "post.go"}},
				actionDir:    filepath.Join(actionRoot, stage),
				env:          map[string]string{},
				remoteAction: mustNewRemoteAction(t, "org/repo@ref"),
			}
			switch stage {
			case "main":
				require.NoError(t, runActionImpl(step)(t.Context()))
			case "pre":
				require.NoError(t, runActionHook(step, stepStagePre)(t.Context()))
			case "post":
				require.NoError(t, runActionHook(step, stepStagePost)(t.Context()))
			}
			cm.AssertExpectations(t)
		})
	}
}

func TestNodeActionCommandPreservesSymlinkedEntrypoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	requireHostTools(t, "node")

	actionDir := t.TempDir()
	entrypoint := filepath.Join(actionDir, "index.mjs")
	require.NoError(t, os.WriteFile(entrypoint, []byte(`
import {fileURLToPath} from "node:url";
const self = fileURLToPath(import.meta.url);
if (process.argv[1] !== self) {
	console.log("argv[1]:", process.argv[1], "import.meta.url:", self);
	process.exitCode = 1;
}
`), 0o600))

	symlinkedActionDir := filepath.Join(t.TempDir(), "action")
	require.NoError(t, os.Symlink(actionDir, symlinkedActionDir))
	symlinkedEntrypoint := filepath.Join(symlinkedActionDir, "index.mjs")

	args := nodeActionCommand("node", symlinkedEntrypoint)
	output, err := exec.CommandContext(t.Context(), args[0], args[1:]...).CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestMaybeCopyToActionDirHoldsCloneLock(t *testing.T) {
	actionDir := t.TempDir()
	cm := &containerMock{}
	cm.On("CopyDir", "/var/run/act/actions/", actionDir+"/", false, true).Return(func(context.Context) error {
		_, err := lock.TryLock(actionDir)
		assert.ErrorIs(t, err, lock.ErrLocked)
		return nil
	})
	step := &stepActionRemote{Step: &model.Step{Uses: "remote/action@v1"}, RunContext: &RunContext{Config: &Config{}, JobContainer: cm}}

	require.NoError(t, maybeCopyToActionDir(t.Context(), step, actionDir, "", "/var/run/act/actions/"))
	release, err := lock.TryLock(actionDir)
	require.NoError(t, err)
	require.NoError(t, release())
	cm.AssertExpectations(t)
}

func TestExecAsDockerImageChecks(t *testing.T) {
	if container.NewContainer(&container.NewContainerInput{}) == nil {
		t.Skip("Docker backend is disabled")
	}
	originalExists, originalBuild := ContainerImageExistsLocally, ContainerNewDockerBuildExecutor
	t.Cleanup(func() {
		ContainerImageExistsLocally, ContainerNewDockerBuildExecutor = originalExists, originalBuild
	})
	for _, testcase := range []struct {
		name, platform, options, wantPlatform string
		exists, rebuild, wantBuild            bool
	}{
		{name: "matching platform", platform: "linux/arm64", wantPlatform: "linux/arm64", exists: true},
		{name: "runner options override architecture", platform: "linux/arm64", options: "--platform linux/amd64", wantPlatform: "linux/amd64", wantBuild: true},
		{name: "forced rebuild", exists: true, rebuild: true, wantBuild: true},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			actionDir := t.TempDir()
			ContainerImageExistsLocally = func(_ctx context.Context, _image, platform string) (bool, error) {
				assert.Equal(t, testcase.wantPlatform, platform)
				return testcase.exists, nil
			}
			var built bool
			ContainerNewDockerBuildExecutor = func(input container.NewDockerBuildExecutorInput) common.Executor {
				assert.Equal(t, testcase.wantPlatform, input.Platform)
				return func(context.Context) error {
					built = true
					_, err := lock.TryLock(actionDir)
					assert.ErrorIs(t, err, lock.ErrLocked)
					return nil
				}
			}
			step := &stepActionRemote{
				Step: &model.Step{ID: "action"},
				RunContext: &RunContext{
					Config: &Config{
						PresetGitHubContext:   &model.GithubContext{},
						ContainerArchitecture: testcase.platform,
						ContainerOptions:      testcase.options,
						ForceRebuild:          testcase.rebuild,
					},
					Run:          &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {}}}},
					JobContainer: &jobContainerMock{},
				},
				action:       &model.Action{Runs: model.ActionRuns{Using: "docker", Image: "Dockerfile"}},
				actionDir:    actionDir,
				remoteAction: mustNewRemoteAction(t, "remote/action@v1"),
				env:          map[string]string{},
			}
			require.NoError(t, runActionImpl(step)(common.WithDryrun(t.Context(), true)))
			assert.Equal(t, testcase.wantBuild, built)
		})
	}
}

func TestDockerActionImageTag(t *testing.T) {
	assert.Equal(t,
		"act-abc123-dockeraction:latest",
		dockerActionImageTag("owner/repo", "abc123", false),
	)

	assert.Equal(t,
		"act-owner-repo-baca2daaa2fe-dockeraction:latest",
		dockerActionImageTag("owner/repo", "./", true),
	)
	assert.Equal(t,
		"act-owner-repo-sub-e847b61255a8-dockeraction:latest",
		dockerActionImageTag("owner/repo", "./sub", true),
	)

	collisions := [][2]struct {
		repoName   string
		actionName string
	}{
		{{"a/b-c", "./"}, {"a-b/c", "./"}},
		{{"owner/repo-a", "./"}, {"owner/repo", "./a"}},
	}
	for _, c := range collisions {
		assert.NotEqual(t,
			dockerActionImageTag(c[0].repoName, c[0].actionName, true),
			dockerActionImageTag(c[1].repoName, c[1].actionName, true),
			"local docker action tags must differ for %q/%q vs %q/%q",
			c[0].repoName, c[0].actionName, c[1].repoName, c[1].actionName,
		)
	}

	assert.NotEqual(t,
		dockerActionImageTag("owner/repo", "./", true),
		dockerActionImageTag("owner/repo", "./sub", true),
	)

	workflow := &model.Workflow{Jobs: map[string]*model.Job{"job1": {}}}
	job := newTestRC(workflow, nil)
	step := &stepActionRemote{Step: &model.Step{}, RunContext: job}
	image := job.dockerActionImage(t.Context(), step, "abc123", false)
	assert.True(t, strings.HasPrefix(image, "act-abc123-dockeraction:latest-"))
	assert.Equal(t, image, (&RunContext{Parent: job}).dockerActionImage(t.Context(), step, "abc123", false))
	assert.NotEqual(t, image, job.dockerActionImage(t.Context(), &stepActionRemote{Step: &model.Step{}, RunContext: job}, "abc123", false))
	assert.NotEqual(t, image, newTestRC(workflow, nil).dockerActionImage(t.Context(), step, "abc123", false))
	job.dockerActionImage(common.WithDryrun(t.Context(), true), &stepActionRemote{Step: &model.Step{}, RunContext: job}, "abc123", false)
	assert.Len(t, job.dockerActionImages, 2)
}

func TestExecAsDockerStageEntrypoint(t *testing.T) {
	orig := ContainerNewContainer
	defer func() { ContainerNewContainer = orig }()
	fakeDockerDaemon(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"Architecture":"x86_64"}`)
	})

	for _, tc := range []struct {
		name           string
		stage          stepStage
		with           map[string]string
		inputs         map[string]model.Input
		runs           model.ActionRuns
		env            map[string]string
		wantCmd        []string
		wantEntrypoint []string
		warning        string
	}{
		{
			name:  "main stage prefers manifest values",
			stage: stepStageMain,
			with:  map[string]string{"args": "caller", "entrypoint": "input.sh"},
			runs: model.ActionRuns{
				Entrypoint: "main.sh",
				Args:       []string{"manifest"},
				Env:        map[string]string{"ACTION_ONLY": "manifest"},
			},
			wantCmd:        []string{"manifest"},
			wantEntrypoint: []string{"main.sh"},
		},
		{
			name:           "main stage uses caller fallbacks in any case and warns about undeclared inputs",
			stage:          stepStageMain,
			with:           map[string]string{"ARGS": "caller --flag", "entryPoint": "input.sh --verbose", "unknown": "value"},
			inputs:         map[string]model.Input{"message": {}},
			runs:           model.ActionRuns{Env: map[string]string{"ACTION_ONLY": "manifest"}},
			wantCmd:        []string{"caller", "--flag"},
			wantEntrypoint: []string{"input.sh", "--verbose"},
			warning:        "##[warning]Unexpected input(s) 'unknown', valid inputs are ['entryPoint', 'args', 'message']",
		},
		{
			name:    "manifest args preserve quoted and explicitly empty caller inputs",
			stage:   stepStageMain,
			with:    map[string]string{"args": "it's literal", "MESSAGE": ""},
			inputs:  map[string]model.Input{"message": {Default: "${{ fromJSON('') }}"}},
			runs:    model.ActionRuns{Args: []string{"${{ inputs.args }}", "${{ inputs.message }}"}},
			wantCmd: []string{"it's literal", ""},
		},
		{
			name:    "explicit empty manifest args suppress caller args",
			stage:   stepStageMain,
			with:    map[string]string{"args": "it's ignored"},
			runs:    model.ActionRuns{Args: []string{}},
			wantCmd: []string{},
		},
		{
			name:  "pre stage keeps step environment",
			stage: stepStagePre,
			with:  map[string]string{"entrypoint": "input.sh"},
			runs: model.ActionRuns{
				PreEntrypoint: "pre.sh --verbose",
				Args:          []string{"hello"},
				Env:           map[string]string{"SHARED": "manifest"},
			},
			env:            map[string]string{"SHARED": "step"},
			wantCmd:        []string{"hello"},
			wantEntrypoint: []string{"pre.sh", "--verbose"},
		},
		{
			name:           "post stage uses manifest entrypoint",
			stage:          stepStagePost,
			inputs:         map[string]model.Input{"message": {Default: "hello"}},
			runs:           model.ActionRuns{PostEntrypoint: "post.sh", Args: []string{"${{ github.job || inputs.message }}"}},
			wantCmd:        []string{"hello"},
			wantEntrypoint: []string{"post.sh"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := logtest.NewWithLevel(logging.LevelWarn)
			ctx := common.WithLogger(t.Context(), logging.New(hook))
			tc.runs.Using, tc.runs.Image = "docker", "docker://node:14"
			var input *container.NewContainerInput
			ContainerNewContainer = func(in *container.NewContainerInput) container.ExecutionsEnvironment {
				input = in
				return fakeContainer{}
			}

			step := &stepActionRemote{
				Step: &model.Step{ID: "1", Uses: "org/action@v1", With: tc.with},
				RunContext: &RunContext{
					Config:       &Config{PresetGitHubContext: &model.GithubContext{}},
					Run:          &model.Run{JobID: "1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}}},
					JobContainer: &jobContainerMock{},
				},
				action:       &model.Action{Inputs: tc.inputs, Runs: tc.runs},
				env:          mergeMaps(tc.env),
				remoteAction: mustNewRemoteAction(t, "org/action@v1"),
			}

			switch tc.stage {
			case stepStageMain:
				require.NoError(t, runActionImpl(step)(ctx))
			case stepStagePre:
				require.NoError(t, runActionHook(step, stepStagePre)(ctx))
			case stepStagePost:
				require.NoError(t, runActionHook(step, stepStagePost)(ctx))
			}
			if tc.warning == "" {
				assert.Empty(t, hook.AllRecords())
			} else {
				require.Len(t, hook.AllRecords(), 1)
				assert.Equal(t, tc.warning, hook.LastRecord().Message)
			}
			require.NotNil(t, input)
			assert.Equal(t, tc.wantCmd, input.Cmd)
			assert.Equal(t, tc.wantEntrypoint, input.Entrypoint)
			for key, value := range mergeMaps(tc.runs.Env, tc.env) {
				assert.Contains(t, input.Env, key+"="+value)
			}
		})
	}
}

func TestActionHasPreAndPostStep(t *testing.T) {
	newStep := func(runs model.ActionRuns) actionStep {
		return &stepActionRemote{action: &model.Action{Runs: runs}}
	}
	ctx := t.Context()

	assert.False(t, hasPreStep(newStep(model.ActionRuns{Using: "composite", Steps: []model.Step{{Run: "true"}}}))(ctx))
	assert.True(t, hasPreStep(newStep(model.ActionRuns{Using: "composite", Steps: []model.Step{{Run: "true"}, {Uses: "actions/cache@v4"}}}))(ctx))

	assert.False(t, hasPreStep(newStep(model.ActionRuns{Using: "docker", Image: "Dockerfile"}))(ctx))
	assert.False(t, hasPostStep(newStep(model.ActionRuns{Using: "docker", Image: "Dockerfile"}))(ctx))

	withStages := model.ActionRuns{Using: "docker", Image: "Dockerfile", PreEntrypoint: "pre.sh", PostEntrypoint: "post.sh"}
	assert.True(t, hasPreStep(newStep(withStages))(ctx))
	assert.True(t, hasPostStep(newStep(withStages))(ctx))
}
