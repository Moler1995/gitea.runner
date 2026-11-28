// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/container"

	"golang.org/x/sync/singleflight"
)

type nodeBuild struct{ goos, goarch, loader, name, version, sha256 string }

func (build nodeBuild) release() string { return "node-v" + build.version + "-" + build.name }

var nodeBuilds = []nodeBuild{
	// renovate: datasource=custom.nodejs
	{"linux", "amd64", "/lib64/ld-linux-x86-64.so.2", "linux-x64", "24.22.3", "061ec3e3062115705b56b51fa9cc330ab50d34b6969387d4e4b1a50c6809acc2"},
	// renovate: datasource=custom.nodejs
	{"linux", "arm64", "/lib/ld-linux-aarch64.so.1", "linux-arm64", "24.22.3", "261af2af250dfd7e9f25cf151d1b2713a0adf8f10a9ad116c4947f68627affe8"},
	// renovate: datasource=custom.alpine-nodejs
	{"linux", "amd64", "/lib/ld-musl-x86_64.so.1", "alpine-x64", "24.22.3", "07fae3d6173ae8bc6529df8a4b3c070d80f94e95bd816982c4af253acf7c2b51"},
	// renovate: datasource=custom.nodejs
	{"darwin", "amd64", "", "darwin-x64", "24.22.3", "977614af0504c81045e7fd85596dbca0e5f951b96681516dadafedcca8c447f7"},
	// renovate: datasource=custom.nodejs
	{"darwin", "arm64", "", "darwin-arm64", "24.22.3", "ff6e17d4874b76d272212b908cc020d6e815fba5a6c7eaaa06add725840307fd"},
	// renovate: datasource=custom.nodejs
	{"windows", "amd64", "", "win-x64", "24.22.3", "86fc4785327db5f5c7558dc6156c543b433c313e813a222e088086222ab4afa6"},
	// renovate: datasource=custom.nodejs
	{"windows", "arm64", "", "win-arm64", "24.22.3", "172e320db179ce416fc012b96b9917dacfb442beb081de57c4deaed7b5a24644"},
}

// nodeBuildsFor returns the pinned builds, or for a runner.node_version override the same builds at that version.
func nodeBuildsFor(version string) []nodeBuild {
	if version == "" {
		return nodeBuilds
	}
	builds := slices.Clone(nodeBuilds)
	for i := range builds {
		builds[i].version, builds[i].sha256 = version, ""
	}
	return builds
}

func (c *Config) nodeVolume() string {
	return nodeVolumePrefix + cmp.Or(c.NodeVersion, nodeBuilds[0].version) // the other builds' releases join it under their own names
}

const (
	nodeVolumePrefix = "gitea-runner-node-v"
	nodeComplete     = "complete" // the last entry of a node tar, so a binary beside it is whole
)

var (
	nodeDownloads singleflight.Group
	nodeFailures  sync.Map // binary path to the time.Time its download last failed
	nodeSeeding   = make(chan struct{}, 1)
)

// PrefetchNode starts downloading the builds the runner's jobs need, so jobs find them cached.
func PrefetchNode(cacheDir, version string, host, docker, kubernetes bool) {
	for _, build := range nodeBuildsFor(version) {
		linux := build.goarch == runtime.GOARCH && strings.HasPrefix(build.name, "linux-")
		if host && nodeFitsHost(build) || (docker || kubernetes) && linux {
			go func() {
				binary, err := downloadNode(context.Background(), filepath.Join(cacheDir, "node"), build)
				if err == nil && kubernetes && linux {
					_, _ = nodeArchive(binary, build.release())
				}
			}()
		}
	}
}

func (rc *RunContext) node(ctx context.Context) string {
	job := rc.topLevelRunContext()
	if job.nodePath != "" {
		return job.nodePath
	}
	node := job.provisionNode(ctx)
	if ctx.Err() == nil { // a cancelled first use must not pin the fallback on later steps
		job.nodePath = node
	}
	return node
}

func (rc *RunContext) provisionNode(ctx context.Context) string {
	_, host := rc.JobContainer.(*container.HostEnvironment)
	seeder, seeds := rc.JobContainer.(interface {
		SeedVolume(volume, prefix, marker string, write func(io.Writer) error) common.Executor
	})
	extractor, extracts := rc.JobContainer.(interface {
		ExtractTarGz(destPath, archive string) common.Executor
	})
	if rc.Config.NoNodeDownload || common.Dryrun(ctx) || !host && !seeds && !extracts {
		return "node"
	}
	cache, dir := filepath.Join(rc.ActionCacheDir(), "node"), path.Join(rc.JobContainer.GetActPath(), "node")
	reason := errors.New("no build matches the job's platform")
	for _, build := range nodeBuildsFor(rc.Config.NodeVersion) {
		if !rc.nodeFits(ctx, host, build) {
			continue
		}
		release := build.release()
		node := path.Join(dir, release, "bin", "node")
		if host {
			if node, reason = downloadNode(ctx, cache, build); reason != nil {
				continue
			}
		}
		probe := rc.JobContainer.Exec([]string{node, "--version"}, map[string]string{"NODE_OPTIONS": ""}, "", "")
		install := probe
		switch {
		case seeds:
			install = func(ctx context.Context) error {
				if probe(ctx) == nil {
					return nil
				}
				binary, err := downloadNode(ctx, cache, build)
				if err != nil {
					return err
				}
				select {
				case nodeSeeding <- struct{}{}:
					defer func() { <-nodeSeeding }()
				case <-ctx.Done():
					return ctx.Err()
				}
				return seeder.SeedVolume(rc.Config.nodeVolume(), nodeVolumePrefix, path.Join(release, nodeComplete), func(out io.Writer) error { return writeNodeTar(out, binary, release) }).Then(probe)(ctx)
			}
		case extracts:
			install = func(ctx context.Context) error {
				binary, err := downloadNode(ctx, cache, build)
				if err != nil {
					return err
				}
				archive, err := nodeArchive(binary, release)
				if err != nil {
					return err
				}
				return extractor.ExtractTarGz(dir, archive).Then(probe)(ctx)
			}
		}
		stdout, stderr := rc.JobContainer.ReplaceLogWriter(io.Discard, io.Discard)
		reason = install(ctx)
		rc.JobContainer.ReplaceLogWriter(stdout, stderr)
		if reason == nil {
			return node
		}
	}
	logging.Infof("%s: the runner's Node.js is unavailable: %v", rc, reason) // the job log gets no reason, it may hold host paths
	common.Logger(ctx).Infof("JavaScript actions use the node on PATH, the runner's Node.js is unavailable")
	return "node"
}

func (rc *RunContext) nodeFits(ctx context.Context, host bool, build nodeBuild) bool {
	if host {
		return nodeFitsHost(build)
	}
	return build.goos == "linux" && rc.containerHas(ctx, build.loader)
}

func (rc *RunContext) containerHas(ctx context.Context, file string) bool {
	archive, err := rc.JobContainer.GetContainerArchive(ctx, file)
	if err == nil {
		_ = archive.Close()
	}
	return err == nil
}

func nodeFitsHost(build nodeBuild) bool {
	_, err := os.Stat(build.loader)
	return build.goos == runtime.GOOS && build.goarch == runtime.GOARCH && (build.loader == "" || err == nil)
}

func downloadNode(ctx context.Context, dir string, build nodeBuild) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	pruneNode(dir)
	version := filepath.Join(dir, build.release())
	binary := filepath.Join(version, "bin", "node")
	if build.goos == "windows" {
		binary += ".exe"
	}
	if _, err := os.Stat(binary); err == nil {
		now := time.Now()
		_ = os.Chtimes(version, now, now)
		return binary, nil
	}
	failed, retry := nodeFailures.Load(binary)
	if failedAt, _ := failed.(time.Time); time.Since(failedAt) < 10*time.Minute {
		return "", errors.New("its download failed recently")
	}
	download := nodeDownloads.DoChan(binary, func() (any, error) {
		if _, err := os.Stat(binary); err == nil {
			return binary, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := fetchNode(ctx, dir, binary, build); err != nil {
			nodeFailures.Store(binary, time.Now())
			logging.Warnf("downloading Node.js %s for JavaScript actions failed: %v", build.version, err)
			return nil, err
		}
		return binary, nil
	})
	if retry { // jobs wait only for a first download and at most a minute, so a slow or failing mirror holds up no later job
		return "", errors.New("its download failed before")
	}
	select {
	case result := <-download:
		return binary, result.Err
	case <-time.After(time.Minute):
		return "", errors.New("still downloading")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// pruneNode removes other versions no runner used for a week, and downloads abandoned for an hour.
func pruneNode(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || slices.ContainsFunc(nodeBuilds, func(build nodeBuild) bool { return build.release() == entry.Name() }) {
			continue
		}
		age := time.Since(info.ModTime())
		if strings.HasPrefix(entry.Name(), "node-v") && age > 7*24*time.Hour || strings.HasPrefix(entry.Name(), ".node-") && age > time.Hour {
			_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
		}
	}
}

func nodeBase(build nodeBuild) string {
	return strings.TrimSuffix(cmp.Or(os.Getenv("NODEJS_ORG_MIRROR"), "https://nodejs.org/dist"), "/") + "/v" + build.version + "/"
}

func nodeSource(build nodeBuild) (url, entry string) {
	name := build.release() + ".tar.gz"
	switch {
	case build.name == "alpine-x64": // nodejs.org's musl build needs libstdc++, GitHub's links it statically
		return "https://github.com/actions/alpine_nodejs/releases/download/v" + build.version + "/" + name, "bin/node"
	case build.goos == "windows":
		return nodeBase(build) + build.name + "/node.exe", ""
	default:
		return nodeBase(build) + name, build.release() + "/bin/node"
	}
}

// nodeSum returns the pinned SHA-256 of build, or for a runner.node_version override the one published beside it.
func nodeSum(ctx context.Context, build nodeBuild, url string) (string, error) {
	if build.sha256 != "" {
		return build.sha256, nil
	}
	if build.name == "alpine-x64" {
		body, err := nodeGet(ctx, "https://api.github.com/repos/actions/alpine_nodejs/releases/tags/v"+build.version)
		if err != nil {
			return "", err
		}
		defer body.Close()
		var release struct {
			Assets []struct {
				Name   string `json:"name"`
				Digest string `json:"digest"`
			} `json:"assets"`
		}
		if err := json.UnmarshalRead(body, &release); err != nil {
			return "", err
		}
		for _, asset := range release.Assets {
			if sum, ok := strings.CutPrefix(asset.Digest, "sha256:"); ok && asset.Name == path.Base(url) {
				return sum, nil
			}
		}
	} else {
		body, err := nodeGet(ctx, nodeBase(build)+"SHASUMS256.txt")
		if err != nil {
			return "", err
		}
		defer body.Close()
		sums, err := io.ReadAll(body)
		if err != nil {
			return "", err
		}
		for line := range strings.Lines(string(sums)) {
			if sum, ok := strings.CutSuffix(strings.TrimSpace(line), "  "+strings.TrimPrefix(url, nodeBase(build))); ok {
				return sum, nil
			}
		}
	}
	return "", fmt.Errorf("no SHA-256 is published for %s", path.Base(url))
}

func fetchNode(ctx context.Context, dir, binary string, build nodeBuild) error {
	url, entry := nodeSource(build)
	sum, err := nodeSum(ctx, build, url)
	if err != nil {
		return err
	}
	body, err := nodeGet(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".node-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o755); err != nil {
		return err
	}
	hash := sha256.New()
	node := io.TeeReader(body, hash)
	if entry != "" {
		if node, err = gzipTarEntry(node, entry); err != nil {
			return err
		}
	}
	if _, err := io.Copy(tmp, node); err != nil {
		return err
	}
	if _, err := io.Copy(hash, body); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sum {
		return fmt.Errorf("%s has checksum %s, %s is expected", build.release(), got, sum)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), binary); err != nil {
		if _, statErr := os.Stat(binary); statErr != nil { // Windows cannot replace a node.exe another runner installed and runs
			return err
		}
	}
	return nil
}

// nodeArchive compresses the binary once for all Kubernetes jobs, which extract it in their pods.
func nodeArchive(binary, release string) (string, error) {
	archive := binary + ".tar.gz"
	_, err, _ := nodeDownloads.Do(archive, func() (any, error) {
		if _, err := os.Stat(archive); err == nil {
			return archive, nil
		}
		tmp, err := os.CreateTemp(filepath.Dir(binary), ".node-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		compressed := gzip.NewWriter(tmp)
		if err := errors.Join(tmp.Chmod(0o644), writeNodeTar(compressed, binary, release), compressed.Close(), tmp.Close()); err != nil {
			return nil, err
		}
		return archive, os.Rename(tmp.Name(), archive)
	})
	return archive, err
}

func gzipTarEntry(archive io.Reader, name string) (io.Reader, error) {
	unzipped, err := gzip.NewReader(archive)
	if err != nil {
		return nil, err
	}
	files := tar.NewReader(unzipped)
	for {
		header, err := files.Next()
		if err != nil {
			return nil, fmt.Errorf("finding %s: %w", name, err)
		}
		if strings.TrimPrefix(header.Name, "./") == name && header.Typeflag == tar.TypeReg {
			return files, nil
		}
	}
}

func writeNodeTar(out io.Writer, binary, release string) error {
	file, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	writer := tar.NewWriter(out)
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: release + "/bin/node", Mode: 0o755, Size: info.Size()}); err != nil {
		return err
	}
	if _, err := io.Copy(writer, file); err != nil {
		return err
	}
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: release + "/" + nodeComplete, Mode: 0o644}); err != nil {
		return err
	}
	return writer.Close()
}

func nodeGet(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", req.URL.Redacted(), resp.Status)
	}
	return resp.Body, nil
}
