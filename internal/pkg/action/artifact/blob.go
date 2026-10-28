// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// BlockSize is the toolkit's upload chunk.
const BlockSize = 8 << 20

// Uploaded is what was uploaded, the digest in the `sha256:<hex>` form finalize takes.
type Uploaded struct {
	Size   int64
	Digest string
}

// Upload puts body to a signed URL with the Azure block protocol Gitea also speaks: blocks go up
// in parallel, and the block list puts them in order. A block that stalls longer than stall fails
// the upload.
func (c *Client) Upload(ctx context.Context, signedURL, contentType string, body io.Reader, concurrency int, stall time.Duration) (Uploaded, error) {
	hash := sha256.New()
	reader := io.TeeReader(body, hash)
	group, blockCtx := errgroup.WithContext(ctx)
	buffers := make(chan []byte, concurrency)
	prefix := rand.Text()
	var ids []string
	var uploaded atomic.Int64
	for index := 0; ; index++ {
		var buf []byte
		if index < concurrency {
			buf = make([]byte, BlockSize)
		} else {
			select {
			case buf = <-buffers:
			case <-blockCtx.Done():
				return Uploaded{}, group.Wait()
			}
		}
		n, readErr := io.ReadFull(reader, buf)
		if errors.Is(readErr, io.EOF) && index > 0 {
			break
		} else if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
			_ = group.Wait()
			return Uploaded{}, readErr
		}
		id := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%s-%08d", prefix, index)) // fixed width as Azure needs, unique as Gitea before 1.25.5 shares a run's blocks
		ids = append(ids, id)
		group.Go(func() error {
			defer func() { buffers <- buf }()
			if err := c.putBlock(blockCtx, signedURL, id, buf[:n], stall); err != nil {
				return err
			}
			c.Logf("Uploaded bytes %d", uploaded.Add(int64(n)))
			return nil
		})
		if readErr != nil {
			break
		}
	}
	if err := group.Wait(); err != nil {
		return Uploaded{}, err
	}
	if err := c.putBlockList(ctx, signedURL, contentType, ids); err != nil {
		return Uploaded{}, err
	}
	return Uploaded{Size: uploaded.Load(), Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil))}, nil
}

func (c *Client) putBlock(ctx context.Context, signedURL, id string, block []byte, stall time.Duration) error {
	resp, err := c.Retry.do(ctx, c.Logf, "block upload", func() (*http.Response, error) {
		attemptCtx, cancel := context.WithTimeoutCause(ctx, stall, errors.New("upload progress stalled"))
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodPut, signedURL+"&comp=block&blockid="+url.QueryEscape(id), bytes.NewReader(block))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("x-ms-blob-type", "BlockBlob")
		resp, err := c.blob.Do(req)
		if err != nil {
			cancel()
			return nil, scrub(err)
		}
		resp.Body = cancelOnClose{resp.Body, cancel}
		return resp, nil
	})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *Client) putBlockList(ctx context.Context, signedURL, contentType string, ids []string) error {
	var list strings.Builder
	list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, id := range ids {
		list.WriteString("<Latest>" + id + "</Latest>")
	}
	list.WriteString("</BlockList>")
	resp, err := c.Retry.do(ctx, c.Logf, "block list upload", func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, signedURL+"&comp=blocklist", strings.NewReader(list.String()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-ms-blob-content-type", contentType)
		resp, err := c.blob.Do(req)
		return resp, scrub(err)
	})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Download GETs a signed URL. A body that sends nothing for stall fails the read.
func (c *Client) Download(ctx context.Context, signedURL string, stall time.Duration) (*http.Response, error) {
	return download(ctx, c.blob, signedURL, stall)
}

func download(ctx context.Context, client *http.Client, signedURL string, stall time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		cancel(nil)
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel(nil)
		return nil, scrub(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel(nil)
		return nil, fmt.Errorf("unexpected HTTP response from blob storage: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	resp.Body = &stallReader{body: resp.Body, timer: time.AfterFunc(stall, func() {
		cancel(fmt.Errorf("blob storage chunk did not respond in %s", stall))
	}), stall: stall, cancel: cancel, ctx: ctx}
	return resp, nil
}

func scrub(err error) error { // the signature of a signed URL is a credential
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		urlErr.URL = ScrubQuery(urlErr.URL)
	}
	return err
}

type stallReader struct {
	body   io.ReadCloser
	timer  *time.Timer
	stall  time.Duration
	cancel context.CancelCauseFunc
	ctx    context.Context
}

func (r *stallReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.timer.Reset(r.stall)
	}
	if err != nil && !errors.Is(err, io.EOF) && context.Cause(r.ctx) != nil {
		err = context.Cause(r.ctx)
	}
	return n, err
}

func (r *stallReader) Close() error {
	r.timer.Stop()
	r.cancel(nil)
	return r.body.Close()
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}
