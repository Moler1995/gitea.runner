// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import "encoding/json/jsontext"

// HookConfig selects an operator-owned container hook program.
type HookConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Entrypoint string `yaml:"entrypoint"`
	Cmd        string `yaml:"cmd"`
}

// HookPaths are absolute Linux paths in the job environment.
type HookPaths struct {
	Workspace string `json:"workspace"`
	Act       string `json:"act"`
	Temp      string `json:"temp"`
	ToolCache string `json:"toolCache"`
}

// HookContainer describes a workload; credentials are referenced through the hook's environment.
type HookContainer struct {
	Image                string                       `json:"image"`
	EntryPoint           []string                     `json:"entryPoint,omitempty"`
	EntryPointArgs       []string                     `json:"entryPointArgs,omitempty"`
	EnvironmentVariables map[string]string            `json:"environmentVariables,omitempty"`
	Volumes              []string                     `json:"volumes,omitempty"`
	Ports                []string                     `json:"ports,omitempty"`
	Options              string                       `json:"options,omitempty"`
	Registry             map[string]map[string]string `json:"registry,omitempty"`
	Username             string                       `json:"-"`
	Password             string                       `json:"-"`
}

// HookPolicy keeps operator settings separate from workflow requests.
type HookPolicy struct {
	Privileged   bool     `json:"privileged"`
	UsernsMode   string   `json:"usernsMode,omitempty"`
	Network      string   `json:"network,omitempty"`
	Options      string   `json:"options,omitempty"`
	ValidVolumes []string `json:"validVolumes,omitempty"`
	Volumes      []string `json:"volumes,omitempty"`
	CapAdd       []string `json:"capAdd,omitempty"`
	CapDrop      []string `json:"capDrop,omitempty"`
	Platform     string   `json:"platform,omitempty"`
	ForcePull    bool     `json:"forcePull"`
	ForceRebuild bool     `json:"forceRebuild"`
}

// HookJob is the prepare_job payload.
type HookJob struct {
	Name      string                   `json:"name"`
	Container HookContainer            `json:"container"`
	Services  map[string]HookContainer `json:"services,omitempty"`
	Paths     HookPaths                `json:"paths"`
	Context   map[string]string        `json:"context,omitempty"`
	Policy    HookPolicy               `json:"policy"`
}

// HookContainerInfo describes a ready workload returned by prepare_job.
type HookContainerInfo struct {
	ID      string            `json:"id"`
	Network string            `json:"network,omitempty"`
	Ports   map[string]string `json:"ports,omitempty"`
}

// HookPrepared is the result of a successful prepare_job.
type HookPrepared struct {
	Paths  HookPaths `json:"paths"`
	Runner struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
	} `json:"runner"`
	ImageEnvironment map[string]string            `json:"imageEnvironment,omitempty"`
	Container        HookContainerInfo            `json:"container"`
	Services         map[string]HookContainerInfo `json:"services,omitempty"`
}

// HookCommand is the run_command_step payload, or part of run_container_step.
type HookCommand struct {
	EntryPoint           string            `json:"entryPoint"`
	EntryPointArgs       []string          `json:"entryPointArgs,omitempty"`
	EnvironmentVariables map[string]string `json:"environmentVariables,omitempty"`
	WorkingDirectory     string            `json:"workingDirectory"`
	User                 string            `json:"user,omitempty"`
}

// HookContainerStep runs an image or builds and runs an action's Dockerfile.
type HookContainerStep struct {
	HookCommand
	Image               string            `json:"image,omitempty"`
	Dockerfile          string            `json:"dockerfile,omitempty"`
	BuildContextArchive string            `json:"buildContextArchive,omitempty"`
	BuildArgs           map[string]string `json:"buildArgs,omitempty"`
}

type hookRequest struct {
	ProtocolVersion int            `json:"protocolVersion"`
	RequestID       string         `json:"requestId"`
	JobID           string         `json:"jobId"`
	Command         string         `json:"command"`
	ResponseFile    string         `json:"responseFile"`
	State           jsontext.Value `json:"state"`
	Args            any            `json:"args"`
}

type hookResponse struct {
	ProtocolVersion int            `json:"protocolVersion"`
	RequestID       string         `json:"requestId"`
	State           jsontext.Value `json:"state,omitempty"`
	Result          jsontext.Value `json:"result,omitempty"`
	Error           *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
