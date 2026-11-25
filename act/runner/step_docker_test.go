// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"errors"
	"slices"
	"testing"

	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func TestStepDockerMain(t *testing.T) {
	cm := &containerMock{}

	var input *container.NewContainerInput

	origContainerNewContainer := ContainerNewContainer
	ContainerNewContainer = func(containerInput *container.NewContainerInput) container.ExecutionsEnvironment {
		input = containerInput
		return cm
	}
	defer func() {
		ContainerNewContainer = origContainerNewContainer
	}()

	ctx := t.Context()

	sd := &stepDocker{
		RunContext: &RunContext{
			StepResults: map[string]*model.StepResult{},
			Config: &Config{
				PresetGitHubContext: &model.GithubContext{},
				Secrets: map[string]string{
					"DOCKER_USERNAME": "docker-user",
					"DOCKER_PASSWORD": "docker-password",
				},
			},
			Run:          &model.Run{JobID: "1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"1": {}}}},
			JobContainer: cm,
			Env:          map[string]string{"GITHUB_ACTION_REPOSITORY": "other/composite", "GITHUB_ACTION_REF": "v1"},
		},
		Step: &model.Step{
			ID:   "1",
			Uses: "docker://node:14",
			If:   yaml.Node{Value: "github.action_repository == 'other/composite' && github.action_ref == 'v1'"},
		},
	}
	require.NoError(t, sd.Step.RawWith.Encode(map[string]string{"args": "identity:${{ github.action_repository }}:${{ github.action_ref }}"}))
	sd.RunContext.ExprEval = sd.RunContext.NewExpressionEvaluator(ctx)

	cm.On("Pull", false).Return(noopExecutor)

	cm.On("Remove").Return(noopExecutor)

	cm.On("Create", []string(nil), []string(nil)).Return(noopExecutor)

	cm.On("Start", true).Return(noopExecutor)

	cm.On("Close").Return(noopExecutor)

	expectStepFileCommands(t, cm)
	require.NoError(t, sd.main()(ctx))

	assert.Equal(t, "node:14", input.Image)
	assert.Equal(t, []string{"identity::"}, input.Cmd)
	assert.Contains(t, input.Env, "GITHUB_ACTION_REPOSITORY=")
	assert.Contains(t, input.Env, "GITHUB_ACTION_REF=")
	assert.Equal(t, "other/composite", sd.RunContext.Env["GITHUB_ACTION_REPOSITORY"])
	assert.Equal(t, "v1", sd.RunContext.Env["GITHUB_ACTION_REF"])

	assert.Empty(t, input.Username)
	assert.Empty(t, input.Password)
	assert.True(t, input.AutoRemove)

	cm.AssertExpectations(t)
	ContainerNewContainer = func(_input *container.NewContainerInput) container.ExecutionsEnvironment { return nil }
	require.ErrorIs(t, sd.runUsesContainer(ctx), errors.ErrUnsupported)
}

func TestStepDockerNewStepContainer(t *testing.T) {
	inheritedPath := (&container.LinuxContainerEnvironmentExtensions{}).DefaultPathVariable()
	for _, test := range []struct {
		name, platform, path, network string
		allocPTY, keepsPath           bool
	}{
		{name: "docker mode attaches to job container network", platform: "node:14", path: inheritedPath, network: "job-network", allocPTY: true, keepsPath: true},
		{name: "host mode uses default network and the image's PATH", platform: "-self-hosted", path: inheritedPath, network: "default"},
		{name: "host mode keeps a PATH that is not the runner's", platform: "-self-hosted", path: "/custom/bin", network: "default", keepsPath: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := &RunContext{
				Config:         &Config{AllocatePTY: test.allocPTY},
				Run:            &model.Run{Workflow: &model.Workflow{}},
				jobNetworkName: "job-network",
			}
			step := &stepDocker{
				RunContext: &RunContext{Parent: job, Config: job.Config, JobContainer: &containerMock{}, platformImage: test.platform},
				Step:       &model.Step{},
				env:        map[string]string{"PATH": test.path},
			}
			input := stepContainerInput(t.Context(), step, "alpine", nil, nil, "")
			assert.Equal(t, test.network, input.NetworkMode)
			assert.Equal(t, test.allocPTY, input.AllocatePTY)
			assert.Equal(t, test.keepsPath, slices.Contains(input.Env, "PATH="+test.path))
		})
	}
}

func TestWindowsHostContainerActions(t *testing.T) {
	for _, test := range []struct {
		name, hostOS string
		jobContainer container.ExecutionsEnvironment
		wantError    bool
	}{
		{name: "Windows host", hostOS: "windows", jobContainer: &container.HostEnvironment{}, wantError: true},
		{name: "Linux host", hostOS: "linux", jobContainer: &container.HostEnvironment{}},
		{name: "macOS host", hostOS: "darwin", jobContainer: &container.HostEnvironment{}},
		{name: "Windows with Linux job container", hostOS: "windows", jobContainer: &containerMock{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := hostOS
			hostOS = test.hostOS
			t.Cleanup(func() { hostOS = original })
			rc := &RunContext{JobContainer: test.jobContainer, kubernetes: true}
			wantError := errKubernetesDocker.Error()
			if test.wantError {
				wantError = errWindowsHostContainer.Error()
			}
			t.Run("docker step", func(t *testing.T) {
				require.EqualError(t, (&stepDocker{RunContext: rc}).runUsesContainer(t.Context()), wantError)
			})
			t.Run("container action", func(t *testing.T) {
				require.EqualError(t, execAsDocker(t.Context(), &stepActionRemote{RunContext: rc}, stepStageMain, nil, "", ""), wantError)
			})
		})
	}
}
