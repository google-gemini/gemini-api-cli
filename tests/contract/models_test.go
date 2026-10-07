package contract_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestModelsListAll pins the ModelsList pagination contract: GET /v1beta/models
// cursored on pageToken/nextPageToken, depaginated by --all (mirrors files list).
func TestModelsListAll(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		if r.URL.Path != "/v1beta/models" {
			t.Errorf("path = %q, want /v1beta/models", r.URL.Path)
		}
		wantToken := ""
		if requestNumber == 2 {
			wantToken = "next"
		}
		if got := r.URL.Query().Get("pageToken"); got != wantToken {
			t.Errorf("request %d pageToken = %q, want %q", requestNumber, got, wantToken)
		}
		page := map[string]any{"models": []map[string]any{{"name": "models/gemini-2.5-flash"}}}
		if requestNumber == 1 {
			page["nextPageToken"] = "next"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "models", "list", "--all")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("models list --all failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if requests.Load() != 2 {
		t.Errorf("requests = %d, want 2", requests.Load())
	}
	if pages := decodeNDJSON(t, result.stdout); len(pages) != 2 {
		t.Errorf("NDJSON pages = %d, want 2", len(pages))
	}
	if result.stderr != "" {
		t.Errorf("stderr = %q, want empty", result.stderr)
	}
}

// TestModelsGet pins the ModelsGet contract: the positional id and the --model
// flag both resolve to GET /v1beta/models/{model} with the bare id, and a
// "models/" prefix on the positional is normalized away.
func TestModelsGet(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "positional bare", args: []string{"models", "get", "gemini-2.5-flash"}},
		{name: "positional prefixed", args: []string{"models", "get", "models/gemini-2.5-flash"}},
		{name: "flag", args: []string{"models", "get", "--model", "gemini-2.5-flash"}},
		{name: "flag prefixed", args: []string{"models", "get", "--model", "models/gemini-2.5-flash"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var got atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.Add(1)
				if r.URL.Path != "/v1beta/models/gemini-2.5-flash" {
					t.Errorf("path = %q, want /v1beta/models/gemini-2.5-flash", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "models/gemini-2.5-flash", "version": "2.5"})
			}))
			defer server.Close()

			args := append(baseArgs(server.URL), tt.args...)
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("models get failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if got.Load() != 1 {
				t.Errorf("requests = %d, want 1", got.Load())
			}
			if !strings.Contains(result.stdout, "models/gemini-2.5-flash") {
				t.Errorf("stdout missing model name:\n%s", result.stdout)
			}
		})
	}
}

// TestModelsGetRejectsDoubleIdentifier pins the "pass model once"
// guard: supplying both the positional and the --model flag is a usage error.
func TestModelsGetRejectsDoubleIdentifier(t *testing.T) {
	args := append(baseArgs("http://127.0.0.1:0"), "models", "get", "gemini-2.5-flash", "--model", "gemini-2.5-flash")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatalf("expected usage error, got success\nstdout: %s", result.stdout)
	}
	if !strings.Contains(result.stderr, "pass model once") {
		t.Errorf("stderr = %q, want the double-identifier usage error", result.stderr)
	}
}

// TestIdentifierRejectedBeforeRequest pins that a missing, empty, prefix-only,
// or path-traversal identifier on the positional-capable commands is a usage
// error (exit 2) raised before any request is sent, rather than a request
// against a corrupt or collection-shaped path.
func TestIdentifierRejectedBeforeRequest(t *testing.T) {
	cases := []struct {
		command    []string
		wantStderr string
	}{
		{command: []string{"models", "get"}, wantStderr: "invalid model id"},
		{command: []string{"files", "get"}, wantStderr: "invalid file id"},
		{command: []string{"files", "delete"}, wantStderr: "invalid file id"},
	}
	for _, tc := range cases {
		prefix := tc.command[0] + "/"
		forms := []struct {
			name       string
			args       []string
			wantStderr string
		}{
			{name: "no identifier", wantStderr: "missing"},
			{name: "empty positional", args: []string{""}, wantStderr: tc.wantStderr},
			{name: "prefix only", args: []string{prefix}, wantStderr: tc.wantStderr},
			{name: "path traversal", args: []string{"a/../b"}, wantStderr: tc.wantStderr},
			{name: "path traversal via flag", args: []string{"--" + strings.TrimSuffix(tc.command[0], "s"), "a/../b"}, wantStderr: tc.wantStderr},
		}
		for _, form := range forms {
			t.Run(strings.Join(tc.command, " ")+"/"+form.name, func(t *testing.T) {
				var got atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got.Add(1)
					w.WriteHeader(http.StatusOK)
				}))
				defer server.Close()

				args := append(append(baseArgs(server.URL), tc.command...), form.args...)
				result := runCLI(t, t.TempDir(), nil, args...)
				if code := exitCode(t, result); code != 2 {
					t.Errorf("exit code = %d, want 2\nstdout: %s", code, result.stdout)
				}
				if !strings.Contains(result.stderr, form.wantStderr) {
					t.Errorf("stderr = %q, want it to contain %q", result.stderr, form.wantStderr)
				}
				if got.Load() != 0 {
					t.Errorf("reached the server %d times, want 0", got.Load())
				}
			})
		}
	}
}

// TestModelsGetNotFound pins the API error path: an object-form 404 is
// classified from the API body, not as a CLI decode failure.
func TestModelsGetNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": 404, "message": "models/nope is not found", "status": "NOT_FOUND",
		}})
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "models", "get", "nope")
	result := runCLI(t, t.TempDir(), nil, args...)
	if code := exitCode(t, result); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	var envelope struct {
		ErrorType   string `json:"error_type"`
		ErrorReason string `json:"error_reason"`
		Message     string `json:"message"`
		StatusCode  int    `json:"status_code"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
		t.Fatalf("stderr is not a JSON error envelope: %v\n%s", err, result.stderr)
	}
	if envelope.ErrorType != "not_found" || envelope.ErrorReason != "NOT_FOUND" || envelope.StatusCode != http.StatusNotFound {
		t.Errorf("envelope = %+v, want not_found / NOT_FOUND / 404", envelope)
	}
	if envelope.Message != "models/nope is not found" {
		t.Errorf("message = %q, want the API message", envelope.Message)
	}
}

// TestModelsListPageFlags pins that --page-size and --page-token reach the API
// as the camelCase query parameters, and that --max-pages needs --all.
func TestModelsListPageFlags(t *testing.T) {
	var gotQuery string
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "models/gemini-2.5-flash"}}})
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "models", "list", "--page-size", "5", "--page-token", "abc")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("models list failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if gotQuery != "pageSize=5&pageToken=abc" {
		t.Errorf("query = %q, want pageSize=5&pageToken=abc", gotQuery)
	}

	requests.Store(0)
	args = append(baseArgs(server.URL), "models", "list", "--max-pages", "2")
	result = runCLI(t, t.TempDir(), nil, args...)
	if code := exitCode(t, result); code != 2 {
		t.Errorf("--max-pages without --all: exit code = %d, want 2", code)
	}
	if requests.Load() != 0 {
		t.Errorf("--max-pages without --all reached the server %d times, want 0", requests.Load())
	}
}

// TestModelsBareRendersCuratedCatalog pins that the generated models group's
// bare invocation still renders the hand-curated catalog (Option A: the catalog
// RunE is merged onto the generated group), marking the default entry, without
// calling the API.
func TestModelsBareRendersCuratedCatalog(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	result := runCLI(t, t.TempDir(), nil, append(humanArgs(server.URL), "models")...)
	if result.err != nil {
		t.Fatalf("models failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stdout, "(default)") {
		t.Errorf("bare models does not mark the default model:\n%s", result.stdout)
	}
	if !strings.Contains(result.stdout, "gemini-") {
		t.Errorf("bare models does not list curated model ids:\n%s", result.stdout)
	}
	if requests.Load() != 0 {
		t.Errorf("bare models reached the server %d times, want 0", requests.Load())
	}
}

// TestModelsHelpSurface pins the reconciled command tree: the models group
// stays under "Manage" in root help, advertises its get/list leaves, and get
// documents its positional.
func TestModelsHelpSurface(t *testing.T) {
	root := runCLI(t, t.TempDir(), nil, "--help")
	manage := root.stdout
	if i := strings.Index(manage, "Manage:"); i >= 0 {
		manage = manage[i:]
	}
	if j := strings.Index(manage, "\n\n"); j >= 0 {
		manage = manage[:j]
	}
	if !strings.Contains(manage, "\n  models ") {
		t.Errorf("root help does not list models under Manage:\n%s", root.stdout)
	}

	group := runCLI(t, t.TempDir(), nil, "models", "--help")
	for _, want := range []string{"\n  get ", "\n  list "} {
		if !strings.Contains(group.stdout, want) {
			t.Errorf("models --help is missing %q:\n%s", strings.TrimSpace(want), group.stdout)
		}
	}

	get := runCLI(t, t.TempDir(), nil, "models", "get", "--help")
	if !strings.Contains(get.stdout, "gemini-api models get [model]") {
		t.Errorf("models get --help does not document the positional:\n%s", get.stdout)
	}
	if !strings.Contains(get.stdout, "gemini-api models get --model gemini-3.8-flash") {
		t.Errorf("models get --help does not show a real model id example:\n%s", get.stdout)
	}
}
