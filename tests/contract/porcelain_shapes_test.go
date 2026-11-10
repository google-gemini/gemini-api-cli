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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/cli"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/output"
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

// inputParts returns the content blocks inside the request's single user_input step.
func (s *interactionStub) inputParts(t *testing.T) []map[string]any {
	t.Helper()
	parts := assertUserInputStepParts(t, s.body)
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

// annotatedTranscript is a gemini-3.5-transcribe response: the transcript text
// plus word_info annotations whose byte indices point into it. The word texts
// carry no punctuation, so the artifact only has it if segments are cut from
// the response text.
const annotatedTranscript = `{
	"id": "interactions/tx-1",
	"status": "completed",
	"steps": [{
		"type": "model_output",
		"content": [{
			"type": "text",
			"text": "Hello world. Hi there!",
			"annotations": [
				{"type": "word_info", "text": "Hello", "speaker": "spk:0", "start_offset": "1s", "end_offset": "1.4s", "start_index": 0, "end_index": 5},
				{"type": "word_info", "text": "world", "speaker": "spk:0", "start_offset": "1.5s", "end_offset": "3.5s", "start_index": 6, "end_index": 11},
				{"type": "word_info", "text": "Hi", "speaker": "spk:1", "start_offset": "4s", "end_offset": "4.3s", "start_index": 13, "end_index": 15},
				{"type": "word_info", "text": "there", "speaker": "spk:1", "start_offset": "4.4s", "end_offset": "5s", "start_index": 16, "end_index": 21}
			]
		}]
	}]
}`

// wordInfoInteraction builds a completed transcription response from raw
// word_info annotation JSON objects.
func wordInfoInteraction(text string, words ...string) string {
	return fmt.Sprintf(`{"id":"interactions/tx","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":%q,"annotations":[%s]}]}]}`,
		text, strings.Join(words, ","))
}

// TestTranscribeDedicatedModelRequestAndArtifact pins the default
// gemini-3.5-transcribe path: audio-only input block (no text prompt),
// generation_config.transcription_config, word_info annotation formatting across
// md/text/json/srt, and automatic fallback to gemini-3.8-flash for video inputs.
func TestTranscribeDedicatedModelRequestAndArtifact(t *testing.T) {
	tests := []struct {
		name              string
		flags             []string
		response          string
		wantExt           string
		wantDiarization   string
		wantGranularities string // fmt.Sprint of the request field
		wantArtifact      []string
		rejectInFile      []string
		wantExactArtifact string
	}{
		{
			name:              "md default",
			response:          annotatedTranscript,
			wantExt:           ".md",
			wantDiarization:   "speaker",
			wantGranularities: "[word]",
			wantExactArtifact: "**[00:01 - 00:03.5] Speaker 1:** Hello world.\n\n**[00:04 - 00:05] Speaker 2:** Hi there!\n",
		},
		{
			name:              "md without timestamps",
			flags:             []string{"--no-timestamps"},
			response:          annotatedTranscript,
			wantExt:           ".md",
			wantDiarization:   "speaker",
			wantGranularities: "<nil>",
			wantExactArtifact: "**Speaker 1:** Hello world.\n\n**Speaker 2:** Hi there!\n",
		},
		{
			name:              "text with speakers and timestamps",
			flags:             []string{"--format", "text"},
			response:          annotatedTranscript,
			wantExt:           ".txt",
			wantDiarization:   "speaker",
			wantGranularities: "[word]",
			wantExactArtifact: "[00:01 - 00:03.5] Speaker 1: Hello world.\n[00:04 - 00:05] Speaker 2: Hi there!\n",
		},
		{
			name:              "text without speakers",
			flags:             []string{"--format", "text", "--no-speakers"},
			response:          annotatedTranscript,
			wantExt:           ".txt",
			wantGranularities: "[word]",
			wantExactArtifact: "[00:01 - 00:03.5] Hello world.\n[00:04 - 00:05] Hi there!\n",
		},
		{
			name:              "text without speakers or timestamps",
			flags:             []string{"--format", "text", "--no-speakers", "--no-timestamps"},
			response:          completedInteraction("Hello.", ""),
			wantExt:           ".txt",
			wantGranularities: "<nil>",
			wantExactArtifact: "Hello.\n",
		},
		{
			name:              "json",
			flags:             []string{"--format", "json"},
			response:          annotatedTranscript,
			wantExt:           ".json",
			wantDiarization:   "speaker",
			wantGranularities: "[word]",
			wantArtifact:      []string{`"speaker": "Speaker 1"`, `"start_time": "00:01"`, `"end_time": "00:03.5"`, `"content": "Hello world."`},
		},
		{
			name:              "json without speakers or timestamps",
			flags:             []string{"--format", "json", "--no-speakers", "--no-timestamps"},
			response:          completedInteraction("Hello.", ""),
			wantExt:           ".json",
			wantGranularities: "<nil>",
			wantArtifact:      []string{`"content": "Hello."`},
			rejectInFile:      []string{"speaker", "start_time"},
		},
		{
			name:              "srt forces timestamps and respects no-speakers",
			flags:             []string{"--format", "srt", "--no-timestamps", "--no-speakers"},
			response:          annotatedTranscript,
			wantExt:           ".srt",
			wantGranularities: "[word]",
			wantExactArtifact: "1\n00:00:01,000 --> 00:00:03,500\nHello world.\n\n2\n00:00:04,000 --> 00:00:05,000\nHi there!\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newInteractionStub(t, tt.response)
			audio := writeTempFile(t, "clip.mp3", "audio")
			out := filepath.Join(t.TempDir(), "out"+tt.wantExt)

			args := append(plainArgs(stub.URL), "transcribe", "-i", audio, "--out", out)
			result := runCLI(t, t.TempDir(), nil, append(args, tt.flags...)...)
			if result.err != nil {
				t.Fatalf("transcribe failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if stub.body["model"] != "gemini-3.5-transcribe" {
				t.Errorf("model = %v, want gemini-3.5-transcribe", stub.body["model"])
			}
			assertNotStored(t, stub.body)
			if _, hasFormat := stub.body["response_format"]; hasFormat {
				t.Errorf("dedicated transcribe model sent response_format: %v", stub.body["response_format"])
			}
			parts := stub.inputParts(t)
			if len(parts) != 1 || parts[0]["type"] != "audio" || parts[0]["mime_type"] != "audio/mp3" {
				t.Errorf("input parts = %v, want a single audio/mp3 block", parts)
			}
			genCfg, _ := stub.body["generation_config"].(map[string]any)
			txCfg, _ := genCfg["transcription_config"].(map[string]any)
			mode, _ := txCfg["mode"].(map[string]any)
			if mode["type"] != "verbatim" {
				t.Fatalf("transcription_config.mode = %v, want a verbatim mode object", mode)
			}
			if got, _ := mode["diarization_mode"].(string); got != tt.wantDiarization {
				t.Errorf("diarization_mode = %q, want %q", got, tt.wantDiarization)
			}
			if got := fmt.Sprint(mode["timestamp_granularities"]); got != tt.wantGranularities {
				t.Errorf("timestamp_granularities = %s, want %s", got, tt.wantGranularities)
			}

			artifact, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantExactArtifact != "" && string(artifact) != tt.wantExactArtifact {
				t.Errorf("artifact = %q, want %q", artifact, tt.wantExactArtifact)
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

	t.Run("video input falls back to gemini-3.8-flash when model is not explicitly set", func(t *testing.T) {
		stub := newInteractionStub(t, completedInteraction("# Video transcript", ""))
		video := writeTempFile(t, "clip.mp4", "video")
		out := filepath.Join(t.TempDir(), "out.md")

		result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "--output-format", "json", "transcribe", "-i", video, "--out", out)...)
		if result.err != nil {
			t.Fatalf("transcribe video failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if stub.body["model"] != "gemini-3.8-flash" {
			t.Errorf("model = %v, want the gemini-3.8-flash fallback for video", stub.body["model"])
		}
		parts := stub.inputParts(t)
		if len(parts) != 2 || parts[0]["type"] != "video" || parts[1]["type"] != "text" {
			t.Errorf("input parts = %v, want video + text prompt", parts)
		}
		var envelope struct {
			Model   string `json:"model"`
			Results []struct {
				Model string `json:"model"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
			t.Fatalf("decode envelope: %v\n%s", err, result.stdout)
		}
		if envelope.Model != "gemini-3.8-flash" || len(envelope.Results) != 1 || envelope.Results[0].Model != "gemini-3.8-flash" {
			t.Errorf("envelope = %+v, want gemini-3.8-flash at the top level and per result", envelope)
		}
	})

	t.Run("mixed audio and video report the model per input", func(t *testing.T) {
		var models []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			model, _ := body["model"].(string)
			models = append(models, model)
			w.Header().Set("Content-Type", "application/json")
			if model == "gemini-3.5-transcribe" {
				fmt.Fprint(w, annotatedTranscript)
				return
			}
			fmt.Fprint(w, completedInteraction("# Video transcript", ""))
		}))
		defer server.Close()
		audio := writeTempFile(t, "a.mp3", "audio")
		video := writeTempFile(t, "b.mp4", "video")
		out := t.TempDir()

		result := runCLI(t, t.TempDir(), nil, append(plainArgs(server.URL), "--output-format", "json", "transcribe", "-i", audio, "-i", video, "--out", out+"/")...)
		if result.err != nil {
			t.Fatalf("transcribe failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if got := fmt.Sprint(models); got != "[gemini-3.5-transcribe gemini-3.8-flash]" {
			t.Errorf("request models = %s, want [gemini-3.5-transcribe gemini-3.8-flash]", got)
		}
		var envelope struct {
			Model   string `json:"model"`
			Results []struct {
				Model string `json:"model"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
			t.Fatalf("decode envelope: %v\n%s", err, result.stdout)
		}
		if envelope.Model != "gemini-3.5-transcribe" || len(envelope.Results) != 2 ||
			envelope.Results[0].Model != "gemini-3.5-transcribe" || envelope.Results[1].Model != "gemini-3.8-flash" {
			t.Errorf("envelope = %+v, want per-result models [gemini-3.5-transcribe gemini-3.8-flash]", envelope)
		}
	})

	t.Run("explicit transcribe model rejects video input before sending a request", func(t *testing.T) {
		stub := newInteractionStub(t, completedInteraction("unreachable", ""))
		video := writeTempFile(t, "clip.mp4", "video")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "transcribe", "-m", "gemini-3.5-transcribe", "-i", video)...)
		if code := exitCode(t, result); code != 2 {
			t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
		}
		if !strings.Contains(result.stderr, "only accepts audio inputs") {
			t.Errorf("stderr = %q, want it to explain the audio-only restriction", result.stderr)
		}
		if stub.requests.Load() != 0 {
			t.Errorf("sent %d requests, want 0", stub.requests.Load())
		}
	})
}

// TestTranscribeDedicatedModelRejectsUnusableAnnotations pins that word_info
// the CLI cannot render is a runtime error that leaves no artifact behind.
func TestTranscribeDedicatedModelRejectsUnusableAnnotations(t *testing.T) {
	tests := []struct {
		name, format, response, wantStderr string
	}{
		{name: "no word_info for srt", format: "srt", response: completedInteraction("Hello there.", ""),
			wantStderr: "returned no word-level annotations"},
		{name: "no word_info for json", format: "json", response: completedInteraction("Hello there.", ""),
			wantStderr: "returned no word-level annotations"},
		{name: "missing end_offset", format: "srt",
			response:   wordInfoInteraction("hi", `{"type":"word_info","text":"hi","speaker":"spk:0","start_offset":"1s"}`),
			wantStderr: "word 1 is missing start_offset/end_offset"},
		{name: "bad offset", format: "json",
			response:   wordInfoInteraction("hi", `{"type":"word_info","text":"hi","speaker":"spk:0","start_offset":"soon","end_offset":"2s"}`),
			wantStderr: `word 1: bad start_offset "soon"`},
		{name: "backward cue start", format: "srt",
			response: wordInfoInteraction("a b",
				`{"type":"word_info","text":"a","speaker":"spk:0","start_offset":"5s","end_offset":"6s"}`,
				`{"type":"word_info","text":"b","speaker":"spk:1","start_offset":"4s","end_offset":"7s"}`),
			wantStderr: "Re-run with --format json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newInteractionStub(t, tt.response)
			audio := writeTempFile(t, "clip.mp3", "audio")
			out := filepath.Join(t.TempDir(), "out."+tt.format)
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "transcribe", "-i", audio, "--format", tt.format, "--out", out)...)
			if code := exitCode(t, result); code != 1 {
				t.Errorf("exit code = %d, want 1\nstderr: %s", code, result.stderr)
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

// TestTranscribeDedicatedModelEdgeCases pins the transcribe-model behaviors
// around a usable response: non-completed interactions, missing word_info for
// md, the 30-minute hint on a rejected request, the WebM hint on the video
// fallback, and the video fallback for structured formats.
func TestTranscribeDedicatedModelEdgeCases(t *testing.T) {
	t.Run("non-completed interaction is a runtime error", func(t *testing.T) {
		response := strings.Replace(annotatedTranscript, `"status": "completed"`, `"status": "incomplete"`, 1)
		stub := newInteractionStub(t, response)
		audio := writeTempFile(t, "clip.mp3", "audio")
		out := filepath.Join(t.TempDir(), "out.md")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "transcribe", "-i", audio, "--out", out)...)
		if code := exitCode(t, result); code != 1 {
			t.Errorf("exit code = %d, want 1\nstderr: %s", code, result.stderr)
		}
		if !strings.Contains(result.stderr, "status: incomplete") {
			t.Errorf("stderr = %q, want it to name the incomplete status", result.stderr)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("artifact exists after an incomplete interaction (stat err = %v)", err)
		}
	})

	t.Run("md without word_info warns that speakers and timestamps are missing", func(t *testing.T) {
		stub := newInteractionStub(t, completedInteraction("Hello there.", ""))
		audio := writeTempFile(t, "clip.mp3", "audio")
		out := filepath.Join(t.TempDir(), "out.md")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "transcribe", "-i", audio, "--out", out)...)
		if result.err != nil {
			t.Fatalf("transcribe failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if !strings.Contains(result.stderr, "returned no word-level annotations") {
			t.Errorf("stderr = %q, want a warning about the missing annotations", result.stderr)
		}
		if artifact, _ := os.ReadFile(out); string(artifact) != "Hello there.\n" {
			t.Errorf("artifact = %q, want the plain transcript", artifact)
		}
	})

	t.Run("a rejected request with speakers or timestamps hints at the 30-minute cap", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`)
		}))
		defer server.Close()
		audio := writeTempFile(t, "long.mp3", "audio")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(server.URL), "transcribe", "-i", audio)...)
		if code := exitCode(t, result); code == 0 {
			t.Fatalf("transcribe unexpectedly succeeded\nstderr: %s", result.stderr)
		}
		if !strings.Contains(result.stderr, "--no-speakers --no-timestamps") {
			t.Errorf("stderr = %q, want the 30-minute remediation hint", result.stderr)
		}
	})

	t.Run("audio-only WebM is pointed at --mime-type audio/webm", func(t *testing.T) {
		stub := newInteractionStub(t, completedInteraction("# Transcript", ""))
		webm := writeTempFile(t, "voice.webm", "webm")
		out := filepath.Join(t.TempDir(), "out.md")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "transcribe", "-i", webm, "--out", out)...)
		if result.err != nil {
			t.Fatalf("transcribe failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if stub.body["model"] != "gemini-3.8-flash" || !strings.Contains(result.stderr, "--mime-type audio/webm") {
			t.Errorf("model = %v, stderr = %q; want the gemini-3.8-flash fallback with the WebM hint", stub.body["model"], result.stderr)
		}

		explicit := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "transcribe", "-m", "gemini-3.5-transcribe", "-i", webm)...)
		if code := exitCode(t, explicit); code != 2 || !strings.Contains(explicit.stderr, "--mime-type audio/webm") {
			t.Errorf("explicit model on .webm: exit %d, stderr %q; want a usage error with the WebM hint", code, explicit.stderr)
		}

		override := newInteractionStub(t, annotatedTranscript)
		result = runCLI(t, t.TempDir(), nil, append(plainArgs(override.URL), "transcribe", "-i", webm, "--mime-type", "audio/webm", "--out", out)...)
		if result.err != nil {
			t.Fatalf("transcribe with --mime-type audio/webm failed: %v\nstderr: %s", result.err, result.stderr)
		}
		parts := override.inputParts(t)
		if override.body["model"] != "gemini-3.5-transcribe" || parts[0]["type"] != "audio" || parts[0]["mime_type"] != "audio/webm" {
			t.Errorf("model = %v, parts = %v; want gemini-3.5-transcribe with an audio/webm block", override.body["model"], parts)
		}
	})

	t.Run("dry-run notes that a files/<id> video would use gemini-3.8-flash", func(t *testing.T) {
		result := runCLI(t, t.TempDir(), nil, append(plainArgs("https://example.invalid"), "--dry-run", "transcribe", "-i", "files/abc123")...)
		if result.err != nil {
			t.Fatalf("dry-run failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if !strings.Contains(result.stderr, "if this file is a video, the live run uses gemini-3.8-flash") {
			t.Errorf("stderr = %q, want the dry-run routing note", result.stderr)
		}
	})

	t.Run("video fallback keeps the structured srt path", func(t *testing.T) {
		const segments = `{"segments":[{"speaker":"Speaker 1","start_time":"00:00","end_time":"00:02","content":"Hello."}]}`
		stub := newInteractionStub(t, completedInteraction(segments, ""))
		video := writeTempFile(t, "clip.mp4", "video")
		out := filepath.Join(t.TempDir(), "out.srt")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "transcribe", "-i", video, "--format", "srt", "--out", out)...)
		if result.err != nil {
			t.Fatalf("transcribe failed: %v\nstderr: %s", result.err, result.stderr)
		}
		format, _ := stub.body["response_format"].(map[string]any)
		if stub.body["model"] != "gemini-3.8-flash" || format["mime_type"] != "application/json" {
			t.Errorf("model = %v, response_format = %v; want gemini-3.8-flash with the JSON schema", stub.body["model"], format)
		}
		if artifact, _ := os.ReadFile(out); !strings.Contains(string(artifact), "1\n00:00:00,000 --> 00:00:02,000\nSpeaker 1: Hello.") {
			t.Errorf("artifact = %q, want an SRT cue from the structured segments", artifact)
		}
	})
}

// TestModelsCatalogListsTranscribe pins gemini-3.5-transcribe in the offline
// model catalog, under Speech.
func TestModelsCatalogListsTranscribe(t *testing.T) {
	human := runCLI(t, t.TempDir(), nil, "models")
	if human.err != nil {
		t.Fatalf("models failed: %v\nstderr: %s", human.err, human.stderr)
	}
	speech := human.stdout[strings.Index(human.stdout, "Speech:"):]
	if i := strings.Index(speech, "\n\n"); i >= 0 {
		speech = speech[:i]
	}
	if !strings.Contains(speech, "gemini-3.5-transcribe") {
		t.Errorf("Speech group does not list gemini-3.5-transcribe:\n%s", human.stdout)
	}

	machine := runCLI(t, t.TempDir(), nil, "models", "--output-format", "json")
	if machine.err != nil {
		t.Fatalf("models --output-format json failed: %v\nstderr: %s", machine.err, machine.stderr)
	}
	if !strings.Contains(machine.stdout, `"gemini-3.5-transcribe"`) || !strings.Contains(machine.stdout, `"Speech"`) {
		t.Errorf("JSON catalog is missing gemini-3.5-transcribe in Speech:\n%s", machine.stdout)
	}
}

// TestTranscribeRequestAndArtifact pins, per --format and the speaker/timestamp
// switches on non-transcribe multimodal models: whether the request enforces
// the transcript JSON schema through response_format, which segment keys the
// schema requires, what the prompt asks for, and the artifact written.
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

			args := append(plainArgs(stub.URL), "transcribe", "-m", "gemini-3.8-flash", "-i", audio, "--out", out)
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

// TestTranscribeRejectsUnusableTranscript pins that a structured transcript a
// general model returns which the CLI cannot render is a runtime error and
// leaves no artifact behind.
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
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "transcribe", "-m", "gemini-3.8-flash", "-i", audio, "--format", "srt", "--out", out)...)
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

func executeRealRoot(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	clearAgentModeEnvironment(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GEMINI_API_VERSION", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_ACCESS_TOKEN", "")
	config.Reset()
	output.ResetAgentMode()
	t.Cleanup(func() {
		config.Reset()
		output.ResetAgentMode()
	})
	root, err := cli.NewRootCommand()
	if err != nil {
		t.Fatalf("NewRootCommand: %v", err)
	}
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(stdin))
	err = cli.ExecuteRoot(context.Background(), root, args)
	return stdout.String(), stderr.String(), err
}

func assertUserInputStepParts(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	steps, ok := body["input"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("request input = %#v, want a single-element step array [{\"type\":\"user_input\",\"content\":[...]}]", body["input"])
	}
	step, ok := steps[0].(map[string]any)
	if !ok || step["type"] != "user_input" {
		t.Fatalf("request input[0] = %#v, want a user_input step object", steps[0])
	}
	rawContent, ok := step["content"].([]any)
	if !ok {
		t.Fatalf("request input[0].content = %#v, want a content block array", step["content"])
	}
	parts := make([]map[string]any, 0, len(rawContent))
	for i, item := range rawContent {
		part, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("request input[0].content[%d] = %#v, want a JSON object", i, item)
		}
		parts = append(parts, part)
	}
	return parts
}

// TestDefaultV1MediaRequestsUseUserInputStep pins issue #177:
// with the real root command and no --api-version override or ambient version
// configuration, convenience media/text builders route to /v1/interactions and
// wrap content blocks in a user_input step rather than a bare content array.
func TestDefaultV1MediaRequestsUseUserInputStep(t *testing.T) {
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNgYAAAAAMAAVCiT19+AAAAAElFTkSuQmCC"
	dir := t.TempDir()
	imgFile := writeTempFile(t, "sample.png", "png-bytes")
	audioFile := writeTempFile(t, "sample.mp3", "mp3-bytes")
	videoFile := writeTempFile(t, "sample.mp4", "mp4-bytes")
	notesFile := writeTempFile(t, "notes.txt", "notes from file")
	wantImgB64 := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	wantAudioB64 := base64.StdEncoding.EncodeToString([]byte("mp3-bytes"))
	wantVideoB64 := base64.StdEncoding.EncodeToString([]byte("mp4-bytes"))

	type capturedReq struct {
		path string
		body map[string]any
	}
	var lastReq capturedReq
	var reqCount atomic.Int32

	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/interactions/int-vid") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"int-vid","status":"completed","model":"gemini-omni-1.1-flash","steps":[{"type":"model_output","content":[{"type":"video","data":"%s","mime_type":"video/mp4"}]}]}`, wantVideoB64)
			return
		}
		reqCount.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("request body is not JSON: %v\n%s", err, raw)
			}
		}
		lastReq = capturedReq{path: r.URL.Path, body: body}

		// Emulate the v1 server validation from issue #177 across all convenience
		// requests: top-level input arrays must contain step objects, never bare content blocks.
		if arr, ok := body["input"].([]any); ok {
			for i, elem := range arr {
				if m, ok := elem.(map[string]any); ok {
					switch m["type"] {
					case "text", "image", "audio", "video", "document":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprintf(w, `{"error":{"code":400,"message":"input[%d].type=%v is unavailable in v1 and available in v1beta","status":"INVALID_ARGUMENT"}}`, i, m["type"])
						return
					}
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		rf, _ := body["response_format"].(map[string]any)
		switch {
		case rf["type"] == "audio":
			_, _ = io.WriteString(w, ttsAudioResponse([]byte{0x01, 0x02}, "audio/l16", 24000, 1))
		case rf["type"] == "image":
			fmt.Fprintf(w, `{"id":"int-img","status":"completed","model":"gemini-nano-banana-2.1","steps":[{"type":"model_output","content":[{"type":"image","data":"%s","mime_type":"image/png"}]}]}`, pngB64)
		case rf["type"] == "video":
			fmt.Fprintf(w, `{"id":"int-vid","status":"completed","model":"gemini-omni-1.1-flash","steps":[{"type":"model_output","content":[{"type":"video","data":"%s","mime_type":"video/mp4"}]}]}`, wantVideoB64)
		case rf["schema"] != nil:
			_, _ = io.WriteString(w, completedInteraction(`{"segments":[{"speaker":"Speaker 1","start_time":"00:00","end_time":"00:01","content":"hello world"}]}`, ""))
		default:
			_, _ = io.WriteString(w, completedInteraction("ok response", ""))
		}
	}))
	defer server.Close()
	serverURL = server.URL

	defaultRootArgs := func(extra ...string) []string {
		base := []string{"--server-url", serverURL, "--no-interactive", "--no-retries", "--color", "never", "--api-key", "test-key"}
		return append(base, extra...)
	}

	// 1. Exact reported reproduction from issue #177 via NewRootCommand (no --api-version flag)
	// and via explicit --api-version v1 and --api-version v1beta.
	for _, tc := range []struct {
		name     string
		verArgs  []string
		wantPath string
	}{
		{name: "default (no --api-version flag)", verArgs: nil, wantPath: "/v1beta/interactions"},
		{name: "explicit --api-version v1", verArgs: []string{"--api-version", "v1"}, wantPath: "/v1/interactions"},
		{name: "explicit --api-version v1beta", verArgs: []string{"--api-version", "v1beta"}, wantPath: "/v1beta/interactions"},
	} {
		t.Run("analyze YouTube "+tc.name, func(t *testing.T) {
			args := append(defaultRootArgs(tc.verArgs...),
				"analyze",
				"-i", "https://www.youtube.com/watch?v=rRJ7Jhun12I",
				"Transcribe the spoken audio verbatim.",
				"--output-format", "json",
			)
			stdout, stderr, err := executeRealRoot(t, "", args...)
			if err != nil {
				t.Fatalf("executeRealRoot(%v) failed: %v\nstderr: %s", args, err, stderr)
			}
			if lastReq.path != tc.wantPath {
				t.Errorf("request path = %q, want %q", lastReq.path, tc.wantPath)
			}
			assertNotStored(t, lastReq.body)
			if lastReq.body["stream"] != false {
				t.Errorf("stream = %v, want false", lastReq.body["stream"])
			}
			parts := assertUserInputStepParts(t, lastReq.body)
			wantParts := []map[string]any{
				{"type": "video", "uri": "https://www.youtube.com/watch?v=rRJ7Jhun12I"},
				{"type": "text", "text": "Transcribe the spoken audio verbatim."},
			}
			if !reflect.DeepEqual(parts, wantParts) {
				t.Errorf("user_input content = %#v, want %#v", parts, wantParts)
			}
			if !strings.Contains(stdout, "ok response") {
				t.Errorf("stdout = %q, want response text", stdout)
			}
		})
	}

	// 2. Also verify subprocess invocation via runCLI with explicit --api-version v1.
	t.Run("subprocess analyze local video with v1 routing", func(t *testing.T) {
		res := runCLI(t, t.TempDir(), nil, defaultRootArgs("--api-version", "v1", "analyze", "-i", videoFile, "Describe this clip.")...)
		if res.err != nil {
			t.Fatalf("runCLI analyze failed: %v\nstderr: %s", res.err, res.stderr)
		}
		if lastReq.path != "/v1/interactions" {
			t.Errorf("request path = %q, want /v1/interactions", lastReq.path)
		}
		parts := assertUserInputStepParts(t, lastReq.body)
		wantParts := []map[string]any{
			{"type": "video", "mime_type": "video/mp4", "data": wantVideoB64},
			{"type": "text", "text": "Describe this clip."},
		}
		if !reflect.DeepEqual(parts, wantParts) {
			t.Errorf("user_input content = %#v, want %#v", parts, wantParts)
		}
	})

	// 3. Shared-helper callers: transcribe (default diarized & structured output), tts,
	// generate, image, video, music, agent run — all with no --api-version override.
	callerCases := []struct {
		name      string
		stdin     string
		args      []string
		wantParts []map[string]any
		checkBody func(t *testing.T, body map[string]any)
	}{
		{
			name: "transcribe default diarized",
			args: []string{"transcribe", "-i", audioFile, "--out", filepath.Join(dir, "transcript.md")},
			wantParts: []map[string]any{
				{"type": "audio", "mime_type": "audio/mp3", "data": wantAudioB64},
			},
			checkBody: func(t *testing.T, body map[string]any) {
				assertNotStored(t, body)
				gc, _ := body["generation_config"].(map[string]any)
				if _, ok := gc["transcription_config"].(map[string]any); !ok {
					t.Errorf("generation_config = %#v, want transcription_config", gc)
				}
			},
		},
		{
			name: "transcribe structured json output on general model",
			args: []string{"transcribe", "-m", "gemini-3.8-flash", "-i", audioFile, "--format", "json", "--out", filepath.Join(dir, "transcript.json")},
			wantParts: []map[string]any{
				{"type": "audio", "mime_type": "audio/mp3", "data": wantAudioB64},
				{"type": "text", "text": "Transcribe the speech in this recording accurately and completely. Identify and label distinct speakers (Speaker 1, Speaker 2, ...). Include accurate timestamps (mm:ss or hh:mm:ss) for each segment. Return JSON matching the response schema: one segment per utterance."},
			},
			checkBody: func(t *testing.T, body map[string]any) {
				assertNotStored(t, body)
				rf, _ := body["response_format"].(map[string]any)
				if rf["type"] != "text" || rf["schema"] == nil {
					t.Errorf("response_format = %#v, want type=text with schema", rf)
				}
			},
		},
		{
			name: "tts single speaker",
			args: []string{"tts", "Hello from v1", "--out", filepath.Join(dir, "tts.wav")},
			wantParts: []map[string]any{
				{"type": "text", "text": "Hello from v1"},
			},
			checkBody: func(t *testing.T, body map[string]any) {
				assertNotStored(t, body)
				assertAudioResponseFormat(t, body)
			},
		},
		{
			name: "generate with local image and prompt",
			args: []string{"generate", "-i", imgFile, "Describe this image", "--stream=false"},
			wantParts: []map[string]any{
				{"type": "image", "mime_type": "image/png", "data": wantImgB64},
				{"type": "text", "text": "Describe this image"},
			},
		},
		{
			name:  "generate with stdin and prompt",
			stdin: "stdin context\n",
			args:  []string{"generate", "--stdin", "Summarize", "--stream=false"},
			wantParts: []map[string]any{
				{"type": "text", "text": "stdin context"},
				{"type": "text", "text": "Summarize"},
			},
		},
		{
			name: "image with input image and prompt",
			args: []string{"image", "-i", imgFile, "Make it watercolor", "--out", filepath.Join(dir, "out.png")},
			wantParts: []map[string]any{
				{"type": "image", "mime_type": "image/png", "data": wantImgB64},
				{"type": "text", "text": "Make it watercolor"},
			},
		},
		{
			name: "video with input image and prompt",
			args: []string{"video", "-i", imgFile, "Animate the clouds", "--out", filepath.Join(dir, "out.mp4")},
			wantParts: []map[string]any{
				{"type": "image", "mime_type": "image/png", "data": wantImgB64},
				{"type": "text", "text": "Animate the clouds"},
			},
		},
		{
			name: "music with file and prompt",
			args: []string{"music", "-f", notesFile, "upbeat synthwave", "--out", filepath.Join(dir, "out.wav")},
			wantParts: []map[string]any{
				{"type": "text", "text": "notes from file"},
				{"type": "text", "text": "upbeat synthwave"},
			},
		},
		{
			name: "agent run with media input and prompt",
			args: []string{"agent", "run", "-i", imgFile, "Inspect this diagram", "--model", "gemini-3.6-flash", "--stream=false"},
			wantParts: []map[string]any{
				{"type": "image", "mime_type": "image/png", "data": wantImgB64},
				{"type": "text", "text": "Inspect this diagram"},
			},
		},
	}

	for _, tc := range callerCases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--api-version", "v1"}, tc.args...)
			_, stderr, err := executeRealRoot(t, tc.stdin, defaultRootArgs(args...)...)
			if err != nil {
				t.Fatalf("executeRealRoot(%v) failed: %v\nstderr: %s", args, err, stderr)
			}
			if lastReq.path != "/v1/interactions" {
				t.Errorf("request path = %q, want /v1/interactions", lastReq.path)
			}
			parts := assertUserInputStepParts(t, lastReq.body)
			if !reflect.DeepEqual(parts, tc.wantParts) {
				t.Errorf("user_input content = %#v, want %#v", parts, tc.wantParts)
			}
			if tc.checkBody != nil {
				tc.checkBody(t, lastReq.body)
			}
		})
	}
}

// TestIntentBodyAndInputMergeSemantics pins raw --body preservation when no
// convenience transformation is requested, unambiguous --body + --input merging
// into user_input steps, preservation of step ordering/roles, and clear usage
// errors on mixed or ambiguous step combinations.
func TestIntentBodyAndInputMergeSemantics(t *testing.T) {
	imgFile := writeTempFile(t, "merge.png", "png-bytes")
	wantImgBlock := map[string]any{
		"type":      "image",
		"mime_type": "image/png",
		"data":      base64.StdEncoding.EncodeToString([]byte("png-bytes")),
	}

	var gotPath string
	var gotBody map[string]any
	var reqCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = nil
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completedInteraction("ok", ""))
	}))
	defer server.Close()

	base := func(extra ...string) []string {
		return append([]string{"--server-url", server.URL, "--no-interactive", "--no-retries", "--color", "never", "--api-key", "test-key"}, extra...)
	}

	// 1. Raw --body without convenience inputs preserves input shape as-is.
	rawCases := []struct {
		name      string
		cmdArgs   []string
		wantInput any
	}{
		{
			name:      "raw string input preserved",
			cmdArgs:   []string{"generate", "--stream=false", "--body", `{"input":"raw string prompt"}`},
			wantInput: "raw string prompt",
		},
		{
			name:      "raw single content object preserved",
			cmdArgs:   []string{"generate", "--stream=false", "--body", `{"input":{"type":"text","text":"single content object"}}`},
			wantInput: map[string]any{"type": "text", "text": "single content object"},
		},
		{
			name:    "raw multi-turn step array preserved",
			cmdArgs: []string{"agent", "run", "--stream=false", "--body", `{"model":"gemini-3.6-flash","input":[{"type":"user_input","content":[{"type":"text","text":"turn 1"}]},{"type":"model_output","content":[{"type":"text","text":"reply 1"}]},{"type":"user_input","content":[{"type":"text","text":"turn 2"}]}]}`},
			wantInput: []any{
				map[string]any{"type": "user_input", "content": []any{map[string]any{"type": "text", "text": "turn 1"}}},
				map[string]any{"type": "model_output", "content": []any{map[string]any{"type": "text", "text": "reply 1"}}},
				map[string]any{"type": "user_input", "content": []any{map[string]any{"type": "text", "text": "turn 2"}}},
			},
		},
	}
	for _, tc := range rawCases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := executeRealRoot(t, "", base(tc.cmdArgs...)...)
			if err != nil {
				t.Fatalf("executeRealRoot(%v) failed: %v\nstderr: %s", tc.cmdArgs, err, stderr)
			}
			if gotPath != "/v1beta/interactions" {
				t.Errorf("path = %q, want /v1beta/interactions", gotPath)
			}
			if !reflect.DeepEqual(gotBody["input"], tc.wantInput) {
				t.Errorf("input = %#v, want %#v", gotBody["input"], tc.wantInput)
			}
		})
	}

	// 2. Unambiguous --body + --input merging produces valid step arrays and preserves step order/roles.
	mergeCases := []struct {
		name      string
		cmdArgs   []string
		wantInput []any
	}{
		{
			name:    "body without input + -i + positional prompt",
			cmdArgs: []string{"generate", "describe this", "-i", imgFile, "--stream=false", "--body", `{"model":"gemini-3.8-flash"}`},
			wantInput: []any{
				map[string]any{
					"type": "user_input",
					"content": []any{
						wantImgBlock,
						map[string]any{"type": "text", "text": "describe this"},
					},
				},
			},
		},
		{
			name:    "body with string input + -i",
			cmdArgs: []string{"generate", "-i", imgFile, "--stream=false", "--body", `{"input":"from body string"}`},
			wantInput: []any{
				map[string]any{
					"type": "user_input",
					"content": []any{
						wantImgBlock,
						map[string]any{"type": "text", "text": "from body string"},
					},
				},
			},
		},
		{
			name:    "body with single content object + -i",
			cmdArgs: []string{"generate", "-i", imgFile, "--stream=false", "--body", `{"input":{"type":"text","text":"from content map"}}`},
			wantInput: []any{
				map[string]any{
					"type": "user_input",
					"content": []any{
						wantImgBlock,
						map[string]any{"type": "text", "text": "from content map"},
					},
				},
			},
		},
		{
			name:    "body with content array + -i",
			cmdArgs: []string{"generate", "-i", imgFile, "--stream=false", "--body", `{"input":[{"type":"text","text":"from content array"}]}`},
			wantInput: []any{
				map[string]any{
					"type": "user_input",
					"content": []any{
						wantImgBlock,
						map[string]any{"type": "text", "text": "from content array"},
					},
				},
			},
		},
		{
			name:    "body with single user_input step array + -i merges inside user_input content",
			cmdArgs: []string{"agent", "run", "-i", imgFile, "--stream=false", "--body", `{"model":"gemini-3.6-flash","input":[{"type":"user_input","content":[{"type":"text","text":"from step"}]}]}`},
			wantInput: []any{
				map[string]any{
					"type": "user_input",
					"content": []any{
						wantImgBlock,
						map[string]any{"type": "text", "text": "from step"},
					},
				},
			},
		},
		{
			name:    "body with prior model_output step and trailing user_input step preserves step order and roles",
			cmdArgs: []string{"agent", "run", "-i", imgFile, "--stream=false", "--body", `{"model":"gemini-3.6-flash","input":[{"type":"model_output","content":[{"type":"text","text":"prior assistant message"}]},{"type":"user_input","content":[{"type":"text","text":"current user prompt"}]}]}`},
			wantInput: []any{
				map[string]any{
					"type": "model_output",
					"content": []any{
						map[string]any{"type": "text", "text": "prior assistant message"},
					},
				},
				map[string]any{
					"type": "user_input",
					"content": []any{
						wantImgBlock,
						map[string]any{"type": "text", "text": "current user prompt"},
					},
				},
			},
		},
		{
			name:    "body with multi-turn history including google_maps_call and google_maps_result merges into final user_input",
			cmdArgs: []string{"agent", "run", "-i", imgFile, "--stream=false", "--body", `{"model":"gemini-3.6-flash","input":[{"type":"user_input","content":[{"type":"text","text":"turn 1"}]},{"type":"model_output","content":[{"type":"text","text":"reply 1"}]},{"type":"google_maps_call","id":"m1"},{"type":"google_maps_result","call_id":"m1","result":[]},{"type":"model_output","content":[{"type":"text","text":"reply 2"}]},{"type":"user_input","content":[{"type":"text","text":"turn 2"}]}]}`},
			wantInput: []any{
				map[string]any{
					"type": "user_input",
					"content": []any{
						map[string]any{"type": "text", "text": "turn 1"},
					},
				},
				map[string]any{
					"type": "model_output",
					"content": []any{
						map[string]any{"type": "text", "text": "reply 1"},
					},
				},
				map[string]any{"type": "google_maps_call", "id": "m1"},
				map[string]any{"type": "google_maps_result", "call_id": "m1", "result": []any{}},
				map[string]any{
					"type": "model_output",
					"content": []any{
						map[string]any{"type": "text", "text": "reply 2"},
					},
				},
				map[string]any{
					"type": "user_input",
					"content": []any{
						wantImgBlock,
						map[string]any{"type": "text", "text": "turn 2"},
					},
				},
			},
		},
		{
			name:    "body with function_result step array appends user_input step after function_result",
			cmdArgs: []string{"generate", "-i", imgFile, "--stream=false", "--body", `{"input":[{"type":"function_result","call_id":"c1","name":"lookup","result":{"ok":true}}]}`},
			wantInput: []any{
				map[string]any{
					"type":    "function_result",
					"call_id": "c1",
					"name":    "lookup",
					"result":  map[string]any{"ok": true},
				},
				map[string]any{
					"type":    "user_input",
					"content": []any{wantImgBlock},
				},
			},
		},
	}
	for _, tc := range mergeCases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := executeRealRoot(t, "", base(tc.cmdArgs...)...)
			if err != nil {
				t.Fatalf("executeRealRoot(%v) failed: %v\nstderr: %s", tc.cmdArgs, err, stderr)
			}
			if !reflect.DeepEqual(gotBody["input"], tc.wantInput) {
				t.Errorf("input = %#v, want %#v", gotBody["input"], tc.wantInput)
			}
		})
	}

	// 3. Ambiguous or invalid --body + --input combinations fail clearly with exit code 2 and zero HTTP requests.
	rejectCases := []struct {
		name    string
		cmdArgs []string
		wantSub string
	}{
		{
			name:    "consecutive user_input steps rejected",
			cmdArgs: []string{"generate", "-i", imgFile, "--body", `{"input":[{"type":"user_input","content":[{"type":"text","text":"turn 1"}]},{"type":"user_input","content":[{"type":"text","text":"turn 2"}]}]}`},
			wantSub: `multiple consecutive "user_input" steps`,
		},
		{
			name:    "user_input step followed by model_output step rejected",
			cmdArgs: []string{"generate", "-i", imgFile, "--body", `{"input":[{"type":"user_input","content":[{"type":"text","text":"turn 1"}]},{"type":"model_output","content":[{"type":"text","text":"reply"}]}]}`},
			wantSub: `"user_input" is not the final step`,
		},
		{
			name:    "step array without user_input rejected",
			cmdArgs: []string{"generate", "-i", imgFile, "--body", `{"input":[{"type":"model_output","content":[{"type":"text","text":"reply"}]}]}`},
			wantSub: `do not contain a "user_input" step`,
		},
		{
			name:    "single model_output step object rejected",
			cmdArgs: []string{"generate", "-i", imgFile, "--body", `{"input":{"type":"model_output","content":[{"type":"text","text":"reply"}]}}`},
			wantSub: `cannot combine --input with "model_output" step`,
		},
		{
			name:    "mixed step and bare content array rejected",
			cmdArgs: []string{"generate", "-i", imgFile, "--body", `{"input":[{"type":"user_input","content":[{"type":"text","text":"turn 1"}]},{"type":"text","text":"bare content"}]}`},
			wantSub: `mixes step objects and bare content blocks`,
		},
	}
	for _, tc := range rejectCases {
		t.Run(tc.name, func(t *testing.T) {
			reqCount.Store(0)
			res := runCLI(t, t.TempDir(), nil, base(tc.cmdArgs...)...)
			if code := exitCode(t, res); code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr: %s", code, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.wantSub) {
				t.Errorf("stderr = %q, want substring %q", res.stderr, tc.wantSub)
			}
			if got := reqCount.Load(); got != 0 {
				t.Errorf("server received %d requests, want 0", got)
			}
		})
	}
}

// TestOptInLiveSmokeDefaultV1AndV1Beta runs a live smoke check against the
// default API (v1) and explicit v1beta when GEMINI_LIVE_SMOKE=1 and
// GEMINI_API_KEY are set in the environment.
func TestOptInLiveSmokeDefaultV1AndV1Beta(t *testing.T) {
	if os.Getenv("GEMINI_LIVE_SMOKE") != "1" || strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) == "" {
		t.Skip("skipping opt-in live smoke test: set GEMINI_LIVE_SMOKE=1 and GEMINI_API_KEY to run against the live Developer API")
	}
	apiKey := os.Getenv("GEMINI_API_KEY")
	note := writeTempFile(t, "live_note.md", "The capital of France is Paris.\n")

	for _, tc := range []struct {
		name    string
		verArgs []string
	}{
		{name: "default v1", verArgs: nil},
		{name: "explicit v1beta", verArgs: []string{"--api-version", "v1beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--no-interactive", "--color", "never"}, tc.verArgs...)
			args = append(args, "analyze", "-i", note, "Reply with the single word Paris.", "--output-format", "json")
			res := runCLI(t, t.TempDir(), map[string]string{"GEMINI_API_KEY": apiKey}, args...)
			if res.err != nil {
				t.Fatalf("live smoke (%s) failed: %v\nstderr: %s", tc.name, res.err, res.stderr)
			}
			if !strings.Contains(strings.ToLower(res.stdout), "paris") {
				t.Errorf("live smoke (%s) stdout = %q, want mention of Paris", tc.name, res.stdout)
			}
		})
	}
}
