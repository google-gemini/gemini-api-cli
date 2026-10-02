package contract_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var cliBinary string

func TestMain(m *testing.M) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "resolve contract test path")
		os.Exit(1)
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	tempDir, err := os.MkdirTemp("", "gemini-api-contract-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(tempDir)

	cliBinary = filepath.Join(tempDir, "gemini-api")
	cmd := exec.Command("go", "build", "-o", cliBinary, "./cmd/gemini-api")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build gemini-api: %v\n%s", err, output)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

type commandResult struct {
	stdout string
	stderr string
	err    error
}

func runCLI(t *testing.T, home string, env map[string]string, args ...string) commandResult {
	t.Helper()
	return runCLIInDir(t, home, "", env, args...)
}

// runCLIInDir runs the CLI with the working directory set to dir ("" keeps
// the test process's directory); artifact commands resolve default output
// filenames against it.
func runCLIInDir(t *testing.T, home, dir string, env map[string]string, args ...string) commandResult {
	t.Helper()

	cmd := exec.Command(cliBinary, args...)
	cmd.Dir = dir
	cmd.Env = isolatedEnv(home, env)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return commandResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// TestImageArtifactOutput pins the media-intent UX: "image" writes the
// returned image to a file and prints only the absolute path (pretty mode),
// honors --out as a file or directory, keeps the API envelope reachable via
// --raw-response, and never writes under --dry-run.
func TestImageArtifactOutput(t *testing.T) {
	// 1x1 PNG.
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNgYAAAAAMAAVCiT19+AAAAAElFTkSuQmCC"
	requests := make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requests <- b
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"int-img","object":"interaction","status":"completed","model":"gemini-3.1-flash-image",`+
			`"steps":[{"type":"model_output","content":[{"type":"image","data":"`+pngB64+`","mime_type":"image/png"}]}]}`)
	}))
	defer server.Close()

	pretty := func(dir string, extra ...string) commandResult {
		args := []string{"--server-url", server.URL, "--api-version", "v1beta", "--no-interactive", "--no-retries", "--color", "never", "--api-key", "test", "image", "a lighthouse at sunset"}
		args = append(args, extra...)
		return runCLIInDir(t, t.TempDir(), dir, nil, args...)
	}

	// Default: file in the working directory, stdout is exactly the absolute
	// path. (Resolve symlinks: macOS TempDir lives under /var -> /private/var.)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result := pretty(dir)
	if result.err != nil {
		t.Fatalf("image failed: %v\nstderr: %s", result.err, result.stderr)
	}
	printed := strings.TrimSpace(result.stdout)
	if strings.Contains(printed, "\n") || !filepath.IsAbs(printed) {
		t.Fatalf("stdout must be exactly one absolute path, got %q", result.stdout)
	}
	if filepath.Dir(printed) != dir || !strings.HasPrefix(filepath.Base(printed), "gemini-image-") || filepath.Ext(printed) != ".png" {
		t.Errorf("default path = %q, want ./gemini-image-*.png inside %s", printed, dir)
	}
	if data, err := os.ReadFile(printed); err != nil || len(data) != 69 {
		t.Errorf("written file %q: err=%v len=%d, want the 69-byte PNG", printed, err, len(data))
	}
	if !strings.Contains(result.stderr, "Wrote image to ") {
		t.Errorf("progress note missing from stderr:\n%s", result.stderr)
	}
	var sent struct {
		Model          string `json:"model"`
		ResponseFormat struct {
			Type string `json:"type"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(<-requests, &sent); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if sent.Model != "gemini-3.1-flash-image" || sent.ResponseFormat.Type != "image" {
		t.Errorf("request presets = %+v, want image model + image response format", sent)
	}

	// --out file: extension follows the returned MIME type; overwrite is silent.
	target := filepath.Join(dir, "shots", "hero.jpg")
	result = pretty(dir, "--out", target)
	if result.err != nil {
		t.Fatalf("image --out failed: %v\nstderr: %s", result.err, result.stderr)
	}
	wantPath := filepath.Join(dir, "shots", "hero.png")
	if strings.TrimSpace(result.stdout) != wantPath {
		t.Errorf("--out path = %q, want %q (extension follows image/png)", strings.TrimSpace(result.stdout), wantPath)
	}
	<-requests
	if result = pretty(dir, "--out", target); result.err != nil {
		t.Fatalf("second image --out failed (must overwrite silently): %v\nstderr: %s", result.err, result.stderr)
	}
	<-requests

	// --out directory: default filename inside it.
	outDir := filepath.Join(dir, "renders")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	result = pretty(dir, "--out", outDir)
	if result.err != nil {
		t.Fatalf("image --out <dir> failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if got := strings.TrimSpace(result.stdout); filepath.Dir(got) != outDir || filepath.Ext(got) != ".png" {
		t.Errorf("--out <dir> path = %q, want a .png inside %s", got, outDir)
	}
	<-requests

	// Structured output reports the artifact envelope, not the payload.
	result = pretty(dir, "--out", filepath.Join(dir, "j.png"), "--output-format", "json")
	if result.err != nil {
		t.Fatalf("image --output-format json failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var envelope struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
		Mime string `json:"mime_type"`
		Size int    `json:"size_bytes"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
		t.Fatalf("json output is not the artifact envelope: %v\n%s", err, result.stdout)
	}
	if envelope.Path != filepath.Join(dir, "j.png") || envelope.Kind != "image" || envelope.Mime != "image/png" || envelope.Size != 69 {
		t.Errorf("artifact envelope = %+v", envelope)
	}
	if strings.Contains(result.stdout, pngB64) {
		t.Error("structured output must not carry the base64 payload")
	}
	<-requests

	// --raw-response restores the API envelope and writes nothing.
	rawDir := t.TempDir()
	result = pretty(rawDir, "--raw-response", "--output-format", "json")
	if result.err != nil {
		t.Fatalf("image --raw-response failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stdout, pngB64) {
		t.Errorf("--raw-response must print the API response:\n%s", result.stdout)
	}
	if entries, _ := os.ReadDir(rawDir); len(entries) != 0 {
		t.Errorf("--raw-response wrote files: %v", entries)
	}
	<-requests

	// --dry-run previews the request and writes nothing.
	dryDir := t.TempDir()
	result = pretty(dryDir, "--dry-run")
	if result.err != nil {
		t.Fatalf("image --dry-run failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stderr, "[DRY-RUN]") || !strings.Contains(result.stderr, "gemini-3.1-flash-image") {
		t.Errorf("dry-run preview missing:\n%s", result.stderr)
	}
	if entries, _ := os.ReadDir(dryDir); len(entries) != 0 {
		t.Errorf("--dry-run wrote files: %v", entries)
	}
	if strings.TrimSpace(result.stdout) != "" {
		t.Errorf("dry-run stdout should be empty, got %q", result.stdout)
	}
}

// TestImageArtifactMissingContent pins the failure contract: no image content
// (or URI-only delivery) is a hard error with an empty stdout and no file.
func TestImageArtifactMissingContent(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"in-progress": {`{"id":"int-1","status":"in_progress"}`, `no image content to write (status "in_progress", id "int-1"`},
		"text-only":   {`{"id":"int-2","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"hi"}]}]}`, "no image content to write"},
		"uri-only":    {`{"id":"int-3","status":"completed","steps":[{"type":"model_output","content":[{"type":"image","uri":"https://media.example.test/a.png"}]}]}`, "delivered by URI"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			dir := t.TempDir()
			args := []string{"--server-url", server.URL, "--api-version", "v1beta", "--no-interactive", "--no-retries", "--color", "never", "--api-key", "test", "image", "a lighthouse"}
			result := runCLIInDir(t, t.TempDir(), dir, nil, args...)
			if result.err == nil {
				t.Fatalf("image unexpectedly succeeded:\nstdout: %s", result.stdout)
			}
			if !strings.Contains(result.stderr, tc.want) {
				t.Errorf("stderr = %q, want %q", result.stderr, tc.want)
			}
			if strings.TrimSpace(result.stdout) != "" {
				t.Errorf("stdout must be empty on failure, got %q", result.stdout)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("failure wrote files: %v", entries)
			}
		})
	}
}

func runCLIWithStdin(t *testing.T, home string, env map[string]string, stdin string, args ...string) commandResult {
	t.Helper()

	cmd := exec.Command(cliBinary, args...)
	cmd.Env = isolatedEnv(home, env)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return commandResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func isolatedEnv(home string, overrides map[string]string) []string {
	blocked := map[string]bool{
		"HOME":                true,
		"GEMINI_API_KEY":      true,
		"GEMINI_ACCESS_TOKEN": true,
		"GEMINI_API_VERSION":  true,
		"GEMINI_USER_PROJECT": true,
		"CLAUDECODE":          true,
		"CLAUDE_CODE":         true,
		"CURSOR_AGENT":        true,
		"CODEX":               true,
		"AIDER":               true,
		"CLINE":               true,
		"WINDSURF_AGENT":      true,
		"GITHUB_COPILOT":      true,
		"AMAZON_Q":            true,
		"GEMINI_CODE_ASSIST":  true,
		"SRC_CODY":            true,
		"FORCE_AGENT_MODE":    true,
	}
	for key := range overrides {
		blocked[key] = true
	}

	env := make([]string, 0, len(os.Environ())+len(overrides)+1)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !blocked[key] && !strings.HasPrefix(key, "GEMINI_") {
			env = append(env, item)
		}
	}

	env = append(env, "HOME="+home)
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func baseArgs(serverURL string) []string {
	return []string{
		"--server-url", serverURL,
		"--api-version", "v1beta",
		"--no-interactive",
		"--no-retries",
		"--output-format", "json",
		"--color", "never",
	}
}

func parseDryRunRequest(t *testing.T, stdout string) map[string]any {
	t.Helper()
	line := strings.TrimSpace(stdout)
	if strings.Contains(line, "\n") {
		line = strings.Split(line, "\n")[0]
	}
	var preview map[string]any
	if err := json.Unmarshal([]byte(line), &preview); err != nil {
		t.Fatalf("dry-run preview is not JSON: %v\nstdout:\n%s", err, stdout)
	}
	if preview["dry_run"] != true {
		t.Fatalf("dry-run preview marker = %#v", preview["dry_run"])
	}
	request, ok := preview["request"].(map[string]any)
	if !ok {
		t.Fatalf("dry-run preview has no request object: %#v", preview)
	}
	return request
}

// parseHumanDryRunBody extracts the JSON body from a human-mode [DRY-RUN]
// block on stderr (between "[DRY-RUN] Body:" and "[DRY-RUN] Network call
// skipped."). JSON-mode previews use parseDryRunRequest on stdout instead.
func parseHumanDryRunBody(t *testing.T, stderr string) map[string]any {
	t.Helper()
	start := strings.Index(stderr, "[DRY-RUN] Body:")
	if start < 0 {
		t.Fatalf("dry-run output has no body marker:\n%s", stderr)
	}
	rest := stderr[start+len("[DRY-RUN] Body:"):]
	end := strings.Index(rest, "[DRY-RUN] Network call skipped.")
	if end < 0 {
		t.Fatalf("dry-run output has no network-skipped marker:\n%s", stderr)
	}
	bodyText := strings.TrimSpace(rest[:end])
	var body map[string]any
	if err := json.Unmarshal([]byte(bodyText), &body); err != nil {
		t.Fatalf("dry-run body is not JSON: %v\nbody:\n%s\nstderr:\n%s", err, bodyText, stderr)
	}
	return body
}

func parseDryRunBody(t *testing.T, stdout string) map[string]any {
	t.Helper()
	request := parseDryRunRequest(t, stdout)
	body, ok := request["body"].(map[string]any)
	if !ok {
		t.Fatalf("dry-run request body = %#v, want object", request["body"])
	}
	return body
}

func dryRunHeader(t *testing.T, request map[string]any, name string) string {
	t.Helper()
	headers, ok := request["headers"].(map[string]any)
	if !ok {
		t.Fatalf("dry-run headers = %#v", request["headers"])
	}
	values, ok := headers[name].([]any)
	if !ok {
		t.Fatalf("dry-run header %s = %#v", name, headers[name])
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprint(value))
	}
	return strings.Join(parts, ", ")
}

func newTextDeltaServer(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server does not support flushing")
			return
		}
		// EventStream wraps each SSE data value in the generated stream
		// envelope's Data field, producing $.data.delta.text for projection.
		for _, event := range []string{
			`{"event_type":"step.delta","delta":{"type":"text","text":"Hello "},"index":0}`,
			`{"event_type":"step.delta","delta":{"type":"text","text":"world"},"index":1}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func TestCommandInventory(t *testing.T) {
	result := runCLI(t, t.TempDir(), nil, "--usage")
	if result.err != nil {
		t.Fatalf("gemini-api --usage failed: %v\nstderr: %s", result.err, result.stderr)
	}

	expected := []string{
		`cmd "agent"`, `cmd "create"`, `cmd "delete"`, `cmd "get"`, `cmd "list"`,
		`cmd "run"`, `cmd "status"`, `cmd "cancel"`, `cmd "delete-interaction"`,
		`cmd "triggers"`, `cmd "list-executions"`, `cmd "update"`,
		`cmd "webhooks"`, `cmd "ping"`, `cmd "rotate-signing-secret"`,
		`cmd "models"`, `cmd "files"`, `cmd "register"`, `cmd "generated-files-list"`,
		`cmd "credentials"`,
	}
	for _, fragment := range expected {
		if !strings.Contains(result.stdout, fragment) {
			t.Errorf("usage schema is missing %q", fragment)
		}
	}

	operationCommands := []string{
		"agent run", "agent status", "agent delete-interaction", "agent cancel",
		"webhooks create", "webhooks list", "webhooks get", "webhooks update",
		"webhooks delete", "webhooks rotate-signing-secret", "webhooks ping",
		"agent create", "agent list", "agent get", "agent delete",
		// "triggers create" is descoped: default injection adds stream/store
		// into the embedded interaction template and the service rejects them.
		"triggers list", "triggers get", "triggers update",
		"triggers delete", "triggers run", "triggers list-executions",
		"models list", "models get",
		"files list", "files get", "files delete", "files register", "files generated-files-list",
		"credentials list", "credentials create", "credentials get", "credentials update", "credentials delete",
	}
	for _, command := range operationCommands {
		parts := append(strings.Fields(command), "--usage")
		result := runCLI(t, t.TempDir(), nil, parts...)
		if result.err != nil {
			t.Errorf("gemini-api %s --usage failed: %v\nstderr: %s", command, result.err, result.stderr)
		}
	}
}

func TestRequestConstruction(t *testing.T) {
	type capturedRequest struct {
		path    string
		query   string
		headers http.Header
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{path: r.URL.Path, query: r.URL.RawQuery, headers: r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"agents":[],"next_page_token":"next-token"}`)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL),
		"--header", "X-Contract-Test: request-construction",
		"agent", "list",
		"--page-size", "7",
		"--page-token", "current-token",
		"--parent", "projects/example",
	)
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("agent list failed: %v\nstderr: %s", result.err, result.stderr)
	}

	request := <-captured
	if request.path != "/v1beta/agents" {
		t.Errorf("request path = %q, want /v1beta/agents", request.path)
	}
	values := make(map[string][]string)
	for _, pair := range strings.Split(request.query, "&") {
		key, value, _ := strings.Cut(pair, "=")
		values[key] = append(values[key], value)
	}
	for key, want := range map[string]string{
		"page_size":  "7",
		"page_token": "current-token",
		"parent":     "projects%2Fexample",
	} {
		if got := strings.Join(values[key], ","); got != want {
			t.Errorf("query %s = %q, want %q (raw query %q)", key, got, want, request.query)
		}
	}
	if got := request.headers.Get("X-Contract-Test"); got != "request-construction" {
		t.Errorf("X-Contract-Test = %q", got)
	}

	var output map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &output); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, result.stdout)
	}
	// Pagination is active on list operations, but structured output keeps
	// stderr silent on success: the --all hint is human-mode only.
	if result.stderr != "" {
		t.Errorf("stderr = %q, want empty for successful JSON output", result.stderr)
	}
}

func TestAuthenticationHeadersAndPrecedence(t *testing.T) {
	tests := []struct {
		name         string
		configAPIKey string
		env          map[string]string
		args         []string
		wantAPIKey   string
		wantAuth     string
	}{
		{
			name:       "api key flag",
			args:       []string{"--api-key", "flag-api-key"},
			wantAPIKey: "flag-api-key",
		},
		{
			name:       "api key environment",
			env:        map[string]string{"GEMINI_API_KEY": "env-api-key"},
			wantAPIKey: "env-api-key",
		},
		{
			name:         "api key config",
			configAPIKey: "config-api-key",
			wantAPIKey:   "config-api-key",
		},
		{
			name:         "flag overrides environment and config",
			configAPIKey: "config-api-key",
			env:          map[string]string{"GEMINI_API_KEY": "env-api-key"},
			args:         []string{"--api-key", "flag-api-key"},
			wantAPIKey:   "flag-api-key",
		},
		{
			name:     "bearer token",
			args:     []string{"--access-token", "access-token"},
			wantAuth: "Bearer access-token",
		},
		{
			name:         "bearer token overrides API key",
			configAPIKey: "config-api-key",
			args:         []string{"--access-token", "access-token"},
			wantAuth:     "Bearer access-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers <- r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"agents":[]}`)
			}))
			defer server.Close()

			home := t.TempDir()
			if tt.configAPIKey != "" {
				configDir := filepath.Join(home, ".config", "gemini-api")
				if err := os.MkdirAll(configDir, 0o700); err != nil {
					t.Fatal(err)
				}
				config := "version: 1\nsecurity:\n  api_key: " + tt.configAPIKey + "\n"
				if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			args := append(baseArgs(server.URL), tt.args...)
			args = append(args, "agent", "list")
			result := runCLI(t, home, tt.env, args...)
			if result.err != nil {
				t.Fatalf("agent list failed: %v\nstderr: %s", result.err, result.stderr)
			}

			requestHeaders := <-headers
			if got := requestHeaders.Get("x-goog-api-key"); got != tt.wantAPIKey {
				t.Errorf("x-goog-api-key = %q, want %q", got, tt.wantAPIKey)
			}
			if got := requestHeaders.Get("Authorization"); got != tt.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tt.wantAuth)
			}
			for _, secret := range []string{"flag-api-key", "env-api-key", "config-api-key", "access-token"} {
				if strings.Contains(result.stdout, secret) || strings.Contains(result.stderr, secret) {
					t.Errorf("credential %q leaked in output\nstdout: %s\nstderr: %s", secret, result.stdout, result.stderr)
				}
			}
		})
	}
}

// TestHelpJourney pins the root → agent → run discovery path: each help
// screen must teach the next step, and run help must show one runnable JSON
// example per request-body union variant instead of a bare command.
func TestHelpJourney(t *testing.T) {
	root := runCLI(t, t.TempDir(), nil, "--help")
	if !strings.Contains(root.stdout, "gemini-api agent --help") {
		t.Errorf("root help does not point at the agent group:\n%s", root.stdout)
	}

	group := runCLI(t, t.TempDir(), nil, "agent", "--help")
	if !strings.Contains(group.stdout, "gemini-api agent run --help") {
		t.Errorf("agent group help does not point at run:\n%s", group.stdout)
	}

	run := runCLI(t, t.TempDir(), nil, "agent", "run", "--help")
	// Build Spec §9: the create-interaction union renders as mutually
	// exclusive --model / --agent flag sets; examples are runnable flag
	// invocations, never one JSON-string flag.
	for _, want := range []string{
		`--model gemini-3.8-flash`,
		`--agent `,
		`--background`,
		"Model variant Flags:",
		"Agent variant Flags:",
	} {
		if !strings.Contains(run.stdout, want) {
			t.Errorf("agent run help missing %q:\n%s", want, run.stdout)
		}
	}
	if strings.Contains(run.stdout, "Just works:\n  gemini-api agent run\n") {
		t.Errorf("agent run help still shows the bare example:\n%s", run.stdout)
	}
	if strings.Contains(run.stdout, "--body-param") {
		t.Errorf("agent run help still offers the union JSON flag:\n%s", run.stdout)
	}

	usage := runCLI(t, t.TempDir(), nil, "agent", "run", "--usage")
	if !strings.Contains(usage.stdout, `--model <model>`) ||
		!strings.Contains(usage.stdout, `--agent <agent>`) {
		t.Errorf("agent run --usage does not expose both variant flag sets:\n%s", usage.stdout)
	}
	if strings.Contains(usage.stdout, "body-param") {
		t.Errorf("agent run --usage still lists the union JSON flag:\n%s", usage.stdout)
	}
}

// TestModelsCatalogFromEnum pins the x-speakeasy-cli-catalog rendering: the
// models command lists the schema's model enum with descriptions and marks
// the schema default.
func TestModelsCatalogFromEnum(t *testing.T) {
	result := runCLI(t, t.TempDir(), nil, "models")
	if result.err != nil {
		t.Fatalf("models failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stdout, "gemini-3.8-flash (default)") {
		t.Errorf("models does not mark the default model:\n%s", result.stdout)
	}
	if !strings.Contains(result.stdout, "gemini-2.5-pro") {
		t.Errorf("models does not list enum values:\n%s", result.stdout)
	}
}

// TestGenerateIntentCommand pins the OAD-declared tier-1 surface: the
// "generate" intent (x-speakeasy-cli-commands) maps a positional prompt onto
// a CreateInteraction body with the schema's default model, and planned
// placeholder commands appear in the tree but fail with an actionable note.
func TestGenerateIntentCommand(t *testing.T) {
	type capturedRequest struct {
		body   []byte
		accept string
	}
	requests := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requests <- capturedRequest{body: b, accept: r.Header.Get("Accept")}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"int-1","status":"completed"}`)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "generate", "Explain concurrency in one sentence")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("generate failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var sent struct {
		Model  string `json:"model"`
		Input  string `json:"input"`
		Stream bool   `json:"stream"`
	}
	request := <-requests
	if err := json.Unmarshal(request.body, &sent); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if sent.Model != "gemini-3.8-flash" {
		t.Errorf("model = %q, want schema default gemini-3.8-flash", sent.Model)
	}
	if sent.Input != "Explain concurrency in one sentence" {
		t.Errorf("input = %q, want positional prompt", sent.Input)
	}
	if !sent.Stream {
		t.Error("stream = false, want the generate intent to stream by default")
	}
	if request.accept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", request.accept)
	}

	// Root help shows the declared tier-1 sections and line-up
	root := runCLI(t, t.TempDir(), nil, "--help")
	for _, want := range []string{"Create:", "Understand:", "Manage:", "Advanced:", "generate", "image", "tts", "tokens", "docs"} {
		if !strings.Contains(root.stdout, want) {
			t.Errorf("root help missing declared surface %q:\n%s", want, root.stdout)
		}
	}

	// Planned placeholders still in the tree fail with the declared note.
	// ("docs" and "tokens" remain planned; tts/analyze/transcribe are claimed
	// by the custom porcelain — covered in porcelain_test.go; embed and batch
	// are cut from this interactions-only build.)
	planned := runCLI(t, t.TempDir(), nil, "docs")
	if planned.err == nil {
		t.Error("planned placeholder command unexpectedly succeeded")
	}
	if !strings.Contains(planned.stderr, "not part of this build") {
		t.Errorf("placeholder error lacks the declared note:\n%s", planned.stderr)
	}
}

func TestGenerateDryRunStreamingDefaults(t *testing.T) {
	tests := []struct {
		name       string
		command    []string
		wantStream bool
		wantAccept string
	}{
		{
			name:       "default streams",
			command:    []string{"generate", "hi"},
			wantStream: true,
			wantAccept: "    Accept: text/event-stream\n",
		},
		{
			name:       "explicit false returns one result",
			command:    []string{"generate", "hi", "--stream=false"},
			wantStream: false,
			wantAccept: "    Accept: application/json;q=1, text/event-stream;q=0\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(baseArgs("https://example.invalid"), "--dry-run")
			args = append(args, tt.command...)
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("generate --dry-run failed: %v\nstderr: %s", result.err, result.stderr)
			}
			request := parseDryRunRequest(t, result.stdout)
			body, ok := request["body"].(map[string]any)
			if !ok {
				t.Fatalf("dry-run body = %#v, want object", request["body"])
			}
			stream, ok := body["stream"].(bool)
			if !ok {
				t.Fatalf("dry-run stream = %#v, want a boolean", body["stream"])
			}
			if stream != tt.wantStream {
				t.Errorf("dry-run stream = %t, want %t", stream, tt.wantStream)
			}
			if got := dryRunHeader(t, request, "Accept"); got != strings.TrimSpace(strings.TrimPrefix(tt.wantAccept, "    Accept:")) {
				t.Errorf("dry-run Accept = %q, want %q", got, strings.TrimSpace(strings.TrimPrefix(tt.wantAccept, "    Accept:")))
			}
			if result.stderr != "" {
				t.Errorf("JSON dry-run wrote stderr: %q", result.stderr)
			}
		})
	}
}

func TestGenerateDryRunInlineBodyPreset(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStream bool
		wantAccept string
	}{
		{
			name:       "missing stream receives preset",
			body:       `{"model":"gemini-3.6-flash","input":"hi"}`,
			wantStream: true,
			wantAccept: "    Accept: text/event-stream\n",
		},
		{
			name:       "explicit false wins over preset",
			body:       `{"model":"gemini-3.6-flash","input":"hi","stream":false}`,
			wantStream: false,
			wantAccept: "    Accept: application/json;q=1, text/event-stream;q=0\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(baseArgs("https://example.invalid"), "--dry-run", "generate", "--body", tt.body)
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("generate --body --dry-run failed: %v\nstderr: %s", result.err, result.stderr)
			}
			request := parseDryRunRequest(t, result.stdout)
			body, ok := request["body"].(map[string]any)
			if !ok {
				t.Fatalf("dry-run body = %#v, want object", request["body"])
			}
			stream, ok := body["stream"].(bool)
			if !ok {
				t.Fatalf("dry-run stream = %#v, want a boolean", body["stream"])
			}
			if stream != tt.wantStream {
				t.Errorf("dry-run stream = %t, want %t", stream, tt.wantStream)
			}
			if got := dryRunHeader(t, request, "Accept"); got != strings.TrimSpace(strings.TrimPrefix(tt.wantAccept, "    Accept:")) {
				t.Errorf("dry-run Accept = %q, want %q", got, strings.TrimSpace(strings.TrimPrefix(tt.wantAccept, "    Accept:")))
			}
			if result.stderr != "" {
				t.Errorf("JSON dry-run wrote stderr: %q", result.stderr)
			}
		})
	}
}

func TestGenerateDefaultStreamProjection(t *testing.T) {
	tests := []struct {
		name      string
		extraArgs []string
	}{
		{name: "normal mode"},
		{name: "agent mode", extraArgs: []string{"--agent-mode"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTextDeltaServer(t)
			defer server.Close()

			args := []string{
				"--server-url", server.URL,
				"--api-version", "v1beta",
				"--no-interactive",
				"--no-retries",
				"--color", "never",
				"generate", "x",
			}
			args = append(args, tt.extraArgs...)
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("generate stream failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if result.stdout != "Hello world\n" {
				t.Errorf("stdout = %q, want raw projected text", result.stdout)
			}
		})
	}
}

func TestGenerateExplicitJSONKeepsNDJSONEvents(t *testing.T) {
	server := newTextDeltaServer(t)
	defer server.Close()

	args := append(baseArgs(server.URL), "generate", "x")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("generate -o json failed: %v\nstderr: %s", result.err, result.stderr)
	}

	var texts []string
	scanner := bufio.NewScanner(strings.NewReader(result.stdout))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("NDJSON line is not JSON: %v\nline: %s", err, scanner.Text())
		}
		data, ok := event["data"].(map[string]any)
		if !ok {
			t.Fatalf("NDJSON event data = %#v, want an object", event["data"])
		}
		if got := data["event_type"]; got != "step.delta" {
			t.Errorf("event_type = %#v, want step.delta", got)
		}
		delta, ok := data["delta"].(map[string]any)
		if !ok {
			t.Fatalf("NDJSON event delta = %#v, want an object", data["delta"])
		}
		text, ok := delta["text"].(string)
		if !ok {
			t.Fatalf("NDJSON delta text = %#v, want a string", delta["text"])
		}
		texts = append(texts, text)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(texts) != 2 || texts[0] != "Hello " || texts[1] != "world" {
		t.Errorf("NDJSON texts = %#v, want two unprojected delta events", texts)
	}
}

func TestJQRawOutputDefaultsTrue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"int-1","status":"completed"}`)
	}))
	defer server.Close()

	tests := []struct {
		name       string
		quotedJSON bool
		want       string
	}{
		{name: "default raw", want: "int-1\n"},
		{name: "explicit JSON quoting", quotedJSON: true, want: "\"int-1\"\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(baseArgs(server.URL), "--jq", ".id")
			if tt.quotedJSON {
				args = append(args, "--raw-output=false")
			}
			args = append(args, "generate", "x", "--stream=false")
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("generate --jq failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if result.stdout != tt.want {
				t.Errorf("stdout = %q, want %q", result.stdout, tt.want)
			}
		})
	}
}

// TestBodySchemaFlag pins the --schema surface: body-bearing commands print
// the exact, self-contained JSON Schema of their request body.
func TestBodySchemaFlag(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "run", "--schema"},
		{"generate", "--schema"},
	} {
		result := runCLI(t, t.TempDir(), nil, args...)
		if result.err != nil {
			t.Fatalf("%v failed: %v\nstderr: %s", args, result.err, result.stderr)
		}
		var schema struct {
			Defs  map[string]json.RawMessage `json:"$defs"`
			OneOf []json.RawMessage          `json:"oneOf"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &schema); err != nil {
			t.Fatalf("%v output is not JSON: %v", args, err)
		}
		if len(schema.OneOf) != 2 {
			t.Errorf("%v: oneOf variants = %d, want 2", args, len(schema.OneOf))
		}
		if len(schema.Defs) == 0 {
			t.Errorf("%v: schema has no bundled $defs", args)
		}
	}
}

// TestReasonFirstErrorClassification pins spec A8: an HTTP 400 carrying a
// google.rpc ErrorInfo reason of API_KEY_INVALID is an authentication error,
// not a validation error.
func TestReasonFirstErrorClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com"}]}}`)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "--agent-mode", "agent", "list")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatal("command succeeded for HTTP 400 API_KEY_INVALID")
	}
	var envelope struct {
		ErrorType   string   `json:"error_type"`
		ErrorReason string   `json:"error_reason"`
		Hints       []string `json:"hints"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
		t.Fatalf("stderr is not one JSON document: %v\n%s", err, result.stderr)
	}
	if envelope.ErrorType != "authentication_error" {
		t.Errorf("error_type = %q, want authentication_error (status-only classification mislabels this)", envelope.ErrorType)
	}
	if envelope.ErrorReason != "API_KEY_INVALID" {
		t.Errorf("error_reason = %q, want API_KEY_INVALID", envelope.ErrorReason)
	}
	if len(envelope.Hints) == 0 || !strings.Contains(strings.Join(envelope.Hints, " "), "GEMINI_API_KEY") {
		t.Errorf("hints do not steer to credentials: %v", envelope.Hints)
	}
}

// TestDefaultModelInjectedWhenNoSelector pins the zero-config run: a body
// naming neither "model" nor "agent" is sent with the schema's default model.
func TestDefaultModelInjectedWhenNoSelector(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"int-1","status":"completed"}`)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "agent", "run",
		"--body", `{"input":"hello"}`)
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("agent run failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var sent struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	if err := json.Unmarshal(<-bodies, &sent); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if sent.Model != "gemini-3.8-flash" {
		t.Errorf("model = %q, want default gemini-3.8-flash injected", sent.Model)
	}
	if sent.Input != "hello" {
		t.Errorf("input = %q, want hello preserved", sent.Input)
	}
}

func TestNoRetriesMakesOneRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"code":500,"message":"synthetic failure"}}`)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "agent", "list")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatal("agent list succeeded for HTTP 500")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("request count = %d, want 1 with --no-retries", got)
	}
}

func TestUserProjectHeader(t *testing.T) {
	headers := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"agents":[]}`)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "--user-project", "billing-project", "agent", "list")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("agent list failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if got := (<-headers).Get("x-goog-user-project"); got != "billing-project" {
		t.Errorf("x-goog-user-project = %q, want billing-project", got)
	}
}

func TestDebugRedactsGeminiAPIKey(t *testing.T) {
	const secret = "contract-debug-secret"
	tests := []struct {
		name string
		args []string
	}{
		{name: "generated auth header", args: []string{"--api-key", secret}},
		{name: "custom header", args: []string{"--header", "x-goog-api-key: " + secret}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers <- r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"agents":[]}`)
			}))
			defer server.Close()

			args := append(baseArgs(server.URL), "--debug")
			args = append(args, tt.args...)
			args = append(args, "agent", "list")
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("agent list failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if got := (<-headers).Get("x-goog-api-key"); got != secret {
				t.Errorf("x-goog-api-key = %q, want secret at server", got)
			}
			if strings.Contains(result.stdout, secret) || strings.Contains(result.stderr, secret) {
				t.Fatalf("API key leaked\nstdout: %s\nstderr: %s", result.stdout, result.stderr)
			}
			if !strings.Contains(result.stderr, "[REDACTED]") {
				t.Errorf("debug output did not show a redaction marker:\n%s", result.stderr)
			}
		})
	}
}

// TestAPIVersionDefaultsToV1Beta pins the zero-config path: with no
// --api-version flag, env var, or config entry, requests go to /v1beta.
func TestAPIVersionDefaultsToV1Beta(t *testing.T) {
	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"agents":[]}`)
	}))
	defer server.Close()

	args := []string{
		"--server-url", server.URL,
		"--no-interactive",
		"--no-retries",
		"--output-format", "json",
		"--color", "never",
		"agent", "list",
	}
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("agent list failed without an explicit API version: %v\nstderr: %s", result.err, result.stderr)
	}
	if got := <-paths; !strings.HasPrefix(got, "/v1beta/") {
		t.Errorf("request path = %q, want /v1beta/ prefix from the schema default", got)
	}
}

func TestInvalidAPIVersionFailsBeforeDispatch(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
	}{
		{name: "contains slash", apiVersion: "v1beta/extra"},
		{name: "contains whitespace", apiVersion: " v1beta"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"agents":[]}`)
			}))
			defer server.Close()

			args := []string{
				"--server-url", server.URL,
				"--no-interactive",
				"--no-retries",
				"--output-format", "json",
				"--color", "never",
			}
			if tt.apiVersion != "" {
				args = append(args, "--api-version", tt.apiVersion)
			}
			args = append(args, "agent", "list")
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err == nil {
				t.Fatalf("command succeeded with API version %q", tt.apiVersion)
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("dispatched %d requests with invalid API version %q", got, tt.apiVersion)
			}
			if !strings.Contains(strings.ToLower(result.stderr), "api version") {
				t.Errorf("stderr does not explain API version failure:\n%s", result.stderr)
			}
		})
	}
}

func TestRedirectResponseIsNotSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusFound)
		fmt.Fprint(w, `{"agents":[]}`)
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "--agent-mode", "agent", "list")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatalf("agent list succeeded for HTTP 302\nstdout: %s", result.stdout)
	}
	if result.stdout != "" {
		t.Errorf("redirect failure wrote stdout: %s", result.stdout)
	}
}

func TestCreateInteractionRequestsSSE(t *testing.T) {
	accept := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept <- r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"event_type\":\"future.event\",\"payload\":\"first\"}\n\n")
	}))
	defer server.Close()

	args := append(baseArgs(server.URL), "agent", "run", "--body", `{"model":"gemini-test","input":"hello","stream":true}`)
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("streaming create failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if got := <-accept; !strings.Contains(got, "text/event-stream") || strings.Contains(got, "q=0") {
		t.Errorf("Accept = %q, want acceptable text/event-stream", got)
	}
}

func TestSingletonArrayInteractionError(t *testing.T) {
	const responseBody = `[{"error":{"code":403,"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED","details":[{"reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}]`

	for _, noRetries := range []bool{true, false} {
		name := "default retry configuration"
		if noRetries {
			name = "retries disabled"
		}
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, responseBody)
			}))
			defer server.Close()

			args := []string{
				"--server-url", server.URL,
				"--api-version", "v1beta",
				"--no-interactive",
				"--output-format", "json",
				"--color", "never",
				"--agent-mode",
			}
			if noRetries {
				args = append(args, "--no-retries")
			}
			args = append(args,
				"agent", "run",
				"--body", `{"model":"gemini-test","input":"hello","stream":true}`,
			)

			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err == nil {
				t.Fatalf("interaction create succeeded for HTTP 403\nstdout: %s", result.stdout)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("request count = %d, want 1", got)
			}
			if result.stdout != "" {
				t.Errorf("failure wrote stdout: %s", result.stdout)
			}
			if strings.Contains(result.stderr, "cannot unmarshal array") {
				t.Fatalf("generated decoder still received the array-shaped body:\n%s", result.stderr)
			}

			// The array wrapper is unwrapped before classification: the
			// envelope is the inner error object, enriched reason-first
			// (ACCESS_TOKEN_SCOPE_INSUFFICIENT is a credential failure even
			// though the HTTP status is 403).
			var envelope struct {
				ErrorType   string          `json:"error_type"`
				ErrorReason string          `json:"error_reason"`
				StatusCode  int             `json:"status_code"`
				Error       json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
				t.Fatalf("stderr is not one JSON document: %v\n%s", err, result.stderr)
			}
			// ACCESS_TOKEN_SCOPE_INSUFFICIENT is a permissions problem with a
			// valid credential, not a missing/invalid one: authorization_error
			// (reason-first still wins over a bare status classification).
			if envelope.ErrorType != "authorization_error" {
				t.Errorf("error_type = %q, want authorization_error (reason-first over the 403 status)", envelope.ErrorType)
			}
			if envelope.ErrorReason != "ACCESS_TOKEN_SCOPE_INSUFFICIENT" {
				t.Errorf("error_reason = %q, want ACCESS_TOKEN_SCOPE_INSUFFICIENT", envelope.ErrorReason)
			}
			if envelope.StatusCode != http.StatusForbidden {
				t.Errorf("status_code = %d, want 403", envelope.StatusCode)
			}
			var compactError bytes.Buffer
			if len(envelope.Error) == 0 || json.Compact(&compactError, envelope.Error) != nil || compactError.String() != `{"code":403,"details":[{"reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}],"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED"}` {
				t.Errorf("error = %s, want the unwrapped inner error object", envelope.Error)
			}
		})
	}
}

func TestRetryableSingletonArrayInteractionError(t *testing.T) {
	const responseBody = `[{"error":{"code":503,"message":"temporary interaction failure","status":"UNAVAILABLE"}}]`
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After-Ms", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, responseBody)
	}))
	defer server.Close()

	args := []string{
		"--server-url", server.URL,
		"--api-version", "v1beta",
		"--no-interactive",
		"--output-format", "json",
		"--color", "never",
		"--agent-mode",
		"--retry-config", `{"Strategy":"attempt-count-backoff","Backoff":{"InitialInterval":1,"MaxInterval":1,"Exponent":1,"MaxElapsedTime":1000},"MaxRetries":1}`,
		"agent", "run",
		"--body", `{"model":"gemini-test","input":"hello","stream":true}`,
	}
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatalf("interaction create succeeded for HTTP 503\nstdout: %s", result.stdout)
	}
	// Mutating calls are billed and never retried — even an explicit
	// --retry-config must not replay a POST.
	if got := requests.Load(); got != 1 {
		t.Errorf("request count = %d, want 1", got)
	}
	if strings.Contains(result.stderr, "cannot unmarshal array") {
		t.Fatalf("generated decoder still received the array-shaped body:\n%s", result.stderr)
	}

	// The array wrapper is unwrapped before classification: the envelope is
	// the inner error object with the RPC status surfaced as the reason.
	var envelope struct {
		ErrorType   string          `json:"error_type"`
		ErrorReason string          `json:"error_reason"`
		StatusCode  int             `json:"status_code"`
		Error       json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
		t.Fatalf("stderr is not one JSON document: %v\n%s", err, result.stderr)
	}
	if envelope.ErrorType != "server_error" {
		t.Errorf("error_type = %q, want server_error", envelope.ErrorType)
	}
	if envelope.ErrorReason != "UNAVAILABLE" {
		t.Errorf("error_reason = %q, want UNAVAILABLE", envelope.ErrorReason)
	}
	if envelope.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status_code = %d, want 503", envelope.StatusCode)
	}
	if len(envelope.Error) == 0 {
		t.Errorf("error missing, want the unwrapped inner error object:\n%s", result.stderr)
	}
}

func TestDefaultRetryBehavior(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After-Ms", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"code":"https://errors.example/503","message":"retry characterization"}}`)
	}))
	defer server.Close()

	args := []string{
		"--server-url", server.URL,
		"--api-version", "v1beta",
		"--no-interactive",
		"--output-format", "json",
		"--color", "never",
		"agent", "list",
	}
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatal("agent list succeeded for repeated HTTP 503")
	}
	if got := requests.Load(); got != 5 {
		t.Errorf("default request count = %d, want 5 (initial request plus four retries)", got)
	}
	var envelope struct {
		StatusCode int `json:"status_code"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
		t.Fatalf("final retry error is not one JSON document: %v\n%s", err, result.stderr)
	}
	if envelope.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status_code = %d, want 503", envelope.StatusCode)
	}
}

func TestMutatingVerbsNeverRetry(t *testing.T) {
	tests := []struct {
		name   string
		method string
		args   []string
	}{
		{
			name:   "delete",
			method: http.MethodDelete,
			args:   []string{"agent", "delete", "--id", "agent-test"},
		},
		{
			name:   "patch",
			method: http.MethodPatch,
			args:   []string{"triggers", "update", "--id", "trigger-test", "--body", `{}`},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != test.method {
					t.Errorf("request method = %s, want %s", r.Method, test.method)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"error":{"code":"https://errors.example/503","message":"mutating retry guard"}}`)
			}))
			defer server.Close()

			args := []string{
				"--server-url", server.URL,
				"--api-version", "v1beta",
				"--api-key", "test",
				"--no-interactive",
				"--output-format", "json",
				"--color", "never",
			}
			args = append(args, test.args...)
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err == nil {
				t.Fatalf("mutating command succeeded for HTTP 503\nstdout: %s", result.stdout)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("request count = %d, want 1", got)
			}
		})
	}
}

func TestGetRetryHonorsRetryAfter(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if request <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"https://errors.example/429","message":"rate limited"}}`)
			return
		}
		fmt.Fprint(w, `{"agents":[]}`)
	}))
	defer server.Close()

	args := []string{
		"--server-url", server.URL,
		"--api-version", "v1beta",
		"--api-key", "test",
		"--no-interactive",
		"--output-format", "json",
		"--color", "never",
		"agent", "list",
	}
	start := time.Now()
	result := runCLI(t, t.TempDir(), nil, args...)
	elapsed := time.Since(start)
	if result.err != nil {
		t.Fatalf("agent list failed after retrying HTTP 429: %v\nstderr: %s", result.err, result.stderr)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("request count = %d, want 3", got)
	}
	if elapsed < 2*time.Second {
		t.Errorf("elapsed time = %s, want at least 2s", elapsed)
	}
	if elapsed >= 8*time.Second {
		t.Errorf("elapsed time = %s, want less than 8s", elapsed)
	}
}

func TestGetRetryBoundedByMaxRetries(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After-Ms", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"https://errors.example/429","message":"rate limited"}}`)
	}))
	defer server.Close()

	args := []string{
		"--server-url", server.URL,
		"--api-version", "v1beta",
		"--api-key", "test",
		"--no-interactive",
		"--output-format", "json",
		"--color", "never",
		"agent", "list",
	}
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatal("agent list succeeded for repeated HTTP 429")
	}
	if got := requests.Load(); got != 5 {
		t.Errorf("request count = %d, want 5 (initial request plus four retries)", got)
	}
	var envelope struct {
		StatusCode int `json:"status_code"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
		t.Fatalf("final retry error is not one JSON document: %v\n%s", err, result.stderr)
	}
	if envelope.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status_code = %d, want 429", envelope.StatusCode)
	}
}

func TestSSEIncrementalOutput(t *testing.T) {
	for _, debug := range []bool{false, true} {
		name := "without debug"
		if debug {
			name = "with debug"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			accept := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				accept <- r.Header.Get("Accept")
				w.Header().Set("Content-Type", "text/event-stream")
				flusher, ok := w.(http.Flusher)
				if !ok {
					t.Error("test server does not support flushing")
					return
				}
				fmt.Fprint(w, "data: {\"event_type\":\"future.event\",\"payload\":\"first\"}\n\n")
				flusher.Flush()
				<-release
			}))
			defer server.Close()

			args := append(baseArgs(server.URL), "agent", "status", "--id", "interaction-1", "--stream")
			if debug {
				args = append([]string{"--debug"}, args...)
			}
			cmd := exec.Command(cliBinary, args...)
			cmd.Env = isolatedEnv(t.TempDir(), nil)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}

			if got := <-accept; !strings.Contains(got, "text/event-stream") || strings.Contains(got, "q=0") {
				t.Errorf("Accept = %q, want acceptable text/event-stream", got)
			}
			line := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				if scanner.Scan() {
					line <- scanner.Text()
					return
				}
				line <- ""
			}()

			select {
			case got := <-line:
				if !strings.Contains(got, `"event_type":"future.event"`) {
					t.Errorf("first stream line = %q", got)
				}
			case <-time.After(2 * time.Second):
				close(release)
				_ = cmd.Wait()
				t.Fatalf("first SSE event was not emitted before the response closed; stderr: %s", stderr.String())
			}
			close(release)
			if err := cmd.Wait(); err != nil {
				t.Fatalf("stream command failed: %v\nstderr: %s", err, stderr.String())
			}
		})
	}
}

// TestAgentEnvironmentDoesNotEnableAgentMode: this CLI does not identify its
// caller from environment variables (agentEnvironmentDetection: false), so a
// known-agent variable never switches on the agent-mode envelope, and an
// explicit --agent-mode=false is simply the default.
func TestAgentEnvironmentDoesNotEnableAgentMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"https://errors.example/401","message":"synthetic auth failure"}}`)
	}))
	defer server.Close()

	// classifiedErrors is enabled, so an explicit --output-format json request
	// is classified with or without agent mode; the proof that the environment
	// did not switch agent mode on is the human rendering: a plain-text error
	// on stderr rather than the agent-mode JSON envelope.
	for _, envVar := range []string{"CLAUDECODE", "CURSOR_AGENT", "CODEX", "FORCE_AGENT_MODE"} {
		t.Run(envVar, func(t *testing.T) {
			args := append(humanArgs(server.URL), "agent", "list")
			result := runCLI(t, t.TempDir(), map[string]string{envVar: "1"}, args...)
			if result.err == nil {
				t.Fatal("agent list succeeded for HTTP 401")
			}
			var envelope map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(result.stderr)), &envelope); err == nil {
				t.Errorf("stderr is an agent-mode JSON envelope; %s=1 must not enable agent mode:\n%s", envVar, result.stderr)
			}
			if !strings.Contains(result.stderr, "synthetic auth failure") {
				t.Errorf("stderr should carry the plain API error; got:\n%s", result.stderr)
			}
		})
	}
}

// TestExplicitAgentModeFalseOverridesEnvironment pins that agent mode is
// explicit-only and that the flag decides the rendering: every mode
// classifies the same error the same way (one taxonomy), so the observable
// difference is that --agent-mode forces the JSON envelope even in pretty
// mode, while --agent-mode=false (with a known-agent variable set, which this
// build ignores) leaves the human diagnostic.
func TestExplicitAgentModeFalseOverridesEnvironment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"https://errors.example/401","message":"synthetic auth failure"}}`)
	}))
	defer server.Close()

	pretty := []string{
		"--server-url", server.URL,
		"--api-version", "v1beta",
		"--no-interactive",
		"--no-retries",
		"--color", "never",
	}

	explicit := runCLI(t, t.TempDir(), nil, append(pretty, "--agent-mode", "agent", "list")...)
	if explicit.err == nil {
		t.Fatal("agent list succeeded for HTTP 401")
	}
	var envelope struct {
		ErrorType  string `json:"error_type"`
		StatusCode int    `json:"status_code"`
	}
	if err := json.Unmarshal([]byte(explicit.stderr), &envelope); err != nil {
		t.Fatalf("explicit --agent-mode must render the JSON envelope: %v\n%s", err, explicit.stderr)
	}
	if envelope.ErrorType != "authentication_error" || envelope.StatusCode != http.StatusUnauthorized {
		t.Errorf("envelope = %+v, want authentication_error / 401", envelope)
	}

	disabled := runCLI(t, t.TempDir(), map[string]string{"FORCE_AGENT_MODE": "1"}, append(pretty, "--agent-mode=false", "agent", "list")...)
	if disabled.err == nil {
		t.Fatal("agent list succeeded for HTTP 401")
	}
	if json.Valid([]byte(strings.TrimSpace(disabled.stderr))) {
		t.Errorf("--agent-mode=false must render the human diagnostic, got JSON:\n%s", disabled.stderr)
	}
	if !strings.HasPrefix(disabled.stderr, "Error (authentication_error): synthetic auth failure") {
		t.Errorf("stderr = %q, want the classified human diagnostic", disabled.stderr)
	}
}

func TestStructuredHTTPFailures(t *testing.T) {
	resources := []struct {
		name string
		args []string
	}{
		{name: "agent", args: []string{"agent", "list"}},
		{name: "agent-status", args: []string{"agent", "status", "--id", "interaction-1"}},
		{name: "triggers", args: []string{"triggers", "list"}},
		{name: "webhooks", args: []string{"webhooks", "list"}},
	}
	statuses := []struct {
		code      int
		errorType string
	}{
		{code: http.StatusUnauthorized, errorType: "authentication_error"},
		{code: http.StatusNotFound, errorType: "not_found"},
		{code: http.StatusTooManyRequests, errorType: "rate_limit_error"},
		{code: http.StatusInternalServerError, errorType: "server_error"},
	}

	for _, resource := range resources {
		for _, status := range statuses {
			name := resource.name + "/" + strconv.Itoa(status.code)
			t.Run(name, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status.code)
					fmt.Fprintf(w, `{"error":{"code":"https://errors.example/%d","message":"synthetic %s failure"}}`, status.code, resource.name)
				}))
				defer server.Close()

				args := append(baseArgs(server.URL), "--agent-mode")
				args = append(args, resource.args...)
				result := runCLI(t, t.TempDir(), nil, args...)
				if result.err == nil {
					t.Fatalf("command succeeded for HTTP %d\nstdout: %s\nstderr: %s", status.code, result.stdout, result.stderr)
				}
				if got := requests.Load(); got != 1 {
					t.Errorf("request count = %d, want 1", got)
				}
				if result.stdout != "" {
					t.Errorf("failure wrote stdout: %s", result.stdout)
				}
				var envelope struct {
					ErrorType  string `json:"error_type"`
					StatusCode int    `json:"status_code"`
				}
				if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
					t.Fatalf("stderr is not one JSON document: %v\n%s", err, result.stderr)
				}
				if envelope.ErrorType != status.errorType {
					t.Errorf("error_type = %q, want %q", envelope.ErrorType, status.errorType)
				}
				if envelope.StatusCode != status.code {
					t.Errorf("status_code = %d, want %d", envelope.StatusCode, status.code)
				}
			})
		}
	}
}
