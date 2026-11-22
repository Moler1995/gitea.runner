// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/require"
)

func TestReusableWorkflowCacheRefreshesBranchRefAndIsolatesSameRepoCalls(t *testing.T) {
	requireHostTools(t, "git")
	remoteDir := t.TempDir()
	const instance = "https://workflows.example"
	const cloneURL = instance + "/org/repo"
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	gitMust(t, "", "config", "--global", "url."+remoteDir+".insteadOf", cloneURL)
	gitMust(t, "", "config", "--global", "core.longpaths", "true")
	gitMust(t, "", "init", "--template=", "--initial-branch=master", remoteDir)

	const workflowPath = ".gitea/workflows/reusable.yml"
	require.NoError(t, os.MkdirAll(filepath.Join(remoteDir, ".gitea/workflows"), 0o755))
	rc := workflowCaller(t, &Config{ActionCacheDir: t.TempDir(), PresetGitHubContext: &model.GithubContext{Repository: "root/project"}}, "./"+workflowPath)
	commit := func(version string) *caller {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(remoteDir, workflowPath), []byte(`on:
  workflow_call:
    outputs: {version: {value: "`+version+`@${{ github.repository }}"}}
jobs:
  skip: {if: false, uses: ./unused.yml}
`), 0o644))
		gitMust(t, remoteDir, "add", workflowPath)
		gitMust(t, remoteDir, "commit", "-m", version)
		call := &caller{runContext: rc}
		_, err := call.loadReusableWorkflow(t.Context(), instance, "org/repo/"+workflowPath+"@master")
		require.NoError(t, err)
		return call
	}

	first := commit("v1")
	commit("v2")
	cacheDir := filepath.Join(rc.ActionCacheDir(), "workflows", model.UsesHash(cloneURL+"@master"))
	got, err := os.ReadFile(filepath.Join(cacheDir, "git", workflowPath))
	require.NoError(t, err)
	require.Contains(t, string(got), "v2@")
	sameRepo := workflowCaller(t, &Config{ActionCacheDir: rc.ActionCacheDir(), PresetGitHubContext: &model.GithubContext{Repository: "org/repo", Sha: "master"}}, "./"+workflowPath)
	sameRepoCall := &caller{runContext: sameRepo}
	_, err = sameRepoCall.loadReusableWorkflow(t.Context(), instance, "./"+workflowPath)
	require.NoError(t, err)
	require.NotEqual(t, cacheDir, filepath.Dir(sameRepoCall.checkout.Dir))

	require.NoError(t, os.RemoveAll(remoteDir))
	require.NoError(t, first.resolveInputs(t.Context(), rc.Run.Workflow))
	call := (&runnerImpl{config: rc.Config, caller: first}).NewPlanExecutor(&model.Plan{Stages: []*model.Stage{{Runs: []*model.Run{rc.Run}}}})
	require.NoError(t, call(t.Context()))
	require.Equal(t, "v1@root/project", rc.Run.Job().Outputs["version"])

	synctest.Test(t, func(t *testing.T) {
		unlock, err := git.AcquireCloneLock(t.Context(), cacheDir)
		require.NoError(t, err)
		defer unlock()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- call(ctx) }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		require.Equal(t, "cancelled", rc.Run.Job().Result)
	})
}

func TestNewLocalReusableWorkflowExecutorFindsSameRepositoryPaths(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workdir, ".gitea/workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workdir, ".gitea/workflows/reusable.yml"), []byte("on: workflow_call\njobs:\n  skip: {if: false, uses: ./unused.yml}\n"), 0o644))

	for _, uses := range []string{"./.gitea/workflows/reusable.yml", "$/.gitea/workflows/reusable.yml"} {
		rc := workflowCaller(t, &Config{Workdir: workdir, Env: testCheckoutEnv()}, uses)
		require.NoError(t, rc.runReusableWorkflow(t.Context()))
		require.Equal(t, "success", rc.Run.Job().Result)
	}
}

func TestCloneRemoteReusableWorkflowSendsInstanceTokenAndClientCertTrustingCA(t *testing.T) {
	var gotCert bool
	var gotToken string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCert = len(r.TLS.PeerCertificates) > 0
		_, gotToken, _ = r.BasicAuth()
		w.WriteHeader(http.StatusNotFound)
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	server.StartTLS()
	defer server.Close()
	pair := server.TLS.Certificates[0]
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	require.NoError(t, err)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	rc := workflowCaller(t, &Config{
		ActionCacheDir: t.TempDir(),
		Env:            testCheckoutEnv(),
		GitHubInstance: server.URL,
		Secrets:        map[string]string{"GITEA_TOKEN": "token"},
		ClientCertFile: certFile,
		ClientKeyFile:  keyFile,
		CAFile:         certFile,
	}, server.URL+"/owner/repo/.gitea/workflows/test.yml@main")

	err = rc.runReusableWorkflow(t.Context())
	require.Error(t, err)
	require.True(t, gotCert, "clone failed before receiving a client certificate: %v", err)
	require.Equal(t, "token", gotToken)
}

func TestInstanceAuthAndTransportOnlyApplyToTheInstance(t *testing.T) {
	for _, tt := range []struct {
		name, instanceURL, trustedActionInstance, cloneURL string
		token, cert, insecure                              bool
	}{
		{"same host with schemaless instance", "gitea.example.net", "", "https://gitea.example.net/actions/tools", true, true, true},
		{"same host with schemaless instance and port", "gitea.example.net:3000", "", "https://gitea.example.net:3000/actions/tools", true, true, true},
		{"different host", "gitea.example.net", "", "https://github.com/actions/tools", false, false, false},
		{"embedded basic auth keeps its own credentials", "gitea.example.net", "", "https://user:pass@gitea.example.net/actions/tools", false, true, true},
		{"invalid clone URL", "gitea.example.net", "", "://gitea.example.net/actions/tools", false, false, false},
		{"self-hosted action instance on different host skips no verification", "gitea.local", "https://gitea.my-nas.lan", "https://gitea.my-nas.lan/owner/action", true, true, false},
		{"self-hosted action instance with embedded basic auth", "gitea.local", "https://gitea.my-nas.lan", "https://user:pass@gitea.my-nas.lan/owner/action", false, true, false},
		{"github mode does not trust mirror host", "gitea.local", "", "https://mirror.example.com/owner/action", false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conf := Config{
				GitHubInstance:                    tt.instanceURL,
				ActionCloneDepth:                  3,
				ActionOfflineMode:                 true,
				DefaultActionInstance:             tt.trustedActionInstance,
				DefaultActionInstanceIsSelfHosted: tt.trustedActionInstance != "",
				InsecureSkipTLS:                   true,
				ClientCertFile:                    "/cert",
				ClientKeyFile:                     "/key",
				CAFile:                            "/ca",
				InstanceTransport:                 &http.Transport{},
			}
			want := git.NewGitCloneExecutorInput{URL: tt.cloneURL, Depth: 3, OfflineMode: true, InsecureSkipTLS: tt.insecure}
			if tt.insecure {
				want.CAFile, want.Transport = conf.CAFile, conf.InstanceTransport
			}
			if tt.token {
				want.Token = "token"
			}
			if tt.cert {
				want.ClientCertFile, want.ClientKeyFile = conf.ClientCertFile, conf.ClientKeyFile
			}
			require.Equal(t, want, conf.withInstanceAuth(git.NewGitCloneExecutorInput{URL: tt.cloneURL}, "token"))
		})
	}

	transport := &http.Transport{}
	conf := Config{GitHubInstance: "https://gitea.local", InstanceTransport: transport}
	require.Same(t, transport, conf.instanceTransportFor("https://gitea.local/api/actions_pipeline/"))
	require.Nil(t, conf.instanceTransportFor("https://gitea.local.example/api/actions_pipeline/"))
	require.Nil(t, conf.instanceTransportFor("https://other.example/x#/api/actions_pipeline/"))
}

func gitMust(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@test"}, args...)...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
}

func workflowCaller(t *testing.T, config *Config, uses string) *RunContext {
	t.Helper()
	rc, err := (&runnerImpl{config: config}).newRunContext(t.Context(), &model.Run{
		JobID: "call", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"call": {Uses: uses}}},
	}, nil)
	require.NoError(t, err)
	return rc
}

func TestReusableWorkflowBadOutputFailsCallerAndKeepsLastNonemptyMatrixOutput(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "callee.yml"), []byte(`on:
  workflow_call:
    inputs: {value: {type: string}}
    outputs: {result: {value: "${{ fromJSON(inputs.value) }}"}}
jobs:
  skipped: {if: false, uses: ./unused.yml}
`), 0o644))
	root, err := model.ReadWorkflow(strings.NewReader(`on: push
jobs:
  call:
    strategy: {max-parallel: 1, matrix: {value: [bad, '"one"', '"two"', '""']}}
    uses: ./callee.yml
    with: {value: "${{ matrix.value }}"}
`))
	require.NoError(t, err)
	plan, err := model.CombineWorkflowPlanner(root).PlanEvent("push")
	require.NoError(t, err)
	runner, err := New(&Config{Workdir: workdir, Env: testCheckoutEnv()})
	require.NoError(t, err)
	require.ErrorContains(t, runner.NewPlanExecutor(plan)(t.Context()), "job 'call' failed")
	require.Equal(t, "two", root.Jobs["call"].Outputs["result"])
}

func TestReusableWorkflowKeepsCompletedOutputsOnChildErrorWithoutLoggingThem(t *testing.T) {
	workdir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "callee.yml"), []byte(`on:
  workflow_call:
    outputs:
      result: {value: "${{ jobs.first.outputs.value }}"}
      unrun: {value: "${{ jobs.third.outputs.value }}"}
jobs:
  placeholder: {result: cancelled}
  first: {runs-on: ubuntu-latest, outputs: {value: from-first}, steps: [{run: echo first}]}
  second: {needs: first, runs-on: ubuntu-latest, container: {image: node, ports: [invalid]}, steps: [{run: echo second}]}
  third: {needs: second, runs-on: ubuntu-latest, outputs: {value: from-third}, steps: [{run: echo third}]}
`), 0o644))
	rc := workflowCaller(t, &Config{Workdir: workdir, Env: testCheckoutEnv(), PlatformPicker: mapPlatformPicker(platforms)}, "./callee.yml")
	hook := logtest.NewWithLevel(logging.LevelDebug)
	ctx := common.WithLogger(common.WithDryrun(t.Context(), true), logging.New(hook))
	require.ErrorContains(t, rc.runReusableWorkflow(ctx), "failed to parse container ports")
	require.Equal(t, "failure", rc.Run.Job().Result)
	require.Equal(t, map[string]string{"result": "from-first"}, rc.Run.Job().Outputs)
	for _, record := range hook.AllRecords() {
		require.NotContains(t, record.Message, "from-first")
	}
}

func TestReusableWorkflowNestingLimit(t *testing.T) {
	workdir := t.TempDir()
	for level := 2; level <= model.MaxWorkflowCallDepth; level++ {
		require.NoError(t, os.WriteFile(filepath.Join(workdir, fmt.Sprintf("%d.yml", level)), []byte(fmt.Sprintf("on: workflow_call\njobs:\n  next: {uses: ./%d.yml}\n", level+1)), 0o644))
	}
	rc := workflowCaller(t, &Config{Workdir: workdir, Env: testCheckoutEnv()}, "./2.yml")
	require.ErrorContains(t, rc.runReusableWorkflow(t.Context()), fmt.Sprintf(`"%d.yml" exceeds the limit of %d`, model.MaxWorkflowCallDepth+1, model.MaxWorkflowCallDepth))
}
