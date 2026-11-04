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
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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
	teSecret := &TransportError{
		Op:         `Op with {"lease_token":"tok_secret_op"}`,
		StatusCode: 403,
		Body:       `{"error":"denied","lease_token":"tok_secret_body","environment_key":"ek_secret_body"}`,
	}
	if got := teSecret.Error(); strings.Contains(got, "tok_secret") || strings.Contains(got, "ek_secret") || !strings.Contains(got, "[REDACTED]") {
		t.Errorf("teSecret.Error() = %q, want secrets redacted", got)
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
	var pingPongReceived atomic.Bool
	cleanCloseCh := make(chan struct{}, 1)
	var lastPingUA atomic.Value
	wsUpgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
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

		case "/v1beta/interactions:streamForEnvironmentRuntime":
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
			if r.Header.Get(WorkerIDHeader) != "w-live" || r.Header.Get("X-Extra") != "v" || r.Header.Get("Sec-WebSocket-Protocol") != "" {
				http.Error(w, "invalid upgrade headers", http.StatusBadRequest)
				return
			}
			wsConn, err := wsUpgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer wsConn.Close()
			wsConn.SetPongHandler(func(appData string) error {
				if appData == "srv-ping" {
					pingPongReceived.Store(true)
				}
				return nil
			})

			// Read initial handshake frame + at least one in-band heartbeat frame, exercise Ping/Pong, and verify client CloseSend.
			frameIdx := 0
			for {
				_, frame, err := wsConn.ReadMessage()
				if err != nil {
					return
				}
				frameIdx++
				batch, err := UnmarshalClientEventBatchJSON(frame)
				if err != nil {
					return
				}
				if frameIdx == 1 {
					if batch.ReconnectInteraction == nil || batch.ReconnectInteraction.InteractionID != "int_live" || batch.ReconnectInteraction.NextOutputSeqNum != 1 {
						return
					}
					_ = wsConn.WriteControl(websocket.PingMessage, []byte("srv-ping"), time.Now().Add(time.Second))
					continue
				}
				if string(frame) == "{}" && batch.ReconnectInteraction == nil && len(batch.Events) == 0 {
					if heartbeatFrames.Add(1) >= 1 {
						srvBytes, _ := MarshalServerEventBatchJSON(&ServerEventBatch{
							InteractionCompleted: &InteractionCompletedEvent{},
						})
						_ = wsConn.WriteMessage(websocket.TextMessage, srvBytes)
						// Drain until client sends 1000 Close frame on CloseSend().
						for {
							if _, _, cErr := wsConn.ReadMessage(); websocket.IsCloseError(cErr, websocket.CloseNormalClosure) {
								select {
								case cleanCloseCh <- struct{}{}:
								default:
								}
								break
							} else if cErr != nil {
								break
							}
						}
						return
					}
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))

	// Verify BuildHeaders, StreamAPIVersion, and custom WSDialer.
	hdrClient := NewHTTPClient(HTTPClientConfig{
		BaseURL:          srvURL,
		APIVersion:       "v1beta",
		StreamAPIVersion: "v1alpha",
		UserAgent:        "ua-custom",
		AccessToken:      "tok-bearer",
	})
	if hdrClient.BidiStreamMethodPath() != "v1alpha/interactions:streamForEnvironmentRuntime" {
		t.Errorf("BidiStreamMethodPath() = %q, want v1alpha/interactions:streamForEnvironmentRuntime", hdrClient.BidiStreamMethodPath())
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

	var customWSDialerCalled bool
	customWSDialerClient := NewHTTPClient(HTTPClientConfig{
		BaseURL: srvURL,
		WSDialer: &websocket.Dialer{
			NetDialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				customWSDialerCalled = true
				return nil, errors.New("custom ws dialer err")
			},
		},
	})
	if _, err := customWSDialerClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{InteractionID: "int_custom"}); err == nil || !customWSDialerCalled {
		t.Errorf("ConnectInteraction(custom WSDialer) = (%v, called=%v), want (non-nil error, called=true)", err, customWSDialerCalled)
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
		BaseURL:        srvURL + "/",
		EnvironmentKey: "ek_live",
		UserProject:    "proj-1",
		APIRevision:    "rev-1",
		Headers: map[string]string{
			"X-Extra":                  "v",
			"Connection":               "keep-alive",
			"Upgrade":                  "h2c",
			"Sec-WebSocket-Key":        "dGhlIHNhbXBsZSBub25jZQ==",
			"Sec-WebSocket-Version":    "8",
			"Sec-WebSocket-Extensions": "permessage-deflate",
			"Sec-WebSocket-Protocol":   "forbidden-proto",
		},
		Debug:    true,
		DebugOut: NewLeaseTokenRedactor(&debugBuf),
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

	// PreviewConnect invokes HTTPDoer with GET + WebSocket upgrade headers and closes response body.
	var previewMethod, previewPath, previewConn, previewUpgrade, previewVersion string
	var previewBodyClosed bool
	previewClient := NewHTTPClient(HTTPClientConfig{
		BaseURL:        srvURL,
		EnvironmentKey: "ek_prev",
		HTTPDoer: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
			previewMethod = req.Method
			previewPath = req.URL.Path
			previewConn = req.Header.Get("Connection")
			previewUpgrade = req.Header.Get("Upgrade")
			previewVersion = req.Header.Get("Sec-WebSocket-Version")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &trackingReadCloser{Reader: strings.NewReader(""), onClose: func() { previewBodyClosed = true }},
			}, nil
		}),
	})
	if err := previewClient.PreviewConnect(t.Context(), "tok_prev"); err != nil || previewMethod != http.MethodGet || previewPath != "/v1beta/interactions:streamForEnvironmentRuntime" || previewConn != "Upgrade" || previewUpgrade != "websocket" || previewVersion != "13" || !previewBodyClosed {
		t.Errorf("PreviewConnect(tok_prev) = (err=%v, method=%q, path=%q, conn=%q, upgrade=%q, ver=%q, closed=%v), want (nil, \"GET\", \"/v1beta/interactions:streamForEnvironmentRuntime\", \"Upgrade\", \"websocket\", \"13\", true)", err, previewMethod, previewPath, previewConn, previewUpgrade, previewVersion, previewBodyClosed)
	}
	for _, badStatus := range []int{http.StatusMultipleChoices, http.StatusForbidden} {
		status := badStatus
		previewErrClient := NewHTTPClient(HTTPClientConfig{
			BaseURL: srvURL,
			HTTPDoer: httpDoerFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: status,
					Body:       io.NopCloser(strings.NewReader("denied")),
				}, nil
			}),
		})
		var prevTE *TransportError
		if err := previewErrClient.PreviewConnect(t.Context(), "tok_prev"); !errors.As(err, &prevTE) || prevTE.StatusCode != status {
			t.Errorf("PreviewConnect(%d) = %v, want TransportError status %d", status, err, status)
		}
	}

	// ConnectInteraction with in-band HeartbeatStream, automatic handshake, Ping/Pong, CloseSend, gRPC header error, and gRPC trailer error.
	stream, err := client.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID:     "int_live",
		LeaseToken:        "tok_secret_stream",
		WorkerID:          "w-live",
		AckOutputSeqNum:   9,
		HeartbeatInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ConnectInteraction() = %v, want nil", err)
	}
	evBatch, err := stream.Recv(t.Context())
	if err != nil || evBatch.InteractionCompleted == nil {
		t.Fatalf("stream.Recv() = (%+v, %v), want InteractionCompleted", evBatch, err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Errorf("stream.CloseSend() = %v, want nil", err)
	}
	select {
	case <-cleanCloseCh:
	case <-time.After(time.Second):
		t.Error("stream.CloseSend() server observed 1000 Close frame = false, want true")
	}
	if err := stream.Send(t.Context(), &ClientEventBatch{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("stream.Send() after CloseSend() = %v, want io.ErrClosedPipe", err)
	}
	_ = stream.Close()
	if heartbeatFrames.Load() < 1 {
		t.Errorf("heartbeatFrames = %d, want >= 1", heartbeatFrames.Load())
	}
	if !pingPongReceived.Load() {
		t.Error("ConnectInteraction(int_live) PingHandler Pong received = false, want true")
	}
	if !strings.Contains(debugBuf.String(), "=== DEBUG STREAM ===") || !strings.Contains(debugBuf.String(), "=== DEBUG STREAM RESPONSE ===") {
		t.Errorf("ConnectInteraction(Debug=true) debugBuf = %q, want DEBUG STREAM headers", debugBuf.String())
	}
	if strings.Contains(debugBuf.String(), "tok_secret_stream") {
		t.Errorf("debug output leaked lease_token: %q", debugBuf.String())
	}

	// Verify HeartbeatStream CloseSend/Close wait for in-flight sends before returning and stop subsequent ticks.
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

	if _, err := client.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID: "int_tr",
		LeaseToken:    "tok_grpc_trailer_err",
	}); err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("ConnectInteraction(Grpc-Status trailer 14) = %v, want backend unavailable", err)
	}
}

func newTestWSPair(t *testing.T, hook *hookedNetConnConfig) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	srvConnCh := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		srvConnCh <- sc
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialer := &websocket.Dialer{}
	var hooked *hookedNetConn
	if hook != nil {
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			hooked = &hookedNetConn{Conn: raw, cfg: hook}
			return hooked, nil
		}
	}
	cliConn, resp, err := dialer.DialContext(t.Context(), wsURL, nil)
	if err != nil {
		t.Fatalf("newTestWSPair DialContext failed: %v", err)
	}
	_ = resp.Body.Close()
	srvConn := <-srvConnCh
	if hooked != nil {
		hooked.enabled.Store(true)
	}
	t.Cleanup(func() {
		_ = cliConn.Close()
		_ = srvConn.Close()
	})
	return cliConn, srvConn
}

type hookedNetConnConfig struct {
	readErr      atomic.Value
	writeErr     atomic.Value
	writes       atomic.Int32
	closed       atomic.Bool
	closedCh     chan struct{}
	closeOnce    sync.Once
	onWrite      func()
	blockReads   bool
	blockWrites  bool
	readStarted  chan struct{}
	writeStarted chan struct{}
	unblock      chan struct{}
	readOnce     sync.Once
	writeOnce    sync.Once
}

func newBlockingHookConfig() *hookedNetConnConfig {
	return &hookedNetConnConfig{
		blockReads:   true,
		blockWrites:  true,
		closedCh:     make(chan struct{}),
		readStarted:  make(chan struct{}),
		writeStarted: make(chan struct{}),
		unblock:      make(chan struct{}),
	}
}

type hookedNetConn struct {
	net.Conn
	cfg     *hookedNetConnConfig
	enabled atomic.Bool
}

func (c *hookedNetConn) Read(p []byte) (int, error) {
	if c.enabled.Load() {
		if c.cfg.blockReads {
			c.cfg.readOnce.Do(func() { close(c.cfg.readStarted) })
			<-c.cfg.unblock
			return 0, io.EOF
		}
		if err, _ := c.cfg.readErr.Load().(error); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(p)
}

func (c *hookedNetConn) Write(p []byte) (int, error) {
	active := c.enabled.Load()
	if active {
		c.cfg.writes.Add(1)
		if c.cfg.blockWrites {
			c.cfg.writeOnce.Do(func() { close(c.cfg.writeStarted) })
			<-c.cfg.unblock
		}
		if err, _ := c.cfg.writeErr.Load().(error); err != nil {
			return 0, err
		}
	}
	n, err := c.Conn.Write(p)
	if active && c.cfg.onWrite != nil {
		c.cfg.onWrite()
	}
	return n, err
}

func (c *hookedNetConn) Close() error {
	c.cfg.closed.Store(true)
	err := c.Conn.Close()
	if c.cfg.closedCh != nil {
		c.cfg.closeOnce.Do(func() { close(c.cfg.closedCh) })
	}
	return err
}

func TestWSInteractionStreamCancellationConcurrencyAndTLS(t *testing.T) {
	// 0. Verify pre-canceled streamCtx during newWSInteractionStream does not race with Close().
	preCli, _ := newTestWSPair(t, nil)
	preCtx, cancelPre := context.WithCancel(t.Context())
	cancelPre()
	preWS := newWSInteractionStream(preCtx, preCli)
	if err := preWS.Close(); err != nil {
		t.Errorf("preWS.Close() with pre-canceled streamCtx = %v, want nil", err)
	}

	// 1. Verify streamCtx cancellation unblocks a blocked wsInteractionStream.Recv(streamCtx).
	cli1, _ := newTestWSPair(t, nil)
	streamCtx1, cancelStream1 := context.WithCancel(t.Context())
	ws1 := newWSInteractionStream(streamCtx1, cli1)
	recvErr1 := make(chan error, 1)
	go func() {
		_, err := ws1.Recv(streamCtx1)
		recvErr1 <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancelStream1()
	select {
	case err := <-recvErr1:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ws1.Recv(canceled streamCtx) = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ws1.Recv(streamCtx) did not unblock when streamCtx was canceled")
	}

	// 2. Verify child ctx cancellation unblocks wsInteractionStream.Recv(childCtx) while streamCtx is still active.
	cli2, _ := newTestWSPair(t, nil)
	ws2 := newWSInteractionStream(t.Context(), cli2)
	childCtx, cancelChild := context.WithTimeout(t.Context(), 15*time.Millisecond)
	defer cancelChild()
	if _, err := ws2.Recv(childCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ws2.Recv(childCtx timeout) = %v, want context.DeadlineExceeded", err)
	}

	// 3. Verify Close() unblocks a blocked wsInteractionStream.Send with io.ErrClosedPipe.
	blockSendCfg := newBlockingHookConfig()
	blockSendCfg.blockReads = false
	cli3, _ := newTestWSPair(t, blockSendCfg)
	ws3 := newWSInteractionStream(t.Context(), cli3)
	sendErr3 := make(chan error, 1)
	go func() {
		sendErr3 <- ws3.Send(t.Context(), &ClientEventBatch{})
	}()
	<-blockSendCfg.writeStarted
	_ = ws3.Close()
	close(blockSendCfg.unblock)
	select {
	case err := <-sendErr3:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("ws3.Send() after Close() = %v, want io.ErrClosedPipe", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ws3.Send() did not unblock on Close()")
	}

	// 4. Verify WebSocket handshake error branches (malformed 101 responses, 401 JSON error, 200 JSON non-101) over live HTTP sockets.
	handshakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get(LeaseTokenHeader) {
		case "tok_101_no_conn":
			conn, brw, _ := w.(http.Hijacker).Hijack()
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n")
			_ = brw.Flush()
		case "tok_101_no_upgrade":
			conn, brw, _ := w.(http.Hijacker).Hijack()
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\n\r\n")
			_ = brw.Flush()
		case "tok_101_bad_accept":
			conn, brw, _ := w.(http.Hijacker).Hijack()
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: invalid\r\n\r\n")
			_ = brw.Flush()
		case "tok_199_info":
			conn, brw, _ := w.(http.Hijacker).Hijack()
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 199 Informational\r\nContent-Length: 4\r\n\r\ninfo")
			_ = brw.Flush()
		case "tok_401_json":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"unauthenticated"}}`))
		case "tok_300_redirect":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusMultipleChoices)
			_, _ = w.Write([]byte("redirect"))
		case "tok_200_json":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "tok_299_ok":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(299)
			_, _ = w.Write([]byte(`{"ok":299}`))
		case "tok_200_grpc_header":
			w.Header().Set("Content-Type", "application/grpc")
			w.Header().Set("Grpc-Status", "7")
			w.Header().Set("Grpc-Message", "header-denied")
			w.WriteHeader(http.StatusOK)
		case "tok_200_grpc_trailer":
			w.Header().Set("Content-Type", "application/grpc")
			w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("body"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			w.Header().Set("Grpc-Status", "7")
			w.Header().Set("Grpc-Message", "trailer-denied")
		case "tok_200_grpc_body":
			w.Header().Set("Content-Type", "application/grpc")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("unexpected-grpc-body"))
		}
	}))
	defer handshakeSrv.Close()

	badHandshakeCases := []struct {
		token      string
		wantStatus int
		wantSub    string
		wantBody   string
	}{
		{token: "tok_101_no_conn", wantStatus: http.StatusBadGateway, wantSub: "invalid WebSocket 101 Switching Protocols response"},
		{token: "tok_101_no_upgrade", wantStatus: http.StatusBadGateway, wantSub: "invalid WebSocket 101 Switching Protocols response"},
		{token: "tok_101_bad_accept", wantStatus: http.StatusBadGateway, wantSub: "invalid WebSocket 101 Switching Protocols response"},
		{token: "tok_199_info", wantStatus: 199, wantSub: "ConnectInteraction failed"},
		{token: "tok_401_json", wantStatus: http.StatusUnauthorized, wantSub: "ConnectInteraction failed", wantBody: "unauthenticated"},
		{token: "tok_300_redirect", wantStatus: http.StatusMultipleChoices, wantSub: "ConnectInteraction failed", wantBody: "redirect"},
		{token: "tok_200_json", wantStatus: http.StatusBadGateway, wantSub: "expected HTTP 101 Switching Protocols", wantBody: `{"ok":true}`},
		{token: "tok_299_ok", wantStatus: http.StatusBadGateway, wantSub: "expected HTTP 101 Switching Protocols", wantBody: `{"ok":299}`},
		{token: "tok_200_grpc_header", wantStatus: http.StatusForbidden, wantSub: "header-denied"},
		{token: "tok_200_grpc_trailer", wantStatus: http.StatusForbidden, wantSub: "trailer-denied"},
		{token: "tok_200_grpc_body", wantStatus: http.StatusBadGateway, wantSub: "expected HTTP 101 Switching Protocols", wantBody: "unexpected-grpc-body"},
	}
	hsClient := NewHTTPClient(HTTPClientConfig{BaseURL: handshakeSrv.URL})
	for _, tc := range badHandshakeCases {
		_, err := hsClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
			InteractionID: "int_u",
			LeaseToken:    tc.token,
		})
		var te *TransportError
		if !errors.As(err, &te) || te.StatusCode != tc.wantStatus || !strings.Contains(te.Message, tc.wantSub) || (tc.wantBody != "" && !strings.Contains(te.Body, tc.wantBody)) {
			t.Errorf("ConnectInteraction(%s) = %v (te=%+v), want status=%d sub=%q bodySub=%q", tc.token, err, te, tc.wantStatus, tc.wantSub, tc.wantBody)
		}
	}

	// 5. Concurrent LeaseSimple (HTTP/2) and ConnectInteraction (HTTP/1.1 WebSocket upgrade via gorilla/websocket) against a TLS server under -race.
	wsUpgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1beta/environments:leaseInteractions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"leases":[{"interaction_id":"int_tls","lease_token":"tok_tls"}]}`))
		case "/v1beta/interactions:streamForEnvironmentRuntime":
			wsConn, err := wsUpgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer wsConn.Close()
			if _, _, err := wsConn.ReadMessage(); err != nil {
				return
			}
			doneBytes, _ := MarshalServerEventBatchJSON(&ServerEventBatch{
				InteractionCompleted: &InteractionCompletedEvent{},
			})
			_ = wsConn.WriteMessage(websocket.TextMessage, doneBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	tlsSrv.EnableHTTP2 = true
	tlsSrv.StartTLS()
	defer tlsSrv.Close()

	baseTLS := tlsSrv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	baseTLS.NextProtos = []string{"h2"}
	tlsClient := NewHTTPClient(HTTPClientConfig{
		BaseURL:        tlsSrv.URL,
		EnvironmentKey: "ek_tls",
		TLSConfig:      baseTLS,
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if leases, err := tlsClient.LeaseSimple(t.Context(), "w-tls", 1); err != nil || len(leases) != 1 {
				t.Errorf("concurrent TLS LeaseSimple() = (%+v, %v), want 1 lease and nil error", leases, err)
			}
		}()
		go func() {
			defer wg.Done()
			st, err := tlsClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
				InteractionID: "int_tls",
				LeaseToken:    "tok_tls",
			})
			if err != nil {
				t.Errorf("concurrent TLS ConnectInteraction() = %v, want nil", err)
				return
			}
			defer st.Close()
			if b, err := st.Recv(t.Context()); err != nil || b.InteractionCompleted == nil {
				t.Errorf("concurrent TLS st.Recv() = (%+v, %v), want InteractionCompleted and nil error", b, err)
			}
		}()
	}
	wg.Wait()
}

func TestNewDefaultWSDialerAndHandshakeGuards(t *testing.T) {
	// 1. newDefaultWSDialer(nil, 0) must not dereference nil tlsCfg and must set 45s default HandshakeTimeout + Proxy.
	nilDialer := newDefaultWSDialer(nil, 0)
	if nilDialer.TLSClientConfig != nil {
		t.Errorf("newDefaultWSDialer(nil, 0).TLSClientConfig = %+v, want nil", nilDialer.TLSClientConfig)
	}
	if nilDialer.HandshakeTimeout != 45*time.Second {
		t.Errorf("newDefaultWSDialer(nil, 0).HandshakeTimeout = %v, want 45s", nilDialer.HandshakeTimeout)
	}
	if nilDialer.Proxy == nil {
		t.Error("newDefaultWSDialer(nil, 0).Proxy = nil, want ProxyFromEnvironment")
	}
	if negDialer := newDefaultWSDialer(nil, -time.Second); negDialer.HandshakeTimeout != 45*time.Second {
		t.Errorf("newDefaultWSDialer(nil, -1s).HandshakeTimeout = %v, want 45s", negDialer.HandshakeTimeout)
	}

	// 2. newDefaultWSDialer(origTLS, 7s) must clone TLSConfig, clear NextProtos without mutating caller, and apply custom timeout.
	origTLS := &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2"},
	}
	customDialer := newDefaultWSDialer(origTLS, 7*time.Second)
	if customDialer.TLSClientConfig == nil || customDialer.TLSClientConfig == origTLS {
		t.Fatalf("newDefaultWSDialer(origTLS).TLSClientConfig = %p (orig=%p), want distinct non-nil clone", customDialer.TLSClientConfig, origTLS)
	}
	if customDialer.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("TLSClientConfig.MinVersion = %v, want TLS 1.3", customDialer.TLSClientConfig.MinVersion)
	}
	if len(customDialer.TLSClientConfig.NextProtos) != 0 {
		t.Errorf("TLSClientConfig.NextProtos = %v, want empty", customDialer.TLSClientConfig.NextProtos)
	}
	if len(origTLS.NextProtos) != 1 || origTLS.NextProtos[0] != "h2" {
		t.Errorf("caller origTLS.NextProtos mutated to %v, want [\"h2\"]", origTLS.NextProtos)
	}
	if customDialer.HandshakeTimeout != 7*time.Second {
		t.Errorf("customDialer.HandshakeTimeout = %v, want 7s", customDialer.HandshakeTimeout)
	}

	// 3. wsEndpointURL scheme mapping and validation errors.
	for _, tc := range []struct {
		baseURL string
		want    string
	}{
		{baseURL: "http://example.com", want: "ws://example.com/v1beta/interactions:streamForEnvironmentRuntime"},
		{baseURL: "https://example.com", want: "wss://example.com/v1beta/interactions:streamForEnvironmentRuntime"},
		{baseURL: "ws://example.com", want: "ws://example.com/v1beta/interactions:streamForEnvironmentRuntime"},
		{baseURL: "wss://example.com", want: "wss://example.com/v1beta/interactions:streamForEnvironmentRuntime"},
	} {
		c := NewHTTPClient(HTTPClientConfig{BaseURL: tc.baseURL})
		gotURL, err := c.wsEndpointURL(c.BidiStreamMethodPath())
		if err != nil || gotURL.String() != tc.want {
			t.Errorf("wsEndpointURL(%q) = (%v, %v), want %q", tc.baseURL, gotURL, err, tc.want)
		}
	}
	for _, badBase := range []string{"http://\x7f", "ftp://example.com", "http:"} {
		badClient := NewHTTPClient(HTTPClientConfig{BaseURL: badBase})
		if _, err := badClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{InteractionID: "int_bad_url"}); err == nil {
			t.Errorf("ConnectInteraction(BaseURL=%q) = nil, want URL error", badBase)
		}
	}
	for _, badReq := range []*ConnectInteractionRequest{nil, {InteractionID: "   "}} {
		var badReqTE *TransportError
		_, err := NewHTTPClient(HTTPClientConfig{BaseURL: "http://example.com"}).ConnectInteraction(t.Context(), badReq)
		if !errors.As(err, &badReqTE) || badReqTE.StatusCode != http.StatusBadRequest {
			t.Errorf("ConnectInteraction(%+v) = %v, want 400 TransportError", badReq, err)
		}
	}

	// 4. Handshake JSON marshal error aborts before dialing and returns error.
	wantMarshalErr := errors.New("injected handshake marshal error")
	var dialAttempted atomic.Bool
	marshalFailClient := NewHTTPClient(HTTPClientConfig{
		BaseURL: "http://example.com",
		WSDialer: &websocket.Dialer{
			NetDialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				dialAttempted.Store(true)
				return nil, errors.New("unexpected dial")
			},
		},
	})
	marshalFailClient.marshalClientBatch = func(*ClientEventBatch) ([]byte, error) {
		return nil, wantMarshalErr
	}
	if _, err := marshalFailClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{InteractionID: "int_m_fail"}); !errors.Is(err, wantMarshalErr) || dialAttempted.Load() {
		t.Errorf("ConnectInteraction(marshal failure) = %v (dialed=%v), want %v and dialed=false", err, dialAttempted.Load(), wantMarshalErr)
	}

	// 5. Initial handshake frame write error closes wsStream and returns write error, and HeartbeatInterval == 0 vs > 0 works cleanly.
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	wsEchoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer sc.Close()
		_, _, _ = sc.ReadMessage()
	}))
	defer wsEchoSrv.Close()

	wantWriteErr := errors.New("handshake write failed")
	var writeFailClosed atomic.Bool
	writeFailClient := NewHTTPClient(HTTPClientConfig{
		BaseURL: wsEchoSrv.URL,
		WSDialer: &websocket.Dialer{
			NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return &nthWriteFailConn{
					Conn:     raw,
					failAt:   2, // 1st write is HTTP GET upgrade; 2nd write is initial JSON handshake frame.
					writeErr: wantWriteErr,
					onClose:  func() { writeFailClosed.Store(true) },
				}, nil
			},
		},
	})
	if _, err := writeFailClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{InteractionID: "int_w_fail"}); !errors.Is(err, wantWriteErr) {
		t.Errorf("ConnectInteraction(handshake write failure) = %v, want %v", err, wantWriteErr)
	}
	if !writeFailClosed.Load() {
		t.Error("ConnectInteraction(handshake write failure) did not close wsStream")
	}

	wsHBClient := NewHTTPClient(HTTPClientConfig{BaseURL: wsEchoSrv.URL})
	rawWSSt, err := wsHBClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID: "int_ws_no_hb",
	})
	if err != nil {
		t.Fatalf("ConnectInteraction(ws HeartbeatInterval=0) failed: %v", err)
	}
	if _, isHB := rawWSSt.(*HeartbeatStream); isHB {
		t.Error("ConnectInteraction(ws HeartbeatInterval=0) wrapped in *HeartbeatStream, want raw *wsInteractionStream")
	}
	_ = rawWSSt.Close()

	hbWSSt, err := wsHBClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID:     "int_ws_hb",
		AckOutputSeqNum:   12,
		HeartbeatInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ConnectInteraction(ws HeartbeatInterval>0) failed: %v", err)
	}
	hbWS, ok := hbWSSt.(*HeartbeatStream)
	if !ok || hbWS.ackSeq.Load() != 12 {
		t.Fatalf("ConnectInteraction(ws HeartbeatInterval>0) = %T (hb=%+v), want *HeartbeatStream with ackSeq=12", hbWSSt, hbWS)
	}
	_ = hbWSSt.Close()

	// 6. Verify mapWSReadError and live Recv handling for 1000, empty close (1005), raw TCP drop (1006 -> ErrUnexpectedEOF), 1007/1008 close errors, inbound ErrReadLimit, and outbound MaxGRPCFrameBytes limit.
	if got := mapWSReadError(&websocket.CloseError{Code: websocket.CloseNormalClosure}); !errors.Is(got, io.EOF) {
		t.Errorf("mapWSReadError(1000) = %v, want %v", got, io.EOF)
	}
	if got := mapWSReadError(&websocket.CloseError{Code: websocket.CloseNoStatusReceived}); !errors.Is(got, io.EOF) {
		t.Errorf("mapWSReadError(1005) = %v, want %v", got, io.EOF)
	}
	if got := mapWSReadError(&websocket.CloseError{Code: websocket.CloseAbnormalClosure}); !errors.Is(got, io.ErrUnexpectedEOF) {
		t.Errorf("mapWSReadError(1006) = %v, want %v", got, io.ErrUnexpectedEOF)
	}
	var mappedCloseTE *TransportError
	if err := mapWSReadError(&websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: " denied "}); !errors.As(err, &mappedCloseTE) || mappedCloseTE.StatusCode != http.StatusForbidden || mappedCloseTE.Message != "denied" {
		t.Errorf("mapWSReadError(1008) = %v, want 403 TransportError with trimmed message", err)
	}
	if limitErr := mapWSReadError(websocket.ErrReadLimit); !errors.Is(limitErr, websocket.ErrReadLimit) || !strings.Contains(limitErr.Error(), "exceeds") {
		t.Errorf("mapWSReadError(ErrReadLimit) = %v, want wrapped ErrReadLimit", limitErr)
	}
	plainReadErr := errors.New("plain read failure")
	if got := mapWSReadError(plainReadErr); !errors.Is(got, plainReadErr) {
		t.Errorf("mapWSReadError(plainReadErr) = %v, want %v", got, plainReadErr)
	}

	// Live empty close frame -> io.EOF, live raw TCP close -> io.ErrUnexpectedEOF, live read limit -> ErrReadLimit.
	emptyCloseCli, emptyCloseSrv := newTestWSPair(t, nil)
	_ = emptyCloseSrv.WriteControl(websocket.CloseMessage, []byte{}, time.Now().Add(time.Second))
	emptyCloseWS := newWSInteractionStream(t.Context(), emptyCloseCli)
	if _, err := emptyCloseWS.Recv(t.Context()); !errors.Is(err, io.EOF) {
		t.Errorf("Recv(empty close frame) = %v, want io.EOF", err)
	}
	_ = emptyCloseWS.Close()

	dropCli, dropSrv := newTestWSPair(t, nil)
	_ = dropSrv.UnderlyingConn().Close()
	dropWS := newWSInteractionStream(t.Context(), dropCli)
	if _, err := dropWS.Recv(t.Context()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("Recv(raw TCP drop) = %v, want io.ErrUnexpectedEOF", err)
	}
	_ = dropWS.Close()

	limitCli, limitSrv := newTestWSPair(t, nil)
	limitWS := newWSInteractionStream(t.Context(), limitCli)
	exactInbound := make([]byte, MaxGRPCFrameBytes)
	for i := range exactInbound {
		exactInbound[i] = ' '
	}
	exactInbound[0] = '{'
	exactInbound[len(exactInbound)-1] = '}'
	limitSrvDone := make(chan struct{})
	go func() {
		defer close(limitSrvDone)
		_ = limitSrv.WriteMessage(websocket.TextMessage, exactInbound)
		_ = limitSrv.WriteMessage(websocket.TextMessage, make([]byte, MaxGRPCFrameBytes+1))
	}()
	if _, err := limitWS.Recv(t.Context()); err != nil {
		t.Errorf("Recv(MaxGRPCFrameBytes) = %v, want nil", err)
	}
	if _, err := limitWS.Recv(t.Context()); !errors.Is(err, websocket.ErrReadLimit) {
		t.Errorf("Recv(MaxGRPCFrameBytes+1) = %v, want websocket.ErrReadLimit", err)
	}
	_ = limitWS.Close()
	_ = limitSrv.Close()
	<-limitSrvDone

	// Outbound MaxGRPCFrameBytes exact and +1 boundary checks.
	outBoundCli, outBoundSrv := newTestWSPair(t, nil)
	outBoundWS := newWSInteractionStream(t.Context(), outBoundCli)
	outBoundSrvDone := make(chan struct{})
	go func() {
		defer close(outBoundSrvDone)
		for {
			if _, _, err := outBoundSrv.ReadMessage(); err != nil {
				return
			}
		}
	}()
	if err := outBoundWS.writeTextMessage(t.Context(), make([]byte, MaxGRPCFrameBytes)); err != nil {
		t.Errorf("writeTextMessage(MaxGRPCFrameBytes) = %v, want nil", err)
	}
	if err := outBoundWS.writeTextMessage(t.Context(), make([]byte, MaxGRPCFrameBytes+1)); err == nil || !strings.Contains(err.Error(), "outbound WebSocket frame size") {
		t.Errorf("writeTextMessage(MaxGRPCFrameBytes+1) = %v, want size limit error", err)
	}
	_ = outBoundWS.Close()
	<-outBoundSrvDone

	// 7. wsInteractionStream edge cases: nil/pre-canceled ctx/streamCtx, unmarshalable payload, underlying write error, double CloseSend write count, closed Recv.
	canceledCtx, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	unmarshalableBatch := &ClientEventBatch{
		Events: []*ClientEvent{{Payload: &EventPayload{FunctionResult: &FunctionResultEvent{CallID: "c", Name: "fn", Result: make(chan int)}}}},
	}

	activeHook := &hookedNetConnConfig{}
	activeHook.writeErr.Store(wantWriteErr)
	activeCli, _ := newTestWSPair(t, activeHook)
	activeWS := newWSInteractionStream(t.Context(), activeCli)
	if err := activeWS.Send(canceledCtx, unmarshalableBatch); !errors.Is(err, context.Canceled) {
		t.Errorf("activeWS.Send(canceledCtx) = %v, want context.Canceled", err)
	}
	if err := activeWS.writeTextMessage(canceledCtx, []byte("{}")); !errors.Is(err, context.Canceled) {
		t.Errorf("activeWS.writeTextMessage(canceledCtx) = %v, want context.Canceled", err)
	}
	if _, err := activeWS.Recv(canceledCtx); !errors.Is(err, context.Canceled) {
		t.Errorf("activeWS.Recv(canceledCtx) = %v, want context.Canceled", err)
	}
	if err := activeWS.Send(t.Context(), unmarshalableBatch); err == nil {
		t.Error("activeWS.Send(unmarshalable chan) = nil, want error")
	}
	if err := activeWS.Send(t.Context(), &ClientEventBatch{}); !errors.Is(err, wantWriteErr) {
		t.Errorf("activeWS.Send(writeErr) = %v, want %v", err, wantWriteErr)
	}

	// Child ctx cancellation during blocked Send (synchronized on writeStarted -> cancel -> closedCh -> unblock).
	childSendHook := newBlockingHookConfig()
	childSendHook.blockReads = false
	childSendCli, _ := newTestWSPair(t, childSendHook)
	childSendWS := newWSInteractionStream(t.Context(), childSendCli)
	childSendCtx, cancelChildSend := context.WithCancel(t.Context())
	defer cancelChildSend()
	childSendDone := make(chan error, 1)
	go func() {
		childSendDone <- childSendWS.Send(childSendCtx, &ClientEventBatch{})
	}()
	<-childSendHook.writeStarted
	cancelChildSend()
	<-childSendHook.closedCh
	close(childSendHook.unblock)
	if err := <-childSendDone; !errors.Is(err, context.Canceled) {
		t.Errorf("childSendWS.Send(childSendCtx cancel) = %v, want context.Canceled", err)
	}
	_ = childSendWS.Close()

	// Verify writeTextMessage returns nil when WriteMessage succeeds even if ctx cancels concurrently during Write.
	raceCtx, cancelRace := context.WithCancel(t.Context())
	raceHook := &hookedNetConnConfig{onWrite: cancelRace}
	raceCli, _ := newTestWSPair(t, raceHook)
	raceWS := newWSInteractionStream(t.Context(), raceCli)
	if err := raceWS.writeTextMessage(raceCtx, []byte("{}")); err != nil {
		t.Errorf("raceWS.writeTextMessage(raceCtx) = %v, want nil", err)
	}
	_ = raceWS.Close()

	// Verify pre-lock fast-path cancellation and closed-pipe checks on contended sendMu / readMu.
	blockCfg1 := newBlockingHookConfig()
	contendedCli1, _ := newTestWSPair(t, blockCfg1)
	streamCtx1, cancelStream1 := context.WithCancel(t.Context())
	contendedWS1 := newWSInteractionStream(streamCtx1, contendedCli1)
	var contendedWG1 sync.WaitGroup
	contendedWG1.Add(2)
	go func() {
		defer contendedWG1.Done()
		_ = contendedWS1.writeTextMessage(t.Context(), []byte("{}"))
	}()
	go func() {
		defer contendedWG1.Done()
		_, _ = contendedWS1.Recv(t.Context())
	}()
	<-blockCfg1.writeStarted
	<-blockCfg1.readStarted

	fastSendErrCh1 := make(chan error, 1)
	go func() {
		fastSendErrCh1 <- contendedWS1.writeTextMessage(canceledCtx, []byte("{}"))
	}()
	select {
	case err := <-fastSendErrCh1:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("contendedWS1.writeTextMessage(canceledCtx) = %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("contendedWS1.writeTextMessage(canceledCtx) blocked on sendMu")
	}

	fastRecvErrCh1 := make(chan error, 1)
	go func() {
		_, err := contendedWS1.Recv(canceledCtx)
		fastRecvErrCh1 <- err
	}()
	select {
	case err := <-fastRecvErrCh1:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("contendedWS1.Recv(canceledCtx) = %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("contendedWS1.Recv(canceledCtx) blocked on readMu")
	}

	cancelStream1()
	fastStreamSendErrCh := make(chan error, 1)
	go func() {
		fastStreamSendErrCh <- contendedWS1.writeTextMessage(t.Context(), []byte("{}"))
	}()
	select {
	case err := <-fastStreamSendErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("contendedWS1.writeTextMessage(canceled streamCtx) = %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("contendedWS1.writeTextMessage(canceled streamCtx) blocked on sendMu")
	}

	fastStreamRecvErrCh := make(chan error, 1)
	go func() {
		_, err := contendedWS1.Recv(t.Context())
		fastStreamRecvErrCh <- err
	}()
	select {
	case err := <-fastStreamRecvErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("contendedWS1.Recv(canceled streamCtx) = %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("contendedWS1.Recv(canceled streamCtx) blocked on readMu")
	}
	close(blockCfg1.unblock)
	contendedWG1.Wait()

	blockCfg2 := newBlockingHookConfig()
	contendedCli2, _ := newTestWSPair(t, blockCfg2)
	contendedWS2 := newWSInteractionStream(t.Context(), contendedCli2)
	var contendedWG2 sync.WaitGroup
	contendedWG2.Add(2)
	go func() {
		defer contendedWG2.Done()
		_ = contendedWS2.writeTextMessage(t.Context(), []byte("{}"))
	}()
	go func() {
		defer contendedWG2.Done()
		_, _ = contendedWS2.Recv(t.Context())
	}()
	<-blockCfg2.writeStarted
	<-blockCfg2.readStarted
	_ = contendedWS2.Close()

	fastClosedSendCh := make(chan error, 1)
	go func() {
		fastClosedSendCh <- contendedWS2.writeTextMessage(t.Context(), []byte("{}"))
	}()
	select {
	case err := <-fastClosedSendCh:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("contendedWS2.writeTextMessage(closed) = %v, want io.ErrClosedPipe", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("contendedWS2.writeTextMessage(closed) blocked on sendMu")
	}

	fastClosedRecvCh := make(chan error, 1)
	go func() {
		_, err := contendedWS2.Recv(t.Context())
		fastClosedRecvCh <- err
	}()
	select {
	case err := <-fastClosedRecvCh:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("contendedWS2.Recv(closed) = %v, want io.ErrClosedPipe", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("contendedWS2.Recv(closed) blocked on readMu")
	}
	close(blockCfg2.unblock)
	contendedWG2.Wait()

	// Verify post-lock re-checks when callers park on sendMu / readMu before ctx, streamCtx, CloseSend, or Close fires.
	// Pre-buffering a second WebSocket frame in gorilla's bufio.Reader ensures ReadMessage() would succeed from memory
	// even after Close() closes the underlying net.Conn if the post-lock s.closed.Load() check were absent.
	for _, tc := range []struct {
		name    string
		trigger func(cancelCall, cancelStream context.CancelFunc, ws *wsInteractionStream)
		wantErr error
	}{
		{
			name:    "call_ctx",
			trigger: func(cancelCall, _ context.CancelFunc, _ *wsInteractionStream) { cancelCall() },
			wantErr: context.Canceled,
		},
		{
			name:    "stream_ctx",
			trigger: func(_, cancelStream context.CancelFunc, _ *wsInteractionStream) { cancelStream() },
			wantErr: context.Canceled,
		},
		{
			name:    "close",
			trigger: func(_, _ context.CancelFunc, ws *wsInteractionStream) { _ = ws.Close() },
			wantErr: io.ErrClosedPipe,
		},
	} {
		t.Run("post_lock_"+tc.name, func(t *testing.T) {
			postCli, postSrv := newTestWSPair(t, nil)
			_ = postSrv.WriteMessage(websocket.TextMessage, []byte("{}"))
			_ = postSrv.WriteMessage(websocket.TextMessage, []byte("{}"))
			postStreamCtx, cancelPostStream := context.WithCancel(t.Context())
			defer cancelPostStream()
			postWS := newWSInteractionStream(postStreamCtx, postCli)
			defer postWS.Close()
			if _, err := postWS.Recv(t.Context()); err != nil {
				t.Fatalf("warmup postWS.Recv(%s) = %v, want nil", tc.name, err)
			}

			postWS.sendMu.Lock()
			postWS.readMu.Lock()

			callCtx, cancelCall := context.WithCancel(t.Context())
			defer cancelCall()
			waiterSendCh := make(chan error, 1)
			waiterRecvCh := make(chan error, 1)
			go func() {
				waiterSendCh <- postWS.writeTextMessage(callCtx, []byte("{}"))
			}()
			go func() {
				_, err := postWS.Recv(callCtx)
				waiterRecvCh <- err
			}()
			time.Sleep(10 * time.Millisecond)

			tc.trigger(cancelCall, cancelPostStream, postWS)
			postWS.sendMu.Unlock()
			postWS.readMu.Unlock()
			if err := <-waiterSendCh; !errors.Is(err, tc.wantErr) {
				t.Errorf("post-lock writeTextMessage(%s) = %v, want %v", tc.name, err, tc.wantErr)
			}
			if err := <-waiterRecvCh; !errors.Is(err, tc.wantErr) {
				t.Errorf("post-lock Recv(%s) = %v, want %v", tc.name, err, tc.wantErr)
			}
		})
	}

	postCloseSendCli, _ := newTestWSPair(t, nil)
	postCloseSendWS := newWSInteractionStream(t.Context(), postCloseSendCli)
	postCloseSendWS.sendMu.Lock()
	waiterCloseSendCh := make(chan error, 1)
	go func() {
		waiterCloseSendCh <- postCloseSendWS.writeTextMessage(t.Context(), []byte("{}"))
	}()
	time.Sleep(10 * time.Millisecond)
	postCloseSendWS.closedSend.Store(true)
	postCloseSendWS.sendMu.Unlock()
	if err := <-waiterCloseSendCh; !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("post-lock writeTextMessage(close_send) = %v, want %v", err, io.ErrClosedPipe)
	}
	_ = postCloseSendWS.Close()

	// Fresh stream for CloseSend idempotency (since activeCli latched writeErr on its gorilla Conn).
	closeSendHook := &hookedNetConnConfig{}
	closeSendCli, _ := newTestWSPair(t, closeSendHook)
	closeSendWS := newWSInteractionStream(t.Context(), closeSendCli)
	if err := closeSendWS.CloseSend(); err != nil {
		t.Errorf("first closeSendWS.CloseSend() = %v, want nil", err)
	}
	if got := closeSendHook.writes.Load(); got != 1 {
		t.Errorf("writes after first CloseSend() = %d, want 1", got)
	}
	if err := closeSendWS.CloseSend(); err != nil {
		t.Errorf("second closeSendWS.CloseSend() = %v, want nil", err)
	}
	if got := closeSendHook.writes.Load(); got != 1 {
		t.Errorf("writes after second CloseSend() = %d, want 1", got)
	}
	if err := closeSendWS.Send(t.Context(), unmarshalableBatch); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("closeSendWS.Send(after CloseSend) = %v, want io.ErrClosedPipe", err)
	}
	if err := closeSendWS.writeTextMessage(t.Context(), []byte("{}")); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("closeSendWS.writeTextMessage(after CloseSend) = %v, want io.ErrClosedPipe", err)
	}
	_ = closeSendWS.Close()
	if _, err := closeSendWS.Recv(t.Context()); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("closeSendWS.Recv(after Close) = %v, want io.ErrClosedPipe", err)
	}

	canceledHook := &hookedNetConnConfig{}
	canceledCli, _ := newTestWSPair(t, canceledHook)
	canceledStreamWS := newWSInteractionStream(canceledCtx, canceledCli)
	if err := canceledStreamWS.Send(t.Context(), unmarshalableBatch); !errors.Is(err, context.Canceled) {
		t.Errorf("canceledStreamWS.Send() = %v, want context.Canceled", err)
	}
	if err := canceledStreamWS.writeTextMessage(t.Context(), []byte("{}")); !errors.Is(err, context.Canceled) {
		t.Errorf("canceledStreamWS.writeTextMessage() = %v, want context.Canceled", err)
	}
	if _, err := canceledStreamWS.Recv(t.Context()); !errors.Is(err, context.Canceled) {
		t.Errorf("canceledStreamWS.Recv() = %v, want context.Canceled", err)
	}
	if err := canceledStreamWS.CloseSend(); err != nil {
		t.Errorf("canceledStreamWS.CloseSend() = %v, want nil", err)
	}
	if got := canceledHook.writes.Load(); got != 0 {
		t.Errorf("canceledStreamWS.CloseSend() writes = %d, want 0", got)
	}

	// Verify defensive nil-Context guards in newWSInteractionStream, Send, writeTextMessage, Recv, and CloseSend do not panic.
	var nilCtxHolder struct {
		ctx context.Context
	}
	nilHook := &hookedNetConnConfig{}
	nilCli, nilSrv := newTestWSPair(t, nilHook)
	nilGuardWS := newWSInteractionStream(nilCtxHolder.ctx, nilCli)
	if err := nilGuardWS.Send(nilCtxHolder.ctx, &ClientEventBatch{}); err != nil {
		t.Errorf("nilGuardWS.Send(nilCtx) = %v, want nil", err)
	}
	if _, _, err := nilSrv.ReadMessage(); err != nil {
		t.Errorf("nilSrv.ReadMessage() = %v, want nil", err)
	}
	_ = nilSrv.UnderlyingConn().Close()
	if _, err := nilGuardWS.Recv(nilCtxHolder.ctx); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("nilGuardWS.Recv(nilCtx) = %v, want io.ErrUnexpectedEOF", err)
	}
	if err := nilGuardWS.CloseSend(); err != nil {
		t.Errorf("nilGuardWS.CloseSend() = %v, want nil", err)
	}
	hbNil := NewHeartbeatStream(nilCtxHolder.ctx, nilGuardWS, 0, 0)
	_ = hbNil.Close()

	nilWriteErrHook := &hookedNetConnConfig{}
	nilWriteErrHook.writeErr.Store(wantWriteErr)
	nilWriteErrCli, _ := newTestWSPair(t, nilWriteErrHook)
	nilWriteErrWS := newWSInteractionStream(nilCtxHolder.ctx, nilWriteErrCli)
	if err := nilWriteErrWS.writeTextMessage(nilCtxHolder.ctx, []byte("{}")); !errors.Is(err, wantWriteErr) {
		t.Errorf("nilWriteErrWS.writeTextMessage(nilCtx, writeErr) = %v, want %v", err, wantWriteErr)
	}
	_ = nilWriteErrWS.Close()

	// Verify in-flight Send/Recv with non-canceled call ctx (including context.Background()) return streamCtx.Err() (not io.ErrClosedPipe) when streamCtx cancels mid-operation.
	midSendHook := newBlockingHookConfig()
	midSendHook.blockReads = false
	midSendCli, _ := newTestWSPair(t, midSendHook)
	midSendStreamCtx, cancelMidSendStream := context.WithCancel(t.Context())
	defer cancelMidSendStream()
	midSendWS := newWSInteractionStream(midSendStreamCtx, midSendCli)
	midSendDone := make(chan error, 1)
	go func() {
		midSendDone <- midSendWS.Send(context.Background(), &ClientEventBatch{})
	}()
	<-midSendHook.writeStarted
	cancelMidSendStream()
	<-midSendHook.closedCh
	close(midSendHook.unblock)
	if err := <-midSendDone; !errors.Is(err, context.Canceled) {
		t.Errorf("midSendWS.Send(streamCtx cancel) = %v, want context.Canceled", err)
	}
	_ = midSendWS.Close()

	midRecvCli, _ := newTestWSPair(t, nil)
	midRecvStreamCtx, cancelMidRecvStream := context.WithTimeout(t.Context(), 15*time.Millisecond)
	defer cancelMidRecvStream()
	midRecvWS := newWSInteractionStream(midRecvStreamCtx, midRecvCli)
	defer midRecvWS.Close()
	if _, err := midRecvWS.Recv(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("midRecvWS.Recv(streamCtx timeout) = %v, want context.DeadlineExceeded", err)
	}

	// Verify ConnectInteraction closes the HTTP handshake response body across 101 success and handshake failure paths.
	bodyCloseOKCli, bodyCloseOKSrv := newTestWSPair(t, nil)
	var okBodyClosed atomic.Bool
	okDialClient := NewHTTPClient(HTTPClientConfig{BaseURL: "http://example.com"})
	okDialClient.wsDialer = wsConnDialerFunc(func(_ context.Context, _ string, _ http.Header) (*websocket.Conn, *http.Response, error) {
		return bodyCloseOKCli, &http.Response{
			StatusCode: http.StatusSwitchingProtocols,
			Status:     "101 Switching Protocols",
			Body: &trackingReadCloser{
				Reader:  strings.NewReader(""),
				onClose: func() { okBodyClosed.Store(true) },
			},
		}, nil
	})
	okDialStream, err := okDialClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID: "int_ok_body_close",
	})
	if err != nil {
		t.Fatalf("ConnectInteraction(101 success) = %v, want nil", err)
	}
	if _, _, err := bodyCloseOKSrv.ReadMessage(); err != nil {
		t.Errorf("bodyCloseOKSrv.ReadMessage() = %v, want nil", err)
	}
	if !okBodyClosed.Load() {
		t.Error("ConnectInteraction(101 success) resp.BodyClosed = false, want true")
	}
	_ = okDialStream.Close()

	handshakeBodyCloseCases := []struct {
		name       string
		statusCode int
		status     string
		headers    http.Header
		body       string
		wantStatus int
	}{
		{
			name:       "101_bad_handshake",
			statusCode: http.StatusSwitchingProtocols,
			status:     "101 Switching Protocols",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "503_non_2xx",
			statusCode: http.StatusServiceUnavailable,
			status:     "503 Service Unavailable",
			body:       "unavailable",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "200_grpc_header_error",
			statusCode: http.StatusOK,
			status:     "200 OK",
			headers:    http.Header{"Grpc-Status": []string{"7"}, "Grpc-Message": []string{"denied"}},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "200_unexpected_non_101",
			statusCode: http.StatusOK,
			status:     "200 OK",
			body:       "unexpected-200-body",
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, tc := range handshakeBodyCloseCases {
		t.Run(tc.name, func(t *testing.T) {
			var bodyClosed atomic.Bool
			errClient := NewHTTPClient(HTTPClientConfig{BaseURL: "http://example.com"})
			errClient.wsDialer = wsConnDialerFunc(func(_ context.Context, _ string, _ http.Header) (*websocket.Conn, *http.Response, error) {
				return nil, &http.Response{
					StatusCode: tc.statusCode,
					Status:     tc.status,
					Header:     tc.headers,
					Body: &trackingReadCloser{
						Reader:  strings.NewReader(tc.body),
						onClose: func() { bodyClosed.Store(true) },
					},
				}, websocket.ErrBadHandshake
			})
			_, err := errClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
				InteractionID: "int_err_body_close",
			})
			var te *TransportError
			if !errors.As(err, &te) || te.StatusCode != tc.wantStatus {
				t.Errorf("ConnectInteraction(%s) = %v, want *TransportError StatusCode=%d", tc.name, err, tc.wantStatus)
			}
			if !bodyClosed.Load() {
				t.Errorf("ConnectInteraction(%s) resp.BodyClosed = false, want true", tc.name)
			}
		})
	}
}

type wsConnDialerFunc func(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error)

func (f wsConnDialerFunc) DialContext(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error) {
	return f(ctx, urlStr, requestHeader)
}

type nthWriteFailConn struct {
	net.Conn
	writes   atomic.Int32
	failAt   int32
	writeErr error
	onClose  func()
}

func (c *nthWriteFailConn) Write(p []byte) (int, error) {
	if c.writes.Add(1) >= c.failAt {
		return 0, c.writeErr
	}
	return c.Conn.Write(p)
}

func (c *nthWriteFailConn) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return c.Conn.Close()
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
