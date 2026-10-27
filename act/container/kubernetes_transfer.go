// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"
)

// KubernetesTransfer has job pods fetch the files the runner copies into them from the runner over the pod network,
// instead of the runner streaming them through the API server, which costs cross-zone traffic and API server load.
type KubernetesTransfer struct {
	Host   string // address the runner listens on and job pods reach it at, empty uses the outbound IP
	Port   uint16 // 0 picks a free port
	Image  string // image an init container copies Binary from into each job pod
	Binary string // path of the runner binary in Image
	// Fallback copies through the API server for a job pod that cannot reach the runner, else such a copy, or a pod the transfer cannot be set up for, fails.
	Fallback bool
}

// KubernetesTransferCommand is the hidden runner subcommand job pods run the transfer helper with.
const KubernetesTransferCommand = "kubernetes-transfer"

const transferPath = "/transfer/"

// transferRequest is what the runner hands the helper over the exec stream, which the API server protects with TLS,
// so neither the token nor the pin shows in the exec command line or the API server audit log.
type transferRequest struct {
	URL string `json:"url"`
	Pin string `json:"pin"` // hex SHA-256 of the runner's certificate, the only one the helper trusts
}

// transferServer serves each copy once over TLS with a certificate generated at start, to the job pod it was offered to.
type transferServer struct {
	base    string
	pin     string
	mu      sync.Mutex
	pending map[string]transferOffer
}

type transferOffer struct {
	clients []net.IP // IPs of the job pod, the only ones allowed to fetch
	write   func(io.Writer) error
}

func (o transferOffer) allows(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	remote := net.ParseIP(host)
	return err == nil && remote != nil && slices.ContainsFunc(o.clients, remote.Equal)
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
	certificate, pin, err := transferCertificate(host)
	if err != nil {
		return nil, err
	}
	// Bound to the address job pods use, not every interface, so a host-network or out-of-cluster runner does not expose it further.
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return nil, fmt.Errorf("listen for job pod transfers: %w", err)
	}
	server := &transferServer{
		base:    "https://" + listener.Addr().String() + transferPath,
		pin:     pin,
		pending: map[string]transferOffer{},
	}
	httpServer := &http.Server{
		Handler:           server,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 10 * time.Second, // requests are bodiless; no read or write timeout, a copy takes as long as it takes
		IdleTimeout:       30 * time.Second,
	}
	go func() { _ = httpServer.ServeTLS(listener, "", "") }()
	transferServers[key] = server
	return server, nil
}

// transferCertificate generates the server's self-signed certificate, trusted by its pin alone, so no CA is involved.
func transferCertificate(host string) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gitea-runner transfer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0), // lives as long as the runner process
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:]), nil
}

// offer serves write once to one of clients, the job pod's IPs, until done is called. It returns what the helper needs to fetch it.
func (s *transferServer) offer(clients []string, write func(io.Writer) error) (request transferRequest, done func()) {
	token := rand.Text()
	offer := transferOffer{write: write}
	for _, client := range clients {
		if ip := net.ParseIP(client); ip != nil {
			offer.clients = append(offer.clients, ip)
		}
	}
	s.mu.Lock()
	s.pending[token] = offer
	s.mu.Unlock()
	return transferRequest{URL: s.base + token, Pin: s.pin}, func() {
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
	offer, ok := s.pending[token]
	// Only the job pod it was offered to may take it, and without consuming it otherwise, so another pod can neither read nor block the copy.
	ok = ok && offer.allows(r.RemoteAddr)
	if ok {
		delete(s.pending, token) // one fetch per offer
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := offer.write(w); err != nil {
		panic(http.ErrAbortHandler) // drop the connection so the helper sees a truncated body, not a short tar
	}
}

// FetchKubernetesTransfer is the job pod side: it reads a transferRequest from request,
// and extracts the tar stream it points to into dest, as the API server path's `tar -xf -` does.
func FetchKubernetesTransfer(ctx context.Context, request io.Reader, dest string) error {
	var transfer transferRequest
	if err := json.NewDecoder(request).Decode(&transfer); err != nil {
		return fmt.Errorf("read transfer request: %w", err)
	}
	if transfer.Pin == "" {
		return errors.New("transfer request has no certificate pin")
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext, // fail fast, auto mode then uses the API server
		ResponseHeaderTimeout: time.Minute,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// The certificate is self-signed and verified against the pin below instead of a CA.
			InsecureSkipVerify: true, //nolint:gosec // VerifyConnection pins the certificate
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("runner sent no certificate")
				}
				sum := sha256.Sum256(state.PeerCertificates[0].Raw)
				if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(transfer.Pin)) != 1 {
					return errors.New("runner certificate does not match the pin")
				}
				return nil
			},
		},
	}}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, transfer.URL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(httpRequest)
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
