// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd || freebsd))

package runner

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging/logtest"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDockerProxyMountedJob(t *testing.T) {
	mode := os.Getenv("ACT_TEST_DOCKER_PROXY")
	if mode == "" {
		t.Skip("set ACT_TEST_DOCKER_PROXY=proxy or direct to verify mounted Docker access")
	}
	require.Contains(t, []string{"proxy", "direct"}, mode)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	dockerClient, err := container.GetDockerClient(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, dockerClient.Close()) })
	require.True(t, strings.HasPrefix(dockerClient.DaemonHost(), "unix://"), "mounted Docker coverage requires a Unix daemon socket")

	fixtureDir, err := filepath.Abs("testdata/docker-proxy")
	require.NoError(t, err)
	resourceName := "gitea-proxy-test-" + strings.ToLower(rand.Text())
	runner, err := New(&Config{
		Workdir:               fixtureDir,
		ActionCacheDir:        t.TempDir(),
		EventName:             "push",
		PlatformPicker:        mapPlatformPicker(platforms),
		ContainerNamePrefix:   resourceName,
		ContainerDaemonSocket: dockerClient.DaemonHost(),
		ContainerMaxLifetime:  2 * time.Minute,
		Sweep:                 true,
		ContainerOptions:      "-v /usr/local/bin/docker:/usr/local/bin/docker:ro -v /usr/local/libexec/docker/cli-plugins:/usr/local/libexec/docker/cli-plugins:ro",
		ValidVolumes:          []string{"/usr/local/bin/docker", "/usr/local/libexec/docker/cli-plugins"},
		Env:                   map[string]string{"PROXY_TEST_RESOURCE": resourceName, "PROXY_TEST_MODE": mode, "PROXY_TEST_IMAGE": baseImage, "COMPOSE_PROJECT_NAME": resourceName},
	})
	require.NoError(t, err)
	planner, err := model.NewWorkflowPlanner(filepath.Join(fixtureDir, "push.yml"), true)
	require.NoError(t, err)
	plan, err := planner.PlanEvent("push")
	if mode == "direct" {
		plan, err = planner.PlanJob("proxy")
	}
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cleanCancel()
		containers, err := dockerClient.ContainerList(cleanCtx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("name", resourceName)})
		assert.NoError(t, err)
		assert.Empty(t, containers.Items, "job container survived cleanup")
		for _, leftover := range containers.Items {
			_, err := dockerClient.ContainerRemove(cleanCtx, leftover.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
			assert.NoError(t, err)
		}
		assert.NoError(t, container.NewDockerNetworkRemoveExecutor(resourceName)(cleanCtx))
		assert.NoError(t, container.NewDockerVolumeRemoveExecutor(resourceName, false)(cleanCtx))
	})

	hook := logtest.New()
	require.NoError(t, runner.NewPlanExecutor(plan)(common.WithLoggerHandler(ctx, hook)))
	var messages []string
	for _, entry := range hook.AllRecords() {
		messages = append(messages, strings.TrimSpace(entry.Message))
	}
	require.Contains(t, messages, "docker proxy post verified")
	if mode == "proxy" {
		require.Contains(t, messages, "docker binds verified")
	}
	_, err = dockerClient.NetworkInspect(ctx, resourceName, client.NetworkInspectOptions{})
	if mode == "proxy" {
		assert.True(t, cerrdefs.IsNotFound(err), "labelled network survived cleanup: %v", err)
	} else {
		require.NoError(t, err)
	}
	_, err = dockerClient.VolumeInspect(ctx, resourceName, client.VolumeInspectOptions{})
	if mode == "proxy" {
		assert.True(t, cerrdefs.IsNotFound(err), "labelled volume survived cleanup: %v", err)
	} else {
		require.NoError(t, err)
	}
}
