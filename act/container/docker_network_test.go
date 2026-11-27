// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd || freebsd))

package container

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectCacheIsolation(t *testing.T) {
	ctx := t.Context()
	cache, lan, gateway := netip.MustParseAddr("172.18.0.3"), netip.MustParseAddr("192.168.1.20"), netip.MustParsePrefix("172.19.0.3/16")
	client := &mockDockerClient{}
	client.On("NetworkInspect", ctx, "docker_gwbridge", mobyclient.NetworkInspectOptions{}).Return(mobyclient.NetworkInspectResult{}, cerrdefs.ErrNotFound).Once()
	client.On("ContainerList", ctx, mobyclient.ContainerListOptions{}).Return(mobyclient.ContainerListResult{Items: []container.Summary{{}, {
		ID: "0123456789abcdef",
		NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{
			"compose": {NetworkID: "c0ffee", IPAddress: cache},
			"lan":     {NetworkID: "beef", IPAddress: lan},
		}},
	}}}, nil)
	for ref, inspected := range map[string]network.Inspect{
		"c0ffee":          {Network: network.Network{ID: "c0ffee", Name: "compose", Driver: "bridge"}},
		"beef":            {Network: network.Network{ID: "beef", Name: "lan", Driver: "macvlan"}},
		"c0f":             {Network: network.Network{ID: "c0ffee"}},
		"jobs":            {Network: network.Network{ID: "f00d"}},
		"docker_gwbridge": {Containers: map[string]network.EndpointResource{"fedcba9876543210": {IPv4Address: gateway}}},
	} {
		client.On("NetworkInspect", ctx, ref, mobyclient.NetworkInspectOptions{}).
			Return(mobyclient.NetworkInspectResult{Network: inspected}, nil)
	}
	check := func(addr netip.Addr, jobNetwork string, want CacheIsolation) {
		got, err := detectCacheIsolation(ctx, client, addr, jobNetwork)
		require.NoError(t, err)
		assert.Equal(t, want, got, jobNetwork)
	}
	check(cache, "", CacheIsolation{ContainerID: "0123456789ab"})
	check(cache, "jobs", CacheIsolation{ContainerID: "0123456789ab"})
	check(cache, "c0f", CacheIsolation{})
	check(cache, "host", CacheIsolation{})
	check(lan, "", CacheIsolation{})
	check(gateway.Addr(), "jobs", CacheIsolation{})
	check(gateway.Addr(), "jobs", CacheIsolation{GatewayBridge: true})
	check(netip.MustParseAddr("10.0.0.5"), "", CacheIsolation{})
}

func TestIsAddressPoolExhausted(t *testing.T) {
	assert.True(t, isAddressPoolExhausted(cerrdefs.ErrInvalidArgument.WithMessage("Error response from daemon: all predefined address pools have been fully subnetted")))
	assert.True(t, isAddressPoolExhausted(errors.New("could not find an available, non-overlapping IPv4 address pool among the defaults to assign to the network")))
	assert.False(t, isAddressPoolExhausted(cerrdefs.ErrInvalidArgument.WithMessage("invalid subnet 10.0.0.0/8: it overlaps with an existing network")))
}

func TestCreateDockerNetworkInheritsDaemonMTU(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		bridgeOptions, want map[string]string
	}{
		{
			map[string]string{"com.docker.network.bridge.host_binding_ipv4": "127.0.0.1", "com.docker.network.driver.mtu": "1450"},
			map[string]string{"com.docker.network.driver.mtu": "1450"},
		},
		{map[string]string{"com.docker.network.driver.mtu": "1500"}, nil},
	} {
		client := &mockDockerClient{}
		client.On("NetworkList", ctx, mobyclient.NetworkListOptions{}).
			Return(mobyclient.NetworkListResult{Items: []network.Summary{{Name: network.NetworkBridge, Options: tc.bridgeOptions}}}, nil)
		client.On("NetworkCreate", ctx, "job-network", mobyclient.NetworkCreateOptions{Driver: "bridge", Scope: "local", Options: tc.want}).
			Return(mobyclient.NetworkCreateResult{}, nil)
		require.NoError(t, createDockerNetwork(ctx, client, "job-network", NewDockerNetworkCreateExecutorInput{}))
		client.AssertExpectations(t)
	}
}

func (m *mockDockerClient) NetworkConnect(ctx context.Context, id string, opts mobyclient.NetworkConnectOptions) (mobyclient.NetworkConnectResult, error) {
	return mobyclient.NetworkConnectResult{}, m.Called(ctx, id, opts).Error(0)
}

func (m *mockDockerClient) NetworkDisconnect(ctx context.Context, id string, opts mobyclient.NetworkDisconnectOptions) (mobyclient.NetworkDisconnectResult, error) {
	return mobyclient.NetworkDisconnectResult{}, m.Called(ctx, id, opts).Error(0)
}

func TestConnectAndDisconnectNetwork(t *testing.T) {
	ctx := t.Context()
	connect := func(answer error) error {
		client := &mockDockerClient{}
		client.On("NetworkConnect", ctx, "jobs", mobyclient.NetworkConnectOptions{
			Container:      "0123456789ab",
			EndpointConfig: &network.EndpointSettings{Aliases: []string{"0123456789ab"}},
		}).Return(answer).Once()
		return connectNetwork(ctx, client, "jobs", "0123456789ab")
	}
	require.NoError(t, connect(nil))
	require.NoError(t, connect(errors.New("endpoint with name runner already exists in network jobs")))
	require.NoError(t, connect(errors.New("container runner already connected to network jobs")))
	require.ErrorIs(t, connect(context.Canceled), context.Canceled)

	disconnect := func(answer error) error {
		client := &mockDockerClient{}
		client.On("NetworkDisconnect", ctx, "jobs", mobyclient.NetworkDisconnectOptions{
			Container: "0123456789ab", Force: true,
		}).Return(answer).Once()
		return disconnectNetwork(ctx, client, "jobs", "0123456789ab")
	}
	require.NoError(t, disconnect(cerrdefs.ErrNotFound))
	require.NoError(t, disconnect(errors.New("container runner is not connected to the network jobs")))
	require.ErrorIs(t, disconnect(context.Canceled), context.Canceled)
}

// Of this runner's networks, only the ones nothing but its own cache container is attached to
// and old enough to predate any job now starting are the runner's to reclaim. An unexpected
// NetworkRemove fails the test on its own, since testify has no expectation to match it against.
func TestRemoveNetworks(t *testing.T) {
	ctx := t.Context()
	cutoff := time.Date(2026, time.April, 29, 20, 0, 0, 0, time.UTC)
	client := &mockDockerClient{}
	client.On("NetworkList", ctx, mobyclient.NetworkListOptions{
		Filters: make(mobyclient.Filters).Add("label", runnerUUIDLabel+"=runner-1"),
	}).Return(mobyclient.NetworkListResult{Items: []network.Summary{
		{ID: "orphan"},
		{ID: "busy"},
		{ID: "starting"},
		{ID: "cached"},
		{ID: "shared"},
		{ID: "gone"},
		{ID: "raced"},
	}}, nil)
	client.On("NetworkInspect", ctx, "orphan", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{}, nil).Twice()
	client.On("NetworkInspect", ctx, "busy", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{Network: network.Inspect{Containers: map[string]network.EndpointResource{"c": {}}}}, nil).Twice()
	client.On("NetworkInspect", ctx, "starting", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{Network: network.Inspect{
			Network:    network.Network{Created: cutoff.Add(time.Second)},
			Containers: map[string]network.EndpointResource{"0123456789abcdef": {}},
		}}, nil)
	client.On("NetworkInspect", ctx, "cached", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{Network: network.Inspect{Containers: map[string]network.EndpointResource{"0123456789abcdef": {}}}}, nil)
	client.On("NetworkInspect", ctx, "shared", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{Network: network.Inspect{Containers: map[string]network.EndpointResource{"0123456789abcdef": {}, "job": {}}}}, nil)
	client.On("NetworkInspect", ctx, "gone", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{}, cerrdefs.ErrNotFound).Twice()
	client.On("NetworkInspect", ctx, "raced", mobyclient.NetworkInspectOptions{}).
		Return(mobyclient.NetworkInspectResult{}, nil).Twice()
	client.On("NetworkRemove", ctx, "raced", mobyclient.NetworkRemoveOptions{}).
		Return(mobyclient.NetworkRemoveResult{}, cerrdefs.ErrNotFound).Twice()
	client.On("NetworkRemove", ctx, "orphan", mobyclient.NetworkRemoveOptions{}).
		Return(mobyclient.NetworkRemoveResult{}, nil).Twice()
	client.On("NetworkDisconnect", ctx, "cached", mobyclient.NetworkDisconnectOptions{Container: "0123456789abcdef", Force: true}).
		Return(nil).Once()
	client.On("NetworkRemove", ctx, "cached", mobyclient.NetworkRemoveOptions{}).
		Return(mobyclient.NetworkRemoveResult{}, nil).Once()

	require.NoError(t, removeOrphanNetworks(ctx, client, "runner-1", "0123456789ab", cutoff))

	client.On("NetworkList", ctx, mobyclient.NetworkListOptions{}).Return(mobyclient.NetworkListResult{Items: []network.Summary{
		{ID: "orphan", Name: "job"},
		{ID: "busy", Name: "job"},
		{ID: "gone", Name: "job"},
		{ID: "raced", Name: "job"},
		{ID: "shared", Name: "other"},
	}}, nil)
	require.NoError(t, removeNetwork(ctx, client, "job"))
	client.AssertExpectations(t)
}
