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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func slowChunkedVideoServer(t *testing.T, createBody string, gotPolls *[]string) *httptest.Server {
	payload := base64.StdEncoding.EncodeToString([]byte("mock-video-bytes"))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			fl := w.(http.Flusher)
			for i := 0; i < len(createBody); i += 64 * 1024 {
				end := i + 64*1024
				if end > len(createBody) {
					end = len(createBody)
				}
				fmt.Fprint(w, createBody[i:end])
				fl.Flush()
				if i == 0 {
					time.Sleep(200 * time.Millisecond)
				}
			}
			return
		}
		*gotPolls = append(*gotPolls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"int-race-1","object":"interaction","status":"completed","model":"m","steps":[{"type":"model_output","content":[{"type":"video","data":"%s","mime_type":"video/mp4"}]}]}`, payload)
	}))
}

// Repro attempts for the eval's video "response-body lifetime race": the create
// response arrives chunked/slow (and, in the large variant, bigger than any
// single read buffer); the recipe must still extract the interaction id.
func TestVideoHandleSurvivesSlowChunkedCreateResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"small", `{"id":"int-race-1","object":"interaction","status":"in_progress"}`},
		{"multi_megabyte", `{"id":"int-race-1","object":"interaction","status":"in_progress","outputs":[{"type":"text","text":"` + strings.Repeat("x", 3<<20) + `"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPolls []string
			server := slowChunkedVideoServer(t, tc.body, &gotPolls)
			defer server.Close()

			dir := t.TempDir()
			result := runCLI(t, dir, nil, append(baseArgs(server.URL), "video", "race probe", "--out", dir+"/out.mp4")...)
			if result.err != nil {
				t.Fatalf("video failed: %v\nstdout: %s\nstderr: %s", result.err, result.stdout, result.stderr)
			}
			if len(gotPolls) == 0 {
				t.Fatal("no poll request observed")
			}
			for _, p := range gotPolls {
				if strings.Contains(p, "//") || strings.HasSuffix(p, "/interactions/") {
					t.Fatalf("poll path lost the interaction id: %q (all: %v)", p, gotPolls)
				}
			}
		})
	}
}

// --async must print the interaction handle from the same slow chunked response.
func TestVideoAsyncHandleSurvivesSlowChunkedCreateResponse(t *testing.T) {
	var gotPolls []string
	server := slowChunkedVideoServer(t, `{"id":"int-race-1","object":"interaction","status":"in_progress"}`, &gotPolls)
	defer server.Close()

	dir := t.TempDir()
	result := runCLI(t, dir, nil, append(baseArgs(server.URL), "video", "race probe", "--async", "--output-format", "json")...)
	if result.err != nil {
		t.Fatalf("video --async failed: %v\nstdout: %s\nstderr: %s", result.err, result.stdout, result.stderr)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, result.stdout)
	}
	if got, _ := envelope["id"].(string); got != "int-race-1" {
		t.Fatalf("async envelope lost the interaction id: %v", envelope)
	}
}

// The eval hit its failures as 400s — the error body must also survive a slow
// chunked delivery (a lost error body reads as "read on closed response body").
func TestVideoErrorBodySurvivesSlowChunkedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fl := w.(http.Flusher)
		body := `{"error":{"code":"invalid_argument","message":"response_modalities is deprecated; use response_format"}}`
		half := len(body) / 2
		fmt.Fprint(w, body[:half])
		fl.Flush()
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, body[half:])
	}))
	defer server.Close()

	dir := t.TempDir()
	result := runCLI(t, dir, nil, append(baseArgs(server.URL), "video", "race probe", "--output-format", "json")...)
	if result.err == nil {
		t.Fatalf("expected failure, got success: %s", result.stdout)
	}
	combined := result.stdout + result.stderr
	if strings.Contains(combined, "closed response body") {
		t.Fatalf("error body lost to a closed-body read: %s", combined)
	}
	if !strings.Contains(combined, "response_modalities is deprecated") {
		t.Fatalf("server error message not surfaced: %s", combined)
	}
}
