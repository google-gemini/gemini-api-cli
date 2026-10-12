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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestBodylessOperationsRequireTheirIdentifier pins the guard on every
// generated body-less operation with a required flag: absent or blank, the
// command is a usage error and nothing is sent or previewed — a blank id would
// otherwise address the collection path ("DELETE /webhooks/").
func TestBodylessOperationsRequireTheirIdentifier(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	commands := []struct {
		path []string
		flag string
	}{
		{[]string{"agent", "get"}, "--id"},
		{[]string{"agent", "delete"}, "--id"},
		{[]string{"agent", "cancel"}, "--id"},
		{[]string{"agent", "delete-interaction"}, "--id"},
		{[]string{"agent", "status"}, "--id"},
		{[]string{"triggers", "get"}, "--id"},
		{[]string{"triggers", "delete"}, "--id"},
		{[]string{"triggers", "run"}, "--id"},
		{[]string{"triggers", "list-executions"}, "--id"},
		{[]string{"credentials", "get"}, "--id"},
		{[]string{"credentials", "delete"}, "--id"},
		{[]string{"environments", "get"}, "--id"},
		{[]string{"environments", "delete"}, "--id"},
		{[]string{"webhooks", "get"}, "--id"},
		{[]string{"webhooks", "delete"}, "--id"},
	}
	for _, tc := range commands {
		name := strings.Join(tc.path, " ")
		for label, extra := range map[string][]string{
			"absent": nil, "empty": {tc.flag, ""}, "blank": {tc.flag, "  "},
		} {
			t.Run(name+"/"+label, func(t *testing.T) {
				for _, mode := range [][]string{nil, {"--dry-run"}} {
					before := requests.Load()
					args := append(append(plainArgs(server.URL), mode...), tc.path...)
					result := runCLI(t, t.TempDir(), nil, append(args, extra...)...)
					if code := exitCode(t, result); code != 2 {
						t.Errorf("%v exit code = %d, want 2\nstderr: %s", mode, code, result.stderr)
					}
					if !strings.Contains(result.stderr, "missing required flag: "+tc.flag) &&
						!strings.Contains(result.stderr, "blank; path parameters require a non-empty value") {
						t.Errorf("%v stderr missing the named flag %s:\n%s", mode, tc.flag, result.stderr)
					}
					if strings.Contains(result.stderr, "[DRY-RUN]") || requests.Load() != before {
						t.Errorf("%v previewed or sent a request:\n%s", mode, result.stderr)
					}
				}
			})
		}
		t.Run(name+"/supplied", func(t *testing.T) {
			before := requests.Load()
			args := append(append(plainArgs(server.URL), tc.path...), tc.flag, "abc")
			runCLI(t, t.TempDir(), nil, args...)
			if requests.Load() == before {
				t.Error("a supplied identifier did not dispatch the request")
			}
		})
	}
}

// TestFilesRegisterRequiresURIs: the schema marks uris required, so an empty
// registration is a usage error instead of a POST of {}.
func TestFilesRegisterRequiresURIs(t *testing.T) {
	var requests atomic.Int32
	var body atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"files":[]}`))
	}))
	defer server.Close()

	for _, mode := range [][]string{nil, {"--dry-run"}} {
		args := append(append(plainArgs(server.URL), mode...), "files", "register")
		result := runCLI(t, t.TempDir(), nil, args...)
		if code := exitCode(t, result); code != 2 {
			t.Errorf("%v exit code = %d, want 2\nstderr: %s", mode, code, result.stderr)
		}
		if !strings.Contains(result.stderr, "missing required flag: --uris") {
			t.Errorf("%v stderr does not name --uris:\n%s", mode, result.stderr)
		}
	}
	if requests.Load() != 0 {
		t.Errorf("requests = %d, want none", requests.Load())
	}

	// --uris takes one JSON array; the body must carry its elements, not the
	// array text as a single string.
	args := append(plainArgs(server.URL), "files", "register", "--uris", `["gs://bucket/object"]`)
	if result := runCLI(t, t.TempDir(), nil, args...); result.err != nil || requests.Load() != 1 {
		t.Errorf("register with --uris: err %v, requests %d\nstderr: %s", result.err, requests.Load(), result.stderr)
	}
	var got string
	if v := body.Load(); v != nil {
		got = strings.TrimSpace(v.(string))
	}
	if want := `{"uris":["gs://bucket/object"]}`; got != want {
		t.Errorf("request body = %q, want %q", got, want)
	}
}
