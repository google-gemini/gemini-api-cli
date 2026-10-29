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
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/cli"
	"github.com/spf13/cobra"
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

func TestEnvironmentsFilesListAll(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		if r.URL.Path != "/v1beta/environments/env_1/files/src" {
			t.Errorf("path = %q, want /v1beta/environments/env_1/files/src", r.URL.Path)
		}
		query := r.URL.Query()
		wantToken := ""
		if requestNumber == 2 {
			wantToken = "p2"
		}
		if got := query.Get("page_token"); got != wantToken {
			t.Errorf("request %d page_token = %q, want %q", requestNumber, got, wantToken)
		}
		if got := query.Get("page_size"); got != "2" {
			t.Errorf("request %d page_size = %q, want 2", requestNumber, got)
		}
		if got := query.Get("recursive"); got != "true" {
			t.Errorf("request %d recursive = %q, want true", requestNumber, got)
		}
		page := map[string]any{
			"files": []map[string]any{{"name": fmt.Sprintf("f%d", requestNumber)}},
		}
		if requestNumber == 1 {
			page["next_page_token"] = "p2"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()

	args := append(
		baseArgs(server.URL),
		"environments", "files", "list",
		"-e", "environments/env_1",
		"-p", "/src",
		"--recursive",
		"--page-size", "2",
		"--all",
	)
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("environments files list --all failed: %v\nstderr: %s", result.err, result.stderr)
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

	requests.Store(0)
	humanResult := runCLI(
		t,
		t.TempDir(),
		nil,
		append(
			humanArgs(server.URL),
			"environments", "files", "list",
			"-e", "environments/env_1",
			"-p", "/src",
			"--recursive",
			"--page-size", "2",
		)...,
	)
	if humanResult.err != nil {
		t.Fatalf("environments files list failed: %v\nstderr: %s", humanResult.err, humanResult.stderr)
	}
	if requests.Load() != 1 {
		t.Errorf("requests = %d, want 1", requests.Load())
	}
	if humanResult.stderr != paginationHint {
		t.Errorf("stderr = %q, want %q", humanResult.stderr, paginationHint)
	}
}

type listCommandPaginationCase struct {
	name          string
	commandPath   string
	args          []string
	wantPath      string
	wantQuery     map[string]string
	pageSizeParam string
	tokenParam    string
	nextTokenKey  string
	itemsKey      string
	item          func(page int) map[string]any
}

func listCommandPaginationCases() []listCommandPaginationCase {
	return []listCommandPaginationCase{
		{
			name:          "agent list",
			commandPath:   "gemini-api agent list",
			args:          []string{"agent", "list"},
			wantPath:      "/v1beta/agents",
			pageSizeParam: "page_size",
			tokenParam:    "page_token",
			nextTokenKey:  "next_page_token",
			itemsKey:      "agents",
			item: func(page int) map[string]any {
				return map[string]any{"id": fmt.Sprintf("agent-%d", page)}
			},
		},
		{
			name:          "credentials list",
			commandPath:   "gemini-api credentials list",
			args:          []string{"credentials", "list"},
			wantPath:      "/v1beta/credentials",
			pageSizeParam: "page_size",
			tokenParam:    "page_token",
			nextTokenKey:  "next_page_token",
			itemsKey:      "credentials",
			item: func(page int) map[string]any {
				return map[string]any{"id": fmt.Sprintf("cred-%d", page), "type": "bearer_token"}
			},
		},
		{
			name:          "environments list",
			commandPath:   "gemini-api environments list",
			args:          []string{"environments", "list"},
			wantPath:      "/v1beta/environments",
			pageSizeParam: "page_size",
			tokenParam:    "page_token",
			nextTokenKey:  "next_page_token",
			itemsKey:      "environments",
			item: func(page int) map[string]any {
				return map[string]any{"id": fmt.Sprintf("env_%d", page)}
			},
		},
		{
			name:          "environments files list (subpath)",
			commandPath:   "gemini-api environments files list",
			args:          []string{"environments", "files", "list", "-e", "environments/env_1", "-p", "/src", "--recursive"},
			wantPath:      "/v1beta/environments/env_1/files/src",
			wantQuery:     map[string]string{"recursive": "true"},
			pageSizeParam: "page_size",
			tokenParam:    "page_token",
			nextTokenKey:  "next_page_token",
			itemsKey:      "files",
			item: func(page int) map[string]any {
				return map[string]any{"name": fmt.Sprintf("f%d", page)}
			},
		},
		{
			name:          "environments files list (root)",
			commandPath:   "gemini-api environments files list",
			args:          []string{"environments", "files", "list", "-e", "environments/env_1"},
			wantPath:      "/v1beta/environments/env_1/files",
			pageSizeParam: "page_size",
			tokenParam:    "page_token",
			nextTokenKey:  "next_page_token",
			itemsKey:      "files",
			item: func(page int) map[string]any {
				return map[string]any{"name": fmt.Sprintf("root-f%d", page)}
			},
		},
		{
			name:          "files list",
			commandPath:   "gemini-api files list",
			args:          []string{"files", "list"},
			wantPath:      "/v1beta/files",
			pageSizeParam: "pageSize",
			tokenParam:    "pageToken",
			nextTokenKey:  "nextPageToken",
			itemsKey:      "files",
			item: func(page int) map[string]any {
				return map[string]any{"name": fmt.Sprintf("files/f-%d", page)}
			},
		},
		{
			name:          "files generated-files-list",
			commandPath:   "gemini-api files generated-files-list",
			args:          []string{"files", "generated-files-list"},
			wantPath:      "/v1beta/generatedFiles",
			pageSizeParam: "pageSize",
			tokenParam:    "pageToken",
			nextTokenKey:  "nextPageToken",
			itemsKey:      "generatedFiles",
			item: func(page int) map[string]any {
				return map[string]any{"name": fmt.Sprintf("generatedFiles/gf-%d", page)}
			},
		},
		{
			name:          "models list",
			commandPath:   "gemini-api models list",
			args:          []string{"models", "list"},
			wantPath:      "/v1beta/models",
			pageSizeParam: "pageSize",
			tokenParam:    "pageToken",
			nextTokenKey:  "nextPageToken",
			itemsKey:      "models",
			item: func(page int) map[string]any {
				return map[string]any{"name": fmt.Sprintf("models/gemini-2.5-flash-%d", page)}
			},
		},
		{
			name:          "triggers list",
			commandPath:   "gemini-api triggers list",
			args:          []string{"triggers", "list"},
			wantPath:      "/v1beta/triggers",
			pageSizeParam: "page_size",
			tokenParam:    "page_token",
			nextTokenKey:  "next_page_token",
			itemsKey:      "triggers",
			item: func(page int) map[string]any {
				return map[string]any{
					"id":        fmt.Sprintf("trig-%d", page),
					"schedule":  "0 * * * *",
					"time_zone": "UTC",
					"interaction": map[string]any{
						"agent": "deep-research-preview-04-2026",
					},
				}
			},
		},
		{
			name:          "triggers list-executions",
			commandPath:   "gemini-api triggers list-executions",
			args:          []string{"triggers", "list-executions", "--id", "trig-1"},
			wantPath:      "/v1beta/triggers/trig-1/executions",
			pageSizeParam: "page_size",
			tokenParam:    "page_token",
			nextTokenKey:  "next_page_token",
			itemsKey:      "trigger_executions",
			item: func(page int) map[string]any {
				return map[string]any{
					"id":         fmt.Sprintf("exec-%d", page),
					"trigger_id": "trig-1",
				}
			},
		},
		{
			name:          "voices list",
			commandPath:   "gemini-api voices list",
			args:          []string{"voices", "list"},
			wantPath:      "/v1beta/voices",
			pageSizeParam: "page_size",
			tokenParam:    "page_token",
			nextTokenKey:  "next_page_token",
			itemsKey:      "voices",
			item: func(page int) map[string]any {
				return map[string]any{
					"id":   fmt.Sprintf("voice_%d", page),
					"type": "prompted",
				}
			},
		},
		{
			name:          "webhooks list",
			commandPath:   "gemini-api webhooks list",
			args:          []string{"webhooks", "list"},
			wantPath:      "/v1beta/webhooks",
			pageSizeParam: "page_size",
			tokenParam:    "page_token",
			nextTokenKey:  "next_page_token",
			itemsKey:      "webhooks",
			item: func(page int) map[string]any {
				return map[string]any{
					"id":                fmt.Sprintf("wh-%d", page),
					"uri":               "https://example.com/hook",
					"subscribed_events": []string{"interaction.completed"},
				}
			},
		},
	}
}

// TestAllListCommandsSupportPagination verifies that every list or cursored
// command in the CLI tree registers --all / --max-pages and traverses multiple
// pages end-to-end when --all is passed.
func TestAllListCommandsSupportPagination(t *testing.T) {
	root, err := cli.NewRootCommand()
	if err != nil {
		t.Fatalf("NewRootCommand() failed: %v", err)
	}

	var discovered []string
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
		if len(cmd.Commands()) > 0 {
			return
		}
		name := cmd.Name()
		isListNamed := name == "list" || strings.HasPrefix(name, "list-") || strings.HasSuffix(name, "-list")
		hasPageFlags := cmd.Flags().Lookup("page-token") != nil || cmd.Flags().Lookup("page-size") != nil
		hasAllFlags := cmd.Flags().Lookup("all") != nil || cmd.Flags().Lookup("max-pages") != nil
		if !isListNamed && !hasPageFlags && !hasAllFlags {
			return
		}
		path := cmd.CommandPath()
		discovered = append(discovered, path)
		allFlag := cmd.Flags().Lookup("all")
		if allFlag == nil {
			t.Errorf("%s is missing --all flag (all list/paginated commands must declare x-speakeasy-pagination)", path)
		} else if allFlag.Shorthand != "a" {
			t.Errorf("%s --all shorthand = %q, want \"a\"", path, allFlag.Shorthand)
		}
		if cmd.Flags().Lookup("max-pages") == nil {
			t.Errorf("%s is missing --max-pages flag", path)
		}
		if cmd.Flags().Lookup("page-token") == nil {
			t.Errorf("%s is missing --page-token flag", path)
		}
		if cmd.Flags().Lookup("page-size") == nil {
			t.Errorf("%s is missing --page-size flag", path)
		}
	}
	walk(root)
	sort.Strings(discovered)

	cases := listCommandPaginationCases()
	coveredSet := make(map[string]struct{}, len(cases))
	for _, tc := range cases {
		coveredSet[tc.commandPath] = struct{}{}
	}
	covered := make([]string, 0, len(coveredSet))
	for path := range coveredSet {
		covered = append(covered, path)
	}
	sort.Strings(covered)
	if !reflect.DeepEqual(discovered, covered) {
		t.Fatalf("paginated CLI commands = %v, contract test cases cover = %v", discovered, covered)
	}

	for _, path := range covered {
		cmdArgs := append(strings.Fields(strings.TrimPrefix(path, "gemini-api ")), "--usage")
		usageRes := runCLI(t, t.TempDir(), nil, cmdArgs...)
		if usageRes.err != nil {
			t.Fatalf("%s --usage failed: %v\nstderr: %s", path, usageRes.err, usageRes.stderr)
		}
		for _, want := range []string{
			`flag "--page-size <page_size>"`,
			`flag "--page-token <page_token>"`,
			`flag "-a --all"`,
			`flag "--max-pages <max_pages>"`,
		} {
			if !strings.Contains(usageRes.stdout, want) {
				t.Errorf("%s --usage missing %q:\n%s", path, want, usageRes.stdout)
			}
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reqNum := int(requests.Add(1))
				if r.URL.Path != tc.wantPath {
					t.Errorf("request %d path = %q, want %q", reqNum, r.URL.Path, tc.wantPath)
				}
				if reqNum > 2 {
					http.Error(w, "too many requests", http.StatusInternalServerError)
					return
				}
				query := r.URL.Query()
				wantToken := ""
				if reqNum == 2 {
					wantToken = "p2"
				}
				if got := query.Get(tc.tokenParam); got != wantToken {
					t.Errorf("request %d %s = %q, want %q", reqNum, tc.tokenParam, got, wantToken)
				}
				if got := query.Get(tc.pageSizeParam); got != "2" {
					t.Errorf("request %d %s = %q, want \"2\"", reqNum, tc.pageSizeParam, got)
				}
				for k, wantVal := range tc.wantQuery {
					if got := query.Get(k); got != wantVal {
						t.Errorf("request %d query %s = %q, want %q", reqNum, k, got, wantVal)
					}
				}
				page := map[string]any{
					tc.itemsKey: []map[string]any{tc.item(reqNum)},
				}
				if reqNum == 1 {
					page[tc.nextTokenKey] = "p2"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(page)
			}))
			defer server.Close()

			args := append(append(baseArgs(server.URL), tc.args...), "--page-size", "2", "--all")
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("%s --all failed: %v\nstderr: %s", tc.name, result.err, result.stderr)
			}
			if got := requests.Load(); got != 2 {
				t.Errorf("requests = %d, want 2", got)
			}
			pages := decodeNDJSON(t, result.stdout)
			if len(pages) != 2 {
				t.Fatalf("NDJSON pages = %d, want 2\nstdout: %s", len(pages), result.stdout)
			}
			for i, page := range pages {
				items, ok := page[tc.itemsKey].([]any)
				if !ok || len(items) != 1 {
					t.Errorf("page %d %s = %#v, want 1 item", i+1, tc.itemsKey, page[tc.itemsKey])
				}
			}
			if result.stderr != "" {
				t.Errorf("stderr = %q, want empty", result.stderr)
			}
		})
	}
}

// TestListHintPolicy pins the "more pages" stderr hint across every cursored
// list command: human mode with a continuation only, never on a terminal page
// or in machine modes.
func TestListHintPolicy(t *testing.T) {
	for _, tc := range listCommandPaginationCases() {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.wantPath {
					t.Errorf("path = %q, want %q", r.URL.Path, tc.wantPath)
				}
				page := map[string]any{
					tc.itemsKey: []map[string]any{tc.item(1)},
				}
				if r.URL.Query().Get(tc.tokenParam) != "terminal" {
					page[tc.nextTokenKey] = "next"
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
					args:       append(humanArgs(server.URL), tc.args...),
					wantStderr: paginationHint,
				},
				{
					name: "human terminal page",
					args: append(append(humanArgs(server.URL), tc.args...), "--page-token", "terminal"),
				},
				{
					name: "JSON",
					args: append(baseArgs(server.URL), tc.args...),
				},
				{
					name: "jq",
					args: append(append(humanArgs(server.URL), "--jq", "."+tc.itemsKey), tc.args...),
				},
				{
					name: "agent mode",
					args: append(append(humanArgs(server.URL), "--agent-mode"), tc.args...),
				},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					result := runCLI(t, t.TempDir(), nil, tt.args...)
					if result.err != nil {
						t.Fatalf("%s failed: %v\nstderr: %s", tc.name, result.err, result.stderr)
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
