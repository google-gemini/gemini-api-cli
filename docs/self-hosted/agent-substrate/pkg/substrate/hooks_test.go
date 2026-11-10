// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package substrate

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workerpb "github.com/google-gemini/gemini-api-cli/docs/self-hosted/agent-substrate/pkg/workerpb"
	"github.com/google/go-cmp/cmp"
	atepb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRunProvisionAndSuspendJSONContract(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	fake.addAtespace("custom-atespace")
	now := time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)
	mgr := NewManager(client, ManagerConfig{
		AllowedAtespaces: []string{"gemini-self-hosted", "custom-atespace"},
		AllowedTemplates: []string{"gemini-env-actor", "custom-env-actor"},
		RouterAddr:       "atenet-router.ate-system.svc.cluster.local:443",
		ResumeCooldown:   1 * time.Second,
		NowFunc:          func() time.Time { return now },
	})

	// Exercise the Gemini API self-hosted custom_config schema.
	reqJSON := `{
		"schema_version": "v1",
		"environment_id": "env_hook_123",
		"interaction_id": "inter_456",
		"environment_config": {
			"custom_config": {
				"atespace": "custom-atespace",
				"template": "custom-env-actor",
				"worker_url_template": "https://console.cloud.google.com/logs/query?atespace={atespace}&actor={worker_id}&env={environment_id}",
				"egress_policy": {
					"rules": [
						{"https": {"hostnames": ["pypi.org", "files.pythonhosted.org"]}},
						{"tls_passthrough": {"hostnames": ["github.com"], "ports": {"numbers": [443]}}}
					]
				},
				"env": {
					"FOO": {"value": "bar"},
					"SECRET_REF": {"credential": "credentials/my-secret"}
				}
			}
		}
	}`

	var out bytes.Buffer
	err := RunProvision(t.Context(), mgr, strings.NewReader(reqJSON), &out)
	if err != nil {
		t.Fatalf("RunProvision() error = %v", err)
	}

	var gotResp ProvisionResponse
	if err := json.Unmarshal(out.Bytes(), &gotResp); err != nil {
		t.Fatalf("json.Unmarshal(ProvisionResponse) error = %v; raw = %s", err, out.String())
	}

	wantActor := ActorName("env_hook_123")
	wantResp := ProvisionResponse{
		SchemaVersion: "v1",
		URI:           "atenet-router.ate-system.svc.cluster.local:443",
		WorkerID:      wantActor,
		WorkerURL:     "https://console.cloud.google.com/logs/query?atespace=custom-atespace&actor=" + wantActor + "&env=env_hook_123",
		Metadata: map[string]string{
			"atespace": "custom-atespace",
			"template": "custom-env-actor",
			"action":   "created",
		},
		Headers: map[string]string{
			TargetActorHeader:  "custom-atespace/" + wantActor,
			EnvoyTimeoutHeader: DefaultRouterTimeoutMs,
		},
	}
	if diff := cmp.Diff(wantResp, gotResp); diff != "" {
		t.Errorf("ProvisionResponse diff (-want +got):\n%s", diff)
	}

	key := actorKey("custom-atespace", wantActor)
	fake.mu.Lock()
	createdActor := fake.actors[key]
	egress := fake.egressByActor[key]
	fake.mu.Unlock()
	if createdActor == nil {
		t.Fatalf("expected actor %q to be created in custom-atespace", wantActor)
	}
	if got := createdActor.GetMetadata().GetAtespace(); got != "custom-atespace" {
		t.Errorf("actor atespace = %q, want %q", got, "custom-atespace")
	}
	if got := createdActor.GetActorTemplate().GetName(); got != "custom-env-actor" {
		t.Errorf("actor template = %q, want %q", got, "custom-env-actor")
	}
	if egress == nil || len(egress.GetRules()) != 3 {
		t.Fatalf("expected 2 passthrough custom_config.egress_policy rules + 1 control-plane rule, got %+v", egress)
	}
	if diff := cmp.Diff([]string{"pypi.org", "files.pythonhosted.org"}, egress.GetRules()[0].GetHttps().GetHostnames()); diff != "" {
		t.Errorf("passthrough HTTPS rule hostnames diff (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]int32{443}, egress.GetRules()[1].GetTlsPassthrough().GetPorts().GetNumbers()); diff != "" {
		t.Errorf("passthrough TLS rule ports diff (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{DefaultGeminiControlPlaneHost}, egress.GetRules()[2].GetTlsPassthrough().GetHostnames()); diff != "" {
		t.Errorf("control-plane TLS passthrough hostnames diff (-want +got):\n%s", diff)
	}

	now = now.Add(5 * time.Second)
	suspJSON := `{
		"schema_version": "v1",
		"environment_id": "env_hook_123",
		"worker_id": "` + wantActor + `",
		"idle_duration": "65s",
		"environment_config": {
			"custom_config": {
				"atespace": "custom-atespace"
			}
		}
	}`
	if err := RunSuspend(t.Context(), mgr, strings.NewReader(suspJSON)); err != nil {
		t.Fatalf("RunSuspend() error = %v", err)
	}

	calls := fake.recordedCalls()
	if calls[len(calls)-1] != "SuspendActor" {
		t.Errorf("last RPC call = %q, want SuspendActor (all calls = %v)", calls[len(calls)-1], calls)
	}
	fake.mu.Lock()
	gotState := fake.actors[key].GetStatus().GetState()
	fake.mu.Unlock()
	if gotState != atepb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("actor state after RunSuspend = %v, want ACTOR_STATE_SUSPENDED", gotState)
	}
}

func TestUnmarshalReadsOnlyCustomConfig(t *testing.T) {
	var cfg ProvisionEnvironmentConfig
	data := `{
		"mode": "self_hosted",
		"type": "remote",
		"network": "disabled",
		"env": {"IGNORED": {"value": "x"}},
		"custom_config": {
			"atespace": "gemini-self-hosted",
			"egress_policy": {"rules": [{"tlsPassthrough": {"hostnames": ["*"], "ports": {"all": {}}}}]}
		}
	}`
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("json.Unmarshal(environment_config) error = %v", err)
	}
	if cfg.Env != nil {
		t.Errorf("cfg.Env = %v, want nil (top-level env must be ignored)", cfg.Env)
	}
	if cfg.Atespace != "gemini-self-hosted" {
		t.Errorf("cfg.Atespace = %q, want %q", cfg.Atespace, "gemini-self-hosted")
	}
	if got := cfg.EgressPolicy.GetRules()[0].GetTlsPassthrough().GetHostnames(); len(got) != 1 || got[0] != "*" {
		t.Errorf("cfg.EgressPolicy TLS passthrough hostnames = %v, want [*]", got)
	}
}

func TestUnmarshalWithoutEgressPolicyDefaultsToControlPlaneOnly(t *testing.T) {
	var cfg ProvisionEnvironmentConfig
	if err := json.Unmarshal([]byte(`{"custom_config": {"atespace": "gemini-self-hosted"}}`), &cfg); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	rules := BuildEgressRules(cfg.EgressPolicy, "")
	if len(rules) != 1 {
		t.Fatalf("len(rules) = %d, want 1 control-plane TLS passthrough rule", len(rules))
	}
	if diff := cmp.Diff([]string{DefaultGeminiControlPlaneHost}, rules[0].GetTlsPassthrough().GetHostnames()); diff != "" {
		t.Errorf("default TLS passthrough hostnames diff (-want +got):\n%s", diff)
	}
}

func TestRunProvisionValidationErrors(t *testing.T) {
	_, client, _ := startFakeControlServer(t)
	mgr := NewManager(client, ManagerConfig{
		AllowedAtespaces: []string{"gemini-self-hosted", "nonexistent-atespace"},
	})

	tests := []struct {
		name          string
		input         string
		wantErrSubstr string
	}{
		{
			name:          "invalid_json",
			input:         `{not-json`,
			wantErrSubstr: "invalid character",
		},
		{
			name:          "unsupported_schema_version",
			input:         `{"schema_version":"v99","environment_id":"env_1"}`,
			wantErrSubstr: "unsupported provision schema_version",
		},
		{
			name:          "missing_environment_id",
			input:         `{"schema_version":"v1","interaction_id":"inter_1"}`,
			wantErrSubstr: "environment_id is required",
		},
		{
			name:          "top_level_request_custom_config_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","custom_config":{"atespace":"gemini-self-hosted"}}`,
			wantErrSubstr: "unknown field \"custom_config\"",
		},
		{
			name:          "unknown_field_in_custom_config_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"unknown_option":"x"}}}`,
			wantErrSubstr: "unknown field \"unknown_option\"",
		},
		{
			name:          "invalid_custom_config_not_object",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":"not-an-object"}}`,
			wantErrSubstr: "custom_config must be a JSON object",
		},
		{
			name:          "invalid_custom_config_atespace",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"atespace":"bad/atespace"}}}`,
			wantErrSubstr: "contains invalid character",
		},
		{
			name:          "unallowlisted_custom_config_atespace",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"atespace":"unapproved-atespace"}}}`,
			wantErrSubstr: "not permitted by --allowed-atespaces",
		},
		{
			name:          "unallowlisted_custom_config_template",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"template":"unapproved-template"}}}`,
			wantErrSubstr: "not permitted by --allowed-templates",
		},
		{
			name:          "nonexistent_custom_config_atespace",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"atespace":"nonexistent-atespace"}}}`,
			wantErrSubstr: "does not exist",
		},
		{
			name:          "invalid_custom_config_template",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"template":"-bad-template"}}}`,
			wantErrSubstr: "must start and end with a lowercase alphanumeric character",
		},
		{
			name:          "invalid_custom_config_worker_url_template",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"worker_url_template":"https://bad url"}}}`,
			wantErrSubstr: "must not contain whitespace or control characters",
		},
		{
			name:          "invalid_server_url",
			input:         `{"schema_version":"v1","environment_id":"env_1","server_url":"ftp://bad"}`,
			wantErrSubstr: "expected http(s) URL with non-empty host",
		},
		{
			name:          "lease_token_without_interaction_id_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","lease_token":"lt_v1_abc"}`,
			wantErrSubstr: "interaction_id is required when lease_token is set",
		},
		{
			name:          "legacy_network_key_in_custom_config_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"network":{"allowlist":[{"domain":"pypi.org"}]}}}}`,
			wantErrSubstr: "unknown field \"network\"",
		},
		{
			name:          "unknown_field_in_egress_policy_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"egress_policy":{"rules":[{"tcp":{"hostnames":["db.example.com"]}}]}}}}`,
			wantErrSubstr: "parsing custom_config.egress_policy",
		},
		{
			name:          "egress_policy_metadata_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"egress_policy":{"metadata":{"name":"default"},"rules":[]}}}}`,
			wantErrSubstr: "metadata must not be set",
		},
		{
			name:          "https_credential_injection_effect_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"egress_policy":{"rules":[{"https":{"hostnames":["attacker.example.com"],"effects":{"replaceHeaders":[{"header":"Authorization","credentialUri":"ate-secret://k8s/default/token"}]}}}]}}}}`,
			wantErrSubstr: "rules[0]: effects",
		},
		{
			name:          "http_actor_jwt_effect_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"egress_policy":{"rules":[{"tlsPassthrough":{"hostnames":["*"],"ports":{"all":{}}}},{"http":{"hostnames":["attacker.example.com"],"effects":{"replaceHeaders":[{"header":"Authorization","actorJwt":{"audiences":["x"],"expirationSeconds":"300"}}]}}}]}}}}`,
			wantErrSubstr: "rules[1]: effects",
		},
		{
			name:          "empty_effects_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"egress_policy":{"rules":[{"https":{"hostnames":["pypi.org"],"effects":{}}}]}}}}`,
			wantErrSubstr: "rules[0]: effects",
		},
		{
			name:          "unknown_field_in_env_entry_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"env":{"FOO":{"value":"bar","extra":1}}}}}`,
			wantErrSubstr: "unknown field \"extra\"",
		},
		{
			name:          "both_value_and_credential_in_env_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"env":{"FOO":{"value":"bar","credential":"credentials/x"}}}}}`,
			wantErrSubstr: "cannot specify both 'value' and 'credential'",
		},
		{
			name:          "reserved_env_key_in_custom_config_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","environment_config":{"custom_config":{"env":{"SSL_CERT_FILE":{"value":"/tmp/evil.pem"}}}}}`,
			wantErrSubstr: "reserved environment variable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := RunProvision(t.Context(), mgr, strings.NewReader(tc.input), &out)
			if err == nil {
				t.Fatalf("RunProvision(%s) error = nil, want non-nil containing %q", tc.name, tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("RunProvision(%s) error = %q, want substring %q", tc.name, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

func TestRunSuspendValidationErrors(t *testing.T) {
	_, client, _ := startFakeControlServer(t)
	mgr := NewManager(client, ManagerConfig{
		AllowedAtespaces: []string{"gemini-self-hosted", "missing-space"},
	})

	tests := []struct {
		name          string
		input         string
		wantErrSubstr string
	}{
		{
			name:          "invalid_json",
			input:         `{not-json`,
			wantErrSubstr: "invalid character",
		},
		{
			name:          "top_level_atespace_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","worker_id":"actor-1","atespace":"gemini-self-hosted"}`,
			wantErrSubstr: "unknown field \"atespace\"",
		},
		{
			name:          "top_level_custom_config_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","worker_id":"actor-1","custom_config":{"atespace":"gemini-self-hosted"}}`,
			wantErrSubstr: "unknown field \"custom_config\"",
		},
		{
			name:          "invalid_worker_id_with_slash_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","worker_id":"gemini-self-hosted/actor-1"}`,
			wantErrSubstr: "contains invalid character",
		},
		{
			name:          "invalid_worker_id_uppercase_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","worker_id":"BAD_ACTOR"}`,
			wantErrSubstr: "must start and end with a lowercase alphanumeric character",
		},
		{
			name:          "unallowlisted_atespace_in_custom_config_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","worker_id":"actor-1","environment_config":{"custom_config":{"atespace":"unapproved-space"}}}`,
			wantErrSubstr: "not permitted by --allowed-atespaces",
		},
		{
			name:          "nonexistent_atespace_in_custom_config_rejected",
			input:         `{"schema_version":"v1","environment_id":"env_1","worker_id":"actor-1","environment_config":{"custom_config":{"atespace":"missing-space"}}}`,
			wantErrSubstr: "does not exist",
		},
		{
			name:          "invalid_custom_config_not_object",
			input:         `{"schema_version":"v1","environment_id":"env_1","worker_id":"actor-1","environment_config":{"custom_config":"not-an-object"}}`,
			wantErrSubstr: "custom_config must be a JSON object",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := RunSuspend(t.Context(), mgr, strings.NewReader(tc.input))
			if err == nil {
				t.Fatalf("RunSuspend(%s) error = nil, want non-nil containing %q", tc.name, tc.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("RunSuspend(%s) error = %q, want substring %q", tc.name, err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

func TestActorDaemonRejectsInvalidAndReservedEnvKeys(t *testing.T) {
	srv := newActorWorkerServiceServer(t.Context(), ActorDaemonConfig{RuntimeBin: "/bin/true"})

	for _, badKey := range []string{
		"FOO=BAR",
		"LD_PRELOAD",
		"LD_AUDIT",
		"PYTHONPATH",
		"HOME",
		"ALL_PROXY",
		"SSL_CERT_FILE",
		"SSL_CERT_DIR",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"KUBERNETES_SERVICE_HOST",
		"KUBERNETES_SERVICE_PORT_HTTPS",
		"123BAD",
	} {
		_, err := srv.StartWork(t.Context(), &workerpb.StartWorkRequest{
			InteractionId: "int_1",
			LeaseToken:    "tok_1",
			EnvironmentConfig: &workerpb.EnvironmentConfig{
				Env: map[string]string{badKey: "val"},
			},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("StartWork(env[%q]) code = %v, want InvalidArgument", badKey, status.Code(err))
		}
	}
}

func TestProvisionDispatchesWorkToActorDaemon(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	fake.addAtespace("dispatch-atespace")

	t.Setenv("GEMINI_API_KEY", "secret-ambient-api-key")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "serve_args.txt")
	envFile := filepath.Join(dir, "serve_env.txt")
	holdFile := filepath.Join(dir, "release.txt")
	fakeRuntime := filepath.Join(dir, "fake_gemini_api.sh")
	script := "#!/bin/sh\n" +
		"env > \"" + envFile + "\"\n" +
		"printf '%s\\n' \"$@\" > \"" + argsFile + "\"\n" +
		"while [ ! -f \"" + holdFile + "\" ]; do sleep 0.02; done\n"
	if err := os.WriteFile(fakeRuntime, []byte(script), 0755); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen error = %v", err)
	}
	daemonCtx, cancelDaemon := context.WithCancel(t.Context())
	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		_ = ServeActorDaemon(daemonCtx, ln, ActorDaemonConfig{
			RuntimeBin: fakeRuntime,
			RootDir:    dir,
			WorkDir:    dir,
			ServerURL:  "https://generativelanguage.googleapis.com",
		})
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(holdFile, []byte("done"), 0644)
		cancelDaemon()
		<-daemonDone
	})

	// Verify /healthz on ActorDaemon.
	healthResp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	_ = healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", healthResp.StatusCode)
	}

	workerConn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(newRouterCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(worker) error = %v", err)
	}
	t.Cleanup(func() {
		_ = workerConn.Close()
	})
	workerClient := workerpb.NewWorkerServiceClient(workerConn)

	idleBefore, err := workerClient.GetIdleStatus(t.Context(), &workerpb.GetIdleStatusRequest{})
	if err != nil {
		t.Fatalf("GetIdleStatus(before) error = %v", err)
	}
	if idleBefore.GetRunning() || idleBefore.GetActiveInteractions() != 0 {
		t.Fatalf("GetIdleStatus(before) = (%v, %d), want (false, 0)", idleBefore.GetRunning(), idleBefore.GetActiveInteractions())
	}

	mgr := NewManager(client, ManagerConfig{
		AllowedAtespaces: []string{"dispatch-atespace"},
		AllowedTemplates: []string{"dispatch-actor"},
		RouterAddr:       ln.Addr().String(),
		RouterInsecure:   true,
	})

	reqJSON := `{
		"schema_version": "v1",
		"environment_id": "env_dispatch_1",
		"interaction_id": "int_dispatch_1",
		"lease_token": "tok_dispatch_1",
		"server_url": "https://generativelanguage.googleapis.com",
		"environment_config": {
			"custom_config": {
				"atespace": "dispatch-atespace",
				"template": "dispatch-actor",
				"env": {"FOO": {"value": "bar"}}
			}
		}
	}`
	var out bytes.Buffer
	if err := RunProvision(t.Context(), mgr, strings.NewReader(reqJSON), &out); err != nil {
		t.Fatalf("RunProvision() error = %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var gotArgs string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(argsFile); err == nil && len(b) > 0 {
			gotArgs = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gotArgs == "" {
		t.Fatalf("expected actor-daemon to invoke fake gemini-api environments serve")
	}
	for _, want := range []string{
		"environments",
		"serve",
		"--interaction-id=int_dispatch_1",
		"--lease-token=tok_dispatch_1",
		"--server-url=https://generativelanguage.googleapis.com",
	} {
		if !strings.Contains(gotArgs, want) {
			t.Errorf("spawned serve args missing %q in:\n%s", want, gotArgs)
		}
	}

	idleDuring, err := workerClient.GetIdleStatus(t.Context(), &workerpb.GetIdleStatusRequest{})
	if err != nil {
		t.Fatalf("GetIdleStatus(during) error = %v", err)
	}
	if !idleDuring.GetRunning() || idleDuring.GetActiveInteractions() != 1 {
		t.Errorf("GetIdleStatus(during) = (%v, %d), want (true, 1)", idleDuring.GetRunning(), idleDuring.GetActiveInteractions())
	}

	dupResp, err := workerClient.StartWork(t.Context(), &workerpb.StartWorkRequest{
		InteractionId: "int_dispatch_1",
		LeaseToken:    "tok_dispatch_1",
	})
	if err != nil {
		t.Fatalf("duplicate StartWork() error = %v", err)
	}
	if dupResp.GetStatus() != "already_running" {
		t.Errorf("duplicate StartWork().Status = %q, want %q", dupResp.GetStatus(), "already_running")
	}

	gotEnvBytes, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("os.ReadFile(envFile) error = %v", err)
	}
	gotEnv := string(gotEnvBytes)
	if !strings.Contains(gotEnv, "FOO=bar") {
		t.Errorf("child env missing FOO=bar:\n%s", gotEnv)
	}
	for _, scrubbed := range []string{"GEMINI_API_KEY=", "KUBERNETES_SERVICE_HOST="} {
		if strings.Contains(gotEnv, scrubbed) {
			t.Errorf("child env leaked scrubbed variable %q:\n%s", scrubbed, gotEnv)
		}
	}

	if err := os.WriteFile(holdFile, []byte("done"), 0644); err != nil {
		t.Fatalf("os.WriteFile(holdFile) error = %v", err)
	}
	drainDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(drainDeadline) {
		st, err := workerClient.GetIdleStatus(t.Context(), &workerpb.GetIdleStatusRequest{})
		if err == nil && !st.GetRunning() && st.GetActiveInteractions() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	idleAfter, err := workerClient.GetIdleStatus(t.Context(), &workerpb.GetIdleStatusRequest{})
	if err != nil {
		t.Fatalf("GetIdleStatus(after) error = %v", err)
	}
	if idleAfter.GetRunning() || idleAfter.GetActiveInteractions() != 0 {
		t.Errorf("GetIdleStatus(after) = (%v, %d), want (false, 0)", idleAfter.GetRunning(), idleAfter.GetActiveInteractions())
	}
}

func TestActorDaemonShutdownRejectsWorkAndStopsSubprocess(t *testing.T) {
	dir := t.TempDir()
	startedFile := filepath.Join(dir, "started.txt")
	fakeRuntime := filepath.Join(dir, "fake_long_serve.sh")
	script := "#!/bin/sh\n" +
		"trap 'exit 0' TERM INT\n" +
		"echo started > \"" + startedFile + "\"\n" +
		"sleep 30 &\n" +
		"wait $!\n"
	if err := os.WriteFile(fakeRuntime, []byte(script), 0755); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	srv := newActorWorkerServiceServer(t.Context(), ActorDaemonConfig{
		RuntimeBin: fakeRuntime,
		RootDir:    dir,
		WorkDir:    dir,
	})
	resp, err := srv.StartWork(t.Context(), &workerpb.StartWorkRequest{
		InteractionId: "int_shutdown_1",
		LeaseToken:    "tok_shutdown_1",
	})
	if err != nil || resp.GetStatus() != "accepted" {
		t.Fatalf("StartWork() = (%v, %v), want accepted", resp, err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(startedFile); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(startedFile); err != nil {
		t.Fatalf("expected child process to start: %v", err)
	}

	srv.shutdown()

	if _, err := srv.StartWork(t.Context(), &workerpb.StartWorkRequest{
		InteractionId: "int_shutdown_2",
		LeaseToken:    "tok_shutdown_2",
	}); status.Code(err) != codes.Unavailable {
		t.Errorf("StartWork() after shutdown code = %v, want Unavailable", status.Code(err))
	}
}
