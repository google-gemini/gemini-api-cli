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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

func TestEnvironmentFilesHeadersTimeoutAndServerURLValidation(t *testing.T) {
	mock := newEnvironmentUploadMock(t, storesEverything("src/main.py"))
	src := writeTempFile(t, "main.py", "print('hi')\n")

	uploadArgs := append(
		plainArgs(mock.URL),
		"-H", "X-A: 1",
		"-H", "x-a: 2",
		"--timeout", "30s",
		"environments", "files", "upload", "env_abc", src, "--path", "src/main.py", "--overwrite",
	)
	res := runCLI(t, t.TempDir(), nil, uploadArgs...)
	if res.err != nil {
		t.Fatalf("environments files upload failed: %v\nstderr: %s", res.err, res.stderr)
	}
	for _, r := range mock.recorded() {
		if got := r.headers.Values("X-A"); !slices.Equal(got, []string{"1", "2"}) {
			t.Errorf("%s X-A = %v, want [1 2]", r.command, got)
		}
	}

	for _, cmdTail := range [][]string{
		{"environments", "files", "upload", "env_abc", src, "--path", "src/main.py"},
		{"environments", "files", "download", "env_abc", "src/main.py"},
	} {
		for _, badFlags := range [][]string{
			{"--timeout", "0s"},
			{"--timeout", "-1s"},
			{"--timeout", "abc"},
			{"-H", "InvalidHeaderWithoutColon"},
			{"-H", "X-Cli-Repeated-Headers: X-A"},
			{"--server-url", "ftp://x"},
			{"--server-url", "https://"},
		} {
			args := append(append(plainArgs(mock.URL), badFlags...), cmdTail...)
			badRes := runCLI(t, t.TempDir(), nil, args...)
			if code := exitCode(t, badRes); code != 2 {
				t.Errorf("%v %v: exit code = %d, want 2\nstderr: %s", cmdTail[2], badFlags, code, badRes.stderr)
			}
		}
	}
}

func TestEnvironmentFilesArgCollisionsAndForms(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	workDir := t.TempDir()
	samplePath := filepath.Join(workDir, "sample.txt")
	if err := os.WriteFile(samplePath, []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	collisionAndExcess := []struct {
		name    string
		tail    []string
		wantMsg string
	}{
		{
			name:    "upload positional and -e collision",
			tail:    []string{"environments", "files", "upload", "env_demo", "sample.txt", "-e", "env_demo", "--dry-run", "-o", "json"},
			wantMsg: "pass environment once: as the <environment> argument or via --environment, not both",
		},
		{
			name:    "upload positional and -f collision",
			tail:    []string{"environments", "files", "upload", "env_demo", "sample.txt", "-f", "sample.txt", "--dry-run", "-o", "json"},
			wantMsg: "pass file once: as the <local-file> argument or via --file, not both",
		},
		{
			name:    "upload single positional with both flags (file collision)",
			tail:    []string{"environments", "files", "upload", "sample.txt", "-e", "env_demo", "-f", "sample.txt", "--dry-run", "-o", "json"},
			wantMsg: "pass file once: as the <local-file> argument or via --file, not both",
		},
		{
			name:    "upload single positional with both flags (env collision)",
			tail:    []string{"environments", "files", "upload", "env_demo", "-e", "env_demo", "-f", "sample.txt", "--dry-run", "-o", "json"},
			wantMsg: "pass environment once: as the <environment> argument or via --environment, not both",
		},
		{
			name:    "upload genuine excess arguments",
			tail:    []string{"environments", "files", "upload", "env_demo", "sample.txt", "extra.txt", "--dry-run", "-o", "json"},
			wantMsg: "accepts 2 arguments (<environment> <local-file>), received 3",
		},
		{
			name:    "download positional and -p collision",
			tail:    []string{"environments", "files", "download", "env_demo", "remote.txt", "-p", "remote.txt", "--dry-run", "-o", "json"},
			wantMsg: "pass path once: as the <path> argument or via --path, not both",
		},
		{
			name:    "download positional and -e collision",
			tail:    []string{"environments", "files", "download", "env_demo", "remote.txt", "-e", "env_demo", "--dry-run", "-o", "json"},
			wantMsg: "pass environment once: as the <environment> argument or via --environment, not both",
		},
		{
			name:    "download single positional with both flags (path collision)",
			tail:    []string{"environments", "files", "download", "remote.txt", "-e", "env_demo", "-p", "remote.txt", "--dry-run", "-o", "json"},
			wantMsg: "pass path once: as the <path> argument or via --path, not both",
		},
		{
			name:    "download single positional with both flags (env collision)",
			tail:    []string{"environments", "files", "download", "env_demo", "-e", "env_demo", "-p", "remote.txt", "--dry-run", "-o", "json"},
			wantMsg: "pass environment once: as the <environment> argument or via --environment, not both",
		},
		{
			name:    "download genuine excess arguments",
			tail:    []string{"environments", "files", "download", "env_demo", "remote.txt", "extra.txt", "--dry-run", "-o", "json"},
			wantMsg: "accepts 2 arguments (<environment> <path>), received 3",
		},
	}

	for _, tc := range collisionAndExcess {
		t.Run(tc.name, func(t *testing.T) {
			res := runCLIInDir(t, t.TempDir(), workDir, nil, append(plainArgs(server.URL), tc.tail...)...)
			if code := exitCode(t, res); code != 2 {
				t.Fatalf("exit code = %d, want 2\nstderr: %s", code, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("expected empty stdout, got %q", res.stdout)
			}
			var errObj struct {
				Error       string `json:"error"`
				ErrorReason string `json:"error_reason"`
				ErrorType   string `json:"error_type"`
				ExitCode    int    `json:"exit_code"`
				Message     string `json:"message"`
			}
			if err := json.Unmarshal([]byte(res.stderr), &errObj); err != nil {
				t.Fatalf("stderr is not JSON: %v\nraw: %s", err, res.stderr)
			}
			if errObj.ErrorReason != "CLI_VALIDATION" {
				t.Errorf("error_reason = %q, want CLI_VALIDATION", errObj.ErrorReason)
			}
			if errObj.ExitCode != 2 {
				t.Errorf("exit_code = %d, want 2", errObj.ExitCode)
			}
			if !strings.Contains(errObj.Message, tc.wantMsg) || !strings.Contains(errObj.Error, tc.wantMsg) {
				t.Errorf("message = %q (error = %q), want substring %q", errObj.Message, errObj.Error, tc.wantMsg)
			}
		})
	}

	validForms := []struct {
		name string
		tail []string
		kind string
	}{
		{"upload positional-only", []string{"environments", "files", "upload", "env_demo", "sample.txt", "--dry-run", "-o", "json"}, "upload"},
		{"upload flag-only", []string{"environments", "files", "upload", "-e", "env_demo", "-f", "sample.txt", "--dry-run", "-o", "json"}, "upload"},
		{"upload mixed env-pos file-flag", []string{"environments", "files", "upload", "env_demo", "-f", "sample.txt", "--dry-run", "-o", "json"}, "upload"},
		{"upload mixed env-flag file-pos", []string{"environments", "files", "upload", "-e", "env_demo", "sample.txt", "--dry-run", "-o", "json"}, "upload"},
		{"download positional-only", []string{"environments", "files", "download", "env_demo", "remote.txt", "--dry-run", "-o", "json"}, "download"},
		{"download flag-only", []string{"environments", "files", "download", "-e", "env_demo", "-p", "remote.txt", "--dry-run", "-o", "json"}, "download"},
		{"download mixed env-pos path-flag", []string{"environments", "files", "download", "env_demo", "-p", "remote.txt", "--dry-run", "-o", "json"}, "download"},
		{"download mixed env-flag path-pos", []string{"environments", "files", "download", "-e", "env_demo", "remote.txt", "--dry-run", "-o", "json"}, "download"},
	}

	for _, tc := range validForms {
		t.Run(tc.name, func(t *testing.T) {
			res := runCLIInDir(t, t.TempDir(), workDir, nil, append(plainArgs(server.URL), tc.tail...)...)
			if res.err != nil {
				t.Fatalf("dry-run failed: %v\nstderr: %s", res.err, res.stderr)
			}
			lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
			if tc.kind == "upload" {
				if len(lines) != 2 {
					t.Fatalf("upload dry-run lines = %d, want 2:\n%s", len(lines), res.stdout)
				}
				var start map[string]any
				if err := json.Unmarshal([]byte(lines[0]), &start); err != nil {
					t.Fatalf("unmarshal upload start: %v", err)
				}
				req, _ := start["request"].(map[string]any)
				if gotURL, _ := req["url"].(string); !strings.HasSuffix(gotURL, "/upload/v1beta/environments/env_demo/files/sample.txt") {
					t.Errorf("upload start url = %q, want suffix /upload/v1beta/environments/env_demo/files/sample.txt", gotURL)
				}
			} else {
				if len(lines) != 1 {
					t.Fatalf("download dry-run lines = %d, want 1:\n%s", len(lines), res.stdout)
				}
				var preview map[string]any
				if err := json.Unmarshal([]byte(lines[0]), &preview); err != nil {
					t.Fatalf("unmarshal download preview: %v", err)
				}
				req, _ := preview["request"].(map[string]any)
				if gotURL, _ := req["url"].(string); !strings.HasSuffix(gotURL, "/v1beta/environments/env_demo/files/remote.txt?alt=media") {
					t.Errorf("download preview url = %q, want suffix /v1beta/environments/env_demo/files/remote.txt?alt=media", gotURL)
				}
				if _, err := os.Stat(filepath.Join(workDir, "remote.txt")); !os.IsNotExist(err) {
					t.Errorf("download dry-run created remote.txt on disk: err=%v", err)
				}
			}
		})
	}

	if got := requests.Load(); got != 0 {
		t.Fatalf("expected 0 network calls across all dry-run and validation checks, got %d", got)
	}
}

func TestEnvironmentFilesDownloadOutputPathsAndValidation(t *testing.T) {
	payload := []byte("downloaded environment file content\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1beta/environments/env_demo/files/dir/remote.txt" || r.URL.RawQuery != "alt=media" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	workDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	existingDir := filepath.Join(workDir, "existing-dir")
	if err := os.MkdirAll(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	destCases := []struct {
		name    string
		outArgs []string
		wantAbs string
	}{
		{
			name:    "default destination",
			outArgs: nil,
			wantAbs: filepath.Join(workDir, "remote.txt"),
		},
		{
			name:    "dot-slash destination",
			outArgs: []string{"--out", "./"},
			wantAbs: filepath.Join(workDir, "remote.txt"),
		},
		{
			name:    "relative filename",
			outArgs: []string{"--out", filepath.Join("sub", "rel-remote.txt")},
			wantAbs: filepath.Join(workDir, "sub", "rel-remote.txt"),
		},
		{
			name:    "absolute filename",
			outArgs: []string{"--out", filepath.Join(workDir, "abs-remote.txt")},
			wantAbs: filepath.Join(workDir, "abs-remote.txt"),
		},
		{
			name:    "existing directory",
			outArgs: []string{"--out", existingDir},
			wantAbs: filepath.Join(existingDir, "remote.txt"),
		},
	}

	for _, tc := range destCases {
		t.Run(tc.name, func(t *testing.T) {
			humanArgs := append(append(plainArgs(server.URL), "environments", "files", "download", "env_demo", "dir/remote.txt"), tc.outArgs...)
			humanRes := runCLIInDir(t, t.TempDir(), workDir, nil, humanArgs...)
			if humanRes.err != nil {
				t.Fatalf("human download failed: %v\nstderr: %s", humanRes.err, humanRes.stderr)
			}
			gotHuman := strings.TrimSpace(humanRes.stdout)
			if gotHuman != tc.wantAbs {
				t.Errorf("human stdout = %q, want %q", gotHuman, tc.wantAbs)
			}
			diskBytes, err := os.ReadFile(tc.wantAbs)
			if err != nil || !bytes.Equal(diskBytes, payload) {
				t.Errorf("ReadFile(%q) = %q, %v; want %q", tc.wantAbs, diskBytes, err, payload)
			}

			jsonArgs := append(append([]string(nil), humanArgs...), "-o", "json")
			jsonRes := runCLIInDir(t, t.TempDir(), workDir, nil, jsonArgs...)
			if jsonRes.err != nil {
				t.Fatalf("json download failed: %v\nstderr: %s", jsonRes.err, jsonRes.stderr)
			}
			var outEnvelope map[string]any
			if err := json.Unmarshal([]byte(jsonRes.stdout), &outEnvelope); err != nil {
				t.Fatalf("unmarshal json download output: %v\nraw: %s", err, jsonRes.stdout)
			}
			gotJSONPath, _ := outEnvelope["output_path"].(string)
			if gotJSONPath != tc.wantAbs || gotJSONPath != gotHuman {
				t.Errorf("json output_path = %q, human = %q, want %q", gotJSONPath, gotHuman, tc.wantAbs)
			}
		})
	}

	// Invalid destination error: parent path is an existing regular file.
	blockerFile := filepath.Join(workDir, "blocker-file")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	badOutArgs := append(plainArgs(server.URL), "environments", "files", "download", "env_demo", "dir/remote.txt", "--out", filepath.Join(blockerFile, "out.txt"))
	badRes := runCLIInDir(t, t.TempDir(), workDir, nil, badOutArgs...)
	if badRes.err == nil {
		t.Fatalf("expected invalid destination error when parent is a file, got nil")
	}
}
