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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/client"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/sdkerrors"
	"github.com/google-gemini/gemini-api-cli/internal/sdk"
	"github.com/google-gemini/gemini-api-cli/internal/testclient"
	"github.com/spf13/cobra"
)

const defaultHeartbeatInterval = 5 * time.Second

// LeasedInteraction represents an active interaction lease returned by LeaseInteractions.
type LeasedInteraction struct {
	InteractionID       string          `json:"interaction_id"`
	EnvironmentID       string          `json:"environment_id,omitempty"`
	LeaseToken          string          `json:"lease_token"`
	LeaseGeneration     int64           `json:"lease_generation,omitempty"`
	LeaseExpirationTime string          `json:"lease_expiration_time,omitempty"`
	EnvironmentConfig   json.RawMessage `json:"environment_config,omitempty"`
}

// UnmarshalJSON decodes LeasedInteraction, accepting ProtoJSON string or numeric lease_generation.
func (l *LeasedInteraction) UnmarshalJSON(data []byte) error {
	type alias LeasedInteraction
	var aux struct {
		alias
		LeaseGeneration json.RawMessage `json:"lease_generation"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*l = LeasedInteraction(aux.alias)
	gen, err := parseProtoInt64(aux.LeaseGeneration)
	if err != nil {
		return fmt.Errorf("invalid lease_generation: %w", err)
	}
	l.LeaseGeneration = gen
	return nil
}

func parseProtoInt64(raw json.RawMessage) (int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return 0, err
		}
		if s == "" {
			return 0, nil
		}
		return strconv.ParseInt(s, 10, 64)
	}
	var n int64
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return 0, err
	}
	return n, nil
}

type activeInteractionLease struct {
	InteractionID string `json:"interaction_id"`
	LeaseToken    string `json:"lease_token,omitempty"`
}

type leaseInteractionsRequest struct {
	WorkerID           string                   `json:"worker_id,omitempty"`
	ActiveLeases       []activeInteractionLease `json:"active_leases,omitempty"`
	MaxNewInteractions *int32                   `json:"max_new_interactions,omitempty"`
	EnvironmentID      string                   `json:"environment_id,omitempty"`
}

type leaseInteractionsResponse struct {
	Leases      []*LeasedInteraction `json:"leases"`
	PollBackoff string               `json:"poll_backoff,omitempty"`
}

// HeartbeatResult summarizes the result of a lease renewal request during worker handoff.
type HeartbeatResult struct {
	Renewed      bool
	ShouldCancel bool
}

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

var leaseTokenJSONPattern = regexp.MustCompile(`("lease_token"\s*:\s*)"(?:\\.|[^"\\])*"`)

type leaseTokenRedactor struct {
	mu    sync.Mutex
	inner io.Writer
}

func (r *leaseTokenRedactor) Write(p []byte) (int, error) {
	redacted := leaseTokenJSONPattern.ReplaceAll(p, []byte(`${1}"[REDACTED]"`))
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.inner.Write(redacted); err != nil {
		return 0, err
	}
	return len(p), nil
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
			if err := validateHeaderSecret("--header", val); err != nil {
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
		redactor := &leaseTokenRedactor{inner: cmd.ErrOrStderr()}
		cmd.SetErr(redactor)
		rt.debug = true
		rt.errOut = redactor
	}

	baseHTTP := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		},
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
	case rt.hasTestClient || isDryRun(cmd):
		rt.streamClient = rt.httpClient
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

func validateHeaderSecret(flagName, val string) error {
	for _, r := range val {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains control characters", flagName)
		}
	}
	return nil
}

func (rt *runtimeTransport) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	fullURL := rt.baseURL + "/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", rawUserAgent)
	for k, v := range rt.headers {
		req.Header.Set(k, v)
	}
	switch {
	case rt.apiKey != "":
		req.Header.Set("x-goog-api-key", rt.apiKey)
	case rt.accessToken != "":
		req.Header.Set("Authorization", "Bearer "+rt.accessToken)
	}
	if rt.userProject != "" {
		req.Header.Set("x-goog-user-project", rt.userProject)
	}
	if rt.apiRevision != "" {
		req.Header.Set("Api-Revision", rt.apiRevision)
	}
	return req, nil
}

// HTTPClientOptions configures an environment runtime HTTP/gRPC client.
type HTTPClientOptions struct {
	Transport      *runtimeTransport
	EnvironmentID  string
	EnvironmentKey string
}

// HTTPClient communicates with the Gemini API LeaseInteractions and bidi stream endpoints.
type HTTPClient struct {
	rt             *runtimeTransport
	environmentID  string
	environmentKey string
}

// NewHTTPClient creates an HTTPClient bound to opts.
func NewHTTPClient(opts HTTPClientOptions) *HTTPClient {
	return &HTTPClient{
		rt:             opts.Transport,
		environmentID:  strings.TrimSpace(opts.EnvironmentID),
		environmentKey: strings.TrimSpace(opts.EnvironmentKey),
	}
}

// ServerURL returns the normalized base URL configured on the client.
func (c *HTTPClient) ServerURL() string {
	return c.rt.baseURL
}

func (c *HTTPClient) applyEnvironmentHeaders(req *http.Request, envID, envKey, leaseToken string) {
	if envKey != "" {
		req.Header.Set("x-goog-environment-key", envKey)
	}
	if envID != "" {
		req.Header.Set("x-goog-environment-id", envID)
	}
	if leaseToken != "" {
		req.Header.Set("x-goog-lease-token", leaseToken)
	}
}

// LeaseInteractions polls POST /<apiVersion>/environments:leaseInteractions for queued work.
func (c *HTTPClient) LeaseInteractions(ctx context.Context, workerID string, maxNew int32) ([]*LeasedInteraction, error) {
	parsed, err := c.doLeaseRPC(ctx, c.environmentID, leaseInteractionsRequest{
		WorkerID:           workerID,
		MaxNewInteractions: &maxNew,
		EnvironmentID:      c.environmentID,
	}, "LeaseInteractions failed")
	if err != nil {
		return nil, err
	}
	return parsed.Leases, nil
}

// HeartbeatInteraction renews a lease over HTTP while a poller hands work off to a worker.
func (c *HTTPClient) HeartbeatInteraction(ctx context.Context, pollerID, interactionID, envID, leaseToken string) (*HeartbeatResult, error) {
	zero := int32(0)
	resolvedEnvID := firstNonEmpty(envID, c.environmentID)
	parsed, err := c.doLeaseRPC(ctx, resolvedEnvID, leaseInteractionsRequest{
		WorkerID: pollerID,
		ActiveLeases: []activeInteractionLease{{
			InteractionID: interactionID,
			LeaseToken:    leaseToken,
		}},
		MaxNewInteractions: &zero,
		EnvironmentID:      resolvedEnvID,
	}, "LeaseInteractions heartbeat failed")
	if err != nil {
		var sdkErr *sdkerrors.SDKDefaultError
		if errors.As(err, &sdkErr) {
			switch sdkErr.StatusCode {
			case http.StatusNotFound, http.StatusConflict, http.StatusPreconditionFailed:
				return &HeartbeatResult{ShouldCancel: true}, nil
			}
		}
		return nil, err
	}
	for _, l := range parsed.Leases {
		if l != nil && l.InteractionID == interactionID {
			return &HeartbeatResult{Renewed: true}, nil
		}
	}
	return &HeartbeatResult{ShouldCancel: true}, nil
}

func (c *HTTPClient) doLeaseRPC(ctx context.Context, envID string, reqBody leaseInteractionsRequest, errPrefix string) (*leaseInteractionsResponse, error) {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	reqCtx := ctx
	var cancel context.CancelFunc
	if c.rt.timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, c.rt.timeout)
		defer cancel()
	}
	req, err := c.rt.newRequest(reqCtx, http.MethodPost, c.rt.apiVersion+"/environments:leaseInteractions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyEnvironmentHeaders(req, envID, c.environmentKey, "")

	resp, err := c.rt.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, sdkerrors.NewSDKDefaultError(errPrefix, resp.StatusCode, string(body), resp)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return &leaseInteractionsResponse{}, nil
	}
	var parsed leaseInteractionsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decoding LeaseInteractions response: %w", err)
	}
	return &parsed, nil
}

type connectStreamRequest struct {
	InteractionID    string
	EnvironmentID    string
	LeaseToken       string
	NextOutputSeqNum int64
	AckOutputSeqNum  int64
}

func (c *HTTPClient) bidiStreamMethodPath() string {
	return fmt.Sprintf("google.learning.gemini.api.interactions.%s.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey", c.rt.apiVersion)
}

// PreviewConnect emits the dry-run preview for opening an interaction bidi stream without touching the network.
func (c *HTTPClient) PreviewConnect(ctx context.Context, leaseToken string) error {
	req, err := c.rt.newRequest(ctx, http.MethodPost, c.bidiStreamMethodPath(), http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	c.applyEnvironmentHeaders(req, c.environmentID, c.environmentKey, leaseToken)
	resp, err := c.rt.httpClient.Do(req)
	if err != nil {
		return err
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return nil
}

type grpcInteractionStream struct {
	resp   *http.Response
	pw     *io.PipeWriter
	sendMu sync.Mutex
	closed bool
}

func (s *grpcInteractionStream) Send(batch *clientEventBatch) error {
	payload := batch.marshalWire()
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	return grpcWriteFrame(s.pw, payload)
}

func (s *grpcInteractionStream) Recv() (*serverEventBatch, error) {
	frame, err := grpcReadFrame(s.resp.Body)
	if err != nil {
		if errors.Is(err, io.EOF) {
			if stErr := checkGRPCTrailers(s.resp); stErr != nil {
				return nil, stErr
			}
		}
		return nil, err
	}
	return unmarshalServerEventBatch(frame)
}

func (s *grpcInteractionStream) CloseSend() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.pw.Close()
}

func (s *grpcInteractionStream) Close() error {
	_ = s.CloseSend()
	if s.resp != nil && s.resp.Body != nil {
		return s.resp.Body.Close()
	}
	return nil
}

func (c *HTTPClient) connectStream(ctx context.Context, req connectStreamRequest) (*grpcInteractionStream, error) {
	pr, pw := io.Pipe()
	methodPath := c.bidiStreamMethodPath()
	httpReq, err := c.rt.newRequest(ctx, http.MethodPost, methodPath, pr)
	if err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/grpc")
	httpReq.Header.Set("TE", "trailers")
	c.applyEnvironmentHeaders(httpReq, firstNonEmpty(req.EnvironmentID, c.environmentID), c.environmentKey, req.LeaseToken)

	if c.rt.debug && c.rt.errOut != nil {
		_, _ = fmt.Fprintf(c.rt.errOut, "=== DEBUG STREAM ===\n%s %s\n", httpReq.Method, httpReq.URL.String())
	}

	handshake := (&clientEventBatch{
		AckOutputSeqNum: req.AckOutputSeqNum,
		ReconnectInteraction: &reconnectInteractionEvent{
			InteractionID:    req.InteractionID,
			NextOutputSeqNum: req.NextOutputSeqNum,
			ClientID:         "environment",
		},
	}).marshalWire()

	// Write the initial ReconnectInteraction frame immediately once the transport starts reading pr.
	writeErrCh := make(chan error, 1)
	go func() {
		writeErrCh <- grpcWriteFrame(pw, handshake)
	}()

	resp, err := c.rt.streamClient.Do(httpReq)
	if err != nil {
		_ = pw.CloseWithError(err)
		_ = pr.Close()
		if c.rt.scheme == "http" && ctx.Err() == nil {
			if httpErr := c.probePlainHTTPError(ctx, methodPath, req); httpErr != nil {
				return nil, httpErr
			}
		}
		return nil, err
	}

	if c.rt.debug && c.rt.errOut != nil {
		_, _ = fmt.Fprintf(c.rt.errOut, "=== DEBUG STREAM RESPONSE ===\n%s\n", resp.Status)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = pw.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		return nil, sdkerrors.NewSDKDefaultError("ConnectInteraction failed", resp.StatusCode, string(body), resp)
	}
	if stErr := checkGRPCHeaders(resp); stErr != nil {
		_ = pw.Close()
		_ = resp.Body.Close()
		return nil, stErr
	}
	if werr := <-writeErrCh; werr != nil {
		_ = pw.Close()
		_ = resp.Body.Close()
		return nil, werr
	}

	return &grpcInteractionStream{
		resp: resp,
		pw:   pw,
	}, nil
}

func (c *HTTPClient) probePlainHTTPError(ctx context.Context, methodPath string, req connectStreamRequest) error {
	probeReq, err := c.rt.newRequest(ctx, http.MethodPost, methodPath, http.NoBody)
	if err != nil {
		return nil
	}
	probeReq.Header.Set("Content-Type", "application/grpc")
	c.applyEnvironmentHeaders(probeReq, firstNonEmpty(req.EnvironmentID, c.environmentID), c.environmentKey, req.LeaseToken)
	resp, err := c.rt.httpClient.Do(probeReq)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return sdkerrors.NewSDKDefaultError("ConnectInteraction failed", resp.StatusCode, string(body), resp)
}

func checkGRPCHeaders(resp *http.Response) error {
	return parseGRPCStatus(resp.Header.Get("Grpc-Status"), resp.Header.Get("Grpc-Message"), resp)
}

func checkGRPCTrailers(resp *http.Response) error {
	return parseGRPCStatus(resp.Trailer.Get("Grpc-Status"), resp.Trailer.Get("Grpc-Message"), resp)
}

func parseGRPCStatus(rawCode, rawMsg string, resp *http.Response) error {
	trimmed := strings.TrimSpace(rawCode)
	if trimmed == "" || trimmed == "0" {
		return nil
	}
	code, err := strconv.Atoi(trimmed)
	if err != nil {
		code = 2
	}
	msg := decodeGRPCMessage(rawMsg)
	if msg == "" {
		msg = fmt.Sprintf("gRPC status %d", code)
	}
	httpStatus := grpcCodeToHTTPStatus(code)
	errBody, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    httpStatus,
			"message": msg,
		},
	})
	return sdkerrors.NewSDKDefaultError(msg, httpStatus, string(errBody), resp)
}

func grpcCodeToHTTPStatus(code int) int {
	switch code {
	case 3, 9, 11: // INVALID_ARGUMENT, FAILED_PRECONDITION, OUT_OF_RANGE
		return http.StatusBadRequest
	case 4: // DEADLINE_EXCEEDED
		return http.StatusGatewayTimeout
	case 5: // NOT_FOUND
		return http.StatusNotFound
	case 6, 10: // ALREADY_EXISTS, ABORTED
		return http.StatusConflict
	case 7: // PERMISSION_DENIED
		return http.StatusForbidden
	case 8: // RESOURCE_EXHAUSTED
		return http.StatusTooManyRequests
	case 12: // UNIMPLEMENTED
		return http.StatusNotImplemented
	case 14: // UNAVAILABLE
		return http.StatusServiceUnavailable
	case 16: // UNAUTHENTICATED
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func isNonRetryableControlPlaneError(err error) bool {
	var sdkErr *sdkerrors.SDKDefaultError
	if errors.As(err, &sdkErr) {
		switch sdkErr.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return true
		}
	}
	return false
}

func retryBackoff(base time.Duration, consecutive int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	var shift int
	if consecutive > 1 {
		shift = min(consecutive-1, 5)
	}
	return min(base*time.Duration(1<<shift), 30*time.Second)
}

func defaultRuntimeID(prefix string) string {
	host, _ := os.Hostname()
	if host == "" {
		host = "local"
	}
	return fmt.Sprintf("%s-%s-%d", prefix, host, os.Getpid())
}
