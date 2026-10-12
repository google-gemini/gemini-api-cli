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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnalyzeMultiInputRequestAndEnvelope(t *testing.T) {
	var generateBody map[string]any
	var fileGets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1beta/interactions":
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &generateBody); err != nil {
				t.Errorf("decode interaction request: %v", err)
			}
			fmt.Fprint(w, completedInteraction("combined answer", `{"total_input_tokens":12,"total_output_tokens":3,"total_tokens":15}`))
		case r.URL.Path == "/v1beta/files/abc123":
			fileGets.Add(1)
			fmt.Fprint(w, `{"name":"files/abc123","uri":"https://files.example/abc123","mimeType":"video/mp4","state":"ACTIVE"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	imagePath := filepath.Join(dir, "small.png")
	if err := os.WriteFile(imagePath, []byte("small png"), 0o644); err != nil {
		t.Fatal(err)
	}
	youtube := "https://youtu.be/dQw4w9WgXcQ"
	question := "What happens at 1:00, and why?"
	args := append(plainArgs(server.URL), "--output-format", "json", "analyze",
		"-i", imagePath, "-i", "files/abc123", "-i", youtube, question)
	result := runCLI(t, dir, nil, args...)
	if result.err != nil {
		t.Fatalf("analyze failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if fileGets.Load() != 1 {
		t.Fatalf("files.get calls = %d, want 1", fileGets.Load())
	}

	// The interaction input is a flat ordered array of content blocks:
	// the three media inputs followed by the text question.
	parts, ok := generateBody["input"].([]any)
	if !ok || len(parts) != 4 {
		t.Fatalf("request input = %#v, want four ordered content blocks", generateBody["input"])
	}
	part0, _ := parts[0].(map[string]any)
	part1, _ := parts[1].(map[string]any)
	part2, _ := parts[2].(map[string]any)
	part3, _ := parts[3].(map[string]any)
	// Local file: inline base64 data.
	if part0["type"] != "image" || part0["data"] == nil {
		t.Errorf("block 1 is not an inline image block: %#v", part0)
	}
	// Resolved Files API input: video block referenced by uri.
	if part1["type"] != "video" || !strings.Contains(fmt.Sprint(part1["uri"]), "files.example/abc123") {
		t.Errorf("block 2 is not the resolved Files API input: %#v", part1)
	}
	// YouTube: video block referenced by uri.
	if part2["type"] != "video" || !strings.Contains(fmt.Sprint(part2["uri"]), youtube) {
		t.Errorf("block 3 is not the YouTube input: %#v", part2)
	}
	if part3["type"] != "text" || part3["text"] != question {
		t.Errorf("last block = %#v, want text %q", part3, question)
	}

	var envelope struct {
		Model     string   `json:"model"`
		Inputs    []string `json:"inputs"`
		MIMETypes []string `json:"mime_types"`
		Question  string   `json:"question"`
		Text      string   `json:"text"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, result.stdout)
	}
	if len(envelope.Inputs) != 3 || envelope.Inputs[1] != "files/abc123" || envelope.Inputs[2] != youtube {
		t.Errorf("envelope inputs = %#v", envelope.Inputs)
	}
	if envelope.Question != question || envelope.Text != "combined answer" {
		t.Errorf("envelope question/text = %q/%q", envelope.Question, envelope.Text)
	}
	if len(envelope.MIMETypes) != 2 || envelope.MIMETypes[0] != "image/png" || envelope.MIMETypes[1] != "video/mp4" {
		t.Errorf("envelope mime_types = %#v", envelope.MIMETypes)
	}
}

func TestAnalyzeRemoteFileErrorPreservesSDKEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/files/missing" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":404,"message":"File missing","status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"FILE_NOT_FOUND"}]}}`)
	}))
	defer server.Close()

	args := append(plainArgs(server.URL), "--output-format", "json", "analyze", "-i", "files/missing", "q")
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err == nil {
		t.Fatal("analyze unexpectedly succeeded on files.get 404")
	}
	// The SDK error must reach output.Error unwrapped: its status code and
	// body are extracted by reflection, so a fmt.Errorf wrapper would drop
	// both from the envelope.
	var envelope struct {
		StatusCode int `json:"status_code"`
		Error      struct {
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
		t.Fatalf("stderr is not one JSON envelope: %v\n%s", err, result.stderr)
	}
	if envelope.StatusCode != http.StatusNotFound {
		t.Errorf("status_code = %d, want 404", envelope.StatusCode)
	}
	if envelope.Error.Status != "NOT_FOUND" || len(envelope.Error.Details) != 1 || envelope.Error.Details[0].Reason != "FILE_NOT_FOUND" {
		t.Errorf("API error body was not preserved in the envelope:\n%s", result.stderr)
	}
}

func TestAnalyzeInputValidation(t *testing.T) {
	t.Run("question without input", func(t *testing.T) {
		result := runCLI(t, t.TempDir(), nil, append(plainArgs("https://example.invalid"), "analyze", "why?")...)
		if result.err == nil || !strings.Contains(result.stderr, "--input is required") {
			t.Fatalf("result = err %v, stderr %q", result.err, result.stderr)
		}
	})

	t.Run("mime override with multiple inputs", func(t *testing.T) {
		dir := t.TempDir()
		a := filepath.Join(dir, "a.png")
		b := filepath.Join(dir, "b.png")
		_ = os.WriteFile(a, []byte("a"), 0o644)
		_ = os.WriteFile(b, []byte("b"), 0o644)
		args := append(plainArgs("https://example.invalid"), "analyze", "-i", a, "-i", b, "--mime-type", "image/png")
		result := runCLI(t, dir, nil, args...)
		if result.err == nil || !strings.Contains(result.stderr, "--mime-type can only be used with exactly one --input") {
			t.Fatalf("result = err %v, stderr %q", result.err, result.stderr)
		}
	})

	t.Run("cumulative inline limit", func(t *testing.T) {
		dir := t.TempDir()
		a := filepath.Join(dir, "a.png")
		b := filepath.Join(dir, "b.png")
		payload := bytes.Repeat([]byte{'x'}, 11<<20)
		if err := os.WriteFile(a, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		args := append(plainArgs("https://example.invalid"), "analyze", "-i", a, "-i", b)
		result := runCLI(t, dir, nil, args...)
		if result.err == nil {
			t.Fatal("analyze unexpectedly accepted 22 MB of inline inputs")
		}
		for _, want := range []string{`--input[2]`, b, "cumulative inline inputs (base64 included) exceed this CLI's 20 MB request budget"} {
			if !strings.Contains(result.stderr, want) {
				t.Errorf("stderr missing %q:\n%s", want, result.stderr)
			}
		}
	})

	t.Run("single file over cap", func(t *testing.T) {
		// A lone local file above the inline cap is rejected up front (no
		// auto-upload): the porcelain hints the user to the Files API rather
		// than inlining 20+ MB of base64 into an interaction request.
		dir := t.TempDir()
		big := filepath.Join(dir, "big.png")
		if err := os.WriteFile(big, bytes.Repeat([]byte{'x'}, 21<<20), 0o644); err != nil {
			t.Fatal(err)
		}
		args := append(plainArgs("https://example.invalid"), "analyze", "-i", big)
		result := runCLI(t, dir, nil, args...)
		if result.err == nil {
			t.Fatal("analyze unexpectedly accepted a 21 MB inline input")
		}
		if code := exitCode(t, result); code != 2 {
			t.Errorf("exit code = %d, want 2", code)
		}
		for _, want := range []string{`--input[1]`, "this CLI keeps inline requests under 20 MB"} {
			if !strings.Contains(result.stderr, want) {
				t.Errorf("stderr missing %q:\n%s", want, result.stderr)
			}
		}
	})

	t.Run("existing path beats the files/ spelling", func(t *testing.T) {
		// A local ./files/abc is a file, never a Files API lookup.
		var lookups atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/files/") {
				lookups.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, completedInteraction("ok", ""))
		}))
		defer server.Close()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "files"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "files", "abc"), []byte("plain"), 0o644); err != nil {
			t.Fatal(err)
		}
		args := append(plainArgs(server.URL), "analyze", "-i", "files/abc", "--mime-type", "text/plain")
		result := runCLIInDir(t, t.TempDir(), dir, nil, args...)
		if result.err != nil {
			t.Fatalf("analyze failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if lookups.Load() != 0 {
			t.Errorf("files.get lookups = %d, want none for an existing local path", lookups.Load())
		}
	})

	t.Run("bare id is a path", func(t *testing.T) {
		result := runCLI(t, t.TempDir(), nil, append(plainArgs("https://example.invalid"), "analyze", "-i", "abc123")...)
		if result.err == nil {
			t.Fatal("bare id unexpectedly resolved through the Files API")
		}
		for _, want := range []string{`--input[1] "abc123"`, "file not found", "files/<id>"} {
			if !strings.Contains(result.stderr, want) {
				t.Errorf("stderr missing %q: %s", want, result.stderr)
			}
		}
	})
}

func TestAnalyzeBareNonTTYAndAgentMode(t *testing.T) {
	t.Run("empty pipe is a usage error without hanging", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, cliBinary, "analyze")
		cmd.Env = isolatedEnv(t.TempDir(), nil)
		cmd.Stdin = strings.NewReader("")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatal("bare analyze hung for 20 seconds")
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Fatalf("bare analyze err = %v, want exit code 2\nstderr: %s", err, stderr.String())
		}
		if strings.TrimSpace(stdout.String()) != "" {
			t.Errorf("bare analyze wrote stdout: %s", stdout.String())
		}
		if !strings.Contains(stderr.String(), "Usage:") || !strings.Contains(stderr.String(), "analyze [question]") {
			t.Errorf("stderr does not contain analyze help:\n%s", stderr.String())
		}
	})

	t.Run("agent mode matches generated bare intent", func(t *testing.T) {
		result := runCLI(t, t.TempDir(), nil, "--agent-mode", "analyze")
		var exitErr *exec.ExitError
		if !errors.As(result.err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Fatalf("bare agent-mode analyze err = %v, want exit code 2\nstderr: %s", result.err, result.stderr)
		}
		if !strings.Contains(result.stderr, "\"exit_code\": 2") {
			t.Errorf("bare agent-mode analyze envelope lacks exit_code 2:\n%s", result.stderr)
		}
	})
}

func TestTranscribeMultipleInputs(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, completedInteraction(fmt.Sprintf("transcript %d", n), fmt.Sprintf(`{"total_tokens":%d}`, n)))
	}))
	defer server.Close()

	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp3")
	b := filepath.Join(dir, "b.mp3")
	_ = os.WriteFile(a, []byte("audio a"), 0o644)
	_ = os.WriteFile(b, []byte("audio b"), 0o644)

	plainOut := filepath.Join(dir, "plain")
	plain := runCLI(t, dir, nil, append(plainArgs(server.URL), "transcribe", "-i", a, "-i", b, "--out", plainOut)...)
	if plain.err != nil {
		t.Fatalf("plain transcribe failed: %v\nstderr: %s", plain.err, plain.stderr)
	}
	wantPlain := []string{filepath.Join(plainOut, "a-1.md"), filepath.Join(plainOut, "b-2.md")}
	if got := strings.Split(strings.TrimSpace(plain.stdout), "\n"); len(got) != 2 || got[0] != wantPlain[0] || got[1] != wantPlain[1] {
		t.Errorf("stdout paths = %#v, want %#v", got, wantPlain)
	}

	jsonOut := filepath.Join(dir, "json")
	jsonArgs := append(plainArgs(server.URL), "--output-format", "json", "transcribe", "-i", a, "-i", b, "--out", jsonOut)
	enveloped := runCLI(t, dir, nil, jsonArgs...)
	if enveloped.err != nil {
		t.Fatalf("JSON transcribe failed: %v\nstderr: %s", enveloped.err, enveloped.stderr)
	}
	var envelope struct {
		Model   string `json:"model"`
		Format  string `json:"format"`
		Results []struct {
			Input     string         `json:"input"`
			Path      string         `json:"path"`
			SizeBytes int            `json:"size_bytes"`
			MIMEType  string         `json:"mime_type"`
			Usage     map[string]any `json:"usage"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(enveloped.stdout), &envelope); err != nil {
		t.Fatalf("decode transcribe envelope: %v\n%s", err, enveloped.stdout)
	}
	if len(envelope.Results) != 2 {
		t.Fatalf("results length = %d, want 2", len(envelope.Results))
	}
	for i, result := range envelope.Results {
		want := filepath.Join(jsonOut, fmt.Sprintf("%c-%d.md", 'a'+rune(i), i+1))
		if result.Path != want || result.SizeBytes == 0 || result.MIMEType != "audio/mp3" || result.Usage == nil {
			t.Errorf("result %d = %+v, want path %s with metadata", i, result, want)
		}
		if _, err := os.Stat(result.Path); err != nil {
			t.Errorf("result %d artifact missing: %v", i, err)
		}
	}
	if calls.Load() != 4 {
		t.Errorf("generateContent calls = %d, want four across the plain and JSON runs", calls.Load())
	}
}

// TestTranscribeSecondRequestErrorPreservesFirstArtifactAndNamesInput: a later
// failure keeps the earlier artifact and names it in every error rendering;
// under a structured format stderr stays a single JSON document.
func TestTranscribeSecondRequestErrorPreservesFirstArtifactAndNamesInput(t *testing.T) {
	for _, mode := range []struct {
		name  string
		flags []string
	}{{"human", nil}, {"json envelope", []string{"--output-format", "json"}}} {
		t.Run(mode.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					fmt.Fprint(w, completedInteraction("first transcript", ""))
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":{"code":500,"message":"transcription failed","status":"INTERNAL"}}`)
			}))
			defer server.Close()

			dir := t.TempDir()
			a := filepath.Join(dir, "a.mp3")
			b := filepath.Join(dir, "b.mp3")
			_ = os.WriteFile(a, []byte("audio a"), 0o644)
			_ = os.WriteFile(b, []byte("audio b"), 0o644)
			out := filepath.Join(dir, "transcripts")
			args := append(append(plainArgs(server.URL), mode.flags...), "transcribe", "-i", a, "-i", b, "--out", out)
			result := runCLI(t, dir, nil, args...)
			if code := exitCode(t, result); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if calls.Load() != 2 {
				t.Errorf("interaction calls = %d, want 2", calls.Load())
			}
			if result.stdout != "" {
				t.Errorf("failure wrote stdout: %q", result.stdout)
			}
			firstPath := filepath.Join(out, "a-1.md")
			if data, err := os.ReadFile(firstPath); err != nil {
				t.Errorf("first artifact was not preserved: %v", err)
			} else if string(data) != "first transcript\n" {
				t.Errorf("first artifact = %q", data)
			}
			if _, err := os.Stat(filepath.Join(out, "b-2.md")); !os.IsNotExist(err) {
				t.Errorf("second artifact should not exist; stat error = %v", err)
			}
			completed := "Completed before the failure: " + firstPath
			if mode.flags == nil {
				for _, want := range []string{fmt.Sprintf(`--input[2] %q: request failed`, b), completed} {
					if !strings.Contains(result.stderr, want) {
						t.Errorf("stderr missing %q:\n%s", want, result.stderr)
					}
				}
				return
			}
			var envelope struct {
				Hints []string `json:"hints"`
			}
			if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil {
				t.Fatalf("stderr is not a single JSON document: %v\n%s", err, result.stderr)
			}
			if !slices.Contains(envelope.Hints, completed) {
				t.Errorf("error envelope hints = %q, want %q among them", envelope.Hints, completed)
			}
		})
	}
}

func TestTranscribeDryRunDoesNotCreateOutDirectory(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp3")
	b := filepath.Join(dir, "b.mp3")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "transcripts")
	args := append(plainArgs("https://example.invalid"), "--dry-run", "transcribe", "-i", a, "-i", b, "--out", out)
	result := runCLI(t, t.TempDir(), nil, args...)
	if result.err != nil {
		t.Fatalf("dry-run transcribe failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if strings.Count(result.stderr, "[DRY-RUN] Would send") != 2 {
		t.Errorf("expected two previewed requests:\n%s", result.stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("--dry-run created the --out directory %s (err=%v)", out, err)
	}
}

func TestTranscribeMultiOutMustBeDirectory(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp3")
	b := filepath.Join(dir, "b.mp3")
	out := filepath.Join(dir, "existing.md")
	_ = os.WriteFile(a, []byte("a"), 0o644)
	_ = os.WriteFile(b, []byte("b"), 0o644)
	_ = os.WriteFile(out, []byte("existing"), 0o644)
	args := append(plainArgs("https://example.invalid"), "transcribe", "-i", a, "-i", b, "--out", out)
	result := runCLI(t, dir, nil, args...)
	if result.err == nil || !strings.Contains(result.stderr, "must be a directory") {
		t.Fatalf("result = err %v, stderr %q", result.err, result.stderr)
	}
}

// TestTranscribeAcceptsOnlyAudioVideo pins the input-class guard: anything but
// audio or video is rejected by name, local files before any request is sent
// and uploaded files before the interaction is.
func TestTranscribeAcceptsOnlyAudioVideo(t *testing.T) {
	serve := func(t *testing.T, fileMeta string) (*httptest.Server, *[]string) {
		t.Helper()
		var paths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "/files/") {
				fmt.Fprint(w, fileMeta)
				return
			}
			fmt.Fprint(w, completedInteraction("hello", ""))
		}))
		t.Cleanup(server.Close)
		return server, &paths
	}

	for _, name := range []string{"shot.png", "paper.pdf", "table.csv", "notes.txt"} {
		t.Run("local "+name, func(t *testing.T) {
			server, paths := serve(t, `{}`)
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
			result := runCLI(t, dir, nil, append(plainArgs(server.URL), "transcribe", "-i", path)...)
			if code := exitCode(t, result); code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
			}
			for _, want := range []string{`--input[1]`, "only audio or video inputs are accepted", "gemini-api analyze"} {
				if !strings.Contains(result.stderr, want) {
					t.Errorf("stderr missing %q:\n%s", want, result.stderr)
				}
			}
			if len(*paths) != 0 {
				t.Errorf("requests sent = %v, want none", *paths)
			}
		})
	}

	t.Run("mime override of another class", func(t *testing.T) {
		server, paths := serve(t, `{"name":"files/abc123","mimeType":"audio/mp3","state":"ACTIVE"}`)
		args := append(plainArgs(server.URL), "transcribe", "-i", "files/abc123", "--mime-type", "image/png")
		result := runCLI(t, t.TempDir(), nil, args...)
		if code := exitCode(t, result); code != 2 {
			t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
		}
		if !strings.Contains(result.stderr, "only audio or video inputs are accepted; got image/png") {
			t.Errorf("stderr = %q", result.stderr)
		}
		if len(*paths) != 0 {
			t.Errorf("requests sent = %v, want none", *paths)
		}
	})

	remote := []struct {
		name, meta, wantStderr string
		wantCode               int
	}{
		{"uploaded image", `{"name":"files/abc123","uri":"https://files.example/abc123","mimeType":"image/png","state":"ACTIVE"}`, "only audio or video inputs are accepted; got image/png", 2},
		{"uploaded file without a MIME type", `{"name":"files/abc123","uri":"https://files.example/abc123","state":"ACTIVE"}`, "reports no MIME type", 1},
	}
	for _, tc := range remote {
		t.Run(tc.name, func(t *testing.T) {
			server, paths := serve(t, tc.meta)
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(server.URL), "transcribe", "-i", "files/abc123")...)
			if code := exitCode(t, result); code != tc.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr: %s", code, tc.wantCode, result.stderr)
			}
			if !strings.Contains(result.stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", result.stderr, tc.wantStderr)
			}
			if len(*paths) != 1 || !strings.Contains((*paths)[0], "/files/abc123") {
				t.Errorf("requests sent = %v, want only the files.get lookup", *paths)
			}
		})
	}

	t.Run("uploaded video still passes", func(t *testing.T) {
		server, paths := serve(t, `{"name":"files/abc123","uri":"https://files.example/abc123","mimeType":"video/mp4","state":"ACTIVE"}`)
		dir := t.TempDir()
		args := append(plainArgs(server.URL), "transcribe", "-i", "files/abc123", "--out", filepath.Join(dir, "t.md"))
		if result := runCLI(t, dir, nil, args...); result.err != nil {
			t.Fatalf("transcribe failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if len(*paths) != 2 {
			t.Errorf("requests sent = %v, want the lookup and the interaction", *paths)
		}
	})
}

// TestRemoteFileMustBeActive pins that an uploaded input is dispatched only in
// the explicit ACTIVE state: every other state, including none, is a runtime
// error naming the state, and the interaction is never sent.
func TestRemoteFileMustBeActive(t *testing.T) {
	cases := []struct {
		name, state, wantStderr string
	}{
		{"absent", ``, "is not ACTIVE (state: unset)"},
		{"unspecified", `,"state":"STATE_UNSPECIFIED"`, "is not ACTIVE (state: STATE_UNSPECIFIED)"},
		{"unknown", `,"state":"SOMETHING_NEW"`, "is not ACTIVE (state: SOMETHING_NEW)"},
		{"processing", `,"state":"PROCESSING"`, "is still processing"},
		{"failed", `,"state":"FAILED"`, "failed processing on the server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"name":"files/abc123","uri":"https://files.example/abc123","mimeType":"audio/mp3"%s}`, tc.state)
			}))
			defer server.Close()
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(server.URL), "analyze", "-i", "files/abc123", "what is said?")...)
			if code := exitCode(t, result); code != 1 {
				t.Errorf("exit code = %d, want 1\nstderr: %s", code, result.stderr)
			}
			if !strings.Contains(result.stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", result.stderr, tc.wantStderr)
			}
			if len(paths) != 1 {
				t.Errorf("requests sent = %v, want only the files.get lookup", paths)
			}
		})
	}
}

// TestPorcelainModelOverride pins the --model guard: an override that is not a
// single model id is a usage error before any request, and a models/ prefix is
// stripped from a valid one.
func TestPorcelainModelOverride(t *testing.T) {
	for _, model := range []string{"models/", "/", "models/a/b", "a/b"} {
		t.Run("rejects "+model, func(t *testing.T) {
			stub := newInteractionStub(t, completedInteraction("ok", ""))
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(stub.URL), "tts", "hello", "--model", model)...)
			if code := exitCode(t, result); code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
			}
			if !strings.Contains(result.stderr, "--model: invalid model id") {
				t.Errorf("stderr = %q, want the --model usage error", result.stderr)
			}
			if n := stub.requests.Load(); n != 0 {
				t.Errorf("requests sent = %d, want none", n)
			}
		})
	}

	for _, model := range []string{"models/gemini-x", "gemini-x"} {
		t.Run("accepts "+model, func(t *testing.T) {
			stub := newInteractionStub(t, ttsAudioResponse([]byte{0x01, 0x02}, "audio/l16", 24000, 1))
			dir := t.TempDir()
			args := append(plainArgs(stub.URL), "tts", "hello", "--model", model, "--out", filepath.Join(dir, "m.wav"))
			if result := runCLI(t, dir, nil, args...); result.err != nil {
				t.Fatalf("tts failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if stub.body["model"] != "gemini-x" {
				t.Errorf("request model = %#v, want gemini-x", stub.body["model"])
			}
		})
	}
}
