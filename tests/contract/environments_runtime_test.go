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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEnvironmentsRuntimeHelpAndUsage(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantText []string
	}{
		{
			name: "connect",
			args: []string{"environments", "connect"},
			wantText: []string{
				"Defaults: root-dir . · workdir . · max-concurrent 4 · poll-interval 2s",
				"Learn: https://ai.google.dev/gemini-api/docs/agent-environment · escalate: multi-tenant orchestration via gemini-api environments poll",
				"--disable-bash",
				"By default, stdout is unused",
			},
		},
		{
			name: "poll",
			args: []string{"environments", "poll"},
			wantText: []string{
				"Defaults: max-interactions 10 · poll-interval 2s · provision-timeout 2m · handoff-heartbeat-interval 5s",
				"Learn: https://ai.google.dev/gemini-api/docs/agent-environment · escalate: single-node execution via gemini-api environments connect",
				"By default, stdout is unused",
			},
		},
		{
			name: "serve",
			args: []string{"environments", "serve"},
			wantText: []string{
				"Defaults: root-dir . · workdir . · heartbeat-interval 5s · command-timeout 2m",
				"Learn: https://ai.google.dev/gemini-api/docs/agent-environment · escalate: single-node polling + worker via gemini-api environments connect",
				"--disable-bash",
				"By default, stdout is unused",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			helpRes := runCLI(t, t.TempDir(), nil, append(tc.args, "--help")...)
			if helpRes.err != nil {
				t.Fatalf("runCLI(%v --help) failed: %v\nstderr: %s", tc.args, helpRes.err, helpRes.stderr)
			}
			if n := helpLines(helpRes.stdout); n > 40 {
				t.Errorf("%v --help lines = %d, want <= 40:\n%s", tc.args, n, helpRes.stdout)
			}
			for _, want := range append(tc.wantText, helpMachineLine, helpGlobalsLine) {
				if !strings.Contains(helpRes.stdout, want) {
					t.Errorf("%v --help missing %q:\n%s", tc.args, want, helpRes.stdout)
				}
			}
			for _, reject := range []string{
				"Global Flags:",
				"Authentication:",
				"--api-key string",
				"Tier-1",
				"Tier-2",
				"https://ai.google.dev/gemini-api/docs/environments",
			} {
				if strings.Contains(helpRes.stdout, reject) {
					t.Errorf("%v --help leaked forbidden text %q:\n%s", tc.args, reject, helpRes.stdout)
				}
			}

			usageRes := runCLI(t, t.TempDir(), nil, append(tc.args, "--usage")...)
			if usageRes.err != nil {
				t.Fatalf("runCLI(%v --usage) failed: %v\nstderr: %s", tc.args, usageRes.err, usageRes.stderr)
			}
			if !strings.Contains(usageRes.stdout, fmt.Sprintf("cmd %q", tc.name)) {
				t.Errorf("%v --usage output missing cmd %q:\n%s", tc.args, tc.name, usageRes.stdout)
			}
		})
	}
}

func TestEnvironmentsRuntimeDryRun(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	baseDir := t.TempDir()
	absentRoot := filepath.Join(baseDir, "uncreated-root")
	absentWork := filepath.Join(absentRoot, "uncreated-work")

	tests := []struct {
		name     string
		args     []string
		wantPath string
	}{
		{
			name:     "connect",
			args:     []string{"environments", "connect", "--root-dir", absentRoot, "--workdir", absentWork},
			wantPath: "/v1beta/environments:leaseInteractions",
		},
		{
			name:     "poll",
			args:     []string{"environments", "poll"},
			wantPath: "/v1beta/environments:leaseInteractions",
		},
		{
			name:     "serve",
			args:     []string{"environments", "serve", "--root-dir", absentRoot, "--workdir", absentWork},
			wantPath: "/google.learning.gemini.api.interactions.v1beta.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			humanArgs := append([]string{
				"--server-url", server.URL,
				"--api-version", "v1beta",
				"--no-interactive",
				"--no-retries",
				"--color", "never",
				"--dry-run",
			}, tc.args...)
			humanRes := runCLI(t, t.TempDir(), nil, humanArgs...)
			if humanRes.err != nil {
				t.Fatalf("runCLI(%v --dry-run) failed: %v\nstderr: %s", tc.args, humanRes.err, humanRes.stderr)
			}
			if humanRes.stdout != "" {
				t.Errorf("human dry-run stdout = %q, want empty", humanRes.stdout)
			}
			if !strings.Contains(humanRes.stderr, "[DRY-RUN] Would send: POST ") || !strings.Contains(humanRes.stderr, tc.wantPath) {
				t.Errorf("human dry-run stderr = %q, want POST %s preview", humanRes.stderr, tc.wantPath)
			}

			jsonArgs := append(append([]string{}, humanArgs...), "--output-format", "json")
			jsonRes := runCLI(t, t.TempDir(), nil, jsonArgs...)
			if jsonRes.err != nil {
				t.Fatalf("runCLI(%v --dry-run -o json) failed: %v\nstderr: %s", tc.args, jsonRes.err, jsonRes.stderr)
			}
			reqPreview := parseDryRunRequest(t, jsonRes.stdout)
			gotURL := fmt.Sprint(reqPreview["url"])
			if !strings.HasSuffix(gotURL, tc.wantPath) {
				t.Errorf("json dry-run url = %q, want suffix %q", gotURL, tc.wantPath)
			}
			if _, err := os.Stat(absentRoot); !os.IsNotExist(err) {
				t.Errorf("dry-run created %q on disk (stat err=%v), want absent", absentRoot, err)
			}
			if got := hits.Load(); got != 0 {
				t.Errorf("dry-run server hits = %d, want 0", got)
			}
		})
	}
}

func TestEnvironmentsRuntimeExitCodesAndErrorTaxonomy(t *testing.T) {
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":401,"message":"unauthenticated"}}`)
	}))
	defer unauthorized.Close()

	sandboxRoot := t.TempDir()
	outsideWork := t.TempDir()

	tests := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{
			name:     "connect_missing_env_key",
			args:     []string{"--no-keyring", "environments", "connect"},
			wantCode: 2,
		},
		{
			name:     "connect_workdir_escapes_root",
			args:     []string{"environments", "connect", "--environment-key", "ek_test", "--root-dir", sandboxRoot, "--workdir", outsideWork},
			wantCode: 2,
		},
		{
			name:     "poll_missing_provision_cmd",
			args:     []string{"--no-keyring", "environments", "poll", "--environment-key", "ek_test"},
			wantCode: 2,
		},
		{
			name:     "serve_orphan_interaction_id",
			args:     []string{"environments", "serve", "--interaction-id", "int_123"},
			wantCode: 2,
		},
		{
			name:     "connect_unauthorized_401",
			args:     append(baseArgs(unauthorized.URL), "environments", "connect", "--environment-key", "ek_bad"),
			wantCode: 3,
		},
		{
			name:     "poll_unauthorized_401",
			args:     append(baseArgs(unauthorized.URL), "environments", "poll", "--environment-key", "ek_bad", "--provision-cmd", "true"),
			wantCode: 3,
		},
		{
			name:     "serve_plain_http_unauthorized_401",
			args:     append(baseArgs(unauthorized.URL), "environments", "serve", "--interaction-id", "int_123", "--lease-token", "tok_bad"),
			wantCode: 3,
		},
		{
			name:     "connect_connection_refused",
			args:     append(baseArgs("http://127.0.0.1:1"), "environments", "connect", "--environment-key", "ek_test"),
			wantCode: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plainRes := runCLI(t, t.TempDir(), nil, tc.args...)
			if got := exitCode(t, plainRes); got != tc.wantCode {
				t.Errorf("runCLI(%v) exit code = %d, want %d\nstdout: %s\nstderr: %s", tc.args, got, tc.wantCode, plainRes.stdout, plainRes.stderr)
			}

			agentArgs := append([]string{"--agent-mode"}, tc.args...)
			agentRes := runCLI(t, t.TempDir(), nil, agentArgs...)
			if got := exitCode(t, agentRes); got != tc.wantCode {
				t.Errorf("runCLI(--agent-mode %v) exit code = %d, want %d\nstderr: %s", tc.args, got, tc.wantCode, agentRes.stderr)
			}
			var envelope struct {
				ExitCode int `json:"exit_code"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(agentRes.stderr)), &envelope); err != nil {
				t.Fatalf("--agent-mode stderr is not valid JSON: %v\nstderr: %s", err, agentRes.stderr)
			}
			if envelope.ExitCode != tc.wantCode {
				t.Errorf("--agent-mode envelope exit_code = %d, want %d", envelope.ExitCode, tc.wantCode)
			}
		})
	}
}

func TestEnvironmentsRuntimeDualEnvCredentials(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantAuthHeader string
		wantAuthValue  string
	}{
		{
			name:           "connect_env_api_key_and_env_key",
			args:           []string{"environments", "connect"},
			wantAuthHeader: "x-goog-environment-key",
			wantAuthValue:  "ek_env_val",
		},
		{
			name:           "poll_env_api_key_and_env_key",
			args:           []string{"environments", "poll", "--provision-cmd", "true"},
			wantAuthHeader: "x-goog-environment-key",
			wantAuthValue:  "ek_env_val",
		},
		{
			name:           "serve_env_api_key_and_lease_token",
			args:           []string{"environments", "serve", "--interaction-id", "int_123", "--lease-token", "lt_val"},
			wantAuthHeader: "x-goog-lease-token",
			wantAuthValue:  "lt_val",
		},
	}

	env := map[string]string{
		"GEMINI_NO_KEYRING":      "true",
		"GEMINI_API_KEY":         "api_key_from_env",
		"GEMINI_ENVIRONMENT_KEY": "ek_env_val",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cliArgs := append([]string{
				"--server-url", "https://autopush-generativelanguage.sandbox.googleapis.com",
				"--api-version", "v1beta",
				"--dry-run",
				"--output-format", "json",
			}, tc.args...)
			res := runCLI(t, t.TempDir(), env, cliArgs...)
			if res.err != nil {
				t.Fatalf("runCLI(%v) unexpected error: %v\nstderr: %s", cliArgs, res.err, res.stderr)
			}
			reqPreview := parseDryRunRequest(t, res.stdout)
			hdrs, _ := reqPreview["headers"].(map[string]any)
			var gotAPIKey, gotRuntimeAuth string
			for k, v := range hdrs {
				switch strings.ToLower(k) {
				case "x-goog-api-key":
					gotAPIKey = fmt.Sprint(v)
				case tc.wantAuthHeader:
					gotRuntimeAuth = fmt.Sprint(v)
				}
			}
			if gotAPIKey == "" {
				t.Errorf("runCLI(%v) headers = %v, want non-empty x-goog-api-key when GEMINI_API_KEY is set alongside runtime credentials", cliArgs, hdrs)
			}
			if gotRuntimeAuth == "" {
				t.Errorf("runCLI(%v) headers = %v, want non-empty %s header", cliArgs, hdrs, tc.wantAuthHeader)
			}
			if strings.Contains(res.stdout, "api_key_from_env") || strings.Contains(res.stdout, tc.wantAuthValue) {
				t.Errorf("runCLI(%v) dry-run leaked raw secret in stdout: %s", cliArgs, res.stdout)
			}
		})
	}
}
