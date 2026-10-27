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
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/cli/custom/envruntime"
	"github.com/google-gemini/gemini-api-cli/internal/client"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/sdkerrors"
	"github.com/google-gemini/gemini-api-cli/internal/sdk"
	"github.com/google-gemini/gemini-api-cli/internal/testclient"
	"github.com/spf13/cobra"
)

// LeasedInteraction aliases envruntime.LeasedInteraction for CLI runtime commands.
type LeasedInteraction = envruntime.LeasedInteraction

// HeartbeatResult aliases envruntime.HeartbeatResult for CLI runtime commands.
type HeartbeatResult = envruntime.HeartbeatResult

type runtimeTransport struct {
	baseURL       string
	scheme        string
	apiVersion    string
	apiKey        string
	accessToken   string
	userProject   string
	apiRevision   string
	headers       map[string]string
	timeout       time.Duration
	httpClient    client.HTTPClient
	streamClient  client.HTTPClient
	hasTestClient bool
	debug         bool
	errOut        io.Writer
}

func newRuntimeTransport(cmd *cobra.Command, explicitRuntimeCred bool) (*runtimeTransport, error) {
	rt := &runtimeTransport{headers: make(map[string]string)}

	if serverURL, _ := flagutil.GetStringFlag(cmd, "server-url"); serverURL != "" {
		if err := validateRuntimeServerURL(serverURL); err != nil {
			return nil, err
		}
		rt.baseURL = strings.TrimRight(serverURL, "/")
	} else {
		rt.baseURL = strings.TrimRight(sdk.ServerList[0], "/")
	}
	parsedBase, err := url.Parse(rt.baseURL)
	if err != nil {
		return nil, usageError(fmt.Sprintf("invalid --server-url %q: %v", rt.baseURL, err))
	}
	rt.scheme = strings.ToLower(parsedBase.Scheme)

	apiVersionExplicit := flagutil.FlagChanged(cmd, "api-version")
	if apiVersionExplicit {
		rt.apiVersion, _ = flagutil.GetStringFlag(cmd, "api-version")
	} else if v := config.GetString("api-version"); v != "" {
		rt.apiVersion = v
	} else {
		rt.apiVersion, _ = flagutil.GetStringFlag(cmd, "api-version")
	}
	if rt.apiVersion == "" && !apiVersionExplicit {
		rt.apiVersion = "v1beta"
	}
	if !validAPIVersion(rt.apiVersion) {
		return nil, usageError("--api-version must be non-empty, contain only letters, numbers, '.', '_', or '-', and cannot be '.' or '..'")
	}

	apiKey, apiKeySource := config.ResolveRequestSecurityCredential(cmd, "api-key")
	accessToken, accessTokenSource := config.ResolveRequestSecurityCredential(cmd, "access-token")
	if config.CredentialSourceRank(apiKeySource) <= config.CredentialSourceRank(accessTokenSource) {
		rt.apiKey = apiKey
	} else {
		rt.accessToken = accessToken
	}
	flagutil.RecordCredentialsMissing(cmd, rt.apiKey == "" && rt.accessToken == "" && !explicitRuntimeCred, "")
	rt.userProject = resolveGlobalString(cmd, "user-project")
	rt.apiRevision = resolveGlobalString(cmd, "api-revision")

	if hdrs, _ := flagutil.GetStringArrayFlag(cmd, "header"); len(hdrs) > 0 {
		for _, h := range hdrs {
			k, v, ok := strings.Cut(h, ":")
			if !ok {
				return nil, usageError(fmt.Sprintf("invalid header format %q: expected \"Key: Value\"", h))
			}
			key := strings.TrimSpace(k)
			val := strings.TrimSpace(v)
			if key == "" || strings.ContainsAny(key, " \t\r\n\x00") {
				return nil, usageError(fmt.Sprintf("invalid header name %q", k))
			}
			if err := envruntime.ValidateHeaderSecret("--header", val); err != nil {
				return nil, usageError(err.Error())
			}
			rt.headers[http.CanonicalHeaderKey(key)] = val
		}
	}

	timeoutStr, _ := flagutil.GetStringFlag(cmd, "timeout")
	if timeoutStr == "" {
		timeoutStr = config.GetString("timeout")
	}
	if timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return nil, usageError(fmt.Sprintf("invalid --timeout value %q: %v", timeoutStr, err))
		}
		rt.timeout = d
	}

	if client.IsDebug(cmd) {
		redactor := envruntime.NewLeaseTokenRedactor(cmd.ErrOrStderr())
		cmd.SetErr(redactor)
		rt.debug = true
		rt.errOut = redactor
	}

	var baseTransport *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		baseTransport = dt.Clone()
		baseTransport.Proxy = http.ProxyFromEnvironment
		baseTransport.ForceAttemptHTTP2 = true
		baseTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		baseTransport = &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		}
	}
	baseHTTP := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     baseTransport,
	}

	var wrapped client.HTTPClient = baseHTTP
	if tc := testclient.NewTestHTTPClient(); tc != nil {
		wrapped = tc
		rt.hasTestClient = true
	}
	rt.httpClient = client.WrapClientForDiagnostics(cmd, wrapped)

	// Bidirectional gRPC streams stream frames over an open io.Pipe body and must
	// NOT be wrapped in DebugClient (which calls io.ReadAll on req.Body and blocks).
	switch {
	case isDryRun(cmd):
		rt.streamClient = rt.httpClient
	case rt.hasTestClient:
		rt.streamClient = wrapped
	case rt.scheme == "http":
		var h2cProtos http.Protocols
		h2cProtos.SetUnencryptedHTTP2(true)
		rt.streamClient = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Protocols: &h2cProtos,
				DialContext: (&net.Dialer{
					Timeout:   15 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		}
	default:
		rt.streamClient = baseHTTP
	}

	return rt, nil
}

func validateRuntimeServerURL(raw string) error {
	if err := flagutil.ValidateServerURL(raw); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return usageError(fmt.Sprintf("invalid --server-url %q: %v", raw, err))
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return usageError(fmt.Sprintf("invalid --server-url %q: expected http(s)://host[:port][/prefix] without credentials, query, or fragment", raw))
	}
	return nil
}

func (rt *runtimeTransport) newEnvHTTPClient(envID, envKey string) *envruntime.HTTPClient {
	return envruntime.NewHTTPClient(envruntime.HTTPClientConfig{
		BaseURL:                     rt.baseURL,
		APIVersion:                  rt.apiVersion,
		APIKey:                      rt.apiKey,
		AccessToken:                 rt.accessToken,
		EnvironmentID:               envID,
		EnvironmentKey:              envKey,
		UserProject:                 rt.userProject,
		APIRevision:                 rt.apiRevision,
		UserAgent:                   rawUserAgent,
		Headers:                     rt.headers,
		Timeout:                     rt.timeout,
		HTTPDoer:                    rt.httpClient,
		StreamDoer:                  rt.streamClient,
		Debug:                       rt.debug,
		DebugOut:                    rt.errOut,
		ProbePlainHTTPOnStreamError: true,
	})
}

func (rt *runtimeTransport) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	fullURL := rt.baseURL + "/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	for k, vals := range rt.newEnvHTTPClient("", "").BuildHeaders("", "", "") {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// HTTPClientOptions configures a CLI environment runtime HTTP/gRPC client.
type HTTPClientOptions struct {
	Transport      *runtimeTransport
	EnvironmentID  string
	EnvironmentKey string
}

// HTTPClient adapts envruntime.HTTPClient to surface CLI sdkerrors.SDKDefaultError diagnostics.
type HTTPClient struct {
	inner *envruntime.HTTPClient
}

// NewHTTPClient creates an HTTPClient bound to opts.
func NewHTTPClient(opts HTTPClientOptions) *HTTPClient {
	var inner *envruntime.HTTPClient
	if opts.Transport != nil {
		inner = opts.Transport.newEnvHTTPClient(opts.EnvironmentID, opts.EnvironmentKey)
	} else {
		inner = envruntime.NewHTTPClient(envruntime.HTTPClientConfig{
			EnvironmentID:               opts.EnvironmentID,
			EnvironmentKey:              opts.EnvironmentKey,
			ProbePlainHTTPOnStreamError: true,
		})
	}
	return &HTTPClient{inner: inner}
}

// ServerURL returns the normalized base URL configured on the client.
func (c *HTTPClient) ServerURL() string {
	return c.inner.ServerURL()
}

// LeaseInteractions implements envruntime.EnvironmentClient, mapping transport errors to SDKDefaultError.
func (c *HTTPClient) LeaseInteractions(ctx context.Context, req *envruntime.LeaseInteractionsRequest) (*envruntime.LeaseInteractionsResult, error) {
	res, err := c.inner.LeaseInteractions(ctx, req)
	if err != nil {
		return nil, wrapTransportError(err)
	}
	return res, nil
}

// LeaseSimple polls POST /<apiVersion>/environments:leaseInteractions for up to maxNew queued interactions.
func (c *HTTPClient) LeaseSimple(ctx context.Context, workerID string, maxNew int32) ([]*LeasedInteraction, error) {
	leases, err := c.inner.LeaseSimple(ctx, workerID, maxNew)
	if err != nil {
		return nil, wrapTransportError(err)
	}
	return leases, nil
}

// HeartbeatInteraction renews a lease over HTTP while a poller hands work off to a worker.
func (c *HTTPClient) HeartbeatInteraction(ctx context.Context, pollerID, interactionID, envID, leaseToken string) (*HeartbeatResult, error) {
	res, err := c.inner.HeartbeatInteraction(ctx, pollerID, interactionID, envID, leaseToken)
	if err != nil {
		return nil, wrapTransportError(err)
	}
	return res, nil
}

// PreviewConnect emits the dry-run preview for opening an interaction bidi stream without touching the network.
func (c *HTTPClient) PreviewConnect(ctx context.Context, leaseToken string) error {
	return wrapTransportError(c.inner.PreviewConnect(ctx, leaseToken))
}

// ConnectInteraction implements envruntime.EnvironmentClient, wrapping HTTP and gRPC trailer errors into SDKDefaultError.
func (c *HTTPClient) ConnectInteraction(ctx context.Context, req *envruntime.ConnectInteractionRequest) (envruntime.InteractionStream, error) {
	stream, err := c.inner.ConnectInteraction(ctx, req)
	if err != nil {
		return nil, wrapTransportError(err)
	}
	return &sdkErrorStream{InteractionStream: stream}, nil
}

type sdkErrorStream struct {
	envruntime.InteractionStream
}

func (s *sdkErrorStream) Recv(ctx context.Context) (*envruntime.ServerEventBatch, error) {
	batch, err := s.InteractionStream.Recv(ctx)
	if err != nil {
		return nil, wrapTransportError(err)
	}
	return batch, nil
}

func wrapTransportError(err error) error {
	if err == nil {
		return nil
	}
	var te *envruntime.TransportError
	if errors.As(err, &te) {
		msg := cmp.Or(te.Op, te.Message)
		return sdkerrors.NewSDKDefaultError(msg, te.StatusCode, te.Body, te.Resp)
	}
	return err
}

func isNonRetryableControlPlaneError(err error) bool {
	if envruntime.IsNonRetryableError(err) {
		return true
	}
	var sdkErr *sdkerrors.SDKDefaultError
	return errors.As(err, &sdkErr) && envruntime.IsNonRetryableStatus(sdkErr.StatusCode)
}
