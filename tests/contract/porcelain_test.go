package contract_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// plainArgs is baseArgs without the JSON output format: porcelain commands
// print only the deliverable on stdout unless an output format is requested.
func plainArgs(serverURL string) []string {
	return []string{
		"--server-url", serverURL,
		"--api-version", "v1beta",
		"--api-key", "test-key",
		"--no-interactive",
		"--no-retries",
		"--color", "never",
	}
}

// completedInteraction renders a completed non-streaming interaction in the
// current wire shape: the model's text lives in a model_output step (the
// top-level output_* fields were removed upstream). usage is an optional raw
// JSON object.
func completedInteraction(text, usage string) string {
	body := map[string]any{
		"id":     "int-1",
		"status": "completed",
		"steps": []map[string]any{{
			"type":    "model_output",
			"content": []map[string]any{{"type": "text", "text": text}},
		}},
	}
	if usage != "" {
		body["usage"] = json.RawMessage(usage)
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// TestAnalyzeInlinePorcelain: local file → inline interaction content block, text on stdout.
func TestAnalyzeInlinePorcelain(t *testing.T) {
	var sawInline bool
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		// A local PDF is inlined as a document content block carrying base64 data.
		sawInline = strings.Contains(string(raw), `"type":"document"`) && strings.Contains(string(raw), `"data"`)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, completedInteraction("A short note about testing.", ""))
	}))
	defer server.Close()

	dir := t.TempDir()
	notePath := filepath.Join(dir, "note.pdf")
	os.WriteFile(notePath, []byte("%PDF-1.4"), 0o644)

	args := append(plainArgs(server.URL), "analyze", "--input", notePath, "what is this?")
	result := runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("analyze failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if gotPath != "/v1beta/interactions" {
		t.Errorf("request path = %q, want /v1beta/interactions", gotPath)
	}
	if !sawInline {
		t.Error("request did not carry inline base64 data")
	}
	if strings.TrimSpace(result.stdout) != "A short note about testing." {
		t.Errorf("stdout = %q", result.stdout)
	}
}

// TestAnalyzeRemoteFilePorcelain: files/<id> is resolved via files.get, then
// referenced by uri in the interaction content block.
func TestAnalyzeRemoteFilePorcelain(t *testing.T) {
	var getHit, genHit atomic.Int32
	var genBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1beta/interactions":
			genHit.Add(1)
			raw, _ := io.ReadAll(r.Body)
			genBody = string(raw)
			fmt.Fprint(w, completedInteraction("answer", ""))
		case strings.Contains(r.URL.Path, "/files/"):
			getHit.Add(1)
			fmt.Fprint(w, `{"name":"files/abc123","uri":"https://example/files/abc123","mimeType":"video/mp4","state":"ACTIVE"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	args := append(plainArgs(server.URL), "analyze", "--input", "files/abc123", "what happens?")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("analyze remote failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if getHit.Load() != 1 || genHit.Load() != 1 {
		t.Errorf("expected one files.get and one interaction, got %d/%d", getHit.Load(), genHit.Load())
	}
	if !strings.Contains(genBody, `"uri"`) || !strings.Contains(genBody, "example/files/abc123") {
		t.Errorf("interaction did not reference the resolved file URI: %s", genBody)
	}
}

// TestTranscribeSRTPorcelain: structured segments rendered to an SRT artifact.
func TestTranscribeSRTPorcelain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		segs := `{"segments":[{"speaker":"Speaker 1","start_time":"00:00","end_time":"00:02","content":"Hello."},{"speaker":"Speaker 2","start_time":"00:02","end_time":"00:05","content":"Hi there."}]}`
		fmt.Fprint(w, completedInteraction(segs, ""))
	}))
	defer server.Close()

	dir := t.TempDir()
	audioPath := filepath.Join(dir, "clip.mp3")
	os.WriteFile(audioPath, []byte("not really audio"), 0o644)
	outPath := filepath.Join(dir, "out.srt")

	args := append(plainArgs(server.URL), "transcribe", "--input", audioPath, "--format", "srt", "--out", outPath)
	result := runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("transcribe failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if strings.TrimSpace(result.stdout) != outPath {
		t.Errorf("stdout = %q, want %q", result.stdout, outPath)
	}
	srt, _ := os.ReadFile(outPath)
	for _, want := range []string{"1\n00:00:00,000 --> 00:00:02,000", "Speaker 1: Hello.", "Speaker 2: Hi there."} {
		if !strings.Contains(string(srt), want) {
			t.Errorf("SRT missing %q:\n%s", want, srt)
		}
	}
}

// TestFilesUploadWait pins --wait: it polls files.get until ACTIVE, and stops
// with a runtime error on FAILED, an unknown state, or the --wait-timeout.
func TestFilesUploadWait(t *testing.T) {
	cases := []struct {
		name       string
		states     []string // successive files.get states; the last one repeats
		wantCode   int
		wantStderr string
		wantPolls  int32
	}{
		{"processing then active", []string{"PROCESSING", "ACTIVE"}, 0, "files/xyz789 is ACTIVE", 2},
		{"unspecified then active", []string{"STATE_UNSPECIFIED", "ACTIVE"}, 0, "files/xyz789 is ACTIVE", 2},
		{"failed", []string{"FAILED"}, 1, "failed processing on the server", 1},
		{"unknown state", []string{"SOMETHING_NEW"}, 1, "unexpected state SOMETHING_NEW", 1},
		{"timeout", []string{"PROCESSING"}, 1, "did not become ACTIVE within 300ms", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var polls atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Header.Get("X-Goog-Upload-Command") {
				case "start":
					w.Header().Set("X-Goog-Upload-Url", server.URL+"/upload-session")
				case "upload, finalize":
					w.Header().Set("X-Goog-Upload-Status", "final")
					fmt.Fprint(w, `{"file":{"name":"files/xyz789","mimeType":"video/mp4","state":"PROCESSING"}}`)
				default:
					n := int(polls.Add(1))
					state := tc.states[min(n, len(tc.states))-1]
					fmt.Fprintf(w, `{"name":"files/xyz789","mimeType":"video/mp4","state":%q}`, state)
				}
			}))
			defer server.Close()

			dir := t.TempDir()
			src := filepath.Join(dir, "clip.mp4")
			os.WriteFile(src, []byte("some video bytes"), 0o644)
			args := append(plainArgs(server.URL), "files", "upload", src, "--wait", "--wait-timeout", "300ms")
			result := runCLI(t, dir, nil, args...)
			if code := exitCode(t, result); code != tc.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr: %s", code, tc.wantCode, result.stderr)
			}
			if !strings.Contains(result.stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", result.stderr, tc.wantStderr)
			}
			if got := polls.Load(); got != tc.wantPolls {
				t.Errorf("files.get polls = %d, want %d", got, tc.wantPolls)
			}
			if tc.wantCode == 0 && strings.TrimSpace(result.stdout) != "files/xyz789" {
				t.Errorf("stdout = %q, want files/xyz789", result.stdout)
			}
		})
	}
}

// TestFilesUploadDryRun: dry-run previews the start and every chunk without networking.
func TestFilesUploadDryRun(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp3")
	os.WriteFile(src, []byte("bytes"), 0o644)

	args := append(plainArgs(server.URL), "--dry-run", "files", "upload", src)
	result := runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("files upload --dry-run failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if requests.Load() != 0 {
		t.Errorf("dry-run made %d network requests, want 0", requests.Load())
	}
	if result.stdout != "" {
		t.Errorf("dry-run wrote stdout: %q", result.stdout)
	}
	if !strings.Contains(result.stderr, "upload/v1beta/files") {
		t.Errorf("dry-run did not preview the upload request:\n%s", result.stderr)
	}
	if strings.Count(result.stderr, "[DRY-RUN] Would send") != 2 {
		t.Errorf("dry-run should preview start plus one chunk:\n%s", result.stderr)
	}
	if !strings.Contains(result.stderr, "dry-run-session") || !strings.Contains(result.stderr, "<bytes:5>") {
		t.Errorf("dry-run did not preview the redacted chunk:\n%s", result.stderr)
	}
}

// TestFilesIdentifierForms: the generated files get/delete accept the id as a
// positional or via --file, bare or "files/"-prefixed, and always send the bare
// id as the path parameter.
func TestFilesIdentifierForms(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantMethod string
	}{
		{name: "get positional prefixed", args: []string{"files", "get", "files/abc"}, wantMethod: http.MethodGet},
		{name: "get positional bare", args: []string{"files", "get", "abc"}, wantMethod: http.MethodGet},
		{name: "get flag prefixed", args: []string{"files", "get", "--file", "files/abc"}, wantMethod: http.MethodGet},
		{name: "get flag bare", args: []string{"files", "get", "--file", "abc"}, wantMethod: http.MethodGet},
		{name: "delete positional prefixed", args: []string{"files", "delete", "files/abc"}, wantMethod: http.MethodDelete},
		{name: "delete flag prefixed", args: []string{"files", "delete", "--file", "files/abc"}, wantMethod: http.MethodDelete},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var gotMethod, gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"name":"files/abc","state":"ACTIVE"}`)
			}))
			defer server.Close()

			args := append(baseArgs(server.URL), tt.args...)
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("%s failed: %v\nstderr: %s", tt.name, result.err, result.stderr)
			}
			if gotMethod != tt.wantMethod || gotPath != "/v1beta/files/abc" {
				t.Errorf("request = %s %q, want %s /v1beta/files/abc", gotMethod, gotPath, tt.wantMethod)
			}
		})
	}
}

// TestPorcelainUsageKDL: --usage on a claimed command yields its live surface.
func TestPorcelainUsageKDL(t *testing.T) {
	for _, tc := range []struct {
		cmd  []string
		want []string
	}{
		{[]string{"analyze", "--usage"}, []string{`cmd "analyze"`, `arg "question"`, `flag "-i --input <input...>"`}},
		{[]string{"transcribe", "--usage"}, []string{`cmd "transcribe"`, `flag "-i --input <input...>"`}},
		{[]string{"tts", "--usage"}, []string{`cmd "tts"`, `arg "text"`, `--voice`, `--multi-speaker`, `--language`}},
		{[]string{"files", "upload", "--usage"}, []string{`cmd "upload"`, `--display-name`, `--wait`}},
	} {
		result := runCLI(t, t.TempDir(), nil, tc.cmd...)
		if result.err != nil {
			t.Fatalf("%v --usage failed: %v\nstderr: %s", tc.cmd, result.err, result.stderr)
		}
		for _, want := range tc.want {
			if !strings.Contains(result.stdout, want) {
				t.Errorf("%v --usage missing %q:\n%s", tc.cmd, want, result.stdout)
			}
		}
	}
}

// TestPorcelainErrorEnvelope: an API error is classified through the shared
// agent-mode envelope, exactly like generated commands.
func TestPorcelainErrorEnvelope(t *testing.T) {
	// The live Interactions API reports errors in a singleton-array envelope
	// (see TestSingletonArrayInteractionError); the shared classifier unwraps it
	// and classifies reason-first.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `[{"error":{"code":400,"message":"API key not valid.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID"}]}}]`)
	}))
	defer server.Close()

	dir := t.TempDir()
	notePath := filepath.Join(dir, "note.txt")
	os.WriteFile(notePath, []byte("some text"), 0o644)

	args := append(plainArgs(server.URL), "--agent-mode", "analyze", "-i", notePath, "hi")
	result := runCLI(t, dir, nil, args...)
	if result.err == nil {
		t.Fatal("analyze succeeded on HTTP 400 API_KEY_INVALID")
	}
	var envelope struct {
		ErrorType   string `json:"error_type"`
		ErrorReason string `json:"error_reason"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
		t.Fatalf("stderr is not one JSON document: %v\n%s", err, result.stderr)
	}
	if envelope.ErrorType != "authentication_error" || envelope.ErrorReason != "API_KEY_INVALID" {
		t.Errorf("classification = %+v, want authentication_error/API_KEY_INVALID", envelope)
	}
}

// TestTranscribeVideoMIME pins that a video container is sent as video/mp4, not
// audio/mp4 (the API extracts the audio track for transcription).
func TestTranscribeVideoMIME(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	os.WriteFile(src, []byte("fake video"), 0o644)

	args := append(plainArgs("https://example.invalid"), "--dry-run", "transcribe", "--input", src)
	result := runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("transcribe dry-run failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stderr, "video/mp4") {
		t.Errorf("transcribe did not use video/mp4 for a .mp4 input:\n%s", result.stderr)
	}
	if strings.Contains(result.stderr, "audio/mp4") {
		t.Errorf("transcribe used the wrong audio/mp4 MIME type:\n%s", result.stderr)
	}
}

// TestServerSelectionFlagOmitted pins the single-server CLI contract:
// --server is absent, while --server-url remains available.
func TestServerSelectionFlagOmitted(t *testing.T) {
	result := runCLI(t, t.TempDir(), nil, "--server", "99", "agent", "list")
	if result.err == nil {
		t.Fatal("unregistered --server unexpectedly succeeded")
	}
	if !strings.Contains(result.stderr, "unknown flag: --server") {
		t.Errorf("error does not report --server as unknown: %s", result.stderr)
	}

	// Global flags are documented once, behind the root-only --help-global.
	help := runCLI(t, t.TempDir(), nil, "--help-global")
	if help.err != nil {
		t.Fatalf("--help-global failed: %v\nstderr: %s", help.err, help.stderr)
	}
	if strings.Contains(help.stdout, "--server string") {
		t.Errorf("--help-global unexpectedly documents --server:\n%s", help.stdout)
	}
	if !strings.Contains(help.stdout, "--server-url string") {
		t.Errorf("--help-global does not document --server-url:\n%s", help.stdout)
	}

	usage := runCLI(t, t.TempDir(), nil, "--usage")
	if usage.err != nil {
		t.Fatalf("--usage failed: %v\nstderr: %s", usage.err, usage.stderr)
	}
	if strings.Contains(usage.stdout, `flag "--server <server>"`) {
		t.Errorf("--usage unexpectedly includes --server:\n%s", usage.stdout)
	}
}

// TestAgentModeEmitsEnvelope: in agent mode a porcelain command emits the
// structured envelope (matching the CLI-wide --agent-mode contract), not the
// bare deliverable.
func TestAgentModeEmitsEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, completedInteraction("the answer", `{"total_tokens":42}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	notePath := filepath.Join(dir, "note.txt")
	os.WriteFile(notePath, []byte("some text"), 0o644)

	args := append(plainArgs(server.URL), "--agent-mode", "analyze", "-i", notePath, "hi")
	result := runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("analyze failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if strings.TrimSpace(result.stdout) == "the answer" {
		t.Errorf("agent mode returned the bare deliverable instead of an envelope:\n%s", result.stdout)
	}
	if !strings.Contains(result.stdout, "total_tokens") || !strings.Contains(result.stdout, "42") {
		t.Errorf("agent-mode envelope missing usage total_tokens:\n%s", result.stdout)
	}
}
