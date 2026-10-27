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
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("os.Symlink unsupported in this Windows environment: %v", err)
		}
		t.Fatalf("os.Symlink(%q, %q) failed: %v", target, link, err)
	}
}

func TestResolveSandboxDirs(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	workDir := filepath.Join(rootDir, "proj")
	// create=false validates confinement without mutating disk (used before --dry-run).
	if _, _, err := ResolveSandboxDirs(rootDir, workDir, false); err != nil {
		t.Fatalf("ResolveSandboxDirs(non-existent, create=false) = %v, want nil", err)
	}
	if _, statErr := os.Stat(rootDir); !os.IsNotExist(statErr) {
		t.Errorf("ResolveSandboxDirs(create=false) unexpectedly created %q", rootDir)
	}
	gotRoot, gotWork, err := ResolveSandboxDirs(rootDir, workDir, true)
	if err != nil {
		t.Fatalf("ResolveSandboxDirs(create=true) failed: %v", err)
	}
	if !strings.HasPrefix(gotWork, gotRoot) {
		t.Errorf("gotWork %q does not reside inside gotRoot %q", gotWork, gotRoot)
	}

	outside := t.TempDir()
	if _, _, err := ResolveSandboxDirs(gotRoot, outside, false); err == nil {
		t.Error("ResolveSandboxDirs(outside workdir) = nil, want escape error")
	}

	// Sibling prefix directory (/tmp/.../root2 vs /tmp/.../root) must be rejected.
	siblingPrefix := gotRoot + "2"
	if err := os.MkdirAll(siblingPrefix, 0o755); err != nil {
		t.Fatalf("MkdirAll(siblingPrefix) failed: %v", err)
	}
	if _, _, err := ResolveSandboxDirs(gotRoot, siblingPrefix, false); err == nil {
		t.Errorf("ResolveSandboxDirs(%q, %q) = nil, want sibling-prefix escape error", gotRoot, siblingPrefix)
	}

	// Symlinked rootDir with workDir under symlinked rootDir canonicalizes both paths.
	symlinkRoot := filepath.Join(base, "sym-root")
	mustSymlink(t, gotRoot, symlinkRoot)
	canonRoot, canonWork, err := ResolveSandboxDirs(symlinkRoot, filepath.Join(symlinkRoot, "proj"), false)
	if err != nil || canonRoot != gotRoot || canonWork != gotWork {
		t.Errorf("ResolveSandboxDirs(symlink) = (%q, %q, %v), want (%q, %q, nil)", canonRoot, canonWork, err, gotRoot, gotWork)
	}
	symFS := NewSandboxFS(symlinkRoot, filepath.Join(symlinkRoot, "proj"))
	if symFS.RootDir() != gotRoot || symFS.WorkDir() != gotWork {
		t.Errorf("NewSandboxFS(symlink) = (%q, %q), want (%q, %q)", symFS.RootDir(), symFS.WorkDir(), gotRoot, gotWork)
	}

	// Symlink inside rootDir pointing to a subdirectory inside rootDir canonicalizes workDir.
	innerWorkLink := filepath.Join(gotRoot, "proj_link")
	mustSymlink(t, gotWork, innerWorkLink)
	_, canonInnerWork, err := ResolveSandboxDirs(gotRoot, innerWorkLink, false)
	if err != nil || canonInnerWork != gotWork {
		t.Errorf("ResolveSandboxDirs(innerWorkLink) = (%q, %v), want (%q, nil)", canonInnerWork, err, gotWork)
	}

	// Symlink inside rootDir pointing outside rootDir cannot be used as workDir.
	escapeWorkLink := filepath.Join(gotRoot, "sym_escape_work")
	mustSymlink(t, outside, escapeWorkLink)
	if _, _, err := ResolveSandboxDirs(gotRoot, escapeWorkLink, false); err == nil {
		t.Error("ResolveSandboxDirs(workDir symlink escaping rootDir) = nil, want escape error")
	}

	// Dangling symlink as rootDir (both direct and nested under dangling prefix) must be rejected.
	danglingRoot := filepath.Join(base, "dangling_root")
	mustSymlink(t, filepath.Join(base, "does_not_exist"), danglingRoot)
	for _, tc := range []struct {
		name   string
		root   string
		create bool
	}{
		{name: "direct_no_create", root: danglingRoot, create: false},
		{name: "direct_create", root: danglingRoot, create: true},
		{name: "nested_no_create", root: filepath.Join(danglingRoot, "nested"), create: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ResolveSandboxDirs(tc.root, "", tc.create); err == nil || !strings.Contains(err.Error(), "dangling symlink") {
				t.Errorf("ResolveSandboxDirs(%q, create=%v) = %v, want dangling symlink error", tc.root, tc.create, err)
			}
		})
	}
}

func TestSandboxFSConfinementModesAndOperations(t *testing.T) {
	modes := []struct {
		name string
		mode ConfinementMode
	}{
		{name: "os_root", mode: ConfinementOSRoot},
		{name: "lexical", mode: ConfinementLexical},
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			rootDir := t.TempDir()
			workDir := filepath.Join(rootDir, "sub")
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				t.Fatalf("MkdirAll failed: %v", err)
			}
			outsideDir := t.TempDir()
			outsideSecret := filepath.Join(outsideDir, "secret.txt")
			if err := os.WriteFile(outsideSecret, []byte("top-secret"), 0o600); err != nil {
				t.Fatalf("WriteFile(outsideSecret) failed: %v", err)
			}

			fs := NewSandboxFSWithConfig(SandboxFSConfig{
				RootDir: rootDir,
				WorkDir: workDir,
				Mode:    tc.mode,
			})
			if fs.Mode() != tc.mode {
				t.Errorf("fs.Mode() = %v, want %v", fs.Mode(), tc.mode)
			}
			if fs.RootDir() == "" || fs.WorkDir() == "" {
				t.Errorf("RootDir=%q WorkDir=%q, want non-empty", fs.RootDir(), fs.WorkDir())
			}

			// Valid parent traversal within rootDir (`../root_note.txt` from `rootDir/sub`).
			if _, err := fs.WriteRegularFile("../root_note.txt", "in-root\n", false, false); err != nil {
				t.Fatalf("WriteRegularFile(../root_note.txt) failed: %v", err)
			}
			if _, err := fs.WriteRegularFile("../root_note.txt", "dup", false, false); err == nil {
				t.Error("WriteRegularFile(overwrite=false existing) = nil, want PathErrAlreadyExists")
			} else {
				var pe *PathError
				if !errors.As(err, &pe) || pe.Kind != PathErrAlreadyExists {
					t.Errorf("existing file error = %v, want PathErrAlreadyExists", err)
				}
			}
			if _, err := fs.WriteRegularFile("../root_note.txt", "more\n", false, true); err != nil {
				t.Fatalf("WriteRegularFile(appendMode=true) failed: %v", err)
			}
			data, err := fs.ReadRegularFile("/root_note.txt")
			if err != nil || string(data) != "in-root\nmore\n" {
				t.Errorf("ReadRegularFile(/root_note.txt) = (%q, %v), want \"in-root\\nmore\\n\"", data, err)
			}

			// Relative traversal outside rootDir must fail with PathErrPermissionDenied.
			if _, err := fs.ReadRegularFile("../../escape.txt"); err == nil {
				t.Error("ReadRegularFile(../../escape.txt) succeeded, want confinement error")
			}
			if _, err := fs.WriteRegularFile("../../escape.txt", "x", true, false); err == nil {
				t.Error("WriteRegularFile(../../escape.txt) succeeded, want confinement error")
			}

			// Absolute paths outside rootDir (including sibling-prefix paths like rootDir+"2/...") are mapped into the virtual root inside rootDir, never touching host files.
			if _, err := fs.ReadRegularFile(outsideSecret); err == nil {
				t.Errorf("ReadRegularFile(%q) succeeded, want not-found inside virtual root", outsideSecret)
			} else {
				var pe *PathError
				if !errors.As(err, &pe) || pe.Kind != PathErrNotFound {
					t.Errorf("ReadRegularFile(missing) error = %v, want PathErrNotFound", err)
				}
			}
			if _, err := fs.WriteRegularFile("/virtual_etc/config", "virtual", true, false); err != nil {
				t.Fatalf("WriteRegularFile(/virtual_etc/config) failed: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(fs.RootDir(), "virtual_etc", "config")); statErr != nil {
				t.Errorf("virtual absolute write did not land inside rootDir: %v", statErr)
			}
			siblingTarget := fs.RootDir() + "2" + string(filepath.Separator) + "sib.txt"
			if _, err := fs.WriteRegularFile(siblingTarget, "virtual-sib", true, false); err != nil {
				t.Fatalf("WriteRegularFile(siblingTarget) failed: %v", err)
			}
			if _, statErr := os.Stat(fs.RootDir() + "2"); !os.IsNotExist(statErr) {
				t.Errorf("sibling-prefix path escaped rootDir onto host disk: statErr=%v", statErr)
			}

			// NUL byte and empty-path guards.
			if _, err := fs.ResolvePath("bad\x00name"); err == nil {
				t.Error("ResolvePath(NUL) = nil, want PathErrPermissionDenied")
			}
			if _, err := fs.OpenRead(""); err == nil {
				t.Error("OpenRead(\"\") = nil, want PathErrInvalidArgument")
			}
			if _, err := fs.OpenWrite("", 0o644, true); err == nil {
				t.Error("OpenWrite(\"\") = nil, want PathErrInvalidArgument")
			}
			if _, err := fs.Stat(""); err == nil {
				t.Error("Stat(\"\") = nil, want PathErrInvalidArgument")
			}
			if err := fs.Mkdir("", 0o755, true); err == nil {
				t.Error("Mkdir(\"\") = nil, want PathErrInvalidArgument")
			}
			if err := fs.Rename("", "dst"); err == nil {
				t.Error("Rename(\"\", \"dst\") = nil, want PathErrInvalidArgument")
			}
			if err := fs.Rename("src", ""); err == nil {
				t.Error("Rename(\"src\", \"\") = nil, want PathErrInvalidArgument")
			}

			// In ConfinementOSRoot mode, symlinks escaping rootDir and dangling symlinks are rejected across all operations.
			if tc.mode == ConfinementOSRoot {
				escapeLink := filepath.Join(workDir, "escape_link")
				mustSymlink(t, outsideDir, escapeLink)
				danglingLink := filepath.Join(workDir, "dangling_link")
				mustSymlink(t, filepath.Join(outsideDir, "missing_target"), danglingLink)

				realTargetDir := filepath.Join(rootDir, "real_target")
				if err := os.MkdirAll(realTargetDir, 0o755); err != nil {
					t.Fatalf("MkdirAll(realTargetDir) failed: %v", err)
				}
				mustSymlink(t, filepath.Join(rootDir, "missing_leaf"), filepath.Join(realTargetDir, "dangling_leaf"))

				loopA := filepath.Join(workDir, "loop_a")
				loopB := filepath.Join(workDir, "loop_b")
				mustSymlink(t, loopB, loopA)
				mustSymlink(t, loopA, loopB)

				for _, esc := range []struct {
					name string
					path string
				}{
					{name: "symlink_existing_target", path: "escape_link/secret.txt"},
					{name: "symlink_nonexistent_leaf", path: "escape_link/new_subdir/pwn.txt"},
					{name: "dangling_symlink", path: "dangling_link"},
					{name: "dangling_suffix_under_real_dir", path: filepath.Join("..", "real_target", "dangling_leaf", "sub")},
					{name: "symlink_loop", path: "loop_a/file.txt"},
				} {
					if got, err := fs.ResolvePath(esc.path); err == nil {
						t.Errorf("ResolvePath(%s=%q) = %q, want confinement/symlink error", esc.name, esc.path, got)
					}
				}
				if _, err := fs.ReadRegularFile("escape_link/secret.txt"); err == nil {
					t.Error("ReadRegularFile(escape symlink) succeeded, want error")
				}
				if _, err := fs.WriteRegularFile("escape_link/new.txt", "bad", true, false); err == nil {
					t.Error("WriteRegularFile(escape symlink) succeeded, want error")
				}
				if _, err := fs.WriteRegularFile("dangling_link", "bad", true, false); err == nil {
					t.Error("WriteRegularFile(dangling symlink) succeeded, want error")
				}
				if _, err := fs.OpenWrite("/real_target", 0o644, true); err == nil || !strings.Contains(err.Error(), "cannot write non-regular file") {
					t.Errorf("OpenWrite(directory in ConfinementOSRoot) = %v, want non-regular file error", err)
				}
				if _, err := fs.OpenRead("/real_target"); err == nil || !strings.Contains(err.Error(), "cannot read non-regular file") {
					t.Errorf("OpenRead(directory in ConfinementOSRoot) = %v, want non-regular file error", err)
				}
				if _, err := fs.Stat("escape_link/secret.txt"); err == nil {
					t.Error("Stat(escape symlink) succeeded in ConfinementOSRoot, want error")
				}
				if err := fs.Mkdir("escape_link/escaped_dir", 0o755, true); err == nil {
					t.Error("Mkdir(escape symlink) succeeded in ConfinementOSRoot, want error")
				}
				if err := fs.Remove("escape_link/secret.txt", false); err == nil {
					t.Error("Remove(escape symlink) succeeded in ConfinementOSRoot, want error")
				}
				if _, statErr := os.Stat(outsideSecret); statErr != nil {
					t.Errorf("outsideSecret was unexpectedly removed: %v", statErr)
				}
				if err := fs.Rename("escape_link/secret.txt", "stolen.txt"); err == nil {
					t.Error("Rename(from escape symlink) succeeded in ConfinementOSRoot, want error")
				}
				if err := fs.Rename("../root_note.txt", "escape_link/stolen.txt"); err == nil {
					t.Error("Rename(to escape symlink) succeeded in ConfinementOSRoot, want error")
				}
				if _, err := fs.ReadDir("escape_link"); err == nil {
					t.Error("ReadDir(escape symlink) succeeded in ConfinementOSRoot, want error")
				}

				// Intra-root symlinks in ConfinementOSRoot: Stat reports FileKindSymlink + SymlinkTarget,
				// Rename moves the symlink itself, and Remove(recursive=true) unlinks a directory symlink without deleting its target directory.
				intraTargetFile := filepath.Join(workDir, "intra_target.txt")
				if err := os.WriteFile(intraTargetFile, []byte("keep-me"), 0o644); err != nil {
					t.Fatalf("WriteFile(intraTargetFile) failed: %v", err)
				}
				intraFileLink := filepath.Join(workDir, "intra_file_link.txt")
				mustSymlink(t, "intra_target.txt", intraFileLink)
				symMeta, err := fs.Stat("intra_file_link.txt")
				if err != nil || symMeta.Kind != FileKindSymlink || symMeta.SymlinkTarget != "intra_target.txt" {
					t.Errorf("Stat(intra_file_link.txt) = (%+v, %v), want FileKindSymlink target=intra_target.txt", symMeta, err)
				}
				if err := fs.Rename("intra_file_link.txt", "renamed_file_link.txt"); err != nil {
					t.Fatalf("Rename(intra_file_link.txt) failed: %v", err)
				}
				if _, lerr := os.Lstat(filepath.Join(workDir, "intra_file_link.txt")); !os.IsNotExist(lerr) {
					t.Errorf("expected intra_file_link.txt moved, lstat err=%v", lerr)
				}
				if err := fs.Remove("renamed_file_link.txt", false); err != nil {
					t.Fatalf("Remove(renamed_file_link.txt) failed: %v", err)
				}
				if gotKeep, rerr := os.ReadFile(intraTargetFile); rerr != nil || string(gotKeep) != "keep-me" {
					t.Errorf("intraTargetFile after link rename/remove = (%q, %v)", gotKeep, rerr)
				}

				keptDir := filepath.Join(workDir, "kept_dir")
				if err := os.MkdirAll(keptDir, 0o755); err != nil {
					t.Fatalf("MkdirAll(keptDir) failed: %v", err)
				}
				keptFile := filepath.Join(keptDir, "inside.txt")
				if err := os.WriteFile(keptFile, []byte("survives"), 0o644); err != nil {
					t.Fatalf("WriteFile(keptFile) failed: %v", err)
				}
				mustSymlink(t, "kept_dir", filepath.Join(workDir, "dir_link"))
				if err := fs.Remove("dir_link", true); err != nil {
					t.Fatalf("Remove(dir_link, recursive=true) failed: %v", err)
				}
				if _, statKept := os.Stat(keptFile); statKept != nil {
					t.Errorf("Remove(dir_link, recursive=true) deleted symlink target contents: %v", statKept)
				}
				_ = os.RemoveAll(keptDir)
				_ = os.Remove(intraTargetFile)
			} else {
				// ConfinementLexical intentionally permits symlinks inside already-isolated container sandboxes
				// (such as /bin -> /usr/bin) while still rejecting lexical '..' traversal outside rootDir.
				containerTarget := filepath.Join(outsideDir, "secret.txt")
				lexicalLink := filepath.Join(workDir, "container_link.txt")
				mustSymlink(t, containerTarget, lexicalLink)
				if got, err := fs.ReadRegularFile("container_link.txt"); err != nil || string(got) != "top-secret" {
					t.Errorf("ConfinementLexical ReadRegularFile(symlink) = (%q, %v), want (\"top-secret\", nil)", got, err)
				}
				if _, err := fs.WriteRegularFile("container_link.txt", "top-secret", true, false); err != nil {
					t.Errorf("ConfinementLexical WriteRegularFile(symlink) = %v, want nil", err)
				}
				extDirLink := filepath.Join(workDir, "ext_dir_link")
				mustSymlink(t, outsideDir, extDirLink)
				if err := fs.Mkdir("ext_dir_link/lex_sub", 0o755, true); err != nil {
					t.Errorf("ConfinementLexical Mkdir(symlink) = %v, want nil", err)
				}
				extWrite, err := fs.OpenWrite("ext_dir_link/lex_sub/out.txt", 0o644, true)
				if err != nil {
					t.Fatalf("ConfinementLexical OpenWrite(symlink) = %v, want nil", err)
				}
				_, _ = extWrite.Write([]byte("lex-data"))
				_ = extWrite.Close()
				if meta, err := fs.Stat("ext_dir_link/lex_sub/out.txt"); err != nil || meta.Size != 8 {
					t.Errorf("ConfinementLexical Stat(symlink) = (%+v, %v), want Size=8", meta, err)
				}
				if entries, err := fs.ReadDir("ext_dir_link/lex_sub"); err != nil || len(entries) != 1 {
					t.Errorf("ConfinementLexical ReadDir(symlink) = (%+v, %v), want 1 entry", entries, err)
				}
				if err := fs.Rename("ext_dir_link/lex_sub/out.txt", "ext_dir_link/lex_sub/renamed.txt"); err != nil {
					t.Errorf("ConfinementLexical Rename(symlink) = %v, want nil", err)
				}
				if err := fs.Remove("ext_dir_link/lex_sub/renamed.txt", false); err != nil {
					t.Errorf("ConfinementLexical Remove(symlink) = %v, want nil", err)
				}
				var dirWritePE *PathError
				if _, err := fs.OpenWrite("ext_dir_link/lex_sub", 0o644, true); !errors.As(err, &dirWritePE) || dirWritePE.Kind != PathErrInternal {
					t.Errorf("ConfinementLexical OpenWrite(directory) = %v, want PathErrInternal", err)
				}
				var dirReadPE *PathError
				if _, err := fs.OpenRead("ext_dir_link/lex_sub"); !errors.As(err, &dirReadPE) || dirReadPE.Kind != PathErrInvalidArgument {
					t.Errorf("ConfinementLexical OpenRead(directory) = %v, want PathErrInvalidArgument", err)
				}
			}

			// OpenWrite & OpenRead round-trip + explicit permission updates on overwrite.
			wFile, err := fs.OpenWrite("streamed.txt", 0o644, false)
			if err != nil {
				t.Fatalf("OpenWrite(streamed.txt) failed: %v", err)
			}
			_, _ = wFile.Write([]byte("stream-payload"))
			_ = wFile.Close()
			if _, err := fs.OpenWrite("streamed.txt", 0o644, false); err == nil {
				t.Error("OpenWrite(overwrite=false existing) = nil, want error")
			}
			rFile, err := fs.OpenRead("streamed.txt")
			if err != nil {
				t.Fatalf("OpenRead(streamed.txt) failed: %v", err)
			}
			streamRead, _ := io.ReadAll(rFile)
			_ = rFile.Close()
			if string(streamRead) != "stream-payload" {
				t.Errorf("OpenRead(streamed.txt) = %q, want \"stream-payload\"", streamRead)
			}
			if runtime.GOOS != "windows" {
				cFile, err := fs.OpenWrite("chmod_target.sh", 0o600, false)
				if err != nil {
					t.Fatalf("OpenWrite(chmod_target.sh, 0600) failed: %v", err)
				}
				_ = cFile.Close()
				if m600, err := fs.Stat("chmod_target.sh"); err != nil || m600.Permissions != 0o600 {
					t.Errorf("Stat(chmod_target.sh initial) = (%#o, %v), want 0o600", m600.Permissions, err)
				}
				cFile755, err := fs.OpenWrite("chmod_target.sh", 0o755, true)
				if err != nil {
					t.Fatalf("OpenWrite(chmod_target.sh, 0755, overwrite=true) failed: %v", err)
				}
				_ = cFile755.Close()
				if m755, err := fs.Stat("chmod_target.sh"); err != nil || m755.Permissions != 0o755 {
					t.Errorf("Stat(chmod_target.sh after 0755 overwrite) = (%#o, %v), want 0o755", m755.Permissions, err)
				}
				cFileZero, err := fs.OpenWrite("chmod_target.sh", 0, true)
				if err != nil {
					t.Fatalf("OpenWrite(chmod_target.sh, 0, overwrite=true) failed: %v", err)
				}
				_ = cFileZero.Close()
				if _, err := fs.WriteRegularFile("chmod_target.sh", "#!/bin/sh\necho ok\n", true, false); err != nil {
					t.Fatalf("WriteRegularFile(chmod_target.sh, overwrite=true) failed: %v", err)
				}
				if mPreserved, err := fs.Stat("chmod_target.sh"); err != nil || mPreserved.Permissions != 0o755 {
					t.Errorf("Stat(chmod_target.sh after perm=0/WriteRegularFile overwrite) = (%#o, %v), want preserved 0o755", mPreserved.Permissions, err)
				}
			}

			// CRLF ReadFileRange, Grep, FindFiles, Stat, Mkdir, Rename, ListEntries, Remove.
			crlfText := "alpha\r\nbeta = 42\r\ngamma\r\n"
			if _, err := fs.WriteRegularFile("cfg/app.ini", crlfText, true, false); err != nil {
				t.Fatalf("WriteRegularFile(cfg/app.ini) failed: %v", err)
			}
			ranged, err := fs.ReadFileRange("cfg/app.ini", 2, 3)
			if err != nil || ranged != "beta = 42\ngamma" {
				t.Errorf("ReadFileRange() = (%q, %v), want (\"beta = 42\\ngamma\", nil)", ranged, err)
			}
			if emptyRange, err := fs.ReadFileRange("cfg/app.ini", 3, 2); err != nil || emptyRange != "" {
				t.Errorf("ReadFileRange(start > end) = (%q, %v), want (\"\", nil)", emptyRange, err)
			}

			matches, truncated, err := fs.Grep(".", "BETA = 42$", true, true)
			if err != nil || truncated || len(matches) != 1 {
				t.Errorf("Grep() = (%v, %v, %v), want 1 non-truncated match", matches, truncated, err)
			}
			if litMatches, _, err := fs.Grep(".", "beta = 42$", false, false); err != nil || len(litMatches) != 0 {
				t.Errorf("Grep(isRegex=false, \"beta = 42$\") = (%v, %v), want 0 literal matches", litMatches, err)
			}
			if _, _, err := fs.Grep(".", "[", false, true); err == nil {
				t.Error("Grep(invalid regex) = nil, want error")
			}
			if _, _, err := fs.Grep(".", "", false, false); err == nil {
				t.Error("Grep(empty query) = nil, want error")
			}

			found, _, err := fs.FindFiles(".", "*", 3, "INI")
			if err != nil || len(found) != 1 || found[0] != "cfg/app.ini" {
				t.Errorf("FindFiles(INI) = (%v, %v), want [cfg/app.ini]", found, err)
			}
			foundEmptyExt, _, err := fs.FindFiles(".", "app.ini", 3, "", " . ")
			if err != nil || len(foundEmptyExt) != 1 {
				t.Errorf("FindFiles(empty extensions) = (%v, %v), want 1 match", foundEmptyExt, err)
			}

			meta, err := fs.Stat("cfg/app.ini")
			if err != nil || meta.IsDir || meta.Kind != FileKindRegular {
				t.Errorf("Stat(cfg/app.ini) = (%+v, %v)", meta, err)
			}
			if err := fs.Mkdir("missing_parent_nonrec/child", 0o755, false); err == nil {
				t.Error("Mkdir(missing parent, recursive=false) = nil, want error")
			}
			if err := fs.Mkdir("default_perm_dir", 0, false); err != nil {
				t.Fatalf("Mkdir(perm=0) failed: %v", err)
			}
			if _, err := fs.WriteRegularFile("default_perm_dir/probe.txt", "ok", true, false); err != nil {
				t.Fatalf("WriteRegularFile(default_perm_dir/probe.txt) failed: %v", err)
			}
			if err := fs.Mkdir("nested/a/b", 0o755, true); err != nil {
				t.Fatalf("Mkdir(recursive) failed: %v", err)
			}
			if err := fs.Rename("no_such_src.txt", "ghost_parent/dst.txt"); err == nil {
				t.Error("Rename(non-existent source) = nil, want error")
			}
			if _, statErr := os.Stat(filepath.Join(workDir, "ghost_parent")); !os.IsNotExist(statErr) {
				t.Errorf("Rename(non-existent source) created ghost_parent: statErr=%v", statErr)
			}
			if err := fs.Rename("cfg/app.ini", "moved/app.ini"); err != nil {
				t.Fatalf("Rename() failed: %v", err)
			}
			if _, err := fs.WriteRegularFile("moved/aaa.ini", "first", true, false); err != nil {
				t.Fatalf("WriteRegularFile(moved/aaa.ini) failed: %v", err)
			}
			entries, err := fs.ListEntries("moved")
			if err != nil || len(entries) != 2 || entries[0].(map[string]any)["name"] != "aaa.ini" || entries[1].(map[string]any)["name"] != "app.ini" {
				t.Errorf("ListEntries(moved) = (%v, %v), want sorted [aaa.ini, app.ini]", entries, err)
			}
			if _, ok := entries[0].(map[string]any)["size"].(int64); !ok {
				t.Errorf("ListEntries size type = %T, want int64", entries[0].(map[string]any)["size"])
			}
			dirMetas, err := fs.ReadDir("moved")
			if err != nil || len(dirMetas) != 2 || dirMetas[0].Name != "aaa.ini" || dirMetas[1].Name != "app.ini" {
				t.Errorf("ReadDir(moved) = (%+v, %v), want sorted [aaa.ini, app.ini]", dirMetas, err)
			}
			if err := fs.Remove("moved", false); err == nil {
				t.Error("Remove(non-empty dir, recursive=false) = nil, want error")
			}
			if err := fs.Remove("moved", true); err != nil {
				t.Errorf("Remove(moved, recursive=true) failed: %v", err)
			}
			if err := fs.Remove("no_such_entry", true); err == nil {
				t.Error("Remove(non-existent, recursive=true) = nil, want error")
			}
			if err := fs.Remove("", true); err == nil {
				t.Error("Remove(\"\", recursive=true) = nil, want PathErrInvalidArgument")
			}
			if err := fs.Remove(".", true); err == nil {
				t.Error("Remove(\".\") = nil, want workDir removal protection error")
			}
			if err := fs.Rename(".", "moved_work"); err == nil {
				t.Error("Rename(\".\", ...) = nil, want workDir rename protection error")
			}
			if err := fs.Remove("..", true); err == nil {
				t.Error("Remove(..) = nil, want root removal protection error")
			}
			if runtime.GOOS != "windows" {
				fifoPath := filepath.Join(workDir, "test_pipe.fifo")
				if mkErr := exec.Command("mkfifo", fifoPath).Run(); mkErr == nil {
					fifoDone := make(chan error, 1)
					go func() {
						_, rerr := fs.OpenRead("test_pipe.fifo")
						fifoDone <- rerr
					}()
					select {
					case fifoErr := <-fifoDone:
						if fifoErr == nil {
							t.Error("OpenRead(FIFO) = nil, want non-regular file error without blocking")
						}
					case <-time.After(time.Second):
						t.Fatal("OpenRead(FIFO) blocked on named pipe without writer")
					}
					grepDone := make(chan error, 1)
					go func() {
						_, _, gerr := fs.Grep(".", "anything", false, false)
						grepDone <- gerr
					}()
					select {
					case gerr := <-grepDone:
						if gerr != nil {
							t.Errorf("Grep(dir containing FIFO) = %v, want nil", gerr)
						}
					case <-time.After(time.Second):
						t.Fatal("Grep() blocked on named pipe")
					}
					_ = os.Remove(fifoPath)
				}
			}
		})
	}

	// Verify ConfinementOSRoot returns structured errors when rootDir does not exist on disk.
	uncreatedFS := NewSandboxFS(filepath.Join(t.TempDir(), "uncreated_root"), "")
	if _, err := uncreatedFS.OpenRead("x.txt"); err == nil {
		t.Error("uncreatedFS.OpenRead() = nil, want error")
	}
	if err := uncreatedFS.Rename("a.txt", "b.txt"); err == nil {
		t.Error("uncreatedFS.Rename() = nil, want error")
	}

	// Verify bounded vs UnlimitedRead behavior above MaxReadFileBytes.
	bigDir := t.TempDir()
	bigPayload := strings.Repeat("x", MaxReadFileBytes+128)
	if err := os.WriteFile(filepath.Join(bigDir, "big.bin"), []byte(bigPayload), 0o644); err != nil {
		t.Fatalf("WriteFile(big.bin) failed: %v", err)
	}
	boundedFS := NewSandboxFS(bigDir, bigDir)
	if _, err := boundedFS.ReadRegularFile("big.bin"); err == nil {
		t.Error("boundedFS.ReadRegularFile(>MaxReadFileBytes) = nil, want size limit error")
	}
	limitSrc := strings.NewReader("0123456789ABCDEF")
	if _, exceeded, err := readLimitedAll(limitSrc, 10); err != nil || !exceeded || limitSrc.Len() != 5 {
		t.Errorf("readLimitedAll(10) = (exceeded=%v, err=%v, remaining=%d), want (true, nil, 5)", exceeded, err, limitSrc.Len())
	}

	halfLine := strings.Repeat("a", MaxReadFileBytes/2)
	twoHalfLines := halfLine + "\n" + halfLine + "\n"
	if err := os.WriteFile(filepath.Join(bigDir, "two_lines.txt"), []byte(twoHalfLines), 0o644); err != nil {
		t.Fatalf("WriteFile(two_lines.txt) failed: %v", err)
	}
	if _, err := boundedFS.ReadFileRange("two_lines.txt", 1, 2); err == nil {
		t.Error("boundedFS.ReadFileRange(two half-max lines + newlines > MaxReadFileBytes) = nil, want size limit error")
	}
	exactHalfLine := strings.Repeat("b", MaxReadFileBytes/2-1)
	exactTwoLines := exactHalfLine + "\n" + exactHalfLine + "\n"
	if err := os.WriteFile(filepath.Join(bigDir, "exact_two_lines.txt"), []byte(exactTwoLines), 0o644); err != nil {
		t.Fatalf("WriteFile(exact_two_lines.txt) failed: %v", err)
	}
	if gotExact, err := boundedFS.ReadFileRange("exact_two_lines.txt", 1, 2); err != nil || len(gotExact) != MaxReadFileBytes-1 {
		t.Errorf("boundedFS.ReadFileRange(exact MaxReadFileBytes) len=%d err=%v, want len=%d", len(gotExact), err, MaxReadFileBytes-1)
	}
	unlimitedFS := NewSandboxFSWithConfig(SandboxFSConfig{
		RootDir:       bigDir,
		WorkDir:       bigDir,
		Mode:          ConfinementLexical,
		UnlimitedRead: true,
	})
	gotBig, err := unlimitedFS.ReadRegularFile("big.bin")
	if err != nil || len(gotBig) != len(bigPayload) {
		t.Errorf("unlimitedFS.ReadRegularFile(big.bin) len=%d err=%v, want len=%d", len(gotBig), err, len(bigPayload))
	}

	// Verify ClassifyOSError mappings (including true untyped nil error) and PathError.Error() formatting.
	if err := ClassifyOSError(nil, "p"); err != nil {
		t.Errorf("ClassifyOSError(nil) = %#v, want nil error interface", err)
	}
	var pe *PathError
	if err := ClassifyOSError(os.ErrNotExist, "missing"); !errors.As(err, &pe) || pe.Kind != PathErrNotFound || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "missing") {
		t.Errorf("ClassifyOSError(ErrNotExist) = %+v", err)
	}
	if err := ClassifyOSError(os.ErrExist, "dup"); !errors.As(err, &pe) || pe.Kind != PathErrAlreadyExists {
		t.Errorf("ClassifyOSError(ErrExist) = %+v", err)
	}
	if err := ClassifyOSError(os.ErrPermission, "denied"); !errors.As(err, &pe) || pe.Kind != PathErrPermissionDenied {
		t.Errorf("ClassifyOSError(ErrPermission) = %+v", err)
	}
	if got := (&PathError{Message: "msg-with-err", Err: errors.New("inner")}).Error(); got != "msg-with-err: inner" {
		t.Errorf("PathError(Message+Err).Error() = %q, want \"msg-with-err: inner\"", got)
	}
	if got := (&PathError{Message: "msg-only"}).Error(); got != "msg-only" {
		t.Errorf("PathError(Message only).Error() = %q, want \"msg-only\"", got)
	}
	if got := (&PathError{Err: errors.New("err-only")}).Error(); got != "err-only" {
		t.Errorf("PathError(Err only).Error() = %q, want \"err-only\"", got)
	}
	if got := (&PathError{Path: "p.txt"}).Error(); got != `filesystem error on "p.txt"` {
		t.Errorf("PathError(Path only).Error() = %q, want %q", got, `filesystem error on "p.txt"`)
	}
}

func TestSandboxShellEnvBuffersAndSubprocessKill(t *testing.T) {
	var defaultMaxBuf CappedBuffer
	if n, err := defaultMaxBuf.Write([]byte("abc")); err != nil || n != 3 || defaultMaxBuf.String() != "abc" {
		t.Errorf("CappedBuffer(default MaxBytes=0) = (%d, %v, %q), want (3, nil, \"abc\")", n, err, defaultMaxBuf.String())
	}

	var exactBuf CappedBuffer
	exactBuf.MaxBytes = 5
	_, _ = exactBuf.Write([]byte("12345"))
	if got := exactBuf.String(); got != "12345" {
		t.Errorf("CappedBuffer(exact MaxBytes) = %q, want \"12345\"", got)
	}
	_, _ = exactBuf.Write([]byte("6"))
	if got := exactBuf.String(); got != "12345\n...[output truncated at 4 MiB]..." {
		t.Errorf("CappedBuffer(overflow after exact MaxBytes) = %q, want \"12345\\n...[output truncated at 4 MiB]...\"", got)
	}

	var singleOverflowBuf CappedBuffer
	singleOverflowBuf.MaxBytes = 5
	_, _ = singleOverflowBuf.Write([]byte("123456"))
	if got := singleOverflowBuf.String(); got != "12345\n...[output truncated at 4 MiB]..." {
		t.Errorf("CappedBuffer(single write > MaxBytes) = %q, want \"12345\\n...[output truncated at 4 MiB]...\"", got)
	}

	allowEnv := BuildAllowlistEnv([]string{
		"PATH=/usr/bin",
		"LC_ALL=C.UTF-8",
		"COMPUTERNAME=WIN-NODE",
		"USERNAME=runner",
		"OS=Windows_NT",
		"SYSTEMDRIVE=C:",
		"PSMODULEPATH=C:\\Modules",
		"GEMINI_API_KEY=secret",
		"UNLISTED=drop",
	})
	if len(allowEnv) != 7 {
		t.Errorf("BuildAllowlistEnv() = %v, want 7 entries", allowEnv)
	}

	if emptyScrub := FilterScrubbedEnv(nil); emptyScrub == nil || len(emptyScrub) != 0 {
		t.Errorf("FilterScrubbedEnv(nil) = %#v, want non-nil empty slice", emptyScrub)
	}
	if allFiltered := FilterScrubbedEnv([]string{"GEMINI_API_KEY=secret", "GEMINI_LEASE_TOKEN=tok"}); allFiltered == nil || len(allFiltered) != 0 {
		t.Errorf("FilterScrubbedEnv(all credentials) = %#v, want non-nil empty slice", allFiltered)
	}

	scrubbed := FilterScrubbedEnv([]string{
		"CUSTOM_SAFE=keep",
		"GEMINI_API_KEY=drop1",
		"GEMINI_ENVIRONMENT_KEY=drop2",
		"GOOGLE_API_KEY=drop3",
		"GEMINI_LEASE_TOKEN=drop4",
	}, "EXTRA_VAR=ok")
	joined := strings.Join(scrubbed, "\n")
	if !strings.Contains(joined, "CUSTOM_SAFE=keep") || !strings.Contains(joined, "EXTRA_VAR=ok") || strings.Contains(joined, "drop") {
		t.Errorf("FilterScrubbedEnv() = %v", scrubbed)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	if bin, args := ResolveShellCommand("windows", "dir"); bin != "cmd.exe" || len(args) != 2 || args[0] != "/C" {
		t.Errorf("ResolveShellCommand(windows, empty PATH) = (%q, %v), want (\"cmd.exe\", [\"/C\", \"dir\"])", bin, args)
	}
	if !isWindowsWSLStubBash("windows", `C:\Windows\System32\bash.exe`) || !isWindowsWSLStubBash("windows", `c:/windows/sysnative/bash.exe`) || isWindowsWSLStubBash("windows", `C:\Program Files\Git\bin\bash.exe`) || isWindowsWSLStubBash("linux", "/usr/bin/bash") {
		t.Error("isWindowsWSLStubBash classification mismatch")
	}
	if bin, args := resolveExecShell("windows", true, "dir"); bin != "cmd.exe" || len(args) != 2 || args[0] != "/C" {
		t.Errorf("resolveExecShell(windows, useBinSh=true, empty PATH) = (%q, %v), want (\"cmd.exe\", [\"/C\", \"dir\"])", bin, args)
	}
	if bin, _ := ResolveShellCommand("linux", "ls"); bin != "sh" {
		t.Errorf("ResolveShellCommand(linux, empty PATH) = %q, want sh", bin)
	}
	t.Setenv("PATH", origPath)
	if len(ShutdownSignals()) == 0 {
		t.Error("ShutdownSignals() returned empty slice")
	}

	workDir := filepath.Join(t.TempDir(), "auto-work")
	sh := NewSandboxShellWithConfig(SandboxShellConfig{
		WorkDir:           workDir,
		EnvPolicy:         EnvPolicyScrubCredentials,
		ExtraEnv:          []string{"INJECTED_FLAG=present"},
		AutoCreateWorkDir: true,
		UseBinSh:          true,
	})
	if sh.WorkDir() != workDir {
		t.Errorf("sh.WorkDir() = %q, want %q", sh.WorkDir(), workDir)
	}

	t.Setenv("GEMINI_API_KEY", "should-not-leak")
	stdout, stderr, exitCode, execErr := sh.ExecuteBash(t.Context(), `printf "%s|%s|%s" "$INJECTED_FLAG" "$GEMINI_API_KEY" "$(pwd)"`, "", 5*time.Second)
	if execErr != nil || stdout != "present||"+workDir || stderr != "" || exitCode != 0 {
		t.Errorf("ExecuteBash(env+cwd check) = (%q, %q, %d, %v), want stdout=%q exitCode=0", stdout, stderr, exitCode, execErr, "present||"+workDir)
	}
	scrubNoExtraSh := NewSandboxShellWithConfig(SandboxShellConfig{
		WorkDir:   workDir,
		EnvPolicy: EnvPolicyScrubCredentials,
		UseBinSh:  true,
	})
	noExtraOut, _, noExtraCode, noExtraErr := scrubNoExtraSh.ExecuteBash(t.Context(), `printf "%s" "$GEMINI_API_KEY"`, "", 5*time.Second)
	if noExtraErr != nil || noExtraCode != 0 || noExtraOut != "" {
		t.Errorf("scrubNoExtraSh.ExecuteBash() = (%q, %d, %v), want (\"\", 0, nil)", noExtraOut, noExtraCode, noExtraErr)
	}

	// Verify AutoCreateWorkDir=false fails when workDir does not exist, EnvPolicyAllowlist drops unlisted env vars,
	// and UseBinSh=false uses ResolveShellCommand (preferring bash on PATH).
	noAutoSh := NewSandboxShell(filepath.Join(t.TempDir(), "does-not-exist"))
	if _, _, _, err := noAutoSh.ExecuteBash(t.Context(), "echo hi", "", 5*time.Second); err == nil {
		t.Error("ExecuteBash(AutoCreateWorkDir=false, missing workDir) = nil, want error")
	}
	allowSh := NewSandboxShellWithConfig(SandboxShellConfig{
		WorkDir:   workDir,
		EnvPolicy: EnvPolicyAllowlist,
		ExtraEnv:  []string{"EXTRA_ALLOW=present"},
	})
	t.Setenv("CUSTOM_UNLISTED_ENV", "secret-val")
	if runtime.GOOS != "windows" {
		fakeBinDir := t.TempDir()
		fakeBash := filepath.Join(fakeBinDir, "bash")
		if err := os.WriteFile(fakeBash, []byte("#!/bin/sh\nprintf \"via-bash:%s|%s\" \"$CUSTOM_UNLISTED_ENV\" \"$EXTRA_ALLOW\"\n"), 0o755); err != nil {
			t.Fatalf("WriteFile(fakeBash) failed: %v", err)
		}
		t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+origPath)
		if bin, _ := resolveExecShell("linux", true, "ls"); bin != "/bin/sh" {
			t.Errorf("resolveExecShell(linux, useBinSh=true) = %q, want /bin/sh", bin)
		}
		if bin, _ := resolveExecShell("linux", false, "ls"); bin != fakeBash {
			t.Errorf("resolveExecShell(linux, useBinSh=false) = %q, want %q", bin, fakeBash)
		}
		allowOut, _, allowCode, allowErr := allowSh.ExecuteBash(t.Context(), "ignored", "", 5*time.Second)
		if allowErr != nil || allowCode != 0 || allowOut != "via-bash:|present" {
			t.Errorf("allowSh.ExecuteBash() = (%q, %d, %v), want (\"via-bash:|present\", 0, nil)", allowOut, allowCode, allowErr)
		}
		binShOut, _, binShCode, binShErr := sh.ExecuteBash(t.Context(), `printf "direct-bin-sh"`, "", 5*time.Second)
		if binShErr != nil || binShCode != 0 || binShOut != "direct-bin-sh" {
			t.Errorf("sh.ExecuteBash(UseBinSh=true with fakeBash on PATH) = (%q, %d, %v), want (\"direct-bin-sh\", 0, nil)", binShOut, binShCode, binShErr)
		}
		t.Setenv("PATH", origPath)
	}

	// Verify already-canceled or already-expired parent ctx returns ctx.Err() without formatting as a command timeout.
	canceledCtx, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	_, _, cancelCode, cancelErr := sh.ExecuteBash(canceledCtx, "echo hi", "", 5*time.Second)
	if !errors.Is(cancelErr, context.Canceled) || cancelCode != -1 || strings.Contains(cancelErr.Error(), "timed out") {
		t.Errorf("ExecuteBash(canceledCtx) = (code=%d, err=%v), want context.Canceled without 'timed out'", cancelCode, cancelErr)
	}
	expiredCtx, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	_, _, expCode, expErr := sh.ExecuteBash(expiredCtx, "echo hi", "", 5*time.Second)
	if !errors.Is(expErr, context.DeadlineExceeded) || expCode != -1 || strings.Contains(expErr.Error(), "timed out") {
		t.Errorf("ExecuteBash(expiredCtx) = (code=%d, err=%v), want context.DeadlineExceeded without 'timed out'", expCode, expErr)
	}

	_, failStderr, failCode, failErr := sh.ExecuteBash(t.Context(), "echo err >&2; exit 7", "", 5*time.Second)
	if failErr != nil || failCode != 7 || !strings.Contains(failStderr, "err") {
		t.Errorf("ExecuteBash(exit 7) = (stderr=%q, code=%d, err=%v)", failStderr, failCode, failErr)
	}

	// Verify timeout kills background grandchild holding stdout pipe without hanging.
	start := time.Now()
	_, _, toCode, toErr := sh.ExecuteBash(t.Context(), "(sleep 30) & sleep 30", "", 80*time.Millisecond)
	if toErr == nil || toCode != -1 || !strings.Contains(toErr.Error(), "timed out") {
		t.Errorf("ExecuteBash(timeout) = (code=%d, err=%v), want timed out error", toCode, toErr)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ExecuteBash(timeout) took %v, subprocess group did not terminate promptly", elapsed)
	}
}

func TestToolDispatcherToolsAndExtendedFS(t *testing.T) {
	rootDir := t.TempDir()
	fs := NewSandboxFS(rootDir, rootDir)
	sh := NewSandboxShell(rootDir)

	disabled := NewToolDispatcher(ToolDispatcherConfig{
		FS:          fs,
		Shell:       sh,
		DisableBash: true,
	})
	if res, isErr := disabled.Dispatch(t.Context(), "execute_bash", map[string]any{"command": "echo hi"}); !isErr || !strings.Contains(res["error"].(string), "disabled") {
		t.Errorf("Dispatch(execute_bash disabled) = (%v, %v), want disabled error", res, isErr)
	}
	if _, isErr := disabled.Dispatch(t.Context(), "stat_file", map[string]any{"path": "."}); !isErr {
		t.Error("Dispatch(stat_file with EnableExtendedFSTools=false) = false, want error")
	}

	var gotTimeout time.Duration
	td := NewToolDispatcher(ToolDispatcherConfig{
		FS:    fs,
		Shell: sh,
		ShellExec: func(_ context.Context, cmd, cwd string, timeout time.Duration) (string, string, int, error) {
			gotTimeout = timeout
			return cmd + "@" + filepath.Base(cwd), "", 0, nil
		},
		CommandTimeout:              2 * time.Second,
		EnableExtendedFSTools:       true,
		DefaultWriteToFileOverwrite: true,
		RespectWaitMsBeforeAsync:    true,
	})

	bashRes, bashErr := td.Dispatch(t.Context(), "run_shell_command", map[string]any{
		"CommandLine":       "ls",
		"WaitMsBeforeAsync": float64(500),
	})
	wantBashOut := "ls@" + filepath.Base(rootDir)
	if bashErr || gotTimeout != 500*time.Millisecond || bashRes["stdout"] != wantBashOut {
		t.Errorf("Dispatch(run_shell_command) = (%v, %v), timeout=%v, want stdout=%q", bashRes, bashErr, gotTimeout, wantBashOut)
	}
	if _, escErr := td.Dispatch(t.Context(), "bash", map[string]any{"command": "pwd", "cwd": "../../outside"}); !escErr {
		t.Error("Dispatch(bash escaping cwd) = false, want error")
	}

	// File tools: write_to_file, view_file, replace_file_content, list_dir, grep_search, find_by_name, extended tools.
	// Note: "dup" appears on BOTH line 1 and line 2 with AllowMultiple=false (default), so string-typed
	// StartLine="2" MUST be parsed to 2 rather than falling back to 0 (which would fail on 2 occurrences).
	if res, isErr := td.Dispatch(t.Context(), "write_to_file", map[string]any{
		"TargetFile":  "dir/code.py",
		"CodeContent": "dup\r\ndup\r\nline3\r\n",
	}); isErr {
		t.Fatalf("write_to_file failed: %v", res)
	}
	// Verify write_file defaults to overwrite=true even when DefaultWriteToFileOverwrite is false,
	// whereas write_to_file defaults to overwrite=false when DefaultWriteToFileOverwrite is false.
	if res, isErr := disabled.Dispatch(t.Context(), "write_to_file", map[string]any{
		"TargetFile":  "dir/code.py",
		"CodeContent": "should_fail",
	}); !isErr {
		t.Errorf("disabled.Dispatch(write_to_file existing with DefaultWriteToFileOverwrite=false) = (%v, false), want error", res)
	}
	if res, isErr := disabled.Dispatch(t.Context(), "write_file", map[string]any{
		"TargetFile":  "dir/code.py",
		"CodeContent": "old",
	}); isErr {
		t.Fatalf("disabled.Dispatch(write_file existing with DefaultWriteToFileOverwrite=false) failed: %v", res)
	}
	if res, isErr := td.Dispatch(t.Context(), "write_to_file", map[string]any{
		"TargetFile":  "dir/code.py",
		"CodeContent": "dup\r\ndup\r\nline3\r\n",
	}); isErr {
		t.Fatalf("td.Dispatch(write_to_file existing with DefaultWriteToFileOverwrite=true) failed: %v", res)
	}
	if res, isErr := td.Dispatch(t.Context(), "replace_file_content", map[string]any{
		"TargetFile":         "dir/code.py",
		"TargetContent":      "dup",
		"ReplacementContent": "line2_edited",
		"StartLine":          "2",
		"EndLine":            float64(2),
	}); isErr {
		t.Fatalf("replace_file_content(string StartLine=2) failed: %v", res)
	}
	viewRes, viewErr := td.Dispatch(t.Context(), "view_file", map[string]any{"AbsolutePath": "dir/code.py"})
	if viewErr || viewRes["content"] != "dup\r\nline2_edited\r\nline3\r\n" {
		t.Errorf("view_file(full) = (%v, %v)", viewRes, viewErr)
	}
	rangeRes, rangeErr := td.Dispatch(t.Context(), "view_file", map[string]any{
		"AbsolutePath": "dir/code.py",
		"StartLine":    1,
		"EndLine":      2,
	})
	if rangeErr || rangeRes["content"] != "dup\nline2_edited" {
		t.Errorf("view_file(range) = (%v, %v)", rangeRes, rangeErr)
	}
	startOnlyRes, startOnlyErr := td.Dispatch(t.Context(), "view_file", map[string]any{
		"AbsolutePath": "dir/code.py",
		"StartLine":    2,
	})
	if startOnlyErr || startOnlyRes["content"] != "line2_edited\nline3" {
		t.Errorf("view_file(StartLine only) = (%v, %v), want \"line2_edited\\nline3\"", startOnlyRes, startOnlyErr)
	}
	endOnlyRes, endOnlyErr := td.Dispatch(t.Context(), "view_file", map[string]any{
		"AbsolutePath": "dir/code.py",
		"EndLine":      1,
	})
	if endOnlyErr || endOnlyRes["content"] != "dup" {
		t.Errorf("view_file(EndLine only) = (%v, %v), want \"dup\"", endOnlyRes, endOnlyErr)
	}

	// Verify ReplaceInText preserves LF-only line endings, normalizes multi-line LF targets against CRLF content, and handles explicit CRLF targets.
	lfOut, lfCount, lfErr := ReplaceInText("alpha\nbeta\n", "alpha", "GAMMA", 0, 1, false)
	if lfErr != nil || lfCount != 1 || lfOut != "GAMMA\nbeta\n" {
		t.Errorf("ReplaceInText(LF-only, startLine=0, endLine=1) = (%q, %d, %v), want (\"GAMMA\\nbeta\\n\", 1, nil)", lfOut, lfCount, lfErr)
	}
	crlfOut, crlfCount, crlfErr := ReplaceInText("line1\r\nline2\r\nline3\r\n", "line1\nline2", "new1\r\nnew2", 1, 2, false)
	if crlfErr != nil || crlfCount != 1 || crlfOut != "new1\r\nnew2\r\nline3\r\n" {
		t.Errorf("ReplaceInText(multi-line LF target in CRLF content) = (%q, %d, %v), want (\"new1\\r\\nnew2\\r\\nline3\\r\\n\", 1, nil)", crlfOut, crlfCount, crlfErr)
	}
	rawCRLFOut, rawCRLFCount, rawCRLFErr := ReplaceInText("line1\r\nline2\r\n", "line1\r\nline2", "new1\r\nnew2", 0, 0, false)
	if rawCRLFErr != nil || rawCRLFCount != 1 || rawCRLFOut != "new1\r\nnew2\r\n" {
		t.Errorf("ReplaceInText(explicit CRLF target in CRLF content) = (%q, %d, %v), want (\"new1\\r\\nnew2\\r\\n\", 1, nil)", rawCRLFOut, rawCRLFCount, rawCRLFErr)
	}

	if res, isErr := td.Dispatch(t.Context(), "stat_file", map[string]any{"path": "dir/code.py"}); isErr || res["is_dir"] != false {
		t.Errorf("stat_file = (%v, %v)", res, isErr)
	} else if _, ok := res["size"].(int64); !ok {
		t.Errorf("stat_file size type = %T, want int64", res["size"])
	}
	nilFSTools := NewToolDispatcher(ToolDispatcherConfig{EnableExtendedFSTools: true})
	if res, isErr := nilFSTools.Dispatch(t.Context(), "view_file", map[string]any{"path": "a.txt"}); !isErr || !strings.Contains(res["error"].(string), "not configured") {
		t.Errorf("nilFSTools.Dispatch(view_file) = (%v, %v), want not configured error", res, isErr)
	}
	// toolMkdir defaults recursive=true, so passing string "false" on a missing parent MUST fail if string coercion works.
	if res, isErr := td.Dispatch(t.Context(), "mkdir", map[string]any{"path": "missing_parent/child", "recursive": "false"}); !isErr {
		t.Errorf("mkdir(recursive=\"false\" with missing parent) = (%v, false), want error", res)
	}
	if res, isErr := td.Dispatch(t.Context(), "mkdir", map[string]any{"path": "newdir/sub", "recursive": "true"}); isErr {
		t.Errorf("mkdir = (%v, %v)", res, isErr)
	}
	if res, isErr := td.Dispatch(t.Context(), "rename_file", map[string]any{"old_path": "dir/code.py", "new_path": "newdir/code.py"}); isErr {
		t.Errorf("rename_file = (%v, %v)", res, isErr)
	}
	if res, isErr := td.Dispatch(t.Context(), "list_dir", map[string]any{"DirectoryPath": "newdir"}); isErr {
		t.Errorf("list_dir = (%v, %v)", res, isErr)
	}
	if res, isErr := td.Dispatch(t.Context(), "grep_search", map[string]any{"SearchPath": ".", "Query": "line2_edited"}); isErr {
		t.Errorf("grep_search = (%v, %v)", res, isErr)
	}
	if res, isErr := td.Dispatch(t.Context(), "find_by_name", map[string]any{"SearchDirectory": ".", "Extensions": "rs,py"}); isErr {
		t.Errorf("find_by_name(comma Extensions=\"rs,py\") = (%v, %v)", res, isErr)
	} else if files, ok := res["files"].([]any); !ok || len(files) != 1 || files[0] != "newdir/code.py" {
		t.Errorf("find_by_name(comma Extensions=\"rs,py\") files = %v, want [newdir/code.py]", res["files"])
	}
	if res, isErr := td.Dispatch(t.Context(), "remove_file", map[string]any{}); !isErr {
		t.Errorf("remove_file(empty path) = (%v, false), want error", res)
	}
	if res, isErr := td.Dispatch(t.Context(), "remove_file", map[string]any{"TargetFile": "newdir", "recursive": "true"}); isErr {
		t.Errorf("remove_file(TargetFile) = (%v, %v)", res, isErr)
	}
	if _, isErr := td.Dispatch(t.Context(), "unknown_tool", nil); !isErr {
		t.Error("Dispatch(unknown_tool) = false, want error")
	}

	// Direct unit verification of intArg and boolArg string coercion and fallback behavior.
	if got := intArg(map[string]any{"n": " 17 "}, 3, "n"); got != 17 {
		t.Errorf("intArg(\" 17 \", fallback=3) = %d, want 17", got)
	}
	if got := intArg(map[string]any{"n": "not-an-int"}, 3, "n"); got != 3 {
		t.Errorf("intArg(\"not-an-int\", fallback=3) = %d, want 3", got)
	}
	if got := boolArg(map[string]any{"b": "false"}, true, "b"); got {
		t.Error("boolArg(\"false\", fallback=true) = true, want false")
	}
	if got := boolArg(map[string]any{"b": " TRUE "}, false, "b"); !got {
		t.Error("boolArg(\" TRUE \", fallback=false) = false, want true")
	}
	if got := boolArg(map[string]any{"b": "not-a-bool"}, true, "b"); !got {
		t.Error("boolArg(\"not-a-bool\", fallback=true) = false, want true")
	}
}
