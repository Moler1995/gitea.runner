// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package action holds the native actions run by `uses: builtin:<name>`.
package action

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
)

type Context struct {
	Container       container.ExecutionsEnvironment
	Github          *model.GithubContext
	Inputs          map[string]string
	HostWorkdir     string
	Workspace       string // HostWorkdir as seen inside Container
	BindWorkdir     bool
	NoSkipCheckout  bool
	UseGitIgnore    bool
	InsecureSkipTLS bool
	ClientCertFile  string
	ClientKeyFile   string
	Env             map[string]string
	ResultsURL      string                           // the results service as the runner reaches it
	RuntimeToken    string                           // ACTIONS_RUNTIME_TOKEN, the job's credential for it
	Transport       http.RoundTripper                // carries the instance's TLS settings, nil for the default
	Infof           func(format string, args ...any) // a line of step output
	Annotate        func(level, message string)      // a notice, warning or error, as the workflow command would log it
}

// Warning logs what @actions/core warning does.
func (c *Context) Warning(format string, args ...any) {
	c.Annotate("warning", fmt.Sprintf(format, args...))
}

// CheckInputs fails on an input the action does not know, as a built-in action has no
// action.yml to ignore it with.
func (c *Context) CheckInputs(action string, known ...string) error {
	for name := range c.Inputs {
		if !slices.Contains(known, name) {
			return fmt.Errorf("%s: unsupported input %q", action, name)
		}
	}
	return nil
}

// Input is the trimmed input, as @actions/core getInput returns it.
func (c *Context) Input(name string) string {
	return strings.TrimSpace(c.Inputs[name])
}

// BoolInput accepts what @actions/core getBooleanInput does, the default standing in for the
// action.yml one when the input is not given.
func (c *Context) BoolInput(name string, fallback bool) (bool, error) {
	switch value := c.Input(name); value {
	case "":
		return fallback, nil
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}
	return false, Failf("Input does not meet YAML 1.2 \"Core Schema\" specification: %s\nSupport boolean input list: `true | True | TRUE | false | False | FALSE`", name)
}

// SetOutputs writes the step's outputs, name and value alternating.
func (c *Context) SetOutputs(ctx context.Context, pairs ...string) error {
	output := c.Env["GITHUB_OUTPUT"]
	if output == "" {
		return nil
	}
	var body strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if strings.ContainsAny(pairs[i+1], "\r\n") {
			return fmt.Errorf("output %s spans several lines", pairs[i])
		}
		body.WriteString(pairs[i] + "=" + pairs[i+1] + "\n")
	}
	return c.Container.Copy(c.Dir(output), &container.FileEntry{Name: c.Base(output), Mode: 0o666, Body: body.String()})(ctx)
}

// Host reports whether the job runs on the runner's host, where its paths are the runner's own.
func (c *Context) Host() bool {
	_, host := c.Container.(*container.HostEnvironment)
	return host
}

// Windows reports whether the job's paths are Windows paths, which only a host job on Windows has.
func (c *Context) Windows() bool {
	return c.Host() && runtime.GOOS == "windows"
}

func (c *Context) Join(elem ...string) string {
	if c.Windows() {
		return filepath.Join(elem...)
	}
	return path.Join(elem...)
}

func (c *Context) Dir(p string) string {
	if c.Windows() {
		return filepath.Dir(p)
	}
	return path.Dir(p)
}

func (c *Context) Base(p string) string {
	if c.Windows() {
		return filepath.Base(p)
	}
	return path.Base(p)
}

func (c *Context) IsAbs(p string) bool {
	if c.Windows() {
		return filepath.IsAbs(p)
	}
	return path.IsAbs(p)
}

// Resolve resolves p against the workspace, as path.resolve does in the step's working directory.
func (c *Context) Resolve(p string) string {
	if c.IsAbs(p) {
		return c.Join(p)
	}
	return c.Join(c.Workspace, p)
}

// Home is the job user's home directory, what os.homedir() returns to a JavaScript action.
func (c *Context) Home(ctx context.Context) (string, error) {
	if home := c.Env["HOME"]; home != "" {
		return home, nil
	}
	if c.Windows() {
		if home := c.Env["USERPROFILE"]; home != "" {
			return home, nil
		}
	}
	if c.Host() {
		return os.UserHomeDir()
	}
	var out strings.Builder
	stdout, stderr := c.Container.ReplaceLogWriter(&out, &out)
	err := c.Container.Exec([]string{"sh", "-c", `printf %s "$HOME"`}, nil, "", "")(ctx)
	c.Container.ReplaceLogWriter(stdout, stderr)
	if err != nil {
		return "", fmt.Errorf("cannot expand ~ without HOME: %w", err)
	}
	if out.Len() == 0 {
		return "", errors.New("cannot expand ~, the job user has no HOME")
	}
	return out.String(), nil
}

// failure carries a message worded as the JavaScript action words it, capitalised and punctuated.
type failure struct {
	msg string
	err error
}

func (f *failure) Error() string { return f.msg }
func (f *failure) Unwrap() error { return f.err }

// Failf formats like fmt.Errorf, %w included, for a message users know from the JavaScript action.
func Failf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	return &failure{msg: err.Error(), err: errors.Unwrap(err)}
}
