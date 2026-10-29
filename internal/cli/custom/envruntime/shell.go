// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package envruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// MaxBashOutputBytes caps stdout and stderr capture per command execution at 4 MiB.
const MaxBashOutputBytes = 4 * 1024 * 1024

// EnvPolicy selects how SandboxShell filters environment variables passed to child processes.
type EnvPolicy int

const (
	// EnvPolicyAllowlist passes only baseline OS/locale variables (recommended for host CLI workers).
	EnvPolicyAllowlist EnvPolicy = iota
	// EnvPolicyScrubCredentials passes the process environment with Gemini/Google credential variables removed plus ExtraEnv.
	EnvPolicyScrubCredentials
)

// CappedBuffer is a bytes.Buffer wrapper that bounds retained output to MaxBytes.
type CappedBuffer struct {
	buf       bytes.Buffer
	MaxBytes  int
	truncated bool
}

// Write appends up to MaxBytes into the buffer and discards overflow while reporting full write length.
func (c *CappedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	maxBytes := c.MaxBytes
	if maxBytes <= 0 {
		maxBytes = MaxBashOutputBytes
	}
	remaining := maxBytes - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return origLen, nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return origLen, nil
	}
	c.buf.Write(p)
	return origLen, nil
}

// String returns the captured text, appending a truncation marker when MaxBytes was exceeded.
func (c *CappedBuffer) String() string {
	if c.truncated {
		return c.buf.String() + "\n...[output truncated at 4 MiB]..."
	}
	return c.buf.String()
}

var scrubbedCredentialEnvKeys = map[string]bool{
	"GEMINI_ENVIRONMENT_KEY":     true,
	"ENVIRONMENT_KEY":            true,
	"GEMINI_API_KEY":             true,
	"GOOGLE_API_KEY":             true,
	"GEMINI_ACCESS_TOKEN":        true,
	"ACCESS_TOKEN":               true,
	"GOOGLE_OAUTH_ACCESS_TOKEN":  true,
	"CLOUDSDK_AUTH_ACCESS_TOKEN": true,
	"GEMINI_LEASE_TOKEN":         true,
	"LEASE_TOKEN":                true,
}

// FilterScrubbedEnv removes Gemini and Google credential environment variables from baseEnv and appends extraEnv.
func FilterScrubbedEnv(baseEnv []string, extraEnv ...string) []string {
	// Must be non-nil: a nil exec.Cmd.Env inherits the parent environment.
	out := make([]string, 0, len(baseEnv)+len(extraEnv))
	for _, kv := range baseEnv {
		key, _, _ := strings.Cut(kv, "=")
		if scrubbedCredentialEnvKeys[strings.ToUpper(strings.TrimSpace(key))] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extraEnv...)
}

var allowedWorkerBashEnvKeys = map[string]bool{
	"PATH":                   true,
	"HOME":                   true,
	"USER":                   true,
	"LOGNAME":                true,
	"SHELL":                  true,
	"LANG":                   true,
	"TERM":                   true,
	"TMPDIR":                 true,
	"SYSTEMROOT":             true,
	"WINDIR":                 true,
	"COMSPEC":                true,
	"PATHEXT":                true,
	"TEMP":                   true,
	"TMP":                    true,
	"USERPROFILE":            true,
	"APPDATA":                true,
	"LOCALAPPDATA":           true,
	"PROGRAMFILES":           true,
	"PROGRAMFILES(X86)":      true,
	"PROGRAMDATA":            true,
	"HOMEDRIVE":              true,
	"HOMEPATH":               true,
	"COMPUTERNAME":           true,
	"USERNAME":               true,
	"USERDOMAIN":             true,
	"OS":                     true,
	"SYSTEMDRIVE":            true,
	"PROCESSOR_ARCHITECTURE": true,
	"NUMBER_OF_PROCESSORS":   true,
	"PSMODULEPATH":           true,
}

// BuildAllowlistEnv retains only baseline OS and locale variables from baseEnv.
func BuildAllowlistEnv(baseEnv []string) []string {
	out := make([]string, 0, 12)
	for _, kv := range baseEnv {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(strings.TrimSpace(key))
		if allowedWorkerBashEnvKeys[upper] || strings.HasPrefix(upper, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

func isWindowsWSLStubBash(goos, p string) bool {
	if goos != "windows" {
		return false
	}
	normalized := strings.ToLower(strings.TrimRight(strings.ReplaceAll(p, "/", `\`), `\`))
	return strings.HasSuffix(normalized, `\system32\bash.exe`) || strings.HasSuffix(normalized, `\sysnative\bash.exe`)
}

// ResolveShellCommand selects bash, sh, or cmd.exe to execute command on goos.
func ResolveShellCommand(goos, command string) (string, []string) {
	return resolveShellCommand(goos, exec.LookPath, command)
}

func resolveShellCommand(goos string, lookPath func(string) (string, error), command string) (string, []string) {
	if p, err := lookPath("bash"); err == nil && !isWindowsWSLStubBash(goos, p) {
		return p, []string{"-c", command}
	}
	if p, err := lookPath("sh"); err == nil {
		return p, []string{"-c", command}
	}
	if goos == "windows" {
		return "cmd.exe", []string{"/C", command}
	}
	return "sh", []string{"-c", command}
}

func resolveExecShell(goos string, useBinSh bool, command string) (string, []string) {
	if useBinSh && goos != "windows" {
		return "/bin/sh", []string{"-c", command}
	}
	return ResolveShellCommand(goos, command)
}

// SandboxShellConfig configures a SandboxShell instance.
type SandboxShellConfig struct {
	WorkDir           string
	EnvPolicy         EnvPolicy
	ExtraEnv          []string
	AutoCreateWorkDir bool
	UseBinSh          bool
}

// SandboxShell executes shell commands with process-group termination, output caps, and environment filtering.
type SandboxShell struct {
	workDir           string
	envPolicy         EnvPolicy
	extraEnv          []string
	autoCreateWorkDir bool
	useBinSh          bool
}

// NewSandboxShell creates a SandboxShell using EnvPolicyAllowlist.
func NewSandboxShell(workDir string) *SandboxShell {
	return NewSandboxShellWithConfig(SandboxShellConfig{
		WorkDir:   workDir,
		EnvPolicy: EnvPolicyAllowlist,
	})
}

// NewSandboxShellWithConfig creates a SandboxShell with the provided configuration.
func NewSandboxShellWithConfig(cfg SandboxShellConfig) *SandboxShell {
	return &SandboxShell{
		workDir:           cfg.WorkDir,
		envPolicy:         cfg.EnvPolicy,
		extraEnv:          append([]string(nil), cfg.ExtraEnv...),
		autoCreateWorkDir: cfg.AutoCreateWorkDir,
		useBinSh:          cfg.UseBinSh,
	}
}

// WorkDir returns the default working directory configured on the shell.
func (s *SandboxShell) WorkDir() string {
	return s.workDir
}

// ExecuteBash executes command in workDir (or s.WorkDir() when empty) with an optional timeout.
// When timeout <= 0, no additional deadline is imposed beyond ctx.
func (s *SandboxShell) ExecuteBash(ctx context.Context, command, workDir string, timeout time.Duration) (string, string, int, error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	dir := s.workDir
	if workDir != "" {
		dir = workDir
	}
	if s.autoCreateWorkDir && dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}

	shellBin, shellArgs := resolveExecShell(runtime.GOOS, s.useBinSh, command)

	cmd := exec.CommandContext(runCtx, shellBin, shellArgs...)
	cmd.Dir = dir
	ConfigureSubprocessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	if s.envPolicy == EnvPolicyScrubCredentials {
		cmd.Env = FilterScrubbedEnv(os.Environ(), s.extraEnv...)
	} else {
		cmd.Env = append(BuildAllowlistEnv(os.Environ()), s.extraEnv...)
	}

	stdoutBuf := &CappedBuffer{MaxBytes: MaxBashOutputBytes}
	stderrBuf := &CappedBuffer{MaxBytes: MaxBashOutputBytes}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	err := cmd.Run()
	stdout := stdoutBuf.String()
	stderr := stderrBuf.String()

	if runCtx.Err() != nil {
		if ctx.Err() == nil {
			return stdout, stderr, -1, fmt.Errorf("command timed out after %v: %w", timeout, runCtx.Err())
		}
		return stdout, stderr, -1, runCtx.Err()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout, stderr, exitErr.ExitCode(), nil
		}
		return stdout, stderr, -1, err
	}
	return stdout, stderr, 0, nil
}
