// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"
)

// KubernetesTransfer has job pods fetch the files the runner copies into them from the runner over the pod network,
// instead of the runner streaming them through the API server, which costs cross-zone traffic and API server load.
type KubernetesTransfer struct {
	Host   string // address job pods reach the runner at, empty uses the outbound IP
	Port   uint16 // 0 picks a free port
	Image  string // image an init container copies Binary from into each job pod
	Binary string // path of the runner binary in Image
	// Fallback copies through the API server for a job pod that cannot reach the runner, else such a copy, or a pod the transfer cannot be set up for, fails.
	Fallback bool
}

// KubernetesTransferCommand is the hidden runner subcommand job pods run the transfer helper with.
const KubernetesTransferCommand = "kubernetes-transfer"

const transferPath = "/transfer/"

type transferServer struct {
	base    string
	mu      sync.Mutex
	pending map[string]func(io.Writer) error
}

var (
	transferServersMu sync.Mutex
	transferServers   = map[string]*transferServer{}
)

// startTransferServer returns the runner's server for these settings, started on first use and shared by every job pod.
func startTransferServer(host string, port uint16) (*transferServer, error) {
	transferServersMu.Lock()
	defer transferServersMu.Unlock()
	key := net.JoinHostPort(host, strconv.Itoa(int(port)))
	if server, ok := transferServers[key]; ok {
		return server, nil
	}
	if host == "" {
		ip := common.GetOutboundIP()
		if ip == nil {
			return nil, errors.New("no outbound IP for job pods to reach the runner at, set kubernetes.transfer.host")
		}
		host = ip.String()
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(int(port))))
	if err != nil {
		return nil, fmt.Errorf("listen for job pod transfers: %w", err)
	}
	_, listenPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		listener.Close()
		return nil, err
	}
	server := &transferServer{
		base:    "http://" + net.JoinHostPort(host, listenPort) + transferPath,
		pending: map[string]func(io.Writer) error{},
	}
	go func() { _ = (&http.Server{Handler: server, ReadHeaderTimeout: 10 * time.Second}).Serve(listener) }()
	transferServers[key] = server
	return server, nil
}

// offer serves write once at the returned URL, until done is called.
func (s *transferServer) offer(write func(io.Writer) error) (url string, done func()) {
	token := rand.Text()
	s.mu.Lock()
	s.pending[token] = write
	s.mu.Unlock()
	return s.base + token, func() {
		s.mu.Lock()
		delete(s.pending, token)
		s.mu.Unlock()
	}
}

func (s *transferServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.URL.Path, transferPath)
	if r.Method != http.MethodGet || !ok {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	write := s.pending[token]
	delete(s.pending, token) // one fetch per offer, a token seen on the wire cannot be replayed
	s.mu.Unlock()
	if write == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := write(w); err != nil {
		panic(http.ErrAbortHandler) // drop the connection so the helper sees a truncated body, not a short tar
	}
}

// FetchKubernetesTransfer is the job pod side: it extracts the tar stream at url into dest, as the API server path's `tar -xf -` does.
func FetchKubernetesTransfer(ctx context.Context, url, dest string) error {
	client := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext, // fail fast so the runner falls back to the API server
		ResponseHeaderTimeout: time.Minute,
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch: %s", response.Status)
	}
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	body := &errorReader{reader: response.Body}
	cmd := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", dest)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = body, os.Stderr, os.Stderr
	return errors.Join(body.err, cmd.Run())
}

// errorReader keeps the body's read error, which tar may hide by stopping at a block boundary.
type errorReader struct {
	reader io.Reader
	err    error
}

func (r *errorReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

// InstallKubernetesTransferHelper copies the running binary into dir, run by the job pod's init container.
func InstallKubernetesTransferHelper(dir string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target, err := os.OpenFile(filepath.Join(dir, filepath.Base(executable)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(target, source)
	return errors.Join(err, target.Close())
}

// setupTransfer turns on transfers for the pod, defaulting the image to the runner pod's only one.
// When that is not possible it leaves copies to the API server, or fails without Fallback.
func (p *KubernetesPod) setupTransfer(ctx context.Context, runnerImages []string) error {
	settings := p.options.Transfer
	if settings == nil {
		return nil
	}
	image := settings.Image
	if image == "" && len(runnerImages) == 1 {
		image = runnerImages[0]
	}
	var err error
	var server *transferServer
	if image == "" || settings.Binary == "" {
		err = fmt.Errorf("set kubernetes.transfer.image, the runner pod has %d containers to take the runner binary from", len(runnerImages))
	} else {
		server, err = startTransferServer(settings.Host, settings.Port)
	}
	if err != nil {
		if !settings.Fallback {
			return fmt.Errorf("kubernetes.transfer: %w", err)
		}
		common.Logger(ctx).Warnf("Job pod copies go through the API server: %v", err)
		return nil
	}
	p.transfer, p.transferImage = server, image
	p.transferHelper = path.Join(p.job.GetActPath(), "bin", path.Base(settings.Binary))
	return nil
}
