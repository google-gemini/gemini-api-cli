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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const uploadChunk = 8 << 20

// uploadRequest is one request the resumable-upload mock received.
type uploadRequest struct {
	method  string
	path    string
	query   string
	command string
	headers http.Header
	body    []byte
}

// uploadMock records every request of the resumable protocol. respond may
// override the default well-behaved answers; returning false falls through.
type uploadMock struct {
	*httptest.Server
	mu       sync.Mutex
	requests []uploadRequest
	respond  func(w http.ResponseWriter, r uploadRequest) bool
}

func newUploadMock(t *testing.T) *uploadMock {
	t.Helper()
	mock := &uploadMock{}
	mock.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := uploadRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, command: r.Header.Get("X-Goog-Upload-Command"),
			headers: r.Header.Clone(), body: body,
		}
		mock.mu.Lock()
		mock.requests = append(mock.requests, req)
		mock.mu.Unlock()
		if mock.respond != nil && mock.respond(w, req) {
			return
		}
		switch req.command {
		case "start":
			w.Header().Set("X-Goog-Upload-Url", mock.URL+"/upload-session")
		case "upload":
			w.Header().Set("X-Goog-Upload-Status", "active")
		case "upload, finalize":
			w.Header().Set("X-Goog-Upload-Status", "final")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"file":{"name":"files/xyz789","uri":"https://example/files/xyz789","mimeType":"audio/mp3","state":"ACTIVE"}}`)
		default:
			t.Errorf("unexpected upload command %q", req.command)
		}
	}))
	t.Cleanup(mock.Close)
	return mock
}

func (m *uploadMock) recorded() []uploadRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uploadRequest(nil), m.requests...)
}

// chunks returns the recorded requests that carried file bytes.
func (m *uploadMock) chunks() []uploadRequest {
	var out []uploadRequest
	for _, r := range m.recorded() {
		if r.command != "start" {
			out = append(out, r)
		}
	}
	return out
}

func patterned(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}

// TestFilesUploadProtocol pins the resumable exchange end to end: the start
// request's shape, and that the live chunk loop sends exactly the file's bytes
// at the right offsets with "finalize" riding only the last chunk.
func TestFilesUploadProtocol(t *testing.T) {
	t.Run("start and single chunk", func(t *testing.T) {
		mock := newUploadMock(t)
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "files", "upload", src, "--display-name", "Clip")...)
		if result.err != nil {
			t.Fatalf("files upload failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if got := strings.TrimSpace(result.stdout); got != "files/xyz789" {
			t.Errorf("stdout = %q, want files/xyz789", got)
		}
		reqs := mock.recorded()
		if len(reqs) != 2 {
			t.Fatalf("requests = %d, want start + one chunk", len(reqs))
		}
		start, chunk := reqs[0], reqs[1]
		if start.method != http.MethodPost || start.path != "/upload/v1beta/files" {
			t.Errorf("start = %s %s, want POST /upload/v1beta/files", start.method, start.path)
		}
		var meta struct {
			File struct {
				DisplayName string `json:"display_name"`
			} `json:"file"`
		}
		if err := json.Unmarshal(start.body, &meta); err != nil || meta.File.DisplayName != "Clip" {
			t.Errorf("start metadata = %s (err %v), want display_name Clip", start.body, err)
		}
		for header, want := range map[string]string{
			"X-Goog-Upload-Protocol":              "resumable",
			"X-Goog-Upload-Header-Content-Length": "16",
			"X-Goog-Upload-Header-Content-Type":   "audio/mp3",
			"X-Goog-Api-Key":                      "test-key",
		} {
			if got := start.headers.Get(header); got != want {
				t.Errorf("start %s = %q, want %q", header, got, want)
			}
		}
		if chunk.method != http.MethodPost || chunk.path != "/upload-session" {
			t.Errorf("chunk = %s %s, want POST /upload-session", chunk.method, chunk.path)
		}
		if chunk.command != "upload, finalize" || chunk.headers.Get("X-Goog-Upload-Offset") != "0" {
			t.Errorf("chunk command/offset = %q/%q", chunk.command, chunk.headers.Get("X-Goog-Upload-Offset"))
		}
		if chunk.headers.Get("X-Goog-Api-Key") != "test-key" {
			t.Error("chunk request did not carry the resolved API key")
		}
		if string(chunk.body) != "some audio bytes" {
			t.Errorf("chunk body = %q", chunk.body)
		}
	})

	sizes := []struct {
		name         string
		size         int
		wantOffsets  []string
		wantCommands []string
	}{
		{"exact chunk multiple", uploadChunk, []string{"0"}, []string{"upload, finalize"}},
		{"three chunks", 2*uploadChunk + 5, []string{"0", "8388608", "16777216"}, []string{"upload", "upload", "upload, finalize"}},
	}
	for _, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			mock := newUploadMock(t)
			data := patterned(tc.size)
			src := filepath.Join(t.TempDir(), "data.bin")
			if err := os.WriteFile(src, data, 0o644); err != nil {
				t.Fatal(err)
			}
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "files", "upload", src, "--mime-type", "application/octet-stream")...)
			if result.err != nil {
				t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
			}
			chunks := mock.chunks()
			if len(chunks) != len(tc.wantOffsets) {
				t.Fatalf("chunk requests = %d, want %d", len(chunks), len(tc.wantOffsets))
			}
			var sent []byte
			for i, chunk := range chunks {
				if got := chunk.headers.Get("X-Goog-Upload-Offset"); got != tc.wantOffsets[i] {
					t.Errorf("chunk %d offset = %q, want %q", i, got, tc.wantOffsets[i])
				}
				if chunk.command != tc.wantCommands[i] {
					t.Errorf("chunk %d command = %q, want %q", i, chunk.command, tc.wantCommands[i])
				}
				sent = append(sent, chunk.body...)
			}
			if !bytes.Equal(sent, data) {
				t.Errorf("uploaded %d bytes that differ from the %d-byte source", len(sent), len(data))
			}
		})
	}
}

// TestFilesUploadFailures pins every way the exchange can go wrong: a
// classified exit code, a named cause, nothing on stdout.
func TestFilesUploadFailures(t *testing.T) {
	status := func(code int, body string) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			fmt.Fprint(w, body)
		}
	}
	cases := []struct {
		name       string
		size       int
		on         string // command the override answers
		answer     func(http.ResponseWriter)
		wantStderr string
		wantChunks int
		wantCode   int
	}{
		{"start rejected", 16, "start", status(http.StatusForbidden, `{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`), "denied", 0, 3},
		{"start without session URL", 16, "start", func(w http.ResponseWriter) {}, "did not return an upload URL", 0, 1},
		{"start redirected", 16, "start", func(w http.ResponseWriter) {
			w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
			w.WriteHeader(http.StatusFound)
		}, "API error occurred", 0, 1},
		{"chunk server error", uploadChunk + 1, "upload", status(http.StatusInternalServerError, `{"error":{"code":500,"message":"chunk exploded","status":"INTERNAL"}}`), "chunk exploded", 1, 1},
		{"chunk session cancelled", uploadChunk + 1, "upload", func(w http.ResponseWriter) {
			w.Header().Set("X-Goog-Upload-Status", "cancelled")
		}, `upload interrupted: server status is "cancelled" at offset 8388608`, 1, 1},
		{"finalize not final", 16, "upload, finalize", func(w http.ResponseWriter) {
			w.Header().Set("X-Goog-Upload-Status", "active")
		}, `upload finalized but server status is "active"`, 1, 1},
		{"finalize body not JSON", 16, "upload, finalize", func(w http.ResponseWriter) {
			w.Header().Set("X-Goog-Upload-Status", "final")
			fmt.Fprint(w, "<html>")
		}, "response was not valid JSON", 1, 1},
		{"finalize without file name", 16, "upload, finalize", func(w http.ResponseWriter) {
			w.Header().Set("X-Goog-Upload-Status", "final")
			fmt.Fprint(w, `{"file":{}}`)
		}, "response had no file name", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newUploadMock(t)
			mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
				if r.command != tc.on {
					return false
				}
				tc.answer(w)
				return true
			}
			src := filepath.Join(t.TempDir(), "data.bin")
			if err := os.WriteFile(src, patterned(tc.size), 0o644); err != nil {
				t.Fatal(err)
			}
			result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "files", "upload", src, "--mime-type", "application/octet-stream")...)
			if code := exitCode(t, result); code != tc.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr: %s", code, tc.wantCode, result.stderr)
			}
			if result.stdout != "" {
				t.Errorf("failure wrote stdout: %q", result.stdout)
			}
			if !strings.Contains(result.stderr, tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, result.stderr)
			}
			if got := len(mock.chunks()); got != tc.wantChunks {
				t.Errorf("chunk requests = %d, want %d", got, tc.wantChunks)
			}
		})
	}
}

// TestFilesUploadRefusesForeignSession: a session URL on another host must
// never receive the credentials or the file bytes.
func TestFilesUploadRefusesForeignSession(t *testing.T) {
	var foreignHits int
	var mu sync.Mutex
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		foreignHits++
		mu.Unlock()
	}))
	defer foreign.Close()
	// Same loopback host, different port: still a different service.
	mock := newUploadMock(t)
	mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
		if r.command != "start" {
			return false
		}
		w.Header().Set("X-Goog-Upload-Url", foreign.URL+"/steal")
		return true
	}
	src := writeTempFile(t, "clip.mp3", "some audio bytes")
	result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "files", "upload", src)...)
	if code := exitCode(t, result); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(result.stderr, "refusing to upload to an unexpected host") {
		t.Errorf("stderr = %q", result.stderr)
	}
	if foreignHits != 0 || len(mock.chunks()) != 0 {
		t.Errorf("foreign hits = %d, chunk requests = %d; want none", foreignHits, len(mock.chunks()))
	}
}

// TestFilesUploadTransportParity holds the hand-written transport to the
// generated client's global options: one credential chosen by source rank,
// and the same API-parameter headers on the start and the chunk requests.
func TestFilesUploadTransportParity(t *testing.T) {
	withoutKey := func(serverURL string) []string {
		var args []string
		base := plainArgs(serverURL)
		for i := 0; i < len(base); i++ {
			if base[i] == "--api-key" {
				i++
				continue
			}
			args = append(args, base[i])
		}
		return args
	}
	credentials := []struct {
		name       string
		env        map[string]string
		flags      []string
		wantKey    string
		wantBearer string
	}{
		{"flag key beats env token", map[string]string{"GEMINI_ACCESS_TOKEN": "env-token"}, []string{"--api-key", "flag-key"}, "flag-key", ""},
		{"flag token beats env key", map[string]string{"GEMINI_API_KEY": "env-key"}, []string{"--access-token", "flag-token"}, "", "Bearer flag-token"},
		{"same tier prefers the key", map[string]string{"GEMINI_API_KEY": "env-key", "GEMINI_ACCESS_TOKEN": "env-token"}, nil, "env-key", ""},
		{"token alone", map[string]string{"GEMINI_ACCESS_TOKEN": "env-token"}, nil, "", "Bearer env-token"},
	}
	for _, tc := range credentials {
		t.Run(tc.name, func(t *testing.T) {
			mock := newUploadMock(t)
			src := writeTempFile(t, "clip.mp3", "some audio bytes")
			args := append(append(withoutKey(mock.URL), tc.flags...), "files", "upload", src)
			result := runCLI(t, t.TempDir(), tc.env, args...)
			if result.err != nil {
				t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
			}
			for _, r := range mock.recorded() {
				if got := r.headers.Get("X-Goog-Api-Key"); got != tc.wantKey {
					t.Errorf("%s x-goog-api-key = %q, want %q", r.command, got, tc.wantKey)
				}
				if got := r.headers.Get("Authorization"); got != tc.wantBearer {
					t.Errorf("%s Authorization = %q, want %q", r.command, got, tc.wantBearer)
				}
			}
		})
	}

	t.Run("global parameters", func(t *testing.T) {
		mock := newUploadMock(t)
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		args := []string{
			"--server-url", mock.URL, "--api-version", "v1", "--api-key", "test-key",
			"--api-revision", "2026-05-20", "--user-project", "quota-project", "--header", "X-Trace: abc",
			"--no-interactive", "--no-retries", "--color", "never", "files", "upload", src,
		}
		result := runCLI(t, t.TempDir(), nil, args...)
		if result.err != nil {
			t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
		}
		reqs := mock.recorded()
		if reqs[0].path != "/upload/v1/files" {
			t.Errorf("start path = %q, want /upload/v1/files", reqs[0].path)
		}
		for _, r := range reqs {
			for header, want := range map[string]string{
				"Api-Revision": "2026-05-20", "X-Goog-User-Project": "quota-project", "X-Trace": "abc",
			} {
				if got := r.headers.Get(header); got != want {
					t.Errorf("%s %s = %q, want %q", r.command, header, got, want)
				}
			}
		}
	})

	t.Run("custom Authorization displaces the API key", func(t *testing.T) {
		// Generated requests drop x-goog-api-key when Authorization is set;
		// the raw transport must not send both schemes either.
		mock := newUploadMock(t)
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		args := append(plainArgs(mock.URL), "--header", "Authorization: Bearer override", "files", "upload", src)
		result := runCLI(t, t.TempDir(), nil, args...)
		if result.err != nil {
			t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
		}
		for _, r := range mock.recorded() {
			if got := r.headers.Get("Authorization"); got != "Bearer override" {
				t.Errorf("%s Authorization = %q, want the supplied header", r.command, got)
			}
			if got := r.headers.Get("X-Goog-Api-Key"); got != "" {
				t.Errorf("%s also sent x-goog-api-key %q", r.command, got)
			}
		}
	})

	t.Run("api version is one path segment", func(t *testing.T) {
		mock := newUploadMock(t)
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		for _, version := range []string{"../evil", "..", ".", "v1/../..", ""} {
			for _, mode := range [][]string{nil, {"--dry-run"}} {
				args := []string{"--server-url", mock.URL, "--api-version=" + version, "--api-key", "test-key", "--no-interactive", "--color", "never"}
				result := runCLI(t, t.TempDir(), nil, append(append(args, mode...), "files", "upload", src)...)
				if code := exitCode(t, result); code != 2 {
					t.Errorf("%q %v exit code = %d, want 2\nstderr: %s", version, mode, code, result.stderr)
				}
				if strings.Contains(result.stderr, "[DRY-RUN]") || !strings.Contains(result.stderr, "--api-version must be non-empty") {
					t.Errorf("%q %v stderr = %q", version, mode, result.stderr)
				}
			}
		}
		if got := len(mock.recorded()); got != 0 {
			t.Errorf("invalid versions dispatched %d requests, want 0", got)
		}
	})

	t.Run("generated requests reject invalid versions too", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		for _, version := range []string{"..", ".", ""} {
			for _, mode := range [][]string{nil, {"--dry-run"}} {
				args := []string{"--server-url", server.URL, "--api-version=" + version, "--api-key", "test-key", "--no-interactive", "--color", "never"}
				result := runCLI(t, t.TempDir(), nil, append(args, append(mode, "files", "list")...)...)
				if result.err == nil || strings.Contains(result.stderr, "[DRY-RUN]") ||
					(!strings.Contains(result.stderr, "API version must be non-empty") &&
						!strings.Contains(result.stderr, "contains a dot segment")) {
					t.Errorf("--api-version %q %v was accepted:\n%s", version, mode, result.stderr)
				}
			}
		}
		if got := requests.Load(); got != 0 {
			t.Errorf("invalid generated versions dispatched %d requests, want 0", got)
		}
	})

	t.Run("debug redacts the upload session id", func(t *testing.T) {
		const sessionID = "secret-session-123"
		mock := newUploadMock(t)
		mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
			if r.command != "start" {
				return false
			}
			w.Header().Set("X-Goog-Upload-Url", mock.URL+"/upload-session?upload_id="+sessionID+"&upload_protocol=resumable")
			return true
		}
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "--debug", "files", "upload", src)...)
		if result.err != nil {
			t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if strings.Contains(result.stderr, sessionID) {
			t.Errorf("--debug leaked the upload session id:\n%s", result.stderr)
		}
		if !strings.Contains(result.stderr, "upload_id=REDACTED") {
			t.Errorf("--debug output does not show the masked session URL:\n%s", result.stderr)
		}
		chunks := mock.chunks()
		if len(chunks) != 1 || chunks[0].query != "upload_id="+sessionID+"&upload_protocol=resumable" {
			t.Errorf("chunk requests = %+v, want one carrying the real session query", len(chunks))
		}
	})

	t.Run("a failed chunk does not leak the upload session id", func(t *testing.T) {
		// Go's transport puts the real request URL in *url.Error, which both
		// --debug and the command's own error message print.
		const sessionID = "secret-session-456"
		mock := newUploadMock(t)
		mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
			if r.command == "start" {
				w.Header().Set("X-Goog-Upload-Url", mock.URL+"/upload-session?upload_id="+sessionID)
				return true
			}
			// Drop the connection mid-request so the transport, not the
			// server, reports the failure.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return true
			}
			conn.Close()
			return true
		}
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		for _, mode := range [][]string{nil, {"--debug"}} {
			result := runCLI(t, t.TempDir(), nil, append(append(plainArgs(mock.URL), mode...), "files", "upload", src)...)
			if result.err == nil {
				t.Fatalf("%v: upload succeeded, want a transport failure\nstderr: %s", mode, result.stderr)
			}
			if strings.Contains(result.stderr, sessionID) || strings.Contains(result.stdout, sessionID) {
				t.Errorf("%v: the failure leaked the upload session id:\nstderr: %s\nstdout: %s", mode, result.stderr, result.stdout)
			}
			if !strings.Contains(result.stderr, "upload_id=REDACTED") {
				t.Errorf("%v: the failure did not report a redacted session URL:\n%s", mode, result.stderr)
			}
		}
	})

	t.Run("debug redacts the credential", func(t *testing.T) {
		mock := newUploadMock(t)
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "--debug", "files", "upload", src)...)
		if result.err != nil {
			t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if strings.Contains(result.stderr, "test-key") {
			t.Errorf("--debug leaked the API key:\n%s", result.stderr)
		}
	})
}

// TestFilesUploadTimeout: --timeout bounds each request including its body. A
// response body that trails its headers is still read; a stalled one fails.
func TestFilesUploadTimeout(t *testing.T) {
	delayedFinalize := func(delay time.Duration) func(http.ResponseWriter, uploadRequest) bool {
		return func(w http.ResponseWriter, r uploadRequest) bool {
			if r.command != "upload, finalize" {
				return false
			}
			w.Header().Set("X-Goog-Upload-Status", "final")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(delay)
			fmt.Fprint(w, `{"file":{"name":"files/late","state":"ACTIVE"}}`)
			return true
		}
	}
	t.Run("body after headers", func(t *testing.T) {
		mock := newUploadMock(t)
		mock.respond = delayedFinalize(150 * time.Millisecond)
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "--timeout", "5s", "files", "upload", src)...)
		if result.err != nil {
			t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if got := strings.TrimSpace(result.stdout); got != "files/late" {
			t.Errorf("stdout = %q, want files/late", got)
		}
	})
	t.Run("stalled body", func(t *testing.T) {
		mock := newUploadMock(t)
		mock.respond = delayedFinalize(2 * time.Second)
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "--timeout", "300ms", "files", "upload", src)...)
		if code := exitCode(t, result); code != 1 {
			t.Errorf("exit code = %d, want 1\nstderr: %s", code, result.stderr)
		}
		if !strings.Contains(result.stderr, "reading the upload response") {
			t.Errorf("stderr = %q", result.stderr)
		}
		if result.stdout != "" {
			t.Errorf("failure wrote stdout: %q", result.stdout)
		}
	})
}

// TestFilesUploadWaitEdges covers what the --wait state table does not: a poll
// response with no state keeps waiting, the poll is a files.get on the uploaded
// name, the envelope carries the refreshed state, and a failing poll is
// classified like any API error.
func TestFilesUploadWaitEdges(t *testing.T) {
	polls := func(mock *uploadMock) []uploadRequest {
		var out []uploadRequest
		for _, r := range mock.recorded() {
			if r.command == "" {
				out = append(out, r)
			}
		}
		return out
	}
	finalizeProcessing := func(w http.ResponseWriter) {
		w.Header().Set("X-Goog-Upload-Status", "final")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"file":{"name":"files/xyz789","mimeType":"video/mp4","state":"PROCESSING"}}`)
	}

	t.Run("stateless poll then active", func(t *testing.T) {
		mock := newUploadMock(t)
		mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
			switch r.command {
			case "upload, finalize":
				finalizeProcessing(w)
			case "":
				w.Header().Set("Content-Type", "application/json")
				if len(polls(mock)) == 1 {
					fmt.Fprint(w, `{}`)
				} else {
					fmt.Fprint(w, `{"name":"files/xyz789","mimeType":"video/mp4","state":"ACTIVE"}`)
				}
			default:
				return false
			}
			return true
		}
		src := writeTempFile(t, "clip.mp4", "some video bytes")
		args := append(plainArgs(mock.URL), "--output-format", "json", "files", "upload", src, "--wait", "--wait-timeout", "10s")
		result := runCLI(t, t.TempDir(), nil, args...)
		if result.err != nil {
			t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
		}
		got := polls(mock)
		if len(got) != 2 {
			t.Fatalf("polls = %d, want 2", len(got))
		}
		for _, p := range got {
			if p.method != http.MethodGet || p.path != "/v1beta/files/xyz789" {
				t.Errorf("poll = %s %s, want GET /v1beta/files/xyz789", p.method, p.path)
			}
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, result.stdout)
		}
		if envelope["state"] != "ACTIVE" || envelope["name"] != "files/xyz789" {
			t.Errorf("envelope = %v, want the refreshed ACTIVE file", envelope)
		}
	})

	t.Run("poll fails", func(t *testing.T) {
		mock := newUploadMock(t)
		mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
			switch r.command {
			case "upload, finalize":
				finalizeProcessing(w)
			case "":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"code":404,"message":"file vanished","status":"NOT_FOUND"}}`)
			default:
				return false
			}
			return true
		}
		src := writeTempFile(t, "clip.mp4", "some video bytes")
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "files", "upload", src, "--wait")...)
		if code := exitCode(t, result); code != 1 {
			t.Errorf("exit code = %d, want 1\nstderr: %s", code, result.stderr)
		}
		if !strings.Contains(result.stderr, "file vanished") || result.stdout != "" {
			t.Errorf("stdout = %q, stderr = %q", result.stdout, result.stderr)
		}
	})
}
