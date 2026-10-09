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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// explicitAPIRevision exercises the opt-in header path; any valid revision
// date works, the wire echo is what matters.
const explicitAPIRevision = "2026-05-20"

// captureServer records the last request's method, path, headers and JSON body.
type capturedCall struct {
	method string
	path   string
	header http.Header
	body   map[string]any
}

func newCaptureServer(t *testing.T, reply string) (*httptest.Server, chan capturedCall) {
	t.Helper()
	calls := make(chan capturedCall, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		calls <- capturedCall{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(server.Close)
	return server, calls
}

// TestAPIRevisionOmittedByDefault pins the header contract the official SDKs
// converged on (js-genai removed its default Api-Revision header 2026-07-08):
// no Api-Revision is sent unless --api-revision is passed, and the explicit
// flag reaches the wire on every interactions request shape.
func TestAPIRevisionOmittedByDefault(t *testing.T) {
	server, calls := newCaptureServer(t, `{"id":"int-1","status":"completed"}`)

	invocations := [][]string{
		{"agent", "run", "hello", "--model", "gemini-3.6-flash", "--stream=false"},
		{"agent", "run", "hello", "--agent", "deep-research-preview-04-2026", "--stream=false"},
		{"agent", "status", "--id", "int-1"},
	}
	for _, args := range invocations {
		result := runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL), args...)...)
		if result.err != nil {
			t.Fatalf("%v failed: %v\nstderr: %s", args, result.err, result.stderr)
		}
		call := <-calls
		if got := call.header.Get("Api-Revision"); got != "" {
			t.Errorf("%v: Api-Revision = %q, want the header omitted by default", args, got)
		}

		withFlag := append(append(baseArgs(server.URL), args...), "--api-revision", explicitAPIRevision)
		result = runCLI(t, t.TempDir(), nil, withFlag...)
		if result.err != nil {
			t.Fatalf("%v --api-revision failed: %v\nstderr: %s", args, result.err, result.stderr)
		}
		call = <-calls
		if got := call.header.Get("Api-Revision"); got != explicitAPIRevision {
			t.Errorf("%v: explicit Api-Revision = %q, want %q on the wire", args, got, explicitAPIRevision)
		}
	}
}

// TestAPIRevisionHasNoBakedDefault guards the omission contract at its
// sources: the overlay must not declare a header default and the generated
// root flag must default to empty, so a regeneration cannot silently
// reintroduce a pin.
func TestAPIRevisionHasNoBakedDefault(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}

	overlayPath := filepath.Join(root, "overlays", "reference-cli", "behavior.yaml")
	overlay, err := os.ReadFile(overlayPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("skipping overlay check because overlays/reference-cli/behavior.yaml is not present in this repository")
		}
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`name: Api-Revision[\s\S]*?default: "`).Find(overlay); m != nil {
		t.Errorf("behavior.yaml declares an Api-Revision header default; the header must be omitted unless --api-revision is passed")
	}

	rootGo, err := os.ReadFile(filepath.Join(root, "internal", "cli", "root.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rootGo), `"api-revision", ""`) {
		t.Errorf("generated root.go does not register --api-revision with an empty default; regenerate after changing the overlay")
	}
}

// TestAgentRunRouteDispatch pins Build Spec §9: "agent run" renders the
// create-interaction union as mutually exclusive --model / --agent flag sets
// (never one JSON-string flag), dispatches on user evidence, and defaults to
// the schema's model variant.
func TestAgentRunRouteDispatch(t *testing.T) {
	reply := `{"id":"int-1","status":"completed"}`

	t.Run("default is the model variant with the streaming preset", func(t *testing.T) {
		server, calls := newCaptureServer(t, reply)
		result := runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL), "agent", "run", "hello there")...)
		if result.err != nil {
			t.Fatalf("agent run failed: %v\nstderr: %s", result.err, result.stderr)
		}
		call := <-calls
		if got := call.body["model"]; got != "gemini-3.8-flash" {
			t.Errorf("model = %v, want schema default gemini-3.8-flash", got)
		}
		if got := call.body["stream"]; got != true {
			t.Errorf("stream = %v, want the streaming preset true", got)
		}
		if got := call.body["input"]; got != "hello there" {
			t.Errorf("input = %v, want the positional prompt", got)
		}
		if _, has := call.body["agent"]; has {
			t.Errorf("body names agent on the default model route: %v", call.body)
		}
	})

	t.Run("--agent selects the agent variant", func(t *testing.T) {
		server, calls := newCaptureServer(t, reply)
		result := runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL),
			"agent", "run", "analyze this", "--agent", "deep-research-preview-04-2026")...)
		if result.err != nil {
			t.Fatalf("agent run --agent failed: %v\nstderr: %s", result.err, result.stderr)
		}
		call := <-calls
		if got := call.body["agent"]; got != "deep-research-preview-04-2026" {
			t.Errorf("agent = %v, want the selected agent", got)
		}
		if _, has := call.body["model"]; has {
			t.Errorf("model default leaked onto the agent variant: %v", call.body)
		}
	})

	t.Run("--model and --agent together is a usage error and nothing is sent", func(t *testing.T) {
		server, calls := newCaptureServer(t, reply)
		result := runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL),
			"agent", "run", "x", "--model", "gemini-3.6-flash", "--agent", "deep-research-preview-04-2026")...)
		if result.err == nil {
			t.Fatal("conflicting selectors did not fail")
		}
		combined := result.stdout + result.stderr
		if !strings.Contains(combined, "select different request variants; pass exactly one") {
			t.Errorf("conflict error not reported:\n%s", combined)
		}
		select {
		case call := <-calls:
			t.Errorf("a request was sent despite conflicting selectors: %+v", call)
		default:
		}
	})

	t.Run("agent-only evidence demands --agent", func(t *testing.T) {
		// agent_config is declared only by the Agent variant.
		server, _ := newCaptureServer(t, reply)
		result := runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL),
			"agent", "run", "--body", `{"agent_config":{"type":"antigravity"},"input":"x"}`)...)
		if result.err == nil {
			t.Fatal("agent-only evidence without --agent did not fail")
		}
		if !strings.Contains(result.stdout+result.stderr, "required flag --agent not set for the Agent variant") {
			t.Errorf("missing conditional-required error:\n%s%s", result.stdout, result.stderr)
		}
	})

	t.Run("a body naming agent dispatches the agent route", func(t *testing.T) {
		server, calls := newCaptureServer(t, reply)
		result := runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL),
			"agent", "run", "--body", `{"agent":"deep-research-preview-04-2026","input":"from body"}`)...)
		if result.err != nil {
			t.Fatalf("agent run --body failed: %v\nstderr: %s", result.err, result.stderr)
		}
		call := <-calls
		if got := call.body["agent"]; got != "deep-research-preview-04-2026" {
			t.Errorf("agent = %v, want body evidence to select the agent route", got)
		}
		if got := call.body["stream"]; got != true {
			t.Errorf("stream = %v, want the command preset applied after body dispatch", got)
		}
		if _, has := call.body["model"]; has {
			t.Errorf("model default injected into an agent body: %v", call.body)
		}
	})

	t.Run("the union JSON flag is gone", func(t *testing.T) {
		result := runCLI(t, t.TempDir(), nil, "agent", "run", "--body-param", `{}`)
		if result.err == nil {
			t.Fatal("--body-param was accepted")
		}
		if !strings.Contains(result.stdout+result.stderr, "unknown flag: --body-param") {
			t.Errorf("--body-param should be an unknown flag:\n%s%s", result.stdout, result.stderr)
		}
	})

	t.Run("help shows both variant flag sets", func(t *testing.T) {
		result := runCLI(t, t.TempDir(), nil, "agent", "run", "--help")
		if result.err != nil {
			t.Fatalf("agent run --help failed: %v", result.err)
		}
		modelIdx := strings.Index(result.stdout, "Model variant Flags:")
		agentIdx := strings.Index(result.stdout, "Agent variant Flags:")
		if modelIdx < 0 || agentIdx < 0 {
			t.Fatalf("help does not show both variant flag sections:\n%s", result.stdout)
		}
		for _, want := range []string{"--agent string", "-m, --model string", "Request variants:"} {
			if !strings.Contains(result.stdout, want) {
				t.Errorf("help missing %q:\n%s", want, result.stdout)
			}
		}
		if strings.Contains(result.stdout, "--body-param") {
			t.Errorf("help still mentions --body-param:\n%s", result.stdout)
		}
	})
}
