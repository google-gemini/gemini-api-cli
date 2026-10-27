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
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// MaxReadFileBytes caps full-file and single-call range reads at 4 MiB unless UnlimitedRead is set.
	MaxReadFileBytes = 4 * 1024 * 1024

	// MaxGrepMatches bounds grep_search match count per call when bounded reads are enabled.
	MaxGrepMatches = 1000

	// MaxGrepOutputBytes bounds grep_search output bytes per call when bounded reads are enabled.
	MaxGrepOutputBytes = 4 * 1024 * 1024

	// MaxFindFiles bounds find_by_name results per call when bounded reads are enabled.
	MaxFindFiles = 5000
)

var errSearchTruncated = errors.New("search results truncated")

// PathErrorKind classifies filesystem confinement and I/O errors for callers that map to status codes.
type PathErrorKind int

const (
	// PathErrInternal indicates an unexpected filesystem I/O failure.
	PathErrInternal PathErrorKind = iota
	// PathErrInvalidArgument indicates an invalid path or file type mismatch (such as reading a directory as a file).
	PathErrInvalidArgument
	// PathErrPermissionDenied indicates a confinement escape, symlink rejection, or OS permission error.
	PathErrPermissionDenied
	// PathErrNotFound indicates that the target path or parent directory does not exist.
	PathErrNotFound
	// PathErrAlreadyExists indicates that an exclusive create encountered an existing entry.
	PathErrAlreadyExists
)

// PathError is a structured filesystem error carrying a PathErrorKind classification.
type PathError struct {
	Kind    PathErrorKind
	Path    string
	Message string
	Err     error
}

// Error implements error.
func (e *PathError) Error() string {
	if e.Message != "" {
		if e.Err != nil {
			return fmt.Sprintf("%s: %v", e.Message, e.Err)
		}
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("filesystem error on %q", e.Path)
}

// Unwrap returns the underlying error.
func (e *PathError) Unwrap() error {
	return e.Err
}

// ClassifyOSError maps an error returned by os filesystem calls to a *PathError, or nil when err is nil.
func ClassifyOSError(err error, path string) error {
	if err == nil {
		return nil
	}
	var pe *PathError
	if errors.As(err, &pe) {
		return pe
	}
	switch {
	case os.IsNotExist(err):
		return &PathError{
			Kind:    PathErrNotFound,
			Path:    path,
			Message: fmt.Sprintf("path not found: %s", path),
			Err:     err,
		}
	case os.IsPermission(err):
		return &PathError{
			Kind:    PathErrPermissionDenied,
			Path:    path,
			Message: fmt.Sprintf("permission denied: %s", path),
			Err:     err,
		}
	case os.IsExist(err):
		return &PathError{
			Kind:    PathErrAlreadyExists,
			Path:    path,
			Message: fmt.Sprintf("path already exists: %s", path),
			Err:     err,
		}
	default:
		return &PathError{
			Kind:    PathErrInternal,
			Path:    path,
			Message: fmt.Sprintf("operation failed on %s", path),
			Err:     err,
		}
	}
}

// ConfinementMode selects how SandboxFS confines file operations under RootDir.
type ConfinementMode int

const (
	// ConfinementOSRoot canonicalizes symlinks, rejects escaping or dangling symlinks, and opens files through *os.Root.
	ConfinementOSRoot ConfinementMode = iota
	// ConfinementLexical confines paths lexically via filepath.Clean under RootDir (for isolated container filesystems).
	ConfinementLexical
)

// FileKind classifies POSIX file types returned by SandboxFS.Stat and SandboxFS.ReadDir.
type FileKind int

const (
	// FileKindRegular denotes a regular file.
	FileKindRegular FileKind = iota
	// FileKindDirectory denotes a directory.
	FileKindDirectory
	// FileKindSymlink denotes a symbolic link.
	FileKindSymlink
	// FileKindFIFOPipe denotes a named pipe (FIFO).
	FileKindFIFOPipe
	// FileKindSocket denotes a Unix domain socket.
	FileKindSocket
	// FileKindCharDevice denotes a character device.
	FileKindCharDevice
	// FileKindBlockDevice denotes a block device.
	FileKindBlockDevice
)

// FileMetadata holds POSIX metadata for a filesystem entry.
type FileMetadata struct {
	Name          string
	Kind          FileKind
	IsDir         bool
	Size          int64
	ModTime       time.Time
	Permissions   uint32
	SymlinkTarget string
}

// SandboxFSConfig configures a SandboxFS instance.
type SandboxFSConfig struct {
	RootDir       string
	WorkDir       string
	Mode          ConfinementMode
	UnlimitedRead bool
}

// SandboxFS provides path-confined filesystem operations for agent tools and gRPC filesystem services.
type SandboxFS struct {
	rootDir       string
	workDir       string
	mode          ConfinementMode
	unlimitedRead bool
}

// NewSandboxFS creates a SandboxFS using ConfinementOSRoot and bounded 4 MiB reads.
func NewSandboxFS(rootDir, workDir string) *SandboxFS {
	return NewSandboxFSWithConfig(SandboxFSConfig{
		RootDir: rootDir,
		WorkDir: workDir,
		Mode:    ConfinementOSRoot,
	})
}

// NewSandboxFSWithConfig creates a SandboxFS with the given configuration.
func NewSandboxFSWithConfig(cfg SandboxFSConfig) *SandboxFS {
	if cfg.Mode == ConfinementLexical {
		root := filepath.Clean(cfg.RootDir)
		if cfg.RootDir == "" {
			root = string(filepath.Separator)
		} else if !isVirtualRootPath(cfg.RootDir) {
			if abs, err := filepath.Abs(cfg.RootDir); err == nil {
				root = filepath.Clean(abs)
			}
		}
		work := root
		if strings.TrimSpace(cfg.WorkDir) != "" {
			work = filepath.Clean(cfg.WorkDir)
			if !isVirtualRootPath(cfg.WorkDir) {
				if absWork, err := filepath.Abs(cfg.WorkDir); err == nil && isWithinRoot(root, absWork) {
					work = filepath.Clean(absWork)
				} else if !isWithinRoot(root, work) {
					work = filepath.Join(root, work)
				}
			}
			if !isWithinRoot(root, work) {
				work = root
			}
		}
		return &SandboxFS{
			rootDir:       root,
			workDir:       work,
			mode:          ConfinementLexical,
			unlimitedRead: cfg.UnlimitedRead,
		}
	}

	rootDir := cfg.RootDir
	if abs, err := filepath.Abs(rootDir); err == nil {
		rootDir = canonicalizeLongestPrefix(abs)
	}
	cleanRoot := filepath.Clean(rootDir)
	cleanWork := cleanRoot
	if strings.TrimSpace(cfg.WorkDir) != "" {
		if absWork, err := filepath.Abs(cfg.WorkDir); err == nil {
			cleanWork = canonicalizeLongestPrefix(absWork)
		} else {
			cleanWork = filepath.Clean(cfg.WorkDir)
		}
		if !isWithinRoot(cleanRoot, cleanWork) {
			cleanWork = cleanRoot
		}
	}
	return &SandboxFS{
		rootDir:       cleanRoot,
		workDir:       cleanWork,
		mode:          ConfinementOSRoot,
		unlimitedRead: cfg.UnlimitedRead,
	}
}

// RootDir returns the normalized confinement root directory.
func (s *SandboxFS) RootDir() string {
	return s.rootDir
}

// WorkDir returns the normalized default working directory for relative paths.
func (s *SandboxFS) WorkDir() string {
	return s.workDir
}

// Mode returns the configured ConfinementMode.
func (s *SandboxFS) Mode() ConfinementMode {
	return s.mode
}

// ResolveSandboxDirs validates rootDir and workDir confinement (creating both directories when create is true).
func ResolveSandboxDirs(rootDir, workDir string, create bool) (string, string, error) {
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
	if isWithinRoot(origAbsRoot, absWork) {
		if rel, relErr := filepath.Rel(origAbsRoot, absWork); relErr == nil {
			absWork = filepath.Join(absRoot, rel)
		}
	}
	if !isWithinRoot(absRoot, absWork) {
		return "", "", fmt.Errorf("workdir %q escapes root-dir %q", absWork, absRoot)
	}
	resolvedWork, err := NewSandboxFS(absRoot, absRoot).ResolvePath(absWork)
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
	if root == string(filepath.Separator) && filepath.IsAbs(target) {
		return true
	}
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

func rejectNULByte(path string) error {
	if strings.Contains(path, "\x00") {
		return &PathError{
			Kind:    PathErrPermissionDenied,
			Path:    path,
			Message: "access denied: path contains NUL byte",
		}
	}
	return nil
}

func isDriveRelativePath(p string) bool {
	return filepath.VolumeName(p) != "" && !filepath.IsAbs(p)
}

func isVirtualRootPath(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	return filepath.VolumeName(p) == "" && (strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`))
}

// ResolvePath resolves relPath within RootDir according to Mode.
func (s *SandboxFS) ResolvePath(relPath string) (string, error) {
	if s.mode == ConfinementLexical {
		return s.resolveLexicalPath(relPath)
	}
	return s.resolveOSRootPath(relPath)
}

func (s *SandboxFS) resolveLexicalPath(clientPath string) (string, error) {
	if err := rejectNULByte(clientPath); err != nil {
		return "", err
	}
	if isDriveRelativePath(clientPath) {
		return "", &PathError{
			Kind:    PathErrPermissionDenied,
			Path:    clientPath,
			Message: fmt.Sprintf("access denied: drive-relative path %q is not permitted", clientPath),
		}
	}
	root := filepath.Clean(s.rootDir)
	if root == "" {
		root = string(filepath.Separator)
	}
	work := s.workDir
	if work == "" {
		work = root
	}
	cleaned := filepath.Clean(clientPath)

	var candidate string
	if isVirtualRootPath(cleaned) {
		switch {
		case root == string(filepath.Separator):
			candidate = cleaned
		case isWithinRoot(root, cleaned):
			candidate = cleaned
		default:
			trimmed := strings.TrimPrefix(cleaned, filepath.VolumeName(cleaned))
			trimmed = strings.TrimLeft(trimmed, `/\`)
			candidate = filepath.Join(root, trimmed)
		}
	} else {
		candidate = filepath.Join(work, cleaned)
	}

	if !isWithinRoot(root, candidate) {
		return "", &PathError{
			Kind:    PathErrPermissionDenied,
			Path:    clientPath,
			Message: fmt.Sprintf("access denied: path %q escapes root prefix %q", clientPath, root),
		}
	}
	return candidate, nil
}

func (s *SandboxFS) lexicalOSRootCandidate(relPath string) (string, error) {
	if err := rejectNULByte(relPath); err != nil {
		return "", err
	}
	if isDriveRelativePath(relPath) {
		return "", &PathError{
			Kind:    PathErrPermissionDenied,
			Path:    relPath,
			Message: fmt.Sprintf("access denied: drive-relative path %q is not permitted", relPath),
		}
	}
	var candidate string
	if isVirtualRootPath(relPath) {
		cleaned := filepath.Clean(relPath)
		if isWithinRoot(s.rootDir, cleaned) {
			candidate = cleaned
		} else {
			trimmed := strings.TrimPrefix(cleaned, filepath.VolumeName(cleaned))
			trimmed = strings.TrimLeft(trimmed, `/\`)
			candidate = filepath.Join(s.rootDir, trimmed)
		}
	} else {
		candidate = filepath.Join(s.workDir, relPath)
	}
	if !isWithinRoot(s.rootDir, candidate) {
		return "", &PathError{
			Kind:    PathErrPermissionDenied,
			Path:    relPath,
			Message: fmt.Sprintf("access denied: path %q escapes root directory", relPath),
		}
	}
	return candidate, nil
}

func (s *SandboxFS) resolveOSRootPath(relPath string) (string, error) {
	candidate, err := s.lexicalOSRootCandidate(relPath)
	if err != nil {
		return "", err
	}

	cur := candidate
	var suffix []string
	for {
		if eval, err := filepath.EvalSymlinks(cur); err == nil {
			resolved := eval
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
				if fi, lerr := os.Lstat(resolved); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
					return "", &PathError{
						Kind:    PathErrPermissionDenied,
						Path:    relPath,
						Message: fmt.Sprintf("access denied: dangling symlink %q is not permitted", relPath),
					}
				}
			}
			if !isWithinRoot(s.rootDir, resolved) {
				return "", &PathError{
					Kind:    PathErrPermissionDenied,
					Path:    relPath,
					Message: fmt.Sprintf("access denied: symlink target for %q escapes root directory", relPath),
				}
			}
			return resolved, nil
		}
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", &PathError{
				Kind:    PathErrPermissionDenied,
				Path:    relPath,
				Message: fmt.Sprintf("access denied: dangling symlink %q is not permitted", relPath),
			}
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

func reanchorPathError(err error, origPath, internalPath string) error {
	var pe *PathError
	if errors.As(err, &pe) {
		return &PathError{
			Kind:    pe.Kind,
			Path:    origPath,
			Message: strings.ReplaceAll(pe.Message, internalPath, origPath),
			Err:     pe.Err,
		}
	}
	return err
}

func (s *SandboxFS) resolveOSRootNoFollowLeaf(relPath string) (string, error) {
	candidate, err := s.lexicalOSRootCandidate(relPath)
	if err != nil {
		return "", err
	}
	if filepath.Clean(candidate) == s.rootDir {
		return s.rootDir, nil
	}
	parentDir := filepath.Dir(candidate)
	parentResolved, err := s.resolveOSRootPath(parentDir)
	if err != nil {
		return "", reanchorPathError(err, relPath, parentDir)
	}
	leafFull := filepath.Join(parentResolved, filepath.Base(candidate))
	if fi, lerr := os.Lstat(leafFull); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if _, err := s.resolveOSRootPath(leafFull); err != nil {
			return "", reanchorPathError(err, relPath, leafFull)
		}
	}
	return leafFull, nil
}

func (s *SandboxFS) openInRoot(relPath string) (*os.Root, string, error) {
	resolved, err := s.resolveOSRootPath(relPath)
	if err != nil {
		return nil, "", err
	}
	rel, _ := filepath.Rel(s.rootDir, resolved)
	root, err := os.OpenRoot(s.rootDir)
	if err != nil {
		return nil, "", ClassifyOSError(err, relPath)
	}
	return root, rel, nil
}

func (s *SandboxFS) openInRootNoFollowLeaf(relPath string) (*os.Root, string, error) {
	resolved, err := s.resolveOSRootNoFollowLeaf(relPath)
	if err != nil {
		return nil, "", err
	}
	rel, _ := filepath.Rel(s.rootDir, resolved)
	root, err := os.OpenRoot(s.rootDir)
	if err != nil {
		return nil, "", ClassifyOSError(err, relPath)
	}
	return root, rel, nil
}

// OpenRead opens path for reading after validating confinement and checking that it is a regular file.
func (s *SandboxFS) OpenRead(path string) (*os.File, error) {
	if path == "" {
		return nil, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: "path is required"}
	}
	var (
		f   *os.File
		err error
	)
	if s.mode == ConfinementLexical {
		targetPath, resErr := s.resolveLexicalPath(path)
		if resErr != nil {
			return nil, resErr
		}
		if fi, statErr := os.Stat(targetPath); statErr == nil {
			if fi.IsDir() {
				return nil, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: fmt.Sprintf("cannot read directory %q as file", path)}
			}
			if fi.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0 {
				return nil, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: fmt.Sprintf("cannot read non-regular file %q", path)}
			}
		}
		f, err = os.Open(targetPath)
	} else {
		root, rel, rootErr := s.openInRoot(path)
		if rootErr != nil {
			return nil, rootErr
		}
		defer root.Close()
		if fi, statErr := root.Lstat(rel); statErr == nil && !fi.Mode().IsRegular() {
			return nil, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: fmt.Sprintf("cannot read non-regular file %q", path)}
		}
		f, err = root.Open(rel)
	}
	if err != nil {
		return nil, ClassifyOSError(err, path)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, ClassifyOSError(err, path)
	}
	if s.mode == ConfinementLexical {
		if info.IsDir() {
			_ = f.Close()
			return nil, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: fmt.Sprintf("cannot read directory %q as file", path)}
		}
		if info.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0 {
			_ = f.Close()
			return nil, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: fmt.Sprintf("cannot read non-regular file %q", path)}
		}
	} else if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: fmt.Sprintf("cannot read non-regular file %q", path)}
	}
	return f, nil
}

// OpenWrite opens path for writing, creating parent directories with 0755 permissions as needed.
// When perm is non-zero, the file's permissions are explicitly set to perm (including when overwriting an existing file).
func (s *SandboxFS) OpenWrite(path string, perm os.FileMode, overwrite bool) (*os.File, error) {
	return s.openWriteFile(path, perm, overwrite, false)
}

func (s *SandboxFS) openWriteFile(path string, perm os.FileMode, overwrite, appendMode bool) (*os.File, error) {
	if path == "" {
		return nil, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: "path is required"}
	}
	effectivePerm := perm
	if effectivePerm == 0 {
		effectivePerm = 0o644
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

	var (
		f   *os.File
		err error
	)
	if s.mode == ConfinementLexical {
		safePath, resErr := s.resolveLexicalPath(path)
		if resErr != nil {
			return nil, resErr
		}
		if mkErr := os.MkdirAll(filepath.Dir(safePath), 0o755); mkErr != nil {
			return nil, ClassifyOSError(mkErr, path)
		}
		f, err = os.OpenFile(safePath, flags, effectivePerm)
	} else {
		root, rel, rootErr := s.openInRoot(path)
		if rootErr != nil {
			return nil, rootErr
		}
		defer root.Close()
		if fi, statErr := root.Lstat(rel); statErr == nil && !fi.Mode().IsRegular() {
			return nil, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: fmt.Sprintf("cannot write non-regular file %q", path)}
		}
		if mkErr := root.MkdirAll(filepath.Dir(rel), 0o755); mkErr != nil {
			return nil, ClassifyOSError(mkErr, path)
		}
		f, err = root.OpenFile(rel, flags, effectivePerm)
	}
	if err != nil {
		return nil, ClassifyOSError(err, path)
	}
	if perm != 0 {
		if chmodErr := f.Chmod(effectivePerm); chmodErr != nil {
			_ = f.Close()
			return nil, ClassifyOSError(chmodErr, path)
		}
	}
	return f, nil
}

// ReadRegularFile reads a file under confinement, enforcing MaxReadFileBytes unless UnlimitedRead is set.
func (s *SandboxFS) ReadRegularFile(path string) ([]byte, error) {
	f, err := s.OpenRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if s.unlimitedRead {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, ClassifyOSError(err, path)
		}
		return data, nil
	}

	data, exceeded, err := readLimitedAll(f, MaxReadFileBytes)
	if err != nil {
		return nil, ClassifyOSError(err, path)
	}
	if exceeded {
		return nil, fmt.Errorf("file %q exceeds maximum full-read size (%d bytes); specify start_line and end_line", path, MaxReadFileBytes)
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

// ReadFileRange reads 1-indexed inclusive lines [startLine, endLine] from path.
func (s *SandboxFS) ReadFileRange(path string, startLine, endLine int) (string, error) {
	if s.unlimitedRead {
		raw, err := s.ReadRegularFile(path)
		if err != nil {
			return "", err
		}
		return sliceLines(string(raw), startLine, endLine), nil
	}

	f, err := s.OpenRead(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	var selected []string
	totalBytes := 0
	for lineNum := 1; endLine <= 0 || lineNum <= endLine; lineNum++ {
		part, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", ClassifyOSError(readErr, path)
		}
		if part == "" {
			break
		}
		line := strings.TrimSuffix(strings.TrimSuffix(part, "\n"), "\r")
		if lineNum >= startLine {
			totalBytes += len(line) + 1
			if totalBytes > MaxReadFileBytes {
				return "", fmt.Errorf("line range [%d, %d] of %q exceeds maximum read size (%d bytes); narrow start_line and end_line", max(startLine, 1), endLine, path, MaxReadFileBytes)
			}
			selected = append(selected, line)
		}
	}
	return strings.Join(selected, "\n"), nil
}

func sliceLines(text string, startLine, endLine int) string {
	lines := strings.Split(text, "\n")
	if startLine <= 1 {
		startLine = 1
	}
	if startLine > len(lines) {
		return ""
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if endLine < startLine {
		return ""
	}
	return strings.Join(lines[startLine-1:endLine], "\n")
}

// WriteRegularFile writes content to path with optional overwrite or append semantics.
func (s *SandboxFS) WriteRegularFile(path, content string, overwrite, appendMode bool) (int, error) {
	f, err := s.openWriteFile(path, 0, overwrite, appendMode)
	if err != nil {
		return 0, err
	}
	n, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if writeErr != nil {
		return n, ClassifyOSError(writeErr, path)
	}
	if closeErr != nil {
		return n, ClassifyOSError(closeErr, path)
	}
	return n, nil
}

// Stat returns FileMetadata for path without following a leaf symlink.
func (s *SandboxFS) Stat(path string) (FileMetadata, error) {
	if path == "" {
		return FileMetadata{}, &PathError{Kind: PathErrInvalidArgument, Path: path, Message: "path is required"}
	}
	if s.mode == ConfinementLexical {
		targetPath, err := s.resolveLexicalPath(path)
		if err != nil {
			return FileMetadata{}, err
		}
		info, err := os.Lstat(targetPath)
		if err != nil {
			return FileMetadata{}, ClassifyOSError(err, path)
		}
		return buildFileMetadata(info, func() (string, error) { return os.Readlink(targetPath) }), nil
	}

	root, rel, err := s.openInRootNoFollowLeaf(path)
	if err != nil {
		return FileMetadata{}, err
	}
	defer root.Close()

	info, err := root.Lstat(rel)
	if err != nil {
		return FileMetadata{}, ClassifyOSError(err, path)
	}
	return buildFileMetadata(info, func() (string, error) { return root.Readlink(rel) }), nil
}

// Mkdir creates a directory (or directory tree when recursive is true) under confinement.
func (s *SandboxFS) Mkdir(path string, perm os.FileMode, recursive bool) error {
	if path == "" {
		return &PathError{Kind: PathErrInvalidArgument, Path: path, Message: "path is required"}
	}
	if perm == 0 {
		perm = 0o755
	}
	if s.mode == ConfinementLexical {
		targetPath, err := s.resolveLexicalPath(path)
		if err != nil {
			return err
		}
		if recursive {
			return ClassifyOSError(os.MkdirAll(targetPath, perm), path)
		}
		return ClassifyOSError(os.Mkdir(targetPath, perm), path)
	}

	root, rel, err := s.openInRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()

	if recursive {
		return ClassifyOSError(root.MkdirAll(rel, perm), path)
	}
	return ClassifyOSError(root.Mkdir(rel, perm), path)
}

// Remove deletes a file or directory under confinement, rejecting removal of RootDir or WorkDir.
func (s *SandboxFS) Remove(path string, recursive bool) error {
	if strings.TrimSpace(path) == "" {
		return &PathError{
			Kind:    PathErrPermissionDenied,
			Path:    path,
			Message: "cannot remove empty path",
		}
	}
	if s.mode == ConfinementLexical {
		targetPath, err := s.resolveLexicalPath(path)
		if err != nil {
			return err
		}
		if targetPath == s.rootDir || targetPath == s.workDir {
			return &PathError{
				Kind:    PathErrPermissionDenied,
				Path:    path,
				Message: fmt.Sprintf("cannot remove workspace root or working directory: %s", path),
			}
		}
		fi, statErr := os.Lstat(targetPath)
		if statErr != nil {
			return ClassifyOSError(statErr, path)
		}
		if recursive && fi.Mode()&os.ModeSymlink == 0 {
			return ClassifyOSError(os.RemoveAll(targetPath), path)
		}
		return ClassifyOSError(os.Remove(targetPath), path)
	}

	root, rel, err := s.openInRootNoFollowLeaf(path)
	if err != nil {
		return err
	}
	defer root.Close()
	if rel == "." || filepath.Join(s.rootDir, rel) == s.workDir {
		return &PathError{
			Kind:    PathErrPermissionDenied,
			Path:    path,
			Message: fmt.Sprintf("cannot remove workspace root or working directory: %s", path),
		}
	}
	fi, statErr := root.Lstat(rel)
	if statErr != nil {
		return ClassifyOSError(statErr, path)
	}
	if recursive && fi.Mode()&os.ModeSymlink == 0 {
		return ClassifyOSError(root.RemoveAll(rel), path)
	}
	return ClassifyOSError(root.Remove(rel), path)
}

// ReadDir lists FileMetadata entries in directory path in lexicographical order.
func (s *SandboxFS) ReadDir(path string) ([]FileMetadata, error) {
	if s.mode == ConfinementLexical {
		targetPath, err := s.resolveLexicalPath(path)
		if err != nil {
			return nil, err
		}
		dirEntries, err := os.ReadDir(targetPath)
		if err != nil {
			return nil, ClassifyOSError(err, path)
		}
		out := make([]FileMetadata, 0, len(dirEntries))
		for _, entry := range dirEntries {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			fullEntryPath := filepath.Join(targetPath, entry.Name())
			out = append(out, buildFileMetadata(info, func() (string, error) { return os.Readlink(fullEntryPath) }))
		}
		return out, nil
	}

	root, rel, err := s.openInRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	dirFile, err := root.Open(rel)
	if err != nil {
		return nil, ClassifyOSError(err, path)
	}
	defer dirFile.Close()

	dirEntries, err := dirFile.ReadDir(-1)
	if err != nil {
		return nil, ClassifyOSError(err, path)
	}
	slices.SortFunc(dirEntries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	out := make([]FileMetadata, 0, len(dirEntries))
	for _, entry := range dirEntries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		entryRel := filepath.Join(rel, entry.Name())
		out = append(out, buildFileMetadata(info, func() (string, error) { return root.Readlink(entryRel) }))
	}
	return out, nil
}

// Rename moves or renames oldPath to newPath under confinement, creating destination parent directories as needed.
func (s *SandboxFS) Rename(oldPath, newPath string) error {
	if oldPath == "" || newPath == "" {
		return &PathError{Kind: PathErrInvalidArgument, Path: oldPath, Message: "old_path and new_path are required"}
	}
	if s.mode == ConfinementLexical {
		resolvedOld, err := s.resolveLexicalPath(oldPath)
		if err != nil {
			return err
		}
		resolvedNew, err := s.resolveLexicalPath(newPath)
		if err != nil {
			return err
		}
		if resolvedOld == s.rootDir || resolvedNew == s.rootDir || resolvedOld == s.workDir || resolvedNew == s.workDir {
			return &PathError{
				Kind:    PathErrPermissionDenied,
				Path:    oldPath,
				Message: "cannot rename workspace root or working directory",
			}
		}
		if _, statErr := os.Lstat(resolvedOld); statErr != nil {
			return ClassifyOSError(statErr, oldPath)
		}
		if err := os.MkdirAll(filepath.Dir(resolvedNew), 0o755); err != nil {
			return ClassifyOSError(err, newPath)
		}
		return ClassifyOSError(os.Rename(resolvedOld, resolvedNew), oldPath)
	}

	resolvedOld, err := s.resolveOSRootNoFollowLeaf(oldPath)
	if err != nil {
		return err
	}
	resolvedNew, err := s.resolveOSRootNoFollowLeaf(newPath)
	if err != nil {
		return err
	}
	relOld, _ := filepath.Rel(s.rootDir, resolvedOld)
	relNew, _ := filepath.Rel(s.rootDir, resolvedNew)
	if relOld == "." || relNew == "." || resolvedOld == s.workDir || resolvedNew == s.workDir {
		return &PathError{
			Kind:    PathErrPermissionDenied,
			Path:    oldPath,
			Message: "cannot rename workspace root or working directory",
		}
	}
	root, err := os.OpenRoot(s.rootDir)
	if err != nil {
		return ClassifyOSError(err, oldPath)
	}
	defer root.Close()
	if _, statErr := root.Lstat(relOld); statErr != nil {
		return ClassifyOSError(statErr, oldPath)
	}
	if err := root.MkdirAll(filepath.Dir(relNew), 0o755); err != nil {
		return ClassifyOSError(err, newPath)
	}
	return ClassifyOSError(root.Rename(relOld, relNew), oldPath)
}

func buildFileMetadata(fi os.FileInfo, readlink func() (string, error)) FileMetadata {
	var kind FileKind
	var symlinkTarget string
	mode := fi.Mode()

	switch {
	case mode&os.ModeSymlink != 0:
		kind = FileKindSymlink
		if readlink != nil {
			if target, err := readlink(); err == nil {
				symlinkTarget = target
			}
		}
	case fi.IsDir():
		kind = FileKindDirectory
	case mode&os.ModeNamedPipe != 0:
		kind = FileKindFIFOPipe
	case mode&os.ModeSocket != 0:
		kind = FileKindSocket
	case mode&os.ModeDevice != 0:
		if mode&os.ModeCharDevice != 0 {
			kind = FileKindCharDevice
		} else {
			kind = FileKindBlockDevice
		}
	default:
		kind = FileKindRegular
	}

	return FileMetadata{
		Name:          fi.Name(),
		Kind:          kind,
		IsDir:         fi.IsDir(),
		Size:          fi.Size(),
		ModTime:       fi.ModTime(),
		Permissions:   uint32(mode.Perm()),
		SymlinkTarget: symlinkTarget,
	}
}

// ListEntries returns tool-formatted directory entries (name, is_dir, size) for path.
func (s *SandboxFS) ListEntries(path string) ([]any, error) {
	entries, err := s.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, map[string]any{
			"name":   entry.Name,
			"is_dir": entry.IsDir,
			"size":   entry.Size,
		})
	}
	return out, nil
}

func (s *SandboxFS) readGrepCandidate(osRoot *os.Root, p string) ([]byte, bool) {
	if osRoot == nil {
		data, err := s.ReadRegularFile(p)
		return data, err == nil
	}
	resolved, err := s.resolveOSRootPath(p)
	if err != nil {
		return nil, false
	}
	rel, err := filepath.Rel(s.rootDir, resolved)
	if err != nil {
		return nil, false
	}
	fi, err := osRoot.Lstat(rel)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	f, err := osRoot.Open(rel)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if s.unlimitedRead {
		data, readErr := io.ReadAll(f)
		return data, readErr == nil
	}
	data, exceeded, readErr := readLimitedAll(f, MaxReadFileBytes)
	return data, readErr == nil && !exceeded
}

// Grep searches regular files under searchPath for query, returning matches and whether output was truncated.
func (s *SandboxFS) Grep(searchPath, query string, caseInsensitive, isRegex bool) ([]any, bool, error) {
	fullPath, err := s.ResolvePath(searchPath)
	if err != nil {
		return nil, false, err
	}
	if query == "" {
		return nil, false, errors.New("query is required")
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

	var osRoot *os.Root
	if s.mode != ConfinementLexical {
		osRoot, err = os.OpenRoot(s.rootDir)
		if err != nil {
			return nil, false, ClassifyOSError(err, searchPath)
		}
		defer osRoot.Close()
	}

	var matches []any
	totalBytes := 0
	err = filepath.WalkDir(fullPath, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if d.Type()&(fs.ModeNamedPipe|fs.ModeSocket|fs.ModeDevice|fs.ModeCharDevice) != 0 {
			return nil
		}
		data, ok := s.readGrepCandidate(osRoot, p)
		if !ok {
			return nil
		}

		rel, relErr := filepath.Rel(fullPath, p)
		if relErr != nil || rel == "." {
			rel = filepath.Base(p)
		}
		slashRel := filepath.ToSlash(rel)
		for idx, rawLine := range strings.Split(string(data), "\n") {
			line := strings.TrimSuffix(rawLine, "\r")
			if re.MatchString(line) {
				entryBytes := len(slashRel) + len(line) + 32
				if !s.unlimitedRead && (len(matches) >= MaxGrepMatches || (len(matches) > 0 && totalBytes+entryBytes > MaxGrepOutputBytes)) {
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

// FindFiles walks searchDir and returns relative paths matching pattern, optional maxDepth, and optional extensions.
func (s *SandboxFS) FindFiles(searchDir, pattern string, maxDepth int, extensions ...string) ([]any, bool, error) {
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
		if _, resErr := s.ResolvePath(p); resErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(fullPath, p)
		if relErr != nil {
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
			if !s.unlimitedRead && len(files) >= MaxFindFiles {
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
