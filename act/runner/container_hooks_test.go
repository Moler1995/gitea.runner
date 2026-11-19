// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
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

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runHookWorkflow(t *testing.T, workflow string, hooks container.HookConfig, configure ...func(*Config)) (string, error) {
	t.Helper()
	workdir := t.TempDir()
	file := filepath.Join(workdir, "workflow.yml")
	require.NoError(t, os.WriteFile(file, []byte(workflow), 0o600))
	planner, err := model.NewWorkflowPlanner(file, true)
	require.NoError(t, err)
	plan, err := planner.PlanEvent("push")
	require.NoError(t, err)
	cfg := &Config{Workdir: workdir, Env: map[string]string{}, ContainerHooks: hooks, NoSkipCheckout: true, NoActionPatch: true, MaxParallel: 2, PlatformPicker: func([]string) string { return "alpine:test" }}
	for _, configure := range configure {
		configure(cfg)
	}
	runner, err := New(cfg)
	require.NoError(t, err)
	logger, records := logtest.NewLogger()
	ctx := WithJobLoggerFactory(common.WithLogger(t.Context(), logger), factoryFunc(func() logging.Handler { return records }))
	err = runner.NewPlanExecutor(plan)(ctx)
	var output strings.Builder
	for _, record := range records.AllRecords() {
		output.WriteString(record.Message)
		output.WriteByte('\n')
	}
	return output.String(), err
}

func TestContainerHooksWorkflow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fixture runs Linux job commands")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("fixture needs Python 3")
	}
	hook, err := filepath.Abs("testdata/container-hooks/hook.py")
	require.NoError(t, err)
	root := t.TempDir()
	t.Setenv("GITEA_HOOK_TEST_ROOT", root)
	t.Setenv("DOCKER_HOST", "unix:///nonexistent-gitea-hook-test.sock")
	actions := t.TempDir()
	checkout := filepath.Join(actions, "fixture", "checkout")
	lifecycle := filepath.Join(actions, "fixture", "lifecycle")
	source := t.TempDir()
	javascript := ""
	if _, err := exec.LookPath("node"); err == nil {
		directory := filepath.Join(actions, "fixture", "javascript")
		require.NoError(t, os.MkdirAll(directory, 0o700))
		for name, body := range map[string]string{
			"action.yml": "name: JavaScript fixture\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n  post: post.js\n",
			"pre.js":     `require('node:fs').appendFileSync(process.env.GITHUB_STATE, 'jsSeen=ready\n'); console.log('hook-js-pre');`,
			"main.js":    `if (process.env.STATE_jsSeen !== 'ready') process.exit(17); console.log('hook-js-main');`,
			"post.js":    `if (process.env.STATE_jsSeen !== 'ready') process.exit(17); console.log('hook-js-post');`,
		} {
			require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600))
		}
		gitMust(t, directory, "init", "--template=", "--initial-branch=main")
		gitMust(t, directory, "add", ".")
		gitMust(t, directory, "commit", "-m", "fixture")
		javascript = "      - uses: fixture/javascript@main"
	} else {
		t.Log("JavaScript lifecycle fixture requires node on PATH")
	}
	for directory, manifest := range map[string]string{
		checkout: `name: checkout fixture
runs:
  using: composite
  steps:
    - shell: bash
      run: |
        test -f "$GITHUB_ACTION_PATH/action.yml"
        git clone -- "$HOOK_CHECKOUT_SOURCE" "$GITHUB_WORKSPACE"
`,
		lifecycle: `name: lifecycle fixture
runs:
  using: docker
  image: docker://fixture/lifecycle
  pre-entrypoint: /bin/sh -c 'echo seen=pre >> "$GITHUB_STATE"; echo hook-action-pre'
  entrypoint: /bin/sh -c 'test "$STATE_seen" = pre; echo hook-action-main'
  post-entrypoint: /bin/sh -c 'test "$STATE_seen" = pre; echo hook-action-post'
`,
		source: "",
	} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
		if manifest != "" {
			require.NoError(t, os.WriteFile(filepath.Join(directory, "action.yml"), []byte(manifest), 0o600))
		} else {
			local := filepath.Join(directory, ".gitea", "actions", "local")
			require.NoError(t, os.MkdirAll(local, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(local, "action.yml"), []byte(`name: local
outputs:
  value:
    value: ${{ steps.emit.outputs.value }}
runs:
  using: composite
  steps:
    - id: emit
      shell: bash
      run: echo 'value=from-local' >> "$GITHUB_OUTPUT"
`), 0o600))
		}
		gitMust(t, directory, "init", "--template=", "--initial-branch=main")
		gitMust(t, directory, "add", ".")
		gitMust(t, directory, "commit", "-m", "fixture")
	}
	previous := newContainer
	newContainer = func(*container.NewContainerInput) container.ExecutionsEnvironment {
		t.Error("hooks called the Docker container factory")
		return nil
	}
	t.Cleanup(func() { newContainer = previous })
	workflow := `name: hooks
on: push
jobs:
  test:
    runs-on: docker
    strategy:
      matrix:
        instance: [one, two]
    services:
      database:
        image: postgres:test
    steps:
      - uses: fixture/checkout@main
      - name: environment commands
        shell: bash
        env:
          HOOK_SECRET: ${{ secrets.HOOK_SECRET }}
        run: |
          echo "$HOOK_SECRET"
          echo 'hook stderr streamed' >&2
          echo 'PERSISTED=from-env' >> "$GITHUB_ENV"
          echo '/extra/tools' >> "$GITHUB_PATH"
      - id: local
        uses: ./.gitea/actions/local
      # JAVASCRIPT_ACTION
      - uses: fixture/lifecycle@main
      - id: tolerated
        continue-on-error: true
        shell: bash
        run: |
          echo 'value=failed-output' >> "$GITHUB_OUTPUT"
          exit 17
      - uses: docker://example/image
        with:
          entrypoint: /bin/sh
          args: -c 'test "$VALUE" = from-local'
        env:
          VALUE: ${{ steps.local.outputs.value }}
      - shell: bash
        env:
          VALUE: ${{ steps.local.outputs.value }}
          FAILED_OUTPUT: ${{ steps.tolerated.outputs.value }}
          OUTCOME: ${{ steps.tolerated.outcome }}
        run: |
          test "$VALUE" = from-local
          test "$FAILED_OUTPUT" = failed-output
          test "$OUTCOME" = failure
          test "$PERSISTED" = from-env
          test "$PWD" = "$GITHUB_WORKSPACE"
          test -n "$RUNNER_ARCH"
          test -d "$RUNNER_TEMP"
          test -d "$RUNNER_TOOL_CACHE"
          [[ "$PATH" == /extra/tools:* ]]
          echo "## Hooks summary" >> "$GITHUB_STEP_SUMMARY"
          echo 'hook workflow completed'
`
	summaries := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		summaries <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	workflow = strings.ReplaceAll(workflow, "      # JAVASCRIPT_ACTION", javascript)
	output, err := runHookWorkflow(t, workflow, container.HookConfig{Enabled: true, Entrypoint: python, Cmd: hook}, func(cfg *Config) {
		cfg.Env = map[string]string{"GITEA_ACTIONS_CAPABILITIES": "job-summary", "ACTIONS_RUNTIME_URL": server.URL, "ACTIONS_RUNTIME_TOKEN": fakeRuntimeToken(34), "GITEA_RUN_ID": "123", "HOOK_CHECKOUT_SOURCE": source, "RUNNER_TOOL_CACHE": "/suggested/tool-cache"}
		cfg.DefaultActionInstance = actions
		cfg.ActionCacheDir = t.TempDir()
		cfg.Secrets = map[string]string{"HOOK_SECRET": "private-hook-value"}
	})
	require.NoError(t, err, "%s", output)
	assert.Contains(t, output, "hook workflow completed")
	assert.Contains(t, output, "hook stderr streamed")
	assert.Contains(t, output, "***")
	assert.NotContains(t, output, "private-hook-value")
	for _, stage := range []string{"pre", "main", "post"} {
		assert.Contains(t, output, "hook-action-"+stage)
		if javascript != "" {
			assert.Contains(t, output, "hook-js-"+stage)
		}
	}
	require.Len(t, summaries, 2)
	assert.Contains(t, <-summaries, "Hooks summary")
	assert.Contains(t, <-summaries, "Hooks summary")
	data, err := os.ReadFile(filepath.Join(root, "events.jsonl"))
	require.NoError(t, err)
	jobs := map[string][]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var event struct {
			JobID   string `json:"jobId"`
			Command string `json:"command"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		jobs[event.JobID] = append(jobs[event.JobID], event.Command)
	}
	require.Len(t, jobs, 2)
	for _, events := range jobs {
		assert.Equal(t, "prepare_job", events[0])
		assert.Equal(t, "cleanup_job", events[len(events)-1])
		assert.Contains(t, events, "copy_to_job")
		assert.Contains(t, events, "copy_from_job")
		assert.Contains(t, events, "run_container_step")
	}
	files, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, files, 1, "runtime workspaces were not removed")

	t.Setenv("GITEA_HOOK_TEST_MODE", "deny")
	output, err = runHookWorkflow(t, "on: push\njobs:\n  test:\n    runs-on: docker\n    steps:\n      - run: echo should-not-run\n", container.HookConfig{Enabled: true, Entrypoint: python, Cmd: hook})
	require.ErrorContains(t, err, "policy_denied", "%s", output)
	assert.NotContains(t, output, "should-not-run")
	files, err = os.ReadDir(root)
	require.NoError(t, err)
	assert.Len(t, files, 1, "failed prepare did not clean up its runtime")
}

func TestContainerHooksLeaveHostAlone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("runs a Linux shell")
	}
	output, err := runHookWorkflow(t, "on: push\njobs:\n  test:\n    runs-on: host\n    steps:\n      - run: echo native-host-completed\n", container.HookConfig{Enabled: true, Cmd: "/missing-hook"}, func(cfg *Config) {
		cfg.PlatformPicker = func([]string) string { return "-self-hosted" }
	})
	require.NoError(t, err, "%s", output)
	assert.Contains(t, output, "native-host-completed")
}

func TestContainerHooksLeaveNativeKubernetesAlone(t *testing.T) {
	rc := &RunContext{Config: &Config{ContainerHooks: container.HookConfig{Enabled: true, Cmd: "/missing-hook"}}, kubernetes: true, containerSpec: model.ContainerSpec{Options: "unsupported"}}
	err := rc.startContainer()(context.Background())
	require.ErrorContains(t, err, "not supported on Kubernetes")
	assert.NotContains(t, err.Error(), "container.hooks")
}
