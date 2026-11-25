// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd || freebsd))

package container

import (
	"context"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageExistsLocally returns a boolean indicating if an image with the
// requested name, tag and architecture exists in the local docker image store
func ImageExistsLocally(ctx context.Context, imageName, platform string) (bool, error) {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return false, err
	}
	defer cli.Close()

	var requested *specs.Platform
	if platform == "" {
		if version, err := cli.ServerVersion(ctx, client.ServerVersionOptions{}); err == nil {
			requested = &specs.Platform{OS: version.Os, Architecture: version.Arch}
		}
	} else if platform != "any" {
		requested, err = parsePlatform(platform)
		if err != nil {
			return false, err
		}
	}

	inspectPlatform := requested
	if versions.LessThan(cli.ClientVersion(), "1.49") {
		inspectPlatform = nil
	}
	inspectImage, err := cli.ImageInspect(ctx, imageName, client.ImageInspectWithPlatform(inspectPlatform))
	if cerrdefs.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return requested == nil || platformMatches(requested, &specs.Platform{OS: inspectImage.Os, Architecture: inspectImage.Architecture, Variant: inspectImage.Variant}), nil
}

func platformMatches(requested, image *specs.Platform) bool {
	variant := image.Variant
	if variant == "" {
		variant = map[string]string{"amd64": "v1", "arm": "v7", "arm64": "v8"}[image.Architecture]
	}
	return image.OS == requested.OS && image.Architecture == requested.Architecture && (requested.Variant == "" || requested.Variant == variant)
}

func RemoveImage(ctx context.Context, imageName string, force, pruneChildren bool) (bool, error) {
	cli, err := GetDockerClient(ctx)
	if err != nil {
		return false, err
	}
	defer cli.Close()

	if _, err = cli.ImageRemove(ctx, imageName, client.ImageRemoveOptions{
		Force:         force,
		PruneChildren: pruneChildren,
	}); cerrdefs.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}
