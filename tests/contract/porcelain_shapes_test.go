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
	"encoding/base64"
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

// interactionStub serves one fixed interaction response and captures the last
// POST /v1beta/interactions request body.
type interactionStub struct {
	*httptest.Server
	body     map[string]any
	requests atomic.Int32
}

func newInteractionStub(t *testing.T, response string) *interactionStub {
	t.Helper()
	stub := &interactionStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.requests.Add(1)
		if r.URL.Path != "/v1beta/interactions" {
			t.Errorf("request path = %q, want /v1beta/interactions", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		stub.body = nil
		if err := json.Unmarshal(raw, &stub.body); err != nil {
			t.Errorf("request body is not JSON: %v\n%s", err, raw)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
	}))
	t.Cleanup(stub.Close)
	return stub
}

// assertNotStored pins the porcelain retention policy: every request opts out
// of server-side interaction storage, which the API otherwise defaults on.
func assertNotStored(t *testing.T, reqBody map[string]any) {
	t.Helper()
	if store, ok := reqBody["store"]; !ok || store != false {
		t.Errorf("store = %#v, want an explicit false", reqBody["store"])
	}
}

// inputParts returns the request's input content blocks.
func (s *interactionStub) inputParts(t *testing.T) []map[string]any {
	t.Helper()
	raw, _ := s.body["input"].([]any)
	parts := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		part, _ := p.(map[string]any)
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		t.Fatalf("request has no input content blocks: %v", s.body)
	}
	return parts
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInteractionTextExtraction pins how porcelain reads the model's answer:
// from the model_output steps of the current wire shape (concatenating text
// blocks, skipping thoughts and non-text blocks), from the legacy top-level
// output_text when present, and as a runtime error — never an empty
// deliverable with exit 0 — when the interaction carries no text.
func TestInteractionTextExtraction(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		wantStdout string
		wantStderr string
	}{
		{
			name:       "single model output step",
			response:   completedInteraction("from steps", ""),
			wantStdout: "from steps",
		},
		{
			name: "thought and non-text blocks are skipped",
			response: `{"id":"int-1","status":"completed","steps":[
				{"type":"thought","summary":[{"type":"text","text":"thinking"}]},
				{"type":"model_output","content":[
					{"type":"text","text":"first "},
					{"type":"image","mime_type":"image/png","data":"aGk="},
					{"type":"text","text":"second"}]}]}`,
			wantStdout: "first second",
		},
		{
			name: "multiple model output steps concatenate",
			response: `{"id":"int-1","status":"completed","steps":[
				{"type":"model_output","content":[{"type":"text","text":"one "}]},
				{"type":"model_output","content":[{"type":"text","text":"two"}]}]}`,
			wantStdout: "one two",
		},
		{
			name:       "legacy top-level output_text",
			response:   `{"id":"int-1","status":"completed","output_text":"legacy answer"}`,
			wantStdout: "legacy answer",
		},
		{
			name:       "failed interaction surfaces the platform error",
			response:   `{"id":"int-1","status":"failed","errors":[{"code":"blocked","message":"content was blocked"}]}`,
			wantStderr: "the interaction did not complete (status: failed): content was blocked",
		},
		{
			name: "incomplete interaction with text is not a deliverable",
			response: `{"id":"int-1","status":"incomplete","steps":[
				{"type":"model_output","content":[{"type":"text","text":"cut off mid"}]}]}`,
			wantStderr: "the API stopped before the output was complete (status: incomplete)",
		},
		{
			name: "budget_exceeded interaction with text is not a deliverable",
			response: `{"id":"int-1","status":"budget_exceeded","steps":[
				{"type":"model_output","content":[{"type":"text","text":"cut off mid"}]}]}`,
			wantStderr: "the API stopped before the output was complete (status: budget_exceeded)",
		},
		{
			name:       "cancelled interaction with text is not a deliverable",
			response:   `{"id":"int-1","status":"cancelled","output_text":"partial"}`,
			wantStderr: "the interaction did not complete (status: cancelled)",
		},
		{
			name: "omitted status is accepted",
			response: `{"id":"int-1","steps":[
				{"type":"model_output","content":[{"type":"text","text":"no status"}]}]}`,
			wantStdout: "no status",
		},
		{
			name: "unknown status is accepted",
			response: `{"id":"int-1","status":"future_state","steps":[
				{"type":"model_output","content":[{"type":"text","text":"unknown status"}]}]}`,
			wantStdout: "unknown status",
		},
		{
			name:       "completed without text names the status",
			response:   `{"id":"int-1","status":"completed","steps":[]}`,
			wantStderr: "the API returned no text (status: completed)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newInteractionStub(t, tt.response)
			note := writeTempFile(t, "note.pdf", "pdf bytes")
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "analyze", "-i", note)...)
			if tt.wantStderr != "" {
				if code := exitCode(t, result); code != 1 {
					t.Errorf("exit code = %d, want 1", code)
				}
				if result.stdout != "" {
					t.Errorf("failure wrote stdout: %q", result.stdout)
				}
				if !strings.Contains(result.stderr, tt.wantStderr) {
					t.Errorf("stderr = %q, want it to contain %q", result.stderr, tt.wantStderr)
				}
				return
			}
			if result.err != nil {
				t.Fatalf("analyze failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if got := strings.TrimSpace(result.stdout); got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
		})
	}
}

// TestAnalyzeRequestBody pins the analyze request: a non-streaming model
// interaction carrying the media block, then the question, with --model,
// --system, and --mime-type applied.
func TestAnalyzeRequestBody(t *testing.T) {
	stub := newInteractionStub(t, completedInteraction("ok", ""))
	clip := writeTempFile(t, "clip.bin", "bytes")

	args := append(plainArgs(stub.URL), "analyze", "-i", clip, "--mime-type", "audio/wav",
		"--model", "models/gemini-custom", "--system", "  Be terse.  ", "what", "is", "this?")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("analyze failed: %v\nstderr: %s", result.err, result.stderr)
	}

	if stub.body["model"] != "gemini-custom" {
		t.Errorf("model = %v, want gemini-custom (models/ prefix stripped)", stub.body["model"])
	}
	if stub.body["stream"] != false {
		t.Errorf("stream = %v, want false", stub.body["stream"])
	}
	assertNotStored(t, stub.body)
	if stub.body["system_instruction"] != "Be terse." {
		t.Errorf("system_instruction = %v, want %q", stub.body["system_instruction"], "Be terse.")
	}
	if _, ok := stub.body["response_format"]; ok {
		t.Errorf("analyze sent response_format: %v", stub.body["response_format"])
	}
	parts := stub.inputParts(t)
	if len(parts) != 2 {
		t.Fatalf("input parts = %d, want media + question", len(parts))
	}
	if parts[0]["type"] != "audio" || parts[0]["mime_type"] != "audio/wav" || parts[0]["data"] != "Ynl0ZXM=" {
		t.Errorf("media part = %v, want inline audio/wav base64", parts[0])
	}
	if parts[1]["type"] != "text" || parts[1]["text"] != "what is this?" {
		t.Errorf("question part = %v", parts[1])
	}
}

// TestTranscribeRequestAndArtifact pins, per --format and the speaker/timestamp
// switches: whether the request enforces the transcript JSON schema through
// response_format, which segment keys the schema requires, what the prompt
// asks for, and the artifact written.
func TestTranscribeRequestAndArtifact(t *testing.T) {
	const segments = `{"segments":[{"speaker":"Speaker 1","start_time":"00:01","end_time":"00:03.5","content":"Hello."}]}`
	const bareSegments = `{"segments":[{"content":"Hello."}]}`

	tests := []struct {
		name          string
		flags         []string
		response      string
		wantExt       string
		wantRequired  []string
		wantPrompt    []string
		wantArtifact  []string
		rejectInFile  []string
		wantNoSchema  bool
		wantArtifactE string
	}{
		{
			name: "md default", response: "# Transcript", wantExt: ".md", wantNoSchema: true,
			wantPrompt:    []string{"label distinct speakers", "Include accurate timestamps", "clean Markdown"},
			wantArtifactE: "# Transcript\n",
		},
		{
			name: "text without speakers or timestamps", flags: []string{"--format", "text", "--no-speakers", "--no-timestamps"},
			response: "Hello.", wantExt: ".txt", wantNoSchema: true,
			wantPrompt:    []string{"Do not include speaker labels", "Do not include timestamps", "plain text"},
			wantArtifactE: "Hello.\n",
		},
		{
			name: "json", flags: []string{"--format", "json"}, response: segments, wantExt: ".json",
			wantRequired: []string{"content", "speaker", "start_time", "end_time"},
			wantPrompt:   []string{"Return JSON matching the response schema"},
			wantArtifact: []string{`"speaker": "Speaker 1"`, `"start_time": "00:01"`, `"content": "Hello."`},
		},
		{
			name: "json without speakers or timestamps", flags: []string{"--format", "json", "--no-speakers", "--no-timestamps"},
			response: bareSegments, wantExt: ".json",
			wantRequired: []string{"content"},
			wantArtifact: []string{`"content": "Hello."`},
			rejectInFile: []string{"speaker", "start_time"},
		},
		{
			name: "srt forces timestamps", flags: []string{"--format", "srt", "--no-timestamps", "--no-speakers"},
			response: segments, wantExt: ".srt",
			wantRequired: []string{"content", "start_time", "end_time"},
			wantArtifact: []string{"1\n00:00:01,000 --> 00:00:03,500\nHello."},
			rejectInFile: []string{"Speaker 1:"},
		},
		{
			name: "fenced JSON is tolerated", flags: []string{"--format", "json"},
			response: "```json\n" + segments + "\n```", wantExt: ".json",
			wantRequired: []string{"content", "speaker", "start_time", "end_time"},
			wantArtifact: []string{`"content": "Hello."`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newInteractionStub(t, completedInteraction(tt.response, ""))
			audio := writeTempFile(t, "clip.mp3", "audio")
			out := filepath.Join(t.TempDir(), "out"+tt.wantExt)

			args := append(plainArgs(stub.URL), "transcribe", "-i", audio, "--out", out)
			result := runCLI(t, t.TempDir(), nil, append(args, tt.flags...)...)
			if result.err != nil {
				t.Fatalf("transcribe failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if strings.TrimSpace(result.stdout) != out {
				t.Errorf("stdout = %q, want %q", result.stdout, out)
			}

			if stub.body["stream"] != false {
				t.Errorf("stream = %v, want false", stub.body["stream"])
			}
			assertNotStored(t, stub.body)
			if _, deprecated := stub.body["response_mime_type"]; deprecated {
				t.Error("request uses the deprecated response_mime_type")
			}
			format, hasFormat := stub.body["response_format"].(map[string]any)
			if tt.wantNoSchema {
				if hasFormat {
					t.Errorf("unstructured format sent response_format: %v", format)
				}
			} else {
				if format["type"] != "text" || format["mime_type"] != "application/json" {
					t.Fatalf("response_format = %v, want a text/application/json format", format)
				}
				schema, _ := format["schema"].(map[string]any)
				props, _ := schema["properties"].(map[string]any)
				segs, _ := props["segments"].(map[string]any)
				items, _ := segs["items"].(map[string]any)
				if got := fmt.Sprint(items["required"]); got != fmt.Sprint(tt.wantRequired) {
					t.Errorf("schema required segment keys = %v, want %v", got, tt.wantRequired)
				}
			}

			parts := stub.inputParts(t)
			if parts[0]["type"] != "audio" || parts[0]["mime_type"] != "audio/mp3" {
				t.Errorf("media part = %v, want inline audio/mp3", parts[0])
			}
			prompt := fmt.Sprint(parts[len(parts)-1]["text"])
			for _, want := range tt.wantPrompt {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt missing %q:\n%s", want, prompt)
				}
			}

			artifact, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantArtifactE != "" && string(artifact) != tt.wantArtifactE {
				t.Errorf("artifact = %q, want %q", artifact, tt.wantArtifactE)
			}
			for _, want := range tt.wantArtifact {
				if !strings.Contains(string(artifact), want) {
					t.Errorf("artifact missing %q:\n%s", want, artifact)
				}
			}
			for _, reject := range tt.rejectInFile {
				if strings.Contains(string(artifact), reject) {
					t.Errorf("artifact unexpectedly contains %q:\n%s", reject, artifact)
				}
			}
		})
	}
}

// TestTranscribeRejectsUnusableTranscript pins that a structured transcript the
// CLI cannot render is a runtime error and leaves no artifact behind.
func TestTranscribeRejectsUnusableTranscript(t *testing.T) {
	tests := []struct {
		name, response, wantStderr string
	}{
		{name: "not JSON", response: "plain prose", wantStderr: "not valid JSON"},
		{name: "no segments", response: `{"segments":[]}`, wantStderr: "has no segments"},
		{name: "missing timestamps", response: `{"segments":[{"content":"hi"}]}`, wantStderr: "missing start_time/end_time"},
		{name: "bad timestamp", response: `{"segments":[{"content":"hi","start_time":"soon","end_time":"later"}]}`, wantStderr: "bad start_time"},
		{name: "out-of-range timestamp", response: `{"segments":[{"content":"hi","start_time":"00:00","end_time":"00:99"}]}`, wantStderr: "bad end_time"},
		{name: "backward end", response: `{"segments":[{"content":"hi","start_time":"00:05","end_time":"00:04"}]}`, wantStderr: "Re-run with --format json"},
		{name: "backward start", response: `{"segments":[{"content":"a","start_time":"00:05","end_time":"00:06"},{"content":"b","start_time":"00:04","end_time":"00:07"}]}`, wantStderr: "before the previous segment's"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newInteractionStub(t, completedInteraction(tt.response, ""))
			audio := writeTempFile(t, "clip.mp3", "audio")
			out := filepath.Join(t.TempDir(), "out.srt")
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "transcribe", "-i", audio, "--format", "srt", "--out", out)...)
			if code := exitCode(t, result); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(result.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", result.stderr, tt.wantStderr)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("artifact exists after a failed transcription (stat err = %v)", err)
			}
		})
	}
}

// TestMediaInputRejectedBeforeRequest pins the local input gates shared by
// analyze and transcribe: they are usage errors raised before any request.
func TestMediaInputRejectedBeforeRequest(t *testing.T) {
	oversized := filepath.Join(t.TempDir(), "big.mp4")
	big, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: one byte over the 20 MB inline cap without writing 20 MB.
	if err := big.Truncate(20<<20 + 1); err != nil {
		t.Fatal(err)
	}
	big.Close()
	empty := writeTempFile(t, "empty.mp3", "")
	unknown := writeTempFile(t, "clip.unknownext", "bytes")
	binaryText := writeTempFile(t, "notes.txt", "caf\xe9 \xff\xfe")
	audio := writeTempFile(t, "clip.mp3", "bytes")
	matroska := writeTempFile(t, "clip.mkv", "bytes")

	tests := []struct {
		name       string
		args       []string
		wantStderr []string
	}{
		{name: "analyze file over the inline cap", args: []string{"analyze", "-i", oversized},
			wantStderr: []string{"this CLI keeps inline requests under 20 MB", "gemini-api files upload"}},
		{name: "transcribe file over the inline cap", args: []string{"transcribe", "-i", oversized},
			wantStderr: []string{"this CLI keeps inline requests under 20 MB"}},
		{name: "transcribe rejects YouTube", args: []string{"transcribe", "-i", "https://youtu.be/dQw4w9WgXcQ"},
			wantStderr: []string{"YouTube URLs are not supported by transcribe"}},
		{name: "analyze rejects other remote URLs", args: []string{"analyze", "-i", "https://example.com/a.mp4"},
			wantStderr: []string{"only YouTube URLs are supported as remote inputs"}},
		{name: "empty file", args: []string{"transcribe", "-i", empty}, wantStderr: []string{"file is empty"}},
		{name: "unknown MIME type", args: []string{"analyze", "-i", unknown}, wantStderr: []string{"cannot determine the MIME type", "--mime-type"}},
		{name: "MIME type outside the content enums", args: []string{"analyze", "-i", audio, "--mime-type", "audio/amr"},
			wantStderr: []string{"MIME type audio/amr is not accepted by the Interactions API", "Supported inputs"}},
		{name: "upload-only container", args: []string{"transcribe", "-i", matroska},
			wantStderr: []string{"MIME type video/x-matroska is not accepted by the Interactions API"}},
		{name: "document type other than PDF and CSV", args: []string{"analyze", "-i", audio, "--mime-type", "application/rtf"},
			wantStderr: []string{"MIME type application/rtf is not accepted by the Interactions API"}},
		{name: "text file that is not UTF-8", args: []string{"analyze", "-i", binaryText},
			wantStderr: []string{"detected as text/plain but the content is not UTF-8 text", "--mime-type"}},
		{name: "malformed files reference", args: []string{"analyze", "-i", "files/a/../b"}, wantStderr: []string{"invalid Files API reference"}},
		{name: "invalid transcribe format", args: []string{"transcribe", "-i", empty, "--format", "docx"}, wantStderr: []string{`invalid --format "docx"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newInteractionStub(t, completedInteraction("unreachable", ""))
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), tt.args...)...)
			if code := exitCode(t, result); code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(result.stderr, want) {
					t.Errorf("stderr missing %q:\n%s", want, result.stderr)
				}
			}
			if got := stub.requests.Load(); got != 0 {
				t.Errorf("reached the server %d times, want 0", got)
			}
		})
	}
}

// TestAnalyzeTextFileTravelsAsText pins that textual inputs are sent as text
// blocks: the document block's mime_type enum only admits PDF and CSV.
func TestAnalyzeTextFileTravelsAsText(t *testing.T) {
	for name, want := range map[string]struct{ blockType, mime string }{
		"notes.md":   {"text", ""},
		"data.json":  {"text", ""},
		"main.go":    {"text", ""},
		"table.csv":  {"document", "text/csv"},
		"report.pdf": {"document", "application/pdf"},
	} {
		t.Run(name, func(t *testing.T) {
			const content = "line one\nline two — ünïcode\n"
			stub := newInteractionStub(t, completedInteraction("ok", ""))
			input := writeTempFile(t, name, content)
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "analyze", "-i", input, "summarize")...)
			if result.err != nil {
				t.Fatalf("analyze failed: %v\nstderr: %s", result.err, result.stderr)
			}
			parts := stub.inputParts(t)
			if len(parts) != 2 || parts[1]["text"] != "summarize" {
				t.Fatalf("input = %#v, want the file block then the question", parts)
			}
			part := parts[0]
			if part["type"] != want.blockType {
				t.Fatalf("file block type = %v, want %s: %#v", part["type"], want.blockType, part)
			}
			if want.blockType == "text" {
				if part["text"] != content || part["mime_type"] != nil || part["data"] != nil {
					t.Errorf("text block = %#v, want the verbatim file content and nothing else", part)
				}
				return
			}
			if part["mime_type"] != want.mime || part["data"] != base64.StdEncoding.EncodeToString([]byte(content)) {
				t.Errorf("document block = %#v, want base64 data with mime_type %s", part, want.mime)
			}
		})
	}
}

// TestAnalyzeTextFileDryRunRedactsContent pins that a text input is redacted
// in the dry-run preview like any other payload.
func TestAnalyzeTextFileDryRunRedactsContent(t *testing.T) {
	stub := newInteractionStub(t, completedInteraction("unreachable", ""))
	input := writeTempFile(t, "secret.txt", "do not print me")
	result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "--dry-run", "analyze", "-i", input)...)
	if result.err != nil {
		t.Fatalf("analyze --dry-run failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if strings.Contains(result.stderr, "do not print me") || !strings.Contains(result.stderr, `"text": "<bytes:15>"`) {
		t.Errorf("dry-run preview did not redact the text input:\n%s", result.stderr)
	}
	if got := stub.requests.Load(); got != 0 {
		t.Errorf("reached the server %d times, want 0", got)
	}
}

// TestMediaMIMEMatchesInteractionsEnum pins that video containers are sent
// with the interactions mime_type spellings rather than the IANA ones.
func TestMediaMIMEMatchesInteractionsEnum(t *testing.T) {
	for ext, want := range map[string]string{".mov": "video/mov", ".avi": "video/avi", ".wmv": "video/wmv", ".mp4": "video/mp4"} {
		t.Run(ext, func(t *testing.T) {
			stub := newInteractionStub(t, completedInteraction("ok", ""))
			clip := writeTempFile(t, "clip"+ext, "bytes")
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "analyze", "-i", clip)...)
			if result.err != nil {
				t.Fatalf("analyze failed: %v\nstderr: %s", result.err, result.stderr)
			}
			part := stub.inputParts(t)[0]
			if part["type"] != "video" || part["mime_type"] != want {
				t.Errorf("media part type/mime = %v/%v, want video/%s", part["type"], part["mime_type"], want)
			}
		})
	}
}
