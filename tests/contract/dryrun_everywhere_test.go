package contract_test

import (
	"bufio"
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

type dryRunMatrixCase struct {
	name             string
	args             []string
	requests         int
	localOutput      bool
	wantHumanStderr  string
	wantJSONContains string
	absentPaths      []string
}

func dryRunPreviewLines(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var previews []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		var preview map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &preview); err != nil {
			t.Fatalf("preview line is not JSON: %v\nline: %s", err, scanner.Text())
		}
		if preview["dry_run"] != true {
			t.Fatalf("preview dry_run = %#v", preview["dry_run"])
		}
		if preview["local"] == true {
			continue
		}
		request, ok := preview["request"].(map[string]any)
		if !ok {
			t.Fatalf("preview request = %#v", preview["request"])
		}
		if request["method"] == "" || request["url"] == "" {
			t.Fatalf("incomplete request preview: %#v", request)
		}
		if _, ok := request["headers"].(map[string]any); !ok {
			t.Fatalf("preview headers = %#v", request["headers"])
		}
		if _, exists := request["body"]; !exists {
			t.Fatalf("preview omits body: %#v", request)
		}
		previews = append(previews, preview)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return previews
}

func TestDryRunEverywhereKeylessHumanAndJSON(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	fixtures := t.TempDir()
	imageInput := filepath.Join(fixtures, "input.png")
	audioA := filepath.Join(fixtures, "a.wav")
	audioB := filepath.Join(fixtures, "b.wav")
	upload := filepath.Join(fixtures, "upload.wav")
	for path, data := range map[string][]byte{
		imageInput: []byte("image"), audioA: []byte("audio-a"), audioB: []byte("audio-b"),
	} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(upload, make([]byte, (8<<20)+5), 0o644); err != nil {
		t.Fatal(err)
	}
	imageOut := filepath.Join(fixtures, "image-new", "x.png")
	ttsOut := filepath.Join(fixtures, "tts-new", "y.wav")
	transcribeOut := filepath.Join(fixtures, "transcribe-new")
	cases := []dryRunMatrixCase{
		{name: "generate streaming", args: []string{"generate", "hi"}, requests: 1},
		{name: "generate unary", args: []string{"generate", "hi", "--stream=false"}, requests: 1},
		{name: "image nested out", args: []string{"image", "a lighthouse", "--out", imageOut}, requests: 1, absentPaths: []string{filepath.Dir(imageOut)}},
		{name: "tts nested out", args: []string{"tts", "hello", "--out", ttsOut}, requests: 1, absentPaths: []string{filepath.Dir(ttsOut)}},
		{name: "video", args: []string{"video", "a city"}, requests: 1},
		{name: "music", args: []string{"music", "synthwave"}, requests: 1},
		{name: "analyze local", args: []string{"analyze", "-i", imageInput}, requests: 1},
		{name: "analyze remote", args: []string{"analyze", "-i", "files/abc"}, requests: 2},
		{name: "transcribe multiple", args: []string{"transcribe", "-i", audioA, "-i", audioB, "--out", transcribeOut}, requests: 2, absentPaths: []string{transcribeOut}},
		// The catalog is a padded table in human mode and a structured value list
		// under machine formats; both mark the default entry.
		{name: "models catalog", args: []string{"models"}, localOutput: true, wantJSONContains: "default"},
		{name: "models list", args: []string{"models", "list"}, requests: 1},
		{name: "models get", args: []string{"models", "get", "gemini-2.5-flash"}, requests: 1},
		{name: "files upload chunks", args: []string{"files", "upload", upload}, requests: 3},
		{name: "files list", args: []string{"files", "list"}, requests: 1},
		{name: "agent run", args: []string{"agent", "run", "--body", `{"model":"gemini-3.6-flash","input":"hi"}`}, requests: 1},
		{name: "agent list", args: []string{"agent", "list"}, requests: 1},
		{name: "configure", args: []string{"configure"}, localOutput: true, wantHumanStderr: "[DRY-RUN] configure changes local settings only"},
		{name: "auth whoami", args: []string{"auth", "whoami"}, localOutput: true, wantJSONContains: "config_file"},
		{name: "whoami", args: []string{"whoami"}, localOutput: true, wantJSONContains: "config_file"},
		{name: "version", args: []string{"version"}, localOutput: true, wantJSONContains: `"version"`},
		{name: "files get", args: []string{"files", "get", "files/abc"}, requests: 1},
		{name: "credentials list", args: []string{"credentials", "list"}, requests: 1},
		{name: "credentials get", args: []string{"credentials", "get", "gh-token"}, requests: 1},
		{name: "credentials create", args: []string{"credentials", "create", "--body-param.bearer-token.id", "gh-token", "--body-param.bearer-token.token", "t"}, requests: 1},
		{name: "credentials update", args: []string{"credentials", "update", "gh-token", "--body-param.bearer-token.token", "t"}, requests: 1},
		{name: "credentials delete", args: []string{"credentials", "delete", "gh-token"}, requests: 1},
	}

	for _, tt := range cases {
		for _, mode := range []string{"human", "json"} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				before := hits.Load()
				args := []string{"--server-url", server.URL, "--api-version", "v1beta", "--no-interactive", "--no-retries", "--color", "never", "--dry-run"}
				if mode == "json" {
					args = append(args, "--output-format", "json")
				}
				args = append(args, tt.args...)
				result := runCLI(t, t.TempDir(), nil, args...)
				if result.err != nil {
					t.Fatalf("dry-run failed: %v\nstdout: %s\nstderr: %s", result.err, result.stdout, result.stderr)
				}
				if got := hits.Load(); got != before {
					t.Fatalf("dry-run reached server: hits %d -> %d", before, got)
				}
				for _, path := range tt.absentPaths {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Errorf("dry-run created %s (stat error %v)", path, err)
					}
				}

				if tt.requests > 0 {
					if mode == "human" {
						if result.stdout != "" {
							t.Errorf("human API dry-run stdout = %q", result.stdout)
						}
						if got := strings.Count(result.stderr, "[DRY-RUN] Would send"); got != tt.requests {
							t.Errorf("human previews = %d, want %d\nstderr: %s", got, tt.requests, result.stderr)
						}
					} else {
						if result.stderr != "" {
							t.Errorf("JSON API dry-run stderr = %q", result.stderr)
						}
						if got := len(dryRunPreviewLines(t, result.stdout)); got != tt.requests {
							t.Errorf("JSON previews = %d, want %d\nstdout: %s", got, tt.requests, result.stdout)
						}
					}
				} else if tt.name == "configure" {
					if mode == "human" && !strings.Contains(result.stderr, tt.wantHumanStderr) {
						t.Errorf("configure dry-run message missing: %s", result.stderr)
					}
					if mode == "json" {
						if result.stderr != "" {
							t.Errorf("JSON configure dry-run stderr = %q", result.stderr)
						}
						var noop map[string]any
						if err := json.Unmarshal([]byte(strings.TrimSpace(result.stdout)), &noop); err != nil {
							t.Fatalf("JSON configure dry-run must emit one local no-op object: %v\nstdout: %s", err, result.stdout)
						}
						if noop["dry_run"] != true || noop["local"] != true || noop["command"] != "gemini-api configure" {
							t.Errorf("local no-op object = %#v", noop)
						}
						if _, hasRequest := noop["request"]; hasRequest {
							t.Errorf("local no-op must not carry a request: %#v", noop)
						}
						if msg, _ := noop["message"].(string); msg == "" {
							t.Errorf("local no-op message empty: %#v", noop)
						}
					}
				} else if result.stdout == "" {
					t.Errorf("local read-only command lost its deliverable")
				}
				if mode == "json" && tt.wantJSONContains != "" && !strings.Contains(result.stdout, tt.wantJSONContains) {
					t.Errorf("local read-only deliverable does not contain %q: %s", tt.wantJSONContains, result.stdout)
				}
			})
		}
	}
}

func TestDryRunValidatesAllLocalMediaBeforeFirstRequest(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.wav")
	for _, command := range [][]string{
		{"analyze", "-i", "files/abc", "-i", missing},
		{"transcribe", "-i", "files/abc", "-i", missing},
	} {
		args := []string{"--server-url", "https://example.invalid", "--no-interactive", "--dry-run", "--output-format", "json"}
		args = append(args, command...)
		result := runCLI(t, t.TempDir(), nil, args...)
		if result.err == nil {
			t.Fatalf("invalid later local input unexpectedly succeeded: %v", command)
		}
		if result.stdout != "" {
			t.Errorf("request was previewed before all local validation: %s", result.stdout)
		}
	}
}

func TestDryRunValidationFailureComposesWithClassifiedJSON(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	missing := filepath.Join(t.TempDir(), "missing.wav")
	result := runCLI(t, t.TempDir(), nil,
		"--server-url", server.URL,
		"--api-version", "v1beta",
		"--no-interactive",
		"--no-retries",
		"--color", "never",
		"--dry-run",
		"--output-format", "json",
		"--agent-mode",
		"analyze", "-i", missing,
	)
	if result.err == nil {
		t.Fatalf("missing local input unexpectedly succeeded: stdout=%s stderr=%s", result.stdout, result.stderr)
	}
	if result.stdout != "" {
		t.Fatalf("validation failure stdout = %q, want empty", result.stdout)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.stderr)), &envelope); err != nil {
		t.Fatalf("stderr is not exactly one JSON envelope: %v\nstderr: %s", err, result.stderr)
	}
	if got := envelope["error_type"]; got != "validation_error" {
		t.Errorf("error_type = %#v, want validation_error", got)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("validation failure reached server %d times", got)
	}
}

func TestFilesUploadDryRunValidatesReadabilityBeforePreview(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "unreadable.wav")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if probe, err := os.Open(path); err == nil {
		probe.Close()
		t.Skip("current user can open mode-000 files")
	}

	result := runCLI(t, t.TempDir(), nil,
		"--server-url", server.URL,
		"--api-version", "v1beta",
		"--no-interactive",
		"--no-retries",
		"--color", "never",
		"--dry-run",
		"--output-format", "json",
		"files", "upload", path,
	)
	if result.err == nil {
		t.Fatalf("unreadable upload unexpectedly succeeded: stdout=%s stderr=%s", result.stdout, result.stderr)
	}
	if result.stdout != "" {
		t.Errorf("unreadable upload emitted a partial preview: %s", result.stdout)
	}
	if strings.Contains(result.stderr, "[DRY-RUN]") || strings.Contains(result.stderr, `"dry_run":true`) {
		t.Errorf("unreadable upload emitted a partial preview: %s", result.stderr)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("unreadable upload reached server %d times", got)
	}
}
