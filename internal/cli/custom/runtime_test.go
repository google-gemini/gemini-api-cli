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
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/cli/custom/envruntime"
	"github.com/google-gemini/gemini-api-cli/internal/clierrors"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/spf13/cobra"
)

func newTestRuntimeCmd(t *testing.T, serverURL string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "gemini-api"}
	pf := root.PersistentFlags()
	pf.String("server-url", "", "")
	pf.String("api-version", "v1beta", "")
	pf.String("api-key", "", "")
	pf.String("access-token", "", "")
	pf.String("user-project", "", "")
	pf.String("api-revision", "", "")
	pf.StringArray("header", nil, "")
	pf.String("timeout", "", "")
	pf.Bool("debug", false, "")
	pf.Bool("dry-run", false, "")
	pf.Bool("no-keyring", true, "")
	sub := &cobra.Command{Use: "test"}
	root.AddCommand(sub)
	sub.SetContext(t.Context())
	if serverURL != "" {
		if err := pf.Set("server-url", serverURL); err != nil {
			t.Fatalf("Set(server-url=%q) failed: %v", serverURL, err)
		}
	}
	return sub
}

func startH2CTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:   handler,
		Protocols: &protos,
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return "http://" + ln.Addr().String()
}

func newRuntimeCLIRootForTest(t *testing.T, serverURL string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := &cobra.Command{Use: "gemini-api", SilenceErrors: true, SilenceUsage: true}
	pf := root.PersistentFlags()
	pf.StringP("output-format", "o", "pretty", "")
	pf.String("color", "never", "")
	pf.StringP("jq", "q", "", "")
	pf.Bool("raw-output", true, "")
	pf.String("server-url", serverURL, "")
	pf.StringArrayP("header", "H", nil, "")
	pf.Bool("include-headers", false, "")
	pf.String("timeout", "", "")
	pf.Bool("interactive", false, "")
	pf.Bool("no-interactive", true, "")
	pf.Bool("usage", false, "")
	pf.Bool("dry-run", false, "")
	pf.BoolP("debug", "d", false, "")
	pf.Bool("agent-mode", false, "")
	pf.Bool("no-retries", false, "")
	pf.Bool("no-keyring", true, "")
	pf.String("api-key", "", "")
	pf.String("access-token", "", "")
	pf.String("api-version", "v1beta", "")
	pf.String("api-revision", "", "")
	pf.String("user-project", "", "")

	envs := &cobra.Command{Use: "environments"}
	envs.AddCommand(newEnvironmentsConnectCmd(), newEnvironmentsPollCmd(), newEnvironmentsServeCmd())
	root.AddCommand(envs)
	root.SetContext(t.Context())

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	return root, &stdout, &stderr
}

func TestRegisterMountsRuntimeCommandsAsDynamic(t *testing.T) {
	noopRunE := func(*cobra.Command, []string) error { return nil }
	buildSkeleton := func() (*cobra.Command, *cobra.Command) {
		root := &cobra.Command{Use: "gemini-api"}
		for _, customName := range []string{"tts", "analyze", "transcribe"} {
			root.AddCommand(&cobra.Command{Use: customName})
		}

		filesGroup := &cobra.Command{Use: "files"}
		for _, leaf := range []string{"get", "delete"} {
			c := &cobra.Command{Use: leaf, RunE: noopRunE}
			c.Flags().String("file", "", "")
			filesGroup.AddCommand(c)
		}
		root.AddCommand(filesGroup)

		modelsGroup := &cobra.Command{Use: "models"}
		modelsGet := &cobra.Command{Use: "get", RunE: noopRunE}
		modelsGet.Flags().String("model", "", "")
		modelsGroup.AddCommand(modelsGet)
		root.AddCommand(modelsGroup)

		envsGroup := &cobra.Command{Use: "environments"}
		envFilesGroup := &cobra.Command{Use: "files"}
		envFilesList := &cobra.Command{Use: "list", RunE: noopRunE}
		envFilesList.Flags().String("environment", "", "")
		envFilesList.Flags().String("path", "", "")
		envFilesGroup.AddCommand(envFilesList)
		envsGroup.AddCommand(envFilesGroup)
		root.AddCommand(envsGroup)

		for _, intent := range []string{"generate", "image", "video", "music"} {
			c := &cobra.Command{Use: intent, RunE: noopRunE}
			c.Flags().String("body", "", "")
			root.AddCommand(c)
		}
		agentGroup := &cobra.Command{Use: "agent"}
		agentRun := &cobra.Command{Use: "run", RunE: noopRunE}
		agentRun.Flags().String("body", "", "")
		agentGroup.AddCommand(agentRun)
		root.AddCommand(agentGroup)
		return root, envsGroup
	}

	root, envsGroup := buildSkeleton()
	Register(root)

	for _, subName := range []string{"connect", "poll", "serve"} {
		sub := findChild(envsGroup, subName)
		if sub == nil {
			t.Fatalf("findChild(environments, %q) = nil, want registered subcommand", subName)
		}
		if got := sub.Annotations["speakeasy_usage_dynamic"]; got != "true" {
			t.Errorf("%s Annotations[speakeasy_usage_dynamic] = %q, want \"true\"", subName, got)
		}
	}

	root2, envsGroup2 := buildSkeleton()
	preExisting := &cobra.Command{Use: "connect", Short: "pre-existing"}
	envsGroup2.AddCommand(preExisting)
	Register(root2)
	if got := findChild(envsGroup2, "connect"); got != preExisting {
		t.Errorf("findChild(environments, \"connect\") = %p, want pre-existing %p", got, preExisting)
	}
	connectCount := 0
	for _, c := range envsGroup2.Commands() {
		if c.Name() == "connect" {
			connectCount++
		}
	}
	if connectCount != 1 {
		t.Errorf("environments connect command count = %d, want 1", connectCount)
	}
	if findChild(envsGroup2, "poll") == nil || findChild(envsGroup2, "serve") == nil {
		t.Errorf("Register() did not mount remaining runtime subcommands")
	}

	t.Run("missing_environments_group_panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("Register(incomplete root) did not panic, want panic")
			}
		}()
		incompleteRoot, missingEnvs := buildSkeleton()
		incompleteRoot.RemoveCommand(missingEnvs)
		Register(incompleteRoot)
	})
}

func TestEnvironmentsRuntimeCommandsFlagsDryRunAndExecution(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GEMINI_ACCESS_TOKEN", "")
	t.Setenv("GEMINI_ENVIRONMENT_KEY", "")
	t.Setenv("GEMINI_ENVIRONMENT_ID", "")
	t.Setenv("GEMINI_WORKER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	t.Run("flag_defaults_and_usage_kdl", func(t *testing.T) {
		connectCmd := newEnvironmentsConnectCmd()
		if got := connectCmd.Flags().Lookup("command-timeout").DefValue; got != "2m" {
			t.Errorf("connect --command-timeout DefValue = %q, want \"2m\"", got)
		}
		if connectCmd.Flags().Lookup("worker-id") == nil {
			t.Errorf("connect missing --worker-id flag")
		}
		if connectCmd.Flags().Lookup("disable-bash") == nil {
			t.Errorf("connect missing --disable-bash flag")
		}
		pollCmd := newEnvironmentsPollCmd()
		if got := pollCmd.Flags().Lookup("provision-timeout").DefValue; got != "2m" {
			t.Errorf("poll --provision-timeout DefValue = %q, want \"2m\"", got)
		}

		for _, sub := range []string{"connect", "poll", "serve"} {
			root, stdout, _ := newRuntimeCLIRootForTest(t, "")
			root.SetArgs([]string{"environments", sub, "--usage", "ignored-positional"})
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(environments %s --usage) unexpected error: %v", sub, err)
			}
			if !strings.Contains(stdout.String(), `cmd "`+sub+`"`) {
				t.Errorf("environments %s --usage output missing cmd header:\n%s", sub, stdout.String())
			}
		}
	})

	t.Run("validation_errors", func(t *testing.T) {
		sbxDir := t.TempDir()
		outsideDir := t.TempDir()
		cases := []struct {
			name    string
			args    []string
			wantSub string
		}{
			{
				name:    "connect_positional_arg",
				args:    []string{"environments", "connect", "extra"},
				wantSub: "accepts 0 arguments, received 1",
			},
			{
				name:    "connect_workdir_escapes_root",
				args:    []string{"environments", "connect", "--root-dir", sbxDir, "--workdir", outsideDir},
				wantSub: "escapes root-dir",
			},
			{
				name:    "connect_zero_heartbeat",
				args:    []string{"environments", "connect", "--heartbeat-interval", "0s"},
				wantSub: "--heartbeat-interval must be > 0",
			},
			{
				name:    "connect_zero_command_timeout",
				args:    []string{"environments", "connect", "--command-timeout", "0s"},
				wantSub: "--command-timeout must be > 0",
			},
			{
				name:    "connect_control_char_env_key",
				args:    []string{"environments", "connect", "--environment-key", "bad\x00key"},
				wantSub: "--environment-key contains control characters",
			},
			{
				name:    "connect_zero_max_concurrent",
				args:    []string{"environments", "connect", "--max-concurrent", "0"},
				wantSub: "--max-concurrent must be >= 1",
			},
			{
				name:    "connect_zero_poll_interval",
				args:    []string{"environments", "connect", "--poll-interval", "0s"},
				wantSub: "--poll-interval must be > 0",
			},
			{
				name:    "connect_missing_credentials",
				args:    []string{"environments", "connect"},
				wantSub: "missing credentials",
			},
			{
				name:    "poll_zero_max_interactions",
				args:    []string{"environments", "poll", "--max-interactions", "0"},
				wantSub: "--max-interactions must be >= 1",
			},
			{
				name:    "poll_zero_poll_interval",
				args:    []string{"environments", "poll", "--poll-interval", "0s"},
				wantSub: "--poll-interval must be > 0",
			},
			{
				name:    "poll_zero_provision_timeout",
				args:    []string{"environments", "poll", "--provision-timeout", "0s"},
				wantSub: "--provision-timeout must be > 0",
			},
			{
				name:    "poll_zero_handoff_heartbeat",
				args:    []string{"environments", "poll", "--handoff-heartbeat-interval", "0s"},
				wantSub: "--handoff-heartbeat-interval must be > 0",
			},
			{
				name:    "poll_control_char_env_key",
				args:    []string{"environments", "poll", "--environment-key", "bad\nkey"},
				wantSub: "--environment-key contains control characters",
			},
			{
				name:    "poll_invalid_environment_id",
				args:    []string{"environments", "poll", "--environment", "environments/a/b"},
				wantSub: `invalid environment id "environments/a/b"`,
			},
			{
				name:    "poll_missing_provision_cmd",
				args:    []string{"environments", "poll", "--environment-key", "ek_1"},
				wantSub: "--provision-cmd is required",
			},
			{
				name:    "poll_missing_credentials",
				args:    []string{"environments", "poll", "--provision-cmd", "true"},
				wantSub: "missing credentials",
			},
			{
				name:    "serve_only_interaction_id",
				args:    []string{"environments", "serve", "--interaction-id", "int_1"},
				wantSub: "--interaction-id and --lease-token must be provided together",
			},
			{
				name:    "serve_invalid_interaction_id",
				args:    []string{"environments", "serve", "--interaction-id", "interactions/a/b", "--lease-token", "tok_1"},
				wantSub: "invalid interaction id",
			},
			{
				name:    "serve_control_char_lease_token",
				args:    []string{"environments", "serve", "--interaction-id", "int_1", "--lease-token", "tok\r1"},
				wantSub: "--lease-token contains control characters",
			},
			{
				name:    "serve_missing_lease",
				args:    []string{"environments", "serve"},
				wantSub: "missing lease",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				root, _, _ := newRuntimeCLIRootForTest(t, "http://127.0.0.1:1")
				root.SetArgs(tc.args)
				rawErr := root.Execute()
				if rawErr == nil {
					t.Fatalf("Execute(%v) = nil, want error containing %q", tc.args, tc.wantSub)
				}
				err := output.CLIError(root, rawErr)
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("Execute(%v) error = %q, want substring %q", tc.args, err.Error(), tc.wantSub)
				}
				if got := clierrors.ExitCode(err); got != clierrors.ExitUsage {
					t.Errorf("ExitCode(%v) = %d, want %d (ExitUsage)", tc.args, got, clierrors.ExitUsage)
				}
			})
		}
	})

	t.Run("dry_run_previews", func(t *testing.T) {
		t.Cleanup(output.ResetPreparsedRendering)
		dryCases := []struct {
			name     string
			args     []string
			wantPath string
			wantBody string
		}{
			{
				name:     "connect_dry_run",
				args:     []string{"-o", "json", "--dry-run", "environments", "connect", "--environment-key", "ek_c", "--worker-id", "w-custom", "--max-concurrent", "2"},
				wantPath: "/v1beta/environments:leaseInteractions",
				wantBody: "w-custom",
			},
			{
				name:     "poll_dry_run_with_scoped_environment",
				args:     []string{"-o", "json", "--dry-run", "environments", "poll", "--environment", "environments/env_77", "--environment-key", "ek_p", "--poller-id", "p-custom", "--max-interactions", "3"},
				wantPath: "/v1beta/environments:leaseInteractions",
				wantBody: "env_77",
			},
			{
				name:     "serve_dry_run_default_sample",
				args:     []string{"-o", "json", "--dry-run", "environments", "serve"},
				wantPath: "/google.learning.gemini.api.interactions.v1beta.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey",
			},
			{
				name:     "serve_dry_run_explicit_lease",
				args:     []string{"-o", "json", "--dry-run", "environments", "serve", "--interaction-id", "interactions/int_s1", "--lease-token", "tok_s1"},
				wantPath: "/google.learning.gemini.api.interactions.v1beta.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey",
			},
		}
		for _, tc := range dryCases {
			t.Run(tc.name, func(t *testing.T) {
				root, stdout, _ := newRuntimeCLIRootForTest(t, "http://127.0.0.1:1")
				output.PreparseRenderingFlags(root, []string{"-o", "json"})
				root.SetArgs(tc.args)
				if err := root.Execute(); err != nil {
					t.Fatalf("Execute(%v) unexpected error: %v", tc.args, err)
				}
				var preview map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil {
					t.Fatalf("json.Unmarshal(dry-run output %q) failed: %v", stdout.String(), err)
				}
				reqObj, _ := preview["request"].(map[string]any)
				if gotURL, _ := reqObj["url"].(string); !strings.HasSuffix(gotURL, tc.wantPath) {
					t.Errorf("dry-run url = %q, want suffix %q", gotURL, tc.wantPath)
				}
				if tc.wantBody != "" && !strings.Contains(stdout.String(), tc.wantBody) {
					t.Errorf("dry-run output = %s, want substring %q", stdout.String(), tc.wantBody)
				}
			})
		}
	})

	t.Run("live_connect_poll_and_serve_execution", func(t *testing.T) {
		var connectWorkerID atomic.Value
		var pollEnvID atomic.Value
		var serveToolRejected atomic.Bool
		connectDone := make(chan struct{}, 1)
		provisionFile := filepath.Join(t.TempDir(), "polled.json")

		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1beta/environments:leaseInteractions":
				var req struct {
					WorkerID      string                  `json:"worker_id"`
					EnvironmentID string                  `json:"environment_id"`
					ActiveLeases  []envruntime.LeaseEntry `json:"active_leases"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				if strings.HasPrefix(req.WorkerID, "poller") {
					pollEnvID.Store(req.EnvironmentID)
					if len(req.ActiveLeases) == 0 && pollEnvID.Load() == "env_live_poll" {
						_ = json.NewEncoder(w).Encode(map[string]any{
							"leases": []LeasedInteraction{{
								InteractionID: "int_polled",
								EnvironmentID: "env_live_poll",
								LeaseToken:    "tok_polled",
							}},
						})
						return
					}
					_, _ = w.Write([]byte(`{}`))
					return
				}
				if prev := connectWorkerID.Swap(req.WorkerID); prev == nil {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"leases": []LeasedInteraction{{
							InteractionID: "int_connected",
							EnvironmentID: "env_conn",
							LeaseToken:    "tok_conn",
						}},
					})
					return
				}
				_, _ = w.Write([]byte(`{}`))

			case "/google.learning.gemini.api.interactions.v1beta.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey":
				w.Header().Set("Content-Type", "application/grpc")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				flusher.Flush()

				hsRaw, err := envruntime.GRPCReadFrame(r.Body)
				if err != nil {
					return
				}
				hsBatch, err := envruntime.UnmarshalClientEventBatch(hsRaw)
				if err != nil || hsBatch.ReconnectInteraction == nil {
					return
				}
				if hsBatch.ReconnectInteraction.ClientID != envruntime.DefaultEnvironmentClientID {
					t.Errorf("handshake ClientID = %q, want %q", hsBatch.ReconnectInteraction.ClientID, envruntime.DefaultEnvironmentClientID)
				}
				intID := hsBatch.ReconnectInteraction.InteractionID
				if intID == "int_serve_bash_off" {
					_ = envruntime.GRPCWriteFrame(w, envruntime.MarshalServerEventBatch(&envruntime.ServerEventBatch{
						Events: []*envruntime.ServerEvent{{
							SeqNum: 1,
							Payload: &envruntime.EventPayload{
								Step: &envruntime.Step{
									ToolCall: &envruntime.ToolCallStep{
										ID: "c_bash",
										FunctionCall: &envruntime.FunctionCallStep{
											Name:      "execute_bash",
											Arguments: map[string]any{"command": "echo nope"},
										},
									},
								},
							},
						}},
					}))
					flusher.Flush()
					for {
						replyRaw, err := envruntime.GRPCReadFrame(r.Body)
						if err != nil {
							return
						}
						if len(replyRaw) == 0 {
							continue
						}
						replyBatch, err := envruntime.UnmarshalClientEventBatch(replyRaw)
						if err == nil && len(replyBatch.Events) == 1 && replyBatch.Events[0].Payload.Step.ToolResult.FunctionResult.IsError {
							serveToolRejected.Store(true)
						}
						break
					}
				}
				_ = envruntime.GRPCWriteFrame(w, envruntime.MarshalServerEventBatch(&envruntime.ServerEventBatch{
					InteractionCompleted: &envruntime.InteractionCompletedEvent{},
				}))
				flusher.Flush()
				if intID == "int_connected" {
					select {
					case connectDone <- struct{}{}:
					default:
					}
				}
			default:
				http.NotFound(w, r)
			}
		}))

		serveWork := filepath.Join(t.TempDir(), "serve-work")
		serveRoot, _, _ := newRuntimeCLIRootForTest(t, srvURL)
		serveRoot.SetArgs([]string{
			"--no-retries",
			"environments", "serve",
			"--interaction-id", "interactions/int_serve_bash_off",
			"--lease-token", "tok_serve",
			"--root-dir", serveWork,
			"--workdir", serveWork,
			"--disable-bash",
			"--quiet",
		})
		if err := serveRoot.Execute(); err != nil {
			t.Fatalf("Execute(environments serve) unexpected error: %v", err)
		}
		if !serveToolRejected.Load() {
			t.Errorf("serveToolRejected = false, want --disable-bash tool error sent before InteractionCompleted")
		}

		connCtx, cancelConn := context.WithCancel(t.Context())
		connDir := filepath.Join(t.TempDir(), "conn-work")
		connRoot, _, _ := newRuntimeCLIRootForTest(t, srvURL)
		connRoot.SetContext(connCtx)
		connRoot.SetArgs([]string{
			"environments", "connect",
			"--environment-key", "ek_conn",
			"--worker-id", "worker-cli-1",
			"--root-dir", connDir,
			"--poll-interval", "20ms",
			"--quiet",
		})
		connErrCh := make(chan error, 1)
		go func() {
			connErrCh <- connRoot.Execute()
		}()
		select {
		case <-connectDone:
		case <-time.After(3 * time.Second):
			t.Errorf("timed out waiting for environments connect to complete leased session")
		}
		cancelConn()
		if err := <-connErrCh; err != nil {
			t.Errorf("Execute(environments connect) = %v, want nil on context cancel", err)
		}
		if got, _ := connectWorkerID.Load().(string); got != "worker-cli-1" {
			t.Errorf("connect WorkerID = %q, want \"worker-cli-1\"", got)
		}

		pollCtx, cancelPoll := context.WithCancel(t.Context())
		pollRoot, _, _ := newRuntimeCLIRootForTest(t, srvURL)
		pollRoot.SetContext(pollCtx)
		pollRoot.SetArgs([]string{
			"environments", "poll",
			"--environment", "environments/env_live_poll",
			"--environment-key", "ek_poll",
			"--poller-id", "poller-cli-1",
			"--poll-interval", "20ms",
			"--provision-cmd", "cat > " + provisionFile,
			"--quiet",
		})
		pollErrCh := make(chan error, 1)
		go func() {
			pollErrCh <- pollRoot.Execute()
		}()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if raw, err := os.ReadFile(provisionFile); err == nil && len(raw) > 0 {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		cancelPoll()
		if err := <-pollErrCh; err != nil {
			t.Errorf("Execute(environments poll) = %v, want nil on context cancel", err)
		}
		if got, _ := pollEnvID.Load().(string); got != "env_live_poll" {
			t.Errorf("poll EnvironmentID = %q, want \"env_live_poll\"", got)
		}
	})

	t.Run("helpers_logger_and_no_retries", func(t *testing.T) {
		output.ResetAgentMode()
		output.ResetPreparsedRendering()
		config.Reset()
		t.Cleanup(func() {
			output.ResetAgentMode()
			output.ResetPreparsedRendering()
			config.Reset()
		})

		root, _, stderr := newRuntimeCLIRootForTest(t, "")
		logNormal := newRuntimeLogger(root, false)
		logNormal.Print("visible-msg")
		if !strings.Contains(stderr.String(), "visible-msg") {
			t.Errorf("newRuntimeLogger(quiet=false) did not write to stderr: %q", stderr.String())
		}
		stderr.Reset()
		logQuiet := newRuntimeLogger(root, true)
		logQuiet.Print("hidden-msg")
		if stderr.Len() != 0 {
			t.Errorf("newRuntimeLogger(quiet=true) wrote %q, want empty", stderr.String())
		}
		_ = root.PersistentFlags().Set("output-format", "json")
		logMachine := newRuntimeLogger(root, false)
		logMachine.Print("machine-hidden-msg")
		if stderr.Len() != 0 {
			t.Errorf("newRuntimeLogger(output-format=json) wrote %q, want empty", stderr.String())
		}

		if got := resolveNoRetries(root); got {
			t.Errorf("resolveNoRetries(default) = true, want false")
		}
		_ = root.PersistentFlags().Set("no-retries", "true")
		if got := resolveNoRetries(root); !got {
			t.Errorf("resolveNoRetries(--no-retries=true) = false, want true")
		}
	})
}

func TestRuntimeTransportCredentialsHeadersValidationAndSDKErrorAdapter(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GEMINI_ACCESS_TOKEN", "")
	t.Setenv("GEMINI_NO_KEYRING", "true")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	config.Reset()
	t.Cleanup(config.Reset)

	t.Run("credentials_missing_and_headers", func(t *testing.T) {
		cases := []struct {
			name         string
			explicitCred bool
			apiKey       string
			accessToken  string
			userProject  string
			apiRevision  string
			header       string
			wantMissing  bool
			wantAPIKey   string
			wantBearer   string
		}{
			{
				name:         "no_creds_without_explicit_runtime_cred",
				explicitCred: false,
				wantMissing:  true,
			},
			{
				name:         "no_creds_with_explicit_runtime_cred",
				explicitCred: true,
				wantMissing:  false,
			},
			{
				name:         "api_key_credential",
				explicitCred: false,
				apiKey:       "key-abc",
				userProject:  "proj-123",
				apiRevision:  "rev-2",
				header:       "X-Custom-Trace: trace-99",
				wantMissing:  false,
				wantAPIKey:   "key-abc",
			},
			{
				name:         "access_token_credential",
				explicitCred: false,
				accessToken:  "oauth-tok-xyz",
				wantMissing:  false,
				wantBearer:   "Bearer oauth-tok-xyz",
			},
			{
				name:         "api_key_wins_same_rank_tie",
				explicitCred: false,
				apiKey:       "tie-key-123",
				accessToken:  "tie-tok-456",
				wantMissing:  false,
				wantAPIKey:   "tie-key-123",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cmd := newTestRuntimeCmd(t, "https://example.googleapis.com/")
				pf := cmd.Root().PersistentFlags()
				if tc.apiKey != "" {
					_ = pf.Set("api-key", tc.apiKey)
				}
				if tc.accessToken != "" {
					_ = pf.Set("access-token", tc.accessToken)
				}
				if tc.userProject != "" {
					_ = pf.Set("user-project", tc.userProject)
				}
				if tc.apiRevision != "" {
					_ = pf.Set("api-revision", tc.apiRevision)
				}
				if tc.header != "" {
					_ = pf.Set("header", tc.header)
				}
				_ = pf.Set("timeout", "3s")

				rt, err := newRuntimeTransport(cmd, tc.explicitCred)
				if err != nil {
					t.Fatalf("newRuntimeTransport() unexpected error: %v", err)
				}
				if got := flagutil.CredentialsMissing(cmd); got != tc.wantMissing {
					t.Errorf("CredentialsMissing() = %v, want %v", got, tc.wantMissing)
				}
				if rt.timeout != 3*time.Second {
					t.Errorf("rt.timeout = %v, want 3s", rt.timeout)
				}

				req, err := rt.newRequest(t.Context(), http.MethodPost, "/v1beta/test", http.NoBody)
				if err != nil {
					t.Fatalf("rt.newRequest() unexpected error: %v", err)
				}
				if got := req.Header.Get("x-goog-api-key"); got != tc.wantAPIKey {
					t.Errorf("x-goog-api-key = %q, want %q", got, tc.wantAPIKey)
				}
				if got := req.Header.Get("Authorization"); got != tc.wantBearer {
					t.Errorf("Authorization = %q, want %q", got, tc.wantBearer)
				}
				if tc.userProject != "" && req.Header.Get("x-goog-user-project") != tc.userProject {
					t.Errorf("x-goog-user-project = %q, want %q", req.Header.Get("x-goog-user-project"), tc.userProject)
				}
				if tc.apiRevision != "" && req.Header.Get("Api-Revision") != tc.apiRevision {
					t.Errorf("Api-Revision = %q, want %q", req.Header.Get("Api-Revision"), tc.apiRevision)
				}
				if tc.header != "" && req.Header.Get("X-Custom-Trace") != "trace-99" {
					t.Errorf("X-Custom-Trace = %q, want \"trace-99\"", req.Header.Get("X-Custom-Trace"))
				}
			})
		}
	})

	t.Run("transport_validation_errors", func(t *testing.T) {
		badCases := []struct {
			name      string
			serverURL string
			apiVer    string
			header    string
			timeout   string
			wantSub   string
		}{
			{name: "ftp_scheme", serverURL: "ftp://example.com", wantSub: "invalid --server-url"},
			{name: "url_with_credentials", serverURL: "https://user:pass@example.com", wantSub: "without credentials, query, or fragment"},
			{name: "url_with_query", serverURL: "https://example.com?foo=bar", wantSub: "without credentials, query, or fragment"},
			{name: "url_with_fragment", serverURL: "https://example.com#sec", wantSub: "without credentials, query, or fragment"},
			{name: "invalid_api_version", apiVer: "..", wantSub: "--api-version"},
			{name: "header_missing_colon", header: "BadHeader", wantSub: "invalid header format"},
			{name: "header_empty_key", header: " : val", wantSub: "invalid header name"},
			{name: "header_space_in_key", header: "Bad Key: val", wantSub: "invalid header name"},
			{name: "header_control_char_val", header: "X-Key: bad\x7fval", wantSub: "--header contains control characters"},
			{name: "invalid_timeout", timeout: "not-a-duration", wantSub: "invalid --timeout"},
		}
		for _, tc := range badCases {
			t.Run(tc.name, func(t *testing.T) {
				cmd := newTestRuntimeCmd(t, tc.serverURL)
				pf := cmd.Root().PersistentFlags()
				if tc.apiVer != "" {
					_ = pf.Set("api-version", tc.apiVer)
				}
				if tc.header != "" {
					_ = pf.Set("header", tc.header)
				}
				if tc.timeout != "" {
					_ = pf.Set("timeout", tc.timeout)
				}
				if got, err := newRuntimeTransport(cmd, true); err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("newRuntimeTransport() = (%v, %v), want error containing %q", got, err, tc.wantSub)
				}
			})
		}
	})

	t.Run("sdk_error_wrapping_and_debug_redaction", func(t *testing.T) {
		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1beta/environments:leaseInteractions" {
				http.Error(w, `{"error":{"message":"permission denied"}}`, http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/grpc")
			w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			w.Header().Set("Grpc-Status", "14")
			w.Header().Set("Grpc-Message", "upstream%20down")
		}))

		cmd := newTestRuntimeCmd(t, srvURL)
		var dbg bytes.Buffer
		cmd.Root().SetErr(&dbg)
		_ = cmd.Root().PersistentFlags().Set("debug", "true")
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport(debug) failed: %v", err)
		}
		hc := NewHTTPClient(HTTPClientOptions{Transport: rt, EnvironmentKey: "ek_err"})
		if hc.ServerURL() != srvURL {
			t.Errorf("ServerURL() = %q, want %q", hc.ServerURL(), srvURL)
		}

		_, leaseErr := hc.LeaseSimple(t.Context(), "w-err", 1)
		if leaseErr == nil || !strings.Contains(leaseErr.Error(), "Status 403") {
			t.Errorf("LeaseSimple(403) = %v, want SDKDefaultError containing \"Status 403\"", leaseErr)
		}
		if !isNonRetryableControlPlaneError(leaseErr) {
			t.Errorf("isNonRetryableControlPlaneError(%v) = false, want true", leaseErr)
		}

		stream, err := hc.ConnectInteraction(t.Context(), &envruntime.ConnectInteractionRequest{
			InteractionID: "int_tr",
			LeaseToken:    "tok_secret_dbg",
		})
		if err != nil {
			t.Fatalf("ConnectInteraction() failed: %v", err)
		}
		_, recvErr := stream.Recv(t.Context())
		_ = stream.Close()
		if recvErr == nil || !strings.Contains(recvErr.Error(), "Status 503") {
			t.Errorf("stream.Recv(Grpc-Status 14) = %v, want SDKDefaultError containing \"Status 503\"", recvErr)
		}
		if isNonRetryableControlPlaneError(recvErr) {
			t.Errorf("isNonRetryableControlPlaneError(503) = true, want false")
		}
		if strings.Contains(dbg.String(), "tok_secret_dbg") {
			t.Errorf("debug stderr leaked lease token: %q", dbg.String())
		}

		// Verify nil-Transport fallback constructor does not panic.
		fallbackHC := NewHTTPClient(HTTPClientOptions{EnvironmentKey: "ek_fb"})
		if fallbackHC == nil {
			t.Error("NewHTTPClient(nil Transport) = nil")
		}

		// Verify SPEAKEASY_TEST_NAME keeps streamClient unwrapped even when --debug is enabled.
		t.Setenv("SPEAKEASY_TEST_NAME", "contract_stream_case")
		testCmd := newTestRuntimeCmd(t, srvURL)
		_ = testCmd.Root().PersistentFlags().Set("debug", "true")
		testRT, err := newRuntimeTransport(testCmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport(SPEAKEASY_TEST_NAME + debug) failed: %v", err)
		}
		if !testRT.hasTestClient || testRT.streamClient == testRT.httpClient {
			t.Errorf("newRuntimeTransport(SPEAKEASY_TEST_NAME + debug): hasTestClient=%v, streamClient==httpClient=%v, want hasTestClient=true and unwrapped streamClient", testRT.hasTestClient, testRT.streamClient == testRT.httpClient)
		}
	})
}
