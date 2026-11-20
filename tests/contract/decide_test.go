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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/cli"
	"github.com/google-gemini/gemini-api-cli/internal/explorer"
)

const sampleNoulResponse = `{"answers":{"q":{"type":"noul","noul":0.96,"confidence":0.96}},"usage":{"total_input_tokens":268,"input_tokens_by_modality":[{"modality":"image","tokens":258},{"modality":"text","tokens":10}],"total_output_tokens":0,"total_tokens":268}}`

const sampleChoiceScoreResponse = `{"answers":{"route":{"type":"choice","choice":"technical","confidence":0.91,"probabilities":{"billing":0.03,"technical":0.91,"other":0.06}},"urgency":{"type":"score","score":1.85,"confidence":0.88,"probabilities":{"0":0.03,"1":0.09,"2":0.88},"legend":{"0":"Can wait","1":"Needs a reply today","2":"Service is down"}}},"usage":{"total_input_tokens":324,"input_tokens_by_modality":[{"modality":"text","tokens":324}],"total_output_tokens":0,"total_tokens":324}}`

func decideArgs(serverURL string, extra ...string) []string {
	args := []string{
		"--server-url", serverURL,
		"--api-version", "v1beta",
		"--api-key", "test-key",
		"--no-interactive",
		"--color", "never",
	}
	return append(args, extra...)
}

// Check 1: Help under Understand with 3 examples; no other Decisions command in
// --help, --usage, or explore.
func TestDecideHelpAndSingleVisibleCommand(t *testing.T) {
	rootHelp := runCLI(t, t.TempDir(), nil, "--help")
	if rootHelp.err != nil {
		t.Fatalf("--help failed: %v\n%s", rootHelp.err, rootHelp.stderr)
	}
	understandIdx := strings.Index(rootHelp.stdout, "Understand:")
	manageIdx := strings.Index(rootHelp.stdout, "Manage:")
	decideIdx := strings.Index(rootHelp.stdout, "\n  decide ")
	if understandIdx < 0 || manageIdx < 0 || decideIdx < understandIdx || decideIdx > manageIdx {
		t.Fatalf("decide must appear under Understand: in root help:\n%s", rootHelp.stdout)
	}
	if strings.Contains(rootHelp.stdout, "\n  decisions ") {
		t.Errorf("raw decisions command must not appear in root --help:\n%s", rootHelp.stdout)
	}

	decideHelp := runCLI(t, t.TempDir(), nil, "decide", "--help")
	if decideHelp.err != nil {
		t.Fatalf("decide --help failed: %v\n%s", decideHelp.err, decideHelp.stderr)
	}
	if n := helpLines(decideHelp.stdout); n > 40 {
		t.Errorf("decide --help is %d lines, want <= 40:\n%s", n, decideHelp.stdout)
	}
	for _, want := range []string{
		"Just works:",
		"Defaults: model gemini-decision-1",
		"Learn: https://aistudio.google.com/docs/decisions-api",
		`"Is a package clearly visible?"`,
		`"type":"choice"`,
		`"other"`,
		"@route.json",
	} {
		if !strings.Contains(decideHelp.stdout, want) {
			t.Errorf("decide --help missing %q:\n%s", want, decideHelp.stdout)
		}
	}
	if got := strings.Count(decideHelp.stdout, "  gemini-api decide "); got < 3 {
		t.Errorf("decide --help has %d examples, want at least 3:\n%s", got, decideHelp.stdout)
	}

	usageOut := runCLI(t, t.TempDir(), nil, "--usage")
	if usageOut.err != nil {
		t.Fatalf("--usage failed: %v\n%s", usageOut.err, usageOut.stderr)
	}
	if !strings.Contains(usageOut.stdout, `cmd "decide"`) {
		t.Errorf("--usage missing decide command:\n%s", usageOut.stdout)
	}
	if strings.Contains(usageOut.stdout, `cmd "decisions"`) {
		t.Errorf("--usage must not expose raw decisions command:\n%s", usageOut.stdout)
	}

	decideUsage := runCLI(t, t.TempDir(), nil, "decide", "--usage")
	if decideUsage.err != nil {
		t.Fatalf("decide --usage failed: %v\n%s", decideUsage.err, decideUsage.stderr)
	}
	for _, want := range []string{`cmd "decide"`, "--input", "--stdin", "--body", "--schema", "--model"} {
		if !strings.Contains(decideUsage.stdout, want) {
			t.Errorf("decide --usage missing %q:\n%s", want, decideUsage.stdout)
		}
	}

	// Explorer tree check: decide is present and decisions is absent.
	root, err := cli.NewRootCommand()
	if err != nil {
		t.Fatalf("NewRootCommand: %v", err)
	}
	tree := explorer.BuildTree(root)
	var walk func(explorer.CommandNode)
	foundDecide := false
	foundDecisions := false
	walk = func(n explorer.CommandNode) {
		for _, child := range n.Children {
			if child.Name == "decide" {
				foundDecide = true
			}
			if child.Name == "decisions" {
				foundDecisions = true
			}
			walk(child)
		}
	}
	walk(tree)
	if !foundDecide {
		t.Error("explorer tree missing decide command")
	}
	if foundDecisions {
		t.Error("explorer tree must not contain raw decisions command")
	}

	// Invoking raw decisions fails as unknown command (exit 2).
	rawCmd := runCLI(t, t.TempDir(), nil, "decisions", "create")
	if code := exitCode(t, rawCmd); code != 2 {
		t.Errorf("gemini-api decisions create exit code = %d, want 2\nstderr: %s", code, rawCmd.stderr)
	}
}

// Check 2: Yes/no on a local image via --dry-run.
func TestDecideYesNoLocalImageDryRun(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	dir := t.TempDir()
	imgPath := filepath.Join(dir, "doorstep.jpg")
	if err := os.WriteFile(imgPath, []byte("fake-jpeg-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runCLI(t, dir, nil,
		"--server-url", server.URL, "--api-version", "v1beta", "--no-interactive", "--color", "never",
		"--dry-run", "-o", "json",
		"decide", "-i", imgPath, "Is a package clearly visible?",
	)
	if res.err != nil {
		t.Fatalf("dry-run failed: %v\nstderr: %s", res.err, res.stderr)
	}
	if hits.Load() != 0 {
		t.Fatalf("dry-run made %d HTTP calls, want 0", hits.Load())
	}
	previews := dryRunPreviewLines(t, res.stdout)
	if len(previews) != 1 {
		t.Fatalf("dry-run previews = %d, want 1\nstdout: %s", len(previews), res.stdout)
	}
	reqObj := previews[0]["request"].(map[string]any)
	if reqObj["method"] != "POST" || !strings.HasSuffix(fmt.Sprint(reqObj["url"]), "/v1beta/decisions") {
		t.Fatalf("unexpected request method/url: %#v", reqObj)
	}
	body := reqObj["body"].(map[string]any)
	if body["model"] != "gemini-decision-1" {
		t.Errorf("model = %#v, want gemini-decision-1", body["model"])
	}
	inputList, ok := body["input"].([]any)
	if !ok || len(inputList) != 1 {
		t.Fatalf("input = %#v, want 1 block", body["input"])
	}
	imgBlock := inputList[0].(map[string]any)
	if imgBlock["type"] != "image" || imgBlock["mime_type"] != "image/jpeg" || imgBlock["data"] != "<bytes:15>" {
		t.Errorf("image block = %#v, want image/jpeg with <bytes:15>", imgBlock)
	}
	questions := body["questions"].(map[string]any)
	qObj := questions["q"].(map[string]any)
	if qObj["type"] != "noul" || qObj["instructions"] != "Is a package clearly visible?" {
		t.Errorf("questions.q = %#v, want noul question", qObj)
	}
}

// Check 3: Piped text with --stdin is sent as input, not parsed as JSON body.
func TestDecideStdinTextInput(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleNoulResponse)
	}))
	defer server.Close()

	diffText := "diff --git a/api.go b/api.go\n-func Old()\n+func New()"
	res := runCLIWithStdin(t, t.TempDir(), nil, diffText,
		decideArgs(server.URL, "--no-retries", "-o", "json", "decide", "--stdin", "Does this diff break a public API?")...,
	)
	if res.err != nil {
		t.Fatalf("decide --stdin failed: %v\nstderr: %s", res.err, res.stderr)
	}
	inputList := gotBody["input"].([]any)
	if len(inputList) != 1 {
		t.Fatalf("input len = %d, want 1", len(inputList))
	}
	blk := inputList[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != diffText+"\n" {
		t.Errorf("stdin text block = %#v, want text with trailing newline", blk)
	}

	// Conflict: --stdin with --body @- exits 2 with 0 network calls.
	conflict := runCLIWithStdin(t, t.TempDir(), nil, "hello",
		decideArgs("http://127.0.0.1:1", "decide", "--stdin", "--body", "@-", "q?")...,
	)
	if code := exitCode(t, conflict); code != 2 {
		t.Errorf("--stdin + --body @- exit code = %d, want 2\nstderr: %s", code, conflict.stderr)
	}
}

// Check 4: Questions file (-i plus body with only questions).
func TestDecideQuestionsFileAndJQ(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleChoiceScoreResponse)
	}))
	defer server.Close()

	dir := t.TempDir()
	ticket := filepath.Join(dir, "ticket.txt")
	if err := os.WriteFile(ticket, []byte("API returns 500 on every call"), 0o644); err != nil {
		t.Fatal(err)
	}
	routeFile := filepath.Join(dir, "route.json")
	routeJSON := `{
		"questions": {
			"route": {
				"type": "choice",
				"instructions": "Which queue should handle this request?",
				"criteria": {
					"billing": "Invoices or refunds",
					"technical": "Bugs or outages",
					"other": "Anything else"
				}
			},
			"urgency": {
				"type": "score",
				"instructions": "How urgent is this request?",
				"criteria": ["Can wait", "Needs a reply today", "Service is down"]
			}
		}
	}`
	if err := os.WriteFile(routeFile, []byte(routeJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runCLI(t, dir, nil, decideArgs(server.URL, "--no-retries",
		"decide", "-i", ticket, "--body", "@"+routeFile, "--jq", ".answers.route.choice",
	)...)
	if res.err != nil {
		t.Fatalf("decide with questions file failed: %v\nstderr: %s", res.err, res.stderr)
	}
	if strings.TrimSpace(res.stdout) != "technical" {
		t.Errorf("jq output = %q, want \"technical\"", res.stdout)
	}
	if gotBody["model"] != "gemini-decision-1" {
		t.Errorf("model = %#v, want gemini-decision-1", gotBody["model"])
	}
	inputList := gotBody["input"].([]any)
	if len(inputList) != 1 || inputList[0].(map[string]any)["text"] != "API returns 500 on every call\n" {
		t.Errorf("input = %#v", inputList)
	}
	questions := gotBody["questions"].(map[string]any)
	if _, ok := questions["route"]; !ok {
		t.Errorf("missing route question: %#v", questions)
	}
	if _, ok := questions["urgency"]; !ok {
		t.Errorf("missing urgency question: %#v", questions)
	}
}

// Check 5: Body input shapes (plain string, single block, list) merged flat with -i first.
func TestDecideBodyInputShapesMergeFlat(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(imgPath, []byte("png-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		bodyInput any
		wantTail  []map[string]any
	}{
		{
			name:      "plain string",
			bodyInput: "body text without newline",
			wantTail:  []map[string]any{{"type": "text", "text": "body text without newline"}},
		},
		{
			name:      "single block",
			bodyInput: map[string]any{"type": "text", "text": "single block"},
			wantTail:  []map[string]any{{"type": "text", "text": "single block"}},
		},
		{
			name: "list of blocks",
			bodyInput: []any{
				map[string]any{"type": "text", "text": "first"},
				map[string]any{"type": "document", "mime_type": "application/pdf", "uri": "https://cdn.example.com/a.pdf"},
			},
			wantTail: []map[string]any{
				{"type": "text", "text": "first"},
				{"type": "document", "mime_type": "application/pdf", "uri": "https://cdn.example.com/a.pdf"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &gotBody)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, sampleNoulResponse)
			}))
			defer server.Close()

			bodyBytes, _ := json.Marshal(map[string]any{
				"input": tc.bodyInput,
				"questions": map[string]any{
					"q": map[string]any{"type": "noul", "instructions": "Matches?"},
				},
			})
			res := runCLI(t, dir, nil, decideArgs(server.URL, "--no-retries",
				"decide", "-i", imgPath, "--body", string(bodyBytes),
			)...)
			if res.err != nil {
				t.Fatalf("decide failed: %v\nstderr: %s", res.err, res.stderr)
			}
			inputList, ok := gotBody["input"].([]any)
			if !ok {
				t.Fatalf("merged input is not a flat list: %#v", gotBody["input"])
			}
			if len(inputList) != 1+len(tc.wantTail) {
				t.Fatalf("merged input len = %d, want %d: %#v", len(inputList), 1+len(tc.wantTail), inputList)
			}
			first := inputList[0].(map[string]any)
			if first["type"] != "image" || first["mime_type"] != "image/png" {
				t.Errorf("first block = %#v, want -i image first", first)
			}
			for i, want := range tc.wantTail {
				got := inputList[i+1].(map[string]any)
				for k, v := range want {
					if got[k] != v {
						t.Errorf("block[%d][%q] = %#v, want %#v (caller body block must stay untouched)", i+1, k, got[k], v)
					}
				}
			}
		})
	}
}

// Checks 6, 7, 8: Local validation exits 2 with zero network calls.
func TestDecideLocalValidationErrors(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	dir := t.TempDir()
	txt := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(txt, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	blankTxt := filepath.Join(dir, "blank.txt")
	if err := os.WriteFile(blankTxt, []byte("   \n\t  "), 0o644); err != nil {
		t.Fatal(err)
	}
	bomTxt := filepath.Join(dir, "bom.txt")
	if err := os.WriteFile(bomTxt, []byte("\uFEFF  \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{
			name:    "two question sources",
			args:    []string{"decide", "-i", txt, "--body", `{"questions":{"q":{"type":"noul","instructions":"A?"}}}`, "B?"},
			wantSub: "cannot combine",
		},
		{
			name:    "missing input with positional question",
			args:    []string{"decide", "Is this valid?"},
			wantSub: "missing input",
		},
		{
			name:    "missing input with questions-only body",
			args:    []string{"decide", "--body", `{"questions":{"q":{"type":"noul","instructions":"A?"}}}`},
			wantSub: "missing input",
		},
		{
			name:    "missing question with -i only",
			args:    []string{"decide", "-i", txt},
			wantSub: "missing question",
		},
		{
			name:    "blank positional question",
			args:    []string{"decide", "-i", txt, "   "},
			wantSub: "question cannot be empty",
		},
		{
			name:    "empty questions map in body",
			args:    []string{"decide", "-i", txt, "--body", `{"questions":{}}`},
			wantSub: "at least one question",
		},
		{
			name:    "null question entry in body",
			args:    []string{"decide", "-i", txt, "--body", `{"questions":{"q":null}}`},
			wantSub: "must be a JSON object",
		},
		{
			name:    "null instructions in body",
			args:    []string{"decide", "-i", txt, "--body", `{"questions":{"q":{"type":"noul","instructions":null}}}`},
			wantSub: "missing required",
		},
		{
			name:    "null element in instructions array",
			args:    []string{"decide", "-i", txt, "--body", `{"questions":{"q":{"type":"noul","instructions":[null]}}}`},
			wantSub: "text content block",
		},
		{
			name:    "singular instruction typo gives plural hint",
			args:    []string{"decide", "-i", txt, "--body", `{"questions":{"q":{"type":"noul","instruction":"Ok?"}}}`},
			wantSub: `plural "instructions"`,
		},
		{
			name:    "blank text block instructions",
			args:    []string{"decide", "-i", txt, "--body", `{"questions":{"q":{"type":"noul","instructions":{"type":"text","text":"   "}}}}`},
			wantSub: "blank instructions",
		},
		{
			name:    "blank list block instructions",
			args:    []string{"decide", "-i", txt, "--body", `{"questions":{"q":{"type":"noul","instructions":[{"type":"text","text":"   "}]}}}`},
			wantSub: "blank instructions",
		},
		{
			name:    "unknown question type fails before files.get",
			args:    []string{"decide", "-i", "files/abc123", "--body", `{"questions":{"q":{"type":"unknown","instructions":"Ok?"}}}`},
			wantSub: `invalid "questions"`,
		},
		{
			name:    "blank input string in body",
			args:    []string{"decide", "--body", `{"input":"   ","questions":{"q":{"type":"noul","instructions":"A?"}}}`},
			wantSub: "input cannot be empty",
		},
		{
			name:    "blank input block in body live",
			args:    []string{"decide", "--body", `{"input":[{"type":"text","text":"  "}],"questions":{"q":{"type":"noul","instructions":"A?"}}}`},
			wantSub: "input cannot be empty",
		},
		{
			name:    "blank input block in body dry-run",
			args:    []string{"--dry-run", "decide", "--body", `{"input":[{"type":"text","text":"  "}],"questions":{"q":{"type":"noul","instructions":"A?"}}}`},
			wantSub: "input cannot be empty",
		},
		{
			name:    "whitespace-only local file live",
			args:    []string{"decide", "-i", blankTxt, "Valid?"},
			wantSub: "input cannot be empty",
		},
		{
			name:    "whitespace-only local file dry-run",
			args:    []string{"--dry-run", "decide", "-i", blankTxt, "Valid?"},
			wantSub: "input cannot be empty",
		},
		{
			name:    "BOM-only local file live",
			args:    []string{"decide", "-i", bomTxt, "Valid?"},
			wantSub: "input cannot be empty",
		},
		{
			name:    "BOM-only local file dry-run",
			args:    []string{"--dry-run", "decide", "-i", bomTxt, "Valid?"},
			wantSub: "input cannot be empty",
		},
		{
			name:    "duplicate model flag and body model",
			args:    []string{"decide", "-m", "gemini-decision-1", "--body", `{"model":"gemini-decision-1","input":"hi","questions":{"q":{"type":"noul","instructions":"Ok?"}}}`},
			wantSub: `key "model" is set both`,
		},
		{
			name:    "blank model in body",
			args:    []string{"decide", "--body", `{"model":"  ","input":"hi","questions":{"q":{"type":"noul","instructions":"Ok?"}}}`},
			wantSub: `must be a non-empty string`,
		},
		{
			name:    "body text block with uri",
			args:    []string{"decide", "--body", `{"input":[{"type":"text","uri":"https://cdn.example.com/a.pdf"}],"questions":{"q":{"type":"noul","instructions":"Ok?"}}}`},
			wantSub: "text blocks cannot carry a uri",
		},
		{
			name:    "body plain http link",
			args:    []string{"decide", "--body", `{"input":[{"type":"document","uri":"http://cdn.example.com/a.pdf","mime_type":"application/pdf"}],"questions":{"q":{"type":"noul","instructions":"Ok?"}}}`},
			wantSub: "https://",
		},
		{
			name:    "body gs link",
			args:    []string{"decide", "--body", `{"input":[{"type":"document","uri":"gs://bucket/a.pdf","mime_type":"application/pdf"}],"questions":{"q":{"type":"noul","instructions":"Ok?"}}}`},
			wantSub: "files register",
		},
		{
			name:    "body YouTube link",
			args:    []string{"decide", "--body", `{"input":[{"type":"video","uri":"https://www.youtube.com/watch?v=abc"}],"questions":{"q":{"type":"noul","instructions":"Ok?"}}}`},
			wantSub: "YouTube",
		},
		{
			name:    "body mismatched type and mime_type",
			args:    []string{"decide", "--body", `{"input":[{"type":"image","uri":"https://cdn.example.com/a.pdf","mime_type":"application/pdf"}],"questions":{"q":{"type":"noul","instructions":"Ok?"}}}`},
			wantSub: "does not match mime_type",
		},
		{
			name:    "invalid retry-config strategy",
			args:    []string{"--retry-config", `{"strategy":"bogus"}`, "decide", "-i", txt, "Ok?"},
			wantSub: "invalid --retry-config strategy",
		},
		{
			name:    "mime-type with multiple inputs",
			args:    []string{"decide", "-i", txt, "-i", txt, "--mime-type", "text/plain", "ok?"},
			wantSub: "exactly one --input",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := hits.Load()
			res := runCLI(t, dir, nil, decideArgs(server.URL, tc.args...)...)
			if code := exitCode(t, res); code != 2 {
				t.Errorf("exit code = %d, want 2\nstdout: %s\nstderr: %s", code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.wantSub) {
				t.Errorf("stderr = %q, want substring %q", res.stderr, tc.wantSub)
			}
			if hits.Load() != before {
				t.Errorf("validation error made %d HTTP calls, want 0", hits.Load()-before)
			}
		})
	}
}

// Check 9: Uploaded file lookup (live sends full URI + MIME; dry-run makes 0 lookups).
func TestDecideUploadedFileLookupAndDryRun(t *testing.T) {
	var fileGets atomic.Int32
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta/files/abc123":
			fileGets.Add(1)
			fmt.Fprint(w, `{"name":"files/abc123","uri":"https://generativelanguage.googleapis.com/v1beta/files/abc123","mimeType":"video/mp4","state":"ACTIVE"}`)
		case "/v1beta/files/textfile":
			fileGets.Add(1)
			fmt.Fprint(w, `{"name":"files/textfile","uri":"https://generativelanguage.googleapis.com/v1beta/files/textfile","mimeType":"text/plain","state":"ACTIVE"}`)
		case "/v1beta/files/proc":
			fileGets.Add(1)
			fmt.Fprint(w, `{"name":"files/proc","uri":"https://generativelanguage.googleapis.com/v1beta/files/proc","mimeType":"video/mp4","state":"PROCESSING"}`)
		case "/v1beta/files/bad":
			fileGets.Add(1)
			fmt.Fprint(w, `{"name":"files/bad","uri":"https://generativelanguage.googleapis.com/v1beta/files/bad","mimeType":"video/mp4","state":"FAILED"}`)
		case "/v1beta/decisions":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			fmt.Fprint(w, sampleNoulResponse)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// Live call resolves files/abc123 via files.get and sends full URI + mime_type.
	live := runCLI(t, t.TempDir(), nil, decideArgs(server.URL, "--no-retries",
		"decide", "-i", "files/abc123", "Does a car appear?",
	)...)
	if live.err != nil {
		t.Fatalf("live uploaded file failed: %v\nstderr: %s", live.err, live.stderr)
	}
	if fileGets.Load() != 1 {
		t.Fatalf("files.get calls = %d, want 1", fileGets.Load())
	}
	blk := gotBody["input"].([]any)[0].(map[string]any)
	if blk["type"] != "video" || blk["mime_type"] != "video/mp4" || blk["uri"] != "https://generativelanguage.googleapis.com/v1beta/files/abc123" {
		t.Errorf("resolved uploaded file block = %#v", blk)
	}

	// Short files/ID written inside --body input is expanded the same way.
	gotBody = nil
	bodyRes := runCLI(t, t.TempDir(), nil, decideArgs(server.URL, "--no-retries",
		"decide", "--body", `{"input":[{"type":"video","uri":"files/abc123"}],"questions":{"q":{"type":"noul","instructions":"Car?"}}}`,
	)...)
	if bodyRes.err != nil {
		t.Fatalf("body files/ID expansion failed: %v\nstderr: %s", bodyRes.err, bodyRes.stderr)
	}
	blk = gotBody["input"].([]any)[0].(map[string]any)
	if blk["uri"] != "https://generativelanguage.googleapis.com/v1beta/files/abc123" || blk["mime_type"] != "video/mp4" {
		t.Errorf("expanded body block = %#v", blk)
	}

	// Dry run MUST NOT send or preview the files.get lookup (0 server hits, 1 POST preview).
	beforeGets := fileGets.Load()
	dry := runCLI(t, t.TempDir(), nil,
		"--server-url", server.URL, "--api-version", "v1beta", "--no-interactive", "--color", "never",
		"--dry-run", "-o", "json",
		"decide", "-i", "files/abc123", "Does a car appear?",
	)
	if dry.err != nil {
		t.Fatalf("dry-run uploaded file failed: %v\nstderr: %s", dry.err, dry.stderr)
	}
	if fileGets.Load() != beforeGets {
		t.Fatalf("dry-run called files.get %d times, want 0", fileGets.Load()-beforeGets)
	}
	previews := dryRunPreviewLines(t, dry.stdout)
	if len(previews) != 1 {
		t.Fatalf("dry-run previews = %d, want 1 (no lookup preview)\nstdout: %s", len(previews), dry.stdout)
	}
	if reqObj := previews[0]["request"].(map[string]any); reqObj["method"] != "POST" {
		t.Errorf("dry-run preview method = %#v, want POST", reqObj["method"])
	}

	// Uploaded text file is refused with exit 2 and points to local file or --stdin.
	textRes := runCLI(t, t.TempDir(), nil, decideArgs(server.URL, "--no-retries",
		"decide", "-i", "files/textfile", "Valid?",
	)...)
	if code := exitCode(t, textRes); code != 2 {
		t.Errorf("uploaded text file exit code = %d, want 2\nstderr: %s", code, textRes.stderr)
	}
	if !strings.Contains(textRes.stderr, "--stdin") {
		t.Errorf("uploaded text file error missing --stdin pointer:\n%s", textRes.stderr)
	}

	// Dry run with files/<id> inside --body also makes 0 files.get calls and 1 POST preview.
	beforeBodyDry := fileGets.Load()
	bodyDry := runCLI(t, t.TempDir(), nil,
		"--server-url", server.URL, "--api-version", "v1beta", "--no-interactive", "--color", "never",
		"--dry-run", "-o", "json",
		"decide", "--body", `{"input":[{"type":"video","uri":"files/abc123"}],"questions":{"q":{"type":"noul","instructions":"Car?"}}}`,
	)
	if bodyDry.err != nil {
		t.Fatalf("dry-run body files/ID failed: %v\nstderr: %s", bodyDry.err, bodyDry.stderr)
	}
	if fileGets.Load() != beforeBodyDry {
		t.Fatalf("dry-run body files/ID called files.get %d times, want 0", fileGets.Load()-beforeBodyDry)
	}
	if bodyPreviews := dryRunPreviewLines(t, bodyDry.stdout); len(bodyPreviews) != 1 {
		t.Fatalf("dry-run body previews = %d, want 1\nstdout: %s", len(bodyPreviews), bodyDry.stdout)
	}

	// Body block type mismatch against resolved Files API MIME exits 2.
	mismatchRes := runCLI(t, t.TempDir(), nil, decideArgs(server.URL, "--no-retries",
		"decide", "--body", `{"input":[{"type":"image","uri":"files/abc123"}],"questions":{"q":{"type":"noul","instructions":"Car?"}}}`,
	)...)
	if code := exitCode(t, mismatchRes); code != 2 {
		t.Errorf("body files/ID type mismatch exit code = %d, want 2\nstderr: %s", code, mismatchRes.stderr)
	}
	if !strings.Contains(mismatchRes.stderr, "does not match resolved MIME type") {
		t.Errorf("mismatch stderr = %q, want mismatch explanation", mismatchRes.stderr)
	}

	// PROCESSING and FAILED file states exit 1 with clear guidance.
	procRes := runCLI(t, t.TempDir(), nil, decideArgs(server.URL, "--no-retries",
		"decide", "-i", "files/proc", "Ready?",
	)...)
	if code := exitCode(t, procRes); code != 1 {
		t.Errorf("PROCESSING file exit code = %d, want 1\nstderr: %s", code, procRes.stderr)
	}
	if !strings.Contains(procRes.stderr, "still processing") || !strings.Contains(procRes.stderr, "files get files/proc") {
		t.Errorf("PROCESSING stderr = %q, want wait hint", procRes.stderr)
	}

	failedRes := runCLI(t, t.TempDir(), nil, decideArgs(server.URL, "--no-retries",
		"decide", "-i", "files/bad", "Ready?",
	)...)
	if code := exitCode(t, failedRes); code != 1 {
		t.Errorf("FAILED file exit code = %d, want 1\nstderr: %s", code, failedRes.stderr)
	}
	if !strings.Contains(failedRes.stderr, "failed processing") {
		t.Errorf("FAILED stderr = %q, want failure reason", failedRes.stderr)
	}
}

// Check 10: Web links (https PDF + signed URL, missing ext, text link, http, YouTube, gs://).
func TestDecideWebLinks(t *testing.T) {
	var hits atomic.Int32
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleNoulResponse)
	}))
	defer server.Close()

	// Signed PDF URL with query string and fragment.
	signedPDF := "https://cdn.example.com/reports/spec.pdf?sig=abc123&exp=99999#page=2"
	okRes := runCLI(t, t.TempDir(), nil, decideArgs(server.URL, "--no-retries",
		"decide", "-i", signedPDF, "Is Q3 revenue above $1M?",
	)...)
	if okRes.err != nil {
		t.Fatalf("signed https PDF failed: %v\nstderr: %s", okRes.err, okRes.stderr)
	}
	blk := gotBody["input"].([]any)[0].(map[string]any)
	if blk["type"] != "document" || blk["mime_type"] != "application/pdf" || blk["uri"] != signedPDF {
		t.Errorf("PDF link block = %#v, want document/application/pdf with signed URI", blk)
	}

	// URL with no extension succeeds when --mime-type is supplied.
	noExtURL := "https://cdn.example.com/download?id=42"
	gotBody = nil
	overrideRes := runCLI(t, t.TempDir(), nil, decideArgs(server.URL, "--no-retries",
		"decide", "-i", noExtURL, "--mime-type", "application/pdf", "Signed?",
	)...)
	if overrideRes.err != nil {
		t.Fatalf("https link with --mime-type failed: %v\nstderr: %s", overrideRes.err, overrideRes.stderr)
	}
	blk = gotBody["input"].([]any)[0].(map[string]any)
	if blk["type"] != "document" || blk["mime_type"] != "application/pdf" || blk["uri"] != noExtURL {
		t.Errorf("override link block = %#v", blk)
	}

	// All refused links exit 2 with 0 HTTP calls.
	refused := []struct {
		name    string
		url     string
		wantSub string
	}{
		{"no extension without --mime-type", "https://cdn.example.com/download?id=42", "--mime-type"},
		{"text link (.txt)", "https://cdn.example.com/notes.txt", "--stdin"},
		{"text link (.csv)", "https://cdn.example.com/data.csv", "--stdin"},
		{"text link (.json)", "https://cdn.example.com/data.json", "--stdin"},
		{"plain http link", "http://cdn.example.com/spec.pdf", "https://"},
		{"YouTube short link", "https://youtu.be/dQw4w9WgXcQ", "YouTube"},
		{"YouTube watch link", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", "YouTube"},
		{"YouTube link with port", "https://www.youtube.com:443/watch?v=dQw4w9WgXcQ", "YouTube"},
		{"empty host with port", "https://:443/spec.pdf", "invalid https:// URL"},
		{"gs:// link", "gs://my-bucket/spec.pdf", "files register"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			before := hits.Load()
			res := runCLI(t, t.TempDir(), nil, decideArgs(server.URL, "--no-retries",
				"decide", "-i", tc.url, "Valid?",
			)...)
			if code := exitCode(t, res); code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr: %s", code, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.wantSub) {
				t.Errorf("stderr = %q, want substring %q", res.stderr, tc.wantSub)
			}
			if hits.Load() != before {
				t.Errorf("refused link made %d HTTP calls, want 0", hits.Load()-before)
			}
		})
	}
}

// Check 11: Two local text files (including CSV): sent as text blocks, each ending with \n.
func TestDecideTwoTextFilesTrailingNewlines(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleNoulResponse)
	}))
	defer server.Close()

	dir := t.TempDir()
	txtPath := filepath.Join(dir, "a.txt")
	csvPath := filepath.Join(dir, "b.csv")
	if err := os.WriteFile(txtPath, []byte("customer=acme"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(csvPath, []byte("id,name\n1,acme"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runCLI(t, dir, nil, decideArgs(server.URL, "--no-retries",
		"decide", "-i", txtPath, "-i", csvPath, "Do both mention acme?",
	)...)
	if res.err != nil {
		t.Fatalf("decide two text files failed: %v\nstderr: %s", res.err, res.stderr)
	}
	inputList := gotBody["input"].([]any)
	if len(inputList) != 2 {
		t.Fatalf("input len = %d, want 2", len(inputList))
	}
	b0 := inputList[0].(map[string]any)
	b1 := inputList[1].(map[string]any)
	if b0["type"] != "text" || b0["text"] != "customer=acme\n" {
		t.Errorf("first text block = %#v, want text ending with \\n", b0)
	}
	if b1["type"] != "text" || b1["text"] != "id,name\n1,acme\n" {
		t.Errorf("second (CSV) block = %#v, want text block ending with \\n", b1)
	}

	// Full ordering: -i files in order (no double newline if file already ends
	// in \n), then --stdin, then body input untouched.
	withNewline := filepath.Join(dir, "already_nl.txt")
	if err := os.WriteFile(withNewline, []byte("first line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gotBody = nil
	orderRes := runCLIWithStdin(t, dir, nil, "piped stdin",
		decideArgs(server.URL, "--no-retries",
			"decide", "-i", withNewline, "-i", txtPath, "--stdin",
			"--body", `{"input":"body tail","questions":{"q":{"type":"noul","instructions":"Ordered?"}}}`,
		)...,
	)
	if orderRes.err != nil {
		t.Fatalf("full input ordering failed: %v\nstderr: %s", orderRes.err, orderRes.stderr)
	}
	ordered := gotBody["input"].([]any)
	wantTexts := []string{"first line\n", "customer=acme\n", "piped stdin\n", "body tail"}
	if len(ordered) != len(wantTexts) {
		t.Fatalf("ordered input len = %d, want %d: %#v", len(ordered), len(wantTexts), ordered)
	}
	for i, wantText := range wantTexts {
		gotText := ordered[i].(map[string]any)["text"]
		if gotText != wantText {
			t.Errorf("ordered[%d].text = %q, want %q", i, gotText, wantText)
		}
	}

	// Live local image sends base64 data.
	imgBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	imgPath := filepath.Join(dir, "live.png")
	if err := os.WriteFile(imgPath, imgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	gotBody = nil
	imgRes := runCLI(t, dir, nil, decideArgs(server.URL, "--no-retries",
		"decide", "-i", imgPath, "Is this PNG?",
	)...)
	if imgRes.err != nil {
		t.Fatalf("live local image failed: %v\nstderr: %s", imgRes.err, imgRes.stderr)
	}
	imgBlk := gotBody["input"].([]any)[0].(map[string]any)
	if imgBlk["type"] != "image" || imgBlk["mime_type"] != "image/png" || imgBlk["data"] != base64.StdEncoding.EncodeToString(imgBytes) {
		t.Errorf("live local image block = %#v", imgBlk)
	}

	// Canonicalized --mime-type overrides on local files (video/quicktime -> video/mov,
	// application/pdf; charset=binary -> application/pdf).
	binPath := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(binPath, []byte("binary-payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		override string
		wantType string
		wantMIME string
	}{
		{"video/quicktime", "video", "video/mov"},
		{"application/pdf; charset=binary", "document", "application/pdf"},
	} {
		gotBody = nil
		ovRes := runCLI(t, dir, nil, decideArgs(server.URL, "--no-retries",
			"decide", "-i", binPath, "--mime-type", tc.override, "Matches?",
		)...)
		if ovRes.err != nil {
			t.Fatalf("--mime-type %q failed: %v\nstderr: %s", tc.override, ovRes.err, ovRes.stderr)
		}
		ovBlk := gotBody["input"].([]any)[0].(map[string]any)
		if ovBlk["type"] != tc.wantType || ovBlk["mime_type"] != tc.wantMIME {
			t.Errorf("--mime-type %q block = %#v, want type=%q mime_type=%q", tc.override, ovBlk, tc.wantType, tc.wantMIME)
		}
	}
}

// Check 12: Low probability answer exits 0.
func TestDecideLowProbabilityExitsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"answers":{"q":{"type":"noul","noul":0.02,"confidence":0.98}},"usage":{"total_input_tokens":15,"total_output_tokens":0,"total_tokens":15}}`)
	}))
	defer server.Close()

	res := runCLIWithStdin(t, t.TempDir(), nil, "ordinary message",
		decideArgs(server.URL, "--no-retries", "decide", "--stdin", "Is this spam?")...,
	)
	if code := exitCode(t, res); code != 0 {
		t.Fatalf("low probability answer exit code = %d, want 0\nstderr: %s", code, res.stderr)
	}
}

// Check 13: Blocked input HTTP 400 exits 1 with message, code, and no-retry hint;
// any other HTTP 400 exits 2.
func TestDecideBlockedInputAndOther400ExitCodes(t *testing.T) {
	blockedCodes := []string{
		"safety", "prohibited_content", "blocklist",
		"image_safety", "content_blocked", "jailbreak", "model_armor",
	}
	for _, blockCode := range blockedCodes {
		t.Run("blocked/"+blockCode, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":{"code":%q,"message":"Image generation blocked by policy"}}`, blockCode)
			}))
			defer server.Close()

			// Plain mode
			plain := runCLIWithStdin(t, t.TempDir(), nil, "sample",
				decideArgs(server.URL, "decide", "--stdin", "Safe?")...,
			)
			if code := exitCode(t, plain); code != 1 {
				t.Fatalf("plain exit code = %d, want 1\nstderr: %s", code, plain.stderr)
			}
			if hits.Load() != 1 {
				t.Errorf("blocked 400 was retried (%d calls), want 1", hits.Load())
			}
			for _, want := range []string{"Image generation blocked by policy", blockCode, "blocked", "will not succeed"} {
				if !strings.Contains(plain.stderr, want) {
					t.Errorf("plain stderr missing %q:\n%s", want, plain.stderr)
				}
			}
			if strings.Contains(plain.stderr, "retry with --debug") {
				t.Errorf("blocked stderr must not suggest retrying:\n%s", plain.stderr)
			}

			// Agent mode
			agent := runCLIWithStdin(t, t.TempDir(), nil, "sample",
				decideArgs(server.URL, "--agent-mode", "decide", "--stdin", "Safe?")...,
			)
			if code := exitCode(t, agent); code != 1 {
				t.Fatalf("agent exit code = %d, want 1\nstderr: %s", code, agent.stderr)
			}
			var env struct {
				ErrorType   string   `json:"error_type"`
				ErrorReason string   `json:"error_reason"`
				ExitCode    int      `json:"exit_code"`
				Message     string   `json:"message"`
				Hints       []string `json:"hints"`
			}
			if err := json.Unmarshal([]byte(agent.stderr), &env); err != nil {
				t.Fatalf("agent stderr not JSON: %v\n%s", err, agent.stderr)
			}
			if env.ExitCode != 1 || env.ErrorReason != blockCode || env.Message != "Image generation blocked by policy" {
				t.Errorf("unexpected agent error envelope: %+v", env)
			}
			if !strings.Contains(strings.Join(env.Hints, " "), "will not succeed") {
				t.Errorf("agent hints missing no-retry guidance: %v", env.Hints)
			}
		})
	}

	t.Run("blocked_with_invalid_argument_status_keeps_no_retry_hint", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"status":"INVALID_ARGUMENT","code":"safety","message":"Blocked by safety policy"}}`)
		}))
		defer server.Close()

		plain := runCLIWithStdin(t, t.TempDir(), nil, "sample",
			decideArgs(server.URL, "decide", "--stdin", "Safe?")...,
		)
		if code := exitCode(t, plain); code != 1 {
			t.Fatalf("plain exit code = %d, want 1\nstderr: %s", code, plain.stderr)
		}
		if !strings.Contains(plain.stderr, "will not succeed") {
			t.Errorf("plain stderr missing blocked no-retry hint:\n%s", plain.stderr)
		}
		if strings.Contains(plain.stderr, "retry with --debug") {
			t.Errorf("blocked stderr must not suggest retrying:\n%s", plain.stderr)
		}

		agent := runCLIWithStdin(t, t.TempDir(), nil, "sample",
			decideArgs(server.URL, "--agent-mode", "decide", "--stdin", "Safe?")...,
		)
		if code := exitCode(t, agent); code != 1 {
			t.Fatalf("agent exit code = %d, want 1\nstderr: %s", code, agent.stderr)
		}
		var env struct {
			ErrorReason string   `json:"error_reason"`
			ExitCode    int      `json:"exit_code"`
			Hints       []string `json:"hints"`
		}
		if err := json.Unmarshal([]byte(agent.stderr), &env); err != nil {
			t.Fatalf("agent stderr not JSON: %v\n%s", err, agent.stderr)
		}
		if env.ExitCode != 1 || env.ErrorReason != "safety" || !strings.Contains(strings.Join(env.Hints, " "), "will not succeed") {
			t.Errorf("unexpected agent error envelope: %+v", env)
		}
	})

	t.Run("other_400_exits_2", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"invalid_request","message":"questions must have between 1 and 1000 entries"}}`)
		}))
		defer server.Close()

		res := runCLIWithStdin(t, t.TempDir(), nil, "sample",
			decideArgs(server.URL, "decide", "--stdin", "Valid?")...,
		)
		if code := exitCode(t, res); code != 2 {
			t.Fatalf("non-blocked HTTP 400 exit code = %d, want 2\nstderr: %s", code, res.stderr)
		}
	})

	t.Run("auth_errors_exit_3", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			body   string
		}{
			{"http_401", http.StatusUnauthorized, `{"error":{"code":"unauthenticated","message":"Invalid API key"}}`},
			{"http_400_api_key_invalid", http.StatusBadRequest, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"API key not valid.","details":[{"reason":"API_KEY_INVALID"}]}}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}))
				defer server.Close()

				res := runCLIWithStdin(t, t.TempDir(), nil, "sample",
					decideArgs(server.URL, "decide", "--stdin", "Valid?")...,
				)
				if code := exitCode(t, res); code != 3 {
					t.Fatalf("%s exit code = %d, want 3\nstderr: %s", tc.name, code, res.stderr)
				}
			})
		}
	})
}

// Check 14: Retries (429/503 recovery, max 4 retries = 5 attempts, Retry-After > budget
// aborts immediately, dropped connection on POST retries).
func TestDecideRetries(t *testing.T) {
	fastRetryCfg := `{"strategy":"backoff","backoff":{"initialInterval":10,"maxInterval":20,"exponent":1.5,"maxElapsedTime":5000},"retryConnectionErrors":true}`

	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("recovers_after_%d", status), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":{"code":"unavailable","message":"try again"}}`)
					return
				}
				fmt.Fprint(w, sampleNoulResponse)
			}))
			defer server.Close()

			res := runCLIWithStdin(t, t.TempDir(), nil, "hello",
				decideArgs(server.URL, "--retry-config", fastRetryCfg, "decide", "--stdin", "Ok?")...,
			)
			if res.err != nil {
				t.Fatalf("decide did not recover after %d: %v\nstderr: %s", status, res.err, res.stderr)
			}
			if attempts.Load() != 2 {
				t.Errorf("attempts = %d, want 2", attempts.Load())
			}
		})
	}

	t.Run("stops_after_4_retries", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"unavailable","message":"still down"}}`)
		}))
		defer server.Close()

		res := runCLIWithStdin(t, t.TempDir(), nil, "hello",
			decideArgs(server.URL, "--retry-config", fastRetryCfg, "decide", "--stdin", "Ok?")...,
		)
		if code := exitCode(t, res); code != 1 {
			t.Fatalf("exhausted retries exit code = %d, want 1\nstderr: %s", code, res.stderr)
		}
		if got := attempts.Load(); got != 5 {
			t.Errorf("attempts = %d, want 5 (1 initial + 4 retries)", got)
		}
	})

	t.Run("retry_after_exceeding_budget_aborts_immediately", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"rate_limit","message":"slow down"}}`)
		}))
		defer server.Close()

		start := time.Now()
		res := runCLIWithStdin(t, t.TempDir(), nil, "hello",
			decideArgs(server.URL, "--retry-max-elapsed-time", "2s", "decide", "--stdin", "Ok?")...,
		)
		elapsed := time.Since(start)
		if code := exitCode(t, res); code != 1 {
			t.Fatalf("Retry-After abort exit code = %d, want 1\nstderr: %s", code, res.stderr)
		}
		if attempts.Load() != 1 {
			t.Errorf("attempts = %d, want 1 (should abort before retrying)", attempts.Load())
		}
		if elapsed > 1500*time.Millisecond {
			t.Errorf("elapsed = %v, expected immediate abort instead of waiting", elapsed)
		}
	})

	t.Run("retries_dropped_connection_on_post", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := attempts.Add(1)
			if n == 1 {
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						conn.Close()
						return
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, sampleNoulResponse)
		}))
		defer server.Close()

		res := runCLIWithStdin(t, t.TempDir(), nil, "hello",
			decideArgs(server.URL, "--retry-config", fastRetryCfg, "decide", "--stdin", "Ok?")...,
		)
		if res.err != nil {
			t.Fatalf("decide did not recover from dropped POST connection: %v\nstderr: %s", res.err, res.stderr)
		}
		if attempts.Load() != 2 {
			t.Errorf("attempts = %d, want 2", attempts.Load())
		}
	})

	t.Run("no_retries_disables_retry", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"unavailable","message":"down"}}`)
		}))
		defer server.Close()

		res := runCLIWithStdin(t, t.TempDir(), nil, "hello",
			decideArgs(server.URL, "--no-retries", "decide", "--stdin", "Ok?")...,
		)
		if code := exitCode(t, res); code != 1 {
			t.Fatalf("--no-retries exit code = %d, want 1\nstderr: %s", code, res.stderr)
		}
		if attempts.Load() != 1 {
			t.Errorf("attempts = %d, want 1", attempts.Load())
		}
	})

	t.Run("retry_after_http_date_and_ms_exceeding_budget_abort", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			header string
			value  string
		}{
			{"http_date", "Retry-After", time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)},
			{"retry_after_ms", "Retry-After-Ms", "120000"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var attempts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set(tc.header, tc.value)
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"error":{"code":"rate_limit","message":"slow down"}}`)
				}))
				defer server.Close()

				start := time.Now()
				res := runCLIWithStdin(t, t.TempDir(), nil, "hello",
					decideArgs(server.URL, "--retry-max-elapsed-time", "2s", "decide", "--stdin", "Ok?")...,
				)
				elapsed := time.Since(start)
				if code := exitCode(t, res); code != 1 {
					t.Fatalf("exit code = %d, want 1\nstderr: %s", code, res.stderr)
				}
				if attempts.Load() != 1 {
					t.Errorf("attempts = %d, want 1", attempts.Load())
				}
				if elapsed > 1500*time.Millisecond {
					t.Errorf("elapsed = %v, want immediate abort", elapsed)
				}
			})
		}
	})

	t.Run("retries_mid_body_disconnect_in_raw_json_mode", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := attempts.Add(1)
			if n == 1 {
				if hj, ok := w.(http.Hijacker); ok {
					if conn, buf, err := hj.Hijack(); err == nil {
						_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"answers\":")
						_ = buf.Flush()
						_ = conn.Close()
						return
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, sampleNoulResponse)
		}))
		defer server.Close()

		res := runCLIWithStdin(t, t.TempDir(), nil, "hello",
			decideArgs(server.URL, "--retry-config", fastRetryCfg, "-o", "json", "decide", "--stdin", "Ok?")...,
		)
		if res.err != nil {
			t.Fatalf("decide -o json did not recover from mid-body disconnect: %v\nstderr: %s", res.err, res.stderr)
		}
		if attempts.Load() != 2 {
			t.Errorf("attempts = %d, want 2", attempts.Load())
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(res.stdout), &parsed); err != nil {
			t.Fatalf("stdout not valid JSON after retry: %v\n%s", err, res.stdout)
		}
		if _, ok := parsed["answers"]; !ok {
			t.Errorf("parsed JSON missing answers: %v", parsed)
		}
	})
}

// Check 15: Loop safety (piped input AND file-redirected input) + piped JSON still works.
func TestDecideLoopSafetyAndPipedBody(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell loop test")
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleNoulResponse)
	}))
	defer server.Close()

	dir := t.TempDir()
	var files []string
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("item "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	listFile := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listFile, []byte(strings.Join(files, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qFile := filepath.Join(dir, "q.json")
	if err := os.WriteFile(qFile, []byte(`{"questions":{"q":{"type":"noul","instructions":"Ok?"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	loopCases := []struct {
		name   string
		script string
	}{
		{
			name: "piped_loop_with_question",
			script: fmt.Sprintf(
				`cat %q | while IFS= read -r f; do %q --server-url %q --api-version v1beta --api-key test --no-interactive --no-retries --color never decide -i "$f" "Ok?" >/dev/null || exit 1; echo "DONE:$f"; done`,
				listFile, cliBinary, server.URL,
			),
		},
		{
			name: "piped_loop_with_body",
			script: fmt.Sprintf(
				`cat %q | while IFS= read -r f; do %q --server-url %q --api-version v1beta --api-key test --no-interactive --no-retries --color never decide -i "$f" --body @%q >/dev/null || exit 1; echo "DONE:$f"; done`,
				listFile, cliBinary, server.URL, qFile,
			),
		},
		{
			name: "file_redirect_loop_with_question",
			script: fmt.Sprintf(
				`while IFS= read -r f; do %q --server-url %q --api-version v1beta --api-key test --no-interactive --no-retries --color never decide -i "$f" "Ok?" >/dev/null || exit 1; echo "DONE:$f"; done < %q`,
				cliBinary, server.URL, listFile,
			),
		},
		{
			name: "file_redirect_loop_with_body",
			script: fmt.Sprintf(
				`while IFS= read -r f; do %q --server-url %q --api-version v1beta --api-key test --no-interactive --no-retries --color never decide -i "$f" --body @%q >/dev/null || exit 1; echo "DONE:$f"; done < %q`,
				cliBinary, server.URL, qFile, listFile,
			),
		},
	}

	for _, tc := range loopCases {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			cmd := exec.Command("sh", "-c", tc.script)
			cmd.Env = isolatedEnv(t.TempDir(), nil)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("shell loop failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
			if got := strings.Count(stdout.String(), "DONE:"); got != 3 {
				t.Errorf("loop processed %d items, want 3:\n%s", got, stdout.String())
			}
			if got := calls.Load() - before; got != 3 {
				t.Errorf("API calls = %d, want 3", got)
			}
		})
	}

	// Piped JSON questions still work when -i has no positional question and no --body.
	pipedRes := runCLIWithStdin(t, dir, nil, `{"questions":{"q":{"type":"noul","instructions":"Piped?"}}}`,
		decideArgs(server.URL, "--no-retries", "decide", "-i", files[0])...,
	)
	if pipedRes.err != nil {
		t.Fatalf("piped JSON body with -i failed: %v\nstderr: %s", pipedRes.err, pipedRes.stderr)
	}
}

// Check 16: Default (pretty), -o json, and --agent-mode (TOON) all show the same
// answers and usage with no fields the API did not send.
func TestDecideOutputFormatsParity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleNoulResponse)
	}))
	defer server.Close()

	absentFields := []string{
		"prompt_feedback",
		"total_cached_tokens",
		"total_thought_tokens",
		"total_tool_use_tokens",
		"output_tokens_by_modality",
		"cached_tokens_by_modality",
	}

	// 1. JSON format
	jsonRes := runCLIWithStdin(t, t.TempDir(), nil, "sample",
		decideArgs(server.URL, "--no-retries", "-o", "json", "decide", "--stdin", "Ok?")...,
	)
	if jsonRes.err != nil {
		t.Fatalf("-o json failed: %v\nstderr: %s", jsonRes.err, jsonRes.stderr)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonRes.stdout), &parsed); err != nil {
		t.Fatalf("-o json not valid JSON: %v\n%s", err, jsonRes.stdout)
	}
	if _, hasAnswers := parsed["answers"]; !hasAnswers {
		t.Errorf("-o json missing answers: %s", jsonRes.stdout)
	}
	usageObj, hasUsage := parsed["usage"].(map[string]any)
	if !hasUsage {
		t.Errorf("-o json missing usage: %s", jsonRes.stdout)
	} else if got, ok := usageObj["total_output_tokens"]; !ok || got != float64(0) {
		t.Errorf("-o json usage.total_output_tokens = %#v (present=%v), want 0", got, ok)
	}
	if len(parsed) != 2 {
		t.Errorf("-o json top-level keys = %v, want only answers and usage", parsed)
	}

	// 2. Default (pretty) format
	prettyRes := runCLIWithStdin(t, t.TempDir(), nil, "sample",
		decideArgs(server.URL, "--no-retries", "decide", "--stdin", "Ok?")...,
	)
	if prettyRes.err != nil {
		t.Fatalf("default pretty failed: %v\nstderr: %s", prettyRes.err, prettyRes.stderr)
	}

	// 3. Agent mode (TOON) format
	agentRes := runCLIWithStdin(t, t.TempDir(), nil, "sample",
		decideArgs(server.URL, "--no-retries", "--agent-mode", "decide", "--stdin", "Ok?")...,
	)
	if agentRes.err != nil {
		t.Fatalf("--agent-mode failed: %v\nstderr: %s", agentRes.err, agentRes.stderr)
	}

	// 4. YAML and table formats
	yamlRes := runCLIWithStdin(t, t.TempDir(), nil, "sample",
		decideArgs(server.URL, "--no-retries", "-o", "yaml", "decide", "--stdin", "Ok?")...,
	)
	if yamlRes.err != nil {
		t.Fatalf("-o yaml failed: %v\nstderr: %s", yamlRes.err, yamlRes.stderr)
	}
	tableRes := runCLIWithStdin(t, t.TempDir(), nil, "sample",
		decideArgs(server.URL, "--no-retries", "-o", "table", "decide", "--stdin", "Ok?")...,
	)
	if tableRes.err != nil {
		t.Fatalf("-o table failed: %v\nstderr: %s", tableRes.err, tableRes.stderr)
	}
	headersRes := runCLIWithStdin(t, t.TempDir(), nil, "sample",
		decideArgs(server.URL, "--no-retries", "--include-headers", "decide", "--stdin", "Ok?")...,
	)
	if headersRes.err != nil {
		t.Fatalf("--include-headers failed: %v\nstderr: %s", headersRes.err, headersRes.stderr)
	}
	if !strings.Contains(headersRes.stderr+headersRes.stdout, "Content-Type") {
		t.Errorf("--include-headers output missing response headers:\nstdout: %s\nstderr: %s", headersRes.stdout, headersRes.stderr)
	}

	for name, out := range map[string]string{
		"json":       jsonRes.stdout,
		"pretty":     prettyRes.stdout,
		"agent-mode": agentRes.stdout,
		"yaml":       yamlRes.stdout,
	} {
		for _, want := range []string{"answers", "0.96", "noul", "confidence", "usage", "total_input_tokens", "268", "total_output_tokens", "total_tokens"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output missing %q:\n%s", name, want, out)
			}
		}
		for _, forbid := range absentFields {
			if strings.Contains(out, forbid) {
				t.Errorf("%s output unexpectedly contains unreturned field %q:\n%s", name, forbid, out)
			}
		}
	}
	if !strings.Contains(tableRes.stdout, "0.96") {
		t.Errorf("-o table output missing answer value:\n%s", tableRes.stdout)
	}

	// Choice + score parity across pretty, json, and agent-mode.
	multiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleChoiceScoreResponse)
	}))
	defer multiServer.Close()

	multiBody := `{"input":"Service down","questions":{"route":{"type":"choice","instructions":"Queue?","criteria":{"billing":"B","technical":"T","other":"O"}},"urgency":{"type":"score","instructions":"Urgency?","criteria":["Low","Med","High"]}}}`
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{"pretty", nil},
		{"json", []string{"-o", "json"}},
		{"agent-mode", []string{"--agent-mode"}},
	} {
		args := append([]string{"--no-retries"}, tc.extra...)
		args = append(args, "decide", "--body", multiBody)
		res := runCLI(t, t.TempDir(), nil, decideArgs(multiServer.URL, args...)...)
		if res.err != nil {
			t.Fatalf("choice/score %s failed: %v\nstderr: %s", tc.name, res.err, res.stderr)
		}
		for _, want := range []string{"route", "technical", "0.91", "urgency", "1.85", "Service is down"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("choice/score %s output missing %q:\n%s", tc.name, want, res.stdout)
			}
		}
		for _, forbid := range absentFields {
			if strings.Contains(res.stdout, forbid) {
				t.Errorf("choice/score %s unexpectedly contains unreturned field %q:\n%s", tc.name, forbid, res.stdout)
			}
		}
	}
}

func TestDecideInteractivePrompts(t *testing.T) {
	clearAgentModeEnvironment(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "door.png")
	if err := os.WriteFile(img, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Missing input + question prompts for both.
	pBoth := &scriptedPrompter{
		t:       t,
		answers: map[string][]string{"arg:question": {"Visible?"}, "flag:input": {img}},
	}
	_, stderr, err := executeInteractive(t, pBoth,
		"--interactive", "--dry-run", "--api-key", "k", "--server-url", "https://example.invalid", "--color", "never",
		"decide",
	)
	if err != nil {
		t.Fatalf("interactive decide failed: %v\nstderr: %s", err, stderr)
	}
	if len(pBoth.calls) != 1 || !reflect.DeepEqual(pBoth.calls[0], []string{"arg:question", "flag:input"}) {
		t.Fatalf("interactive decide prompt calls = %#v, want [arg:question flag:input]", pBoth.calls)
	}

	// With -i given, prompts only for arg:question.
	pQuestion := &scriptedPrompter{
		t:       t,
		answers: map[string][]string{"arg:question": {"Visible?"}},
	}
	_, stderr, err = executeInteractive(t, pQuestion,
		"--interactive", "--dry-run", "--api-key", "k", "--server-url", "https://example.invalid", "--color", "never",
		"decide", "-i", img,
	)
	if err != nil {
		t.Fatalf("interactive decide -i failed: %v\nstderr: %s", err, stderr)
	}
	if len(pQuestion.calls) != 1 || !reflect.DeepEqual(pQuestion.calls[0], []string{"arg:question"}) {
		t.Fatalf("interactive decide -i prompt calls = %#v, want [arg:question]", pQuestion.calls)
	}

	// With --body given, neither input nor question is prompted.
	pBody := &scriptedPrompter{t: t, answers: map[string][]string{}}
	_, stderr, err = executeInteractive(t, pBody,
		"--interactive", "--dry-run", "--api-key", "k", "--server-url", "https://example.invalid", "--color", "never",
		"decide", "--body", `{"input":"hi","questions":{"q":{"type":"noul","instructions":"Ok?"}}}`,
	)
	if err != nil {
		t.Fatalf("interactive decide --body failed: %v\nstderr: %s", err, stderr)
	}
	if len(pBody.calls) != 0 {
		t.Fatalf("interactive decide --body called prompter: %#v", pBody.calls)
	}
}

// Check 17: --schema prints the Decisions request JSON schema without network calls.
func TestDecideSchema(t *testing.T) {
	res := runCLI(t, t.TempDir(), nil, "decide", "--schema")
	if res.err != nil {
		t.Fatalf("decide --schema failed: %v\nstderr: %s", res.err, res.stderr)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &schema); err != nil {
		t.Fatalf("decide --schema is not valid JSON: %v\n%s", err, res.stdout)
	}
	// Resolve root schema or $ref -> $defs/CreateDecisionRequest.
	target := schema
	if ref, ok := schema["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
		defName := strings.TrimPrefix(ref, "#/$defs/")
		defs, ok := schema["$defs"].(map[string]any)
		if !ok {
			t.Fatalf("schema has $ref %q but missing $defs: %s", ref, res.stdout)
		}
		defObj, ok := defs[defName].(map[string]any)
		if !ok {
			t.Fatalf("schema missing $defs[%q]: %s", defName, res.stdout)
		}
		target = defObj
	}
	props, ok := target["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema missing properties: %s", res.stdout)
	}
	for _, field := range []string{"input", "model", "questions"} {
		if _, exists := props[field]; !exists {
			t.Errorf("schema properties missing %q: %s", field, res.stdout)
		}
	}
	modelProp, _ := props["model"].(map[string]any)
	if modelProp["default"] != "gemini-decision-1" {
		t.Errorf("schema model.default = %#v, want gemini-decision-1", modelProp["default"])
	}
}
