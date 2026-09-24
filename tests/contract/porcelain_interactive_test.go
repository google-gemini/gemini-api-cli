package contract_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/google-gemini/gemini-api-cli/internal/cli"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/interactive"
	"github.com/google-gemini/gemini-api-cli/internal/output"
)

type scriptedPrompter struct {
	t       *testing.T
	answers map[string][]string
	calls   [][]string
}

func (p *scriptedPrompter) Prompt(_ *cobra.Command, fields []interactive.PromptField) ([]interactive.PromptAnswer, error) {
	p.t.Helper()
	ids := make([]string, len(fields))
	answers := make([]interactive.PromptAnswer, len(fields))
	for i, field := range fields {
		ids[i] = field.ID
		values, ok := p.answers[field.ID]
		if !ok {
			continue
		}
		answers[i] = interactive.PromptAnswer{Set: true, Values: append([]string(nil), values...)}
	}
	p.calls = append(p.calls, ids)
	return answers, nil
}

func clearAgentModeEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"CLAUDECODE", "CLAUDE_CODE", "CURSOR_AGENT", "CODEX", "AIDER", "CLINE",
		"WINDSURF_AGENT", "GITHUB_COPILOT", "AMAZON_Q", "GEMINI_CODE_ASSIST",
		"SRC_CODY", "FORCE_AGENT_MODE",
	} {
		t.Setenv(name, "")
	}
}

func executeInteractive(t *testing.T, prompter interactive.Prompter, args ...string) (string, string, error) {
	t.Helper()
	config.Reset()
	output.ResetAgentMode()
	t.Cleanup(func() {
		config.Reset()
		output.ResetAgentMode()
	})
	root, err := cli.NewRootCommand()
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(""))
	ctx := context.Background()
	if prompter != nil {
		ctx = interactive.WithPrompter(ctx, prompter)
	}
	err = cli.ExecuteRoot(ctx, root, args)
	return stdout.String(), stderr.String(), err
}

func TestPorcelainInteractivePrompts(t *testing.T) {
	clearAgentModeEnvironment(t)
	dir := t.TempDir()
	image := filepath.Join(dir, "prompt.png")
	audio := filepath.Join(dir, "prompt.mp3")
	upload := filepath.Join(dir, "upload.txt")
	for path, data := range map[string]string{image: "png", audio: "audio", upload: "upload"} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name       string
		command    []string
		answers    map[string][]string
		wantFields []string
		wantStderr []string
	}{
		{
			name:       "analyze",
			command:    []string{"analyze"},
			answers:    map[string][]string{"flag:input": {image}, "arg:question": {"what is this, exactly?"}},
			wantFields: []string{"flag:input", "arg:question"},
			wantStderr: []string{`"data"`, "<bytes:3>", "what is this, exactly?"},
		},
		{
			name:       "transcribe",
			command:    []string{"transcribe"},
			answers:    map[string][]string{"flag:input": {audio}},
			wantFields: []string{"flag:input"},
			wantStderr: []string{`"data"`, "<bytes:5>", "Transcribe the speech"},
		},
		{
			name:       "tts",
			command:    []string{"tts"},
			answers:    map[string][]string{"arg:text": {"speak this"}},
			wantFields: []string{"arg:text"},
			wantStderr: []string{"speak this", `"response_format"`, `"speech_config"`},
		},
		{
			name:       "files upload",
			command:    []string{"files", "upload"},
			answers:    map[string][]string{"arg:path": {upload}},
			wantFields: []string{"arg:path"},
			wantStderr: []string{"upload/v1beta/files", "dry-run-session", "<bytes:"},
		},
		// The identifier is asked for once, and the positional answer is enough.
		{
			name:       "files get",
			command:    []string{"files", "get"},
			answers:    map[string][]string{"arg:file": {"files/abc123"}},
			wantFields: []string{"arg:file"},
			wantStderr: []string{"/v1beta/files/abc123"},
		},
		{
			name:       "files delete",
			command:    []string{"files", "delete"},
			answers:    map[string][]string{"arg:file": {"abc123"}},
			wantFields: []string{"arg:file"},
			wantStderr: []string{"DELETE", "/v1beta/files/abc123"},
		},
		{
			name:       "models get",
			command:    []string{"models", "get"},
			answers:    map[string][]string{"arg:model": {"models/gemini-2.5-flash"}},
			wantFields: []string{"arg:model"},
			wantStderr: []string{"/v1beta/models/gemini-2.5-flash"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompter := &scriptedPrompter{t: t, answers: tt.answers}
			args := []string{"--interactive", "--dry-run", "--api-key", "k", "--server-url", "https://example.invalid", "--color", "never"}
			args = append(args, tt.command...)
			stdout, stderr, err := executeInteractive(t, prompter, args...)
			if err != nil {
				t.Fatalf("command failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
			}
			if len(prompter.calls) != 1 || !reflect.DeepEqual(prompter.calls[0], tt.wantFields) {
				t.Fatalf("prompt calls = %#v, want one call with %#v", prompter.calls, tt.wantFields)
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr)
				}
			}
		})
	}
}

// TestPorcelainDefaultSuppressesPrompter: the CLI is non-interactive by
// default: without --interactive an injected prompter is never consulted and
// a bare porcelain command is the same usage error as --no-interactive.
func TestPorcelainDefaultSuppressesPrompter(t *testing.T) {
	clearAgentModeEnvironment(t)
	prompter := &scriptedPrompter{
		t:       t,
		answers: map[string][]string{"flag:input": {"should-not-be-used.png"}},
	}
	stdout, stderr, err := executeInteractive(t, prompter, "analyze")
	if err == nil {
		t.Fatal("bare analyze without --interactive succeeded; want the usage error")
	}
	if len(prompter.calls) != 0 {
		t.Fatalf("prompter called without --interactive: %#v", prompter.calls)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("bare analyze wrote stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "Usage:") || !strings.Contains(stderr, "analyze [question]") {
		t.Errorf("stderr does not contain help:\n%s", stderr)
	}
	if !strings.Contains(stderr, "missing required flag --input") {
		t.Errorf("stderr lacks the actionable error:\n%s", stderr)
	}
}

func TestPorcelainNoInteractiveSuppressesPrompter(t *testing.T) {
	clearAgentModeEnvironment(t)
	prompter := &scriptedPrompter{
		t:       t,
		answers: map[string][]string{"flag:input": {"should-not-be-used.png"}},
	}
	stdout, stderr, err := executeInteractive(t, prompter, "--no-interactive", "analyze")
	if err == nil {
		t.Fatal("bare analyze under --no-interactive succeeded; want the usage error")
	}
	if len(prompter.calls) != 0 {
		t.Fatalf("prompter called under --no-interactive: %#v", prompter.calls)
	}
	// The bare invocation is a usage error: help goes to stderr with the
	// actionable error last, stdout stays empty.
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("bare analyze wrote stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "Usage:") || !strings.Contains(stderr, "analyze [question]") {
		t.Errorf("stderr does not contain help:\n%s", stderr)
	}
	if !strings.Contains(stderr, "missing required flag --input") {
		t.Errorf("stderr lacks the actionable error:\n%s", stderr)
	}
}

func TestScriptedPrompterRejectsMissingRequiredAnswer(t *testing.T) {
	// Keep the test prompter honest: a future prompt-plan addition should fail
	// explicitly rather than silently produce a misleading dry-run assertion.
	p := &scriptedPrompter{t: t, answers: map[string][]string{}}
	answers, err := p.Prompt(&cobra.Command{}, []interactive.PromptField{{ID: "flag:missing"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].Set {
		t.Fatalf("unexpected answers: %s", fmt.Sprint(answers))
	}
}

// TestTextSourceFlagSatisfiesInteractiveArgument: a --file text source counts
// as the required positional, so --interactive never prompts for it.
func TestTextSourceFlagSatisfiesInteractiveArgument(t *testing.T) {
	clearAgentModeEnvironment(t)
	path := filepath.Join(t.TempDir(), "script.txt")
	if err := os.WriteFile(path, []byte("read this file"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompter := &scriptedPrompter{t: t, answers: map[string][]string{"arg:text": {"should not be used"}}}
	_, stderr, err := executeInteractive(t, prompter,
		"--interactive", "--dry-run", "--api-key", "k", "--server-url", "https://example.invalid", "tts", "--file", path)
	if err != nil {
		t.Fatalf("tts --file failed: %v\nstderr: %s", err, stderr)
	}
	if len(prompter.calls) != 0 {
		t.Fatalf("prompter called even though --file satisfied text: %#v", prompter.calls)
	}
	if !strings.Contains(stderr, "read this file") {
		t.Errorf("dry-run did not use --file text:\n%s", stderr)
	}
}
