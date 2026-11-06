// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package uploadartifact works like actions/upload-artifact v7.
package uploadartifact

import (
	"archive/zip"
	"cmp"
	"compress/flate"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.com/gitea/runner/internal/pkg/action"
	"gitea.com/gitea/runner/internal/pkg/action/artifact"
	"gitea.com/gitea/runner/internal/pkg/action/glob"
)

var noFileBehaviors = []string{"warn", "error", "ignore"}

type inputs struct {
	name, path, ifNoFilesFound          string
	retentionDays, compressionLevel     int
	overwrite, includeHidden, archiveIt bool
}

func Main(ctx context.Context, c *action.Context) error {
	c.ApplyDefaults("name", "artifact", "path", "", "if-no-files-found", "warn", "retention-days", "", "compression-level", "6",
		"overwrite", "false", "include-hidden-files", "false", "archive", "true")
	in, err := readInputs(c)
	if err != nil {
		return err
	}
	g, err := glob.Parse(in.path, glob.Options{
		Workdir:             c.Workspace,
		Home:                func() (string, error) { return c.Home(ctx) },
		Windows:             c.Windows(),
		ImplicitDescendants: true,
	})
	if err != nil {
		return err
	}
	spool, err := os.CreateTemp("", "artifact-*")
	if err != nil {
		return err
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()
	collected, err := collect(ctx, c, g, in, spool)
	if err != nil {
		return err
	}
	if collected.files == 0 {
		message := fmt.Sprintf("No files were found with the provided path: %s. No artifacts will be uploaded.", in.path)
		switch in.ifNoFilesFound {
		case "warn":
			c.Warning("%s", message)
		case "error":
			return action.Failf("%s", message)
		default:
			c.Infof("%s", message)
		}
		return nil
	}
	plural := "s"
	if collected.files == 1 {
		plural = ""
	}
	c.Infof("With the provided path, there will be %d file%s uploaded", collected.files, plural)
	if !in.archiveIt && collected.files > 1 {
		return action.Failf("When 'archive' is set to false, only a single file can be uploaded. Found %d files to upload.", collected.files)
	}
	return upload(ctx, c, in, collected, spool)
}

func readInputs(c *action.Context) (*inputs, error) {
	in := &inputs{name: c.Input("name"), path: c.Input("path"), ifNoFilesFound: c.Input("if-no-files-found"), compressionLevel: 6}
	if in.path == "" {
		return nil, action.Failf("Input required and not supplied: path")
	}
	var err error
	if in.overwrite, err = c.BoolInput("overwrite"); err != nil {
		return nil, err
	}
	if in.includeHidden, err = c.BoolInput("include-hidden-files"); err != nil {
		return nil, err
	}
	if in.archiveIt, err = c.BoolInput("archive"); err != nil {
		return nil, err
	}
	if !slices.Contains(noFileBehaviors, in.ifNoFilesFound) {
		return nil, action.Failf("Unrecognized if-no-files-found input. Provided: %s. Available options: %s", in.ifNoFilesFound, strings.Join(noFileBehaviors, ","))
	}
	if days := c.Input("retention-days"); days != "" {
		if in.retentionDays, err = action.ParseInt(days); err != nil {
			return nil, action.Failf("Invalid retention-days")
		}
	}
	if level := c.Input("compression-level"); level != "" {
		if in.compressionLevel, err = action.ParseInt(level); err != nil {
			return nil, action.Failf("Invalid compression-level")
		}
		if in.compressionLevel < 0 || in.compressionLevel > 9 {
			return nil, action.Failf("Invalid compression-level. Valid values are 0-9")
		}
	}
	return in, nil
}

type collected struct {
	files    int
	fileName string // the single file uploaded as is
}

// collect writes the selected files to spool, zipped or, without archiving, the first one as is.
func collect(ctx context.Context, c *action.Context, globber *glob.Globber, in *inputs, spool *os.File) (*collected, error) {
	roots := globber.SearchPaths()
	root := ""
	if len(roots) > 1 {
		c.Infof("Multiple search paths detected. Calculating the least common ancestor of all paths")
		root = glob.LeastCommonAncestor(roots, c.Windows())
		c.Infof("The least common ancestor is %s. This will be the root directory of the artifact", root)
	}
	result := &collected{}
	deflater, err := flate.NewWriter(nil, in.compressionLevel)
	if err != nil {
		return nil, err
	}
	zw := zip.NewWriter(spool)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		deflater.Reset(w)
		return deflater, nil
	})
	seen := map[string]bool{}
	files := &walker{g: globber, includeHidden: in.includeHidden, visit: func(file found) error {
		result.files++
		if lower := strings.ToLower(file.path); seen[lower] {
			c.Infof("Uploads are case insensitive: %s was detected that it will be overwritten by another file with the same path", file.path)
		} else {
			seen[lower] = true
		}
		if !in.archiveIt {
			if result.files == 1 {
				result.fileName = c.Base(file.path)
				_, err := io.Copy(spool, file.body)
				return err
			}
			return nil
		}
		base := root
		if base == "" {
			base = roots[0]
			if file.path == base { // a single file uploaded by its own path keeps no directory
				base = c.Dir(base)
			}
		}
		return addToZip(zw, in.compressionLevel, base, file)
	}}
	walk := func() error { return files.walkContainer(ctx, c, roots) }
	if c.Host() {
		walk = func() error { return files.walkHost(roots) }
	}
	if err := walk(); err != nil {
		return nil, err
	}
	if in.archiveIt {
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

type invalidChar struct{ char, label string }

// The toolkit's characters, with its labels, which NTFS cannot store.
var (
	invalidPathChars = []invalidChar{
		{`"`, ` Double quote "`},
		{":", " Colon :"},
		{"<", " Less than <"},
		{">", " Greater than >"},
		{"|", " Vertical bar |"},
		{"*", " Asterisk *"},
		{"?", " Question mark ?"},
		{"\r", ` Carriage return \r`},
		{"\n", ` Line feed \n`},
	}
	invalidNameChars = append(slices.Clone(invalidPathChars), invalidChar{`\`, ` Backslash \`}, invalidChar{"/", " Forward slash /"})
)

func labels(chars []invalidChar) string {
	var list []string
	for _, char := range chars {
		list = append(list, char.label)
	}
	return strings.Join(list, ",")
}

func addToZip(zw *zip.Writer, level int, root string, file found) error {
	rel := strings.TrimPrefix(file.path, root)
	for _, invalid := range invalidPathChars {
		if strings.Contains(rel, invalid.char) {
			return action.Failf("The path for one of the files in artifact is not valid: %s. Contains the following character: %s\n\nInvalid characters include: %s\n\nThe following characters are not allowed in files that are uploaded due to limitations with certain file systems such as NTFS. To maintain file system agnostic behavior, these characters are intentionally not allowed to prevent potential problems with downloads on different file systems.", rel, invalid.label, labels(invalidPathChars))
		}
	}
	header := &zip.FileHeader{Name: strings.TrimLeft(strings.ReplaceAll(rel, `\`, "/"), "/"), Method: zip.Deflate, Modified: file.mtime.UTC()} // zip-stream writes DOS times in UTC
	if level == 0 {
		header.Method = zip.Store
	}
	header.SetMode(file.mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky))
	entry, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, file.body)
	return err
}

func validateName(c *action.Context, name string) error {
	if name == "" {
		return action.Failf("Provided artifact name input during validation is empty")
	}
	for _, invalid := range invalidNameChars {
		if strings.Contains(name, invalid.char) {
			return action.Failf("The artifact name is not valid: %s. Contains the following character: %s\n\nInvalid characters include: %s\n\nThese characters are not allowed in the artifact name due to limitations with certain file systems such as NTFS. To maintain file system agnostic behavior, these characters are intentionally not allowed to prevent potential problems with downloads on different file systems.", name, invalid.label, labels(invalidNameChars))
		}
	}
	c.Infof("Artifact name is valid!")
	return nil
}

func upload(ctx context.Context, c *action.Context, in *inputs, got *collected, spool *os.File) error {
	name, fileName, mimeType := in.name, in.name+".zip", ""
	if !in.archiveIt {
		name, fileName, mimeType = got.fileName, got.fileName, mimeTypeOf(got.fileName)
	}
	if err := validateName(c, name); err != nil {
		return err
	}
	client, err := artifact.New(c.ResultsURL, c.RuntimeToken, c.Transport)
	if err != nil {
		return err
	}
	client.Logf = c.Infof
	if in.overwrite {
		if id, err := client.Delete(ctx, name); err == nil {
			c.Infof("Artifact '%s' (ID: %d) deleted", name, id)
		} else if !artifact.IsNotFound(err) {
			c.Infof("Unable to delete artifact: %v", err) // best effort, as the JavaScript action
		}
	}
	signed, err := client.Create(ctx, name, mimeType, expiresAt(c, in.retentionDays))
	if err != nil {
		return err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	concurrency, err := uploadConcurrency(c)
	if err != nil {
		return err
	}
	stall, err := uploadTimeout(c)
	if err != nil {
		return err
	}
	c.Infof("Uploading artifact: %s", fileName)
	c.Infof("Beginning upload of artifact content to blob storage")
	uploaded, err := client.Upload(ctx, signed, cmp.Or(mimeType, "application/zip"), spool, concurrency, stall)
	if err != nil {
		return err
	}
	digest := strings.TrimPrefix(uploaded.Digest, "sha256:")
	c.Infof("Finished uploading artifact content to blob storage!")
	c.Infof("SHA256 digest of uploaded artifact is %s", digest)
	c.Infof("Finalizing artifact upload")
	id, err := client.Finalize(ctx, name, uploaded.Size, uploaded.Digest)
	if err != nil {
		return err
	}
	c.Infof("Artifact %s successfully finalized. Artifact ID %d", name, id)
	c.Infof("Artifact %s has been successfully uploaded! Final size is %d bytes. Artifact ID is %d", name, uploaded.Size, id)
	artifactURL := artifactURL(c, name)
	c.Infof("Artifact download URL: %s", artifactURL)
	return c.SetOutputs(ctx, "artifact-id", strconv.FormatInt(id, 10), "artifact-digest", digest, "artifact-url", artifactURL)
}

// artifactURL links the run page by name and attempt, as the JavaScript action's ID-based link only resolves on GitHub.
func artifactURL(c *action.Context, name string) string {
	ghc := c.Github
	link := strings.TrimSuffix(ghc.ServerURL, "/") + "/" + ghc.Repository + "/actions/runs/" + ghc.RunID + "/artifacts/" + url.PathEscape(name)
	if ghc.RunAttempt != "" {
		link += "?attempt=" + url.QueryEscape(ghc.RunAttempt)
	}
	return link
}

func expiresAt(c *action.Context, days int) *time.Time {
	if days == 0 {
		return nil
	}
	if limit, err := action.ParseInt(c.Github.RetentionDays); err == nil && limit != 0 && limit < days {
		c.Warning("Retention days cannot be greater than the maximum allowed retention set within the repository. Using %d instead.", limit)
		days = limit
	}
	expires := time.Now().AddDate(0, 0, days)
	return &expires
}

func uploadConcurrency(c *action.Context) (int, error) {
	limit := 32
	if cpus := runtime.NumCPU(); cpus > 4 {
		limit = min(16*cpus, 300)
	}
	override := c.Env["ACTIONS_ARTIFACT_UPLOAD_CONCURRENCY"]
	if override == "" {
		return 5, nil
	}
	concurrency, err := action.ParseInt(override)
	if err != nil || concurrency < 1 {
		return 0, action.Failf("Invalid value set for ACTIONS_ARTIFACT_UPLOAD_CONCURRENCY env variable")
	}
	if concurrency < limit {
		c.Infof("Set concurrency based on the value set in ACTIONS_ARTIFACT_UPLOAD_CONCURRENCY.")
		return concurrency, nil
	}
	c.Infof("ACTIONS_ARTIFACT_UPLOAD_CONCURRENCY is higher than the cap of %d based on the number of cpus. Set it to the maximum value allowed.", limit)
	return limit, nil
}

func uploadTimeout(c *action.Context) (time.Duration, error) {
	value := c.Env["ACTIONS_ARTIFACT_UPLOAD_TIMEOUT_MS"]
	if value == "" {
		return 5 * time.Minute, nil
	}
	ms, err := action.ParseInt(value)
	if err != nil {
		return 0, action.Failf("Invalid value set for ACTIONS_ARTIFACT_UPLOAD_TIMEOUT_MS env variable")
	}
	return time.Duration(ms) * time.Millisecond, nil
}
