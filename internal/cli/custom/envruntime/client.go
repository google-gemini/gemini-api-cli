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

package envruntime

import (
	"bytes"
	"cmp"
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
	"sync/atomic"
	"time"
)

const (
	// EnvironmentKeyHeader is the HTTP header carrying a self-hosted environment key.
	EnvironmentKeyHeader = "x-goog-environment-key"

	// EnvironmentIDHeader is the HTTP header carrying an explicit environment identifier.
	EnvironmentIDHeader = "x-goog-environment-id"

	// APIKeyHeader is the HTTP header carrying a Gemini API key.
	APIKeyHeader = "x-goog-api-key"

	// LeaseTokenHeader is the HTTP header carrying an interaction lease token.
	LeaseTokenHeader = "x-goog-lease-token"

	// WorkerIDHeader is the HTTP header carrying the worker identifier.
	WorkerIDHeader = "x-goog-worker-id"

	// DefaultEnvironmentClientID is the client_id sent in ReconnectInteraction for environment workers.
	DefaultEnvironmentClientID = "environment"

	// DefaultHeartbeatInterval is the default in-band stream heartbeat interval (5s = 15s server TTL / 3).
	DefaultHeartbeatInterval = 5 * time.Second

	// DefaultLeaseTTL is the default lease duration requested in LeaseInteractions.
	DefaultLeaseTTL = 15 * time.Second

	// DefaultPollBackoff is the fallback backoff duration between LeaseInteractions requests.
	DefaultPollBackoff = 5 * time.Second

	// MaxControlPlaneBodyBytes caps HTTP control-plane error and JSON response reads at 32 MiB.
	MaxControlPlaneBodyBytes = 32 << 20
)

var (
	validIdentifierRe     = regexp.MustCompile(`^[a-zA-Z0-9._:\-]+$`)
	leaseTokenJSONPattern = regexp.MustCompile(`("(?:lease_token|environment_key)"\s*:\s*)"(?:\\.|[^"\\\r\n])*"?`)
)

// IsValidIdentifier reports whether s is a safe resource identifier without path traversal segments.
func IsValidIdentifier(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.Contains(s, "..") && validIdentifierRe.MatchString(s)
}

// ValidateHeaderSecret verifies that val contains no ASCII control characters.
func ValidateHeaderSecret(flagName, val string) error {
	for _, r := range val {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains control characters", flagName)
		}
	}
	return nil
}

// DefaultRuntimeID returns a default worker or poller identifier combining prefix, hostname, and PID.
func DefaultRuntimeID(prefix string) string {
	host, _ := os.Hostname()
	if host == "" {
		host = "local"
	}
	return fmt.Sprintf("%s-%s-%d", prefix, host, os.Getpid())
}

// HTTPDoer executes an *http.Request and is satisfied by *http.Client and CLI diagnostic wrappers.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// EnvironmentClient abstracts control-plane leasing and data-plane stream attachment for workers and pollers.
type EnvironmentClient interface {
	LeaseInteractions(ctx context.Context, req *LeaseInteractionsRequest) (*LeaseInteractionsResult, error)
	ConnectInteraction(ctx context.Context, req *ConnectInteractionRequest) (InteractionStream, error)
}

// InteractionStream represents an active bidirectional event stream for a leased interaction.
type InteractionStream interface {
	Send(ctx context.Context, batch *ClientEventBatch) error
	Recv(ctx context.Context) (*ServerEventBatch, error)
	CloseSend() error
	Close() error
}

// HTTPStreamDialer opens a custom bidirectional interaction stream for HTTPClient.
type HTTPStreamDialer func(ctx context.Context, endpoint string, headers http.Header, req *ConnectInteractionRequest) (InteractionStream, error)

// TransportError represents an HTTP or gRPC status failure returned by HTTPClient.
type TransportError struct {
	Op         string
	StatusCode int
	GRPCCode   int
	Message    string
	Body       string
	Resp       *http.Response
}

// Error implements error.
func (e *TransportError) Error() string {
	detail := cmp.Or(strings.TrimSpace(e.Body), e.Message, http.StatusText(e.StatusCode))
	if e.Op != "" {
		return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.StatusCode, detail)
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, detail)
	}
	return detail
}

// LeaseEntry represents an active interaction lease held by a worker or poller for renewal.
type LeaseEntry struct {
	InteractionID string `json:"interaction_id"`
	LeaseToken    string `json:"lease_token,omitempty"`
}

// LeaseInteractionsRequest contains parameters for heartbeating active leases and claiming new ones.
type LeaseInteractionsRequest struct {
	EnvironmentID      string
	EnvironmentKey     string
	WorkerID           string
	ActiveLeases       []LeaseEntry
	MaxNewInteractions int32
	SendZeroMaxNew     bool
	// RequestedLeaseTTL and Labels are forwarded by custom or gRPC EnvironmentClient
	// implementations and omitted by HTTPClient over REST.
	RequestedLeaseTTL time.Duration
	Labels            map[string]string
}

// EnvironmentSource represents a source mounted into an environment.
type EnvironmentSource struct {
	Type     string `json:"type,omitempty"`
	Source   string `json:"source,omitempty"`
	Target   string `json:"target,omitempty"`
	Content  string `json:"content,omitempty"`
	Encoding string `json:"encoding,omitempty"`
}

// EnvironmentEgressRule represents a domain egress rule in an EnvironmentConfig network allowlist.
type EnvironmentEgressRule struct {
	Domain     string            `json:"domain,omitempty"`
	Transform  map[string]string `json:"transform,omitempty"`
	Credential string            `json:"credential,omitempty"`
	Mode       string            `json:"mode,omitempty"`
}

// EnvironmentEnvVar represents an environment variable value or credential reference.
type EnvironmentEnvVar struct {
	Value      string `json:"value,omitempty"`
	Credential string `json:"credential,omitempty"`
}

// EnvironmentConfig represents the sandbox environment configuration attached to a LeasedInteraction.
type EnvironmentConfig struct {
	NetworkMode      string                       `json:"network_mode,omitempty"`
	NetworkAllowlist []EnvironmentEgressRule      `json:"network_allowlist,omitempty"`
	AllowlistDomains []string                     `json:"allowlist_domains,omitempty"`
	DenylistDomains  []string                     `json:"denylist_domains,omitempty"`
	Sources          []EnvironmentSource          `json:"sources,omitempty"`
	Env              map[string]string            `json:"env,omitempty"`
	EnvVars          map[string]EnvironmentEnvVar `json:"env_vars,omitempty"`
	RawJSON          json.RawMessage              `json:"-"`
}

// LeasedInteraction represents an interaction lease granted or renewed by LeaseInteractions.
type LeasedInteraction struct {
	InteractionID          string
	EnvironmentID          string
	LeaseToken             string
	LeaseGeneration        int64
	LeaseExpirationTime    time.Time
	RawLeaseExpirationTime string
	EnvironmentKey         string
	EnvironmentConfig      *EnvironmentConfig
	RawEnvironmentConfig   json.RawMessage
	HeartbeatInterval      time.Duration
}

type leasedInteractionJSON struct {
	InteractionID       string          `json:"interaction_id"`
	EnvironmentID       string          `json:"environment_id,omitempty"`
	LeaseToken          string          `json:"lease_token"`
	LeaseGeneration     json.RawMessage `json:"lease_generation,omitempty"`
	LeaseExpirationTime string          `json:"lease_expiration_time,omitempty"`
	EnvironmentKey      string          `json:"environment_key,omitempty"`
	EnvironmentConfig   json.RawMessage `json:"environment_config,omitempty"`
}

// UnmarshalJSON decodes LeasedInteraction, accepting ProtoJSON string or numeric lease_generation.
func (l *LeasedInteraction) UnmarshalJSON(data []byte) error {
	var wire leasedInteractionJSON
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	gen, err := ParseProtoInt64(wire.LeaseGeneration)
	if err != nil {
		return fmt.Errorf("invalid lease_generation: %w", err)
	}
	var expTime time.Time
	if parsed, err := time.Parse(time.RFC3339Nano, wire.LeaseExpirationTime); err == nil {
		expTime = parsed
	}
	var rawCfg json.RawMessage
	if trimmed := bytes.TrimSpace(wire.EnvironmentConfig); string(trimmed) != "null" {
		rawCfg = append(json.RawMessage(nil), trimmed...)
	}
	*l = LeasedInteraction{
		InteractionID:          wire.InteractionID,
		EnvironmentID:          wire.EnvironmentID,
		LeaseToken:             wire.LeaseToken,
		LeaseGeneration:        gen,
		LeaseExpirationTime:    expTime,
		RawLeaseExpirationTime: wire.LeaseExpirationTime,
		EnvironmentKey:         wire.EnvironmentKey,
		EnvironmentConfig:      ParseEnvironmentConfig(rawCfg),
		RawEnvironmentConfig:   rawCfg,
	}
	return nil
}

// MarshalJSON encodes LeasedInteraction into JSON preserving raw environment_config bytes when present.
func (l LeasedInteraction) MarshalJSON() ([]byte, error) {
	expStr := l.RawLeaseExpirationTime
	if expStr == "" && !l.LeaseExpirationTime.IsZero() {
		expStr = l.LeaseExpirationTime.UTC().Format(time.RFC3339Nano)
	}
	rawCfg := l.RawEnvironmentConfig
	if len(rawCfg) == 0 && l.EnvironmentConfig != nil {
		if len(l.EnvironmentConfig.RawJSON) > 0 {
			rawCfg = l.EnvironmentConfig.RawJSON
		} else {
			encoded, err := json.Marshal(l.EnvironmentConfig)
			if err != nil {
				return nil, err
			}
			rawCfg = encoded
		}
	}
	var rawGen json.RawMessage
	if l.LeaseGeneration != 0 {
		rawGen = json.RawMessage(strconv.FormatInt(l.LeaseGeneration, 10))
	}
	return json.Marshal(leasedInteractionJSON{
		InteractionID:       l.InteractionID,
		EnvironmentID:       l.EnvironmentID,
		LeaseToken:          l.LeaseToken,
		LeaseGeneration:     rawGen,
		LeaseExpirationTime: expStr,
		EnvironmentKey:      l.EnvironmentKey,
		EnvironmentConfig:   rawCfg,
	})
}

// LeaseInteractionsResult holds leases returned by LeaseInteractions and the recommended poll backoff.
type LeaseInteractionsResult struct {
	Leases      []LeasedInteraction
	PollBackoff time.Duration
}

// ConnectInteractionRequest contains parameters for attaching to an interaction stream as a worker.
type ConnectInteractionRequest struct {
	InteractionID     string
	EnvironmentID     string
	EnvironmentKey    string
	LeaseToken        string
	WorkerID          string
	ClientID          string
	NextOutputSeqNum  int64
	AckOutputSeqNum   int64
	HeartbeatInterval time.Duration
}

// HeartbeatResult summarizes the result of a lease renewal request during worker handoff.
type HeartbeatResult struct {
	Renewed      bool
	ShouldCancel bool
}

// NewLeaseTokenRedactor wraps w so any JSON "lease_token" string value written to it is replaced with "[REDACTED]".
func NewLeaseTokenRedactor(w io.Writer) io.Writer {
	return &leaseTokenRedactor{inner: w}
}

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

// ParseProtoInt64 decodes a ProtoJSON int64 field encoded either as a JSON string or JSON number.
func ParseProtoInt64(raw json.RawMessage) (int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
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
		return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	}
	var n int64
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return 0, err
	}
	return n, nil
}

// ParseDurationRaw decodes a ProtoJSON duration string ("5s"), object ({seconds,nanos}), or numeric seconds.
func ParseDurationRaw(raw json.RawMessage, fallback time.Duration) time.Duration {
	trimmed := bytes.TrimSpace(raw)
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	var obj struct {
		Seconds int64 `json:"seconds"`
		Nanos   int64 `json:"nanos"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil && (obj.Seconds > 0 || obj.Nanos > 0) {
		return time.Duration(obj.Seconds)*time.Second + time.Duration(obj.Nanos)*time.Nanosecond
	}
	var sec float64
	if err := json.Unmarshal(trimmed, &sec); err == nil && sec > 0 {
		return time.Duration(sec * float64(time.Second))
	}
	return fallback
}

type rawEnvironmentConfigWire struct {
	NetworkMode      string                     `json:"network_mode"`
	NetworkAllowlist json.RawMessage            `json:"network_allowlist"`
	NetworkDenylist  json.RawMessage            `json:"network_denylist"`
	AllowlistDomains []string                   `json:"allowlist_domains"`
	DenylistDomains  []string                   `json:"denylist_domains"`
	Sources          []EnvironmentSource        `json:"sources"`
	Env              map[string]json.RawMessage `json:"env"`
	EnvVars          map[string]json.RawMessage `json:"env_vars"`
}

// ParseEnvironmentConfig parses rawCfg into an *EnvironmentConfig while retaining RawJSON.
func ParseEnvironmentConfig(rawCfg json.RawMessage) *EnvironmentConfig {
	raw := bytes.TrimSpace(rawCfg)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var parsed rawEnvironmentConfigWire
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return &EnvironmentConfig{
			RawJSON: append(json.RawMessage(nil), raw...),
		}
	}
	allowlist := parseNetworkAllowlist(parsed.NetworkAllowlist)
	envMap, envVars := parseEnvironmentEnvVars(parsed.Env, parsed.EnvVars)
	return &EnvironmentConfig{
		NetworkMode:      parsed.NetworkMode,
		NetworkAllowlist: allowlist,
		AllowlistDomains: parsed.AllowlistDomains,
		DenylistDomains:  parsed.DenylistDomains,
		Sources:          parsed.Sources,
		Env:              envMap,
		EnvVars:          envVars,
		RawJSON:          append(json.RawMessage(nil), raw...),
	}
}

func parseNetworkAllowlist(raw json.RawMessage) []EnvironmentEgressRule {
	trimmed := bytes.TrimSpace(raw)
	var wrapper struct {
		Allowlist []EnvironmentEgressRule `json:"allowlist"`
	}
	if err := json.Unmarshal(trimmed, &wrapper); err == nil {
		return wrapper.Allowlist
	}
	var directRules []EnvironmentEgressRule
	if err := json.Unmarshal(trimmed, &directRules); err == nil {
		return directRules
	}
	var domains []string
	if err := json.Unmarshal(trimmed, &domains); err == nil {
		var rules []EnvironmentEgressRule
		for _, d := range domains {
			if d != "" {
				rules = append(rules, EnvironmentEgressRule{Domain: d})
			}
		}
		return rules
	}
	return nil
}

func parseEnvironmentEnvVars(maps ...map[string]json.RawMessage) (map[string]string, map[string]EnvironmentEnvVar) {
	var envMap map[string]string
	var envVars map[string]EnvironmentEnvVar
	for _, m := range maps {
		for k, rawVal := range m {
			trimmed := bytes.TrimSpace(rawVal)
			if len(trimmed) == 0 || string(trimmed) == "null" {
				continue
			}
			var strVal string
			if err := json.Unmarshal(trimmed, &strVal); err == nil {
				if envMap == nil {
					envMap = make(map[string]string, len(m))
					envVars = make(map[string]EnvironmentEnvVar, len(m))
				}
				envMap[k] = strVal
				envVars[k] = EnvironmentEnvVar{Value: strVal}
				continue
			}
			var objVal EnvironmentEnvVar
			if err := json.Unmarshal(trimmed, &objVal); err == nil {
				if envMap == nil {
					envMap = make(map[string]string, len(m))
					envVars = make(map[string]EnvironmentEnvVar, len(m))
				}
				envVars[k] = objVal
				if objVal.Value != "" {
					envMap[k] = objVal.Value
				} else if objVal.Credential != "" {
					envMap[k] = objVal.Credential
				}
			}
		}
	}
	return envMap, envVars
}

// HTTPClientConfig configures an HTTPClient for control-plane REST and gRPC/HTTP2 streams.
type HTTPClientConfig struct {
	BaseURL                     string
	APIVersion                  string
	StreamAPIVersion            string
	APIKey                      string
	AccessToken                 string
	EnvironmentID               string
	EnvironmentKey              string
	WorkerID                    string
	UserProject                 string
	APIRevision                 string
	UserAgent                   string
	Headers                     map[string]string
	Timeout                     time.Duration
	HTTPDoer                    HTTPDoer
	StreamDoer                  HTTPDoer
	TLSConfig                   *tls.Config
	StreamDialer                HTTPStreamDialer
	Debug                       bool
	DebugOut                    io.Writer
	ProbePlainHTTPOnStreamError bool
	RequireCredentialsOnLease   bool
}

// HTTPClient implements EnvironmentClient over Gemini API REST and HTTP/2 gRPC bidi endpoints.
type HTTPClient struct {
	baseURL          string
	scheme           string
	apiVersion       string
	streamAPIVersion string
	apiKey           string
	accessToken      string
	environmentID    string
	environmentKey   string
	workerID         string
	userProject      string
	apiRevision      string
	userAgent        string
	headers          map[string]string
	timeout          time.Duration
	httpDoer         HTTPDoer
	streamDoer       HTTPDoer
	streamDialer     HTTPStreamDialer
	debug            bool
	debugOut         io.Writer
	probePlainHTTP   bool
	requireLeaseCred bool
}

func newDefaultHTTPTransport(tlsCfg *tls.Config) *http.Transport {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		tr := dt.Clone()
		tr.Proxy = http.ProxyFromEnvironment
		tr.ForceAttemptHTTP2 = true
		tr.TLSClientConfig = tlsCfg
		return tr
	}
	return &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: true,
		TLSClientConfig:   tlsCfg,
	}
}

// NewHTTPClient creates an HTTPClient from cfg.
func NewHTTPClient(cfg HTTPClientConfig) *HTTPClient {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	var scheme string
	if u, err := url.Parse(baseURL); err == nil {
		scheme = strings.ToLower(u.Scheme)
	}
	apiVersion := strings.Trim(strings.TrimSpace(cfg.APIVersion), "/")
	if apiVersion == "" {
		apiVersion = "v1beta"
	}
	streamAPIVersion := strings.Trim(strings.TrimSpace(cfg.StreamAPIVersion), "/")
	if streamAPIVersion == "" {
		streamAPIVersion = apiVersion
	}

	var tlsCfg *tls.Config
	if cfg.TLSConfig != nil {
		tlsCfg = cfg.TLSConfig.Clone()
	} else {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	httpDoer := cfg.HTTPDoer
	if httpDoer == nil {
		httpDoer = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     newDefaultHTTPTransport(tlsCfg),
		}
	}

	streamDoer := cmp.Or(cfg.StreamDoer, httpDoer)
	if cfg.StreamDoer == nil && scheme == "http" {
		var h2cProtos http.Protocols
		h2cProtos.SetUnencryptedHTTP2(true)
		streamDoer = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Protocols: &h2cProtos,
				DialContext: (&net.Dialer{
					Timeout:   15 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		}
	}

	copiedHeaders := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		copiedHeaders[k] = v
	}

	return &HTTPClient{
		baseURL:          baseURL,
		scheme:           scheme,
		apiVersion:       apiVersion,
		streamAPIVersion: streamAPIVersion,
		apiKey:           cfg.APIKey,
		accessToken:      cfg.AccessToken,
		environmentID:    strings.TrimSpace(cfg.EnvironmentID),
		environmentKey:   strings.TrimSpace(cfg.EnvironmentKey),
		workerID:         strings.TrimSpace(cfg.WorkerID),
		userProject:      cfg.UserProject,
		apiRevision:      cfg.APIRevision,
		userAgent:        cfg.UserAgent,
		headers:          copiedHeaders,
		timeout:          cfg.Timeout,
		httpDoer:         httpDoer,
		streamDoer:       streamDoer,
		streamDialer:     cfg.StreamDialer,
		debug:            cfg.Debug,
		debugOut:         cmp.Or[io.Writer](cfg.DebugOut, os.Stderr),
		probePlainHTTP:   cfg.ProbePlainHTTPOnStreamError,
		requireLeaseCred: cfg.RequireCredentialsOnLease,
	}
}

// ServerURL returns the normalized base URL configured on the client.
func (c *HTTPClient) ServerURL() string {
	return c.baseURL
}

// EndpointURL joins path onto BaseURL/<APIVersion>.
func (c *HTTPClient) EndpointURL(path string) string {
	return c.baseURL + "/" + c.apiVersion + "/" + strings.TrimLeft(path, "/")
}

// BidiStreamMethodPath returns the gRPC HTTP/2 path for CreateInteractionBidiStreamByEnvironmentKey.
func (c *HTTPClient) BidiStreamMethodPath() string {
	return fmt.Sprintf("google.learning.gemini.api.interactions.%s.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey", c.streamAPIVersion)
}

// BuildHeaders constructs standard authentication and diagnostic headers for an outbound request.
func (c *HTTPClient) BuildHeaders(envID, envKey, leaseToken string) http.Header {
	h := make(http.Header)
	if c.userAgent != "" {
		h.Set("User-Agent", c.userAgent)
	}
	switch {
	case c.apiKey != "":
		h.Set(APIKeyHeader, c.apiKey)
	case c.accessToken != "":
		h.Set("Authorization", "Bearer "+c.accessToken)
	}
	if c.userProject != "" {
		h.Set("x-goog-user-project", c.userProject)
	}
	if c.apiRevision != "" {
		h.Set("Api-Revision", c.apiRevision)
	}
	hasCustomAuth := false
	hasCustomAPIKey := false
	for k, v := range c.headers {
		if http.CanonicalHeaderKey(k) == "Authorization" {
			hasCustomAuth = true
		}
		if strings.EqualFold(k, APIKeyHeader) {
			hasCustomAPIKey = true
		}
		h.Set(k, v)
	}
	if hasCustomAuth && !hasCustomAPIKey {
		h.Del(APIKeyHeader)
	}
	if envKey != "" {
		h.Set(EnvironmentKeyHeader, envKey)
	}
	if envID != "" {
		h.Set(EnvironmentIDHeader, envID)
	}
	if leaseToken != "" {
		h.Set(LeaseTokenHeader, leaseToken)
	}
	return h
}

func (c *HTTPClient) newRequest(ctx context.Context, method, path, envID, envKey, leaseToken string, body io.Reader) (*http.Request, error) {
	fullURL := c.baseURL + "/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	for k, vals := range c.BuildHeaders(envID, envKey, leaseToken) {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// DoJSON sends a JSON HTTP request to BaseURL/<relPath> and decodes any 2xx JSON body into respOut.
func (c *HTTPClient) DoJSON(ctx context.Context, method, relPath, envID, envKey, leaseToken, errPrefix string, reqBody, respOut any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		payload, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshaling request JSON: %w", err)
		}
		bodyReader = bytes.NewReader(payload)
	}

	reqCtx := ctx
	var cancel context.CancelFunc
	if c.timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	req, err := c.newRequest(reqCtx, method, relPath, envID, envKey, leaseToken, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpDoer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxControlPlaneBodyBytes+1))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(body) > MaxControlPlaneBodyBytes {
			body = body[:MaxControlPlaneBodyBytes]
		}
		return &TransportError{
			Op:         errPrefix,
			StatusCode: resp.StatusCode,
			GRPCCode:   HTTPStatusToGRPCCode(resp.StatusCode),
			Message:    errPrefix,
			Body:       string(body),
			Resp:       resp,
		}
	}
	if len(body) > MaxControlPlaneBodyBytes {
		return fmt.Errorf("%s response exceeds %d-byte limit", cmp.Or(errPrefix, "HTTP JSON"), MaxControlPlaneBodyBytes)
	}
	if respOut != nil && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, respOut); err != nil {
			return fmt.Errorf("decoding %s response: %w", cmp.Or(errPrefix, "HTTP JSON"), err)
		}
	}
	return nil
}

type httpLeaseInteractionsBody struct {
	WorkerID           string       `json:"worker_id,omitempty"`
	ActiveLeases       []LeaseEntry `json:"active_leases,omitempty"`
	MaxNewInteractions *int32       `json:"max_new_interactions,omitempty"`
	EnvironmentID      string       `json:"environment_id,omitempty"`
	// NOTE: requested_lease_ttl and labels are not part of the public REST schema
	// and are rejected by the HTTP endpoint, so HTTPClient intentionally omits them.
}

type httpLeaseInteractionsResponse struct {
	Leases      []LeasedInteraction `json:"leases"`
	PollBackoff json.RawMessage     `json:"poll_backoff,omitempty"`
}

// LeaseInteractions heartbeats active leases and claims new queued interactions via POST /<apiVersion>/environments:leaseInteractions.
func (c *HTTPClient) LeaseInteractions(ctx context.Context, req *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
	return c.doLeaseRPC(ctx, req, "LeaseInteractions failed")
}

func (c *HTTPClient) doLeaseRPC(ctx context.Context, req *LeaseInteractionsRequest, errPrefix string) (*LeaseInteractionsResult, error) {
	if req == nil {
		req = &LeaseInteractionsRequest{}
	}
	envID := cmp.Or(req.EnvironmentID, c.environmentID)
	envKey := cmp.Or(req.EnvironmentKey, c.environmentKey)
	if c.requireLeaseCred && envKey == "" && c.apiKey == "" && c.accessToken == "" {
		return nil, &TransportError{
			Op:         errPrefix,
			StatusCode: http.StatusBadRequest,
			GRPCCode:   3,
			Message:    "environment key or API key is required for LeaseInteractions",
		}
	}

	var maxNewPtr *int32
	if req.MaxNewInteractions > 0 || req.SendZeroMaxNew {
		v := req.MaxNewInteractions
		maxNewPtr = &v
	}

	wireReq := httpLeaseInteractionsBody{
		WorkerID:           cmp.Or(req.WorkerID, c.workerID),
		ActiveLeases:       req.ActiveLeases,
		MaxNewInteractions: maxNewPtr,
		EnvironmentID:      envID,
	}

	var parsed httpLeaseInteractionsResponse
	if err := c.DoJSON(ctx, http.MethodPost, c.apiVersion+"/environments:leaseInteractions", envID, envKey, "", errPrefix, wireReq, &parsed); err != nil {
		return nil, err
	}

	leases := make([]LeasedInteraction, 0, len(parsed.Leases))
	for _, item := range parsed.Leases {
		if item.InteractionID == "" {
			continue
		}
		if item.EnvironmentID == "" && envID != "" {
			item.EnvironmentID = envID
		}
		leases = append(leases, item)
	}

	return &LeaseInteractionsResult{
		Leases:      leases,
		PollBackoff: ParseDurationRaw(parsed.PollBackoff, DefaultPollBackoff),
	}, nil
}

// LeaseSimple claims up to maxNew queued interactions for workerID.
func (c *HTTPClient) LeaseSimple(ctx context.Context, workerID string, maxNew int32) ([]*LeasedInteraction, error) {
	res, err := c.LeaseInteractions(ctx, &LeaseInteractionsRequest{
		WorkerID:           workerID,
		MaxNewInteractions: maxNew,
		SendZeroMaxNew:     true,
		EnvironmentID:      c.environmentID,
		EnvironmentKey:     c.environmentKey,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*LeasedInteraction, 0, len(res.Leases))
	for i := range res.Leases {
		cp := res.Leases[i]
		out = append(out, &cp)
	}
	return out, nil
}

// HeartbeatInteraction renews a single interaction lease over HTTP with max_new_interactions=0.
func (c *HTTPClient) HeartbeatInteraction(ctx context.Context, pollerID, interactionID, envID, leaseToken string) (*HeartbeatResult, error) {
	resolvedEnvID := cmp.Or(envID, c.environmentID)
	res, err := c.doLeaseRPC(ctx, &LeaseInteractionsRequest{
		WorkerID:       pollerID,
		EnvironmentID:  resolvedEnvID,
		EnvironmentKey: c.environmentKey,
		ActiveLeases: []LeaseEntry{{
			InteractionID: interactionID,
			LeaseToken:    leaseToken,
		}},
		MaxNewInteractions: 0,
		SendZeroMaxNew:     true,
	}, "LeaseInteractions heartbeat failed")
	if err != nil {
		var te *TransportError
		if errors.As(err, &te) {
			switch te.StatusCode {
			case http.StatusNotFound, http.StatusConflict, http.StatusPreconditionFailed:
				return &HeartbeatResult{ShouldCancel: true}, nil
			}
		}
		return nil, err
	}
	for _, l := range res.Leases {
		if l.InteractionID == interactionID {
			return &HeartbeatResult{Renewed: true}, nil
		}
	}
	return &HeartbeatResult{ShouldCancel: true}, nil
}

// PreviewConnect sends a dry-run preview request for CreateInteractionBidiStreamByEnvironmentKey without opening a stream pipe.
func (c *HTTPClient) PreviewConnect(ctx context.Context, leaseToken string) error {
	req, err := c.newRequest(ctx, http.MethodPost, c.BidiStreamMethodPath(), c.environmentID, c.environmentKey, leaseToken, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	resp, err := c.httpDoer.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// ConnectInteraction opens a bidirectional interaction event stream and sends the initial ReconnectInteraction handshake.
func (c *HTTPClient) ConnectInteraction(ctx context.Context, req *ConnectInteractionRequest) (InteractionStream, error) {
	if req == nil || strings.TrimSpace(req.InteractionID) == "" {
		return nil, &TransportError{
			Op:         "ConnectInteraction failed",
			StatusCode: http.StatusBadRequest,
			GRPCCode:   3,
			Message:    "interaction ID is required",
		}
	}
	envID := cmp.Or(req.EnvironmentID, c.environmentID)
	envKey := cmp.Or(req.EnvironmentKey, c.environmentKey)
	workerID := cmp.Or(req.WorkerID, c.workerID)
	clientID := cmp.Or(req.ClientID, DefaultEnvironmentClientID)
	nextOut := req.NextOutputSeqNum
	if nextOut <= 0 {
		nextOut = 1
	}

	if c.streamDialer != nil {
		headers := c.BuildHeaders(envID, envKey, req.LeaseToken)
		if workerID != "" {
			headers.Set(WorkerIDHeader, workerID)
		}
		endpoint := c.EndpointURL("/interactions/" + url.PathEscape(req.InteractionID) + ":stream")
		stream, err := c.streamDialer(ctx, endpoint, headers, &ConnectInteractionRequest{
			InteractionID:     req.InteractionID,
			EnvironmentID:     envID,
			EnvironmentKey:    envKey,
			LeaseToken:        req.LeaseToken,
			WorkerID:          workerID,
			ClientID:          clientID,
			NextOutputSeqNum:  nextOut,
			AckOutputSeqNum:   req.AckOutputSeqNum,
			HeartbeatInterval: req.HeartbeatInterval,
		})
		if err != nil {
			return nil, err
		}
		if req.HeartbeatInterval > 0 {
			return NewHeartbeatStream(ctx, stream, req.HeartbeatInterval, req.AckOutputSeqNum), nil
		}
		return stream, nil
	}

	pr, pw := io.Pipe()
	methodPath := c.BidiStreamMethodPath()
	httpReq, err := c.newRequest(ctx, http.MethodPost, methodPath, envID, envKey, req.LeaseToken, pr)
	if err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/grpc")
	httpReq.Header.Set("TE", "trailers")
	if workerID != "" {
		httpReq.Header.Set(WorkerIDHeader, workerID)
	}

	if c.debug {
		_, _ = fmt.Fprintf(c.debugOut, "=== DEBUG STREAM ===\n%s %s\n", httpReq.Method, httpReq.URL.String())
	}

	handshake := MarshalClientEventBatch(&ClientEventBatch{
		AckOutputSeqNum: req.AckOutputSeqNum,
		ReconnectInteraction: &ReconnectInteractionEvent{
			InteractionID:    req.InteractionID,
			NextOutputSeqNum: nextOut,
			ClientID:         clientID,
		},
	})

	writeErrCh := make(chan error, 1)
	go func() {
		writeErrCh <- GRPCWriteFrame(pw, handshake)
	}()

	resp, err := c.streamDoer.Do(httpReq)
	if err != nil {
		_ = pw.CloseWithError(err)
		_ = pr.Close()
		if c.probePlainHTTP && c.scheme == "http" && ctx.Err() == nil {
			if httpErr := c.probePlainHTTPError(ctx, methodPath, envID, envKey, req.LeaseToken); httpErr != nil {
				return nil, httpErr
			}
		}
		return nil, err
	}

	if c.debug {
		_, _ = fmt.Fprintf(c.debugOut, "=== DEBUG STREAM RESPONSE ===\n%s\n", resp.Status)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = pr.Close()
		_ = pw.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		return nil, &TransportError{
			Op:         "ConnectInteraction failed",
			StatusCode: resp.StatusCode,
			GRPCCode:   HTTPStatusToGRPCCode(resp.StatusCode),
			Message:    "ConnectInteraction failed",
			Body:       string(body),
			Resp:       resp,
		}
	}
	if stErr := checkGRPCHeaders(resp); stErr != nil {
		_ = pr.Close()
		_ = pw.Close()
		_ = resp.Body.Close()
		return nil, stErr
	}
	select {
	case werr := <-writeErrCh:
		if werr != nil {
			_ = pw.Close()
			if !errors.Is(werr, io.ErrClosedPipe) {
				_ = resp.Body.Close()
				return nil, werr
			}
			// The server closed its read side early (for example, returning gRPC
			// trailers without draining the request stream); keep resp.Body open so
			// Recv surfaces buffered frames and trailer status codes.
		}
	case <-ctx.Done():
		_ = pw.CloseWithError(ctx.Err())
		_ = pr.Close()
		_ = resp.Body.Close()
		return nil, ctx.Err()
	}

	var stream InteractionStream = &grpcInteractionStream{
		streamCtx: ctx,
		resp:      resp,
		pw:        pw,
	}
	if req.HeartbeatInterval > 0 {
		stream = NewHeartbeatStream(ctx, stream, req.HeartbeatInterval, req.AckOutputSeqNum)
	}
	return stream, nil
}

func (c *HTTPClient) probePlainHTTPError(ctx context.Context, methodPath, envID, envKey, leaseToken string) error {
	probeReq, err := c.newRequest(ctx, http.MethodPost, methodPath, envID, envKey, leaseToken, http.NoBody)
	if err != nil {
		return nil
	}
	probeReq.Header.Set("Content-Type", "application/grpc")
	resp, err := c.httpDoer.Do(probeReq)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return &TransportError{
		Op:         "ConnectInteraction failed",
		StatusCode: resp.StatusCode,
		GRPCCode:   HTTPStatusToGRPCCode(resp.StatusCode),
		Message:    "ConnectInteraction failed",
		Body:       string(body),
		Resp:       resp,
	}
}

type grpcInteractionStream struct {
	streamCtx context.Context
	resp      *http.Response
	pw        *io.PipeWriter
	sendMu    sync.Mutex
}

func (s *grpcInteractionStream) Send(ctx context.Context, batch *ClientEventBatch) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	payload := MarshalClientEventBatch(batch)
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if ctx != nil {
		stop := context.AfterFunc(ctx, func() {
			_ = s.pw.CloseWithError(ctx.Err())
		})
		defer stop()
	}
	err := GRPCWriteFrame(s.pw, payload)
	if err != nil && ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (s *grpcInteractionStream) Recv(ctx context.Context) (*ServerEventBatch, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if ctx != nil && ctx != s.streamCtx {
		stop := context.AfterFunc(ctx, func() {
			_ = s.Close()
		})
		defer stop()
	}
	frame, err := GRPCReadFrame(s.resp.Body)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, io.EOF) {
			if stErr := checkGRPCTrailers(s.resp); stErr != nil {
				return nil, stErr
			}
		}
		return nil, err
	}
	return UnmarshalServerEventBatch(frame)
}

func (s *grpcInteractionStream) CloseSend() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.pw.Close()
}

func (s *grpcInteractionStream) Close() error {
	_ = s.pw.CloseWithError(io.ErrClosedPipe)
	return s.resp.Body.Close()
}

// SendHeartbeat transmits an AckOutputSeqNum-only ClientEventBatch over stream to renew the server-side lease.
func SendHeartbeat(ctx context.Context, stream InteractionStream, ackOutputSeqNum int64) error {
	if stream == nil {
		return fmt.Errorf("interaction stream is nil")
	}
	return stream.Send(ctx, &ClientEventBatch{
		AckOutputSeqNum: ackOutputSeqNum,
	})
}

// HeartbeatStream wraps an InteractionStream to send periodic in-band AckOutputSeqNum frames.
type HeartbeatStream struct {
	inner     InteractionStream
	hbCtx     context.Context
	hbCancel  context.CancelFunc
	sendMu    sync.Mutex
	ackSeq    atomic.Int64
	closing   atomic.Bool
	stoppedWg sync.WaitGroup
}

// NewHeartbeatStream wraps inner with a background ticker sending SendHeartbeat every interval.
func NewHeartbeatStream(ctx context.Context, inner InteractionStream, interval time.Duration, initialAckSeqNum int64) *HeartbeatStream {
	hbCtx, hbCancel := context.WithCancel(ctx)
	hs := &HeartbeatStream{
		inner:    inner,
		hbCtx:    hbCtx,
		hbCancel: hbCancel,
	}
	hs.ackSeq.Store(initialAckSeqNum)
	if interval <= 0 {
		return hs
	}
	hs.stoppedWg.Add(1)
	go func() {
		defer hs.stoppedWg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				hs.sendMu.Lock()
				if hs.closing.Load() {
					hs.sendMu.Unlock()
					return
				}
				err := SendHeartbeat(hbCtx, hs.inner, hs.ackSeq.Load())
				hs.sendMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
	return hs
}

// Send updates the tracked AckOutputSeqNum and forwards batch to the underlying stream.
func (s *HeartbeatStream) Send(ctx context.Context, batch *ClientEventBatch) error {
	if batch != nil && batch.AckOutputSeqNum > 0 {
		for {
			cur := s.ackSeq.Load()
			if batch.AckOutputSeqNum <= cur || s.ackSeq.CompareAndSwap(cur, batch.AckOutputSeqNum) {
				break
			}
		}
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.inner.Send(ctx, batch)
}

// Recv reads the next ServerEventBatch from the underlying stream and advances the tracked AckOutputSeqNum.
func (s *HeartbeatStream) Recv(ctx context.Context) (*ServerEventBatch, error) {
	batch, err := s.inner.Recv(ctx)
	if batch != nil {
		for _, ev := range batch.Events {
			if ev != nil && ev.SeqNum > 0 {
				for {
					cur := s.ackSeq.Load()
					if ev.SeqNum <= cur || s.ackSeq.CompareAndSwap(cur, ev.SeqNum) {
						break
					}
				}
			}
		}
	}
	if err != nil {
		s.hbCancel()
	}
	return batch, err
}

// CloseSend stops the background heartbeat ticker and half-closes the underlying stream.
func (s *HeartbeatStream) CloseSend() error {
	s.closing.Store(true)
	s.sendMu.Lock()
	s.hbCancel()
	err := s.inner.CloseSend()
	s.sendMu.Unlock()
	s.stoppedWg.Wait()
	return err
}

// Close stops the background heartbeat ticker and closes the underlying stream.
func (s *HeartbeatStream) Close() error {
	s.closing.Store(true)
	s.hbCancel()
	err := s.inner.Close()
	s.stoppedWg.Wait()
	return err
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
	msg := DecodeGRPCMessage(rawMsg)
	if msg == "" {
		msg = fmt.Sprintf("gRPC status %d", code)
	}
	httpStatus := GRPCCodeToHTTPStatus(code)
	errBody, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    httpStatus,
			"message": msg,
		},
	})
	return &TransportError{
		Op:         msg,
		StatusCode: httpStatus,
		GRPCCode:   code,
		Message:    msg,
		Body:       string(errBody),
		Resp:       resp,
	}
}

// GRPCCodeToHTTPStatus maps a canonical gRPC status code to its HTTP status equivalent.
func GRPCCodeToHTTPStatus(code int) int {
	switch code {
	case 3, 9, 11:
		return http.StatusBadRequest
	case 4:
		return http.StatusGatewayTimeout
	case 5:
		return http.StatusNotFound
	case 6, 10:
		return http.StatusConflict
	case 7:
		return http.StatusForbidden
	case 8:
		return http.StatusTooManyRequests
	case 12:
		return http.StatusNotImplemented
	case 14:
		return http.StatusServiceUnavailable
	case 16:
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

// HTTPStatusToGRPCCode maps an HTTP status code to its canonical gRPC status code integer.
func HTTPStatusToGRPCCode(statusCode int) int {
	switch statusCode {
	case http.StatusBadRequest:
		return 3
	case http.StatusUnauthorized:
		return 16
	case http.StatusForbidden:
		return 7
	case http.StatusNotFound:
		return 5
	case http.StatusConflict:
		return 10
	case http.StatusTooManyRequests:
		return 8
	case http.StatusNotImplemented:
		return 12
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return 14
	default:
		return 13
	}
}

// IsNonRetryableStatus reports whether statusCode represents a non-retryable 400/401/403/404 control-plane HTTP status.
func IsNonRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// IsNonRetryableError reports whether err represents a non-retryable 400/401/403/404 control-plane error.
func IsNonRetryableError(err error) bool {
	var te *TransportError
	return errors.As(err, &te) && IsNonRetryableStatus(te.StatusCode)
}

// RetryBackoff computes bounded exponential backoff capped at 30 seconds.
func RetryBackoff(base time.Duration, consecutive int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	var shift int
	if consecutive > 1 {
		shift = min(consecutive-1, 5)
	}
	return min(base*time.Duration(1<<shift), 30*time.Second)
}
