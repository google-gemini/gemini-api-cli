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

package custom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/client"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/sdkerrors"
	"github.com/google-gemini/gemini-api-cli/internal/sdk"
	"github.com/google-gemini/gemini-api-cli/internal/testclient"
	"github.com/spf13/cobra"
)

// rawTransport sends hand-built HTTP requests (today: the resumable Files
// upload protocol, which the OpenAPI document does not describe). It mirrors
// the generated client's global options from the generated CLI's exported
// helpers: the single credential config.PickCredential selects, a validated
// server URL, API version, Api-Revision, quota project, --header values,
// test-client injection, and the --dry-run / --debug diagnostics wrapper.
//
// Deliberate differences from the generated client: SDK request hooks do not
// run, --timeout bounds each request end to end (headers and body) instead of
// each transport phase, and nothing is retried — an upload chunk is a
// mutation at a fixed offset, so a blind replay is not safe.
type rawTransport struct {
	baseURL     string
	apiVersion  string
	apiKey      string
	accessToken string
	userProject string
	apiRevision string
	headers     map[string]string
	timeout     time.Duration
	client      client.HTTPClient
}

var apiVersionShape = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validAPIVersion holds the version to one inert path segment: the shape alone
// would still admit the traversal segments "." and "..".
func validAPIVersion(v string) bool {
	return apiVersionShape.MatchString(v) && v != "." && v != ".."
}

const rawUserAgent = "speakeasy-sdk/go 0.0.1 internal v1beta github.com/speakeasy-sdks/gemini-cli-next/internal/sdk (porcelain)"

func newRawTransport(cmd *cobra.Command) (*rawTransport, error) {
	t := &rawTransport{headers: map[string]string{}}

	// Server: --server-url > first declared server.
	if serverURL, _ := flagutil.GetStringFlag(cmd, "server-url"); serverURL != "" {
		if err := flagutil.ValidateServerURL(serverURL); err != nil {
			return nil, err
		}
		t.baseURL = serverURL
	} else {
		t.baseURL = sdk.ServerList[0]
	}
	t.baseURL = strings.TrimRight(t.baseURL, "/")

	// API version: flag > env/config > flag default (v1beta). An explicitly
	// empty flag is invalid; only an absent value may fall back to the default.
	apiVersionExplicit := flagutil.FlagChanged(cmd, "api-version")
	if apiVersionExplicit {
		t.apiVersion, _ = flagutil.GetStringFlag(cmd, "api-version")
	} else if v := config.GetString("api-version"); v != "" {
		t.apiVersion = v
	} else {
		t.apiVersion, _ = flagutil.GetStringFlag(cmd, "api-version")
	}
	if t.apiVersion == "" && !apiVersionExplicit {
		t.apiVersion = "v1beta"
	}
	// The generated client's request hook holds the version to one path
	// segment; that hook does not run here, and the version is spliced into the
	// upload path ("/upload/../evil/files").
	if !validAPIVersion(t.apiVersion) {
		return nil, usageError("--api-version must be non-empty, contain only letters, numbers, '.', '_', or '-', and cannot be '.' or '..'")
	}

	// Request credentials use the SDK client's request-scoped chain. Dry-run
	// deliberately skips the OS keychain so unattended probing cannot prompt.
	// Exactly one scheme is kept, ranked like the generated client
	// (flag > env > keyring > config, API key on a tie).
	apiKey, apiKeySource := config.ResolveRequestSecurityCredential(cmd, "api-key")
	accessToken, accessTokenSource := config.ResolveRequestSecurityCredential(cmd, "access-token")
	switch config.PickCredential([]config.CredentialCandidate{
		{Field: "APIKey", Complete: apiKey != "", Sources: []string{apiKeySource}},
		{Field: "AccessToken", Complete: accessToken != "", Sources: []string{accessTokenSource}},
	}, nil) {
	case 0:
		t.apiKey = apiKey
	case 1:
		t.accessToken = accessToken
	}
	t.userProject = resolveGlobalString(cmd, "user-project")
	t.apiRevision = resolveGlobalString(cmd, "api-revision")

	// Repeatable --header "Key: Value".
	if hdrs, _ := flagutil.GetStringArrayFlag(cmd, "header"); len(hdrs) > 0 {
		for _, h := range hdrs {
			k, v, ok := strings.Cut(h, ":")
			if !ok {
				return nil, fmt.Errorf("invalid header format %q: expected \"Key: Value\"", h)
			}
			t.headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}

	// Timeout: flag > env/config.
	timeoutStr, changed := flagutil.GetStringFlag(cmd, "timeout")
	if !changed || timeoutStr == "" {
		timeoutStr = config.GetString("timeout")
	}
	if timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --timeout value %q: %w", timeoutStr, err)
		}
		t.timeout = d
	}

	// Never follow redirects: a 3xx from the upload endpoint could otherwise
	// forward the resolved credentials and the file bytes to an arbitrary
	// Location. The resumable protocol never legitimately redirects.
	var httpClient client.HTTPClient = &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if tc := testclient.NewTestHTTPClient(); tc != nil {
		httpClient = tc
	}
	if !isDryRun(cmd) {
		// The diagnostics wrapper does not treat the resumable session id as
		// sensitive. Keep it out of diagnostics and transport errors on both
		// sides of the wrapper, whether or not --debug is enabled.
		httpClient = sessionRedactor{client.WrapClientForDiagnostics(cmd, sessionRestorer{httpClient})}
	} else {
		httpClient = client.WrapClientForDiagnostics(cmd, httpClient)
	}
	t.client = httpClient
	return t, nil
}

// resolveGlobalString reads a global parameter: flag > env/config.
func resolveGlobalString(cmd *cobra.Command, name string) string {
	if flagutil.FlagChanged(cmd, name) {
		val, _ := flagutil.GetStringFlag(cmd, name)
		return val
	}
	return config.GetString(name)
}

// apiURL joins a path (already including the API version segment when
// needed) onto the configured server.
func (t *rawTransport) apiURL(path string) string {
	return t.baseURL + "/" + strings.TrimLeft(path, "/")
}

// newRequest builds a request against an absolute URL with the runtime's
// credentials and standard headers applied.
func (t *rawTransport) newRequest(ctx context.Context, method, absURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, absURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", rawUserAgent)
	if t.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+t.accessToken)
	} else if t.apiKey != "" {
		req.Header.Set("x-goog-api-key", t.apiKey)
	}
	if t.userProject != "" {
		req.Header.Set("x-goog-user-project", t.userProject)
	}
	if t.apiRevision != "" {
		req.Header.Set("Api-Revision", t.apiRevision)
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	// Exactly one scheme leaves the process, as on generated requests: a
	// caller-supplied Authorization header displaces the API key.
	if req.Header.Get("Authorization") != "" {
		req.Header.Del("x-goog-api-key")
	}
	return req, nil
}

// do sends the request through the wrapped client, applying the configured
// timeout. Non-2xx responses are converted into the SDK's default error so
// output.Error classifies them exactly like generated commands. The timeout
// covers the response body too: it is released when the caller closes the body.
func (t *rawTransport) do(req *http.Request) (*http.Response, error) {
	cancel := context.CancelFunc(func() {})
	if t.timeout > 0 {
		var ctx context.Context
		ctx, cancel = context.WithTimeout(req.Context(), t.timeout)
		req = req.WithContext(ctx)
	}
	res, err := t.client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("error sending request: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		cancel()
		return nil, sdkerrors.NewSDKDefaultError("API error occurred", res.StatusCode, string(body), res)
	}
	res.Body = cancelOnClose{res.Body, cancel}
	return res, nil
}

// cancelOnClose releases a request's timeout context once its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// sameService reports whether a server-provided URL (the resumable upload
// session) may receive our credentials and file bytes. It must be either the
// exact configured server (same scheme AND host — a scheme downgrade such as
// https→http on the same host is rejected) or an HTTPS Google API host.
func (t *rawTransport) sameService(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return false
	}
	if base, err := url.Parse(t.baseURL); err == nil &&
		strings.EqualFold(u.Host, base.Host) && strings.EqualFold(u.Scheme, base.Scheme) {
		return true
	}
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (host == "googleapis.com" || strings.HasSuffix(host, ".googleapis.com"))
}

// The resumable upload session id ("upload_id") is a bearer-like capability:
// whoever holds the session URL can write to the upload. sessionRedactor and
// sessionRestorer sandwich the diagnostics wrapper so it only ever sees the id
// redacted — in the chunk request URL and in the start response's
// X-Goog-Upload-Url header — while the wire and the caller see the real value.
const (
	sessionQueryKey  = "upload_id"
	sessionURLHeader = "X-Goog-Upload-Url"
)

type sessionSecrets struct {
	requestURL *url.URL
	sessionURL string
}

type sessionSecretsKey struct{}

// redactSessionID returns the URL with its upload_id value masked, and whether
// there was one to mask.
func redactSessionID(u *url.URL) (*url.URL, bool) {
	query := u.Query()
	if !query.Has(sessionQueryKey) {
		return u, false
	}
	query.Set(sessionQueryKey, "REDACTED")
	masked := *u
	masked.RawQuery = query.Encode()
	return &masked, true
}

// redactSessionError masks the session id inside a transport error. Go's
// transport wraps the real request URL in *url.Error, which the diagnostics
// wrapper prints as "Transport Error" and the command reports to the user.
func redactSessionError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	parsed, parseErr := url.Parse(urlErr.URL)
	if parseErr != nil {
		return err
	}
	if masked, ok := redactSessionID(parsed); ok {
		urlErr.URL = masked.String()
	}
	return err
}

// sessionRedactor sits outside the diagnostics wrapper.
type sessionRedactor struct{ inner client.HTTPClient }

func (c sessionRedactor) Do(req *http.Request) (*http.Response, error) {
	secrets := &sessionSecrets{}
	req = req.WithContext(context.WithValue(req.Context(), sessionSecretsKey{}, secrets))
	if masked, ok := redactSessionID(req.URL); ok {
		secrets.requestURL = req.URL
		req.URL = masked
	}
	res, err := c.inner.Do(req)
	if res != nil && secrets.sessionURL != "" {
		res.Header.Set(sessionURLHeader, secrets.sessionURL)
	}
	return res, err
}

// sessionRestorer sits inside the diagnostics wrapper, next to the wire.
type sessionRestorer struct{ inner client.HTTPClient }

func (c sessionRestorer) Do(req *http.Request) (*http.Response, error) {
	secrets, _ := req.Context().Value(sessionSecretsKey{}).(*sessionSecrets)
	if secrets != nil && secrets.requestURL != nil {
		req = req.Clone(req.Context())
		req.URL = secrets.requestURL
	}
	res, err := c.inner.Do(req)
	err = redactSessionError(err)
	if res == nil || secrets == nil {
		return res, err
	}
	if raw := res.Header.Get(sessionURLHeader); raw != "" {
		if parsed, parseErr := url.Parse(raw); parseErr == nil {
			if masked, ok := redactSessionID(parsed); ok {
				secrets.sessionURL = raw
				res.Header.Set(sessionURLHeader, masked.String())
			}
		}
	}
	return res, err
}
