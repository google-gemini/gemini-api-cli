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
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google-gemini/gemini-api-cli/internal/cli/custom/skill"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/interactive"
	"github.com/spf13/cobra"
)

// harness is a coding agent and its skills directories, slash-separated and
// relative to the project and to the home directory. They are only ever
// filepath.Join-ed onto an absolute base: no "~" expansion, no shell.
type harness struct {
	ID      string // --agent value
	Name    string
	Project string
	Global  string
	Default bool // used when no --agent is given
}

// In display order. Agents sharing a directory share one file.
var harnesses = []harness{
	{ID: "antigravity", Name: "Antigravity", Project: ".agents/skills", Global: ".gemini/config/skills", Default: true},
	{ID: "gemini-cli", Name: "Gemini CLI", Project: ".agents/skills", Global: ".gemini/skills", Default: true},
	{ID: "claude-code", Name: "Claude Code", Project: ".claude/skills", Global: ".claude/skills", Default: true},
	{ID: "codex", Name: "Codex", Project: ".agents/skills", Global: ".agents/skills", Default: true},
	{ID: "cursor", Name: "Cursor", Project: ".agents/skills", Global: ".cursor/skills", Default: true},
	{ID: "github-copilot", Name: "GitHub Copilot", Project: ".agents/skills", Global: ".copilot/skills"},
	{ID: "opencode", Name: "OpenCode", Project: ".agents/skills", Global: ".config/opencode/skills"},
	{ID: "windsurf", Name: "Windsurf", Project: ".windsurf/skills", Global: ".codeium/windsurf/skills"},
	{ID: "amp", Name: "Amp", Project: ".agents/skills", Global: ".config/agents/skills"},
	{ID: "kiro-cli", Name: "Kiro CLI", Project: ".kiro/skills", Global: ".kiro/skills"},
}

const allHarnesses = "all"

func harnessIDs() string {
	ids := make([]string, len(harnesses))
	for i, h := range harnesses {
		ids[i] = h.ID
	}
	return strings.Join(ids, ", ")
}

func newInstallSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install-skill",
		Short: "Install the gemini-api-cli agent skill for AI coding assistants",
		Long: "Install the " + skill.Name + " skill, which teaches a coding agent how to drive this CLI.\n" +
			"The skill ships inside the binary, so this works offline and matches this version.\n\n" +
			"In a terminal with no flags, pick the agents and the location from a short form.\n" +
			"With --agent or --global, or without a terminal, nothing is asked: the skill\n" +
			"goes to the named agents (default: the ones under Defaults) in the current project.\n" +
			"Add --dry-run to see what would be written without writing it. Running it again\n" +
			"updates the skill in place.\n\n" +
			"Stdout prints the path of each SKILL.md written; stderr says what happened to each.\n\n" +
			"Agents: " + harnessIDs(),
		Example: "  gemini-api install-skill                              # pick agents interactively\n" +
			"  gemini-api install-skill --agent antigravity          # this project\n" +
			"  gemini-api install-skill --agent antigravity --global # every project\n" +
			"  gemini-api install-skill --agent claude-code --dry-run  # preview, writes nothing",
		Args: cobra.NoArgs,
		RunE: runInstallSkill,
	}
	helpMeta(cmd, "project scope; agents antigravity, gemini-cli, claude-code, codex, cursor", skill.Source, "")
	cmd.Flags().StringSlice("agent", nil, "Agent to install for (repeatable or comma-separated), or \"all\"")
	cmd.Flags().Bool("global", false, "Install into your home directory, for every project")
	return cmd
}

func runInstallSkill(cmd *cobra.Command, _ []string) error {
	if usageRequested(cmd) {
		return emitUsageKDL(cmd, cmd.OutOrStdout())
	}

	agentIDs, agentsGiven := flagutil.GetStringSliceFlag(cmd, "agent")
	global, _ := flagutil.GetBoolFlag(cmd, "global")
	selected, err := selectHarnesses(agentIDs)
	if err != nil {
		return err
	}

	// Ask only for what the flags left open, and never under --dry-run.
	dryRun := isDryRun(cmd)
	mode := interactive.Resolve(cmd).SetupFormMode(agentsGiven || global)
	if mode != interactive.FormOff && !dryRun {
		linePrompts := mode == interactive.FormAccessible
		if !agentsGiven {
			if selected, err = promptHarnesses(cmd, selected, linePrompts); err != nil {
				return err
			}
			if len(selected) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "No agents selected; nothing was installed.")
				return nil
			}
		}
		if !global {
			if global, err = promptGlobal(cmd, linePrompts); err != nil {
				return err
			}
		}
	}

	// The working directory, or the home directory: used exactly as the OS reports it.
	base, err := os.Getwd()
	if global {
		base, err = os.UserHomeDir()
	}
	if err != nil {
		return runtimeError(fmt.Sprintf("cannot find the directory to install into: %v", err))
	}
	targets := skillTargets(base, global, selected)
	for i := range targets {
		if err := targets[i].write(dryRun); err != nil {
			return runtimeError(fmt.Sprintf("cannot install the skill: %v", err))
		}
	}

	// Stdout is the paths written; everything else goes to stderr.
	for _, t := range targets {
		agents := strings.Join(t.Agents, ", ")
		if dryRun {
			fmt.Fprintf(cmd.ErrOrStderr(), "[DRY-RUN] %s: %s (%s)\n", t.Status, t.Path, agents)
			continue
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "%s for %s\n", t.Status, agents)
		fmt.Fprintln(cmd.OutOrStdout(), t.Path)
	}
	if dryRun {
		fmt.Fprintln(cmd.ErrOrStderr(), "[DRY-RUN] install-skill writes local files only (no API request); nothing was changed.")
	}
	return nil
}

// selectHarnesses resolves --agent values, in display order. None means the defaults.
func selectHarnesses(ids []string) ([]harness, error) {
	want := map[string]bool{}
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == allHarnesses {
			return harnesses, nil
		}
		want[id] = true
	}
	var selected []harness
	for _, h := range harnesses {
		if want[h.ID] || (len(ids) == 0 && h.Default) {
			selected = append(selected, h)
			delete(want, h.ID)
		}
	}
	for id := range want {
		return nil, usageError(fmt.Sprintf("unknown agent %q", id), "Supported agents: "+harnessIDs()+", or \"all\"")
	}
	return selected, nil
}

// The questions go to stderr, so stdout stays the list of paths.

func promptHarnesses(cmd *cobra.Command, preselected []harness, linePrompts bool) ([]harness, error) {
	list := &multiSelect{title: "Which agents do you use?", selected: make([]bool, len(harnesses))}
	for i, h := range harnesses {
		list.options = append(list.options, h.Name)
		for _, p := range preselected {
			list.selected[i] = list.selected[i] || p.ID == h.ID
		}
	}
	if err := list.run(cmd.InOrStdin(), cmd.ErrOrStderr(), linePrompts); err != nil {
		return nil, fmt.Errorf("install-skill: %w", err)
	}
	var selected []harness
	for i, h := range harnesses {
		if list.selected[i] {
			selected = append(selected, h)
		}
	}
	return selected, nil
}

func promptGlobal(cmd *cobra.Command, linePrompts bool) (bool, error) {
	list := &multiSelect{
		title:    "Where should the skill be installed?",
		options:  []string{"This project (current directory)", "Global (your home directory, for every project)"},
		selected: []bool{true, false},
		single:   true,
	}
	if err := list.run(cmd.InOrStdin(), cmd.ErrOrStderr(), linePrompts); err != nil {
		return false, fmt.Errorf("install-skill: %w", err)
	}
	return list.selected[1], nil
}

// skillTarget is one SKILL.md and the agents that read it.
type skillTarget struct {
	Path   string
	Agents []string
	Status string
}

func skillTargets(base string, global bool, selected []harness) []skillTarget {
	var targets []skillTarget
	index := map[string]int{}
	for _, h := range selected {
		rel := h.Project
		if global {
			rel = h.Global
		}
		path := filepath.Join(base, filepath.FromSlash(rel), skill.Name, skill.File)
		i, seen := index[path]
		if !seen {
			i = len(targets)
			index[path] = i
			targets = append(targets, skillTarget{Path: path})
		}
		targets[i].Agents = append(targets[i].Agents, h.ID)
	}
	return targets
}

// write installs the skill and sets Status; a dry run only sets Status.
func (t *skillTarget) write(dryRun bool) error {
	existing, err := os.ReadFile(t.Path)
	switch {
	case err == nil && bytes.Equal(existing, skill.Content):
		t.Status = "unchanged"
		return nil
	case err == nil:
		t.Status = "updated"
	case errors.Is(err, os.ErrNotExist):
		t.Status = "installed"
	default:
		return err
	}
	if dryRun {
		t.Status = map[string]string{"installed": "would install", "updated": "would update"}[t.Status]
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(t.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(t.Path, skill.Content, 0o644)
}
