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

package custom

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	// maxBashOutputBytes caps stdout and stderr capture per command execution at 4 MiB.
	maxBashOutputBytes = 4 * 1024 * 1024
	// maxReadFileBytes caps full-file and single-call range reads at 4 MiB.
	maxReadFileBytes = 4 * 1024 * 1024
	// maxGrepMatches and maxGrepOutputBytes bound grep_search payloads well under maxGRPCFrameBytes.
	maxGrepMatches     = 1000
	maxGrepOutputBytes = 4 * 1024 * 1024
	// maxFindFiles bounds find_by_name results per call.
	maxFindFiles = 5000
)

var errSearchTruncated = errors.New("search results truncated")

// resolveSandboxDirs validates rootDir and workDir confinement (and creates
// both directories when create is true).
func resolveSandboxDirs(rootDir, workDir string, create bool) (string, string, error) {
	if strings.TrimSpace(rootDir) == "" {
		rootDir = "."
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return "", "", fmt.Errorf("resolving root-dir %q: %w", rootDir, err)
	}
	origAbsRoot := filepath.Clean(absRoot)
	absRoot = canonicalizeLongestPrefix(origAbsRoot)

	if strings.TrimSpace(workDir) == "" || workDir == "." {
		workDir = absRoot
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return "", "", fmt.Errorf("resolving workdir %q: %w", workDir, err)
	}
	if origAbsRoot != absRoot && isWithinRoot(origAbsRoot, absWork) {
		if rel, relErr := filepath.Rel(origAbsRoot, absWork); relErr == nil {
			absWork = filepath.Join(absRoot, rel)
		}
	}
	if !isWithinRoot(absRoot, absWork) {
		return "", "", fmt.Errorf("workdir %q escapes root-dir %q", absWork, absRoot)
	}
	resolvedWork, err := newFSServer(absRoot, absRoot).ResolvePath(absWork)
	if err != nil {
		return "", "", fmt.Errorf("workdir %q escapes root-dir %q: %w", absWork, absRoot, err)
	}
	absWork = resolvedWork

	if !create {
		return absRoot, absWork, nil
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return "", "", fmt.Errorf("creating root-dir %q: %w", absRoot, err)
	}
	if err := os.MkdirAll(absWork, 0o755); err != nil {
		return "", "", fmt.Errorf("creating workdir %q: %w", absWork, err)
	}
	return absRoot, absWork, nil
}

func isWithinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func canonicalizeLongestPrefix(absPath string) string {
	cur := filepath.Clean(absPath)
	var suffix []string
	for {
		if eval, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				eval = filepath.Join(eval, suffix[i])
			}
			return filepath.Clean(eval)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
	return filepath.Clean(absPath)
}

type fsServer struct {
	rootDir string
	workDir string
}

func newFSServer(rootDir, workDir string) *fsServer {
	if abs, err := filepath.Abs(rootDir); err == nil {
		rootDir = canonicalizeLongestPrefix(abs)
	}
	cleanRoot := filepath.Clean(rootDir)
	cleanWork := cleanRoot
	if strings.TrimSpace(workDir) != "" {
		if absWork, err := filepath.Abs(workDir); err == nil {
			cleanWork = canonicalizeLongestPrefix(absWork)
		} else {
			cleanWork = filepath.Clean(workDir)
		}
		if !isWithinRoot(cleanRoot, cleanWork) {
			cleanWork = cleanRoot
		}
	}
	return &fsServer{
		rootDir: cleanRoot,
		workDir: cleanWork,
	}
}

func (s *fsServer) ResolvePath(relPath string) (string, error) {
	if strings.Contains(relPath, "\x00") {
		return "", fmt.Errorf("access denied: path contains NUL byte")
	}
	var candidate string
	if filepath.IsAbs(relPath) {
		cleaned := filepath.Clean(relPath)
		if isWithinRoot(s.rootDir, cleaned) {
			candidate = cleaned
		} else {
			trimmed := strings.TrimPrefix(cleaned, filepath.VolumeName(cleaned))
			trimmed = strings.TrimPrefix(trimmed, string(filepath.Separator))
			candidate = filepath.Join(s.rootDir, trimmed)
		}
	} else {
		candidate = filepath.Join(s.workDir, relPath)
	}
	if !isWithinRoot(s.rootDir, candidate) {
		return "", fmt.Errorf("access denied: path %q escapes root directory", relPath)
	}

	cur := candidate
	var suffix []string
	for {
		if eval, err := filepath.EvalSymlinks(cur); err == nil {
			resolved := eval
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
				if fi, lerr := os.Lstat(resolved); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
					return "", fmt.Errorf("access denied: dangling symlink %q is not permitted", relPath)
				}
			}
			if !isWithinRoot(s.rootDir, resolved) {
				return "", fmt.Errorf("access denied: symlink target for %q escapes root directory", relPath)
			}
			return resolved, nil
		}
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("access denied: dangling symlink %q is not permitted", relPath)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
	return candidate, nil
}

func (s *fsServer) resolveRelInRoot(relPath string) (string, error) {
	resolved, err := s.ResolvePath(relPath)
	if err != nil {
		return "", err
	}
	return filepath.Rel(s.rootDir, resolved)
}

func (s *fsServer) openInRoot(relPath string) (*os.Root, string, error) {
	rel, err := s.resolveRelInRoot(relPath)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(s.rootDir)
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}

func mkdirAllInRoot(root *os.Root, relDir string) error {
	cur := ""
	for _, part := range strings.Split(filepath.Clean(relDir), string(filepath.Separator)) {
		if cur == "" {
			cur = part
		} else {
			cur = cur + string(filepath.Separator) + part
		}
		if fi, err := root.Stat(cur); err == nil && fi.IsDir() {
			continue
		}
		if err := root.Mkdir(cur, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (s *fsServer) ReadRegularFile(path string) ([]byte, error) {
	root, rel, err := s.openInRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("cannot read directory %q as file", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("cannot read non-regular file %q", path)
	}
	data, exceeded, err := readLimitedAll(f, maxReadFileBytes)
	if err != nil {
		return nil, err
	}
	if exceeded {
		return nil, fmt.Errorf("file %q exceeds maximum full-read size (%d bytes); specify start_line and end_line", path, maxReadFileBytes)
	}
	return data, nil
}

func readLimitedAll(r io.Reader, maxBytes int) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxBytes {
		return nil, true, nil
	}
	return data, false, nil
}

func (s *fsServer) ReadFileRange(path string, startLine, endLine int) (string, error) {
	root, rel, err := s.openInRoot(path)
	if err != nil {
		return "", err
	}
	defer root.Close()

	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("cannot read directory %q as file", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("cannot read non-regular file %q", path)
	}
	if startLine <= 0 {
		startLine = 1
	}

	reader := bufio.NewReader(f)
	var selected []string
	totalBytes := 0
	for lineNum := 1; ; lineNum++ {
		part, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		line := strings.TrimSuffix(strings.TrimSuffix(part, "\n"), "\r")
		if lineNum >= startLine && (endLine <= 0 || lineNum <= endLine) {
			totalBytes += len(line) + 1
			if totalBytes > maxReadFileBytes {
				return "", fmt.Errorf("line range [%d, %d] of %q exceeds maximum read size (%d bytes); narrow start_line and end_line", startLine, endLine, path, maxReadFileBytes)
			}
			selected = append(selected, line)
		}
		if (endLine > 0 && lineNum >= endLine) || errors.Is(readErr, io.EOF) {
			break
		}
	}
	return strings.Join(selected, "\n"), nil
}

func (s *fsServer) WriteRegularFile(path, content string, overwrite, appendMode bool) (int, error) {
	root, rel, err := s.openInRoot(path)
	if err != nil {
		return 0, err
	}
	defer root.Close()

	if fi, err := root.Lstat(rel); err == nil && !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("cannot write non-regular file %q", path)
	}
	if err := mkdirAllInRoot(root, filepath.Dir(rel)); err != nil {
		return 0, err
	}
	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case appendMode:
		flags |= os.O_APPEND
	case overwrite:
		flags |= os.O_TRUNC
	default:
		flags |= os.O_EXCL
	}
	f, err := root.OpenFile(rel, flags, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.WriteString(content)
}

func (s *fsServer) ListEntries(path string) ([]any, error) {
	root, rel, err := s.openInRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	dirFile, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer dirFile.Close()

	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		var size int64
		if info, infoErr := entry.Info(); infoErr == nil {
			size = info.Size()
		}
		out = append(out, map[string]any{
			"name":   entry.Name(),
			"is_dir": entry.IsDir(),
			"size":   int(size),
		})
	}
	return out, nil
}

func (s *fsServer) Grep(searchPath, query string, caseInsensitive, isRegex bool) ([]any, bool, error) {
	fullPath, err := s.ResolvePath(searchPath)
	if err != nil {
		return nil, false, err
	}
	if query == "" {
		return nil, false, fmt.Errorf("query is required")
	}
	pattern := query
	if !isRegex {
		pattern = regexp.QuoteMeta(query)
	}
	if caseInsensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, fmt.Errorf("invalid search pattern: %w", err)
	}
	root, err := os.OpenRoot(s.rootDir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()

	var matches []any
	totalBytes := 0
	err = filepath.WalkDir(fullPath, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		fileRel, err := s.resolveRelInRoot(p)
		if err != nil {
			return nil
		}
		f, err := root.Open(fileRel)
		if err != nil {
			return nil
		}
		info, statErr := f.Stat()
		if statErr != nil || info.IsDir() || !info.Mode().IsRegular() || info.Size() > maxReadFileBytes {
			_ = f.Close()
			return nil
		}
		data, readErr := io.ReadAll(io.LimitReader(f, maxReadFileBytes+1))
		_ = f.Close()
		if readErr != nil || len(data) > maxReadFileBytes {
			return nil
		}
		rel, err := filepath.Rel(fullPath, p)
		if err != nil || rel == "." {
			rel = filepath.Base(p)
		}
		slashRel := filepath.ToSlash(rel)
		for idx, rawLine := range strings.Split(string(data), "\n") {
			line := strings.TrimSuffix(rawLine, "\r")
			if re.MatchString(line) {
				entryBytes := len(slashRel) + len(line) + 32
				if len(matches) >= maxGrepMatches || (len(matches) > 0 && totalBytes+entryBytes > maxGrepOutputBytes) {
					return errSearchTruncated
				}
				totalBytes += entryBytes
				matches = append(matches, map[string]any{
					"file":        slashRel,
					"line_number": idx + 1,
					"line":        line,
				})
			}
		}
		return nil
	})
	if errors.Is(err, errSearchTruncated) {
		return matches, true, nil
	}
	return matches, false, err
}

func (s *fsServer) FindFiles(searchDir, pattern string, maxDepth int, extensions ...string) ([]any, bool, error) {
	fullPath, err := s.ResolvePath(searchDir)
	if err != nil {
		return nil, false, err
	}
	var extAllow map[string]bool
	for _, rawExt := range extensions {
		clean := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(rawExt)), ".")
		if clean != "" {
			if extAllow == nil {
				extAllow = make(map[string]bool, len(extensions))
			}
			extAllow[clean] = true
		}
	}
	patternLower := strings.ToLower(pattern)
	var files []any
	err = filepath.WalkDir(fullPath, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || p == fullPath {
			return nil
		}
		if _, err := s.ResolvePath(p); err != nil {
			return nil
		}
		rel, err := filepath.Rel(fullPath, p)
		if err != nil {
			return nil
		}
		slashRel := filepath.ToSlash(rel)
		if maxDepth > 0 && strings.Count(slashRel, "/")+1 > maxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if len(extAllow) > 0 {
			if d.IsDir() {
				return nil
			}
			fileExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
			if !extAllow[fileExt] {
				return nil
			}
		}
		matched, _ := filepath.Match(pattern, name)
		if matched || patternLower == "" || strings.Contains(strings.ToLower(name), patternLower) {
			if len(files) >= maxFindFiles {
				return errSearchTruncated
			}
			files = append(files, slashRel)
		}
		return nil
	})
	if errors.Is(err, errSearchTruncated) {
		return files, true, nil
	}
	return files, false, err
}

type shellServer struct {
	workDir string
}

func newShellServer(workDir string) *shellServer {
	return &shellServer{workDir: workDir}
}

type cappedBuffer struct {
	buf       bytes.Buffer
	maxBytes  int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	remaining := c.maxBytes - c.buf.Len()
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

func (c *cappedBuffer) String() string {
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

func filterScrubbedEnv(baseEnv []string) []string {
	out := make([]string, 0, len(baseEnv))
	for _, kv := range baseEnv {
		key, _, _ := strings.Cut(kv, "=")
		if scrubbedCredentialEnvKeys[strings.ToUpper(strings.TrimSpace(key))] {
			continue
		}
		out = append(out, kv)
	}
	return out
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

func buildWorkerBashEnv(baseEnv []string) []string {
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

func resolveShellCommand(goos, command string) (string, []string) {
	if p, err := exec.LookPath("bash"); err == nil {
		return p, []string{"-c", command}
	}
	if p, err := exec.LookPath("sh"); err == nil {
		return p, []string{"-c", command}
	}
	if goos == "windows" {
		return "cmd.exe", []string{"/C", command}
	}
	return "sh", []string{"-c", command}
}

func (s *shellServer) ExecuteBash(ctx context.Context, command, workDir string, timeout time.Duration) (string, string, int, error) {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := s.workDir
	if workDir != "" {
		dir = workDir
	}

	shellBin, shellArgs := resolveShellCommand(runtime.GOOS, command)
	cmd := exec.CommandContext(ctx, shellBin, shellArgs...)
	cmd.Dir = dir
	configureSubprocessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = buildWorkerBashEnv(os.Environ())

	stdoutBuf := &cappedBuffer{maxBytes: maxBashOutputBytes}
	stderrBuf := &cappedBuffer{maxBytes: maxBashOutputBytes}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	err := cmd.Run()
	stdout := stdoutBuf.String()
	stderr := stderrBuf.String()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout, stderr, -1, fmt.Errorf("command timed out after %v", timeout)
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
