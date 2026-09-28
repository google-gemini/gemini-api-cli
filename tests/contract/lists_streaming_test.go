package contract_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const paginationHint = "Hint: more pages available. Use --all to fetch all results, or --page-token for manual pagination.\n"

func humanArgs(serverURL string) []string {
	return []string{
		"--server-url", serverURL,
		"--api-version", "v1beta",
		"--no-interactive",
		"--no-retries",
		"--color", "never",
	}
}

func decodeNDJSON(t *testing.T, output string) []map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		t.Fatal("stdout is empty, want NDJSON")
	}
	lines := strings.Split(trimmed, "\n")
	pages := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var page map[string]any
		if err := json.Unmarshal([]byte(line), &page); err != nil {
			t.Fatalf("NDJSON line is not JSON: %v\nline: %s", err, line)
		}
		pages = append(pages, page)
	}
	return pages
}

func TestAgentListAllSnakeCase(t *testing.T) {
	expectedTokens := []string{"", "snake-2", "snake-3"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestIndex := int(requests.Add(1)) - 1
		if r.URL.Path != "/v1beta/agents" {
			t.Errorf("path = %q, want /v1beta/agents", r.URL.Path)
		}
		if requestIndex >= len(expectedTokens) {
			http.Error(w, "too many requests", http.StatusInternalServerError)
			return
		}
		query := r.URL.Query()
		if got := query.Get("page_token"); got != expectedTokens[requestIndex] {
			t.Errorf("request %d page_token = %q, want %q", requestIndex+1, got, expectedTokens[requestIndex])
		}
		if got := query.Get("page_size"); got != "2" {
			t.Errorf("request %d page_size = %q, want 2", requestIndex+1, got)
		}

		page := map[string]any{
			"agents": []map[string]any{{"id": fmt.Sprintf("agent-%d", requestIndex+1)}},
		}
		if requestIndex+1 < len(expectedTokens) {
			page["next_page_token"] = expectedTokens[requestIndex+1]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "agent", "list", "--page-size", "2", "--all")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("agent list --all failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if requests.Load() != 3 {
		t.Errorf("requests = %d, want 3", requests.Load())
	}
	if pages := decodeNDJSON(t, result.stdout); len(pages) != 3 {
		t.Errorf("NDJSON pages = %d, want 3", len(pages))
	}
	if result.stderr != "" {
		t.Errorf("stderr = %q, want empty", result.stderr)
	}
}

func TestFilesListAll(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		if r.URL.Path != "/v1beta/files" {
			t.Errorf("path = %q, want /v1beta/files", r.URL.Path)
		}
		wantToken := ""
		if requestNumber == 2 {
			wantToken = "next"
		}
		if got := r.URL.Query().Get("pageToken"); got != wantToken {
			t.Errorf("request %d pageToken = %q, want %q", requestNumber, got, wantToken)
		}
		page := map[string]any{"files": []map[string]any{{"name": "files/example"}}}
		if requestNumber == 1 {
			page["nextPageToken"] = "next"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "files", "list", "--all")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("files list --all failed: %v\nstderr: %s", result.err, result.stderr)
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

// TestListHintPolicy pins the "more pages" stderr hint on the camelCase
// cursored lists: human mode with a continuation only, never in machine modes.
func TestListHintPolicy(t *testing.T) {
	for _, resource := range []string{"files", "models"} {
		t.Run(resource, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := map[string]any{
					resource: []map[string]any{{"name": resource + "/example"}},
				}
				if r.URL.Query().Get("pageToken") != "terminal" {
					page["nextPageToken"] = "next"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(page)
			}))
			defer server.Close()

			tests := []struct {
				name       string
				args       []string
				wantStderr string
			}{
				{
					name:       "human continuation",
					args:       append(humanArgs(server.URL), resource, "list"),
					wantStderr: paginationHint,
				},
				{
					name: "human terminal page",
					args: append(humanArgs(server.URL), resource, "list", "--page-token", "terminal"),
				},
				{
					name: "JSON",
					args: append(baseArgs(server.URL), resource, "list"),
				},
				{
					name: "jq",
					args: append(humanArgs(server.URL), "--jq", "."+resource, resource, "list"),
				},
				{
					name: "agent mode",
					args: append(humanArgs(server.URL), "--agent-mode", resource, "list"),
				},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					result := runCLI(t, t.TempDir(), nil, tt.args...)
					if result.err != nil {
						t.Fatalf("%s list failed: %v\nstderr: %s", resource, result.err, result.stderr)
					}
					if result.stderr != tt.wantStderr {
						t.Errorf("stderr = %q, want %q", result.stderr, tt.wantStderr)
					}
				})
			}
		})
	}
}

func TestPaginationMaxPages(t *testing.T) {
	runCapped := func(t *testing.T, human bool) (commandResult, int32) {
		t.Helper()
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestNumber := requests.Add(1)
			page := map[string]any{
				"agents":          []map[string]any{{"id": fmt.Sprintf("agent-%d", requestNumber)}},
				"next_page_token": fmt.Sprintf("token-%d", requestNumber+1),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(page)
		}))
		t.Cleanup(server.Close)

		args := baseArgs(server.URL)
		if human {
			args = humanArgs(server.URL)
		}
		args = append(args, "agent", "list", "--all", "--max-pages", "2")
		return runCLI(t, t.TempDir(), nil, args...), requests.Load()
	}

	t.Run("JSON request bound", func(t *testing.T) {
		result, requests := runCapped(t, false)
		if result.err != nil {
			t.Fatalf("agent list failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if requests != 2 {
			t.Errorf("requests = %d, want 2", requests)
		}
		if pages := decodeNDJSON(t, result.stdout); len(pages) != 2 {
			t.Errorf("NDJSON pages = %d, want 2", len(pages))
		}
		if result.stderr != "" {
			t.Errorf("stderr = %q, want empty", result.stderr)
		}
	})

	t.Run("human diagnostic", func(t *testing.T) {
		result, requests := runCapped(t, true)
		if result.err != nil {
			t.Fatalf("agent list failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if requests != 2 {
			t.Errorf("requests = %d, want 2", requests)
		}
		want := "Stopped after 2 pages (--max-pages); more results may be available.\n"
		if result.stderr != want {
			t.Errorf("stderr = %q, want %q", result.stderr, want)
		}
	})

	t.Run("validation before network", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"files":[]}`)
		}))
		defer server.Close()

		tests := []struct {
			name string
			args []string
			want string
		}{
			{
				name: "requires all",
				args: append(baseArgs(server.URL), "files", "list", "--max-pages", "1"),
				want: "--max-pages requires --all",
			},
			{
				name: "negative",
				args: append(baseArgs(server.URL), "files", "list", "--all", "--max-pages", "-1"),
				want: "--max-pages must be zero or greater",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				requests.Store(0)
				result := runCLI(t, t.TempDir(), nil, tt.args...)
				if result.err == nil {
					t.Fatal("command unexpectedly succeeded")
				}
				if !strings.Contains(result.stderr, tt.want) {
					t.Errorf("stderr = %q, want it to contain %q", result.stderr, tt.want)
				}
				if requests.Load() != 0 {
					t.Errorf("requests = %d, want 0", requests.Load())
				}
			})
		}
	})
}

func TestPaginationRepeatedCursorIsClassifiedProtocolError(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agents":          []map[string]any{{"id": fmt.Sprintf("agent-%d", requestNumber)}},
			"next_page_token": "same",
		})
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "agent", "list", "--all")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatal("agent list unexpectedly succeeded for a repeated cursor")
	}
	if requests.Load() != 2 {
		t.Errorf("requests = %d, want 2", requests.Load())
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
		t.Fatalf("stderr is not a JSON envelope: %v\n%s", err, result.stderr)
	}
	if envelope["error_reason"] != "CLI_PROTOCOL" {
		t.Errorf("error_reason = %#v, want CLI_PROTOCOL", envelope["error_reason"])
	}
	if envelope["error_type"] != "protocol_error" {
		t.Errorf("error_type = %#v, want protocol_error", envelope["error_type"])
	}
}

func TestPaginationRetriesCarryAcrossPages(t *testing.T) {
	var requests atomic.Int32
	var secondPageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "t2" {
			if secondPageRequests.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"error":{"code":429,"message":"retry page"}}`)
				return
			}
			fmt.Fprint(w, `{"files":[{"name":"files/page-2"}]}`)
			return
		}
		fmt.Fprint(w, `{"files":[{"name":"files/page-1"}],"nextPageToken":"t2"}`)
	}))
	defer server.Close()

	args := []string{
		"--server-url", server.URL,
		"--api-version", "v1beta",
		"--no-interactive",
		"--output-format", "json",
		"--color", "never",
		"--timeout", "5s",
		"files", "list", "--all",
	}
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("files list --all failed after retry: %v\nstderr: %s", result.err, result.stderr)
	}
	if requests.Load() != 3 {
		t.Errorf("total requests = %d, want 3", requests.Load())
	}
	if secondPageRequests.Load() != 2 {
		t.Errorf("page-two requests = %d, want 2", secondPageRequests.Load())
	}
	if pages := decodeNDJSON(t, result.stdout); len(pages) != 2 {
		t.Errorf("NDJSON pages = %d, want 2", len(pages))
	}
	if result.stderr != "" {
		t.Errorf("stderr = %q, want empty", result.stderr)
	}
}

func TestAgentStatusStreamFalseReturnsJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("stream"); got != "false" {
			t.Errorf("stream = %q, want false", got)
		}
		if got := r.Header.Get("Accept"); strings.Contains(got, "text/event-stream") && !strings.Contains(got, "q=0") {
			t.Errorf("Accept = %q, text/event-stream must not be acceptable", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"interaction-1","status":"completed"}`)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "agent", "status", "--id", "interaction-1", "--stream=false")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("agent status --stream=false failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("stdout is not one JSON result: %v\n%s", err, result.stdout)
	}
	if response["id"] != "interaction-1" {
		t.Errorf("id = %#v, want interaction-1", response["id"])
	}
	if result.stderr != "" {
		t.Errorf("stderr = %q, want empty", result.stderr)
	}
}
