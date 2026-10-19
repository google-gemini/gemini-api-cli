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
	raw, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("request input = %#v, want an array of content blocks", body["input"])
	}
	parts := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		m, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("input part = %#v, want an object", p)
		}
		parts = append(parts, m)
	}
	return parts
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
