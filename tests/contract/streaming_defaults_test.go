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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Streaming is the default on every stream-capable operation, not only on
// the generate intent: "agent run" sends stream:true unless the body (or
// --stream=false) says otherwise, "agent status" asks for the event stream
// unless --stream=false, and both project text deltas raw exactly like
// "generate". The interactive (prompted) path must behave like argv.

const agentRunBody = `{"model":"gemini-3.6-flash","input":"hi"}`

func TestAgentRunDryRunStreamsByDefault(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStream bool
		wantAccept string
	}{
		{
			name:       "body without stream streams",
			args:       []string{"agent", "run", "--body", agentRunBody},
			wantStream: true,
			wantAccept: "    Accept: text/event-stream\n",
		},
		{
			name:       "body stream:false is honored",
			args:       []string{"agent", "run", "--body", `{"model":"gemini-3.6-flash","input":"hi","stream":false}`},
			wantStream: false,
			wantAccept: "    Accept: application/json;q=1, text/event-stream;q=0\n",
		},
		{
			name:       "--stream=false opts out",
			args:       []string{"agent", "run", "--body", agentRunBody, "--stream=false"},
			wantStream: false,
			wantAccept: "    Accept: application/json;q=1, text/event-stream;q=0\n",
		},
		{
			// The create-interaction union renders as --model / --agent flag
			// sets (Build Spec §9); the flag path streams by default too.
			name:       "model flag without stream streams",
			args:       []string{"agent", "run", "hi", "--model", "gemini-3.6-flash"},
			wantStream: true,
			wantAccept: "    Accept: text/event-stream\n",
		},
		{
			name:       "agent variant streams too",
			args:       []string{"agent", "run", "--body", `{"agent":"research-bot","input":"hi"}`},
			wantStream: true,
			wantAccept: "    Accept: text/event-stream\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(baseArgs("https://example.invalid"), "--dry-run")
			args = append(args, tt.args...)
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("agent run --dry-run failed: %v\nstderr: %s", result.err, result.stderr)
			}
			// baseArgs selects --output-format json, so the preview is the
			// NDJSON object on stdout (stderr is silent).
			request := parseDryRunRequest(t, result.stdout)
			body := parseDryRunBody(t, result.stdout)
			stream, ok := body["stream"].(bool)
			if !ok {
				t.Fatalf("dry-run stream = %#v, want a boolean\n%s", body["stream"], result.stdout)
			}
			if stream != tt.wantStream {
				t.Errorf("dry-run stream = %t, want %t", stream, tt.wantStream)
			}
			wantAccept := strings.TrimSpace(strings.TrimPrefix(tt.wantAccept, "    Accept:"))
			if got := dryRunHeader(t, request, "Accept"); got != wantAccept {
				t.Errorf("dry-run Accept = %q, want %q", got, wantAccept)
			}
		})
	}
}

func TestAgentRunStreamFlagConflictsWithBodyKey(t *testing.T) {
	args := append(baseArgs("https://example.invalid"), "--dry-run",
		"agent", "run", "--body", `{"model":"gemini-3.6-flash","input":"hi","stream":true}`, "--stream=false")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatalf("expected an error when --stream and the body both set stream\nstderr: %s", result.stderr)
	}
	if !strings.Contains(result.stderr, "stream") {
		t.Errorf("error does not name the conflicting input:\n%s", result.stderr)
	}
}

func TestAgentRunHelpDocumentsStreamingDefault(t *testing.T) {
	result := runCLI(t, t.TempDir(), nil, "agent", "run", "--help")
	if result.err != nil {
		t.Fatalf("agent run --help failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stdout, "--stream") {
		t.Errorf("agent run help lacks a --stream flag:\n%s", result.stdout)
	}
	if !strings.Contains(strings.ToLower(result.stdout), "stream=false") {
		t.Errorf("agent run help does not explain the opt-out:\n%s", result.stdout)
	}
}

func TestAgentRunDefaultStreamProjection(t *testing.T) {
	server := newTextDeltaServer(t)
	defer server.Close()

	args := []string{
		"--server-url", server.URL, "--api-version", "v1beta", "--no-interactive",
		"--no-retries", "--color", "never", "--api-key", "test",
		"agent", "run", "--body", agentRunBody,
	}
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("agent run failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if result.stdout != "Hello world\n" {
		t.Errorf("stdout = %q, want the raw projected text", result.stdout)
	}

	// An explicit -o json keeps the NDJSON events.
	result = runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL), "agent", "run", "--body", agentRunBody)...)
	if result.err != nil {
		t.Fatalf("agent run -o json failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if got := deltaTexts(t, result.stdout); !reflect.DeepEqual(got, []string{"Hello ", "world"}) {
		t.Errorf("NDJSON texts = %#v, want the two unprojected delta events", got)
	}
}

func TestAgentRunStreamFalseReturnsCompleteResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"stream":true`) {
			t.Errorf("request body still streams: %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"int-1","object":"interaction","status":"completed","model":"gemini-3.6-flash","steps":[{"type":"model_output","content":[{"type":"text","text":"Hello world"}]}]}`)
	}))
	defer server.Close()

	result := runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL), "agent", "run", "--body", agentRunBody, "--stream=false")...)
	if result.err != nil {
		t.Fatalf("agent run --stream=false failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var interaction map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &interaction); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, result.stdout)
	}
	if interaction["id"] != "int-1" || interaction["status"] != "completed" {
		t.Errorf("interaction = %#v, want the complete result", interaction)
	}
}

func TestAgentStatusDryRunDefaultIsNonStreaming(t *testing.T) {
	tests := []struct {
		name       string
		extra      []string
		wantQuery  string
		wantAccept string
	}{
		{name: "default returns the status object", wantQuery: "stream=false", wantAccept: "    Accept: application/json;q=1, text/event-stream;q=0\n"},
		{name: "--stream opts into event stream", extra: []string{"--stream"}, wantQuery: "stream=true", wantAccept: "    Accept: text/event-stream\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(baseArgs("https://example.invalid"), "--dry-run", "agent", "status", "--id", "int-1")
			args = append(args, tt.extra...)
			result := runCLI(t, t.TempDir(), nil, args...)
			if result.err != nil {
				t.Fatalf("agent status --dry-run failed: %v\nstderr: %s", result.err, result.stderr)
			}
			// baseArgs selects --output-format json: the preview object is on stdout.
			request := parseDryRunRequest(t, result.stdout)
			if url, _ := request["url"].(string); !strings.Contains(url, tt.wantQuery) {
				t.Errorf("dry-run URL lacks %q:\n%s", tt.wantQuery, result.stdout)
			}
			wantAccept := strings.TrimSpace(strings.TrimPrefix(tt.wantAccept, "    Accept:"))
			if got := dryRunHeader(t, request, "Accept"); got != wantAccept {
				t.Errorf("dry-run Accept = %q, want %q", got, wantAccept)
			}
		})
	}
}

func TestAgentStatusStreamOptInProjection(t *testing.T) {
	server := newTextDeltaServer(t)
	defer server.Close()

	args := []string{
		"--server-url", server.URL, "--api-version", "v1beta", "--no-interactive",
		"--no-retries", "--color", "never", "--api-key", "test",
		"agent", "status", "--id", "int-1", "--stream",
	}
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("agent status --stream failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if result.stdout != "Hello world\n" {
		t.Errorf("stdout = %q, want the raw projected text", result.stdout)
	}
	result = runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL), "agent", "status", "--id", "int-1", "--stream")...)
	if result.err != nil {
		t.Fatalf("agent status --stream -o json failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if got := deltaTexts(t, result.stdout); !reflect.DeepEqual(got, []string{"Hello ", "world"}) {
		t.Errorf("NDJSON texts = %#v, want the two unprojected delta events", got)
	}
}

// deltaTexts decodes NDJSON stream events and returns their delta texts.
func deltaTexts(t *testing.T, stdout string) []string {
	t.Helper()
	var texts []string
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("NDJSON line is not JSON: %v\nline: %s", err, scanner.Text())
		}
		data, _ := event["data"].(map[string]any)
		delta, _ := data["delta"].(map[string]any)
		text, _ := delta["text"].(string)
		texts = append(texts, text)
	}
	return texts
}

// The interactive path: a prompted "generate" streams and projects like the
// argv path; the preset stream:true survives the prompt.
func TestGenerateInteractiveStreamsLikeArgv(t *testing.T) {
	clearAgentModeEnvironment(t)

	prompter := &scriptedPrompter{t: t, answers: map[string][]string{"arg:prompt": {"hi there"}}}
	_, stderr, err := executeInteractive(t, prompter,
		"--interactive", "--dry-run", "--api-key", "k", "--server-url", "https://example.invalid", "--api-version", "v1beta", "--color", "never", "generate")
	if err != nil {
		t.Fatalf("prompted generate --dry-run failed: %v\nstderr: %s", err, stderr)
	}
	if !reflect.DeepEqual(prompter.calls, [][]string{{"arg:prompt"}}) {
		t.Fatalf("prompt calls = %#v, want one call for arg:prompt", prompter.calls)
	}
	// Human mode (no --output-format): the [DRY-RUN] block is on stderr.
	body := parseHumanDryRunBody(t, stderr)
	if body["stream"] != true || body["input"] != "hi there" {
		t.Errorf("prompted dry-run body = %#v, want stream:true and the prompted input", body)
	}
	if !strings.Contains(stderr, "    Accept: text/event-stream\n") {
		t.Errorf("prompted dry-run does not ask for SSE:\n%s", stderr)
	}

	server := newTextDeltaServer(t)
	defer server.Close()
	prompter = &scriptedPrompter{t: t, answers: map[string][]string{"arg:prompt": {"hi there"}}}
	stdout, stderr, err := executeInteractive(t, prompter,
		"--interactive", "--api-key", "k", "--server-url", server.URL, "--api-version", "v1beta", "--no-retries", "--color", "never", "generate")
	if err != nil {
		t.Fatalf("prompted generate failed: %v\nstderr: %s", err, stderr)
	}
	if stdout != "Hello world\n" {
		t.Errorf("prompted stdout = %q, want the raw projected text (same as argv)", stdout)
	}

	// A user-chosen -o json before prompting keeps NDJSON events.
	prompter = &scriptedPrompter{t: t, answers: map[string][]string{"arg:prompt": {"hi there"}}}
	stdout, stderr, err = executeInteractive(t, prompter,
		"--interactive", "--api-key", "k", "--server-url", server.URL, "--api-version", "v1beta", "--no-retries", "--color", "never", "-o", "json", "generate")
	if err != nil {
		t.Fatalf("prompted generate -o json failed: %v\nstderr: %s", err, stderr)
	}
	if got := deltaTexts(t, stdout); !reflect.DeepEqual(got, []string{"Hello ", "world"}) {
		t.Errorf("prompted NDJSON texts = %#v, want the two unprojected delta events", got)
	}
}

// The interactive path for an artifact intent offers an optional output file
// directly after the prompt; empty keeps the default name, a path is used
// exactly like --out (extension follows the returned MIME type).
func TestImageInteractivePromptsForOutputFile(t *testing.T) {
	clearAgentModeEnvironment(t)
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNgYAAAAAMAAVCiT19+AAAAAElFTkSuQmCC"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"int-img","object":"interaction","status":"completed","model":"gemini-3.1-flash-image",`+
			`"steps":[{"type":"model_output","content":[{"type":"image","data":"`+pngB64+`","mime_type":"image/png"}]}]}`)
	}))
	defer server.Close()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	base := []string{"--interactive", "--api-key", "k", "--server-url", server.URL, "--api-version", "v1beta", "--no-retries", "--color", "never", "image"}

	// A chosen path (extension corrected to the returned MIME type, with a note).
	target := filepath.Join(dir, "shots", "hero.jpg")
	prompter := &scriptedPrompter{t: t, answers: map[string][]string{"arg:prompt": {"a lighthouse"}, "flag:out": {target}}}
	stdout, stderr, err := executeInteractive(t, prompter, base...)
	if err != nil {
		t.Fatalf("prompted image failed: %v\nstderr: %s", err, stderr)
	}
	if !reflect.DeepEqual(prompter.calls, [][]string{{"arg:prompt", "flag:out"}}) {
		t.Fatalf("prompt calls = %#v, want one call for arg:prompt then flag:out", prompter.calls)
	}
	wantPath := filepath.Join(dir, "shots", "hero.png")
	if strings.TrimSpace(stdout) != wantPath {
		t.Errorf("stdout = %q, want %q", stdout, wantPath)
	}
	if data, err := os.ReadFile(wantPath); err != nil || len(data) != 69 {
		t.Errorf("written file %q: err=%v len=%d, want the 69-byte PNG", wantPath, err, len(data))
	}
	if !strings.Contains(stderr, "hero.jpg") || !strings.Contains(stderr, "hero.png") {
		t.Errorf("stderr lacks the extension-change note:\n%s", stderr)
	}

	// An empty answer keeps the default filename in the working directory.
	prompter = &scriptedPrompter{t: t, answers: map[string][]string{"arg:prompt": {"a lighthouse"}}}
	stdout, stderr, err = executeInteractive(t, prompter, base...)
	if err != nil {
		t.Fatalf("prompted image (default name) failed: %v\nstderr: %s", err, stderr)
	}
	printed := strings.TrimSpace(stdout)
	if filepath.Dir(printed) != dir || !strings.HasPrefix(filepath.Base(printed), "gemini-image-") || filepath.Ext(printed) != ".png" {
		t.Errorf("default path = %q, want ./gemini-image-*.png inside %s", printed, dir)
	}

	// --out given up front is not asked again.
	prompter = &scriptedPrompter{t: t, answers: map[string][]string{"arg:prompt": {"a lighthouse"}, "flag:out": {"unused.png"}}}
	_, stderr, err = executeInteractive(t, prompter, append(base, "--out", filepath.Join(dir, "given.png"))...)
	if err != nil {
		t.Fatalf("prompted image --out failed: %v\nstderr: %s", err, stderr)
	}
	if !reflect.DeepEqual(prompter.calls, [][]string{{"arg:prompt"}}) {
		t.Errorf("prompt calls = %#v, want only arg:prompt when --out was given", prompter.calls)
	}
}
