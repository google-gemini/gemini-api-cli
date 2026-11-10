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
)

// intentDryRunArgs previews an intent request as JSON on stdout without
// sending it.
func intentDryRunArgs(args ...string) []string {
	return append(append(baseArgs("https://example.invalid"), "--api-key", "test", "--dry-run"), args...)
}

func textPart(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func inputParts(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	return assertUserInputStepParts(t, body)
}

// TestIntentModelOverride pins the --model guard on the generated intents and
// agent run: an override that is not a single model id is a usage error before
// any request, as on tts/analyze/transcribe and models get, and a models/
// prefix is stripped from a valid one.
func TestIntentModelOverride(t *testing.T) {
	for _, command := range [][]string{
		{"generate", "hi"},
		{"image", "a cat"},
		{"video", "a cat"},
		{"music", "lofi"},
		{"agent", "run", "hi"},
	} {
		for _, model := range []string{"a/b", "models/a/b", "/"} {
			t.Run(strings.Join(command, " ")+" rejects "+model, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer server.Close()

				args := append(append(plainArgs(server.URL), command...), "--model", model)
				result := runCLI(t, t.TempDir(), nil, args...)
				if code := exitCode(t, result); code != 2 {
					t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
				}
				if !strings.Contains(result.stderr, "--model: invalid model id") {
					t.Errorf("stderr = %q, want the --model usage error", result.stderr)
				}
				if n := requests.Load(); n != 0 {
					t.Errorf("requests sent = %d, want none", n)
				}
			})
		}
	}

	result := runCLI(t, t.TempDir(), nil, intentDryRunArgs("generate", "hi", "--model", "models/gemini-x")...)
	if result.err != nil {
		t.Fatalf("generate failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if body := parseDryRunBody(t, result.stdout); body["model"] != "gemini-x" {
		t.Errorf("request model = %#v, want gemini-x", body["model"])
	}
}

// TestIntentTextInput pins --stdin on the Create intents and analyze, and
// -f/--file on music (the commands with -i already take text files there):
// the text alone is the prompt; next to a positional prompt or question it is
// a text block placed before it (after any --input media).
func TestIntentTextInput(t *testing.T) {
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("\xef\xbb\xbfnotes from a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("stdin before the prompt", func(t *testing.T) {
		result := runCLIWithStdin(t, t.TempDir(), nil, "Plain text\n", intentDryRunArgs("generate", "--stdin", "Summarize this")...)
		if result.err != nil {
			t.Fatalf("generate failed: %v\nstderr: %s", result.err, result.stderr)
		}
		got := inputParts(t, parseDryRunBody(t, result.stdout))
		want := []map[string]any{textPart("Plain text"), textPart("Summarize this")}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("input = %#v, want %#v", got, want)
		}
	})

	t.Run("stdin alone is the prompt", func(t *testing.T) {
		for _, command := range []string{"generate", "image", "video", "music"} {
			result := runCLIWithStdin(t, t.TempDir(), nil, "What is 2+2?\n", intentDryRunArgs(command, "--stdin")...)
			if result.err != nil {
				t.Fatalf("%s failed: %v\nstderr: %s", command, result.err, result.stderr)
			}
			if body := parseDryRunBody(t, result.stdout); body["input"] != "What is 2+2?" {
				t.Errorf("%s input = %#v, want the stdin text as the prompt", command, body["input"])
			}
		}
	})

	t.Run("file before the prompt", func(t *testing.T) {
		result := runCLI(t, t.TempDir(), nil, intentDryRunArgs("music", "-f", notes, "make it upbeat")...)
		if result.err != nil {
			t.Fatalf("music failed: %v\nstderr: %s", result.err, result.stderr)
		}
		got := inputParts(t, parseDryRunBody(t, result.stdout))
		want := []map[string]any{textPart("notes from a file"), textPart("make it upbeat")}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("input = %#v, want %#v", got, want)
		}
	})

	t.Run("stdin merges into a --body", func(t *testing.T) {
		result := runCLIWithStdin(t, t.TempDir(), nil, "context", intentDryRunArgs("generate", "--stdin", "question", "--body", `{"model":"gemini-x"}`)...)
		if result.err != nil {
			t.Fatalf("generate failed: %v\nstderr: %s", result.err, result.stderr)
		}
		body := parseDryRunBody(t, result.stdout)
		if body["model"] != "gemini-x" {
			t.Errorf("request model = %#v, want the --body model", body["model"])
		}
		want := []map[string]any{textPart("context"), textPart("question")}
		if got := inputParts(t, body); !reflect.DeepEqual(got, want) {
			t.Errorf("input = %#v, want %#v", got, want)
		}
	})

	t.Run("analyze stdin before the question", func(t *testing.T) {
		result := runCLIWithStdin(t, t.TempDir(), nil, "a,b\n1,2\n", intentDryRunArgs("analyze", "--stdin", "Summarize")...)
		if result.err != nil {
			t.Fatalf("analyze failed: %v\nstderr: %s", result.err, result.stderr)
		}
		got := inputParts(t, parseDryRunBody(t, result.stdout))
		want := []map[string]any{textPart("a,b\n1,2"), textPart("Summarize")}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("input = %#v, want %#v", got, want)
		}
	})
}

// TestIntentTextInputErrors pins the usage errors around --file/--stdin and
// the hint pointing piped plain text at --stdin. Piped stdin without --stdin
// is still the JSON request body.
func TestIntentTextInputErrors(t *testing.T) {
	notes := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notes, []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		stdin   string
		args    []string
		wantErr string
	}{
		{"stdin and body @-", "{}", []string{"generate", "--stdin", "--body", "@-"}, "--stdin and --body @- both read stdin"},
		{"stdin and file", "text", []string{"music", "--stdin", "-f", notes}, "provide the text once"},
		{"empty stdin", "  \n", []string{"generate", "--stdin"}, "stdin is empty"},
		{"piped text without --stdin", "Plain text", []string{"generate", "Summarize"}, "add --stdin to send it as prompt text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := runCLIWithStdin(t, t.TempDir(), nil, tc.stdin, intentDryRunArgs(tc.args...)...)
			if code := exitCode(t, result); code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
			}
			if !strings.Contains(result.stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want %q", result.stderr, tc.wantErr)
			}
		})
	}

	result := runCLIWithStdin(t, t.TempDir(), nil, `{"input":"hi"}`, intentDryRunArgs("generate")...)
	if result.err != nil {
		t.Fatalf("generate with a piped JSON body failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if body := parseDryRunBody(t, result.stdout); body["input"] != "hi" {
		t.Errorf("input = %#v, want the piped JSON body's input", body["input"])
	}
}

// TestGenerateSystemInstruction pins --system on generate: it sets
// system_instruction, and a --body that also sets it is a usage error.
func TestGenerateSystemInstruction(t *testing.T) {
	result := runCLI(t, t.TempDir(), nil, intentDryRunArgs("generate", "hi", "--system", "Be brief")...)
	if result.err != nil {
		t.Fatalf("generate failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if body := parseDryRunBody(t, result.stdout); body["system_instruction"] != "Be brief" {
		t.Errorf("system_instruction = %#v, want %q", body["system_instruction"], "Be brief")
	}

	result = runCLI(t, t.TempDir(), nil, intentDryRunArgs("generate", "hi", "--system", "x", "--body", `{"system_instruction":"y"}`)...)
	if code := exitCode(t, result); code != 2 {
		t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
	}
	if !strings.Contains(result.stderr, "set both by --system and by --body") {
		t.Errorf("stderr = %q, want the duplicate-key error", result.stderr)
	}
}

func decodeMapUseNumber(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode JSON with UseNumber: %v\nraw:\n%s", err, string(data))
	}
	return m
}

// TestIntentBodyPreservesLargeIntegers verifies that custom request-body
// handling preserves exact 64-bit integer digits in function_result inputs and
// response_format schemas across --body, stdin, and -i, both in --dry-run and
// over the wire, while rejecting invalid or trailing JSON with exit code 2.
func TestIntentBodyPreservesLargeIntegers(t *testing.T) {
	const bodyJSON = `{` +
		`"stream":false,` +
		`"input":[{"type":"function_result","call_id":"call_1","name":"lookup_order","result":{"order_id":1234567890123456789,"cursor":9007199254740993,"ratio":0.25,"code":"1234567890123456789"}}],` +
		`"response_format":{"type":"text","mime_type":"application/json","schema":{"type":"object","properties":{"bound":{"type":"integer","maximum":9223372036854775807}}}}` +
		`}`

	noteFile := filepath.Join(t.TempDir(), "context.txt")
	if err := os.WriteFile(noteFile, []byte("context note"), 0o644); err != nil {
		t.Fatal(err)
	}

	assertBodyNumbers := func(t *testing.T, body map[string]any, wantInputLen int) {
		t.Helper()
		inputs, ok := body["input"].([]any)
		if !ok || len(inputs) != wantInputLen {
			t.Fatalf("input = %#v, want slice of len %d", body["input"], wantInputLen)
		}
		fnStep, ok := inputs[0].(map[string]any)
		if !ok {
			t.Fatalf("function_result step = %#v, want object", inputs[0])
		}
		if wantInputLen == 2 {
			userStep, ok := inputs[1].(map[string]any)
			if !ok || userStep["type"] != "user_input" {
				t.Fatalf("appended step = %#v, want user_input step after function_result", inputs[1])
			}
		}
		resMap, ok := fnStep["result"].(map[string]any)
		if !ok {
			t.Fatalf("function_result.result = %#v, want object", fnStep["result"])
		}
		if got := resMap["order_id"]; got != json.Number("1234567890123456789") {
			t.Errorf("order_id = %#v (%T), want json.Number(\"1234567890123456789\")", got, got)
		}
		if got := resMap["cursor"]; got != json.Number("9007199254740993") {
			t.Errorf("cursor = %#v (%T), want json.Number(\"9007199254740993\")", got, got)
		}
		if got := resMap["ratio"]; got != json.Number("0.25") {
			t.Errorf("ratio = %#v (%T), want json.Number(\"0.25\")", got, got)
		}
		if got := resMap["code"]; got != "1234567890123456789" {
			t.Errorf("code = %#v (%T), want string \"1234567890123456789\"", got, got)
		}

		rf, ok := body["response_format"].(map[string]any)
		if !ok {
			t.Fatalf("response_format = %#v, want object", body["response_format"])
		}
		schema, ok := rf["schema"].(map[string]any)
		if !ok {
			t.Fatalf("response_format.schema = %#v, want object", rf["schema"])
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("schema.properties = %#v, want object", schema["properties"])
		}
		bound, ok := props["bound"].(map[string]any)
		if !ok {
			t.Fatalf("properties.bound = %#v, want object", props["bound"])
		}
		if got := bound["maximum"]; got != json.Number("9223372036854775807") {
			t.Errorf("schema bound maximum = %#v (%T), want json.Number(\"9223372036854775807\")", got, got)
		}
	}

	modes := []struct {
		name         string
		stdin        string
		args         []string
		wantInputLen int
	}{
		{
			name:         "via --body",
			args:         []string{"generate", "--body", bodyJSON},
			wantInputLen: 1,
		},
		{
			name:         "via stdin",
			stdin:        bodyJSON,
			args:         []string{"generate"},
			wantInputLen: 1,
		},
		{
			name:         "via --body with -i",
			args:         []string{"generate", "-i", noteFile, "--body", bodyJSON},
			wantInputLen: 2,
		},
		{
			name:         "via stdin with -i",
			stdin:        bodyJSON,
			args:         []string{"generate", "-i", noteFile},
			wantInputLen: 2,
		},
		{
			name:         "agent run via --body",
			args:         []string{"agent", "run", "--body", bodyJSON},
			wantInputLen: 1,
		},
	}

	for _, tc := range modes {
		t.Run("dry-run "+tc.name, func(t *testing.T) {
			dryArgs := intentDryRunArgs(tc.args...)
			var res commandResult
			if tc.stdin != "" {
				res = runCLIWithStdin(t, t.TempDir(), nil, tc.stdin, dryArgs...)
			} else {
				res = runCLI(t, t.TempDir(), nil, dryArgs...)
			}
			if res.err != nil {
				t.Fatalf("dry-run failed: %v\nstderr: %s", res.err, res.stderr)
			}
			preview := decodeMapUseNumber(t, []byte(strings.TrimSpace(res.stdout)))
			req, ok := preview["request"].(map[string]any)
			if !ok {
				t.Fatalf("preview.request = %#v, want object", preview["request"])
			}
			body, ok := req["body"].(map[string]any)
			if !ok {
				t.Fatalf("preview.request.body = %#v, want object", req["body"])
			}
			assertBodyNumbers(t, body, tc.wantInputLen)
		})

		t.Run("live wire "+tc.name, func(t *testing.T) {
			captured := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				captured <- b
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"int-1","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"{\"bound\":1}"}]}]}`)
			}))
			defer server.Close()

			liveArgs := append(append(baseArgs(server.URL), "--api-key", "test"), tc.args...)
			var res commandResult
			if tc.stdin != "" {
				res = runCLIWithStdin(t, t.TempDir(), nil, tc.stdin, liveArgs...)
			} else {
				res = runCLI(t, t.TempDir(), nil, liveArgs...)
			}
			if res.err != nil {
				t.Fatalf("live call failed: %v\nstderr: %s", res.err, res.stderr)
			}
			wireBytes := <-captured
			for _, literal := range []string{
				`"order_id":1234567890123456789`,
				`"cursor":9007199254740993`,
				`"maximum":9223372036854775807`,
				`"ratio":0.25`,
				`"code":"1234567890123456789"`,
			} {
				if !bytes.Contains(wireBytes, []byte(literal)) {
					t.Errorf("wire JSON missing %s; got %s", literal, string(wireBytes))
				}
			}
			assertBodyNumbers(t, decodeMapUseNumber(t, wireBytes), tc.wantInputLen)
		})
	}

	invalidCases := []struct {
		name  string
		stdin string
		args  []string
	}{
		{
			name: "trailing JSON object in --body",
			args: []string{"generate", "--body", `{"input":"hi"}{"extra":1}`},
		},
		{
			name: "trailing token in --body with -i",
			args: []string{"generate", "-i", noteFile, "--body", `{"input":"hi"} trailing`},
		},
		{
			name: "invalid JSON in --body with -i",
			args: []string{"generate", "-i", noteFile, "--body", `{"input":`},
		},
		{
			name:  "trailing JSON object on stdin",
			stdin: `{"input":"hi"}{"extra":1}`,
			args:  []string{"generate"},
		},
		{
			name:  "trailing JSON object on stdin with -i",
			stdin: `{"input":"hi"}{"extra":1}`,
			args:  []string{"generate", "-i", noteFile},
		},
		{
			name:  "trailing token on stdin",
			stdin: `{"input":"hi"} trailing`,
			args:  []string{"generate"},
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()

			args := append(append(baseArgs(server.URL), "--api-key", "test"), tc.args...)
			var res commandResult
			if tc.stdin != "" {
				res = runCLIWithStdin(t, t.TempDir(), nil, tc.stdin, args...)
			} else {
				res = runCLI(t, t.TempDir(), nil, args...)
			}
			if code := exitCode(t, res); code != 2 {
				t.Errorf("exit code = %d, want 2\nstdout: %s\nstderr: %s", code, res.stdout, res.stderr)
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("requests sent = %d, want 0", got)
			}
		})
	}
}

// TestIntentBlankModelFlagRejected verifies that explicitly empty or
// whitespace-only -m / --model values on generated intent commands reject
// locally with CLI_VALIDATION (exit code 2) without sending network requests,
// while omitted -m, models/<id> stripping, agent-vs-model dispatch, and custom
// tts/analyze/transcribe fallback behavior remain intact.
func TestIntentBlankModelFlagRejected(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	commands := []struct {
		name        string
		args        []string
		wantDefault string
	}{
		{name: "generate", args: []string{"generate", "hi"}, wantDefault: "gemini-3.8-flash"},
		{name: "image", args: []string{"image", "a cat"}, wantDefault: "gemini-nano-banana-2.1"},
		{name: "video", args: []string{"video", "waves"}, wantDefault: "gemini-omni-1.2-flash"},
		{name: "music", args: []string{"music", "jazz"}, wantDefault: "lyria-3.5"},
		{name: "agent run", args: []string{"agent", "run", "hi"}, wantDefault: "gemini-3.8-flash"},
	}

	for _, cmd := range commands {
		for _, blank := range []string{"", " ", "   ", "\t\n", os.Getenv("UNSET_MODEL_FOR_TEST")} {
			t.Run(fmt.Sprintf("%s dry-run blank=%q", cmd.name, blank), func(t *testing.T) {
				before := requests.Load()
				args := append(append([]string(nil), baseArgs(server.URL)...), "--dry-run", "-o", "json")
				args = append(args, cmd.args...)
				args = append(args, "-m", blank)
				res := runCLI(t, t.TempDir(), nil, args...)
				if code := exitCode(t, res); code != 2 {
					t.Fatalf("exit code = %d, want 2\nstdout: %s\nstderr: %s", code, res.stdout, res.stderr)
				}
				env := decodeEnvelope(t, res.stderr)
				if env.ErrorReason != "CLI_VALIDATION" {
					t.Errorf("error_reason = %q, want CLI_VALIDATION\nstderr: %s", env.ErrorReason, res.stderr)
				}
				if got := requests.Load() - before; got != 0 {
					t.Errorf("requests sent = %d, want 0", got)
				}
			})

			t.Run(fmt.Sprintf("%s sink blank=%q", cmd.name, blank), func(t *testing.T) {
				before := requests.Load()
				args := append(append([]string(nil), baseArgs(server.URL)...), "--api-key", "test", "-o", "json")
				args = append(args, cmd.args...)
				args = append(args, "-m", blank)
				res := runCLI(t, t.TempDir(), nil, args...)
				if code := exitCode(t, res); code != 2 {
					t.Fatalf("exit code = %d, want 2\nstdout: %s\nstderr: %s", code, res.stdout, res.stderr)
				}
				env := decodeEnvelope(t, res.stderr)
				if env.ErrorReason != "CLI_VALIDATION" {
					t.Errorf("error_reason = %q, want CLI_VALIDATION\nstderr: %s", env.ErrorReason, res.stderr)
				}
				if got := requests.Load() - before; got != 0 {
					t.Errorf("requests sent = %d, want 0", got)
				}
			})
		}

		t.Run(cmd.name+" omitted -m retains default", func(t *testing.T) {
			res := runCLI(t, t.TempDir(), nil, intentDryRunArgs(cmd.args...)...)
			if res.err != nil {
				t.Fatalf("omitted -m failed: %v\nstderr: %s", res.err, res.stderr)
			}
			body := parseDryRunBody(t, res.stdout)
			if got := body["model"]; got != cmd.wantDefault {
				t.Errorf("model = %v, want default %q", got, cmd.wantDefault)
			}
		})

		t.Run(cmd.name+" bare and models/ prefixed controls", func(t *testing.T) {
			for _, in := range []string{"gemini-2.5-pro", "models/gemini-2.5-pro"} {
				args := append(append([]string(nil), cmd.args...), "-m", in)
				res := runCLI(t, t.TempDir(), nil, intentDryRunArgs(args...)...)
				if res.err != nil {
					t.Fatalf("-m %q failed: %v\nstderr: %s", in, res.err, res.stderr)
				}
				body := parseDryRunBody(t, res.stdout)
				if got := body["model"]; got != "gemini-2.5-pro" {
					t.Errorf("-m %q -> model = %v, want gemini-2.5-pro", in, got)
				}
			}
		})
	}

	t.Run("agent run with --agent and omitted -m dispatches to agent", func(t *testing.T) {
		res := runCLI(t, t.TempDir(), nil, intentDryRunArgs("agent", "run", "hi", "--agent", "deep-research-pro-preview-12-2025")...)
		if res.err != nil {
			t.Fatalf("agent run --agent failed: %v\nstderr: %s", res.err, res.stderr)
		}
		body := parseDryRunBody(t, res.stdout)
		if got := body["agent"]; got != "deep-research-pro-preview-12-2025" {
			t.Errorf("agent = %v, want deep-research-pro-preview-12-2025", got)
		}
		if _, hasModel := body["model"]; hasModel {
			t.Errorf("unexpected model field in agent run body: %#v", body)
		}
	})

	t.Run("tts analyze and transcribe blank -m fallback does not regress", func(t *testing.T) {
		dir := t.TempDir()
		audioPath := filepath.Join(dir, "clip.wav")
		if err := os.WriteFile(audioPath, []byte("RIFFxxxxWAVEfmt "), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			args      []string
			wantModel string
		}{
			{args: []string{"tts", "hello", "-m", ""}, wantModel: "gemini-3.8-flash-tts"},
			{args: []string{"analyze", "what is this", "-i", audioPath, "-m", "   "}, wantModel: "gemini-3.8-flash"},
			{args: []string{"transcribe", "-i", audioPath, "-m", ""}, wantModel: "gemini-3.5-transcribe"},
		} {
			res := runCLI(t, dir, nil, intentDryRunArgs(tc.args...)...)
			if res.err != nil {
				t.Fatalf("%v failed: %v\nstderr: %s", tc.args, res.err, res.stderr)
			}
			body := parseDryRunBody(t, res.stdout)
			if got := body["model"]; got != tc.wantModel {
				t.Errorf("%v -> model = %v, want %q", tc.args, got, tc.wantModel)
			}
		}
	})
}
