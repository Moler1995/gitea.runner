// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	"github.com/kballard/go-shellquote"
)

func (rc *RunContext) IsContainerHooks() bool {
	return !rc.kubernetes && !rc.IsHostEnv() && rc.Config.ContainerHooks.Enabled
}

func (rc *RunContext) startHookJob() common.Executor {
	return func(ctx context.Context) error {
		spec := rc.containerSpec
		spec.Image = rc.platformImage
		jobContainer, err := rc.hookContainer(spec)
		if err != nil {
			return err
		}
		var services map[string]*model.ContainerSpec
		if err := model.DecodeEvaluated("job services", rc.Run.Job().RawServices, rc.NewExpressionEvaluatorWithEnv(ctx, mergeMaps(rc.GetEnv(), rc.jobEnv)).shared(ctx).EvaluateYamlNode, &services); err != nil {
			return err
		}
		job := container.HookJob{
			Name: rc.jobContainerName(), Container: jobContainer, Services: map[string]container.HookContainer{},
			Paths: container.HookPaths{Workspace: (&container.LinuxContainerEnvironmentExtensions{}).ToContainerPath(rc.Config.Workdir), Act: "/var/run/act", Temp: "/tmp", ToolCache: rc.toolCache(container.DefaultToolCache)},
			Policy: container.HookPolicy{
				Privileged: rc.Config.Privileged, UsernsMode: rc.Config.UsernsMode, Network: string(rc.Config.ContainerNetworkMode),
				Options: rc.Config.ContainerOptions, ValidVolumes: rc.Config.ValidVolumes, Volumes: rc.Config.ContainerVolumes,
				CapAdd: rc.Config.ContainerCapAdd, CapDrop: rc.Config.ContainerCapDrop, Platform: rc.Config.ContainerArchitecture,
				ForcePull: rc.Config.ForcePull, ForceRebuild: rc.Config.ForceRebuild,
			},
		}
		for name, spec := range services {
			if spec == nil {
				return fmt.Errorf("service %s has no container definition", name)
			}
			if spec.Image == "" {
				continue
			}
			job.Services[name], err = rc.hookContainer(*spec)
			if err != nil {
				return fmt.Errorf("service %s: %w", name, err)
			}
		}
		github := rc.getGithubContext(ctx)
		job.Context = map[string]string{"repository": github.Repository, "ref": github.Ref, "sha": github.Sha, "actor": github.Actor, "eventName": github.EventName, "runId": github.RunID, "job": rc.Run.JobID, "workflow": rc.Run.Workflow.Name}
		logWriter := rc.commandLogWriter(ctx)
		environment, err := container.NewHookEnvironment(rc.Config.ContainerHooks, job, rc.Config.Workdir, logWriter, logWriter)
		if err != nil {
			return err
		}
		rc.JobContainer = environment
		rc.cleanUpJobContainer = environment.Remove()
		if err := environment.Prepare(ctx); err != nil {
			return err
		}
		prepared := environment.Prepared()
		rc.jobContainerID, rc.jobNetworkName = prepared.Container.ID, prepared.Container.Network
		for name, info := range prepared.Services {
			rc.serviceContainers = append(rc.serviceContainers, &serviceContainer{name: name, info: &container.Info{ID: info.ID, Ports: info.Ports}})
		}
		rc.setRunnerContextEnv(ctx)
		return rc.copyWorkflowFiles()(ctx)
	}
}

func (rc *RunContext) hookContainer(spec model.ContainerSpec) (container.HookContainer, error) {
	username, password, err := registryCredentials(spec.Credentials, "")
	if err != nil {
		return container.HookContainer{}, err
	}
	args := spec.Cmd
	if len(args) == 0 && spec.Command != "" {
		args, err = shellquote.Split(spec.Command)
		if err != nil {
			return container.HookContainer{}, err
		}
	}
	var entrypoint []string
	if spec.Entrypoint != "" {
		entrypoint = []string{spec.Entrypoint}
	}
	return container.HookContainer{Image: strings.TrimPrefix(spec.Image, "docker://"), EntryPoint: entrypoint, EntryPointArgs: args, EnvironmentVariables: mergeMaps(rc.Config.ProxyEnv, spec.Env), Volumes: spec.Volumes, Ports: spec.Ports, Options: spec.Options, Username: username, Password: password}, nil
}

func runHookContainerAction(ctx context.Context, environment *container.HookEnvironment, step actionStep, actionDir, basedir string, localAction bool, stage stepStage) error {
	command, entrypoint, err := dockerActionCommand(ctx, step, stage)
	if err != nil {
		return err
	}
	rc := step.getRunContext()
	exec := container.HookCommand{EntryPointArgs: command, WorkingDirectory: rc.githubWorkspace(), EnvironmentVariables: maps.Clone(*step.getEnv())}
	input := container.HookContainerStep{HookCommand: exec}
	if len(entrypoint) > 0 {
		input.EntryPoint = entrypoint[0]
		input.EntryPointArgs = slices.Concat(entrypoint[1:], command)
	}
	image := step.getActionModel().Runs.Image
	contextDir := ""
	if after, ok := strings.CutPrefix(image, "docker://"); ok {
		input.Image = after
	} else {
		if !localAction {
			unlock, err := git.AcquireCloneLock(ctx, actionDir)
			if err != nil {
				return err
			}
			defer unlock()
		}
		contextDir, input.Dockerfile = filepath.Split(filepath.Join(basedir, image))
		if localAction {
			contextDir, input.Dockerfile = path.Split(path.Join(basedir, image))
		}
		input.BuildArgs = maps.Clone(rc.Config.ProxyEnv)
	}
	return environment.RunContainerStep(ctx, input, contextDir, localAction)
}
