// Copyright 2026 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"context"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/filecollector"
)

// TarWriter takes names relative to the destination, owns entries by the user the container extracts as, and writes missing parents first.
type TarWriter struct {
	*tar.Writer
	uid, gid int
	prefix   string
	dirs     map[string]bool
}

func (w *TarWriter) WriteHeader(hdr *tar.Header) error {
	name := strings.TrimSuffix(hdr.Name, "/")
	hdr.Name = path.Join(w.prefix, name) + hdr.Name[len(name):]
	return w.writeHeader(hdr)
}

func (w *TarWriter) writeHeader(hdr *tar.Header) error {
	name := strings.TrimSuffix(hdr.Name, "/")
	if dir := path.Dir(name); dir != "." && dir != "/" && !w.dirs[dir] { // else the daemon creates it as root
		if err := w.writeHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755, ModTime: time.Now()}); err != nil {
			return err
		}
	}
	if hdr.Typeflag == tar.TypeDir {
		w.dirs[name] = true
	}
	hdr.Uid, hdr.Gid = w.uid, w.gid
	return w.Writer.WriteHeader(hdr)
}

func writeTarStream(w io.Writer, uid, gid int, prefix string, write func(*TarWriter) error) error {
	tw := &TarWriter{Writer: tar.NewWriter(w), uid: uid, gid: gid, prefix: prefix, dirs: map[string]bool{}}
	if prefix != "" {
		if err := tw.writeHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix + "/", Mode: 0o755, ModTime: time.Now()}); err != nil {
			return err
		}
	}
	if err := write(tw); err != nil {
		return err
	}
	return tw.Close()
}

func writeFilesTar(ctx context.Context, w io.Writer, uid, gid int, files ...*FileEntry) error {
	tw := tar.NewWriter(w)
	for _, file := range files {
		common.Logger(ctx).Debugf("Writing entry to tarball %s len:%d", file.Name, len(file.Body))
		if err := tw.WriteHeader(&tar.Header{
			Name: file.Name,
			Mode: file.Mode,
			Size: int64(len(file.Body)),
			Uid:  uid,
			Gid:  gid,
		}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(file.Body)); err != nil {
			return err
		}
	}
	return tw.Close()
}

// writeDirTar prefixes entries with dstPath for extraction at root.
func writeDirTar(ctx context.Context, w io.Writer, dstPath, srcPath string, useGitIgnore, skipGitDir bool, uid, gid int, executable bool) error {
	logger := common.Logger(ctx)
	tw := tar.NewWriter(w)
	srcPrefix := strings.TrimSuffix(filepath.Dir(srcPath), string(filepath.Separator)) + string(filepath.Separator)
	logger.Debugf("Stripping prefix:%s src:%s", srcPrefix, srcPath)

	if err := filepath.Walk(srcPath, (&filecollector.FileCollector{
		SrcPath:      srcPath,
		SrcPrefix:    srcPrefix,
		SkipGitDir:   skipGitDir,
		UseGitIgnore: useGitIgnore,
		Handler:      &filecollector.TarCollector{TarWriter: tw, UID: uid, GID: gid, DstDir: dstPath[1:], Executable: executable},
	}).CollectFiles(ctx)); err != nil {
		return err
	}
	return tw.Close()
}
