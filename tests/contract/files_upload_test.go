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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/cli"
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
	ctx     context.Context
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
			headers: r.Header.Clone(), body: body, ctx: r.Context(),
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

	for _, tc := range uploadSizes {
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
			checkChunks(t, mock.chunks(), data, tc)
		})
	}
}

// uploadSize is a source size and the chunk layout the resumable loop must
// send for it.
type uploadSize struct {
	name         string
	size         int
	wantOffsets  []string
	wantCommands []string
}

var uploadSizes = []uploadSize{
	{"exact chunk multiple", uploadChunk, []string{"0"}, []string{"upload, finalize"}},
	{"three chunks", 2*uploadChunk + 5, []string{"0", "8388608", "16777216"}, []string{"upload", "upload", "upload, finalize"}},
}

// checkChunks asserts the chunk requests are POSTs of raw bytes at the
// expected offsets, with "finalize" riding only the last, and that together
// they carry exactly data.
func checkChunks(t *testing.T, chunks []uploadRequest, data []byte, want uploadSize) {
	t.Helper()
	if len(chunks) != len(want.wantOffsets) {
		t.Fatalf("chunk requests = %d, want %d", len(chunks), len(want.wantOffsets))
	}
	var sent []byte
	for i, chunk := range chunks {
		if chunk.method != http.MethodPost {
			t.Errorf("chunk %d method = %s, want POST", i, chunk.method)
		}
		if got := chunk.headers.Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("chunk %d Content-Type = %q, want application/octet-stream", i, got)
		}
		if got := chunk.headers.Get("X-Goog-Upload-Offset"); got != want.wantOffsets[i] {
			t.Errorf("chunk %d offset = %q, want %q", i, got, want.wantOffsets[i])
		}
		if chunk.command != want.wantCommands[i] {
			t.Errorf("chunk %d command = %q, want %q", i, chunk.command, want.wantCommands[i])
		}
		sent = append(sent, chunk.body...)
	}
	if !bytes.Equal(sent, data) {
		t.Errorf("uploaded %d bytes that differ from the %d-byte source", len(sent), len(data))
	}
}

// TestFilesUploadFailures pins every way the exchange can go wrong: a
// classified exit code, a named cause, nothing on stdout.
func TestFilesUploadFailures(t *testing.T) {
	cases := append(uploadExchangeFailures(),
		uploadFailure{"start redirected", 16, "start", func(w http.ResponseWriter) {
			w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
			w.WriteHeader(http.StatusFound)
		}, "API error occurred", 0, 1},
		uploadFailure{"finalize body not JSON", 16, "upload, finalize", func(w http.ResponseWriter) {
			w.Header().Set("X-Goog-Upload-Status", "final")
			fmt.Fprint(w, "<html>")
		}, "response was not valid JSON", 1, 1},
		uploadFailure{"finalize without file name", 16, "upload, finalize", func(w http.ResponseWriter) {
			w.Header().Set("X-Goog-Upload-Status", "final")
			fmt.Fprint(w, `{"file":{}}`)
		}, "response had no file name", 1, 1},
	)
	runUploadFailures(t, cases, func(serverURL, src string) []string {
		return append(plainArgs(serverURL), "files", "upload", src, "--mime-type", "application/octet-stream")
	})
}

// uploadFailure is one way the resumable exchange goes wrong, and how the
// command must report it.
type uploadFailure struct {
	name       string
	size       int
	on         string // command the override answers
	answer     func(http.ResponseWriter)
	wantStderr string
	wantChunks int
	wantCode   int
}

func answerStatus(code int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}
}

// uploadExchangeFailures are the failures every resumable upload command
// classifies the same way.
func uploadExchangeFailures() []uploadFailure {
	return []uploadFailure{
		{"start rejected", 16, "start", answerStatus(http.StatusForbidden, `{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`), "denied", 0, 3},
		{"start without session URL", 16, "start", func(w http.ResponseWriter) {}, "did not return an upload URL", 0, 1},
		{"chunk server error", uploadChunk + 1, "upload", answerStatus(http.StatusInternalServerError, `{"error":{"code":500,"message":"chunk exploded","status":"INTERNAL"}}`), "chunk exploded", 1, 1},
		{"chunk session cancelled", uploadChunk + 1, "upload", func(w http.ResponseWriter) {
			w.Header().Set("X-Goog-Upload-Status", "cancelled")
		}, `upload interrupted: server status is "cancelled" at offset 8388608`, 1, 1},
		{"finalize not final", 16, "upload, finalize", func(w http.ResponseWriter) {
			w.Header().Set("X-Goog-Upload-Status", "active")
		}, `upload finalized but server status is "active"`, 1, 1},
	}
}

// runUploadFailures runs each case through the command args builds and
// expects a classified exit code, a named cause, and nothing on stdout.
func runUploadFailures(t *testing.T, cases []uploadFailure, args func(serverURL, src string) []string) {
	t.Helper()
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
			result := runCLI(t, t.TempDir(), nil, args(mock.URL, src)...)
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

// TestFilesUploadDryRunProgress: --dry-run prints no upload progress, and
// says that --wait polling was not simulated.
func TestFilesUploadDryRunProgress(t *testing.T) {
	src := writeTempFile(t, "clip.mp3", "some audio bytes")
	result := runCLI(t, t.TempDir(), nil, append(plainArgs("http://127.0.0.1:1"), "--dry-run", "files", "upload", src, "--wait")...)
	if result.err != nil {
		t.Fatalf("files upload --dry-run --wait failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if strings.Contains(result.stderr, "Starting upload") || strings.Contains(result.stderr, "Uploaded ") {
		t.Errorf("dry-run printed upload progress:\n%s", result.stderr)
	}
	if !strings.Contains(result.stderr, "[DRY-RUN] --wait polling is response-dependent and was not simulated.") {
		t.Errorf("dry-run did not note the skipped --wait polling:\n%s", result.stderr)
	}
}

// TestFilesUploadRefusesForeignSession: a session URL on another host must
// never receive the credentials or the file bytes.
func TestFilesUploadRefusesForeignSession(t *testing.T) {
	checkForeignSessionRefused(t, func(serverURL, src string) []string {
		return append(plainArgs(serverURL), "files", "upload", src)
	})
}

// checkForeignSessionRefused points the start response at another host and
// expects the command to send it nothing, naming the host only: the session
// URL carries the upload_id capability.
func checkForeignSessionRefused(t *testing.T, args func(serverURL, src string) []string) {
	t.Helper()
	var foreignHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		foreignHits.Add(1)
	}))
	defer foreign.Close()
	// Same loopback host, different port: still a different service.
	mock := newUploadMock(t)
	mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
		if r.command != "start" {
			return false
		}
		w.Header().Set("X-Goog-Upload-Url", foreign.URL+"/steal?upload_id=secret")
		return true
	}
	src := writeTempFile(t, "clip.mp3", "some audio bytes")
	result := runCLI(t, t.TempDir(), nil, args(mock.URL, src)...)
	if code := exitCode(t, result); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(result.stderr, "refusing to upload to an unexpected host") || strings.Contains(result.stderr, "secret") {
		t.Errorf("stderr = %q", result.stderr)
	}
	if foreignHits.Load() != 0 || len(mock.chunks()) != 0 {
		t.Errorf("foreign hits = %d, chunk requests = %d; want none", foreignHits.Load(), len(mock.chunks()))
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

	t.Run("repeated headers, Accept replacement, trimming, and dry-run header preview", func(t *testing.T) {
		mock := newUploadMock(t)
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		args := append(
			plainArgs(mock.URL),
			"-H", "X-A: 1",
			"-H", "x-a: 2",
			"-H", "  X-Trim:  trimmed\r\n",
			"-H", "Accept: application/vnd.custom+json",
			"--timeout", "30s",
			"files", "upload", src,
		)
		result := runCLI(t, t.TempDir(), nil, args...)
		if result.err != nil {
			t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
		}
		for _, r := range mock.recorded() {
			if got := r.headers.Values("X-A"); !slices.Equal(got, []string{"1", "2"}) {
				t.Errorf("%s X-A = %v, want [1 2]", r.command, got)
			}
			if got := r.headers.Values("X-Trim"); !slices.Equal(got, []string{"trimmed"}) {
				t.Errorf("%s X-Trim = %v, want [trimmed]", r.command, got)
			}
			if got := r.headers.Values("Accept"); !slices.Equal(got, []string{"application/vnd.custom+json"}) {
				t.Errorf("%s Accept = %v, want [application/vnd.custom+json]", r.command, got)
			}
		}

		dryArgs := append(
			plainArgs(mock.URL),
			"--dry-run", "-o", "json",
			"-H", "X-Trace: a",
			"-H", "x-trace: b",
			"files", "upload", src,
		)
		dryRes := runCLI(t, t.TempDir(), nil, dryArgs...)
		if dryRes.err != nil {
			t.Fatalf("dry-run failed: %v\nstderr: %s", dryRes.err, dryRes.stderr)
		}
		lines := strings.Split(strings.TrimSpace(dryRes.stdout), "\n")
		if len(lines) == 0 {
			t.Fatalf("dry-run produced empty stdout")
		}
		for i, line := range lines {
			var preview struct {
				Request struct {
					Headers map[string][]string `json:"headers"`
				} `json:"request"`
			}
			if err := json.Unmarshal([]byte(line), &preview); err != nil {
				t.Fatalf("dry-run line %d JSON decode: %v\nline: %s", i, err, line)
			}
			if got := preview.Request.Headers["X-Trace"]; !slices.Equal(got, []string{"a", "b"}) {
				t.Errorf("dry-run line %d X-Trace = %v, want [a b]", i, got)
			}
		}
	})

	t.Run("header, timeout, and server-url validation fail with exit 2 before network access", func(t *testing.T) {
		mock := newUploadMock(t)
		src := writeTempFile(t, "clip.mp3", "some audio bytes")
		const secret = "secret-header-token-999"

		badFlagCases := []struct {
			name string
			args []string
		}{
			{"missing colon", []string{"-H", "Authorization Bearer " + secret}},
			{"invalid header name", []string{"-H", "bad name: " + secret}},
			{"embedded CRLF", []string{"-H", "Authorization: Bearer " + secret + "\r\nX-Injected: 1"}},
			{"embedded LF", []string{"-H", "X-Token: " + secret + "\nX-Injected: 1"}},
			{"reserved repeated header marker", []string{"-H", "X-Cli-Repeated-Headers: X-A"}},
			{"timeout 0s", []string{"--timeout", "0s"}},
			{"timeout -1s", []string{"--timeout", "-1s"}},
			{"timeout abc", []string{"--timeout", "abc"}},
		}
		for _, tc := range badFlagCases {
			for _, mode := range [][]string{nil, {"--dry-run", "-o", "json"}} {
				args := append(append(plainArgs(mock.URL), tc.args...), mode...)
				args = append(args, "files", "upload", src)
				res := runCLI(t, t.TempDir(), nil, args...)
				if code := exitCode(t, res); code != 2 {
					t.Errorf("%s (%v): exit code = %d, want 2\nstderr: %s", tc.name, mode, code, res.stderr)
				}
				if strings.Contains(res.stderr, secret) || strings.Contains(res.stdout, secret) {
					t.Errorf("%s (%v): leaked secret in output:\nstderr: %s\nstdout: %s", tc.name, mode, res.stderr, res.stdout)
				}
			}
		}

		for _, envTimeout := range []string{"0s", "-1s", "abc"} {
			args := append(plainArgs(mock.URL), "files", "upload", src)
			res := runCLI(t, t.TempDir(), map[string]string{"GEMINI_TIMEOUT": envTimeout}, args...)
			if code := exitCode(t, res); code != 2 {
				t.Errorf("GEMINI_TIMEOUT=%q: exit code = %d, want 2\nstderr: %s", envTimeout, code, res.stderr)
			}
		}

		home := t.TempDir()
		cfgDir := filepath.Join(home, ".config", "gemini-api")
		if err := os.MkdirAll(cfgDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("version: 1\ntimeout: 0s\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfgRes := runCLI(t, home, nil, append(plainArgs(mock.URL), "files", "upload", src)...)
		if code := exitCode(t, cfgRes); code != 2 {
			t.Errorf("config timeout 0s: exit code = %d, want 2\nstderr: %s", code, cfgRes.stderr)
		}
		overrideRes := runCLI(t, home, nil, append(plainArgs(mock.URL), "--timeout", "30s", "--dry-run", "-o", "json", "files", "upload", src)...)
		if overrideRes.err != nil {
			t.Errorf("--timeout 30s flag should override config timeout 0s: %v\nstderr: %s", overrideRes.err, overrideRes.stderr)
		}

		for _, badURL := range []string{"ftp://x", "https://"} {
			for _, mode := range [][]string{nil, {"--dry-run", "-o", "json"}} {
				args := append(append(plainArgs(badURL), mode...), "files", "upload", src)
				res := runCLI(t, t.TempDir(), nil, args...)
				if code := exitCode(t, res); code != 2 {
					t.Errorf("--server-url %q (%v): exit code = %d, want 2\nstderr: %s", badURL, mode, code, res.stderr)
				}
			}
		}

		if got := len(mock.recorded()); got != 0 {
			t.Errorf("validation failures sent %d requests, want 0", got)
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

	t.Run("prompt active and delayed processing then delayed active before deadline", func(t *testing.T) {
		// Prompt ACTIVE on first poll.
		mockPrompt := newUploadMock(t)
		mockPrompt.respond = func(w http.ResponseWriter, r uploadRequest) bool {
			switch r.command {
			case "upload, finalize":
				finalizeProcessing(w)
			case "":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"name":"files/xyz789","mimeType":"video/mp4","state":"ACTIVE"}`)
			default:
				return false
			}
			return true
		}
		src := writeTempFile(t, "clip.mp4", "some video bytes")
		resPrompt := runCLI(t, t.TempDir(), nil, append(plainArgs(mockPrompt.URL), "files", "upload", src, "--wait", "--wait-timeout", "3s")...)
		if resPrompt.err != nil {
			t.Fatalf("prompt active upload failed: %v\nstderr: %s", resPrompt.err, resPrompt.stderr)
		}
		if got := strings.TrimSpace(resPrompt.stdout); got != "files/xyz789" {
			t.Errorf("stdout = %q, want files/xyz789", got)
		}

		// Delayed PROCESSING on poll 1, then delayed ACTIVE on poll 2 before the deadline.
		mockDelayed := newUploadMock(t)
		mockDelayed.respond = func(w http.ResponseWriter, r uploadRequest) bool {
			switch r.command {
			case "upload, finalize":
				finalizeProcessing(w)
			case "":
				time.Sleep(150 * time.Millisecond)
				w.Header().Set("Content-Type", "application/json")
				if len(polls(mockDelayed)) == 1 {
					fmt.Fprint(w, `{"name":"files/xyz789","mimeType":"video/mp4","state":"PROCESSING"}`)
				} else {
					fmt.Fprint(w, `{"name":"files/xyz789","mimeType":"video/mp4","state":"ACTIVE"}`)
				}
			default:
				return false
			}
			return true
		}
		resDelayed := runCLI(t, t.TempDir(), nil, append(plainArgs(mockDelayed.URL), "files", "upload", src, "--wait", "--wait-timeout", "4s")...)
		if resDelayed.err != nil {
			t.Fatalf("delayed active upload failed: %v\nstderr: %s", resDelayed.err, resDelayed.stderr)
		}
		if got := strings.TrimSpace(resDelayed.stdout); got != "files/xyz789" {
			t.Errorf("stdout = %q, want files/xyz789", got)
		}
		if got := len(polls(mockDelayed)); got != 2 {
			t.Errorf("polls = %d, want 2", got)
		}
	})

	t.Run("hanging poll exits near wait-timeout with wait-timeout message and never late success", func(t *testing.T) {
		mock := newUploadMock(t)
		mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
			switch r.command {
			case "upload, finalize":
				finalizeProcessing(w)
			case "":
				select {
				case <-r.ctx.Done():
					return true
				case <-time.After(8 * time.Second):
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"name":"files/xyz789","mimeType":"video/mp4","state":"ACTIVE"}`)
				}
			default:
				return false
			}
			return true
		}
		src := writeTempFile(t, "clip.mp4", "some video bytes")
		start := time.Now()
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "files", "upload", src, "--wait", "--wait-timeout", "3s")...)
		elapsed := time.Since(start)
		if code := exitCode(t, result); code != 1 {
			t.Fatalf("exit code = %d, want 1\nstdout: %s\nstderr: %s", code, result.stdout, result.stderr)
		}
		if elapsed >= 6*time.Second {
			t.Errorf("elapsed = %v, want near 3s wait-timeout (< 6s)", elapsed)
		}
		if !strings.Contains(result.stderr, "files/xyz789 did not become ACTIVE within 3s") {
			t.Errorf("stderr = %q, want wait-timeout message", result.stderr)
		}
		if result.stdout != "" {
			t.Errorf("stdout = %q, want empty", result.stdout)
		}
	})

	t.Run("earlier per-request --timeout 1s is not relabeled as --wait-timeout 3s expiry", func(t *testing.T) {
		mock := newUploadMock(t)
		mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
			switch r.command {
			case "upload, finalize":
				finalizeProcessing(w)
			case "":
				select {
				case <-r.ctx.Done():
					return true
				case <-time.After(5 * time.Second):
				}
			default:
				return false
			}
			return true
		}
		src := writeTempFile(t, "clip.mp4", "some video bytes")
		start := time.Now()
		result := runCLI(t, t.TempDir(), nil, append(plainArgs(mock.URL), "--timeout", "1s", "files", "upload", src, "--wait", "--wait-timeout", "3s")...)
		elapsed := time.Since(start)
		if code := exitCode(t, result); code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr: %s", code, result.stderr)
		}
		if elapsed >= 2500*time.Millisecond {
			t.Errorf("elapsed = %v, want near 1s request timeout (< 2.5s)", elapsed)
		}
		if strings.Contains(result.stderr, "did not become ACTIVE within") {
			t.Errorf("earlier --timeout 1s was relabeled as wait-timeout expiry:\n%s", result.stderr)
		}
	})

	t.Run("dry-run with --wait and --wait-timeout preserves preview without polling or network", func(t *testing.T) {
		mock := newUploadMock(t)
		src := writeTempFile(t, "sample.txt", "hello world\n")
		args := append(plainArgs(mock.URL), "--dry-run", "-o", "json", "files", "upload", src, "--wait", "--wait-timeout", "3s")
		res := runCLI(t, t.TempDir(), nil, args...)
		if res.err != nil {
			t.Fatalf("dry-run failed: %v\nstderr: %s", res.err, res.stderr)
		}
		if got := len(mock.recorded()); got != 0 {
			t.Errorf("dry-run sent %d network requests, want 0", got)
		}
		lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
		if len(lines) != 2 {
			t.Fatalf("dry-run preview lines = %d, want 2 (start + chunk):\n%s", len(lines), res.stdout)
		}
	})
}

func TestRawHTTPAndSDKUserAgentIdentityAndOverrides(t *testing.T) {
	wantUA := fmt.Sprintf("google-genai-cli/%s gl-go/%s", cli.Version, runtime.Version())
	workDir := t.TempDir()
	samplePath := writeTempFile(t, "sample.txt", "hello world\n")

	t.Run("default User-Agent matches across files upload, environments files upload/download, and SDK commands", func(t *testing.T) {
		filesMock := newUploadMock(t)
		res := runCLI(t, t.TempDir(), nil, append(plainArgs(filesMock.URL), "files", "upload", samplePath)...)
		if res.err != nil {
			t.Fatalf("files upload failed: %v\nstderr: %s", res.err, res.stderr)
		}
		filesReqs := filesMock.recorded()
		if len(filesReqs) != 2 {
			t.Fatalf("files upload requests = %d, want 2", len(filesReqs))
		}
		for i, r := range filesReqs {
			if got := r.headers.Get("User-Agent"); got != wantUA {
				t.Errorf("files upload req[%d] (%s) User-Agent = %q, want %q", i, r.command, got, wantUA)
			}
		}

		envUploadMock := newEnvironmentUploadMock(t, storesEverything("sample.txt"))
		envRes := runCLI(t, t.TempDir(), nil, environmentUploadArgs()(envUploadMock.URL, samplePath)...)
		if envRes.err != nil {
			t.Fatalf("environments files upload failed: %v\nstderr: %s", envRes.err, envRes.stderr)
		}
		envReqs := envUploadMock.recorded()
		if len(envReqs) != 2 {
			t.Fatalf("environments files upload requests = %d, want 2", len(envReqs))
		}
		for i, r := range envReqs {
			if got := r.headers.Get("User-Agent"); got != wantUA {
				t.Errorf("environments files upload req[%d] (%s) User-Agent = %q, want %q", i, r.command, got, wantUA)
			}
		}

		var gotDownloadUA, gotModelsUA string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1beta/environments/env_abc/files/remote.txt":
				gotDownloadUA = r.Header.Get("User-Agent")
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte("content\n"))
			case "/v1beta/models":
				gotModelsUA = r.Header.Get("User-Agent")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"models":[]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		dlOut := filepath.Join(workDir, "downloaded.txt")
		dlRes := runCLI(t, t.TempDir(), nil, append(plainArgs(srv.URL), "environments", "files", "download", "env_abc", "remote.txt", "--out", dlOut)...)
		if dlRes.err != nil {
			t.Fatalf("environments files download failed: %v\nstderr: %s", dlRes.err, dlRes.stderr)
		}
		if gotDownloadUA != wantUA {
			t.Errorf("environments files download User-Agent = %q, want %q", gotDownloadUA, wantUA)
		}

		modelsRes := runCLI(t, t.TempDir(), nil, append(plainArgs(srv.URL), "models", "list")...)
		if modelsRes.err != nil {
			t.Fatalf("models list failed: %v\nstderr: %s", modelsRes.err, modelsRes.stderr)
		}
		if gotModelsUA != wantUA {
			t.Errorf("models list User-Agent = %q, want %q", gotModelsUA, wantUA)
		}
	})

	t.Run("explicit --header User-Agent override and header precedence", func(t *testing.T) {
		const customUA = "custom-cli-caller/9.9"
		filesMock := newUploadMock(t)
		res := runCLI(t, t.TempDir(), nil, append(
			plainArgs(filesMock.URL),
			"-H", "User-Agent: "+customUA,
			"-H", "Authorization: Bearer custom-tok",
			"-H", "X-Custom: a",
			"-H", "x-custom: b",
			"files", "upload", samplePath,
		)...)
		if res.err != nil {
			t.Fatalf("files upload with custom headers failed: %v\nstderr: %s", res.err, res.stderr)
		}
		for i, r := range filesMock.recorded() {
			if got := r.headers.Get("User-Agent"); got != customUA {
				t.Errorf("files upload req[%d] User-Agent = %q, want %q", i, got, customUA)
			}
			// Explicit Authorization header still displaces API key.
			if gotAuth := r.headers.Get("Authorization"); gotAuth != "Bearer custom-tok" {
				t.Errorf("files upload req[%d] Authorization = %q, want Bearer custom-tok", i, gotAuth)
			}
			if gotKey := r.headers.Get("X-Goog-Api-Key"); gotKey != "" {
				t.Errorf("files upload req[%d] X-Goog-Api-Key = %q, want displaced (empty)", i, gotKey)
			}
			if gotCustom := r.headers.Values("X-Custom"); !slices.Equal(gotCustom, []string{"a", "b"}) {
				t.Errorf("files upload req[%d] X-Custom = %v, want [a b]", i, gotCustom)
			}
		}

		var gotDownloadUA string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotDownloadUA = r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("ok\n"))
		}))
		defer srv.Close()

		dlOut := filepath.Join(workDir, "downloaded-override.txt")
		dlRes := runCLI(t, t.TempDir(), nil, append(
			plainArgs(srv.URL),
			"-H", "user-agent: "+customUA,
			"environments", "files", "download", "env_abc", "remote.txt", "--out", dlOut,
		)...)
		if dlRes.err != nil {
			t.Fatalf("download with custom User-Agent failed: %v\nstderr: %s", dlRes.err, dlRes.stderr)
		}
		if gotDownloadUA != customUA {
			t.Errorf("download custom User-Agent = %q, want %q", gotDownloadUA, customUA)
		}
	})

	t.Run("dry-run JSON previews render current User-Agent with zero network calls and no file writes", func(t *testing.T) {
		var sinkCalls atomic.Int32
		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sinkCalls.Add(1)
		}))
		defer sink.Close()

		dryDir := t.TempDir()
		previewCases := []struct {
			name      string
			tail      []string
			wantLines int
		}{
			{"files upload", []string{"files", "upload", samplePath, "--dry-run", "-o", "json"}, 2},
			{"environments files upload", []string{"environments", "files", "upload", "env_abc", samplePath, "--dry-run", "-o", "json"}, 2},
			{"environments files download", []string{"environments", "files", "download", "env_abc", "remote.txt", "--dry-run", "-o", "json"}, 1},
		}

		for _, tc := range previewCases {
			t.Run(tc.name, func(t *testing.T) {
				res := runCLIInDir(t, t.TempDir(), dryDir, nil, append(plainArgs(sink.URL), tc.tail...)...)
				if res.err != nil {
					t.Fatalf("dry-run failed: %v\nstderr: %s", res.err, res.stderr)
				}
				lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
				if len(lines) != tc.wantLines {
					t.Fatalf("preview lines = %d, want %d:\n%s", len(lines), tc.wantLines, res.stdout)
				}
				for i, line := range lines {
					var preview struct {
						DryRun  bool `json:"dry_run"`
						Request struct {
							Headers map[string][]string `json:"headers"`
						} `json:"request"`
					}
					if err := json.Unmarshal([]byte(line), &preview); err != nil {
						t.Fatalf("line %d unmarshal: %v\nraw: %s", i, err, line)
					}
					if !preview.DryRun {
						t.Errorf("line %d dry_run = false, want true", i)
					}
					if gotUA := preview.Request.Headers["User-Agent"]; !slices.Equal(gotUA, []string{wantUA}) {
						t.Errorf("line %d User-Agent = %v, want [%s]", i, gotUA, wantUA)
					}
					if gotKey := preview.Request.Headers["X-Goog-Api-Key"]; !slices.Equal(gotKey, []string{"[REDACTED]"}) {
						t.Errorf("line %d X-Goog-Api-Key = %v, want [[REDACTED]]", i, gotKey)
					}
				}
			})
		}

		if got := sinkCalls.Load(); got != 0 {
			t.Errorf("sink received %d requests during dry-run previews, want 0", got)
		}
		entries, err := os.ReadDir(dryDir)
		if err != nil || len(entries) != 0 {
			t.Errorf("dry-run wrote files into working directory: %v (err=%v)", entries, err)
		}
	})
}
