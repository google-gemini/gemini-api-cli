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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/interactions"
)

func TestSameService(t *testing.T) {
	transport := &rawTransport{baseURL: "https://generativelanguage.googleapis.com"}
	local := &rawTransport{baseURL: "http://127.0.0.1:8080"}
	cases := []struct {
		t    *rawTransport
		url  string
		want bool
	}{
		{transport, "https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=x", true},
		{transport, "https://upload.googleapis.com/session", true},
		{transport, "https://googleapis.com/session", true},
		{transport, "HTTPS://Generativelanguage.GoogleApis.com/session", true},
		{transport, "http://generativelanguage.googleapis.com/session", false},
		{transport, "https://googleapis.com.evil.example/session", false},
		{transport, "https://evilgoogleapis.com/session", false},
		{transport, "https://googleapis.com@evil.example/session", false},
		{transport, "https://evil.example/?h=.googleapis.com", false},
		{transport, "/upload/v1beta/files", false},
		{transport, "", false},
		{transport, "://bad", false},
		{local, "http://127.0.0.1:8080/session", true},
		{local, "http://127.0.0.1:9090/session", false},
		{local, "https://127.0.0.1:8080/session", false},
		{local, "http://localhost:8080/session", false},
	}
	for _, tc := range cases {
		if got := tc.t.sameService(tc.url); got != tc.want {
			t.Errorf("sameService(%q) with base %s = %v, want %v", tc.url, tc.t.baseURL, got, tc.want)
		}
	}
}

func TestInlineCost(t *testing.T) {
	if got := inlineCost(3<<20, contentAudio); got != 4<<20 {
		t.Errorf("3 MiB of audio costs %d, want 4 MiB as base64", got)
	}
	if got := inlineCost(3<<20, contentText); got != 3<<20 {
		t.Errorf("3 MiB of text costs %d, want it unchanged", got)
	}
	// The largest accepted media file still fits the request budget encoded.
	largest := int64(maxInlineBytes) / 4 * 3
	if cost := inlineCost(largest, contentVideo); cost > maxInlineBytes || cost+inlineRequestReserve > maxInlineRequestBytes {
		t.Errorf("largest media file costs %d, over the %d budget", cost, maxInlineBytes)
	}
}

func TestCheckLocalFileInlineBoundary(t *testing.T) {
	largest := int64(maxInlineBytes) / 4 * 3
	for name, tc := range map[string]struct {
		file   string
		size   int64
		wantOK bool
	}{
		"media at the cap":   {"a.mp4", largest, true},
		"media over the cap": {"b.mp4", largest + 3, false},
		"text at the cap":    {"a.txt", maxInlineBytes, true},
		"text over the cap":  {"b.txt", maxInlineBytes + 1, false},
	} {
		path := filepath.Join(t.TempDir(), tc.file)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		// Sparse: sized without writing the bytes.
		if err := f.Truncate(tc.size); err != nil {
			t.Fatal(err)
		}
		f.Close()
		_, _, err = checkLocalFile(path, "", "--input[1]", analyzePolicy)
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: checkLocalFile error = %v, want ok=%v", name, err, tc.wantOK)
		}
	}
}

func TestCanonicalRemoteMIME(t *testing.T) {
	for in, want := range map[string]string{
		"video/quicktime":           "video/mov",
		"Video/QuickTime; codecs=x": "video/mov",
		"audio/mp3":                 "audio/mp3",
		" audio/WAV ;rate=1":        "audio/wav",
		"":                          "",
	} {
		if got := canonicalRemoteMIME(in); got != want {
			t.Errorf("canonicalRemoteMIME(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInteractionOutcome(t *testing.T) {
	rejected := []interactions.InteractionStatus{
		interactions.InteractionStatusIncomplete, interactions.InteractionStatusBudgetExceeded,
		interactions.InteractionStatusFailed, interactions.InteractionStatusCancelled,
		interactions.InteractionStatusRequiresAction, interactions.InteractionStatusInProgress,
		interactions.InteractionStatusQueued,
	}
	for _, status := range rejected {
		err := interactionOutcome(&interactions.Interaction{Status: status})
		if err == nil || !strings.Contains(err.Error(), string(status)) {
			t.Errorf("status %q: error = %v, want it rejected by name", status, err)
		}
	}
	for _, status := range []interactions.InteractionStatus{interactions.InteractionStatusCompleted, "", "future_state"} {
		if err := interactionOutcome(&interactions.Interaction{Status: status}); err != nil {
			t.Errorf("status %q: unexpected error %v", status, err)
		}
	}
	if err := interactionOutcome(nil); err == nil {
		t.Error("a nil interaction was accepted")
	}
}

func TestRenderSRTKeepsCuesIntact(t *testing.T) {
	segments := []transcriptSegment{
		{StartTime: "00:00", EndTime: "00:02", Content: "first line\n\n \nsecond line --> still text"},
		{StartTime: "00:02", EndTime: "00:04", Content: "next"},
	}
	srt, err := renderSRT(segments, false)
	if err != nil {
		t.Fatal(err)
	}
	if cues := strings.Split(strings.TrimSpace(srt), "\n\n"); len(cues) != len(segments) {
		t.Errorf("rendered %d cues from %d segments:\n%s", len(cues), len(segments), srt)
	}
	if !strings.Contains(srt, "first line\nsecond line --> still text") {
		t.Errorf("cue text was altered beyond blank lines:\n%s", srt)
	}
}

func FuzzTimestampRoundTrip(f *testing.F) {
	for _, seed := range []int64{0, 999, 62500, 3723004, 359999999} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ms int64) {
		if ms < 0 || ms > 1<<40 {
			t.Skip()
		}
		got, err := parseTimestamp(formatSRTTime(ms))
		if err != nil || got != ms {
			t.Errorf("parseTimestamp(formatSRTTime(%d)) = %d, %v", ms, got, err)
		}
	})
}

func FuzzIdentifierNormalizers(f *testing.F) {
	for _, seed := range []string{"files/abc-123", "models/gemini-2.5-flash", "../x", "files/", "a/b", "%2e%2e", " x "} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for name, normalize := range map[string]func(string) (string, error){
			"file": normalizeFilePositional, "model": normalizeModelPositional,
		} {
			id, err := normalize(in)
			if err != nil {
				continue
			}
			if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\%?# ") {
				t.Errorf("%s normalizer accepted %q as %q, not a plain path segment", name, in, id)
			}
		}
	})
}

func FuzzAudioMIME(f *testing.F) {
	for _, seed := range []string{"audio/l16; rate=24000; channels=1", "audio/L16;rate=;channels=x", ";;;", "audio/mp3"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		parseAudioMIME(in)
		extensionForAudioMIME(in)
	})
}

func TestUploadBytesSendsTheDeclaredSize(t *testing.T) {
	// A source that shrank below the declared size fails before finalizing.
	transport := &rawTransport{}
	_, err := transport.uploadBytes(t.Context(), "http://127.0.0.1:1/session", bytes.NewReader(nil), "gone.bin", 10)
	if err == nil || !strings.Contains(err.Error(), "changed during upload") {
		t.Errorf("error = %v, want the shrunken source named", err)
	}
}
