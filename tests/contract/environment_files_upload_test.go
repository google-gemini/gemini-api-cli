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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// environmentUploadArgs builds "environments files upload env_abc <src>".
func environmentUploadArgs(extra ...string) func(serverURL, src string) []string {
	return func(serverURL, src string) []string {
		args := append(plainArgs(serverURL), "environments", "files", "upload", "env_abc", src)
		return append(args, extra...)
	}
}

// newEnvironmentUploadMock answers the finalize chunk with finalizeBody, given
// the number of file bytes the session received.
func newEnvironmentUploadMock(t *testing.T, finalizeBody func(received int) string) *uploadMock {
	t.Helper()
	mock := newUploadMock(t)
	var received int
	mock.respond = func(w http.ResponseWriter, r uploadRequest) bool {
		received += len(r.body)
		if r.command != "upload, finalize" {
			return false
		}
		w.Header().Set("X-Goog-Upload-Status", "final")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, finalizeBody(received))
		return true
	}
	return mock
}

// environmentFileListing renders the live finalize shape; sizeJSON is the raw
// size_bytes value, or "" to omit it as proto3 JSON does for 0.
func environmentFileListing(path, sizeJSON string) string {
	sizeField := ""
	if sizeJSON != "" {
		sizeField = `,"size_bytes":` + sizeJSON
	}
	return fmt.Sprintf(`{"files":[{"name":%q,"path":%q,"type":"FILE","mime_type":"text/plain"%s}]}`, filepath.Base(path), path, sizeField)
}

// storesEverything reports every received byte as stored.
func storesEverything(path string) func(int) string {
	return func(received int) string {
		if received == 0 {
			return environmentFileListing(path, "")
		}
		return environmentFileListing(path, fmt.Sprintf("%q", fmt.Sprint(received)))
	}
}

// TestEnvironmentFilesUploadProtocol pins the resumable exchange end to end:
// PUT start on the environment file, then POST chunks to the session URL
// carrying exactly the file's bytes, with "finalize" riding only the last one.
func TestEnvironmentFilesUploadProtocol(t *testing.T) {
	t.Run("start and single chunk", func(t *testing.T) {
		mock := newEnvironmentUploadMock(t, storesEverything("src/main.py"))
		src := writeTempFile(t, "main.py", "print('hi')\n")
		result := runCLI(t, t.TempDir(), nil, environmentUploadArgs("--path", "src/main.py", "--overwrite")(mock.URL, src)...)
		if result.err != nil {
			t.Fatalf("environments files upload failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if got := strings.TrimSpace(result.stdout); got != "src/main.py" {
			t.Errorf("stdout = %q, want src/main.py", got)
		}
		reqs := mock.recorded()
		if len(reqs) != 2 {
			t.Fatalf("requests = %d, want start + one chunk", len(reqs))
		}
		start, chunk := reqs[0], reqs[1]
		if start.method != http.MethodPut || start.path != "/upload/v1beta/environments/env_abc/files/src/main.py" || start.query != "overwrite=true" {
			t.Errorf("start = %s %s?%s, want PUT /upload/v1beta/environments/env_abc/files/src/main.py?overwrite=true", start.method, start.path, start.query)
		}
		if len(start.body) != 0 {
			t.Errorf("start body = %q, want empty", start.body)
		}
		for header, want := range map[string]string{
			"X-Goog-Upload-Protocol":              "resumable",
			"X-Goog-Upload-Header-Content-Length": "12",
			"X-Goog-Upload-Header-Content-Type":   "text/x-python",
			"X-Goog-Api-Key":                      "test-key",
		} {
			if got := start.headers.Get(header); got != want {
				t.Errorf("start %s = %q, want %q", header, got, want)
			}
		}
		if chunk.path != "/upload-session" || chunk.headers.Get("X-Goog-Api-Key") != "test-key" {
			t.Errorf("chunk = %s with key %q, want /upload-session with the resolved API key", chunk.path, chunk.headers.Get("X-Goog-Api-Key"))
		}
		checkChunks(t, reqs[1:], []byte("print('hi')\n"), uploadSize{"", 12, []string{"0"}, []string{"upload, finalize"}})
	})

	sizes := append([]uploadSize{{"empty file", 0, []string{"0"}, []string{"upload, finalize"}}}, uploadSizes...)
	for _, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			mock := newEnvironmentUploadMock(t, storesEverything("data.bin"))
			data := patterned(tc.size)
			src := filepath.Join(t.TempDir(), "data.bin")
			if err := os.WriteFile(src, data, 0o644); err != nil {
				t.Fatal(err)
			}
			result := runCLI(t, t.TempDir(), nil, environmentUploadArgs()(mock.URL, src)...)
			if result.err != nil {
				t.Fatalf("upload failed: %v\nstderr: %s", result.err, result.stderr)
			}
			checkChunks(t, mock.chunks(), data, tc)
		})
	}
}

// TestEnvironmentFilesUploadVerifiesStoredSize: unless the finalize response
// proves the server stored the whole local file, the command fails instead of
// reporting a file the environment does not hold.
func TestEnvironmentFilesUploadVerifiesStoredSize(t *testing.T) {
	listing := func(sizeJSON string) func(int) string {
		return func(int) string { return environmentFileListing("hello.txt", sizeJSON) }
	}
	cases := []struct {
		name       string
		finalize   func(int) string
		extra      []string
		wantStderr string
	}{
		{"size_bytes omitted (0 stored)", listing(""), nil, "the server stored 0 bytes at hello.txt, expected 12"},
		{"partial size", listing(`"5"`), nil, "the server stored 5 bytes at hello.txt, expected 12"},
		{"larger size", listing(`"20"`), nil, "the server stored 20 bytes at hello.txt, expected 12"},
		{"unreadable size", listing(`"12 "`), nil, `the server reported an unreadable size_bytes "12 "`},
		{"fractional size", listing(`12.5`), nil, "the server reported an unreadable size_bytes 12.5"},
		{"size beyond int64", listing(`1e30`), nil, "the server reported an unreadable size_bytes 1e+30"},
		{"negative size", listing(`"-5"`), nil, `the server reported an unreadable size_bytes "-5"`},
		{"several files entries", func(int) string {
			return `{"files":[{"path":"hello.txt","size_bytes":"12"},{"path":"other.txt","size_bytes":"12"}]}`
		}, nil, "did not describe the stored file, so its size could not be verified"},
		{"empty finalize body", func(int) string { return "" }, nil, "did not describe the stored file, so its size could not be verified"},
		{"finalize body not JSON", func(int) string { return "<html>" }, nil, "did not describe the stored file, so its size could not be verified"},
		{"matching string size", listing(`"12"`), nil, ""},
		{"matching number size", listing(`12`), nil, ""},
		{"extract skips the check", listing(""), []string{"--extract"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newEnvironmentUploadMock(t, tc.finalize)
			src := writeTempFile(t, "hello.txt", "hello world\n")
			result := runCLI(t, t.TempDir(), nil, environmentUploadArgs(tc.extra...)(mock.URL, src)...)
			wantCode := 1
			if tc.wantStderr == "" {
				wantCode = 0
			}
			if code := exitCode(t, result); code != wantCode {
				t.Fatalf("exit code = %d, want %d\nstderr: %s", code, wantCode, result.stderr)
			}
			if wantCode == 0 {
				if got := strings.TrimSpace(result.stdout); got != "hello.txt" {
					t.Errorf("stdout = %q, want hello.txt", got)
				}
				return
			}
			if result.stdout != "" {
				t.Errorf("failure wrote stdout: %q", result.stdout)
			}
			for _, want := range []string{tc.wantStderr, `"gemini-api environments files list --environment env_abc --path hello.txt"`} {
				if !strings.Contains(result.stderr, want) {
					t.Errorf("stderr missing %q:\n%s", want, result.stderr)
				}
			}
		})
	}

	t.Run("empty file needs no size", func(t *testing.T) {
		mock := newEnvironmentUploadMock(t, func(int) string { return "" })
		src := writeTempFile(t, "empty.txt", "")
		result := runCLI(t, t.TempDir(), nil, environmentUploadArgs()(mock.URL, src)...)
		if result.err != nil {
			t.Fatalf("0-byte upload with an empty finalize body failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if got := strings.TrimSpace(result.stdout); got != "empty.txt" {
			t.Errorf("stdout = %q, want empty.txt", got)
		}
	})

	t.Run("hint quotes the destination path", func(t *testing.T) {
		mock := newEnvironmentUploadMock(t, func(int) string { return environmentFileListing("my dir/hello.txt", "") })
		src := writeTempFile(t, "hello.txt", "hello world\n")
		result := runCLI(t, t.TempDir(), nil, environmentUploadArgs("--path", "my dir/hello.txt")(mock.URL, src)...)
		if want := `--path 'my dir/hello.txt'"`; !strings.Contains(result.stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, result.stderr)
		}
	})
}

// TestEnvironmentFilesUploadFailures pins every way the exchange can go
// wrong: a classified exit code, a named cause, nothing on stdout.
func TestEnvironmentFilesUploadFailures(t *testing.T) {
	cases := append(uploadExchangeFailures(),
		uploadFailure{"finalize server error", 16, "upload, finalize", answerStatus(http.StatusInternalServerError, `{"error":{"code":500,"message":"Internal error encountered.","status":"INTERNAL"}}`), "Internal error encountered.", 1, 1},
	)
	runUploadFailures(t, cases, environmentUploadArgs())
}

// TestEnvironmentFilesUploadUnreadableFile: an unreadable local file is a
// usage error, as for files upload, and a bad --path is reported first.
func TestEnvironmentFilesUploadUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of permissions")
	}
	src := writeTempFile(t, "secret.txt", "hello world\n")
	if err := os.Chmod(src, 0o000); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, wantStderr string
	}{
		{"valid path", "secret.txt", "cannot read"},
		{"invalid path", "../x", `invalid path "../x"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := runCLI(t, t.TempDir(), nil, environmentUploadArgs("--path", tc.path)("http://127.0.0.1:1", src)...)
			if code := exitCode(t, result); code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr: %s", code, result.stderr)
			}
			if !strings.Contains(result.stderr, tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, result.stderr)
			}
		})
	}
}

// TestEnvironmentFilesUploadRefusesForeignSession: a session URL on another
// host never receives the credentials or the file bytes.
func TestEnvironmentFilesUploadRefusesForeignSession(t *testing.T) {
	checkForeignSessionRefused(t, environmentUploadArgs())
}

// TestEnvironmentFilesUploadDryRun previews the start request and every chunk
// without touching the network.
func TestEnvironmentFilesUploadDryRun(t *testing.T) {
	cases := []struct {
		name        string
		size        int
		wantMarkers []string
	}{
		{"two chunks", uploadChunk + 5, []string{"<bytes:8388608>", "<bytes:5>"}},
		{"empty file", 0, []string{"X-Goog-Upload-Command: upload, finalize"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
			}))
			defer server.Close()

			src := filepath.Join(t.TempDir(), "data.bin")
			if err := os.WriteFile(src, patterned(tc.size), 0o644); err != nil {
				t.Fatal(err)
			}
			result := runCLI(t, t.TempDir(), nil, environmentUploadArgs("--overwrite", "--dry-run")(server.URL, src)...)
			if result.err != nil {
				t.Fatalf("environments files upload --dry-run failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if requests.Load() != 0 {
				t.Errorf("dry-run made %d network requests, want 0", requests.Load())
			}
			wants := append([]string{
				"[DRY-RUN] Would send: PUT " + server.URL + "/upload/v1beta/environments/env_abc/files/data.bin?overwrite=true",
				"[DRY-RUN] Would send: POST " + server.URL + "/upload/v1beta/environments/env_abc/files/data.bin/dry-run-session",
			}, tc.wantMarkers...)
			for _, want := range wants {
				if !strings.Contains(result.stderr, want) {
					t.Errorf("dry-run stderr missing %q:\n%s", want, result.stderr)
				}
			}
			if got, want := strings.Count(result.stderr, "[DRY-RUN] Would send"), 1+len(tc.wantMarkers); got != want {
				t.Errorf("dry-run previews = %d, want %d:\n%s", got, want, result.stderr)
			}
		})
	}
}
