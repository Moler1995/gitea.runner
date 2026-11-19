// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/internal/pkg/process"
)

const hookResponseLimit = 1 << 20

var errHookSourceNotFound = errors.New("container hook source not found")

// HookEnvironment delegates container operations without a shared runner/job filesystem.
type HookEnvironment struct {
	LinuxContainerEnvironmentExtensions
	config            HookConfig
	job               HookJob
	workdir, root, id string
	state             jsontext.Value
	secrets           []string
	prepared          HookPrepared
	stdout, stderr    io.Writer
	mu                sync.Mutex
	cleanup           sync.Once
	cleanupErr        error
}

var _ ExecutionsEnvironment = (*HookEnvironment)(nil)

func hookLocalError(message string, err error) error {
	if err == nil {
		return nil
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return cause
		}
	}
	logging.Errorf("%s: %v", message, err) // Host paths belong in operator logs.
	return errors.New(message)
}

// NewHookEnvironment validates the selected hook and allocates its private state directory.
func NewHookEnvironment(config HookConfig, job HookJob, workdir string, stdout, stderr io.Writer) (*HookEnvironment, error) {
	if !filepath.IsAbs(config.Cmd) {
		return nil, errors.New("container.hooks.cmd must be an absolute path")
	}
	info, err := os.Stat(config.Cmd)
	if err != nil {
		return nil, hookLocalError("container.hooks.cmd cannot be read", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("container.hooks.cmd must name a regular file")
	}
	root, err := os.MkdirTemp("", "gitea-container-hooks-")
	if err != nil {
		return nil, hookLocalError("container hook private directory creation failed", err)
	}
	e := &HookEnvironment{config: config, job: job, workdir: workdir, root: root, id: rand.Text(), state: jsontext.Value(`{}`), stdout: stdout, stderr: stderr}
	e.job.Services = maps.Clone(job.Services)
	e.prepared.Paths = job.Paths
	e.addRegistrySecrets(&e.job.Container, "JOB")
	for index, name := range slices.Sorted(maps.Keys(e.job.Services)) {
		spec := e.job.Services[name]
		e.addRegistrySecrets(&spec, fmt.Sprintf("SERVICE_%d", index))
		e.job.Services[name] = spec
	}
	return e, nil
}

func (e *HookEnvironment) addRegistrySecrets(spec *HookContainer, prefix string) {
	if spec.Username == "" && spec.Password == "" {
		return
	}
	spec.Registry = map[string]map[string]string{}
	for name, value := range map[string]string{"username": spec.Username, "password": spec.Password} {
		key := "GITEA_CONTAINER_HOOK_SECRET_" + prefix + "_" + strings.ToUpper(name)
		e.secrets = append(e.secrets, key+"="+value)
		spec.Registry[name] = map[string]string{"$env": key}
	}
	spec.Username, spec.Password = "", ""
}

func (e *HookEnvironment) invoke(ctx context.Context, event string, args, result any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	dir, err := os.MkdirTemp(e.root, "request-")
	if err != nil {
		return hookLocalError("container hook request directory creation failed", err)
	}
	defer os.RemoveAll(dir)
	request := hookRequest{ProtocolVersion: 1, RequestID: filepath.Base(dir), JobID: e.id, Command: event, ResponseFile: filepath.Join(dir, "response.json"), State: e.state, Args: args}
	data, err := json.Marshal(request)
	if err != nil {
		return hookLocalError("container hook request encoding failed", err)
	}
	if len(data) > 8<<20 {
		return errors.New("container hook request exceeds 8 MiB")
	}
	program, argv := e.config.Cmd, []string{}
	if e.config.Entrypoint != "" {
		program, argv = e.config.Entrypoint, []string{e.config.Cmd}
	}
	cmd := exec.CommandContext(ctx, program, argv...)
	cmd.Stdin = bytes.NewReader(append(data, '\n'))
	cmd.Env = append(os.Environ(), e.secrets...)
	cmd.Stdout, cmd.Stderr = e.stdout, e.stderr
	cmd.SysProcAttr = process.SysProcAttr("", false)
	tree := process.NewTreeKill(cmd)
	if err := cmd.Start(); err != nil {
		return hookLocalError("container hook "+event+" could not start", err)
	}
	killer, captureErr := tree.Capture(cmd.Process)
	if captureErr != nil {
		logging.Warnf("capturing container hook process tree failed: %v", captureErr)
	}
	defer killer.Close()
	hookErr := errors.Join(tree.Wait(ctx, cmd), ctx.Err())
	common.FlushWriter(e.stdout)
	common.FlushWriter(e.stderr)
	file, readErr := os.Open(request.ResponseFile)
	if readErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(hookErr, hookLocalError("container hook "+event+" response is missing", readErr))
	}
	defer file.Close()
	responseData, readErr := io.ReadAll(io.LimitReader(file, hookResponseLimit+1))
	if readErr != nil {
		return hookLocalError("container hook "+event+" response cannot be read", readErr)
	}
	if len(responseData) > hookResponseLimit {
		return errors.New("container hook response exceeds 1 MiB")
	}
	var response hookResponse
	if err := json.Unmarshal(responseData, &response); err != nil {
		return errors.Join(hookErr, hookLocalError("container hook "+event+" response is invalid", err))
	}
	if response.ProtocolVersion != 1 || response.RequestID != request.RequestID {
		return errors.New("container hook response version or requestId mismatch")
	}
	if len(response.State) != 0 {
		if response.State.Kind() != '{' || len(response.State) > 64<<10 {
			return errors.New("container hook state must be an object of at most 64 KiB")
		}
		e.state = slices.Clone(response.State)
	}
	if response.Error != nil {
		var cause error
		if response.Error.Code == "not_found" {
			cause = errHookSourceNotFound
		}
		failure := fmt.Errorf("container hook %s: %s: %s", event, response.Error.Code, response.Error.Message)
		return errors.Join(failure, hookErr, cause)
	}
	if hookErr != nil {
		return fmt.Errorf("container hook %s: %w", event, hookErr)
	}
	if response.Result.Kind() != '{' {
		return fmt.Errorf("container hook %s must return a result object", event)
	}
	if result != nil {
		return hookLocalError("container hook "+event+" result is invalid", json.Unmarshal(response.Result, result))
	}
	return nil
}

// Prepare creates the runtime and fixes its paths and metadata for the job's lifetime.
func (e *HookEnvironment) Prepare(ctx context.Context) error {
	var prepared HookPrepared
	if err := e.invoke(ctx, "prepare_job", e.job, &prepared); err != nil {
		return err
	}
	for _, value := range []string{prepared.Paths.Workspace, prepared.Paths.Act, prepared.Paths.Temp, prepared.Paths.ToolCache} {
		if !path.IsAbs(value) || strings.ContainsAny(value, "\\\x00\r\n") {
			return errors.New("container hook prepare must return absolute Linux paths")
		}
	}
	if prepared.Runner.OS != "Linux" || prepared.Runner.Arch == "" || prepared.Container.ID == "" {
		return errors.New("container hook prepare must return Linux runner metadata and container.id")
	}
	for name := range e.job.Services {
		if prepared.Services[name].ID == "" {
			return fmt.Errorf("container hook prepare must return ready service %q", name)
		}
	}
	e.prepared = prepared
	return nil
}

// Prepared returns runtime metadata, including ready services.
func (e *HookEnvironment) Prepared() HookPrepared { return e.prepared }

func (e *HookEnvironment) Exec(command []string, env map[string]string, user, workdir string) common.Executor {
	return func(ctx context.Context) error {
		if len(command) == 0 {
			return errors.New("container hook command is empty")
		}
		if workdir == "" {
			workdir = e.prepared.Paths.Workspace
		}
		if !path.IsAbs(workdir) {
			workdir = path.Join(e.prepared.Paths.Workspace, workdir)
		}
		return e.run(ctx, "run_command_step", HookCommand{EntryPoint: command[0], EntryPointArgs: command[1:], EnvironmentVariables: env, User: user, WorkingDirectory: workdir})
	}
}

func (e *HookEnvironment) run(ctx context.Context, event string, args any) error {
	var result struct {
		ExitCode *int `json:"exitCode"`
	}
	if err := e.invoke(ctx, event, args, &result); err != nil {
		return err
	}
	if result.ExitCode == nil || *result.ExitCode < 0 {
		return errors.New("container hook must return a nonnegative exitCode")
	}
	if *result.ExitCode != 0 {
		return ExitCodeError(*result.ExitCode)
	}
	return nil
}

// RunContainerStep delegates image execution and optional Dockerfile building to the hook.
func (e *HookEnvironment) RunContainerStep(ctx context.Context, step HookContainerStep, contextDir string, contextIsJob bool) error {
	if step.Dockerfile != "" {
		archive, err := e.payload(func(w io.Writer) error {
			if contextIsJob {
				reader, err := e.GetContainerArchive(ctx, contextDir+"/.")
				if err != nil {
					return err
				}
				defer reader.Close()
				_, err = io.Copy(w, reader)
				return err
			}
			return writeDirTar(ctx, w, "/", filepath.Join(contextDir, ".")+string(filepath.Separator), false, false, 0, 0, hostOS == "windows")
		})
		if err != nil {
			return err
		}
		defer os.Remove(archive)
		step.BuildContextArchive = archive
	}
	return e.run(ctx, "run_container_step", step)
}

func (e *HookEnvironment) payload(write func(io.Writer) error) (string, error) {
	file, err := os.CreateTemp(e.root, "payload-*.tar")
	if err != nil {
		return "", hookLocalError("container hook archive creation failed", err)
	}
	err = errors.Join(write(file), file.Close())
	if err != nil {
		os.Remove(file.Name())
		return "", hookLocalError("container hook archive writing failed", err)
	}
	return file.Name(), nil
}

func (e *HookEnvironment) copyTo(ctx context.Context, dest string, write func(io.Writer) error) error {
	archive, err := e.payload(write)
	if err != nil {
		return err
	}
	defer os.Remove(archive)
	return e.invoke(ctx, "copy_to_job", map[string]string{"archiveFile": archive, "destination": dest}, nil)
}

func (e *HookEnvironment) Copy(destPath string, files ...*FileEntry) common.Executor {
	return func(ctx context.Context) error {
		return e.copyTo(ctx, destPath, func(w io.Writer) error { return writeFilesTar(ctx, w, 0, 0, files...) })
	}
}

func (e *HookEnvironment) CopyDir(destPath, srcPath string, useGitIgnore, skipGitDir bool) common.Executor {
	return func(ctx context.Context) error {
		return e.copyTo(ctx, destPath, func(w io.Writer) error {
			return writeDirTar(ctx, w, "/", srcPath, useGitIgnore, skipGitDir, 0, 0, hostOS == "windows")
		})
	}
}

type hookArchive struct{ *os.File }

func (a *hookArchive) Close() error {
	return hookLocalError("container hook archive cleanup failed", errors.Join(a.File.Close(), os.Remove(a.Name())))
}

func (e *HookEnvironment) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	archive, err := e.payload(func(io.Writer) error { return nil })
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrArchiveUnavailable, err)
	}
	if err = e.invoke(ctx, "copy_from_job", map[string]string{"archiveFile": archive, "source": srcPath}, nil); err != nil {
		os.Remove(archive)
		// A missing local response or executable must not hide a failed transfer.
		if errors.Is(err, errHookSourceNotFound) {
			return nil, errors.Join(fs.ErrNotExist, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrArchiveUnavailable, err)
	}
	file, err := os.Open(archive)
	if err != nil {
		os.Remove(archive)
		return nil, errors.Join(ErrArchiveUnavailable, hookLocalError("container hook archive cannot be opened", err))
	}
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		file.Close()
		os.Remove(archive)
		return nil, errors.Join(ErrArchiveUnavailable, errors.New("hook did not produce an archive"), hookLocalError("container hook archive cannot be inspected", err))
	}
	return &hookArchive{file}, nil
}

func (e *HookEnvironment) Remove() common.Executor {
	return func(ctx context.Context) error {
		e.cleanup.Do(func() {
			e.cleanupErr = errors.Join(e.invoke(ctx, "cleanup_job", struct{}{}, nil), hookLocalError("container hook private directory cleanup failed", os.RemoveAll(e.root)))
		})
		return e.cleanupErr
	}
}

func (*HookEnvironment) Create(_, _ []string) common.Executor { return common.NewPipelineExecutor() }

func (*HookEnvironment) Pull(bool) common.Executor { return common.NewPipelineExecutor() }

func (*HookEnvironment) Start(bool) common.Executor { return common.NewPipelineExecutor() }

func (*HookEnvironment) Close() common.Executor         { return common.NewPipelineExecutor() }
func (*HookEnvironment) DumpLogs(context.Context) error { return nil }
func (e *HookEnvironment) Inspect(context.Context) (*Info, error) {
	return &Info{ID: e.prepared.Container.ID, State: StateRunning, Health: HealthNone}, nil
}

func (e *HookEnvironment) UpdateFromEnv(src string, env *map[string]string) common.Executor {
	return parseEnvFile(e, src, env, false)
}

func (e *HookEnvironment) UpdateFromImageEnv(env *map[string]string) common.Executor {
	return func(context.Context) error {
		for k, v := range e.prepared.ImageEnvironment {
			if _, exists := (*env)[k]; !exists {
				(*env)[k] = v
			}
		}
		return nil
	}
}
func (e *HookEnvironment) GetActPath() string { return e.prepared.Paths.Act }
func (e *HookEnvironment) GetRunnerContext(context.Context) map[string]any {
	return map[string]any{"os": e.prepared.Runner.OS, "arch": e.prepared.Runner.Arch, "temp": e.prepared.Paths.Temp, "tool_cache": e.prepared.Paths.ToolCache}
}

func (e *HookEnvironment) ToContainerPath(value string) string {
	if rel, err := filepath.Rel(e.workdir, value); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path.Join(e.prepared.Paths.Workspace, filepath.ToSlash(rel))
	}
	if strings.HasPrefix(value, "/") {
		return path.Clean(value)
	}
	return e.LinuxContainerEnvironmentExtensions.ToContainerPath(value)
}

func (e *HookEnvironment) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	previousOut, previousErr := e.stdout, e.stderr
	e.stdout, e.stderr = stdout, stderr
	return previousOut, previousErr
}
