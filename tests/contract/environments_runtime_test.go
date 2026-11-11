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
	"reflect"
	"strings"
	"sync"
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
				"Learn: https://aistudio.google.com/docs/agent-environment · escalate: multi-tenant orchestration via gemini-api environments poll",
				"--disable-bash",
				"By default, stdout is unused",
			},
		},
		{
			name: "poll",
			args: []string{"environments", "poll"},
			wantText: []string{
				"Defaults: max-interactions 10 · poll-interval 2s · provision-timeout 2m · handoff-heartbeat-interval 5s",
				"Learn: https://aistudio.google.com/docs/agent-environment · escalate: single-node execution via gemini-api environments connect",
				"By default, stdout is unused",
			},
		},
		{
			name: "serve",
			args: []string{"environments", "serve"},
			wantText: []string{
				"Defaults: root-dir . · workdir . · heartbeat-interval 5s · command-timeout 2m",
				"Learn: https://aistudio.google.com/docs/agent-environment · escalate: single-node polling + worker via gemini-api environments connect",
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
				"https://aistudio.google.com/docs/environments",
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
		name       string
		args       []string
		wantMethod string
		wantPath   string
	}{
		{
			name:       "connect",
			args:       []string{"environments", "connect", "--root-dir", absentRoot, "--workdir", absentWork},
			wantMethod: "POST",
			wantPath:   "/v1beta/environments:leaseInteractions",
		},
		{
			name:       "poll",
			args:       []string{"environments", "poll"},
			wantMethod: "POST",
			wantPath:   "/v1beta/environments:leaseInteractions",
		},
		{
			name:       "serve",
			args:       []string{"environments", "serve", "--root-dir", absentRoot, "--workdir", absentWork},
			wantMethod: "GET",
			wantPath:   "/v1beta/interactions:streamForEnvironmentRuntime",
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
			wantDryPrefix := "[DRY-RUN] Would send: " + tc.wantMethod + " "
			if !strings.Contains(humanRes.stderr, wantDryPrefix) || !strings.Contains(humanRes.stderr, tc.wantPath) {
				t.Errorf("human dry-run stderr = %q, want %s%s preview", humanRes.stderr, wantDryPrefix, tc.wantPath)
			}

			jsonArgs := append(append([]string{}, humanArgs...), "--output-format", "json")
			jsonRes := runCLI(t, t.TempDir(), nil, jsonArgs...)
			if jsonRes.err != nil {
				t.Fatalf("runCLI(%v --dry-run -o json) failed: %v\nstderr: %s", tc.args, jsonRes.err, jsonRes.stderr)
			}
			reqPreview := parseDryRunRequest(t, jsonRes.stdout)
			if gotMethod := fmt.Sprint(reqPreview["method"]); gotMethod != tc.wantMethod {
				t.Errorf("json dry-run method = %q, want %q", gotMethod, tc.wantMethod)
			}
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

	noKeyringEnv := map[string]string{"GEMINI_NO_KEYRING": "true"}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plainRes := runCLI(t, t.TempDir(), noKeyringEnv, tc.args...)
			if got := exitCode(t, plainRes); got != tc.wantCode {
				t.Errorf("runCLI(%v) exit code = %d, want %d\nstdout: %s\nstderr: %s", tc.args, got, tc.wantCode, plainRes.stdout, plainRes.stderr)
			}

			agentArgs := append([]string{"--agent-mode"}, tc.args...)
			agentRes := runCLI(t, t.TempDir(), noKeyringEnv, agentArgs...)
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

func TestEnvironmentsCreateAndConnect(t *testing.T) {
	var (
		mu             sync.Mutex
		createBody     map[string]any
		createCalls    int
		leaseEnvKeyHdr []string
		leaseCalls     atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1beta/environments":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			createCalls++
			createBody = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"id":"env_1","mode":"self_hosted","environment_key":"ek_e2e_123"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1beta/environments:leaseInteractions":
			hdr := r.Header.Get("x-goog-environment-key")
			mu.Lock()
			leaseEnvKeyHdr = append(leaseEnvKeyHdr, hdr)
			mu.Unlock()
			if leaseCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `{"leases":[]}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":404,"message":"environment gone"}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	env := map[string]string{
		"GEMINI_NO_KEYRING": "true",
		"GEMINI_API_KEY":    "api_key_test",
	}

	// 1. Create a self-hosted environment and extract environment_key via --jq.
	createArgs := append(baseArgs(server.URL),
		"environments", "create",
		"--mode", "self_hosted",
		"--system-instruction", "Be concise.",
		"--custom-config", `{"team":"a"}`,
		"--jq", ".environment_key",
	)
	createRes := runCLI(t, t.TempDir(), env, createArgs...)
	if createRes.err != nil {
		t.Fatalf("environments create failed: %v\nstdout: %s\nstderr: %s", createRes.err, createRes.stdout, createRes.stderr)
	}
	mu.Lock()
	gotCreateCalls := createCalls
	gotCreateBody := createBody
	mu.Unlock()
	if gotCreateCalls != 1 {
		t.Fatalf("POST /v1beta/environments calls = %d, want 1", gotCreateCalls)
	}
	envKey := strings.TrimSpace(createRes.stdout)
	if envKey != "ek_e2e_123" {
		t.Fatalf("create --jq .environment_key = %q, want %q", envKey, "ek_e2e_123")
	}
	if got := gotCreateBody["mode"]; got != "self_hosted" {
		t.Errorf("create request body mode = %#v, want %q", got, "self_hosted")
	}
	if got := gotCreateBody["system_instruction"]; got != "Be concise." {
		t.Errorf("create request body system_instruction = %#v, want %q", got, "Be concise.")
	}
	wantCustomConfig := map[string]any{"team": "a"}
	if got := gotCreateBody["custom_config"]; !reflect.DeepEqual(got, wantCustomConfig) {
		t.Errorf("create request body custom_config = %#v, want %#v", got, wantCustomConfig)
	}

	sandboxDir := t.TempDir()

	// 2a. Verify dry-run connect carries x-goog-environment-key and redacts secrets.
	dryRunArgs := append(baseArgs(server.URL),
		"--dry-run",
		"environments", "connect",
		"--environment-key", envKey,
		"--root-dir", sandboxDir,
		"--workdir", sandboxDir,
	)
	dryRunRes := runCLI(t, t.TempDir(), env, dryRunArgs...)
	if dryRunRes.err != nil {
		t.Fatalf("environments connect --dry-run failed: %v\nstderr: %s", dryRunRes.err, dryRunRes.stderr)
	}
	if got := leaseCalls.Load(); got != 0 {
		t.Fatalf("dry-run leaseInteractions call count = %d, want 0", got)
	}
	reqPreview := parseDryRunRequest(t, dryRunRes.stdout)
	hdrs, _ := reqPreview["headers"].(map[string]any)
	var gotPreviewEnvKey string
	for k, v := range hdrs {
		if strings.EqualFold(k, "x-goog-environment-key") {
			gotPreviewEnvKey = fmt.Sprint(v)
		}
	}
	if gotPreviewEnvKey == "" || !strings.Contains(gotPreviewEnvKey, "REDACTED") {
		t.Errorf("dry-run x-goog-environment-key = %q (headers=%v), want redacted value", gotPreviewEnvKey, hdrs)
	}
	if strings.Contains(dryRunRes.stdout, envKey) || strings.Contains(dryRunRes.stderr, envKey) ||
		strings.Contains(dryRunRes.stdout, "api_key_test") || strings.Contains(dryRunRes.stderr, "api_key_test") {
		t.Errorf("dry-run leaked raw secret\nstdout: %s\nstderr: %s", dryRunRes.stdout, dryRunRes.stderr)
	}

	// 2b. Connect against the same server and verify x-goog-environment-key on every leaseInteractions call.
	connectArgs := append(baseArgs(server.URL),
		"environments", "connect",
		"--environment-key", envKey,
		"--root-dir", sandboxDir,
		"--workdir", sandboxDir,
		"--poll-interval", "10ms",
	)
	connectRes := runCLI(t, t.TempDir(), env, connectArgs...)
	if got := exitCode(t, connectRes); got != 1 {
		t.Errorf("environments connect exit code = %d, want 1 after 404 stop\nstdout: %s\nstderr: %s", got, connectRes.stdout, connectRes.stderr)
	}
	if got := leaseCalls.Load(); got != 2 {
		t.Errorf("leaseInteractions call count = %d, want 2", got)
	}
	mu.Lock()
	gotLeaseHdrs := append([]string(nil), leaseEnvKeyHdr...)
	mu.Unlock()
	for i, hdr := range gotLeaseHdrs {
		if hdr != envKey {
			t.Errorf("leaseInteractions call %d x-goog-environment-key = %q, want %q", i, hdr, envKey)
		}
	}
}
