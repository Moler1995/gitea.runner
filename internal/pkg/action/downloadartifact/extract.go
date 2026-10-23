// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package downloadartifact

import (
	"archive/tar"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"

	"github.com/bmatcuk/doublestar/v4"
)

// spooled is a download on the runner's disk, as a zip can only be read with random access, and
// the toolkit's zips carry data descriptors even on stored entries.
type spooled struct {
	file   *os.File
	size   int64
	digest string
}

func spool(body io.Reader) (*spooled, error) {
	file, err := os.CreateTemp("", "artifact-*")
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), body)
	if err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	return &spooled{file: file, size: size, digest: "sha256:" + hex.EncodeToString(hash.Sum(nil))}, nil
}

func (s *spooled) remove() {
	s.file.Close()
	os.Remove(s.file.Name())
}

var zipTypes = []string{"application/zip", "application/x-zip-compressed", "application/zip-compressed"}

func isZip(resp *http.Response) bool {
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return slices.Contains(zipTypes, mediaType) || strings.HasSuffix(strings.ToLower(resp.Request.URL.Path), ".zip")
}

// rawFileName prefers filename*, as the toolkit does, and keeps only the base name.
func rawFileName(disposition string) string {
	_, params, err := mime.ParseMediaType(disposition)
	name := params["filename"] // filename* arrives decoded in here
	if err != nil || name == "" {
		return "artifact"
	}
	if unescaped, err := url.PathUnescape(name); err == nil && !strings.Contains(strings.ToLower(disposition), "filename*") {
		name = unescaped
	}
	if name = path.Base(name); name == "/" || name == "." {
		return "artifact"
	}
	return name
}

// dotdot is unzip-stream's rewrite of every run of dots that makes up a path segment.
var dotdot = regexp.MustCompile(`(^|[/\\])\.\.+([/\\]|$)`)

// entryName is where unzip-stream writes an entry: below the destination, with `..` segments
// turned into `.`, and on Windows with either separator.
func entryName(name string, windows bool) string {
	for dotdot.MatchString(name) {
		name = dotdot.ReplaceAllString(name, "$1.$2")
	}
	if windows {
		name = strings.ReplaceAll(name, `\`, "/")
	}
	return strings.TrimPrefix(path.Clean("/"+name), "/")
}

// extractZip writes the entries as unzip-stream does: files with the default mode and the
// current time, symlinks as files holding their target, directories created.
func extractZip(ctx context.Context, c *action.Context, s *spooled, target string) error {
	reader, err := zip.NewReader(s.file, s.size)
	if err != nil {
		return err
	}
	now := time.Now()
	return c.Container.CopyTarStream(ctx, target, func(tw *container.TarWriter) error {
		for _, f := range reader.File {
			name := entryName(f.Name, c.Windows())
			if name == "" {
				continue
			}
			if f.UncompressedSize64 == 0 && strings.ContainsAny(f.Name[len(f.Name)-1:], `/\`) {
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: now}); err != nil {
					return err
				}
				continue
			}
			if err := writeEntry(tw, f, name, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeEntry(tw *container.TarWriter, f *zip.File, name string, now time.Time) error {
	body, err := f.Open()
	if err != nil {
		return err
	}
	defer body.Close()
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(f.UncompressedSize64), ModTime: now}); err != nil {
		return err
	}
	_, err = io.Copy(tw, body)
	return err
}

func writeRaw(ctx context.Context, c *action.Context, s *spooled, target, fileName string) error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return c.Container.CopyTarStream(ctx, target, func(tw *container.TarWriter) error {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: fileName, Mode: 0o644, Size: s.size, ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := io.Copy(tw, s.file)
		return err
	})
}

// matchName matches an artifact name the way minimatch does with its default options: a leading
// `!` negates, a `#` pattern matches nothing, and a wildcard never matches a leading dot.
func matchName(pattern, name string) bool {
	if strings.HasPrefix(pattern, "#") {
		return false
	}
	negate := false
	for strings.HasPrefix(pattern, "!") {
		negate, pattern = !negate, pattern[1:]
	}
	matched, err := doublestar.Match(pattern, name)
	if err != nil {
		matched = pattern == name
	}
	if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
		matched = false
	}
	return matched != negate
}
