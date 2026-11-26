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

package contract_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSkill(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(root, "internal", "cli", "custom", "skill", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "User~.Name") // used verbatim: no "~" expansion
	project := filepath.Join(tmp, "project")
	for _, dir := range []string{home, project} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	skillPath := func(base string, dir ...string) string {
		return filepath.Join(append(append([]string{base}, dir...), "gemini-api-cli", "SKILL.md")...)
	}
	run := func(args ...string) commandResult {
		return runCLIInDir(t, home, project, nil, append([]string{"install-skill"}, args...)...)
	}
	assertInstalled := func(paths ...string) {
		t.Helper()
		for _, path := range paths {
			if got, err := os.ReadFile(path); err != nil || string(got) != string(want) {
				t.Errorf("%s is not the embedded skill (err: %v)", path, err)
			}
		}
	}
	agents, claude := skillPath(project, ".agents", "skills"), skillPath(project, ".claude", "skills")

	// A dry run reports and writes nothing.
	result := run("--dry-run")
	if result.err != nil || result.stdout != "" || !strings.HasPrefix(result.stderr, "[DRY-RUN] would install: "+agents+" (antigravity, gemini-cli, codex, cursor)\n") {
		t.Errorf("dry run: err=%v stdout=%q stderr=%q", result.err, result.stdout, result.stderr)
	}
	if entries, _ := os.ReadDir(project); len(entries) != 0 {
		t.Fatalf("dry run wrote %v", entries)
	}

	// The default agents, in the working directory.
	result = run()
	if result.err != nil || result.stdout != agents+"\n"+claude+"\n" {
		t.Errorf("install: err=%v stdout=%q stderr=%q", result.err, result.stdout, result.stderr)
	}
	assertInstalled(agents, claude)

	// Running again repairs a stale copy.
	if err := os.WriteFile(claude, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	result = run()
	if result.stderr != "unchanged for antigravity, gemini-cli, codex, cursor\nupdated for claude-code\n" {
		t.Errorf("rerun: stderr=%q", result.stderr)
	}
	assertInstalled(claude)

	// Every agent, in the home directory, Antigravity first.
	result = run("--agent", "all", "--global")
	global := strings.Split(strings.TrimSuffix(result.stdout, "\n"), "\n")
	if result.err != nil || len(global) != 10 || global[0] != skillPath(home, ".gemini", "config", "skills") {
		t.Errorf("global: err=%v stdout=%q", result.err, result.stdout)
	}
	assertInstalled(global...)

	// --interactive off a terminal: agents 1 and 8, then scope 2 (global).
	other := filepath.Join(tmp, "other")
	result = runCLIWithStdin(t, other, nil, "1, 8\n2\n", "install-skill", "--interactive")
	if result.err != nil || result.stdout != skillPath(other, ".gemini", "config", "skills")+"\n"+skillPath(other, ".codeium", "windsurf", "skills")+"\n" {
		t.Errorf("interactive: err=%v stdout=%q stderr=%q", result.err, result.stdout, result.stderr)
	}

	result = run("--agent", "notepad")
	if exit, ok := result.err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
		t.Errorf("unknown agent: err=%v, want exit code 2", result.err)
	}
}
