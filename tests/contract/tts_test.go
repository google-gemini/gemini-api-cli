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
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ttsAudioResponse stubs a completed interaction carrying one inline audio
// block in a model-output step (the current wire shape).
func ttsAudioResponse(data []byte, mimeType string, sampleRate, channels int) string {
	block := audioBlock(data, mimeType)
	if sampleRate > 0 {
		block["sample_rate"] = sampleRate
	}
	if channels > 0 {
		block["channels"] = channels
	}
	return ttsInteraction(nil, block)
}

// TestTTSWrapsPCMAsWav pins the TTS happy path: the request asks for the audio
// response format with a single-voice speech config, and raw L16 PCM comes back and is
// written to a .wav artifact with a valid RIFF header; stdout is the abs path.
func TestTTSWrapsPCMAsWav(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	var reqBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/interactions" {
			t.Errorf("path = %q, want /v1beta/interactions", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &reqBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, ttsAudioResponse(pcm, "audio/l16", 24000, 1))
	}))
	defer server.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "voice.wav")
	args := append(humanArgs(server.URL), "tts", "hello world", "--out", out)
	result := runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
	}

	// Request: audio response format + single-voice speech config (array of one).
	assertAudioResponseFormat(t, reqBody)
	assertNotStored(t, reqBody)
	gc, _ := reqBody["generation_config"].(map[string]any)
	sc, ok := gc["speech_config"].([]any)
	if !ok || len(sc) != 1 {
		t.Fatalf("speech_config = %#v, want a single-element array", gc["speech_config"])
	}
	if v, _ := sc[0].(map[string]any); v["voice"] != "Kore" {
		t.Errorf("speech_config[0].voice = %#v, want Kore", sc[0])
	}

	// Artifact: valid WAV wrapping the exact PCM payload, path on stdout.
	if strings.TrimSpace(result.stdout) != out {
		t.Errorf("stdout = %q, want the artifact path %q", strings.TrimSpace(result.stdout), out)
	}
	wav, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading artifact: %v", err)
	}
	if len(wav) != 44+len(pcm) {
		t.Errorf("artifact size = %d, want %d (44-byte header + PCM)", len(wav), 44+len(pcm))
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Errorf("artifact is not a RIFF/WAVE file: %x", wav[:12])
	}
	if string(wav[44:]) != string(pcm) {
		t.Errorf("PCM payload not preserved after the header")
	}
}

// TestTTSMultiSpeakerRequest pins the multi-speaker request shape: a per-speaker
// SpeakerConfig rather than the single-voice array.
func TestTTSMultiSpeakerRequest(t *testing.T) {
	var reqBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &reqBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, ttsAudioResponse([]byte{0x00, 0x01}, "audio/l16", 24000, 1))
	}))
	defer server.Close()

	dir := t.TempDir()
	args := append(humanArgs(server.URL), "tts", "Alice: hi. Bob: yo.",
		"--multi-speaker", "Alice=Kore,Bob=Puck", "--out", filepath.Join(dir, "d.wav"))
	if result := runCLI(t, dir, nil, args...); result.err != nil {
		t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
	}

	assertAudioResponseFormat(t, reqBody)
	assertNotStored(t, reqBody)
	gc, _ := reqBody["generation_config"].(map[string]any)
	sc, ok := gc["speech_config"].(map[string]any)
	if !ok {
		t.Fatalf("speech_config = %#v, want a SpeakerConfig object", gc["speech_config"])
	}
	speakers, _ := sc["speakers"].([]any)
	if len(speakers) != 2 {
		t.Fatalf("speakers = %#v, want 2", sc["speakers"])
	}
	first, _ := speakers[0].(map[string]any)
	if first["speaker"] != "Alice" || first["voice"] != "Kore" {
		t.Errorf("speakers[0] = %#v, want Alice=Kore", first)
	}
}

// TestTTSMultiSpeakerInput pins the per-model input shape: current TTS models
// take one text block per speaker turn annotated with its speaker, label
// removed; the legacy models reject annotations and take the script as one
// plain block.
func TestTTSMultiSpeakerInput(t *testing.T) {
	stub := newInteractionStub(t, ttsAudioResponse([]byte{0x00, 0x01}, "audio/l16", 24000, 1))
	turn := func(speaker, text string) any {
		return map[string]any{"type": "text", "text": text,
			"annotations": []any{map[string]any{"type": "speech_metadata", "speaker": speaker}}}
	}
	for model, want := range map[string][]any{
		"":                                    {turn("Alice", "hi."), turn("Bob", "yo.")},
		"gemini-3.1-flash-tts-preview":        {map[string]any{"type": "text", "text": "Alice: hi. Bob: yo."}},
		"models/gemini-3.1-flash-tts-preview": {map[string]any{"type": "text", "text": "Alice: hi. Bob: yo."}},
	} {
		dir := t.TempDir()
		args := append(plainArgs(stub.URL), "tts", "Alice: hi. Bob: yo.",
			"--multi-speaker", "Alice=Kore,Bob=Puck", "--out", filepath.Join(dir, "d.wav"))
		if model != "" {
			args = append(args, "--model", model)
		}
		if result := runCLI(t, dir, nil, args...); result.err != nil {
			t.Fatalf("tts (model %q) failed: %v\nstderr: %s", model, result.err, result.stderr)
		}
		if got, _ := stub.body["input"].([]any); !reflect.DeepEqual(got, want) {
			t.Errorf("model %q: input = %#v, want %#v", model, stub.body["input"], want)
		}
	}
}

// TestTTSMultiSpeakerScriptFile pins a Windows-saved -f script: the UTF-8
// byte-order mark is dropped and CRLF line ends split turns like LF.
func TestTTSMultiSpeakerScriptFile(t *testing.T) {
	stub := newInteractionStub(t, ttsAudioResponse([]byte{0x00, 0x01}, "audio/l16", 24000, 1))
	dir := t.TempDir()
	script := filepath.Join(dir, "script.txt")
	if err := os.WriteFile(script, []byte("\uFEFFAlice: hi.\r\nBob: yo.\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append(plainArgs(stub.URL), "tts", "-f", script,
		"--multi-speaker", "Alice=Kore,Bob=Puck", "--out", filepath.Join(dir, "d.wav"))
	if result := runCLI(t, dir, nil, args...); result.err != nil {
		t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
	}
	var texts []any
	for _, block := range stub.body["input"].([]any) {
		texts = append(texts, block.(map[string]any)["text"])
	}
	if want := []any{"hi.", "yo."}; !reflect.DeepEqual(texts, want) {
		t.Errorf("input texts = %q, want %q", texts, want)
	}
}

// TestTTSMultiSpeakerRejectsUnlabelledText pins the local usage error for text
// before the first speaker label, which the API rejects opaquely; no request
// is sent.
func TestTTSMultiSpeakerRejectsUnlabelledText(t *testing.T) {
	stub := newInteractionStub(t, ttsAudioResponse([]byte{0x00, 0x01}, "audio/l16", 24000, 1))
	dir := t.TempDir()
	args := append(plainArgs(stub.URL), "tts", "Intro. Alice: hi. Bob: yo.",
		"--multi-speaker", "Alice=Kore,Bob=Puck", "--out", filepath.Join(dir, "d.wav"))
	result := runCLI(t, dir, nil, args...)
	if code := exitCode(t, result); code != 2 || !strings.Contains(result.stderr, `text before the first speaker label: "Intro."`) {
		t.Errorf("exit = %d, stderr = %q; want exit 2 and the unlabelled-text error", code, result.stderr)
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("sent %d requests, want none", n)
	}
}

// TestTTSWarnsOnSpeakerAbsentFromText pins the stderr warning for a declared
// speaker the script never names; the API accepts that silently, so it is not
// an error.
func TestTTSWarnsOnSpeakerAbsentFromText(t *testing.T) {
	stub := newInteractionStub(t, ttsAudioResponse([]byte{0x00, 0x01}, "audio/l16", 24000, 1))
	dir := t.TempDir()
	// "Al" occurs only inside "Alice", which does not name the speaker.
	args := append(plainArgs(stub.URL), "tts", "Alice: hi. Bob: yo.",
		"--multi-speaker", "Alice=Kore,Al=Puck", "--out", filepath.Join(dir, "d.wav"))
	result := runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if !strings.Contains(result.stderr, `warning: speaker "Al" does not appear in the text`) {
		t.Errorf("stderr = %q, want the absent-speaker warning", result.stderr)
	}
	if strings.Contains(result.stderr, `speaker "Alice"`) {
		t.Errorf("stderr = %q, warns about a speaker the text names", result.stderr)
	}

	// Under a structured format stderr stays silent and the warning travels in
	// the result envelope instead.
	args = append(plainArgs(stub.URL), "--output-format", "json", "tts", "Alice: hi. Bob: yo.",
		"--multi-speaker", "Alice=Kore,Al=Puck", "--out", filepath.Join(dir, "e.wav"))
	result = runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if result.stderr != "" {
		t.Errorf("structured mode wrote stderr: %q", result.stderr)
	}
	var envelope struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, result.stdout)
	}
	if want := []string{`speaker "Al" does not appear in the text`}; !slices.Equal(envelope.Warnings, want) {
		t.Errorf("envelope warnings = %q, want %q", envelope.Warnings, want)
	}
}

// TestTTSNonPCMPassthrough pins that the container formats the API documents
// (MP3, Ogg Opus, WAV) are written as-is — no WAV wrapping — under the matching
// extension.
func TestTTSNonPCMPassthrough(t *testing.T) {
	payload := []byte{0xFF, 0xFB, 0x90, 0x00, 0xAA}
	for mimeType, ext := range map[string]string{"audio/mp3": ".mp3", "audio/ogg_opus": ".ogg", "audio/wav": ".wav"} {
		t.Run(mimeType, func(t *testing.T) {
			stub := newInteractionStub(t, ttsAudioResponse(payload, mimeType, 0, 0))
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			result := runCLIInDir(t, t.TempDir(), dir, nil, append(plainArgs(stub.URL), "tts", "hi")...)
			if result.err != nil {
				t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
			}
			out := strings.TrimSpace(result.stdout)
			if filepath.Dir(out) != dir || filepath.Ext(out) != ext {
				t.Errorf("artifact path = %q, want a %s file in %s", out, ext, dir)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("reading artifact: %v", err)
			}
			if string(got) != string(payload) {
				t.Errorf("artifact = %x, want the exact bytes (no WAV header)", got)
			}
		})
	}
}

// TestTTSValidation pins the usage guards: no HTTP request, exit 2.
func TestTTSValidation(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{"voice and multi-speaker", []string{"tts", "x", "--voice", "Puck", "--multi-speaker", "A=Kore"}, "mutually exclusive"},
		{"bad multi-speaker entry", []string{"tts", "x", "--multi-speaker", "Alice"}, "expected Speaker=Voice"},
		{"one speaker", []string{"tts", "x", "--multi-speaker", "A=Kore"}, "Use --voice for a single speaker"},
		{"three speakers", []string{"tts", "x", "--multi-speaker", "A=Kore,B=Puck,C=Zephyr"}, "exactly two Speaker=Voice entries (got 3)"},
		{"duplicate speaker", []string{"tts", "x", "--multi-speaker", "A=Kore, A =Puck"}, `speaker "A" more than once`},
		{"missing text", []string{"tts"}, "missing required input text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got++
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			args := append(humanArgs(server.URL), tc.args...)
			result := runCLI(t, t.TempDir(), nil, args...)
			if code := exitCode(t, result); code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
			}
			if !strings.Contains(result.stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", result.stderr, tc.wantStderr)
			}
			if n := strings.Count(result.stderr, "--help for runnable examples"); n > 1 {
				t.Errorf("the --help pointer appears %d times, want at most once:\n%s", n, result.stderr)
			}
			if got != 0 {
				t.Errorf("reached the server %d times, want 0", got)
			}
		})
	}
}

// assertAudioResponseFormat pins the request's audio selector: response_format
// {"type":"audio"}, never the deprecated response_modalities.
func assertAudioResponseFormat(t *testing.T, reqBody map[string]any) {
	t.Helper()
	format, _ := reqBody["response_format"].(map[string]any)
	if len(format) != 1 || format["type"] != "audio" {
		t.Errorf("response_format = %#v, want {type: audio}", reqBody["response_format"])
	}
	if _, ok := reqBody["response_modalities"]; ok {
		t.Errorf("request carries the deprecated response_modalities: %#v", reqBody["response_modalities"])
	}
}

// inputText returns the text of the single text content block in the request.
func (s *interactionStub) inputText(t *testing.T) string {
	t.Helper()
	parts := s.inputParts(t)
	if len(parts) != 1 {
		t.Fatalf("request input = %#v, want one text block", s.body["input"])
	}
	part := parts[0]
	if part["type"] != "text" {
		t.Fatalf("request input block = %#v, want a text block", part)
	}
	text, _ := part["text"].(string)
	return text
}

// ttsInteraction renders a completed interaction whose model-output step
// carries the given content blocks; extra merges top-level fields.
func ttsInteraction(extra map[string]any, blocks ...map[string]any) string {
	body := map[string]any{
		"id":     "int-tts",
		"status": "completed",
		"steps":  []map[string]any{{"type": "model_output", "content": blocks}},
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func audioBlock(data []byte, mimeType string) map[string]any {
	block := map[string]any{"type": "audio", "data": base64.StdEncoding.EncodeToString(data)}
	if mimeType != "" {
		block["mime_type"] = mimeType
	}
	return block
}

// wavFormat reads the channel count, sample rate, and declared data length out
// of a canonical 44-byte WAV header.
func wavFormat(t *testing.T, wav []byte) (channels, sampleRate, dataLen int) {
	t.Helper()
	if len(wav) < 44 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("artifact is not a RIFF/WAVE file: %x", wav)
	}
	return int(binary.LittleEndian.Uint16(wav[22:])), int(binary.LittleEndian.Uint32(wav[24:])), int(binary.LittleEndian.Uint32(wav[40:]))
}

// TestTTSTextSources pins the three text sources and the --language hint: each
// source lands as the single text block, and mixing sources is a usage error.
func TestTTSTextSources(t *testing.T) {
	response := ttsAudioResponse([]byte{0x01, 0x02}, "audio/l16", 24000, 1)

	t.Run("file with language", func(t *testing.T) {
		stub := newInteractionStub(t, response)
		dir := t.TempDir()
		script := writeTempFile(t, "script.txt", "  from the file\n")
		args := append(plainArgs(stub.URL), "tts", "-f", script, "--voice", "Puck", "--language", "fr-FR", "--out", filepath.Join(dir, "f.wav"))
		if result := runCLI(t, dir, nil, args...); result.err != nil {
			t.Fatalf("tts -f failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if got := stub.inputText(t); got != "from the file" {
			t.Errorf("input text = %q, want the trimmed file content", got)
		}
		gc, _ := stub.body["generation_config"].(map[string]any)
		sc, _ := gc["speech_config"].([]any)
		if len(sc) != 1 {
			t.Fatalf("speech_config = %#v, want a single-element array", gc["speech_config"])
		}
		if v, _ := sc[0].(map[string]any); v["voice"] != "Puck" || v["language"] != "fr-FR" {
			t.Errorf("speech_config[0] = %#v, want voice Puck / language fr-FR", sc[0])
		}
	})

	t.Run("stdin", func(t *testing.T) {
		stub := newInteractionStub(t, response)
		dir := t.TempDir()
		args := append(plainArgs(stub.URL), "tts", "--stdin", "--out", filepath.Join(dir, "s.wav"))
		if result := runCLIWithStdin(t, dir, nil, "from stdin\n", args...); result.err != nil {
			t.Fatalf("tts --stdin failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if got := stub.inputText(t); got != "from stdin" {
			t.Errorf("input text = %q, want the trimmed stdin content", got)
		}
	})

	t.Run("multi-speaker language", func(t *testing.T) {
		stub := newInteractionStub(t, response)
		dir := t.TempDir()
		args := append(plainArgs(stub.URL), "tts", "A: hi. B: yo.", "--multi-speaker", "A=Kore, B=Puck", "--language", "en-GB", "--out", filepath.Join(dir, "m.wav"))
		if result := runCLI(t, dir, nil, args...); result.err != nil {
			t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
		}
		gc, _ := stub.body["generation_config"].(map[string]any)
		sc, _ := gc["speech_config"].(map[string]any)
		speakers, _ := sc["speakers"].([]any)
		if len(speakers) != 2 {
			t.Fatalf("speakers = %#v, want 2", sc["speakers"])
		}
		for i, want := range []string{"Kore", "Puck"} {
			if s, _ := speakers[i].(map[string]any); s["voice"] != want || s["language"] != "en-GB" {
				t.Errorf("speakers[%d] = %#v, want voice %s / language en-GB", i, s, want)
			}
		}
	})

	for _, tc := range []struct {
		name       string
		args       []string
		stdin      string
		wantStderr string
	}{
		{"argument and file", []string{"tts", "x", "-f", "script.txt"}, "", "provide the text once"},
		{"argument and stdin", []string{"tts", "x", "--stdin"}, "y", "provide the text once"},
		{"missing file", []string{"tts", "-f", "absent.txt"}, "", "cannot read --file"},
		{"empty stdin", []string{"tts", "--stdin"}, " \n", "stdin is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newInteractionStub(t, response)
			result := runCLIWithStdin(t, t.TempDir(), nil, tc.stdin, append(plainArgs(stub.URL), tc.args...)...)
			if code := exitCode(t, result); code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
			}
			if !strings.Contains(result.stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", result.stderr, tc.wantStderr)
			}
			if got := stub.requests.Load(); got != 0 {
				t.Errorf("reached the server %d times, want 0", got)
			}
		})
	}
}

// TestTTSEnvelope pins the structured result: the snake_case envelope carries
// the artifact path, the PCM shape, and the flattened usage.
func TestTTSEnvelope(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	block := audioBlock(pcm, "audio/l16")
	block["sample_rate"], block["channels"] = 24000, 1
	stub := newInteractionStub(t, ttsInteraction(map[string]any{
		"usage": map[string]any{"total_input_tokens": 3, "total_output_tokens": 40, "total_tokens": 43},
	}, block))

	dir := t.TempDir()
	out := filepath.Join(dir, "e.wav")
	args := append(plainArgs(stub.URL), "--output-format", "json", "tts", "hello", "--out", out)
	result := runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if strings.TrimSpace(result.stderr) != "" {
		t.Errorf("stderr is not silent under the JSON envelope: %s", result.stderr)
	}
	var envelope struct {
		Model      string `json:"model"`
		Path       string `json:"path"`
		MIMEType   string `json:"mime_type"`
		SizeBytes  int    `json:"size_bytes"`
		SampleRate int    `json:"sample_rate"`
		Channels   int    `json:"channels"`
		Usage      struct {
			PromptTokens int `json:"prompt_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, result.stdout)
	}
	if envelope.Model != "gemini-3.8-flash-tts" || envelope.Path != out || envelope.MIMEType != "audio/l16" {
		t.Errorf("envelope model/path/mime_type = %q/%q/%q", envelope.Model, envelope.Path, envelope.MIMEType)
	}
	if envelope.SizeBytes != 44+len(pcm) || envelope.SampleRate != 24000 || envelope.Channels != 1 {
		t.Errorf("envelope size/rate/channels = %d/%d/%d, want %d/24000/1", envelope.SizeBytes, envelope.SampleRate, envelope.Channels, 44+len(pcm))
	}
	if envelope.Usage.PromptTokens != 3 || envelope.Usage.OutputTokens != 40 || envelope.Usage.TotalTokens != 43 {
		t.Errorf("envelope usage = %+v, want 3/40/43", envelope.Usage)
	}
}

// TestTTSRejectsUnusableAudio pins the runtime failures detected after a
// successful exchange: exit 1, a named cause, and no artifact on disk.
func TestTTSRejectsUnusableAudio(t *testing.T) {
	cases := []struct {
		name       string
		response   string
		wantStderr string
	}{
		{"text only", completedInteraction("I cannot speak", ""), "no audio data in the API response"},
		{"platform error", `{"id":"int-tts","status":"failed","errors":[{"code":"blocked","message":"content was blocked"}]}`, "the interaction did not complete (status: failed): content was blocked"},
		{"incomplete with audio", ttsInteraction(map[string]any{"status": "incomplete"}, audioBlock([]byte{0x01, 0x02}, "audio/l16")), "the API stopped before the output was complete (status: incomplete)"},
		{"cancelled with audio", ttsInteraction(map[string]any{"status": "cancelled"}, audioBlock([]byte{0x01, 0x02}, "audio/l16")), "the interaction did not complete (status: cancelled)"},
		{"invalid base64", ttsInteraction(nil, map[string]any{"type": "audio", "data": "!!not-base64!!", "mime_type": "audio/l16"}), "audio payload is not valid base64"},
		{"companded audio", ttsInteraction(nil, audioBlock([]byte{0x01, 0x02}, "audio/alaw")), "returned audio as audio/alaw, which cannot be written"},
		{"unknown audio type", ttsInteraction(nil, audioBlock([]byte{0x01, 0x02}, "audio/x-future")), "returned audio as audio/x-future, which cannot be written"},
		{"mixed block formats", ttsInteraction(nil, audioBlock([]byte{0x01, 0x02}, "audio/l16"), audioBlock([]byte{0x03}, "audio/mp3")), "audio blocks in different formats"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newInteractionStub(t, tc.response)
			dir := t.TempDir()
			out := filepath.Join(dir, "none.wav")
			result := runCLI(t, dir, nil, append(plainArgs(stub.URL), "tts", "hello", "--out", out)...)
			if code := exitCode(t, result); code != 1 {
				t.Errorf("exit code = %d, want 1\nstderr: %s", code, result.stderr)
			}
			if !strings.Contains(result.stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", result.stderr, tc.wantStderr)
			}
			if strings.TrimSpace(result.stdout) != "" {
				t.Errorf("stdout = %q, want it empty", result.stdout)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("artifact %s exists after a failed synthesis (stat err: %v)", out, err)
			}
		})
	}
}

// TestTTSAudioFormatResolution pins where the WAV header's shape comes from:
// the audio block's own fields, then the MIME parameters, then 24 kHz mono.
func TestTTSAudioFormatResolution(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	withShape := func(block map[string]any, sampleRate, channels int) map[string]any {
		block["sample_rate"], block["channels"] = sampleRate, channels
		return block
	}
	cases := []struct {
		name                   string
		blocks                 []map[string]any
		wantChannels, wantRate int
		wantPayload            []byte
	}{
		{"missing mime defaults to 24k mono PCM", []map[string]any{audioBlock(pcm, "")}, 1, 24000, pcm},
		{"rate from the MIME parameter", []map[string]any{audioBlock(pcm, "audio/L16;codec=pcm;rate=16000")}, 1, 16000, pcm},
		{"block fields win over the MIME", []map[string]any{withShape(audioBlock(pcm, "audio/l16;rate=16000"), 48000, 2)}, 2, 48000, pcm},
		{"blocks are concatenated in order", []map[string]any{
			audioBlock(pcm[:2], "audio/l16"),
			{"type": "text", "text": "ignored"},
			audioBlock(pcm[2:], "audio/l16"),
		}, 1, 24000, pcm},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newInteractionStub(t, ttsInteraction(nil, tc.blocks...))
			dir := t.TempDir()
			out := filepath.Join(dir, "shape.wav")
			if result := runCLI(t, dir, nil, append(plainArgs(stub.URL), "tts", "hello", "--out", out)...); result.err != nil {
				t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
			}
			wav, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("reading artifact: %v", err)
			}
			channels, rate, dataLen := wavFormat(t, wav)
			if channels != tc.wantChannels || rate != tc.wantRate {
				t.Errorf("WAV channels/rate = %d/%d, want %d/%d", channels, rate, tc.wantChannels, tc.wantRate)
			}
			if dataLen != len(tc.wantPayload) || string(wav[44:]) != string(tc.wantPayload) {
				t.Errorf("WAV payload = %x (declared %d bytes), want %x", wav[44:], dataLen, tc.wantPayload)
			}
		})
	}
}

// TestTTSArtifactPath pins where the artifact lands: a default name in the
// working directory, and an --out whose extension follows the returned bytes.
func TestTTSArtifactPath(t *testing.T) {
	stub := newInteractionStub(t, ttsAudioResponse([]byte{0x01, 0x02}, "audio/l16", 24000, 1))
	// Resolve symlinks: macOS TempDir lives under /var -> /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("default name in the working directory", func(t *testing.T) {
		result := runCLIInDir(t, t.TempDir(), dir, nil, append(plainArgs(stub.URL), "tts", "hello")...)
		if result.err != nil {
			t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
		}
		path := strings.TrimSpace(result.stdout)
		if ok, _ := filepath.Match(filepath.Join(dir, "gemini-tts-*.wav"), path); !ok {
			t.Errorf("stdout = %q, want %s/gemini-tts-*.wav", path, dir)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("artifact missing: %v", err)
		}
	})

	for _, tc := range []struct{ out, want string }{
		{"speech.mp3", "speech.wav"},
		{"bare", "bare.wav"},
		{filepath.Join("nested", "deep", "v.WAV"), filepath.Join("nested", "deep", "v.WAV")},
	} {
		t.Run("out "+tc.out, func(t *testing.T) {
			result := runCLIInDir(t, t.TempDir(), dir, nil, append(plainArgs(stub.URL), "tts", "hello", "--out", tc.out)...)
			if result.err != nil {
				t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
			}
			want := filepath.Join(dir, tc.want)
			if got := strings.TrimSpace(result.stdout); got != want {
				t.Errorf("stdout = %q, want %q", got, want)
			}
			if _, err := os.Stat(want); err != nil {
				t.Errorf("artifact missing: %v", err)
			}
			if tc.out != tc.want {
				if _, err := os.Stat(filepath.Join(dir, tc.out)); !os.IsNotExist(err) {
					t.Errorf("%s was written alongside %s", tc.out, tc.want)
				}
			}
		})
	}

	// A directory receives the default-named file: never "<dir>/.wav", and
	// never "<dir>.wav" beside an existing directory.
	if err := os.Mkdir(filepath.Join(dir, "existingdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{"existingdir", "existingdir/", "newdir/"} {
		t.Run("out directory "+out, func(t *testing.T) {
			result := runCLIInDir(t, t.TempDir(), dir, nil, append(plainArgs(stub.URL), "tts", "hello", "--out", out)...)
			if result.err != nil {
				t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
			}
			path := strings.TrimSpace(result.stdout)
			wantDir := filepath.Join(dir, strings.TrimSuffix(out, "/"))
			if ok, _ := filepath.Match(filepath.Join(wantDir, "gemini-tts-*.wav"), path); !ok {
				t.Errorf("stdout = %q, want %s/gemini-tts-*.wav", path, wantDir)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("artifact missing: %v", err)
			}
		})
	}

	t.Run("existing destination is replaced", func(t *testing.T) {
		out := filepath.Join(dir, "replace.wav")
		if err := os.WriteFile(out, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		result := runCLIInDir(t, t.TempDir(), dir, nil, append(plainArgs(stub.URL), "tts", "hello", "--out", out)...)
		if result.err != nil {
			t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if data, _ := os.ReadFile(out); !bytes.HasPrefix(data, []byte("RIFF")) {
			t.Errorf("destination was not replaced with the WAV: %q", data)
		}
	})

	t.Run("unwritable parent leaves nothing behind", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		result := runCLIInDir(t, t.TempDir(), dir, nil, append(plainArgs(stub.URL), "tts", "hello", "--out", filepath.Join(locked, "x.wav"))...)
		if code := exitCode(t, result); code != 1 {
			t.Errorf("exit code = %d, want 1\nstderr: %s", code, result.stderr)
		}
		if !strings.Contains(result.stderr, "cannot write") || result.stdout != "" {
			t.Errorf("stdout = %q, stderr = %q", result.stdout, result.stderr)
		}
		if entries, _ := os.ReadDir(locked); len(entries) != 0 {
			t.Errorf("failed write left %d entries behind", len(entries))
		}
	})
}
