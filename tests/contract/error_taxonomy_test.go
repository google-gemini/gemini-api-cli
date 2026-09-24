package contract_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Build Specification (rev 1.0, item 4 + Appendix A8) requires every
// failure to render as ONE classified error object with an actionable fix, in
// every output mode, classified by error.details[].reason before the HTTP
// status. The reason table ships as data in overlays/reference-cli/errors.yaml
// (x-speakeasy-cli-errors); these tests pin the human rendering and the
// pretty/json/agent parity for the three reasons the spec calls out.

type taxonomyCase struct {
	name       string
	status     int
	body       string
	wantType   string
	wantReason string
	wantMsg    string
	wantHint   string   // substring that must appear in the fix
	forbidHint []string // substrings that must NOT appear anywhere on stderr
}

var taxonomyCases = []taxonomyCase{
	{
		name:       "API_KEY_INVALID on HTTP 400 is an authentication error",
		status:     http.StatusBadRequest,
		body:       `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com","metadata":{"service":"generativelanguage.googleapis.com"}}]}}`,
		wantType:   "authentication_error",
		wantReason: "API_KEY_INVALID",
		wantMsg:    "API key not valid. Please pass a valid API key.",
		wantHint:   "GEMINI_API_KEY",
		forbidHint: []string{"--dry-run", "check parameters"},
	},
	{
		name:       "SERVICE_DISABLED on HTTP 403 is not a credential problem",
		status:     http.StatusForbidden,
		body:       `{"error":{"code":403,"message":"Generative Language API has not been used in project 123 before or it is disabled.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED","domain":"googleapis.com","metadata":{"service":"generativelanguage.googleapis.com","consumer":"projects/123"}}]}}`,
		wantType:   "service_disabled",
		wantReason: "SERVICE_DISABLED",
		wantMsg:    "Generative Language API has not been used in project 123 before or it is disabled.",
		wantHint:   "not a credential problem",
		forbidHint: []string{"GEMINI_API_KEY", "configure' to set up or update your credentials", "credentials are valid but not permitted"},
	},
	{
		name:       "BILLING_DISABLED on HTTP 403 is not a credential problem",
		status:     http.StatusForbidden,
		body:       `{"error":{"code":403,"message":"This API method requires billing to be enabled.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"BILLING_DISABLED","domain":"googleapis.com"}]}}`,
		wantType:   "billing_disabled",
		wantReason: "BILLING_DISABLED",
		wantMsg:    "This API method requires billing to be enabled.",
		wantHint:   "Enable billing",
		forbidHint: []string{"GEMINI_API_KEY", "configure' to set up or update your credentials"},
	},
}

func taxonomyServer(t *testing.T, tc taxonomyCase) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(tc.status)
		fmt.Fprint(w, tc.body)
	}))
	t.Cleanup(server.Close)
	return server
}

func prettyArgs(serverURL string) []string {
	return []string{
		"--server-url", serverURL,
		"--api-version", "v1beta",
		"--api-key", "test-key",
		"--no-interactive",
		"--no-retries",
		"--color", "never",
	}
}

type errorEnvelope struct {
	ErrorType   string   `json:"error_type"`
	ErrorReason string   `json:"error_reason"`
	Message     string   `json:"message"`
	Hints       []string `json:"hints"`
	StatusCode  int      `json:"status_code"`
}

func decodeEnvelope(t *testing.T, stderr string) errorEnvelope {
	t.Helper()
	var envelope errorEnvelope
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("stderr is not one JSON document: %v\n%s", err, stderr)
	}
	return envelope
}

// TestPrettyModeErrorIsClassifiedOnce pins the human rendering: the first
// stderr line names the type and the API message, the reason and HTTP status
// follow, and the fix comes from the overlay's reason table. The message
// appears exactly once (no raw-body echo of the same text) and stdout stays
// empty.
func TestPrettyModeErrorIsClassifiedOnce(t *testing.T) {
	for _, tc := range taxonomyCases {
		t.Run(tc.name, func(t *testing.T) {
			server := taxonomyServer(t, tc)
			result := runCLI(t, t.TempDir(), nil, append(prettyArgs(server.URL), "agent", "list")...)
			if result.err == nil {
				t.Fatalf("agent list succeeded on HTTP %d\nstdout: %s", tc.status, result.stdout)
			}
			if result.stdout != "" {
				t.Errorf("failure wrote stdout: %q", result.stdout)
			}
			stderr := result.stderr
			lines := strings.Split(strings.TrimSpace(stderr), "\n")
			wantFirst := fmt.Sprintf("Error (%s): %s", tc.wantType, tc.wantMsg)
			if lines[0] != wantFirst {
				t.Errorf("first stderr line = %q, want %q\nfull stderr:\n%s", lines[0], wantFirst, stderr)
			}
			wantReason := fmt.Sprintf("Reason: %s (HTTP %d)", tc.wantReason, tc.status)
			if !strings.Contains(stderr, wantReason) {
				t.Errorf("stderr lacks %q:\n%s", wantReason, stderr)
			}
			if !strings.Contains(stderr, "Fix:") || !strings.Contains(stderr, tc.wantHint) {
				t.Errorf("stderr lacks a Fix: block containing %q:\n%s", tc.wantHint, stderr)
			}
			if got := strings.Count(stderr, tc.wantMsg); got != 1 {
				t.Errorf("message printed %d times, want exactly one copy:\n%s", got, stderr)
			}
			for _, forbidden := range tc.forbidHint {
				if strings.Contains(stderr, forbidden) {
					t.Errorf("stderr contains misleading advice %q:\n%s", forbidden, stderr)
				}
			}
			if strings.Contains(stderr, "API Error (HTTP") {
				t.Errorf("legacy unclassified rendering still present:\n%s", stderr)
			}
		})
	}
}

// TestErrorClassificationParityAcrossModes pins that pretty, --output-format
// json and --agent-mode agree on error_type / error_reason / the leading fix
// for the same failure, and that the JSON envelope carries the classification
// even outside agent mode.
func TestErrorClassificationParityAcrossModes(t *testing.T) {
	for _, tc := range taxonomyCases {
		t.Run(tc.name, func(t *testing.T) {
			server := taxonomyServer(t, tc)

			jsonResult := runCLI(t, t.TempDir(), nil, append(prettyArgs(server.URL), "--output-format", "json", "agent", "list")...)
			if jsonResult.err == nil {
				t.Fatal("json mode succeeded on an API error")
			}
			jsonEnv := decodeEnvelope(t, jsonResult.stderr)

			agentResult := runCLI(t, t.TempDir(), nil, append(prettyArgs(server.URL), "--agent-mode", "agent", "list")...)
			if agentResult.err == nil {
				t.Fatal("agent mode succeeded on an API error")
			}
			agentEnv := decodeEnvelope(t, agentResult.stderr)

			for label, env := range map[string]errorEnvelope{"json": jsonEnv, "agent": agentEnv} {
				if env.ErrorType != tc.wantType {
					t.Errorf("%s error_type = %q, want %q", label, env.ErrorType, tc.wantType)
				}
				if env.ErrorReason != tc.wantReason {
					t.Errorf("%s error_reason = %q, want %q", label, env.ErrorReason, tc.wantReason)
				}
				if env.Message != tc.wantMsg {
					t.Errorf("%s message = %q, want %q", label, env.Message, tc.wantMsg)
				}
				if env.StatusCode != tc.status {
					t.Errorf("%s status_code = %d, want %d", label, env.StatusCode, tc.status)
				}
				if len(env.Hints) == 0 || !strings.Contains(strings.Join(env.Hints, "\n"), tc.wantHint) {
					t.Errorf("%s hints = %v, want one containing %q", label, env.Hints, tc.wantHint)
				}
				for _, forbidden := range tc.forbidHint {
					if strings.Contains(strings.Join(env.Hints, "\n"), forbidden) {
						t.Errorf("%s hints contain misleading advice %q: %v", label, forbidden, env.Hints)
					}
				}
			}
			if strings.Join(jsonEnv.Hints, "\n") != strings.Join(agentEnv.Hints, "\n") {
				t.Errorf("json and agent hints differ:\n%v\n%v", jsonEnv.Hints, agentEnv.Hints)
			}

			// The pretty rendering carries the same leading fix.
			prettyResult := runCLI(t, t.TempDir(), nil, append(prettyArgs(server.URL), "agent", "list")...)
			if len(jsonEnv.Hints) > 0 && !strings.Contains(prettyResult.stderr, jsonEnv.Hints[0]) {
				t.Errorf("pretty output lacks the leading hint %q:\n%s", jsonEnv.Hints[0], prettyResult.stderr)
			}
		})
	}
}
