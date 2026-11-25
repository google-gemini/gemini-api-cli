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
	"reflect"
	"strings"
	"sync"
	"testing"
)

type recordedHop struct {
	Method string
	Path   string
	Accept string
	Body   map[string]any
}

type continuationTestServer struct {
	*httptest.Server
	mu   sync.Mutex
	hops []recordedHop
}

func newContinuationServer(t *testing.T, handler func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request)) *continuationTestServer {
	t.Helper()
	s := &continuationTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("unmarshal request body %q: %v", string(raw), err)
			}
		}
		rec := recordedHop{
			Method: r.Method,
			Path:   r.URL.Path,
			Accept: r.Header.Get("Accept"),
			Body:   body,
		}
		s.mu.Lock()
		s.hops = append(s.hops, rec)
		hopNum := len(s.hops)
		s.mu.Unlock()
		handler(hopNum, rec, w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *continuationTestServer) recorded() []recordedHop {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedHop, len(s.hops))
	copy(out, s.hops)
	return out
}

func contHumanArgs(serverURL string, extra ...string) []string {
	args := []string{
		"--server-url", serverURL,
		"--api-version", "v1beta",
		"--no-interactive",
		"--no-retries",
		"--color", "never",
		"--api-key", "test",
	}
	return append(args, extra...)
}

func TestContinuationMultiHopJSON(t *testing.T) {
	runThreeHopJSON := func(t *testing.T, cmdArgs []string) {
		t.Helper()
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch hop {
			case 1:
				fmt.Fprint(w, `{
					"id": "int-1",
					"status": "requires_continuation",
					"continuation_token": "ct-1",
					"previous_interaction_id": "parent-0",
					"created": "2026-11-01T00:00:00Z",
					"updated": "2026-11-01T00:00:01Z",
					"input": "Write a long report",
					"steps": [
						{"type": "user_input", "content": [{"type": "text", "text": "Write a long report"}]},
						{"type": "model_output", "content": [{"type": "text", "text": "Alpha "}]}
					],
					"usage": {
						"total_input_tokens": 100,
						"total_output_tokens": 50,
						"total_thought_tokens": 10,
						"total_tool_use_tokens": 2,
						"total_cached_tokens": 20,
						"total_tokens": 162,
						"input_tokens_by_modality": [{"modality": "text", "tokens": 90}, {"modality": "image", "tokens": 10}],
						"output_tokens_by_modality": [{"modality": "text", "tokens": 50}],
						"cached_tokens_by_modality": [{"modality": "text", "tokens": 20}],
						"tool_use_tokens_by_modality": [{"modality": "text", "tokens": 2}],
						"grounding_tool_count": [{"type": "google_search", "count": 1}]
					}
				}`)
			case 2:
				fmt.Fprint(w, `{
					"id": "int-2",
					"status": "requires_continuation",
					"continuation_token": "ct-2",
					"previous_interaction_id": "int-1",
					"created": "2026-11-01T00:00:01Z",
					"updated": "2026-11-01T00:00:02Z",
					"steps": [
						{"type": "user_input", "content": [{"type": "text", "text": "repeated prompt"}]},
						{"type": "model_output", "content": [{"type": "text", "text": "Beta "}]}
					],
					"usage": {
						"initial_input_tokens": 150,
						"total_input_tokens": 150,
						"total_output_tokens": 60,
						"total_thought_tokens": 15,
						"total_tool_use_tokens": 3,
						"total_cached_tokens": 25,
						"total_tokens": 228,
						"input_tokens_by_modality": [{"modality": "text", "tokens": 150}],
						"output_tokens_by_modality": [{"modality": "text", "tokens": 60}],
						"cached_tokens_by_modality": [{"modality": "text", "tokens": 25}],
						"tool_use_tokens_by_modality": [{"modality": "text", "tokens": 3}],
						"grounding_tool_count": [{"type": "google_search", "count": 2}, {"type": "url_context", "count": 1}]
					}
				}`)
			case 3:
				fmt.Fprint(w, `{
					"id": "int-3",
					"status": "completed",
					"previous_interaction_id": "int-2",
					"created": "2026-11-01T00:00:02Z",
					"updated": "2026-11-01T00:00:03Z",
					"steps": [
						{"type": "user_input", "content": [{"type": "text", "text": "repeated prompt"}]},
						{"type": "model_output", "content": [{"type": "text", "text": "Gamma"}]}
					],
					"usage": {
						"initial_input_tokens": 210,
						"total_input_tokens": 210,
						"total_output_tokens": 70,
						"total_thought_tokens": 20,
						"total_tool_use_tokens": 5,
						"total_cached_tokens": 30,
						"total_tokens": 305,
						"input_tokens_by_modality": [{"modality": "text", "tokens": 210}],
						"output_tokens_by_modality": [{"modality": "text", "tokens": 70}],
						"cached_tokens_by_modality": [{"modality": "text", "tokens": 30}],
						"tool_use_tokens_by_modality": [{"modality": "text", "tokens": 5}],
						"grounding_tool_count": [{"type": "google_search", "count": 1}]
					}
				}`)
			default:
				t.Errorf("unexpected hop %d", hop)
				w.WriteHeader(http.StatusInternalServerError)
			}
		})

		args := append(baseArgs(srv.URL), cmdArgs...)
		res := runCLI(t, t.TempDir(), nil, args...)
		if res.err != nil {
			t.Fatalf("%v failed: %v\nstderr: %s", cmdArgs, res.err, res.stderr)
		}
		if res.stderr != "" {
			t.Errorf("machine-mode stderr must be empty on success, got %q", res.stderr)
		}

		hops := srv.recorded()
		if len(hops) != 3 {
			t.Fatalf("recorded %d hops, want 3", len(hops))
		}

		// Verify Hop 1 carries the initial input, parent ID, and settings, while
		// stored continuation Hops 2-3 chain continuation_token + previous_interaction_id,
		// omit input (already stored server-side), and preserve request settings.
		wantPrevIDs := []any{"parent-0", "int-1", "int-2"}
		wantTokens := []any{nil, "ct-1", "ct-2"}
		for i, hop := range hops {
			if hop.Method != http.MethodPost || hop.Path != "/v1beta/interactions" {
				t.Errorf("hop %d request = %s %s, want POST /v1beta/interactions", i+1, hop.Method, hop.Path)
			}
			if i == 0 {
				if got := hop.Body["input"]; got != "Write a long report" {
					t.Errorf("hop 1 input = %#v, want %q", got, "Write a long report")
				}
			} else {
				if _, hasInput := hop.Body["input"]; hasInput {
					t.Errorf("stored hop %d must omit input, got %#v", i+1, hop.Body["input"])
				}
			}
			if got := hop.Body["model"]; got != "gemini-3.8-flash" {
				t.Errorf("hop %d model = %#v, want %q", i+1, got, "gemini-3.8-flash")
			}
			if got := hop.Body["stream"]; got != false {
				t.Errorf("hop %d stream = %#v, want false", i+1, got)
			}
			if got := hop.Body["previous_interaction_id"]; got != wantPrevIDs[i] {
				t.Errorf("hop %d previous_interaction_id = %#v, want %#v", i+1, got, wantPrevIDs[i])
			}
			if got := hop.Body["continuation_token"]; got != wantTokens[i] {
				t.Errorf("hop %d continuation_token = %#v, want %#v", i+1, got, wantTokens[i])
			}
			if got := hop.Body["system_instruction"]; got != "Be thorough" {
				t.Errorf("hop %d system_instruction = %#v, want %q", i+1, got, "Be thorough")
			}
			genCfg, ok := hop.Body["generation_config"].(map[string]any)
			if !ok {
				t.Fatalf("hop %d missing generation_config: %#v", i+1, hop.Body)
			}
			if genCfg["max_output_tokens"] != float64(4096) || genCfg["thinking_level"] != "high" || genCfg["thinking_summaries"] != "auto" {
				t.Errorf("hop %d generation_config = %#v, want preserved settings", i+1, genCfg)
			}
		}

		var combined map[string]any
		if err := json.Unmarshal([]byte(res.stdout), &combined); err != nil {
			t.Fatalf("unmarshal combined response: %v\nstdout: %s", err, res.stdout)
		}
		if combined["id"] != "int-3" || combined["status"] != "completed" || combined["updated"] != "2026-11-01T00:00:03Z" {
			t.Errorf("top-level fields must come from last response, got id=%v status=%v updated=%v", combined["id"], combined["status"], combined["updated"])
		}
		// keepFirst preserves turn-start fields from hop 1.
		if combined["previous_interaction_id"] != "parent-0" || combined["created"] != "2026-11-01T00:00:00Z" || combined["input"] != "Write a long report" {
			t.Errorf("keepFirst fields must come from hop 1, got previous_interaction_id=%v created=%v input=%v", combined["previous_interaction_id"], combined["created"], combined["input"])
		}
		// collect gathers all hop IDs in order.
		if !reflect.DeepEqual(combined["continuation_ids"], []any{"int-1", "int-2", "int-3"}) {
			t.Errorf("continuation_ids = %#v, want [int-1 int-2 int-3]", combined["continuation_ids"])
		}
		if _, hasToken := combined["continuation_token"]; hasToken {
			t.Errorf("last response had no continuation_token, but combined has %v", combined["continuation_token"])
		}

		// Steps must keep hop 1's user_input and drop repeated leading user_input
		// steps on continuation hops 2-3.
		steps, ok := combined["steps"].([]any)
		if !ok || len(steps) != 4 {
			t.Fatalf("combined steps = %#v, want 4 steps (1 user_input + 3 model_output)", combined["steps"])
		}
		var stepTypes, modelTexts []string
		for _, s := range steps {
			sm, _ := s.(map[string]any)
			typ, _ := sm["type"].(string)
			stepTypes = append(stepTypes, typ)
			if typ == "model_output" {
				content, _ := sm["content"].([]any)
				cm, _ := content[0].(map[string]any)
				text, _ := cm["text"].(string)
				modelTexts = append(modelTexts, text)
			}
		}
		if !reflect.DeepEqual(stepTypes, []string{"user_input", "model_output", "model_output", "model_output"}) {
			t.Errorf("step types = %#v, want [user_input model_output model_output model_output]", stepTypes)
		}
		if !reflect.DeepEqual(modelTexts, []string{"Alpha ", "Beta ", "Gamma"}) {
			t.Errorf("concatenated model step texts = %#v, want [Alpha , Beta , Gamma]", modelTexts)
		}

		// Per-hop usage counters are summed; initial_input_tokens falls back to
		// hop 1's total_input_tokens (100) and is NOT summed.
		usage, ok := combined["usage"].(map[string]any)
		if !ok {
			t.Fatalf("combined usage missing: %#v", combined)
		}
		wantUsage := map[string]float64{
			"initial_input_tokens":  100, // fallback from hop 1 total_input_tokens
			"total_input_tokens":    460, // 100 + 150 + 210
			"total_output_tokens":   180, // 50 + 60 + 70
			"total_thought_tokens":  45,  // 10 + 15 + 20
			"total_tool_use_tokens": 10,  // 2 + 3 + 5
			"total_cached_tokens":   75,  // 20 + 25 + 30
			"total_tokens":          695, // 162 + 228 + 305
		}
		for k, want := range wantUsage {
			if got, _ := usage[k].(float64); got != want {
				t.Errorf("usage.%s = %v, want %v", k, usage[k], want)
			}
		}

		// sumBy aggregates modality token breakdowns and grounding tool counts by key.
		wantInputMod := []any{
			map[string]any{"modality": "text", "tokens": float64(450)},
			map[string]any{"modality": "image", "tokens": float64(10)},
		}
		if !reflect.DeepEqual(usage["input_tokens_by_modality"], wantInputMod) {
			t.Errorf("usage.input_tokens_by_modality = %#v, want %#v", usage["input_tokens_by_modality"], wantInputMod)
		}
		wantOutputMod := []any{map[string]any{"modality": "text", "tokens": float64(180)}}
		if !reflect.DeepEqual(usage["output_tokens_by_modality"], wantOutputMod) {
			t.Errorf("usage.output_tokens_by_modality = %#v, want %#v", usage["output_tokens_by_modality"], wantOutputMod)
		}
		wantCachedMod := []any{map[string]any{"modality": "text", "tokens": float64(75)}}
		if !reflect.DeepEqual(usage["cached_tokens_by_modality"], wantCachedMod) {
			t.Errorf("usage.cached_tokens_by_modality = %#v, want %#v", usage["cached_tokens_by_modality"], wantCachedMod)
		}
		wantToolMod := []any{map[string]any{"modality": "text", "tokens": float64(10)}}
		if !reflect.DeepEqual(usage["tool_use_tokens_by_modality"], wantToolMod) {
			t.Errorf("usage.tool_use_tokens_by_modality = %#v, want %#v", usage["tool_use_tokens_by_modality"], wantToolMod)
		}
		wantGrounding := []any{
			map[string]any{"type": "google_search", "count": float64(4)},
			map[string]any{"type": "url_context", "count": float64(1)},
		}
		if !reflect.DeepEqual(usage["grounding_tool_count"], wantGrounding) {
			t.Errorf("usage.grounding_tool_count = %#v, want %#v", usage["grounding_tool_count"], wantGrounding)
		}
	}

	t.Run("generate intent with generation_config and system", func(t *testing.T) {
		runThreeHopJSON(t, []string{
			"generate", "Write a long report",
			"--model", "gemini-3.8-flash",
			"--system", "Be thorough",
			"--previous-interaction-id", "parent-0",
			"--stream=false",
			"--body", `{"generation_config":{"max_output_tokens":4096,"thinking_level":"high","thinking_summaries":"auto"}}`,
		})
	})

	t.Run("agent run model route", func(t *testing.T) {
		runThreeHopJSON(t, []string{
			"agent", "run", "Write a long report",
			"--model", "gemini-3.8-flash",
			"--previous-interaction-id", "parent-0",
			"--stream=false",
			"--body", `{"system_instruction":"Be thorough","generation_config":{"max_output_tokens":4096,"thinking_level":"high","thinking_summaries":"auto"}}`,
		})
	})

	t.Run("agent run agent route skips client-side continuation via unlessRequest", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"int-agent-1","status":"requires_continuation","continuation_token":"ct-agent-1","steps":[{"type":"model_output","content":[{"type":"text","text":"Managed"}]}]}`)
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL),
			"agent", "run", "Write a long report",
			"--agent", "deep-research-preview-04-2026",
			"--stream=false")...)
		if res.err != nil {
			t.Fatalf("agent route failed: %v\nstderr: %s", res.err, res.stderr)
		}
		if len(srv.recorded()) != 1 {
			t.Fatalf("agent route sent %d requests, want 1 (client-side continuation skipped)", len(srv.recorded()))
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
			t.Fatalf("unmarshal stdout: %v\nstdout: %s", err, res.stdout)
		}
		if out["id"] != "int-agent-1" || out["status"] != "requires_continuation" || out["continuation_token"] != "ct-agent-1" {
			t.Errorf("agent route response = %#v, want verbatim server response", out)
		}
		if _, hasIDs := out["continuation_ids"]; hasIDs {
			t.Errorf("skipped agent route must not add continuation_ids, got %#v", out["continuation_ids"])
		}
	})

	t.Run("human mode prints continuation count on stderr while jq and agent-mode keep stderr silent", func(t *testing.T) {
		newTwoHopServer := func(t *testing.T) *continuationTestServer {
			return newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if hop == 1 {
					fmt.Fprint(w, `{"id":"int-1","status":"requires_continuation","continuation_token":"ct-1","steps":[{"type":"model_output","content":[{"type":"text","text":"First"}]}],"usage":{"total_tokens":10}}`)
					return
				}
				fmt.Fprint(w, `{"id":"int-2","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"Second"}]}],"usage":{"total_tokens":15}}`)
			})
		}

		srvHuman := newTwoHopServer(t)
		resHuman := runCLI(t, t.TempDir(), nil, contHumanArgs(srvHuman.URL, "generate", "hi", "--stream=false")...)
		if resHuman.err != nil {
			t.Fatalf("human mode failed: %v\nstderr: %s", resHuman.err, resHuman.stderr)
		}
		if !strings.Contains(resHuman.stderr, "Continued across 2 responses.") {
			t.Errorf("human stderr = %q, want 'Continued across 2 responses.'", resHuman.stderr)
		}

		srvJQ := newTwoHopServer(t)
		resJQ := runCLI(t, t.TempDir(), nil, contHumanArgs(srvJQ.URL, "generate", "hi", "--stream=false", "--jq", `[.steps[].content[].text, .usage.total_tokens]`)...)
		if resJQ.err != nil {
			t.Fatalf("jq mode failed: %v\nstderr: %s", resJQ.err, resJQ.stderr)
		}
		if resJQ.stderr != "" {
			t.Errorf("jq stderr must be silent, got %q", resJQ.stderr)
		}
		var jqOut []any
		if err := json.Unmarshal([]byte(resJQ.stdout), &jqOut); err != nil {
			t.Fatalf("unmarshal jq stdout: %v\nstdout: %s", err, resJQ.stdout)
		}
		if !reflect.DeepEqual(jqOut, []any{"First", "Second", float64(25)}) {
			t.Errorf("jq stdout = %#v, want [First Second 25]", jqOut)
		}

		srvAgent := newTwoHopServer(t)
		resAgent := runCLI(t, t.TempDir(), nil, contHumanArgs(srvAgent.URL, "--agent-mode", "generate", "hi", "--stream=false")...)
		if resAgent.err != nil {
			t.Fatalf("agent-mode failed: %v\nstderr: %s", resAgent.err, resAgent.stderr)
		}
		if resAgent.stderr != "" {
			t.Errorf("agent-mode stderr must be silent on success, got %q", resAgent.stderr)
		}
		if !strings.Contains(resAgent.stdout, "steps[2]:") || !strings.Contains(resAgent.stdout, "total_tokens: 25") {
			t.Errorf("agent-mode TOON stdout = %q, want merged steps[2] and summed total_tokens: 25", resAgent.stdout)
		}
	})
}

func TestContinuationMultiHopStreaming(t *testing.T) {
	newThreeHopSSEServer := func(t *testing.T) *continuationTestServer {
		return newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			switch hop {
			case 1:
				// Hop 1: id arrives on interaction.created, text on step.delta,
				// and status + continuation_token + interaction_id arrive on interaction.status_update ($.data.*).
				fmt.Fprint(w, "data: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"int-s1\",\"status\":\"in_progress\"}}\n\n")
				fmt.Fprint(w, "data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text\",\"text\":\"Hello \"},\"index\":0}\n\n")
				fmt.Fprint(w, "data: {\"event_type\":\"interaction.status_update\",\"interaction_id\":\"int-s1\",\"status\":\"requires_continuation\",\"continuation_token\":\"ct-s1\"}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			case 2:
				// Hop 2: token, id, and status arrive across separate events and on interaction.completed ($.data.interaction.*).
				fmt.Fprint(w, "data: {\"event_type\":\"interaction.status_update\",\"interaction_id\":\"int-s2\",\"status\":\"in_progress\"}\n\n")
				fmt.Fprint(w, "data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text\",\"text\":\"streaming \"},\"index\":0}\n\n")
				fmt.Fprint(w, "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"int-s2\",\"status\":\"requires_continuation\",\"continuation_token\":\"ct-s2\"}}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			case 3:
				fmt.Fprint(w, "data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text\",\"text\":\"world!\"},\"index\":0}\n\n")
				fmt.Fprint(w, "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"int-s3\",\"status\":\"completed\"}}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			default:
				t.Errorf("unexpected SSE hop %d", hop)
			}
			if flusher != nil {
				flusher.Flush()
			}
		})
	}

	t.Run("human stream projection across hops", func(t *testing.T) {
		srv := newThreeHopSSEServer(t)
		res := runCLI(t, t.TempDir(), nil, contHumanArgs(srv.URL, "generate", "Tell a story")...)
		if res.err != nil {
			t.Fatalf("streaming generate failed: %v\nstderr: %s", res.err, res.stderr)
		}
		if res.stdout != "Hello streaming world!\n" {
			t.Errorf("stdout = %q, want %q", res.stdout, "Hello streaming world!\n")
		}
		if !strings.Contains(res.stderr, `Interaction ID: "int-s3"`) {
			t.Errorf("stderr = %q, want final metadata 'Interaction ID: \"int-s3\"'", res.stderr)
		}
		if !strings.Contains(res.stderr, "Continued across 3 responses.") {
			t.Errorf("stderr = %q, want 'Continued across 3 responses.'", res.stderr)
		}

		hops := srv.recorded()
		if len(hops) != 3 {
			t.Fatalf("recorded %d hops, want 3", len(hops))
		}
		if hops[0].Body["continuation_token"] != nil || hops[0].Body["previous_interaction_id"] != nil {
			t.Errorf("hop 1 unexpected resume fields: %#v", hops[0].Body)
		}
		if hops[1].Body["continuation_token"] != "ct-s1" || hops[1].Body["previous_interaction_id"] != "int-s1" {
			t.Errorf("hop 2 body = %#v, want ct-s1 and int-s1", hops[1].Body)
		}
		if hops[2].Body["continuation_token"] != "ct-s2" || hops[2].Body["previous_interaction_id"] != "int-s2" {
			t.Errorf("hop 3 body = %#v, want ct-s2 and int-s2", hops[2].Body)
		}
	})

	t.Run("NDJSON machine stream across hops", func(t *testing.T) {
		srv := newThreeHopSSEServer(t)
		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), "agent", "run", "Tell a story")...)
		if res.err != nil {
			t.Fatalf("agent run -o json failed: %v\nstderr: %s", res.err, res.stderr)
		}
		if res.stderr != "" {
			t.Errorf("stderr must be empty in -o json mode, got %q", res.stderr)
		}
		var nonEmpty []string
		for _, txt := range deltaTexts(t, res.stdout) {
			if txt != "" {
				nonEmpty = append(nonEmpty, txt)
			}
		}
		if !reflect.DeepEqual(nonEmpty, []string{"Hello ", "streaming ", "world!"}) {
			t.Errorf("NDJSON delta texts = %#v, want [Hello , streaming , world!]", nonEmpty)
		}
	})
}

func TestContinuationStoredAndStatelessModes(t *testing.T) {
	t.Run("stateless --store=false without parent id never sends previous_interaction_id", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if hop == 1 {
				fmt.Fprint(w, `{"id":"int-stateless-1","status":"requires_continuation","continuation_token":"ct-stateless-1","steps":[{"type":"model_output","content":[{"type":"text","text":"One "}]}]}`)
				return
			}
			fmt.Fprint(w, `{"id":"int-stateless-2","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"Two"}]}]}`)
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), "generate", "hi", "--store=false", "--stream=false")...)
		if res.err != nil {
			t.Fatalf("stateless generate failed: %v\nstderr: %s", res.err, res.stderr)
		}
		hops := srv.recorded()
		if len(hops) != 2 {
			t.Fatalf("recorded %d hops, want 2", len(hops))
		}
		for i, hop := range hops {
			if hop.Body["store"] != false {
				t.Errorf("hop %d store = %#v, want false", i+1, hop.Body["store"])
			}
			if hop.Body["input"] != "hi" {
				t.Errorf("hop %d in stateless mode must resend input, got %#v", i+1, hop.Body["input"])
			}
			if _, hasPrev := hop.Body["previous_interaction_id"]; hasPrev {
				t.Errorf("hop %d in stateless mode must not send previous_interaction_id, got %#v", i+1, hop.Body["previous_interaction_id"])
			}
		}
		if hops[1].Body["continuation_token"] != "ct-stateless-1" {
			t.Errorf("hop 2 continuation_token = %#v, want ct-stateless-1", hops[1].Body["continuation_token"])
		}
	})

	t.Run("stateless --store=false with initial previous_interaction_id preserves parent id across hops and omits it from resume hints", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if hop == 1 {
				fmt.Fprint(w, `{"id":"ephemeral-hop-1","status":"requires_continuation","continuation_token":"ct-ephemeral-1","steps":[{"type":"model_output","content":[{"type":"text","text":"One "}]}]}`)
				return
			}
			fmt.Fprint(w, `{"id":"ephemeral-hop-2","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"Two"}]}]}`)
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL),
			"generate", "hi", "--store=false", "--previous-interaction-id", "stored-parent-0", "--stream=false")...)
		if res.err != nil {
			t.Fatalf("stateless generate with parent id failed: %v\nstderr: %s", res.err, res.stderr)
		}
		hops := srv.recorded()
		if len(hops) != 2 {
			t.Fatalf("recorded %d hops, want 2", len(hops))
		}
		for i, hop := range hops {
			if got := hop.Body["previous_interaction_id"]; got != "stored-parent-0" {
				t.Errorf("hop %d previous_interaction_id = %#v, want preserved 'stored-parent-0' (not ephemeral hop id)", i+1, got)
			}
		}

		// With --auto-continue=false in stateless mode, the resume hint must NOT
		// suggest --previous-interaction-id "ephemeral-hop-1".
		srvHint := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"ephemeral-hop-1","status":"requires_continuation","continuation_token":"ct-ephemeral-1","steps":[]}`)
		})
		resHint := runCLI(t, t.TempDir(), nil, contHumanArgs(srvHint.URL,
			"generate", "hi", "--store=false", "--previous-interaction-id", "stored-parent-0", "--stream=false", "--auto-continue=false")...)
		if resHint.err != nil {
			t.Fatalf("stateless --auto-continue=false failed: %v\nstderr: %s", resHint.err, resHint.stderr)
		}
		if !strings.Contains(resHint.stderr, `--continuation-token "ct-ephemeral-1"`) {
			t.Errorf("stderr = %q, want --continuation-token hint", resHint.stderr)
		}
		if strings.Contains(resHint.stderr, "previous-interaction-id") || strings.Contains(resHint.stderr, "ephemeral-hop-1") {
			t.Errorf("stateless resume hint must not include previous-interaction-id, got: %q", resHint.stderr)
		}
	})

	t.Run("stateless streaming with store:false in --body never binds previous_interaction_id", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if hop == 1 {
				fmt.Fprint(w, "data: {\"event_type\":\"interaction.status_update\",\"interaction_id\":\"ephemeral-s1\",\"status\":\"requires_continuation\",\"continuation_token\":\"ct-s1\"}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text\",\"text\":\"Done\"},\"index\":0}\n\n")
			fmt.Fprint(w, "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"ephemeral-s2\",\"status\":\"completed\"}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		})

		res := runCLI(t, t.TempDir(), nil, contHumanArgs(srv.URL,
			"agent", "run", "--body", `{"model":"gemini-3.8-flash","input":"hi","store":false}`)...)
		if res.err != nil {
			t.Fatalf("stateless streaming agent run failed: %v\nstderr: %s", res.err, res.stderr)
		}
		hops := srv.recorded()
		if len(hops) != 2 {
			t.Fatalf("recorded %d hops, want 2", len(hops))
		}
		if _, hasPrev := hops[1].Body["previous_interaction_id"]; hasPrev {
			t.Errorf("hop 2 in stateless streaming must not send previous_interaction_id, got %#v", hops[1].Body)
		}
		if hops[1].Body["continuation_token"] != "ct-s1" {
			t.Errorf("hop 2 continuation_token = %#v, want ct-s1", hops[1].Body["continuation_token"])
		}
	})
}

func TestContinuationControlsAndLimits(t *testing.T) {
	t.Run("--auto-continue=false stops after one hop and prints manual resume guidance in human mode only", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"int-manual-1","status":"requires_continuation","continuation_token":"ct-manual-1","steps":[{"type":"model_output","content":[{"type":"text","text":"Partial"}]}]}`)
		})

		resHuman := runCLI(t, t.TempDir(), nil, contHumanArgs(srv.URL, "generate", "hi", "--stream=false", "--auto-continue=false")...)
		if resHuman.err != nil {
			t.Fatalf("--auto-continue=false failed: %v\nstderr: %s", resHuman.err, resHuman.stderr)
		}
		if len(srv.recorded()) != 1 {
			t.Fatalf("recorded %d hops, want 1", len(srv.recorded()))
		}
		wantHint := `re-run the same command with --continuation-token "ct-manual-1" --previous-interaction-id "int-manual-1", leaving $.input out of the request body`
		if !strings.Contains(resHuman.stderr, wantHint) {
			t.Errorf("human stderr = %q, want %q", resHuman.stderr, wantHint)
		}

		// Machine mode (-o json) must keep stderr silent.
		srvJSON := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"int-manual-1","status":"requires_continuation","continuation_token":"ct-manual-1","steps":[]}`)
		})
		resJSON := runCLI(t, t.TempDir(), nil, append(baseArgs(srvJSON.URL), "generate", "hi", "--stream=false", "--auto-continue=false")...)
		if resJSON.err != nil {
			t.Fatalf("-o json --auto-continue=false failed: %v\nstderr: %s", resJSON.err, resJSON.stderr)
		}
		if resJSON.stderr != "" {
			t.Errorf("-o json stderr = %q, want empty", resJSON.stderr)
		}
	})

	t.Run("manual stored-mode resume omits input on generate and agent run across flags and --body", func(t *testing.T) {
		cases := []struct {
			name string
			args []string
		}{
			{
				name: "generate flags without prompt",
				args: []string{"generate", "--continuation-token", "ct-resume-99", "--previous-interaction-id", "int-prev-99", "--stream=false"},
			},
			{
				name: "generate --body without input",
				args: []string{"generate", "--body", `{"continuation_token":"ct-resume-99","previous_interaction_id":"int-prev-99","stream":false}`},
			},
			{
				name: "agent run flags without input",
				args: []string{"agent", "run", "--continuation-token", "ct-resume-99", "--previous-interaction-id", "int-prev-99", "--stream=false"},
			},
			{
				name: "agent run --body without input",
				args: []string{"agent", "run", "--body", `{"model":"gemini-3.8-flash","continuation_token":"ct-resume-99","previous_interaction_id":"int-prev-99","stream":false}`},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if hop == 1 {
						fmt.Fprint(w, `{"id":"int-resumed-1","status":"requires_continuation","continuation_token":"ct-resume-100","steps":[{"type":"model_output","content":[{"type":"text","text":"Part 2"}]}],"usage":{"total_tokens":5}}`)
						return
					}
					fmt.Fprint(w, `{"id":"int-resumed-2","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"Part 3"}]}],"usage":{"total_tokens":7}}`)
				})

				res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), tc.args...)...)
				if res.err != nil {
					t.Fatalf("manual resume failed: %v\nstderr: %s", res.err, res.stderr)
				}
				hops := srv.recorded()
				if len(hops) != 2 {
					t.Fatalf("recorded %d hops, want 2 (resumed hop + follow-up continuation)", len(hops))
				}
				if hops[0].Body["continuation_token"] != "ct-resume-99" || hops[0].Body["previous_interaction_id"] != "int-prev-99" {
					t.Errorf("hop 1 body = %#v, want ct-resume-99 and int-prev-99", hops[0].Body)
				}
				if _, hasInput := hops[0].Body["input"]; hasInput {
					t.Errorf("hop 1 stored manual resume must omit input, got %#v", hops[0].Body["input"])
				}
				if hops[1].Body["continuation_token"] != "ct-resume-100" || hops[1].Body["previous_interaction_id"] != "int-resumed-1" {
					t.Errorf("hop 2 body = %#v, want ct-resume-100 and int-resumed-1", hops[1].Body)
				}
				if _, hasInput := hops[1].Body["input"]; hasInput {
					t.Errorf("hop 2 stored continuation must omit input, got %#v", hops[1].Body["input"])
				}
			})
		}

		t.Run("generate piped stdin JSON resume without input", func(t *testing.T) {
			srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"int-stdin-resumed","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"Done"}]}]}`)
			})
			res := runCLIWithStdin(t, t.TempDir(), nil,
				`{"continuation_token":"ct-stdin-1","previous_interaction_id":"int-stdin-0","stream":false}`,
				append(baseArgs(srv.URL), "generate")...)
			if res.err != nil {
				t.Fatalf("stdin manual resume failed: %v\nstderr: %s", res.err, res.stderr)
			}
			hops := srv.recorded()
			if len(hops) != 1 {
				t.Fatalf("recorded %d hops, want 1", len(hops))
			}
			if _, hasInput := hops[0].Body["input"]; hasInput {
				t.Errorf("stdin stored manual resume must omit input, got %#v", hops[0].Body["input"])
			}
			if hops[0].Body["continuation_token"] != "ct-stdin-1" || hops[0].Body["previous_interaction_id"] != "int-stdin-0" {
				t.Errorf("stdin resume body = %#v, want ct-stdin-1 and int-stdin-0", hops[0].Body)
			}
		})

		t.Run("manual stateless resume with prompt re-sends input", func(t *testing.T) {
			srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"int-stateless-resumed","status":"completed","steps":[]}`)
			})
			res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL),
				"generate", "original prompt", "--continuation-token", "ct-s-1", "--store=false", "--stream=false")...)
			if res.err != nil {
				t.Fatalf("stateless manual resume with prompt failed: %v\nstderr: %s", res.err, res.stderr)
			}
			hops := srv.recorded()
			if len(hops) != 1 || hops[0].Body["input"] != "original prompt" || hops[0].Body["store"] != false {
				t.Errorf("stateless manual resume body = %#v, want input and store=false", hops)
			}
		})
	})

	t.Run("missing input still fails CLI_VALIDATION when not resuming a stored model continuation", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected HTTP request on hop %d: %#v", hop, req.Body)
		})

		badCases := []struct {
			name     string
			args     []string
			wantSub  string
			wantHelp bool
		}{
			{
				name:     "bare generate",
				args:     []string{"generate"},
				wantSub:  "missing required argument <prompt>",
				wantHelp: true,
			},
			{
				name:     "bare agent run",
				args:     []string{"agent", "run"},
				wantSub:  "missing required argument <input>",
				wantHelp: true,
			},
			{
				name:    "generate with --model only",
				args:    []string{"generate", "--model", "gemini-3.8-flash"},
				wantSub: "missing required argument <prompt>",
			},
			{
				name:    "generate with --body missing input and token",
				args:    []string{"generate", "--body", `{"model":"gemini-3.8-flash"}`},
				wantSub: `has no "input"`,
			},
			{
				name:    "generate with empty --continuation-token",
				args:    []string{"generate", "--continuation-token", ""},
				wantSub: "missing required argument <prompt>",
			},
			{
				name:    "generate stateless resume without prompt",
				args:    []string{"generate", "--continuation-token", "ct-1", "--store=false"},
				wantSub: "stateless continuation (store=false) requires <prompt>",
			},
			{
				name:    "agent run with --model only",
				args:    []string{"agent", "run", "--model", "gemini-3.8-flash"},
				wantSub: "missing required argument <input>",
			},
			{
				name:    "agent run with --body missing input and token",
				args:    []string{"agent", "run", "--body", `{"model":"gemini-3.8-flash"}`},
				wantSub: `has no "input"`,
			},
			{
				name:    "agent run stateless --body resume without input",
				args:    []string{"agent", "run", "--body", `{"model":"gemini-3.8-flash","continuation_token":"ct-1","store":false}`},
				wantSub: "stateless continuation (store=false) requires <input>",
			},
			{
				name:    "agent run --agent without input",
				args:    []string{"agent", "run", "--agent", "deep-research-preview-04-2026"},
				wantSub: "missing required argument <input>",
			},
			{
				name:    "agent run --agent with --continuation-token without input",
				args:    []string{"agent", "run", "--agent", "deep-research-preview-04-2026", "--continuation-token", "ct-1"},
				wantSub: "missing required argument <input>",
			},
			{
				name:    "agent run --background with --continuation-token without input",
				args:    []string{"agent", "run", "--background", "--continuation-token", "ct-1"},
				wantSub: "missing required argument <input>",
			},
		}
		for _, tc := range badCases {
			t.Run(tc.name, func(t *testing.T) {
				var cmdArgs []string
				if tc.wantHelp {
					cmdArgs = contHumanArgs(srv.URL, tc.args...)
				} else {
					cmdArgs = append(baseArgs(srv.URL), tc.args...)
				}
				res := runCLI(t, t.TempDir(), nil, cmdArgs...)
				if code := exitCode(t, res); code != 2 {
					t.Fatalf("exit code = %d, want 2\nstderr: %s", code, res.stderr)
				}
				if tc.wantHelp {
					if !strings.Contains(res.stderr, tc.wantSub) {
						t.Errorf("stderr = %q, want substring %q", res.stderr, tc.wantSub)
					}
					if !strings.Contains(res.stderr, "Usage:") {
						t.Errorf("bare invocation stderr missing Usage help:\n%s", res.stderr)
					}
					return
				}
				var env struct {
					Error       string `json:"error"`
					ErrorReason string `json:"error_reason"`
				}
				if err := json.Unmarshal([]byte(res.stderr), &env); err != nil {
					t.Fatalf("unmarshal error JSON: %v\nstderr: %s", err, res.stderr)
				}
				if env.ErrorReason != "CLI_VALIDATION" {
					t.Errorf("error_reason = %q, want CLI_VALIDATION (stderr: %s)", env.ErrorReason, res.stderr)
				}
				if !strings.Contains(env.Error, tc.wantSub) {
					t.Errorf("error = %q, want substring %q", env.Error, tc.wantSub)
				}
			})
		}
		if len(srv.recorded()) != 0 {
			t.Errorf("recorded %d requests, want 0", len(srv.recorded()))
		}
	})

	t.Run("--max-continuations counts total responses including initial response and fails with CLI_CONTINUATION_LIMIT", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"int-%d","status":"requires_continuation","continuation_token":"ct-%d","steps":[]}`, hop, hop)
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL),
			"generate", "hi", "--stream=false", "--max-continuations", "2")...)
		if code := exitCode(t, res); code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr: %s", code, res.stderr)
		}
		if len(srv.recorded()) != 2 {
			t.Fatalf("recorded %d hops, want 2 (initial + 1 continuation)", len(srv.recorded()))
		}

		var errEnv struct {
			ErrorType    string            `json:"error_type"`
			ErrorReason  string            `json:"error_reason"`
			Hops         int               `json:"hops"`
			Continuation map[string]string `json:"continuation"`
			Hints        []string          `json:"hints"`
		}
		if err := json.Unmarshal([]byte(res.stderr), &errEnv); err != nil {
			t.Fatalf("unmarshal error JSON: %v\nstderr: %s", err, res.stderr)
		}
		if errEnv.ErrorReason != "CLI_CONTINUATION_LIMIT" || errEnv.Hops != 2 {
			t.Errorf("error = %+v, want error_reason=CLI_CONTINUATION_LIMIT hops=2", errEnv)
		}
		wantCont := map[string]string{
			"$.continuation_token":      "ct-2",
			"$.previous_interaction_id": "int-2",
		}
		if !reflect.DeepEqual(errEnv.Continuation, wantCont) {
			t.Errorf("continuation = %#v, want %#v", errEnv.Continuation, wantCont)
		}
		joinedHints := strings.Join(errEnv.Hints, " | ")
		if !strings.Contains(joinedHints, `--continuation-token "ct-2" --previous-interaction-id "int-2"`) {
			t.Errorf("hints = %q, want flag resume guidance", joinedHints)
		}
	})

	t.Run("--max-continuations <= 0 fails locally with CLI_VALIDATION exit 2 before sending a request", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected HTTP request on hop %d", hop)
		})

		for _, bad := range []string{"0", "-2"} {
			res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL),
				"generate", "hi", "--max-continuations", bad)...)
			if code := exitCode(t, res); code != 2 {
				t.Errorf("--max-continuations %s exit code = %d, want 2\nstderr: %s", bad, code, res.stderr)
			}
			if !strings.Contains(res.stderr, "CLI_VALIDATION") {
				t.Errorf("--max-continuations %s stderr = %q, want CLI_VALIDATION", bad, res.stderr)
			}
		}
		if len(srv.recorded()) != 0 {
			t.Errorf("recorded %d requests, want 0", len(srv.recorded()))
		}
	})

	t.Run("missing required continuation_token when status is requires_continuation fails with CLI_PROTOCOL exit 1", func(t *testing.T) {
		srvJSON := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"int-bad","status":"requires_continuation","steps":[]}`)
		})
		resJSON := runCLI(t, t.TempDir(), nil, append(baseArgs(srvJSON.URL), "generate", "hi", "--stream=false")...)
		if code := exitCode(t, resJSON); code != 1 {
			t.Fatalf("missing token JSON exit code = %d, want 1\nstderr: %s", code, resJSON.stderr)
		}
		if !strings.Contains(resJSON.stderr, `"error_reason": "CLI_PROTOCOL"`) {
			t.Errorf("missing token JSON stderr = %s, want CLI_PROTOCOL", resJSON.stderr)
		}

		srvSSE := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"event_type\":\"interaction.status_update\",\"interaction_id\":\"int-bad-sse\",\"status\":\"requires_continuation\"}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		})
		resSSE := runCLI(t, t.TempDir(), nil, append(baseArgs(srvSSE.URL), "generate", "hi")...)
		if code := exitCode(t, resSSE); code != 1 {
			t.Fatalf("missing token SSE exit code = %d, want 1\nstderr: %s", code, resSSE.stderr)
		}
		if !strings.Contains(resSSE.stderr, `"error_reason": "CLI_PROTOCOL"`) {
			t.Errorf("missing token SSE stderr = %s, want CLI_PROTOCOL", resSSE.stderr)
		}
	})
}

func TestContinuationStatusClassificationAndTokenGuard(t *testing.T) {
	// Token presence alone must NEVER trigger another request or emit a resume hint.
	for _, status := range []string{"completed", "incomplete", "budget_exceeded", "requires_action"} {
		t.Run("non-continue status "+status+" stops after one hop even with continuation_token", func(t *testing.T) {
			srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"int-term","status":%q,"continuation_token":"ct-should-not-use","steps":[{"type":"model_output","content":[{"type":"text","text":"Done"}]}]}`, status)
			})

			res := runCLI(t, t.TempDir(), nil, contHumanArgs(srv.URL,
				"generate", "hi", "--stream=false",
				"--body", `{"generation_config":{"max_output_tokens":256}}`)...)
			if res.err != nil {
				t.Fatalf("status %s failed: %v\nstderr: %s", status, res.err, res.stderr)
			}
			if len(srv.recorded()) != 1 {
				t.Fatalf("status %s made %d hops, want 1", status, len(srv.recorded()))
			}
			if strings.Contains(res.stderr, "ct-should-not-use") || strings.Contains(res.stderr, "--continuation-token") {
				t.Errorf("status %s unexpectedly emitted continuation hint on stderr: %q", status, res.stderr)
			}
		})
	}

	// Failed and cancelled responses must fail with CLI_OUTCOME_FAILED (exit 1)
	// and never auto-resume, even when a continuation_token is present.
	for _, status := range []string{"failed", "cancelled"} {
		t.Run("failure status "+status+" on hop 1 exits 1 without auto-resuming", func(t *testing.T) {
			srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"int-fail-1","status":%q,"continuation_token":"ct-do-not-resume","errors":[{"message":"interaction %s"}]}`, status, status)
			})

			res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), "generate", "hi", "--stream=false")...)
			if code := exitCode(t, res); code != 1 {
				t.Fatalf("status %s exit code = %d, want 1\nstderr: %s", status, code, res.stderr)
			}
			if len(srv.recorded()) != 1 {
				t.Fatalf("status %s made %d hops, want 1", status, len(srv.recorded()))
			}
			if !strings.Contains(res.stderr, `"error_reason": "CLI_OUTCOME_FAILED"`) {
				t.Errorf("status %s stderr = %s, want CLI_OUTCOME_FAILED", status, res.stderr)
			}
			if strings.Contains(res.stderr, "ct-do-not-resume") {
				t.Errorf("status %s on hop 1 must not offer a continuation checkpoint, got: %s", status, res.stderr)
			}
		})
	}
}

func TestContinuationMidFlightFailuresCarryResumeContext(t *testing.T) {
	t.Run("dropped stream on hop 1 after checkpoint token fails with resume context and never replays", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			// Emit a checkpoint token and partial text, then close the stream before [DONE]
			// without reporting a terminal state.
			fmt.Fprint(w, "data: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"int-drop-1\",\"status\":\"in_progress\"}}\n\n")
			fmt.Fprint(w, "data: {\"event_type\":\"interaction.status_update\",\"interaction_id\":\"int-drop-1\",\"status\":\"in_progress\",\"continuation_token\":\"ct-drop-1\"}\n\n")
			fmt.Fprint(w, "data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text\",\"text\":\"partial\"},\"index\":0}\n\n")
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), "generate", "hi")...)
		if code := exitCode(t, res); code != 1 {
			t.Fatalf("dropped stream exit code = %d, want 1\nstderr: %s", code, res.stderr)
		}
		if len(srv.recorded()) != 1 {
			t.Fatalf("dropped stream made %d hops, want 1 (no silent replay)", len(srv.recorded()))
		}
		var errEnv struct {
			ErrorReason  string            `json:"error_reason"`
			Hops         int               `json:"hops"`
			Continuation map[string]string `json:"continuation"`
			Hints        []string          `json:"hints"`
		}
		if err := json.Unmarshal([]byte(res.stderr), &errEnv); err != nil {
			t.Fatalf("unmarshal error JSON: %v\nstderr: %s", err, res.stderr)
		}
		if errEnv.ErrorReason != "CLI_CONNECTION" || errEnv.Hops != 1 {
			t.Errorf("error = %+v, want error_reason=CLI_CONNECTION hops=1", errEnv)
		}
		if errEnv.Continuation["$.continuation_token"] != "ct-drop-1" || errEnv.Continuation["$.previous_interaction_id"] != "int-drop-1" {
			t.Errorf("continuation context = %#v, want ct-drop-1 and int-drop-1", errEnv.Continuation)
		}
		joinedHints := strings.Join(errEnv.Hints, " | ")
		if !strings.Contains(joinedHints, `--continuation-token "ct-drop-1" --previous-interaction-id "int-drop-1"`) {
			t.Errorf("hints = %q, want flag resume guidance", joinedHints)
		}
	})

	t.Run("HTTP 500 on hop 2 reports hop 1 resume context", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if hop == 1 {
				fmt.Fprint(w, `{"id":"int-ok-1","status":"requires_continuation","continuation_token":"ct-ok-1","steps":[{"type":"model_output","content":[{"type":"text","text":"Hop 1"}]}]}`)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"backend unavailable"}}`)
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), "generate", "hi", "--stream=false")...)
		if code := exitCode(t, res); code != 1 {
			t.Fatalf("HTTP 500 on hop 2 exit code = %d, want 1\nstderr: %s", code, res.stderr)
		}
		if len(srv.recorded()) != 2 {
			t.Fatalf("recorded %d hops, want 2", len(srv.recorded()))
		}
		var errEnv struct {
			ErrorType    string            `json:"error_type"`
			StatusCode   int               `json:"status_code"`
			Hops         int               `json:"hops"`
			Continuation map[string]string `json:"continuation"`
			Hints        []string          `json:"hints"`
		}
		if err := json.Unmarshal([]byte(res.stderr), &errEnv); err != nil {
			t.Fatalf("unmarshal error JSON: %v\nstderr: %s", err, res.stderr)
		}
		if errEnv.ErrorType != "server_error" || errEnv.StatusCode != 500 || errEnv.Hops != 1 {
			t.Errorf("error = %+v, want error_type=server_error status_code=500 hops=1", errEnv)
		}
		if errEnv.Continuation["$.continuation_token"] != "ct-ok-1" || errEnv.Continuation["$.previous_interaction_id"] != "int-ok-1" {
			t.Errorf("continuation = %#v, want ct-ok-1 and int-ok-1", errEnv.Continuation)
		}
		joinedHints := strings.Join(errEnv.Hints, " | ")
		if !strings.Contains(joinedHints, `--continuation-token "ct-ok-1" --previous-interaction-id "int-ok-1"`) {
			t.Errorf("hints = %q, want flag resume guidance", joinedHints)
		}
	})

	t.Run("failed or cancelled status on hop 2 reports hop 1 resume context and does not overwrite with failed hop token", func(t *testing.T) {
		for _, status := range []string{"failed", "cancelled"} {
			srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if hop == 1 {
					fmt.Fprint(w, `{"id":"int-good-1","status":"requires_continuation","continuation_token":"ct-good-1","steps":[]}`)
					return
				}
				fmt.Fprintf(w, `{"id":"int-bad-2","status":%q,"continuation_token":"ct-bad-2","errors":[{"message":"hop 2 %s"}]}`, status, status)
			})

			res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), "generate", "hi", "--stream=false")...)
			if code := exitCode(t, res); code != 1 {
				t.Fatalf("hop 2 %s exit code = %d, want 1\nstderr: %s", status, code, res.stderr)
			}
			var errEnv struct {
				ErrorReason  string            `json:"error_reason"`
				Hops         int               `json:"hops"`
				Continuation map[string]string `json:"continuation"`
			}
			if err := json.Unmarshal([]byte(res.stderr), &errEnv); err != nil {
				t.Fatalf("unmarshal error JSON: %v\nstderr: %s", err, res.stderr)
			}
			if errEnv.ErrorReason != "CLI_OUTCOME_FAILED" || errEnv.Hops != 2 {
				t.Errorf("hop 2 %s error = %+v, want CLI_OUTCOME_FAILED hops=2", status, errEnv)
			}
			if errEnv.Continuation["$.continuation_token"] != "ct-good-1" || errEnv.Continuation["$.previous_interaction_id"] != "int-good-1" {
				t.Errorf("hop 2 %s continuation = %#v, want hop 1 checkpoint ct-good-1 / int-good-1", status, errEnv.Continuation)
			}
		}
	})

	t.Run("stalled repeated continuation_token stops without looping", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Hop 1 and Hop 2 return the exact same token; CLI must stop after hop 2.
			fmt.Fprintf(w, `{"id":"int-stall-%d","status":"requires_continuation","continuation_token":"ct-same","steps":[{"type":"model_output","content":[{"type":"text","text":"part %d"}]}]}`, hop, hop)
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), "generate", "hi", "--stream=false")...)
		if res.err != nil {
			t.Fatalf("stalled token failed unexpectedly: %v\nstderr: %s", res.err, res.stderr)
		}
		if len(srv.recorded()) != 2 {
			t.Fatalf("recorded %d hops, want 2 (stop on repeated token)", len(srv.recorded()))
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
			t.Fatalf("unmarshal stdout: %v\nstdout: %s", err, res.stdout)
		}
		if out["status"] != "requires_continuation" || out["continuation_token"] != "ct-same" {
			t.Errorf("stalled output = %#v, want requires_continuation with ct-same", out)
		}
	})

	t.Run("authoritative interaction.completed without token clears earlier stream checkpoint", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"int-auth-1\",\"status\":\"in_progress\"}}\n\n")
			fmt.Fprint(w, "data: {\"event_type\":\"interaction.status_update\",\"interaction_id\":\"int-auth-1\",\"status\":\"in_progress\",\"continuation_token\":\"ct-stale\"}\n\n")
			// Final interaction.completed snapshot reports requires_continuation WITHOUT a token;
			// streamAuthoritativeWhen must clear ct-stale so CLI fails with CLI_PROTOCOL instead of looping.
			fmt.Fprint(w, "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"int-auth-1\",\"status\":\"requires_continuation\"}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), "generate", "hi")...)
		if code := exitCode(t, res); code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr: %s", code, res.stderr)
		}
		if len(srv.recorded()) != 1 {
			t.Fatalf("recorded %d hops, want 1 (must not continue from cleared stale checkpoint)", len(srv.recorded()))
		}
		if !strings.Contains(res.stderr, `"error_reason": "CLI_PROTOCOL"`) {
			t.Errorf("stderr = %s, want CLI_PROTOCOL", res.stderr)
		}
	})

	t.Run("SSE error event on hop 2 after mid-hop checkpoint falls back to hop 1 completed resume context", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if hop == 1 {
				fmt.Fprint(w, "data: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"int-sse-1\",\"status\":\"in_progress\"}}\n\n")
				fmt.Fprint(w, "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"int-sse-1\",\"status\":\"requires_continuation\",\"continuation_token\":\"ct-sse-1\"}}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"int-sse-2\",\"status\":\"in_progress\"}}\n\n")
			fmt.Fprint(w, "data: {\"event_type\":\"interaction.status_update\",\"interaction_id\":\"int-sse-2\",\"status\":\"in_progress\",\"continuation_token\":\"ct-sse-2-aborted\"}\n\n")
			fmt.Fprint(w, "data: {\"event_type\":\"error\",\"error\":{\"code\":\"503\",\"message\":\"overloaded\"}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL), "generate", "hi")...)
		if code := exitCode(t, res); code != 1 {
			t.Fatalf("SSE error on hop 2 exit code = %d, want 1\nstderr: %s", code, res.stderr)
		}
		var errEnv struct {
			EventType    string            `json:"event_type"`
			Continuation map[string]string `json:"continuation"`
		}
		if err := json.Unmarshal([]byte(res.stderr), &errEnv); err != nil {
			t.Fatalf("unmarshal error JSON: %v\nstderr: %s", err, res.stderr)
		}
		if errEnv.EventType != "error" {
			t.Errorf("event_type = %q, want error", errEnv.EventType)
		}
		if errEnv.Continuation["$.continuation_token"] != "ct-sse-1" || errEnv.Continuation["$.previous_interaction_id"] != "int-sse-1" {
			t.Errorf("continuation = %#v, want hop 1 completed resume values (not aborted hop 2 checkpoint)", errEnv.Continuation)
		}
	})
}

func TestContinuationDryRunAndBackgroundIsolation(t *testing.T) {
	t.Run("dry-run previews one request without entering continuation loop", func(t *testing.T) {
		res := runCLI(t, t.TempDir(), nil, append(baseArgs("https://example.invalid"),
			"--dry-run", "generate", "hello", "--continuation-token", "ct-preview", "--store=false")...)
		if res.err != nil {
			t.Fatalf("dry-run failed: %v\nstderr: %s", res.err, res.stderr)
		}
		req := parseDryRunRequest(t, res.stdout)
		body, _ := req["body"].(map[string]any)
		if body["continuation_token"] != "ct-preview" || body["store"] != false {
			t.Errorf("dry-run preview body = %#v, want continuation_token=ct-preview and store=false", body)
		}
	})

	t.Run("agent run --background returns handle after one request and skips continuation even on requires_continuation", func(t *testing.T) {
		srv := newContinuationServer(t, func(hop int, req recordedHop, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"int-bg-1","status":"requires_continuation","continuation_token":"ct-bg-1"}`)
		})

		res := runCLI(t, t.TempDir(), nil, append(baseArgs(srv.URL),
			"agent", "run", "research solar", "--model", "gemini-3.8-flash", "--background")...)
		if res.err != nil {
			t.Fatalf("agent run --background failed: %v\nstderr: %s", res.err, res.stderr)
		}
		hops := srv.recorded()
		if len(hops) != 1 {
			t.Fatalf("recorded %d hops, want 1", len(hops))
		}
		if hops[0].Body["background"] != true || hops[0].Body["stream"] != false {
			t.Errorf("background request body = %#v, want background=true stream=false", hops[0].Body)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
			t.Fatalf("unmarshal stdout: %v\nstdout: %s", err, res.stdout)
		}
		if _, hasIDs := out["continuation_ids"]; hasIDs {
			t.Errorf("skipped background request must not add continuation_ids, got %#v", out["continuation_ids"])
		}
	})

	t.Run("async polling stops immediately on requires_continuation as handoff", func(t *testing.T) {
		pollRequiresContinuation := `{"id":"int-vid","object":"interaction","status":"requires_continuation","continuation_token":"ct-poll-1","steps":[]}`
		handoff := newAsyncVideoServer(t, asyncCreateInProgress, pollRequiresContinuation)
		dir := t.TempDir()
		result := runCLIInDir(t, t.TempDir(), dir, nil, videoArgs(handoff.URL, "--output-format", "json")...)
		if result.err != nil {
			t.Fatalf("requires_continuation handoff must exit 0: %v\nstderr: %s", result.err, result.stderr)
		}
		var payload struct {
			ID                string `json:"id"`
			Status            string `json:"status"`
			ContinuationToken string `json:"continuation_token"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil || payload.Status != "requires_continuation" || payload.ID != "int-vid" || payload.ContinuationToken != "ct-poll-1" {
			t.Errorf("handoff stdout = %q (err %v), want requires_continuation payload", result.stdout, err)
		}
		if seen := handoff.seen(); countPrefix(seen, "GET ") != 1 {
			t.Errorf("handoff must stop polling after first requires_continuation, requests: %v", seen)
		}
	})
}
