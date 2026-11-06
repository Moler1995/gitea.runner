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

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/filecollector"
)

// TarWriter takes names relative to the destination, and owns every entry by the user the
// container extracts as, like the other copies do.
type TarWriter struct {
	*tar.Writer
	uid, gid int
	prefix   string
}

func (w *TarWriter) WriteHeader(hdr *tar.Header) error {
	hdr.Uid, hdr.Gid = w.uid, w.gid
	if w.prefix != "" {
		dir := strings.HasSuffix(hdr.Name, "/")
		hdr.Name = path.Join(w.prefix, hdr.Name)
		if dir {
			hdr.Name += "/"
		}
	}
	return w.Writer.WriteHeader(hdr)
}

// writeTarStream writes parents first with their names as given, then what write produces.
func writeTarStream(w io.Writer, uid, gid int, prefix string, parents []string, write func(*TarWriter) error) error {
	tw := &TarWriter{Writer: tar.NewWriter(w), uid: uid, gid: gid}
	for _, name := range parents {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755}); err != nil {
			return err
		}
	}
	tw.prefix = prefix
	if err := write(tw); err != nil {
		return err
	}
	return tw.Close()
}

func pipeTar(uid, gid int, prefix string, parents []string, write func(*TarWriter) error) *io.PipeReader {
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(writeTarStream(writer, uid, gid, prefix, parents, write)) }()
	return reader
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
