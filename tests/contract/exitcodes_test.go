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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// exitCode extracts the process exit code from a commandResult: 0 on success,
// the child's code on *exec.ExitError, -1 for anything else.
func exitCode(t *testing.T, result commandResult) int {
	t.Helper()
	if result.err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(result.err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("command did not run: %v", result.err)
	return -1
}

// TestExitCodeContract pins the Build Spec process contract — 0 ok · 1
// runtime · 2 usage · 3 auth — across the failure classes an agent must tell
// apart without parsing text.
func TestExitCodeContract(t *testing.T) {
	// A refused connection is a runtime failure, not the caller's mistake.
	refusedURL := "http://127.0.0.1:1"

	// The live API reports an invalid key as HTTP 400 with reason
	// API_KEY_INVALID in an array-wrapped envelope; reason-first
	// classification must map it to the auth exit code.
	invalidKey := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `[{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com"}]}}]`)
	}))
	defer invalidKey.Close()

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":401,"message":"unauthenticated"}}`)
	}))
	defer unauthorized.Close()

	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":404,"message":"no such agent"}}`)
	}))
	defer notFound.Close()

	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "success: --help", args: []string{"--help"}, want: 0},
		{name: "success: keyless dry-run", args: []string{"--api-version", "v1beta", "--no-interactive", "generate", "hi", "--dry-run"}, want: 0},
		{name: "usage: unknown root command", args: []string{"no-such-command"}, want: 2},
		{name: "usage: unknown nested subcommand", args: []string{"agent", "nosuch"}, want: 2},
		{name: "usage: unknown flag", args: []string{"agent", "list", "--no-such-flag"}, want: 2},
		{name: "usage: malformed --body", args: append(baseArgs(refusedURL), "agent", "run", "--body", "{"), want: 2},
		{name: "runtime: connection refused", args: append(baseArgs(refusedURL), "agent", "list"), want: 1},
		{name: "runtime: HTTP 404", args: append(baseArgs(notFound.URL), "agent", "get", "--id", "agent-1"), want: 1},
		{name: "auth: HTTP 401", args: append(baseArgs(unauthorized.URL), "agent", "list"), want: 3},
		{name: "auth: HTTP 400 API_KEY_INVALID (reason-first)", args: append(baseArgs(invalidKey.URL), "agent", "list"), want: 3},
	}

	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "plain"},
		{name: "agent", args: []string{"--agent-mode"}},
	} {
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				args := append(append([]string{}, mode.args...), tc.args...)
				result := runCLI(t, t.TempDir(), nil, args...)
				if got := exitCode(t, result); got != tc.want {
					t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", got, tc.want, result.stdout, result.stderr)
				}
				if tc.want != 0 && strings.TrimSpace(result.stdout) != "" {
					t.Errorf("failure wrote stdout: %s", result.stdout)
				}
				if mode.name == "agent" && tc.want != 0 {
					var envelope struct {
						ExitCode int `json:"exit_code"`
					}
					if err := json.Unmarshal([]byte(strings.TrimSpace(result.stderr)), &envelope); err != nil {
						t.Fatalf("stderr is not one JSON document: %v\n%s", err, result.stderr)
					}
					if envelope.ExitCode != tc.want {
						t.Errorf("envelope exit_code = %d, want %d", envelope.ExitCode, tc.want)
					}
				}
			})
		}
	}
}

// TestExitCodeBareIntent pins the incomplete-invocation contract: a bare
// required-input intent is a usage error (exit 2). Plain mode renders the
// full help to stderr with the actionable error last; structured modes emit
// exactly one envelope with a --help hint. stdout stays empty either way.
func TestExitCodeBareIntent(t *testing.T) {
	commands := [][]string{
		{"generate"},        // generated intent
		{"analyze"},         // porcelain: --input required
		{"transcribe"},      // porcelain: --input required
		{"tts"},             // porcelain: input text required
		{"files", "upload"}, // porcelain: <file> argument required
	}
	for _, command := range commands {
		name := strings.Join(command, " ")
		t.Run("plain/"+name, func(t *testing.T) {
			result := runCLI(t, t.TempDir(), nil, command...)
			if got := exitCode(t, result); got != 2 {
				t.Fatalf("exit code = %d, want 2\nstdout: %s\nstderr: %s", got, result.stdout, result.stderr)
			}
			if strings.TrimSpace(result.stdout) != "" {
				t.Errorf("bare %q wrote stdout: %s", name, result.stdout)
			}
			if !strings.Contains(result.stderr, "Usage:") {
				t.Errorf("stderr lacks the help page:\n%s", result.stderr)
			}
			trimmed := strings.TrimSpace(result.stderr)
			lastLine := trimmed[strings.LastIndex(trimmed, "\n")+1:]
			if !strings.HasPrefix(lastLine, "Error: ") {
				t.Errorf("the actionable error is not the last stderr line: %q", lastLine)
			}
		})
		t.Run("agent/"+name, func(t *testing.T) {
			args := append([]string{"--agent-mode"}, command...)
			result := runCLI(t, t.TempDir(), nil, args...)
			if got := exitCode(t, result); got != 2 {
				t.Fatalf("exit code = %d, want 2\nstdout: %s\nstderr: %s", got, result.stdout, result.stderr)
			}
			if strings.TrimSpace(result.stdout) != "" {
				t.Errorf("bare %q wrote stdout: %s", name, result.stdout)
			}
			var envelope struct {
				ExitCode int      `json:"exit_code"`
				Hints    []string `json:"hints"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(result.stderr)), &envelope); err != nil {
				t.Fatalf("stderr is not one JSON document: %v\n%s", err, result.stderr)
			}
			if envelope.ExitCode != 2 {
				t.Errorf("envelope exit_code = %d, want 2", envelope.ExitCode)
			}
			if !strings.Contains(strings.Join(envelope.Hints, " "), "--help") {
				t.Errorf("hints do not point at --help: %v", envelope.Hints)
			}
		})
	}
}

// TestExitCodeHelpFooter pins the advertised contract line on the help
// surfaces an agent reads first.
func TestExitCodeHelpFooter(t *testing.T) {
	const footer = "Exit codes: 0 ok · 1 runtime · 2 usage · 3 auth"
	for _, args := range [][]string{
		{"--help"},
		{"agent", "--help"},
		{"agent", "run", "--help"},
		{"tts", "--help"},
	} {
		result := runCLI(t, t.TempDir(), nil, args...)
		if result.err != nil {
			t.Fatalf("%v failed: %v\n%s", args, result.err, result.stderr)
		}
		if !strings.Contains(result.stdout, footer) {
			t.Errorf("%v help lacks the exit-code footer:\n%s", args, result.stdout)
		}
	}
}
