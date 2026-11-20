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

package custom

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/client"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/sdkerrors"
	"github.com/spf13/cobra"
)

func TestDecideContentClassOf(t *testing.T) {
	cases := []struct {
		mime         string
		wantClass    contentClass
		wantSendable bool
	}{
		{"image/png", contentImage, true},
		{"image/jpeg", contentImage, true},
		{"audio/mp3", contentAudio, true},
		{"audio/wav", contentAudio, true},
		{"video/mp4", contentVideo, true},
		{"video/mov", contentVideo, true},
		{"application/pdf", contentDocument, true},
		// CSV and other text-like files must travel as text blocks in Decisions.
		{"text/csv", contentText, true},
		{"text/plain", contentText, true},
		{"text/markdown", contentText, true},
		{"text/x-python", contentText, true},
		{"application/json", contentText, true},
		{"application/yaml", contentText, true},
		// Non-PDF documents are rejected for Decisions.
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", contentDocument, false},
		{"application/octet-stream", contentDocument, false},
	}
	for _, tc := range cases {
		gotClass, gotSendable := decideContentClassOf(tc.mime)
		if gotClass != tc.wantClass || gotSendable != tc.wantSendable {
			t.Errorf("decideContentClassOf(%q) = (%v, %v), want (%v, %v)", tc.mime, gotClass, gotSendable, tc.wantClass, tc.wantSendable)
		}
	}
}

func TestCheckDecideURIMIME(t *testing.T) {
	for _, allowed := range []string{"", "image/png", "audio/mp3", "video/mp4", "application/pdf"} {
		if err := checkDecideURIMIME(allowed, "test"); err != nil {
			t.Errorf("checkDecideURIMIME(%q) unexpected error: %v", allowed, err)
		}
	}
	for _, textMIME := range []string{"text/plain", "text/csv", "application/json", "text/markdown"} {
		err := checkDecideURIMIME(textMIME, "test")
		if err == nil {
			t.Errorf("checkDecideURIMIME(%q) = nil, want text link refusal", textMIME)
			continue
		}
		var hinted interface{ CLIHints() []string }
		if !errors.As(err, &hinted) || !strings.Contains(strings.Join(hinted.CLIHints(), " "), "--stdin") {
			t.Errorf("checkDecideURIMIME(%q) hints missing --stdin pointer: %v", textMIME, err)
		}
	}
	if err := checkDecideURIMIME("application/vnd.ms-excel", "test"); err == nil {
		t.Error("checkDecideURIMIME(ms-excel) = nil, want error")
	}
}

func TestEnsureTrailingNewline(t *testing.T) {
	if got := ensureTrailingNewline("hello"); got != "hello\n" {
		t.Errorf("ensureTrailingNewline(\"hello\") = %q, want \"hello\\n\"", got)
	}
	if got := ensureTrailingNewline("hello\n"); got != "hello\n" {
		t.Errorf("ensureTrailingNewline(\"hello\\n\") = %q, want \"hello\\n\"", got)
	}
	if got := ensureTrailingNewline(""); got != "" {
		t.Errorf("ensureTrailingNewline(\"\") = %q, want \"\"", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "45")
	if got := parseRetryAfter(&http.Response{Header: h}); got != 45*time.Second {
		t.Errorf("Retry-After seconds = %v, want 45s", got)
	}

	hMs := http.Header{}
	hMs.Set("Retry-After-Ms", "250")
	if got := parseRetryAfter(&http.Response{Header: hMs}); got != 250*time.Millisecond {
		t.Errorf("Retry-After-Ms = %v, want 250ms", got)
	}

	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	hDate := http.Header{}
	hDate.Set("Retry-After", future)
	if got := parseRetryAfter(&http.Response{Header: hDate}); got < 60*time.Second || got > 95*time.Second {
		t.Errorf("Retry-After HTTP-date = %v, want ~90s", got)
	}

	for name, layout := range map[string]string{
		"RFC850":   time.RFC850,
		"ANSIC":    time.ANSIC,
		"RFC1123Z": time.RFC1123Z,
	} {
		hdr := http.Header{}
		hdr.Set("Retry-After", time.Now().Add(75*time.Second).UTC().Format(layout))
		if got := parseRetryAfter(&http.Response{Header: hdr}); got < 50*time.Second || got > 80*time.Second {
			t.Errorf("Retry-After %s = %v, want ~75s", name, got)
		}
	}

	hPast := http.Header{}
	hPast.Set("Retry-After", time.Now().Add(-30*time.Second).UTC().Format(http.TimeFormat))
	if got := parseRetryAfter(&http.Response{Header: hPast}); got != 0 {
		t.Errorf("Retry-After past date = %v, want 0", got)
	}

	// Huge numeric values must clamp to MaxInt64 instead of overflowing negative.
	hHugeSec := http.Header{}
	hHugeSec.Set("Retry-After", "9223372037")
	if got := parseRetryAfter(&http.Response{Header: hHugeSec}); got != time.Duration(math.MaxInt64) {
		t.Errorf("Retry-After overflow seconds = %v, want MaxInt64", got)
	}
	hHugeMs := http.Header{}
	hHugeMs.Set("Retry-After-Ms", "9223372036855")
	if got := parseRetryAfter(&http.Response{Header: hHugeMs}); got != time.Duration(math.MaxInt64) {
		t.Errorf("Retry-After-Ms overflow = %v, want MaxInt64", got)
	}

	for _, raw := range []string{"0", "-5"} {
		hdr := http.Header{}
		hdr.Set("Retry-After", raw)
		if got := parseRetryAfter(&http.Response{Header: hdr}); got != 0 {
			t.Errorf("Retry-After %q = %v, want 0", raw, got)
		}
	}
}

func TestClassifyDecideRetry(t *testing.T) {
	ctx := context.Background()
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		err := sdkerrors.NewSDKDefaultError("err", status, `{}`, &http.Response{StatusCode: status, Header: http.Header{}})
		retryable, _ := classifyDecideRetry(ctx, err, true)
		if !retryable {
			t.Errorf("HTTP %d retryable = false, want true", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 422} {
		err := sdkerrors.NewSDKDefaultError("err", status, `{}`, &http.Response{StatusCode: status, Header: http.Header{}})
		retryable, _ := classifyDecideRetry(ctx, err, true)
		if retryable {
			t.Errorf("HTTP %d retryable = true, want false", status)
		}
	}
	for _, connErr := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		net.ErrClosed,
		syscall.ECONNRESET,
		syscall.EPIPE,
		context.DeadlineExceeded,
		errors.New("http2: client connection lost"),
		errors.New("http2: server sent GOAWAY"),
		errors.New("stream error: stream ID 1; INTERNAL_ERROR"),
	} {
		if retryable, _ := classifyDecideRetry(ctx, connErr, true); !retryable {
			t.Errorf("connErr %v retryable = false, want true", connErr)
		}
		if retryable, _ := classifyDecideRetry(ctx, connErr, false); retryable {
			t.Errorf("connErr %v with retryConn=false retryable = true, want false", connErr)
		}
	}
	if retryable, _ := classifyDecideRetry(ctx, &client.RedirectRefusedError{}, true); retryable {
		t.Error("RedirectRefusedError retryable = true, want false")
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if retryable, _ := classifyDecideRetry(canceledCtx, io.EOF, true); retryable {
		t.Error("canceled context retryable = true, want false")
	}
}

func TestValidateBodyQuestions(t *testing.T) {
	validCases := []struct {
		name string
		raw  any
	}{
		{
			name: "noul string instructions",
			raw: map[string]any{
				"q": map[string]any{"type": "noul", "instructions": "Is this valid?"},
			},
		},
		{
			name: "choice single text block instructions",
			raw: map[string]any{
				"route": map[string]any{
					"type":         "choice",
					"instructions": map[string]any{"type": "text", "text": "Which queue?"},
					"criteria":     map[string]any{"a": "Alpha", "b": "Beta"},
				},
			},
		},
		{
			name: "score list of text blocks instructions",
			raw: map[string]any{
				"urgency": map[string]any{
					"type":         "score",
					"instructions": []any{map[string]any{"type": "text", "text": "Rate urgency."}},
					"criteria":     []any{"Low", "High"},
				},
			},
		},
	}
	for _, tc := range validCases {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			if err := validateBodyQuestions(tc.raw, true); err != nil {
				t.Errorf("validateBodyQuestions unexpected error: %v", err)
			}
		})
	}

	invalidCases := []struct {
		name    string
		present bool
		raw     any
		wantSub string
	}{
		{"missing", false, nil, "missing question"},
		{"null map", true, nil, "missing question"},
		{"empty map", true, map[string]any{}, "at least one question"},
		{"blank id", true, map[string]any{"  ": map[string]any{"type": "noul", "instructions": "Ok?"}}, "cannot be empty"},
		{"null question value", true, map[string]any{"q": nil}, "must be a JSON object"},
		{"null instructions", true, map[string]any{"q": map[string]any{"type": "noul", "instructions": nil}}, "missing required"},
		{"singular instruction typo", true, map[string]any{"q": map[string]any{"type": "noul", "instruction": "Ok?"}}, `plural "instructions"`},
		{"blank string instructions", true, map[string]any{"q": map[string]any{"type": "noul", "instructions": "   "}}, "blank instructions"},
		{"empty list instructions", true, map[string]any{"q": map[string]any{"type": "noul", "instructions": []any{}}}, "empty instructions"},
		{"null element in list instructions", true, map[string]any{"q": map[string]any{"type": "noul", "instructions": []any{nil}}}, "text content block"},
		{"blank block instructions", true, map[string]any{"q": map[string]any{"type": "noul", "instructions": map[string]any{"type": "text", "text": "  "}}}, "blank instructions"},
		{"blank list block instructions", true, map[string]any{"q": map[string]any{"type": "noul", "instructions": []any{map[string]any{"type": "text", "text": "\t"}}}}, "blank instructions"},
		{"unknown question type", true, map[string]any{"q": map[string]any{"type": "bogus", "instructions": "Ok?"}}, `invalid "questions"`},
	}
	for _, tc := range invalidCases {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			err := validateBodyQuestions(tc.raw, tc.present)
			if err == nil {
				t.Fatalf("validateBodyQuestions = nil, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestIsYouTubeURLCaseInsensitive(t *testing.T) {
	for _, yt := range []string{
		"https://www.youtube.com/watch?v=abc",
		"https://www.youtube.com:443/watch?v=abc",
		"HTTPS://YOUTU.BE:443/abc",
		"http://m.youtube.com/watch?v=abc",
	} {
		if !isYouTubeURLCaseInsensitive(yt) {
			t.Errorf("isYouTubeURLCaseInsensitive(%q) = false, want true", yt)
		}
	}
	for _, nonYT := range []string{
		"https://notyoutube.com/watch?v=abc",
		"https://cdn.example.com/youtube.mp4",
		"gs://youtube.com/video.mp4",
	} {
		if isYouTubeURLCaseInsensitive(nonYT) {
			t.Errorf("isYouTubeURLCaseInsensitive(%q) = true, want false", nonYT)
		}
	}
}

func TestRegisterRemovesRawDecisionsGroup(t *testing.T) {
	noopRunE := func(*cobra.Command, []string) error { return nil }
	root := &cobra.Command{Use: "gemini-api"}
	for _, customName := range []string{"tts", "analyze", "decide", "transcribe"} {
		root.AddCommand(&cobra.Command{Use: customName})
	}
	root.AddCommand(&cobra.Command{Use: "decisions"})
	filesGroup := &cobra.Command{Use: "files"}
	for _, leaf := range []string{"get", "delete"} {
		c := &cobra.Command{Use: leaf, RunE: noopRunE}
		c.Flags().String("file", "", "")
		filesGroup.AddCommand(c)
	}
	root.AddCommand(filesGroup)
	modelsGroup := &cobra.Command{Use: "models"}
	modelsGet := &cobra.Command{Use: "get", RunE: noopRunE}
	modelsGet.Flags().String("model", "", "")
	modelsGroup.AddCommand(modelsGet)
	root.AddCommand(modelsGroup)
	voicesGroup := &cobra.Command{Use: "voices"}
	voicesGroup.AddCommand(&cobra.Command{Use: "create", RunE: noopRunE})
	for _, leaf := range []string{"get", "delete"} {
		c := &cobra.Command{Use: leaf, RunE: noopRunE}
		c.Flags().String("id", "", "")
		voicesGroup.AddCommand(c)
	}
	root.AddCommand(voicesGroup)
	envsGroup := &cobra.Command{Use: "environments"}
	envFilesGroup := &cobra.Command{Use: "files"}
	envFilesList := &cobra.Command{Use: "list", RunE: noopRunE}
	envFilesList.Flags().String("environment", "", "")
	envFilesList.Flags().String("path", "", "")
	envFilesGroup.AddCommand(envFilesList)
	envsGroup.AddCommand(envFilesGroup)
	root.AddCommand(envsGroup)
	for _, intent := range []string{"generate", "image", "video", "music"} {
		c := &cobra.Command{Use: intent, RunE: noopRunE}
		c.Flags().String("body", "", "")
		root.AddCommand(c)
	}
	agentGroup := &cobra.Command{Use: "agent"}
	agentRun := &cobra.Command{Use: "run", RunE: noopRunE}
	agentRun.Flags().String("body", "", "")
	agentGroup.AddCommand(agentRun)
	root.AddCommand(agentGroup)

	Register(root)

	if findChild(root, "decide") == nil {
		t.Fatal("expected decide command to remain registered")
	}
	if findChild(root, "decisions") != nil {
		t.Fatal("expected raw decisions command to be removed by Register")
	}
}
