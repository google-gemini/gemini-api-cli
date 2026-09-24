package contract_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// asyncVideoServer simulates a background video interaction: the create call
// answers in_progress with an id, and the poll endpoint walks through the
// scripted statuses (one per GET, the last one repeating). It records the
// method+path of every request so tests can pin the create-once/poll-only
// contract.
type asyncVideoServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	polls    []string // JSON bodies served by successive GETs
}

func newAsyncVideoServer(t *testing.T, createBody string, polls ...string) *asyncVideoServer {
	t.Helper()
	s := &asyncVideoServer{polls: polls}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		n := len(s.requests)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/interactions"):
			fmt.Fprint(w, createBody)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/interactions/"):
			if got := r.URL.Query().Get("stream"); got != "false" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":{"message":"poll must pin stream=false, got %q"}}`, got)
				return
			}
			idx := n - 2 // requests after the create
			if idx >= len(s.polls) {
				idx = len(s.polls) - 1
			}
			if idx < 0 {
				idx = 0
			}
			fmt.Fprint(w, s.polls[idx])
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"unexpected request"}}`)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *asyncVideoServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

const (
	asyncCreateInProgress = `{"id":"int-vid","object":"interaction","status":"in_progress","model":"gemini-omni-flash-preview"}`
	asyncPollInProgress   = `{"id":"int-vid","object":"interaction","status":"in_progress","model":"gemini-omni-flash-preview"}`
	// "video-bytes" base64-encoded.
	asyncPollCompleted = `{"id":"int-vid","object":"interaction","status":"completed","model":"gemini-omni-flash-preview",` +
		`"steps":[{"type":"model_output","content":[{"type":"video","data":"dmlkZW8tYnl0ZXM=","mime_type":"video/mp4"}]}]}`
	asyncPollFailed = `{"id":"int-vid","object":"interaction","status":"failed","model":"gemini-omni-flash-preview",` +
		`"error":{"message":"generation rejected"}}`
	asyncPollRequiresAction = `{"id":"int-vid","object":"interaction","status":"requires_action","model":"gemini-omni-flash-preview",` +
		`"steps":[{"type":"model_output","content":[{"type":"function_call","name":"lookup","arguments":{"q":"x"},"id":"call-1"}]}]}`
)

// videoArgs runs "video" against the server with fast polling so the suite
// stays quick; extra flags are appended.
func videoArgs(serverURL string, extra ...string) []string {
	args := []string{"--server-url", serverURL, "--api-version", "v1beta", "--no-interactive", "--no-retries", "--color", "never", "--api-key", "test",
		"video", "a timelapse of a city at night", "--poll-interval", "20ms"}
	return append(args, extra...)
}

func countPrefix(list []string, prefix string) int {
	n := 0
	for _, item := range list {
		if strings.HasPrefix(item, prefix) {
			n++
		}
	}
	return n
}

// TestVideoPollsToArtifact pins spec §8 for the media intent: the create call
// happens exactly once, the CLI polls "agent status" until the interaction
// completes, writes the video, verifies it on disk, and prints only the path.
func TestVideoPollsToArtifact(t *testing.T) {
	server := newAsyncVideoServer(t, asyncCreateInProgress, asyncPollInProgress, asyncPollInProgress, asyncPollCompleted)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	result := runCLIInDir(t, t.TempDir(), dir, nil, videoArgs(server.URL)...)
	if result.err != nil {
		t.Fatalf("video failed: %v\nstderr: %s", result.err, result.stderr)
	}
	printed := strings.TrimSpace(result.stdout)
	if strings.Contains(printed, "\n") || !filepath.IsAbs(printed) {
		t.Fatalf("stdout must be exactly one absolute path, got %q", result.stdout)
	}
	if filepath.Dir(printed) != dir || !strings.HasPrefix(filepath.Base(printed), "gemini-video-") || filepath.Ext(printed) != ".mp4" {
		t.Errorf("default path = %q, want ./gemini-video-*.mp4 inside %s", printed, dir)
	}
	if data, err := os.ReadFile(printed); err != nil || string(data) != "video-bytes" {
		t.Errorf("written file %q: err=%v content=%q", printed, err, data)
	}
	seen := server.seen()
	if countPrefix(seen, "POST ") != 1 {
		t.Errorf("create must be sent exactly once, requests: %v", seen)
	}
	if countPrefix(seen, "GET /v1beta/interactions/int-vid") != 3 {
		t.Errorf("expected three polls (in_progress, in_progress, completed), requests: %v", seen)
	}
	if !strings.Contains(result.stderr, "int-vid") {
		t.Errorf("human progress should name the interaction on stderr:\n%s", result.stderr)
	}

	// Structured mode: the artifact envelope on stdout, and NOTHING on stderr
	// (spec §7: stderr is silent on success under --json).
	server2 := newAsyncVideoServer(t, asyncCreateInProgress, asyncPollInProgress, asyncPollCompleted)
	target := filepath.Join(dir, "clip.mp4")
	result = runCLIInDir(t, t.TempDir(), dir, nil, videoArgs(server2.URL, "--out", target, "--output-format", "json")...)
	if result.err != nil {
		t.Fatalf("video -o json failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var envelope struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
		Size int    `json:"size_bytes"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
		t.Fatalf("json output is not the artifact envelope: %v\n%s", err, result.stdout)
	}
	if envelope.Path != target || envelope.Kind != "video" || envelope.Size != len("video-bytes") {
		t.Errorf("artifact envelope = %+v", envelope)
	}
	if result.stderr != "" {
		t.Errorf("stderr must be silent on success under -o json, got:\n%s", result.stderr)
	}
}

// TestVideoAsyncReturnsHandle pins the --async escape: no polling, the
// interaction id on stdout (pretty) or the create response (structured).
func TestVideoAsyncReturnsHandle(t *testing.T) {
	server := newAsyncVideoServer(t, asyncCreateInProgress, asyncPollCompleted)
	dir := t.TempDir()

	result := runCLIInDir(t, t.TempDir(), dir, nil, videoArgs(server.URL, "--async")...)
	if result.err != nil {
		t.Fatalf("video --async failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if strings.TrimSpace(result.stdout) != "int-vid" {
		t.Errorf("--async stdout = %q, want the bare interaction id", result.stdout)
	}
	if strings.TrimSpace(result.stderr) != "" {
		t.Errorf("--async must not print progress, stderr:\n%s", result.stderr)
	}
	if seen := server.seen(); countPrefix(seen, "GET ") != 0 || countPrefix(seen, "POST ") != 1 {
		t.Errorf("--async must create once and never poll, requests: %v", seen)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("--async wrote files: %v", entries)
	}

	result = runCLIInDir(t, t.TempDir(), dir, nil, videoArgs(server.URL, "--async", "--output-format", "json")...)
	if result.err != nil {
		t.Fatalf("video --async -o json failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &created); err != nil || created.ID != "int-vid" || created.Status != "in_progress" {
		t.Errorf("--async json = %q (err %v), want the create response", result.stdout, err)
	}
	if result.stderr != "" {
		t.Errorf("stderr must be silent under -o json, got:\n%s", result.stderr)
	}
}

// TestVideoHandoffAndFailure pins the terminal-state contract: requires_action
// is terminal for a foreground run (payload printed, exit 0, no file); a
// failure state exits 1 with an empty stdout and a diagnostic naming the id.
func TestVideoHandoffAndFailure(t *testing.T) {
	handoff := newAsyncVideoServer(t, asyncCreateInProgress, asyncPollRequiresAction)
	dir := t.TempDir()
	result := runCLIInDir(t, t.TempDir(), dir, nil, videoArgs(handoff.URL, "--output-format", "json")...)
	if result.err != nil {
		t.Fatalf("handoff must exit 0: %v\nstderr: %s", result.err, result.stderr)
	}
	var payload struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil || payload.Status != "requires_action" || payload.ID != "int-vid" {
		t.Errorf("handoff stdout = %q (err %v), want the requires_action payload", result.stdout, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("handoff wrote files: %v", entries)
	}
	if seen := handoff.seen(); countPrefix(seen, "GET ") != 1 {
		t.Errorf("handoff must stop polling after the first requires_action, requests: %v", seen)
	}

	failed := newAsyncVideoServer(t, asyncCreateInProgress, asyncPollFailed)
	result = runCLIInDir(t, t.TempDir(), dir, nil, videoArgs(failed.URL)...)
	if result.err == nil {
		t.Fatalf("failure state must exit non-zero:\nstdout: %s", result.stdout)
	}
	if strings.TrimSpace(result.stdout) != "" {
		t.Errorf("stdout must be empty on failure, got %q", result.stdout)
	}
	if !strings.Contains(result.stderr, "failed") || !strings.Contains(result.stderr, "int-vid") {
		t.Errorf("failure diagnostic should name the state and id:\n%s", result.stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("failure wrote files: %v", entries)
	}
}

// TestVideoDryRunDoesNotPoll pins that --dry-run previews the create request
// only: no network calls, no polling, empty stdout.
func TestVideoDryRunDoesNotPoll(t *testing.T) {
	server := newAsyncVideoServer(t, asyncCreateInProgress, asyncPollCompleted)
	dir := t.TempDir()
	result := runCLIInDir(t, t.TempDir(), dir, nil, videoArgs(server.URL, "--dry-run")...)
	if result.err != nil {
		t.Fatalf("video --dry-run failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stderr, "[DRY-RUN]") || !strings.Contains(result.stderr, `"background": true`) {
		t.Errorf("dry-run preview missing background preset:\n%s", result.stderr)
	}
	if strings.TrimSpace(result.stdout) != "" {
		t.Errorf("dry-run stdout must be empty, got %q", result.stdout)
	}
	if seen := server.seen(); len(seen) != 0 {
		t.Errorf("dry-run must not touch the network, requests: %v", seen)
	}
}

// TestAgentRunRequiresActionIsTerminal pins §8 for the operation command: a
// synchronous "agent run" whose interaction ends in requires_action prints
// the pending-action payload and exits 0 — it never polls.
func TestAgentRunRequiresActionIsTerminal(t *testing.T) {
	server := newAsyncVideoServer(t, asyncPollRequiresAction)
	args := append(baseArgs(server.URL), "--api-key", "test", "agent", "run", "--body", `{"model":"gemini-3.6-flash","input":"look this up","stream":false}`)
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("agent run failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil || payload.Status != "requires_action" {
		t.Errorf("stdout = %q (err %v), want the requires_action payload", result.stdout, err)
	}
	if seen := server.seen(); len(seen) != 1 {
		t.Errorf("agent run must not poll, requests: %v", seen)
	}
	if result.stderr != "" {
		t.Errorf("stderr must be silent on success under -o json, got:\n%s", result.stderr)
	}
}

// TestMachineModeStderrIsSilentOnSuccess pins spec §7 across command kinds:
// under a structured output format (or agent mode) a successful run writes
// nothing to stderr — no "Wrote …" progress, no pagination hints — and
// --dry-run never leaks the synthetic response to stdout.
func TestMachineModeStderrIsSilentOnSuccess(t *testing.T) {
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNgYAAAAAMAAVCiT19+AAAAAElFTkSuQmCC"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/interactions"):
			fmt.Fprint(w, `{"id":"int-img","object":"interaction","status":"completed","model":"gemini-3.1-flash-image",`+
				`"steps":[{"type":"model_output","content":[{"type":"image","data":"`+pngB64+`","mime_type":"image/png"}]}]}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
			fmt.Fprint(w, `{"data":[{"id":"agent-1","object":"agent","name":"a","model":"gemini-3.6-flash"}],"has_more":true,"next_page_token":"tok"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"unexpected request"}}`)
		}
	}))
	defer server.Close()

	common := []string{"--server-url", server.URL, "--api-version", "v1beta", "--no-interactive", "--no-retries", "--color", "never", "--api-key", "test"}
	dir := t.TempDir()

	cases := map[string]struct {
		env  map[string]string
		args []string
	}{
		"image -o json":         {nil, []string{"image", "a cat", "--output-format", "json"}},
		"image --jq":            {nil, []string{"image", "a cat", "--jq", ".path"}},
		"image agent-mode":      {nil, []string{"image", "a cat", "--agent-mode"}},
		"agent list -o json":    {nil, []string{"agent", "list", "--output-format", "json"}},
		"agent list agent-mode": {nil, []string{"agent", "list", "--agent-mode"}},
		"agent list -o yaml":    {nil, []string{"agent", "list", "--output-format", "yaml"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := runCLIInDir(t, t.TempDir(), dir, tc.env, append(append([]string{}, common...), tc.args...)...)
			if result.err != nil {
				t.Fatalf("%s failed: %v\nstderr: %s", name, result.err, result.stderr)
			}
			if result.stderr != "" {
				t.Errorf("%s: stderr must be silent on success, got:\n%s", name, result.stderr)
			}
			if strings.TrimSpace(result.stdout) == "" {
				t.Errorf("%s: stdout must carry the deliverable", name)
			}
		})
	}

	// Human mode keeps its progress on stderr and the pagination hint.
	result := runCLIInDir(t, t.TempDir(), dir, nil, append(append([]string{}, common...), "agent", "list")...)
	if result.err != nil {
		t.Fatalf("agent list failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stderr, "--all") {
		t.Errorf("pretty mode should keep the pagination hint on stderr:\n%s", result.stderr)
	}

	// --dry-run in human mode: the request preview goes to stderr and stdout
	// stays empty (no synthetic `{}` response), for the streaming intent as
	// well as plain operations. Machine-mode dry runs print the JSON preview
	// protocol on stdout instead (pinned in TestDryRunEverywhereKeylessHumanAndJSON).
	for _, args := range [][]string{
		{"generate", "hi", "--dry-run"},
		{"agent", "list", "--dry-run"},
		{"agent", "get", "--id", "x", "--dry-run"},
	} {
		result := runCLI(t, t.TempDir(), nil, append(append([]string{}, common...), args...)...)
		if result.err != nil {
			t.Fatalf("%v failed: %v\nstderr: %s", args, result.err, result.stderr)
		}
		if strings.TrimSpace(result.stdout) != "" {
			t.Errorf("%v: dry-run stdout must be empty, got %q", args, result.stdout)
		}
		if !strings.Contains(result.stderr, "[DRY-RUN] Network call skipped.") {
			t.Errorf("%v: dry-run preview missing:\n%s", args, result.stderr)
		}
	}
	for _, args := range [][]string{
		{"generate", "hi", "--dry-run", "--output-format", "json"},
		{"agent", "get", "--id", "x", "--dry-run", "--jq", ".dry_run"},
	} {
		result := runCLI(t, t.TempDir(), nil, append(append([]string{}, common...), args...)...)
		if result.err != nil {
			t.Fatalf("%v failed: %v\nstderr: %s", args, result.err, result.stderr)
		}
		if result.stderr != "" {
			t.Errorf("%v: machine-mode dry-run must keep stderr silent, got %q", args, result.stderr)
		}
		if !strings.Contains(result.stdout, "true") {
			t.Errorf("%v: machine-mode dry-run stdout should carry the preview, got %q", args, result.stdout)
		}
	}
}
