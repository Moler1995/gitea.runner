// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package uploadartifact

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/glob"

	cerrdefs "github.com/containerd/errdefs"
)

// found is a file the patterns select, symlinks resolved to their target's content and mode.
type found struct {
	path  string
	mode  fs.FileMode
	mtime time.Time
	body  io.Reader
}

// maxFollow bounds how deep symlinked directories nest, standing in for the realpath cycle check
// where a path cannot be resolved.
const maxFollow = 32

// walker visits what @actions/glob yields with the options upload-artifact uses: symlinks
// followed, broken ones omitted, hidden entries skipped unless included, files only.
type walker struct {
	g             *glob.Globber
	includeHidden bool
	visit         func(found) error
}

// selected reports how a visited entry matches, and whether it or its descendants may match.
func (w *walker) selected(p string) (glob.MatchKind, bool) {
	match := w.g.Match(p)
	return match, match != glob.MatchNone || w.g.PartialMatch(p)
}

func (w *walker) hidden(base string) bool {
	return !w.includeHidden && strings.HasPrefix(base, ".")
}

// walkHost walks the host filesystem the way the toolkit's globber does.
func (w *walker) walkHost(roots []string) error {
	for _, root := range roots {
		if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := w.hostItem(root, nil); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) hostItem(p string, chain []string) error {
	match, selected := w.selected(p)
	if !selected {
		return nil
	}
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // broken symlink
	} else if err != nil {
		return err
	}
	if w.hidden(filepath.Base(p)) {
		return nil
	}
	if !info.IsDir() {
		if match&glob.MatchFile == 0 {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return w.visit(found{path: p, mode: info.Mode(), mtime: info.ModTime(), body: f})
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	if slices.Contains(chain, resolved) {
		return nil // symlink cycle
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return err
	}
	chain = append(chain, resolved)
	for _, entry := range entries {
		if err := w.hostItem(filepath.Join(p, entry.Name()), chain); err != nil {
			return err
		}
	}
	return nil
}

// walkContainer reads each search root as an archive. The archive holds symlinks as links, so
// each one selected is followed afterwards by archiving its target.
func (w *walker) walkContainer(ctx context.Context, c *action.Context, roots []string) error {
	for _, root := range roots {
		if err := w.containerTree(ctx, c, root, root, nil, 0); err != nil {
			return err
		}
	}
	return nil
}

type link struct {
	virtual, target string
	chain           []string
}

// missing reports a path the backend found nothing at: a not-found error from Docker, or the
// failing `test -e` that Kubernetes archives start with.
func missing(err error) bool {
	var exit container.ExitCodeError
	return cerrdefs.IsNotFound(err) || errors.Is(err, fs.ErrNotExist) || errors.As(err, &exit)
}

// containerTree walks the archive of source as if it were found at virtual. chain holds the real
// directories already descended into, to tell a symlink cycle.
func (w *walker) containerTree(ctx context.Context, c *action.Context, virtual, source string, chain []string, depth int) error {
	archive, err := c.Container.GetContainerArchive(ctx, source)
	if missing(err) {
		return nil // a search path that does not exist, or a broken symlink
	} else if err != nil {
		return err
	}
	defer archive.Close()
	var links []link
	descended := map[string][]string{} // the chain below each directory the walk enters
	reader := tar.NewReader(archive)
	for {
		hdr, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		entryVirtual := virtual + strings.TrimPrefix(name, path.Base(source))
		entryChain := chain
		if entryVirtual != virtual {
			var entered bool
			if entryChain, entered = descended[c.Dir(entryVirtual)]; !entered {
				continue
			}
		}
		match, selected := w.selected(entryVirtual)
		if !selected || w.hidden(c.Base(entryVirtual)) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			target := hdr.Linkname
			if !path.IsAbs(target) {
				target = path.Join(path.Dir(path.Join(path.Dir(source), name)), target)
			}
			links = append(links, link{virtual: entryVirtual, target: target, chain: entryChain})
		case tar.TypeLink:
			if match&glob.MatchFile != 0 {
				links = append(links, link{virtual: entryVirtual, target: path.Join(path.Dir(source), hdr.Linkname), chain: entryChain})
			}
		case tar.TypeDir:
			if dirReal := path.Join(path.Dir(source), name); !slices.Contains(entryChain, dirReal) { // else a symlink cycle
				descended[entryVirtual] = append(slices.Clip(entryChain), dirReal)
			}
		case tar.TypeReg:
			if match&glob.MatchFile != 0 {
				if err := w.visit(found{path: entryVirtual, mode: hdr.FileInfo().Mode(), mtime: hdr.ModTime, body: reader}); err != nil {
					return err
				}
			}
		}
	}
	if _, err := io.Copy(io.Discard, archive); err != nil { // the exit status of a Kubernetes tar arrives after its archive
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	for _, l := range links {
		if depth >= maxFollow || slices.Contains(l.chain, l.target) {
			continue // a symlink cycle
		}
		if err := w.containerTree(ctx, c, l.virtual, l.target, l.chain, depth+1); err != nil {
			return err
		}
	}
	return nil
}
