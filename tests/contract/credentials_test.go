package contract_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// TestCredentialsRequestConstruction pins the wire shape of every credentials
// operation: method, path, query and the body each union variant produces.
func TestCredentialsRequestConstruction(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		method string
		path   string
		query  url.Values
		body   string
	}{
		{
			name:   "list",
			args:   []string{"credentials", "list", "--page-size", "5", "--page-token", "p2"},
			method: http.MethodGet, path: "/v1beta/credentials",
			query: url.Values{"page_size": {"5"}, "page_token": {"p2"}},
		},
		{
			name:   "get positional",
			args:   []string{"credentials", "get", "gh-token"},
			method: http.MethodGet, path: "/v1beta/credentials/gh-token",
		},
		{
			name:   "get flag",
			args:   []string{"credentials", "get", "--id", "gh-token"},
			method: http.MethodGet, path: "/v1beta/credentials/gh-token",
		},
		{
			name:   "delete",
			args:   []string{"credentials", "delete", "gh-token"},
			method: http.MethodDelete, path: "/v1beta/credentials/gh-token",
		},
		{
			name: "create bearer_token via expanded flags",
			args: []string{"credentials", "create",
				"--body-param.bearer-token.id", "gh-token",
				"--body-param.bearer-token.token", "ghp_secret",
				"--body-param.bearer-token.prefix", "token",
			},
			method: http.MethodPost, path: "/v1beta/credentials",
			body: `{"type":"bearer_token","id":"gh-token","token":"ghp_secret","prefix":"token"}`,
		},
		{
			name: "create environment_variable via variant flag",
			args: []string{"credentials", "create",
				"--body-param.environment-variable", `{"id":"gh-env","injection_location":["header","query"],"value":"s3cr3t"}`,
			},
			method: http.MethodPost, path: "/v1beta/credentials",
			body: `{"type":"environment_variable","id":"gh-env","injection_location":["header","query"],"value":"s3cr3t"}`,
		},
		{
			name: "create oauth2 via --body",
			args: []string{"credentials", "create", "--body",
				`{"type":"oauth2","id":"gcp","client_id":"c","client_secret":"s","refresh_token":"r","token_url":"https://oauth2.googleapis.com/token"}`,
			},
			method: http.MethodPost, path: "/v1beta/credentials",
			body: `{"type":"oauth2","id":"gcp","client_id":"c","client_secret":"s","refresh_token":"r","token_url":"https://oauth2.googleapis.com/token"}`,
		},
		{
			name: "update bearer_token with update mask",
			args: []string{"credentials", "update", "gh-token",
				"--body-param.bearer-token.token", "ghp_rotated",
				"--update-mask", "token",
			},
			method: http.MethodPatch, path: "/v1beta/credentials/gh-token",
			query: url.Values{"update_mask": {"token"}},
			body:  `{"type":"bearer_token","token":"ghp_rotated"}`,
		},
		{
			name:   "update oauth2 via --body",
			args:   []string{"credentials", "update", "--id", "gcp", "--body", `{"type":"oauth2","scopes":["a","b"]}`},
			method: http.MethodPatch, path: "/v1beta/credentials/gcp",
			body: `{"type":"oauth2","scopes":["a","b"]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, calls := newCaptureServer(t, `{}`)
			result := runCLI(t, t.TempDir(), nil, append(baseArgs(server.URL), tc.args...)...)
			if result.err != nil {
				t.Fatalf("failed: %v\nstderr: %s", result.err, result.stderr)
			}
			call := <-calls
			if call.method != tc.method || call.path != tc.path {
				t.Errorf("request = %s %s, want %s %s", call.method, call.path, tc.method, tc.path)
			}
			query, err := url.ParseQuery(call.query)
			if err != nil {
				t.Fatalf("parse query %q: %v", call.query, err)
			}
			if len(query) == 0 && tc.query == nil {
				query = nil
			}
			if !reflect.DeepEqual(query, tc.query) {
				t.Errorf("query = %v, want %v", query, tc.query)
			}
			var want map[string]any
			if tc.body != "" {
				if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(call.body, want) {
				t.Errorf("body = %v, want %v", call.body, want)
			}
		})
	}
}

// TestCredentialsBodyValidation: the union body is validated before dispatch.
// The bearer_token fields are required only once that variant is selected, so
// another variant must go through without them.
func TestCredentialsBodyValidation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"create without body", []string{"credentials", "create"}, "selects no variant"},
		{"create bearer_token without token",
			[]string{"credentials", "create", "--body-param.bearer-token.id", "gh"},
			"missing required flag: --body-param.bearer-token.token"},
		{"create bearer_token without id",
			[]string{"credentials", "create", "--body-param.bearer-token.token", "t"},
			"missing required flag: --body-param.bearer-token.id"},
		{"create with two variants",
			[]string{"credentials", "create", "--body-param.bearer-token.id", "gh", "--body-param.bearer-token.token", "t", "--body-param.oauth2", `{"id":"o"}`},
			"multiple union variants provided for --body-param"},
		{"update without id",
			[]string{"credentials", "update", "--body-param.bearer-token.token", "t"},
			"missing required flag: --id"},
		{"update with blank id",
			[]string{"credentials", "update", "--id", " ", "--body-param.bearer-token.token", "t"},
			"blank; path parameters require a non-empty value"},
		{"update without body", []string{"credentials", "update", "gh"}, "selects no variant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range [][]string{nil, {"--dry-run"}} {
				before := requests.Load()
				args := append(append(plainArgs(server.URL), mode...), tc.args...)
				result := runCLI(t, t.TempDir(), nil, args...)
				if code := exitCode(t, result); code != 2 {
					t.Errorf("%v exit code = %d, want 2\nstderr: %s", mode, code, result.stderr)
				}
				if !strings.Contains(result.stderr, tc.want) {
					t.Errorf("%v stderr missing %q:\n%s", mode, tc.want, result.stderr)
				}
				if strings.Contains(result.stderr, "[DRY-RUN]") || requests.Load() != before {
					t.Errorf("%v previewed or sent a request:\n%s", mode, result.stderr)
				}
			}
		})
	}

	t.Run("non-bearer variant needs no bearer flags", func(t *testing.T) {
		before := requests.Load()
		args := append(plainArgs(server.URL), "credentials", "create",
			"--body-param.environment-variable", `{"id":"gh-env","injection_location":"header","value":"v"}`)
		if result := runCLI(t, t.TempDir(), nil, args...); result.err != nil {
			t.Fatalf("failed: %v\nstderr: %s", result.err, result.stderr)
		}
		if requests.Load() == before {
			t.Error("request was not dispatched")
		}
	})
}

// TestCredentialsHelpDoesNotRequireVariantFields: CredentialCreateParams is a
// discriminated union and only the bearer_token variant is expanded into
// per-field flags. Its fields are required within that variant, not by the
// command, so help must not list them under "Required Flags:".
func TestCredentialsHelpDoesNotRequireVariantFields(t *testing.T) {
	for _, verb := range []string{"create", "update"} {
		t.Run(verb, func(t *testing.T) {
			result := runCLI(t, t.TempDir(), nil, "credentials", verb, "--help")
			if result.err != nil {
				t.Fatalf("--help failed: %v\nstderr: %s", result.err, result.stderr)
			}
			if !strings.Contains(result.stdout, "--body-param.bearer-token.token") {
				t.Fatalf("help does not list the bearer_token variant flags:\n%s", result.stdout)
			}
			_, required, _ := strings.Cut(result.stdout, "Required Flags:\n")
			required, _, _ = strings.Cut(required, "\n\n")
			if strings.Contains(required, "--body-param.") {
				t.Errorf("help lists union variant fields as required:\n%s", required)
			}
		})
	}
}

// TestCredentialsHelpDocumentsOperations: every credentials operation carries
// a runnable example, and the bearer_token id flag is described.
func TestCredentialsHelpDocumentsOperations(t *testing.T) {
	for _, verb := range []string{"list", "create", "get", "update", "delete"} {
		t.Run(verb, func(t *testing.T) {
			result := runCLI(t, t.TempDir(), nil, "credentials", verb, "--help")
			if result.err != nil {
				t.Fatalf("--help failed: %v\nstderr: %s", result.err, result.stderr)
			}
			_, examples, _ := strings.Cut(result.stdout, "Just works:\n")
			if !strings.HasPrefix(examples, "  gemini-api credentials "+verb) {
				t.Errorf("help has no example invocation:\n%s", result.stdout)
			}
		})
	}

	result := runCLI(t, t.TempDir(), nil, "credentials", "create", "--usage")
	if result.err != nil {
		t.Fatalf("--usage failed: %v\nstderr: %s", result.err, result.stderr)
	}
	if strings.Contains(result.stdout, `flag "--body-param.bearer-token.id <id>" help="[required]"`) {
		t.Errorf("--body-param.bearer-token.id has no description")
	}
}

// TestCredentialsSecretsNeverPrinted: every write-only secret of every create
// variant stays out of dry-run previews (human and JSON) and --debug traces.
func TestCredentialsSecretsNeverPrinted(t *testing.T) {
	const secret = "sekr1t-c4nary"
	variants := map[string]string{
		"bearer_token token":         `{"type":"bearer_token","id":"b","token":"` + secret + `"}`,
		"environment_variable value": `{"type":"environment_variable","id":"e","injection_location":"header","value":"` + secret + `"}`,
		"oauth2 client_secret":       `{"type":"oauth2","id":"o","client_id":"c","client_secret":"` + secret + `","refresh_token":"r","token_url":"https://oauth2.googleapis.com/token"}`,
		"oauth2 refresh_token":       `{"type":"oauth2","id":"o","client_id":"c","client_secret":"s","refresh_token":"` + secret + `","token_url":"https://oauth2.googleapis.com/token"}`,
	}
	modes := map[string][]string{
		"dry-run human": {"--dry-run"},
		"dry-run json":  {"--dry-run", "--output-format", "json"},
		"debug":         {"--debug"},
	}
	for name, body := range variants {
		for label, mode := range modes {
			t.Run(name+"/"+label, func(t *testing.T) {
				server, _ := newCaptureServer(t, `{}`)
				args := append(append(plainArgs(server.URL), mode...), "credentials", "create", "--body", body)
				result := runCLI(t, t.TempDir(), nil, args...)
				if result.err != nil {
					t.Fatalf("failed: %v\nstderr: %s", result.err, result.stderr)
				}
				if strings.Contains(result.stdout+result.stderr, secret) {
					t.Errorf("secret leaked\nstdout: %s\nstderr: %s", result.stdout, result.stderr)
				}
			})
		}
	}
}
