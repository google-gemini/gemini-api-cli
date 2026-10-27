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
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ShellExecFunc executes a shell command and returns stdout, stderr, exitCode, and any execution error.
type ShellExecFunc func(ctx context.Context, command, workDir string, timeout time.Duration) (string, string, int, error)

// ToolDispatcherConfig configures built-in sandbox tool dispatch.
type ToolDispatcherConfig struct {
	FS                          *SandboxFS
	Shell                       *SandboxShell
	ShellExec                   ShellExecFunc
	DisableBash                 bool
	CommandTimeout              time.Duration
	EnableExtendedFSTools       bool
	DefaultWriteToFileOverwrite bool
	RespectWaitMsBeforeAsync    bool
}

// ToolDispatcher dispatches client-side filesystem, search, and shell tool invocations.
type ToolDispatcher struct {
	cfg ToolDispatcherConfig
}

// NewToolDispatcher constructs a ToolDispatcher from cfg.
func NewToolDispatcher(cfg ToolDispatcherConfig) *ToolDispatcher {
	return &ToolDispatcher{cfg: cfg}
}

// Dispatch executes toolName with args and returns the structured JSON-like map result and isError flag.
func (d *ToolDispatcher) Dispatch(ctx context.Context, toolName string, args map[string]any) (map[string]any, bool) {
	normalized := strings.ToLower(strings.TrimSpace(toolName))
	switch normalized {
	case "execute_bash", "run_command", "run_shell_command", "bash", "shell":
		return d.toolExecuteBash(ctx, args)
	case "view_file", "read_file":
		return d.dispatchFS(func() (map[string]any, bool) { return d.toolViewFile(args) })
	case "write_to_file", "write_file":
		return d.dispatchFS(func() (map[string]any, bool) { return d.toolWriteFile(normalized, args) })
	case "replace_file_content", "edit_file":
		return d.dispatchFS(func() (map[string]any, bool) { return d.toolReplaceFileContent(args) })
	case "list_dir", "read_dir", "list_directory":
		return d.dispatchFS(func() (map[string]any, bool) { return d.toolListDir(args) })
	case "grep_search":
		return d.dispatchFS(func() (map[string]any, bool) { return d.toolGrepSearch(args) })
	case "find_by_name", "find_file":
		return d.dispatchFS(func() (map[string]any, bool) { return d.toolFindByName(args) })
	case "stat_file", "stat":
		if d.cfg.EnableExtendedFSTools {
			return d.dispatchFS(func() (map[string]any, bool) { return d.toolStatFile(args) })
		}
	case "mkdir":
		if d.cfg.EnableExtendedFSTools {
			return d.dispatchFS(func() (map[string]any, bool) { return d.toolMkdir(args) })
		}
	case "remove_file", "remove":
		if d.cfg.EnableExtendedFSTools {
			return d.dispatchFS(func() (map[string]any, bool) { return d.toolRemoveFile(args) })
		}
	case "rename_file", "rename":
		if d.cfg.EnableExtendedFSTools {
			return d.dispatchFS(func() (map[string]any, bool) { return d.toolRenameFile(args) })
		}
	}
	return map[string]any{"error": fmt.Sprintf("unsupported sandbox tool %q", toolName)}, true
}

func (d *ToolDispatcher) dispatchFS(fn func() (map[string]any, bool)) (map[string]any, bool) {
	if d.cfg.FS == nil {
		return map[string]any{"error": "filesystem is not configured"}, true
	}
	return fn()
}

func (d *ToolDispatcher) toolExecuteBash(ctx context.Context, args map[string]any) (map[string]any, bool) {
	if d.cfg.DisableBash {
		return map[string]any{"error": "execute_bash is disabled (--disable-bash)"}, true
	}
	cmdLine := stringArg(args, "command", "CommandLine", "cmd")
	rawWorkDir := stringArg(args, "Cwd", "cwd", "working_dir")
	resolvedWorkDir := rawWorkDir
	if d.cfg.FS != nil && (rawWorkDir != "" || d.cfg.FS.Mode() == ConfinementOSRoot) {
		resolved, err := d.cfg.FS.ResolvePath(rawWorkDir)
		if err != nil {
			return map[string]any{"error": err.Error()}, true
		}
		resolvedWorkDir = resolved
	}

	timeout := d.cfg.CommandTimeout
	timeoutKeys := []string{"timeout_ms"}
	if d.cfg.RespectWaitMsBeforeAsync {
		timeoutKeys = append(timeoutKeys, "WaitMsBeforeAsync")
	}
	if timeoutMS := intArg(args, 0, timeoutKeys...); timeoutMS > 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	} else if timeoutSec := intArg(args, 0, "timeout_seconds", "timeout"); timeoutSec > 0 {
		timeout = time.Duration(timeoutSec) * time.Second
	}
	if d.cfg.CommandTimeout > 0 && timeout > d.cfg.CommandTimeout {
		timeout = d.cfg.CommandTimeout
	}

	var (
		stdout   string
		stderr   string
		exitCode int
		err      error
	)
	switch {
	case d.cfg.ShellExec != nil:
		stdout, stderr, exitCode, err = d.cfg.ShellExec(ctx, cmdLine, resolvedWorkDir, timeout)
	case d.cfg.Shell != nil:
		stdout, stderr, exitCode, err = d.cfg.Shell.ExecuteBash(ctx, cmdLine, resolvedWorkDir, timeout)
	default:
		return map[string]any{"error": "shell executor is not configured"}, true
	}
	if err != nil {
		return map[string]any{
			"error":     err.Error(),
			"stdout":    stdout,
			"stderr":    stderr,
			"exit_code": exitCode,
		}, true
	}
	return map[string]any{
		"stdout":    stdout,
		"stderr":    stderr,
		"exit_code": exitCode,
	}, false
}

func (d *ToolDispatcher) toolViewFile(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "AbsolutePath", "path", "file_path")
	if path == "" {
		return map[string]any{"error": "path is required"}, true
	}
	startLine := intArg(args, 0, "StartLine", "start_line")
	endLine := intArg(args, 0, "EndLine", "end_line")
	var content string
	if startLine > 0 || endLine > 0 {
		sliced, err := d.cfg.FS.ReadFileRange(path, startLine, endLine)
		if err != nil {
			return map[string]any{"error": err.Error()}, true
		}
		content = sliced
	} else {
		data, err := d.cfg.FS.ReadRegularFile(path)
		if err != nil {
			return map[string]any{"error": err.Error()}, true
		}
		content = string(data)
	}
	return map[string]any{
		"path":    path,
		"content": content,
	}, false
}

func (d *ToolDispatcher) toolWriteFile(normalizedToolName string, args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "TargetFile", "path", "file_path")
	if path == "" {
		return map[string]any{"error": "path is required"}, true
	}
	content := stringArg(args, "CodeContent", "content")
	defaultOverwrite := normalizedToolName == "write_file" || d.cfg.DefaultWriteToFileOverwrite
	overwrite := boolArg(args, defaultOverwrite, "Overwrite", "overwrite")
	appendMode := boolArg(args, false, "Append", "append")
	n, err := d.cfg.FS.WriteRegularFile(path, content, overwrite, appendMode)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{
		"status":        "ok",
		"path":          path,
		"bytes_written": n,
	}, false
}

func (d *ToolDispatcher) toolReplaceFileContent(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "TargetFile", "path", "file_path")
	if path == "" {
		return map[string]any{"error": "TargetFile is required"}, true
	}
	target := stringArg(args, "TargetContent", "old_string")
	if target == "" {
		return map[string]any{"error": "TargetContent is required"}, true
	}
	replacement := stringArg(args, "ReplacementContent", "new_string")
	allowMultiple := boolArg(args, false, "AllowMultiple", "allow_multiple")
	startLine := intArg(args, 0, "StartLine", "start_line")
	endLine := intArg(args, 0, "EndLine", "end_line")

	raw, err := d.cfg.FS.ReadRegularFile(path)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	updated, count, err := ReplaceInText(string(raw), target, replacement, startLine, endLine, allowMultiple)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	if _, err := d.cfg.FS.WriteRegularFile(path, updated, true, false); err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{
		"status":       "ok",
		"path":         path,
		"replacements": count,
	}, false
}

func (d *ToolDispatcher) toolListDir(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "DirectoryPath", "path", "dir_path")
	if path == "" {
		path = "."
	}
	entries, err := d.cfg.FS.ListEntries(path)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{
		"path":    path,
		"entries": entries,
	}, false
}

func (d *ToolDispatcher) toolStatFile(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "AbsolutePath", "path", "file_path")
	info, err := d.cfg.FS.Stat(path)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{
		"name":        info.Name,
		"is_dir":      info.IsDir,
		"size":        info.Size,
		"permissions": int(info.Permissions),
	}, false
}

func (d *ToolDispatcher) toolMkdir(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "DirectoryPath", "path", "dir_path")
	recursive := boolArg(args, true, "recursive", "Recursive")
	if err := d.cfg.FS.Mkdir(path, 0o755, recursive); err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{"status": "ok", "path": path}, false
}

func (d *ToolDispatcher) toolRemoveFile(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "AbsolutePath", "TargetFile", "path", "file_path")
	if strings.TrimSpace(path) == "" {
		return map[string]any{"error": "path is required"}, true
	}
	recursive := boolArg(args, false, "recursive", "Recursive")
	if err := d.cfg.FS.Remove(path, recursive); err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{"status": "ok", "path": path}, false
}

func (d *ToolDispatcher) toolRenameFile(args map[string]any) (map[string]any, bool) {
	oldPath := stringArg(args, "old_path", "source_path", "OldPath")
	newPath := stringArg(args, "new_path", "destination_path", "NewPath")
	if err := d.cfg.FS.Rename(oldPath, newPath); err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{"status": "ok", "old_path": oldPath, "new_path": newPath}, false
}

func (d *ToolDispatcher) toolGrepSearch(args map[string]any) (map[string]any, bool) {
	searchPath := cmp.Or(stringArg(args, "SearchPath", "path"), ".")
	query := stringArg(args, "Query", "query")
	caseInsensitive := boolArg(args, false, "CaseInsensitive", "case_insensitive")
	isRegex := boolArg(args, false, "IsRegex", "is_regex")
	matches, truncated, err := d.cfg.FS.Grep(searchPath, query, caseInsensitive, isRegex)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	res := map[string]any{"matches": matches}
	if truncated {
		res["truncated"] = true
	}
	return res, false
}

func (d *ToolDispatcher) toolFindByName(args map[string]any) (map[string]any, bool) {
	searchDir := cmp.Or(stringArg(args, "SearchDirectory", "path"), ".")
	pattern := stringArg(args, "Pattern", "pattern")
	maxDepth := intArg(args, 0, "MaxDepth", "max_depth")
	extensions := stringSliceArg(args, "Extensions", "extensions")
	files, truncated, err := d.cfg.FS.FindFiles(searchDir, pattern, maxDepth, extensions...)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	res := map[string]any{"files": files}
	if truncated {
		res["truncated"] = true
	}
	return res, false
}

// ReplaceInText replaces target with replacement within 1-indexed inclusive [startLine, endLine] while preserving CRLF line endings.
func ReplaceInText(content, target, replacement string, startLine, endLine int, allowMultiple bool) (string, int, error) {
	preserveCRLF := strings.Contains(content, "\r\n") && !strings.Contains(target, "\r\n")
	if preserveCRLF {
		content = strings.ReplaceAll(content, "\r\n", "\n")
		replacement = strings.ReplaceAll(replacement, "\r\n", "\n")
	}
	restore := func(s string) string {
		if preserveCRLF {
			return strings.ReplaceAll(s, "\n", "\r\n")
		}
		return s
	}

	if startLine <= 0 && endLine <= 0 {
		count := strings.Count(content, target)
		if count == 0 {
			return "", 0, fmt.Errorf("target content not found")
		}
		if count > 1 && !allowMultiple {
			return "", 0, fmt.Errorf("found %d occurrences of target content when AllowMultiple is false", count)
		}
		return restore(strings.ReplaceAll(content, target, replacement)), count, nil
	}

	lines := strings.Split(content, "\n")
	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > endLine || startLine > len(lines) {
		return "", 0, fmt.Errorf("invalid line range [%d, %d]", startLine, endLine)
	}

	region := strings.Join(lines[startLine-1:endLine], "\n")
	count := strings.Count(region, target)
	if count == 0 {
		return "", 0, fmt.Errorf("target content not found in lines [%d, %d]", startLine, endLine)
	}
	if count > 1 && !allowMultiple {
		return "", 0, fmt.Errorf("found %d occurrences in lines [%d, %d] when AllowMultiple is false", count, startLine, endLine)
	}

	var parts []string
	if startLine > 1 {
		parts = append(parts, strings.Join(lines[:startLine-1], "\n"))
	}
	parts = append(parts, strings.ReplaceAll(region, target, replacement))
	if endLine < len(lines) {
		parts = append(parts, strings.Join(lines[endLine:], "\n"))
	}
	return restore(strings.Join(parts, "\n")), count, nil
}

func stringArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := args[k]; v != nil {
			if s, ok := v.(string); ok {
				return s
			}
			return fmt.Sprint(v)
		}
	}
	return ""
}

func intArg(args map[string]any, fallback int, keys ...string) int {
	for _, k := range keys {
		switch val := args[k].(type) {
		case int:
			return val
		case int32:
			return int(val)
		case int64:
			return int(val)
		case float64:
			return int(val)
		case float32:
			return int(val)
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				return n
			}
		}
	}
	return fallback
}

func boolArg(args map[string]any, fallback bool, keys ...string) bool {
	for _, k := range keys {
		switch val := args[k].(type) {
		case bool:
			return val
		case string:
			if b, err := strconv.ParseBool(strings.TrimSpace(val)); err == nil {
				return b
			}
		}
	}
	return fallback
}

func stringSliceArg(args map[string]any, keys ...string) []string {
	for _, k := range keys {
		switch val := args[k].(type) {
		case []string:
			return val
		case []any:
			out := make([]string, 0, len(val))
			for _, item := range val {
				if item != nil {
					out = append(out, fmt.Sprint(item))
				}
			}
			return out
		case string:
			var out []string
			for _, part := range strings.Split(val, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					out = append(out, trimmed)
				}
			}
			return out
		}
	}
	return nil
}
