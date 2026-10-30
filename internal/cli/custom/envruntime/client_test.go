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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startH2CServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: handler, Protocols: &protos}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return "http://" + ln.Addr().String()
}

type failingWriter struct {
	err error
}

func (f failingWriter) Write([]byte) (int, error) {
	return 0, f.err
}

func TestIdentifiersSecretsRedactorAndParsers(t *testing.T) {
	for _, valid := range []string{"int_123", "env-abc.1:2"} {
		if !IsValidIdentifier(valid) {
			t.Errorf("IsValidIdentifier(%q) = false, want true", valid)
		}
	}
	for _, invalid := range []string{"", ".", "..", "a..b", "a/b", "bad id"} {
		if IsValidIdentifier(invalid) {
			t.Errorf("IsValidIdentifier(%q) = true, want false", invalid)
		}
	}

	if err := ValidateHeaderSecret("--key", "clean_secret_123"); err != nil {
		t.Errorf("ValidateHeaderSecret(clean) = %v, want nil", err)
	}
	for _, bad := range []string{"a\nb", "a\rb", "a\x00b", "a\x7fb"} {
		if err := ValidateHeaderSecret("--key", bad); err == nil {
			t.Errorf("ValidateHeaderSecret(%q) = nil, want error", bad)
		}
	}

	if id := DefaultRuntimeID("worker"); !strings.HasPrefix(id, "worker-") {
		t.Errorf("DefaultRuntimeID(worker) = %q, want prefix worker-", id)
	}

	te1 := &TransportError{Op: "LeaseInteractions", StatusCode: 403, Body: " forbidden "}
	if got := te1.Error(); got != "LeaseInteractions: HTTP 403: forbidden" {
		t.Errorf("te1.Error() = %q", got)
	}
	te2 := &TransportError{StatusCode: 503}
	if got := te2.Error(); !strings.Contains(got, "HTTP 503") {
		t.Errorf("te2.Error() = %q", got)
	}
	te3 := &TransportError{Message: "stream closed"}
	if got := te3.Error(); got != "stream closed" {
		t.Errorf("te3.Error() = %q", got)
	}

	var redBuf bytes.Buffer
	red := NewLeaseTokenRedactor(&redBuf)
	sample := `{"leases":[{"lease_token":"tok_\"secret\"_1"},{"lease_token": "tok_2"}],"environment_key":"ek_secret_1"}`
	if n, err := red.Write([]byte(sample)); err != nil || n != len(sample) {
		t.Fatalf("redactor.Write() = (%d, %v)", n, err)
	}
	if strings.Contains(redBuf.String(), "tok_") || strings.Contains(redBuf.String(), "ek_secret_1") || strings.Count(redBuf.String(), "[REDACTED]") != 3 {
		t.Errorf("redacted output = %q", redBuf.String())
	}
	redBuf.Reset()
	truncSample := `{"lease_token":"tok_truncated_without_quote`
	if _, err := red.Write([]byte(truncSample)); err != nil || strings.Contains(redBuf.String(), "tok_truncated") {
		t.Errorf("redactor truncated output = (%q, %v), want token redacted", redBuf.String(), err)
	}
	wErr := errors.New("disk full")
	if _, err := NewLeaseTokenRedactor(failingWriter{err: wErr}).Write([]byte(`{"lease_token":"x"}`)); !errors.Is(err, wErr) {
		t.Errorf("redactor write error = %v, want %v", err, wErr)
	}

	if n, err := ParseProtoInt64(json.RawMessage(`"42"`)); err != nil || n != 42 {
		t.Errorf("ParseProtoInt64(string) = (%d, %v)", n, err)
	}
	if n, err := ParseProtoInt64(json.RawMessage(`99`)); err != nil || n != 99 {
		t.Errorf("ParseProtoInt64(number) = (%d, %v)", n, err)
	}
	if n, err := ParseProtoInt64(json.RawMessage(`null`)); err != nil || n != 0 {
		t.Errorf("ParseProtoInt64(null) = (%d, %v)", n, err)
	}
	if n, err := ParseProtoInt64(json.RawMessage(`""`)); err != nil || n != 0 {
		t.Errorf("ParseProtoInt64(empty string) = (%d, %v)", n, err)
	}

	if d := ParseDurationRaw(json.RawMessage(`"3s"`), time.Second); d != 3*time.Second {
		t.Errorf("ParseDurationRaw(string) = %v, want 3s", d)
	}
	if d := ParseDurationRaw(nil, 4*time.Second); d != 4*time.Second {
		t.Errorf("ParseDurationRaw(nil) = %v, want 4s", d)
	}
	if d := ParseDurationRaw(json.RawMessage(`null`), 4*time.Second); d != 4*time.Second {
		t.Errorf("ParseDurationRaw(null) = %v, want 4s", d)
	}
	if d := ParseDurationRaw(json.RawMessage(`"-5s"`), 4*time.Second); d != 4*time.Second {
		t.Errorf("ParseDurationRaw(-5s) = %v, want 4s", d)
	}
	if d := ParseDurationRaw(json.RawMessage(`"0s"`), 4*time.Second); d != 4*time.Second {
		t.Errorf("ParseDurationRaw(0s) = %v, want 4s", d)
	}
	if d := ParseDurationRaw(json.RawMessage(`{"seconds":2,"nanos":500000000}`), time.Second); d != 2500*time.Millisecond {
		t.Errorf("ParseDurationRaw(obj) = %v, want 2.5s", d)
	}
	if d := ParseDurationRaw(json.RawMessage(`1.5`), time.Second); d != 1500*time.Millisecond {
		t.Errorf("ParseDurationRaw(float) = %v, want 1.5s", d)
	}
	if d := ParseDurationRaw(json.RawMessage(`-2`), 4*time.Second); d != 4*time.Second {
		t.Errorf("ParseDurationRaw(-2) = %v, want 4s", d)
	}
	if d := ParseDurationRaw(json.RawMessage(`0`), 4*time.Second); d != 4*time.Second {
		t.Errorf("ParseDurationRaw(0) = %v, want 4s", d)
	}
	if d := ParseDurationRaw(json.RawMessage(`"bad"`), 4*time.Second); d != 4*time.Second {
		t.Errorf("ParseDurationRaw(fallback) = %v, want 4s", d)
	}

	// LeasedInteraction JSON marshal/unmarshal & ParseEnvironmentConfig variants.
	rawLease := `{
		"interaction_id": "int_1",
		"environment_id": "env_1",
		"lease_token": "tok_1",
		"lease_generation": "7",
		"lease_expiration_time": "2026-10-26T12:00:00Z",
		"environment_config": {
			"network_mode": "ALLOWLIST",
			"network_allowlist": {"allowlist": [{"domain": "api.github.com"}]},
			"env": {"PLAIN": "v1"},
			"env_vars": {
				"CRED_KEY": {"credential": "cred_ref"},
				"OBJ_VAL": {"value": "val_obj"},
				"EMPTY_OBJ": {}
			}
		}
	}`
	var lease LeasedInteraction
	if err := json.Unmarshal([]byte(rawLease), &lease); err != nil {
		t.Fatalf("json.Unmarshal(LeasedInteraction) failed: %v", err)
	}
	wantExp := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if lease.LeaseGeneration != 7 || !lease.LeaseExpirationTime.Equal(wantExp) || lease.EnvironmentConfig == nil || len(lease.EnvironmentConfig.NetworkAllowlist) != 1 {
		t.Fatalf("parsed lease = %+v", lease)
	}
	if lease.EnvironmentConfig.Env["PLAIN"] != "v1" || lease.EnvironmentConfig.Env["CRED_KEY"] != "cred_ref" || lease.EnvironmentConfig.Env["OBJ_VAL"] != "val_obj" {
		t.Errorf("parsed Env = %v", lease.EnvironmentConfig.Env)
	}
	if _, hasEmpty := lease.EnvironmentConfig.Env["EMPTY_OBJ"]; hasEmpty {
		t.Errorf("expected EMPTY_OBJ to be omitted from Env map, got %v", lease.EnvironmentConfig.Env)
	}
	if lease.EnvironmentConfig.EnvVars["PLAIN"].Value != "v1" || lease.EnvironmentConfig.EnvVars["CRED_KEY"].Credential != "cred_ref" || lease.EnvironmentConfig.EnvVars["OBJ_VAL"].Value != "val_obj" {
		t.Errorf("parsed EnvVars = %+v", lease.EnvironmentConfig.EnvVars)
	}
	marshaled, err := json.Marshal(lease)
	if err != nil || !strings.Contains(string(marshaled), `"api.github.com"`) || !strings.Contains(string(marshaled), `"lease_generation":7`) {
		t.Errorf("json.Marshal(LeasedInteraction) = (%s, %v)", marshaled, err)
	}
	zeroGenBytes, err := json.Marshal(LeasedInteraction{InteractionID: "int_zero"})
	if err != nil || strings.Contains(string(zeroGenBytes), "lease_generation") {
		t.Errorf("json.Marshal(zero LeaseGeneration) = (%s, %v), want no lease_generation", zeroGenBytes, err)
	}
	var badExpLease LeasedInteraction
	if err := json.Unmarshal([]byte(`{"interaction_id":"int_bad_exp","lease_expiration_time":"not-a-timestamp"}`), &badExpLease); err != nil || !badExpLease.LeaseExpirationTime.IsZero() {
		t.Errorf("Unmarshal(invalid lease_expiration_time) = (%+v, %v), want zero time", badExpLease, err)
	}

	if ParseEnvironmentConfig(nil) != nil || ParseEnvironmentConfig(json.RawMessage(`null`)) != nil {
		t.Error("ParseEnvironmentConfig(nil/null) != nil")
	}
	malformedCfg := ParseEnvironmentConfig(json.RawMessage(`"not-an-object"`))
	if malformedCfg == nil || string(malformedCfg.RawJSON) != `"not-an-object"` {
		t.Errorf("ParseEnvironmentConfig(malformed) = %+v", malformedCfg)
	}
	domainCfg := ParseEnvironmentConfig(json.RawMessage(`{"network_allowlist":["example.com",""]}`))
	if domainCfg == nil || len(domainCfg.NetworkAllowlist) != 1 || domainCfg.NetworkAllowlist[0].Domain != "example.com" {
		t.Errorf("ParseEnvironmentConfig(domains) = %+v", domainCfg)
	}
	wantCustom := `{"atespace":"tenant-acme"}`
	customCfg := ParseEnvironmentConfig(json.RawMessage(`{"custom_config":{"atespace":"tenant-acme"}}`))
	if customCfg == nil || string(customCfg.CustomConfig) != wantCustom {
		t.Errorf("ParseEnvironmentConfig(custom_config) = %v, want %s", customCfg, wantCustom)
	}
	nullCustomCfg := ParseEnvironmentConfig(json.RawMessage(`{"custom_config":null}`))
	if nullCustomCfg == nil || nullCustomCfg.CustomConfig != nil {
		t.Errorf("ParseEnvironmentConfig(null custom_config) = %+v, want nil CustomConfig", nullCustomCfg)
	}

	// Status & retry helpers.
	for _, code := range []int{0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 14, 16, 99} {
		httpStatus := GRPCCodeToHTTPStatus(code)
		_ = HTTPStatusToGRPCCode(httpStatus)
	}
	if !IsNonRetryableStatus(http.StatusBadRequest) || IsNonRetryableStatus(http.StatusServiceUnavailable) {
		t.Error("IsNonRetryableStatus classification mismatch")
	}
	if !IsNonRetryableError(&TransportError{StatusCode: http.StatusUnauthorized}) {
		t.Error("IsNonRetryableError(401) = false, want true")
	}
	if IsNonRetryableError(&TransportError{StatusCode: http.StatusServiceUnavailable}) {
		t.Error("IsNonRetryableError(503) = true, want false")
	}
	if d := RetryBackoff(time.Second, 2); d != 2*time.Second {
		t.Errorf("RetryBackoff(1s, 2) = %v, want 2s", d)
	}
	if d := RetryBackoff(2*time.Second, 20); d != 30*time.Second {
		t.Errorf("RetryBackoff(capped) = %v, want 30s", d)
	}
	if err := parseGRPCStatus("", "", &http.Response{StatusCode: http.StatusOK}); err != nil {
		t.Errorf("parseGRPCStatus(empty) = %v, want nil", err)
	}
	if err := parseGRPCStatus("0", "", &http.Response{StatusCode: http.StatusOK}); err != nil {
		t.Errorf("parseGRPCStatus(0) = %v, want nil", err)
	}
	if err := parseGRPCStatus("3", "invalid", &http.Response{StatusCode: http.StatusOK}); err == nil {
		t.Error("parseGRPCStatus(3) = nil, want error")
	}
}

func TestHTTPClientLeaseHeartbeatPreviewAndStream(t *testing.T) {
	var heartbeatFrames atomic.Int32
	var lastPingUA atomic.Value
	srvURL := startH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1beta/ping":
			lastPingUA.Store(r.Header.Get("User-Agent"))
			reqBytes, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "" || len(reqBytes) != 0 {
				http.Error(w, "unexpected content-type or body on nil body", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
			if r.URL.Query().Get("body") == "json" {
				_, _ = w.Write([]byte(`{"ok":true}`))
			} else {
				_, _ = w.Write([]byte("   "))
			}
		case "/v1beta/environments:leaseInteractions":
			if r.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "missing application/json content-type", http.StatusBadRequest)
				return
			}
			if r.Header.Get(EnvironmentKeyHeader) == "ek_403" {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			if body["worker_id"] == "hb-conflict" {
				http.Error(w, "conflict", http.StatusConflict)
				return
			}
			if body["worker_id"] == "hb-empty" {
				_, _ = w.Write([]byte(`{"leases":[]}`))
				return
			}
			if body["worker_id"] == "hb-null" {
				_, _ = w.Write([]byte(`{"leases":[null]}`))
				return
			}
			if body["worker_id"] == "hb-ok" {
				if v, ok := body["max_new_interactions"]; !ok || v != float64(0) {
					http.Error(w, "expected max_new_interactions=0 on heartbeat", http.StatusBadRequest)
					return
				}
			}
			if body["worker_id"] == "omit-max-new" {
				if _, ok := body["max_new_interactions"]; ok {
					http.Error(w, "expected max_new_interactions omitted", http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(`{"leases":[]}`))
				return
			}
			if body["worker_id"] == "backoff-parse" {
				if r.Header.Get(EnvironmentIDHeader) != "env_inherited" {
					http.Error(w, "expected EnvironmentIDHeader", http.StatusBadRequest)
					return
				}
				if v, ok := body["max_new_interactions"]; !ok || v != float64(2) {
					http.Error(w, "expected max_new_interactions=2", http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(`{"leases":[{"interaction_id":"int_p1","lease_token":"tok_p1"},{"interaction_id":"int_p2","environment_id":"env_explicit","lease_token":"tok_p2"}],"poll_backoff":"1.5s"}`))
				return
			}
			_, _ = w.Write([]byte(`{"leases":[{"interaction_id":"int_hb","lease_token":"tok_hb"}]}`))

		case "/google.learning.gemini.api.interactions.v1beta.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey":
			if r.Header.Get("Content-Type") != "application/grpc" || r.Header.Get("TE") != "trailers" {
				http.Error(w, "missing grpc headers", http.StatusBadRequest)
				return
			}
			if r.Header.Get(LeaseTokenHeader) == "tok_grpc_hdr_err" {
				if len(r.Header.Values(WorkerIDHeader)) != 0 {
					http.Error(w, "unexpected worker id header", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/grpc")
				w.Header().Set("Grpc-Status", "7")
				w.Header().Set("Grpc-Message", "permission%20denied")
				w.WriteHeader(http.StatusOK)
				return
			}
			if r.Header.Get(LeaseTokenHeader) == "tok_grpc_trailer_err" {
				w.Header().Set("Content-Type", "application/grpc")
				w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				w.Header().Set("Grpc-Status", "14")
				w.Header().Set("Grpc-Message", "backend%20unavailable")
				return
			}
			if r.Header.Get(WorkerIDHeader) != "w-live" {
				http.Error(w, "missing worker id header", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/grpc")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			flusher.Flush()

			// Read initial handshake frame + at least one in-band heartbeat frame.
			frameIdx := 0
			for {
				frame, err := GRPCReadFrame(r.Body)
				if err != nil {
					return
				}
				frameIdx++
				batch, err := UnmarshalClientEventBatch(frame)
				if err != nil {
					return
				}
				if frameIdx == 1 {
					if batch.ReconnectInteraction == nil || batch.ReconnectInteraction.NextOutputSeqNum != 1 || batch.AckOutputSeqNum != 9 {
						return
					}
					continue
				}
				if batch.ReconnectInteraction == nil && batch.AckOutputSeqNum == 9 {
					if heartbeatFrames.Add(1) >= 1 {
						_ = GRPCWriteFrame(w, MarshalServerEventBatch(&ServerEventBatch{
							InteractionCompleted: &InteractionCompletedEvent{},
						}))
						flusher.Flush()
						return
					}
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))

	// Verify BuildHeaders, StreamAPIVersion, and custom StreamDoer.
	hdrClient := NewHTTPClient(HTTPClientConfig{
		BaseURL:          srvURL,
		APIVersion:       "v1beta",
		StreamAPIVersion: "v1alpha",
		UserAgent:        "ua-custom",
		AccessToken:      "tok-bearer",
	})
	if !strings.Contains(hdrClient.BidiStreamMethodPath(), ".v1alpha.") {
		t.Errorf("BidiStreamMethodPath() = %q, want .v1alpha.", hdrClient.BidiStreamMethodPath())
	}
	hFull := hdrClient.BuildHeaders("env-id-1", "ek-1", "lt-1")
	if hFull.Get("User-Agent") != "ua-custom" || hFull.Get("Authorization") != "Bearer tok-bearer" || hFull.Get(EnvironmentIDHeader) != "env-id-1" || hFull.Get(EnvironmentKeyHeader) != "ek-1" || hFull.Get(LeaseTokenHeader) != "lt-1" {
		t.Errorf("BuildHeaders(full) = %v", hFull)
	}
	hEmpty := hdrClient.BuildHeaders("", "", "")
	if hEmpty.Values(EnvironmentIDHeader) != nil || hEmpty.Values(EnvironmentKeyHeader) != nil || hEmpty.Values(LeaseTokenHeader) != nil {
		t.Errorf("BuildHeaders(empty) = %v, want no env/lease headers", hEmpty)
	}
	overrideHdrClient := NewHTTPClient(HTTPClientConfig{
		BaseURL:     srvURL,
		UserAgent:   "ua-default",
		APIKey:      "default-api-key",
		UserProject: "proj-default",
		APIRevision: "rev-default",
		Headers: map[string]string{
			"User-Agent":          "ua-override",
			"Authorization":       "Bearer custom-override",
			"x-goog-user-project": "proj-override",
			"Api-Revision":        "rev-override",
		},
	})
	hOverride := overrideHdrClient.BuildHeaders("", "", "")
	if hOverride.Get("User-Agent") != "ua-override" || hOverride.Get("Authorization") != "Bearer custom-override" || hOverride.Values("x-goog-api-key") != nil || hOverride.Get("x-goog-user-project") != "proj-override" || hOverride.Get("Api-Revision") != "rev-override" {
		t.Errorf("BuildHeaders(override) = %v", hOverride)
	}
	explicitDualCredClient := NewHTTPClient(HTTPClientConfig{
		BaseURL: srvURL,
		APIKey:  "default-api-key",
		Headers: map[string]string{
			"Authorization":  "Bearer custom-override",
			"x-goog-api-key": "explicit-hdr-key",
		},
	})
	if hDual := explicitDualCredClient.BuildHeaders("", "", ""); hDual.Get("Authorization") != "Bearer custom-override" || hDual.Get("x-goog-api-key") != "explicit-hdr-key" {
		t.Errorf("BuildHeaders(explicit dual headers) = %v", hDual)
	}

	oversizedJSONClient := NewHTTPClient(HTTPClientConfig{
		BaseURL: srvURL,
		HTTPDoer: httpDoerFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(strings.Repeat(" ", MaxControlPlaneBodyBytes+16))),
			}, nil
		}),
	})
	if err := oversizedJSONClient.DoJSON(t.Context(), http.MethodGet, "v1beta/oversized", "", "", "", "oversized", nil, nil); err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Errorf("DoJSON(>MaxControlPlaneBodyBytes) = %v, want response exceeds limit error", err)
	}

	var customStreamDoerCalled bool
	customStreamClient := NewHTTPClient(HTTPClientConfig{
		BaseURL: srvURL,
		StreamDoer: httpDoerFunc(func(_ *http.Request) (*http.Response, error) {
			customStreamDoerCalled = true
			return nil, errors.New("custom stream doer err")
		}),
	})
	if _, err := customStreamClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{InteractionID: "int_custom"}); err == nil || !customStreamDoerCalled {
		t.Errorf("ConnectInteraction(custom StreamDoer) = %v, called=%v", err, customStreamDoerCalled)
	}

	var dialReq *ConnectInteractionRequest
	dialClient := NewHTTPClient(HTTPClientConfig{
		BaseURL: srvURL,
		StreamDialer: func(_ context.Context, _ string, _ http.Header, req *ConnectInteractionRequest) (InteractionStream, error) {
			cp := *req
			dialReq = &cp
			return nil, errors.New("dialer stop")
		},
	})
	_, _ = dialClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{InteractionID: "int_d", NextOutputSeqNum: 5})
	if dialReq == nil || dialReq.NextOutputSeqNum != 5 {
		t.Errorf("ConnectInteraction(NextOutputSeqNum=5) = %+v, want 5", dialReq)
	}

	var debugBuf bytes.Buffer
	client := NewHTTPClient(HTTPClientConfig{
		BaseURL:                     srvURL + "/",
		EnvironmentKey:              "ek_live",
		UserProject:                 "proj-1",
		APIRevision:                 "rev-1",
		Headers:                     map[string]string{"X-Extra": "v"},
		Debug:                       true,
		DebugOut:                    NewLeaseTokenRedactor(&debugBuf),
		ProbePlainHTTPOnStreamError: true,
	})
	if client.ServerURL() != srvURL {
		t.Errorf("ServerURL() = %q, want %q", client.ServerURL(), srvURL)
	}
	if err := client.DoJSON(t.Context(), http.MethodGet, "v1beta/ping?body=json", "", "", "", "ping", nil, nil); err != nil {
		t.Errorf("DoJSON(nil body, nil respOut with json body) failed: %v", err)
	}
	if _, hasUA := client.BuildHeaders("", "", "")["User-Agent"]; hasUA {
		t.Errorf("BuildHeaders() set User-Agent when UserAgent config is empty: %v", client.BuildHeaders("", "", ""))
	}
	if gotPingUA, _ := lastPingUA.Load().(string); gotPingUA == "" {
		t.Error("expected Go default User-Agent header on wire when UserAgent config is empty")
	}
	var emptyOut map[string]any
	if err := client.DoJSON(t.Context(), http.MethodGet, "v1beta/ping", "", "", "", "ping", nil, &emptyOut); err != nil || emptyOut != nil {
		t.Errorf("DoJSON(nil body, non-nil respOut with whitespace body) = (%v, %v)", emptyOut, err)
	}

	// EnvironmentID inheritance + PollBackoff parsing + omit MaxNewInteractions when 0 and !SendZeroMaxNew.
	res, err := client.LeaseInteractions(t.Context(), &LeaseInteractionsRequest{
		EnvironmentID:      "env_inherited",
		WorkerID:           "backoff-parse",
		MaxNewInteractions: 2,
	})
	if err != nil || len(res.Leases) != 2 || res.Leases[0].InteractionID != "int_p1" || res.Leases[0].EnvironmentID != "env_inherited" || res.Leases[1].EnvironmentID != "env_explicit" || res.PollBackoff != 1500*time.Millisecond {
		t.Fatalf("LeaseInteractions(backoff-parse) = (%+v, %v)", res, err)
	}
	if _, err := client.LeaseInteractions(t.Context(), &LeaseInteractionsRequest{
		WorkerID:           "omit-max-new",
		MaxNewInteractions: 0,
		SendZeroMaxNew:     false,
	}); err != nil {
		t.Errorf("LeaseInteractions(omit-max-new) failed: %v", err)
	}

	// RequireCredentialsOnLease check.
	strictNoCred := NewHTTPClient(HTTPClientConfig{BaseURL: srvURL, RequireCredentialsOnLease: true})
	if _, err := strictNoCred.LeaseSimple(t.Context(), "w1", 1); err == nil {
		t.Error("LeaseSimple(RequireCredentialsOnLease without creds) = nil, want error")
	}

	// Heartbeat outcomes: renewed, empty->cancel, [null]->neither, 409->cancel.
	hbOK, err := client.HeartbeatInteraction(t.Context(), "hb-ok", "int_hb", "env_1", "tok_hb")
	if err != nil || !hbOK.Renewed || hbOK.ShouldCancel {
		t.Errorf("HeartbeatInteraction(ok) = (%+v, %v)", hbOK, err)
	}
	hbEmpty, err := client.HeartbeatInteraction(t.Context(), "hb-empty", "int_hb", "env_1", "tok_hb")
	if err != nil || hbEmpty.Renewed || !hbEmpty.ShouldCancel {
		t.Errorf("HeartbeatInteraction(empty) = (%+v, %v)", hbEmpty, err)
	}
	hbNull, err := client.HeartbeatInteraction(t.Context(), "hb-null", "int_hb", "env_1", "tok_hb")
	if err != nil || hbNull.Renewed || !hbNull.ShouldCancel {
		t.Errorf("HeartbeatInteraction(null) = (%+v, %v), want ShouldCancel=true", hbNull, err)
	}
	hbConflict, err := client.HeartbeatInteraction(t.Context(), "hb-conflict", "int_hb", "env_1", "tok_hb")
	if err != nil || hbConflict.Renewed || !hbConflict.ShouldCancel {
		t.Errorf("HeartbeatInteraction(409) = (%+v, %v)", hbConflict, err)
	}

	// 403 TransportError on LeaseSimple.
	deniedClient := NewHTTPClient(HTTPClientConfig{BaseURL: srvURL, EnvironmentKey: "ek_403"})
	if _, err := deniedClient.LeaseSimple(t.Context(), "w1", 1); err == nil || !IsNonRetryableError(err) {
		t.Errorf("LeaseSimple(403) = %v, want non-retryable TransportError", err)
	}

	// PreviewConnect invokes HTTPDoer with grpc headers and closes response body.
	var previewPath, previewCT, previewTE string
	var previewBodyClosed bool
	previewClient := NewHTTPClient(HTTPClientConfig{
		BaseURL:        srvURL,
		EnvironmentKey: "ek_prev",
		HTTPDoer: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
			previewPath = req.URL.Path
			previewCT = req.Header.Get("Content-Type")
			previewTE = req.Header.Get("TE")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &trackingReadCloser{Reader: strings.NewReader(""), onClose: func() { previewBodyClosed = true }},
			}, nil
		}),
	})
	if err := previewClient.PreviewConnect(t.Context(), "tok_prev"); err != nil || !strings.Contains(previewPath, "CreateInteractionBidiStreamByEnvironmentKey") || previewCT != "application/grpc" || previewTE != "trailers" || !previewBodyClosed {
		t.Errorf("PreviewConnect() = %v, path=%q ct=%q te=%q closed=%v", err, previewPath, previewCT, previewTE, previewBodyClosed)
	}

	// ConnectInteraction with in-band HeartbeatStream, gRPC header error, and gRPC trailer error.
	stream, err := client.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID:     "int_live",
		LeaseToken:        "tok_secret_stream",
		WorkerID:          "w-live",
		AckOutputSeqNum:   9,
		HeartbeatInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ConnectInteraction() failed: %v", err)
	}
	if err := stream.Send(t.Context(), &ClientEventBatch{
		ReconnectInteraction: &ReconnectInteractionEvent{InteractionID: "int_live", ClientID: DefaultEnvironmentClientID},
	}); err != nil {
		t.Fatalf("stream.Send() failed: %v", err)
	}
	evBatch, err := stream.Recv(t.Context())
	if err != nil || evBatch.InteractionCompleted == nil {
		t.Fatalf("stream.Recv() = (%+v, %v), want InteractionCompleted", evBatch, err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Errorf("stream.CloseSend() = %v", err)
	}
	if err := stream.Send(t.Context(), &ClientEventBatch{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("stream.Send() after CloseSend() = %v, want io.ErrClosedPipe", err)
	}
	_ = stream.Close()
	if heartbeatFrames.Load() < 1 {
		t.Errorf("heartbeatFrames = %d, want >= 1", heartbeatFrames.Load())
	}
	if !strings.Contains(debugBuf.String(), "=== DEBUG STREAM ===") || !strings.Contains(debugBuf.String(), "=== DEBUG STREAM RESPONSE ===") {
		t.Errorf("expected debug stream output in debugBuf, got %q", debugBuf.String())
	}
	if strings.Contains(debugBuf.String(), "tok_secret_stream") {
		t.Errorf("debug output leaked lease_token: %q", debugBuf.String())
	}

	// Verify grpcInteractionStream.Close() closes underlying resp.Body without deadlocking an in-flight blocked Send,
	// and HeartbeatStream CloseSend/Close wait for or unblock in-flight sends.
	var rawBodyClosed bool
	prBlocked, pwBlocked := io.Pipe()
	defer prBlocked.Close()
	rawStream := &grpcInteractionStream{
		resp: &http.Response{
			Body: &trackingReadCloser{Reader: strings.NewReader(""), onClose: func() { rawBodyClosed = true }},
		},
		pw: pwBlocked,
	}
	blockedSendErr := make(chan error, 1)
	go func() {
		blockedSendErr <- rawStream.Send(t.Context(), &ClientEventBatch{
			Events: []*ClientEvent{{SeqNum: 1, Payload: &EventPayload{SubInteractionID: strings.Repeat("x", 1024)}}},
		})
	}()
	time.Sleep(10 * time.Millisecond)
	if err := rawStream.Close(); err != nil || !rawBodyClosed {
		t.Errorf("rawStream.Close() = %v, rawBodyClosed=%v", err, rawBodyClosed)
	}
	select {
	case sErr := <-blockedSendErr:
		if !errors.Is(sErr, io.ErrClosedPipe) {
			t.Errorf("blocked rawStream.Send() after Close() = %v, want io.ErrClosedPipe", sErr)
		}
	case <-time.After(time.Second):
		t.Fatal("rawStream.Close() deadlocked or failed to unblock in-flight Send")
	}

	for _, useClose := range []bool{false, true} {
		sendEntered := make(chan struct{})
		var sendCompleted atomic.Bool
		var sendCalls atomic.Int32
		slowInner := &scriptedStream{
			sendFn: func(call int, _ *ClientEventBatch) error {
				sendCalls.Add(1)
				if call == 1 {
					close(sendEntered)
					time.Sleep(20 * time.Millisecond)
					sendCompleted.Store(true)
				}
				return nil
			},
		}
		hs := NewHeartbeatStream(t.Context(), slowInner, 10*time.Millisecond, 3)
		<-sendEntered
		if useClose {
			_ = hs.Close()
		} else {
			_ = hs.CloseSend()
		}
		if !sendCompleted.Load() {
			t.Errorf("HeartbeatStream stop (useClose=%v) returned before in-flight Send completed", useClose)
		}
		afterStopCount := sendCalls.Load()
		time.Sleep(25 * time.Millisecond)
		if sendCalls.Load() != afterStopCount {
			t.Errorf("HeartbeatStream (useClose=%v) continued sending after stop: before=%d after=%d", useClose, afterStopCount, sendCalls.Load())
		}
	}
	reqPr, reqPw := io.Pipe()
	respPr, respPw := io.Pipe()
	hbHalf := NewHeartbeatStream(t.Context(), &grpcInteractionStream{resp: &http.Response{Body: respPr}, pw: reqPw}, 5*time.Millisecond, 7)
	closeSendErr := make(chan error, 1)
	go func() {
		_, _ = reqPr.Read(make([]byte, 1))
		time.Sleep(12 * time.Millisecond)
		closeSendErr <- hbHalf.CloseSend()
	}()
	for !hbHalf.closing.Load() {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := io.ReadFull(reqPr, make([]byte, 6)); err != nil {
		t.Fatalf("ReadFull(in-flight heartbeat remainder) = %v", err)
	}
	if n, err := reqPr.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) || <-closeSendErr != nil {
		t.Errorf("reqPr.Read after CloseSend() = (%d, %v), want (0, io.EOF)", n, err)
	}
	_ = respPw.Close()
	_ = hbHalf.Close()

	var noDebugBuf bytes.Buffer
	noDebugClient := NewHTTPClient(HTTPClientConfig{
		BaseURL:        srvURL,
		EnvironmentKey: "ek_live",
		Debug:          false,
		DebugOut:       &noDebugBuf,
	})
	if _, err := noDebugClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID: "int_hdr",
		LeaseToken:    "tok_grpc_hdr_err",
	}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("ConnectInteraction(Grpc-Status header 7) = %v, want permission denied", err)
	}
	if noDebugBuf.Len() != 0 {
		t.Errorf("expected no debug output when Debug=false, got %q", noDebugBuf.String())
	}

	nilDebugOutClient := NewHTTPClient(HTTPClientConfig{
		BaseURL:        srvURL,
		EnvironmentKey: "ek_live",
		Debug:          true,
	})
	if nilDebugOutClient.debugOut != os.Stderr {
		t.Errorf("nilDebugOutClient.debugOut = %v, want os.Stderr", nilDebugOutClient.debugOut)
	}
	if _, err := nilDebugOutClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID: "int_hdr_default_debug",
		LeaseToken:    "tok_grpc_hdr_err",
	}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("nilDebugOutClient.ConnectInteraction(Debug=true, DebugOut=nil) = %v, want permission denied", err)
	}

	trStream, err := client.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID: "int_tr",
		LeaseToken:    "tok_grpc_trailer_err",
	})
	if err != nil {
		t.Fatalf("ConnectInteraction(trailer setup) failed: %v", err)
	}
	if _, err := trStream.Recv(t.Context()); err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("trStream.Recv(Grpc-Status trailer 14) = %v, want backend unavailable", err)
	}
	_ = trStream.Close()
}

type trackingReadCloser struct {
	io.Reader
	onClose func()
}

func (t *trackingReadCloser) Close() error {
	if t.onClose != nil {
		t.onClose()
	}
	return nil
}

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (f httpDoerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}
