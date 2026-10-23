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
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/spf13/cobra"
)

func noPositionalArgs(cmd *cobra.Command, args []string) error {
	if usageRequested(cmd) {
		return nil
	}
	if len(args) > 0 {
		return usageError(fmt.Sprintf("accepts 0 arguments, received %d", len(args)))
	}
	return nil
}

func newRuntimeLogger(cmd *cobra.Command, quiet bool) *log.Logger {
	if quiet || output.IsMachineMode(cmd) {
		return log.New(io.Discard, "", 0)
	}
	return log.New(cmd.ErrOrStderr(), "", log.LstdFlags)
}

func resolveNoRetries(cmd *cobra.Command) bool {
	if v, changed := flagutil.GetBoolFlag(cmd, "no-retries"); changed {
		return v
	}
	return config.GetString("no-retries") == "true"
}

func runtimeSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, runtimeShutdownSignals()...)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

func normalizeInteractionID(id string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(id), "interactions/")
	if !validAPIVersion(trimmed) {
		return "", fmt.Errorf("invalid interaction id %q; expected interactions/<id> or a bare id", strings.TrimSpace(id))
	}
	return trimmed, nil
}

type workerCommonFlags struct {
	envKey            string
	workerID          string
	rootDir           string
	workDir           string
	heartbeatInterval time.Duration
	commandTimeout    time.Duration
	disableBash       bool
	quiet             bool
}

func (f *workerCommonFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.envKey, "environment-key", "", "Self-hosted environment key (or GEMINI_ENVIRONMENT_KEY)")
	cmd.Flags().StringVar(&f.workerID, "worker-id", "", "Unique worker identifier (or GEMINI_WORKER_ID; default: worker-<hostname>-<pid>)")
	cmd.Flags().StringVar(&f.rootDir, "root-dir", ".", "Root directory confining built-in file tools (execute_bash is not sandboxed)")
	cmd.Flags().StringVar(&f.workDir, "workdir", ".", "Working directory for bash and relative tool paths (must reside within --root-dir)")
	cmd.Flags().DurationVar(&f.heartbeatInterval, "heartbeat-interval", defaultHeartbeatInterval, "In-band stream heartbeat interval")
	cmd.Flags().DurationVar(&f.commandTimeout, "command-timeout", 2*time.Minute, "Maximum timeout for execute_bash commands")
	cmd.Flags().Lookup("command-timeout").DefValue = "2m"
	cmd.Flags().BoolVar(&f.disableBash, "disable-bash", false, "Disable the execute_bash tool completely")
	cmd.Flags().BoolVar(&f.quiet, "quiet", false, "Suppress operational progress logs on stderr")
}

func (f *workerCommonFlags) validate() (string, string, error) {
	if _, _, err := resolveSandboxDirs(f.rootDir, f.workDir, false); err != nil {
		return "", "", usageError(err.Error())
	}
	if f.heartbeatInterval <= 0 {
		return "", "", usageError("--heartbeat-interval must be > 0")
	}
	if f.commandTimeout <= 0 {
		return "", "", usageError("--command-timeout must be > 0")
	}
	resolvedEnvKey := firstNonEmpty(f.envKey, os.Getenv("GEMINI_ENVIRONMENT_KEY"))
	if err := validateHeaderSecret("--environment-key", resolvedEnvKey); err != nil {
		return "", "", usageError(err.Error())
	}
	resolvedWorkerID := firstNonEmpty(f.workerID, os.Getenv("GEMINI_WORKER_ID"))
	return resolvedEnvKey, resolvedWorkerID, nil
}

func newEnvironmentsConnectCmd() *cobra.Command {
	var (
		common        workerCommonFlags
		maxConcurrent int
		pollInterval  time.Duration
	)

	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect this machine as a single-environment sandbox worker",
		Long: "Run a single-environment sandbox worker with interaction polling on this machine.\n\n" +
			"Polls LeaseInteractions using --environment-key (or GEMINI_ENVIRONMENT_KEY) and executes tool calls in --workdir " +
			"(file tools confined to --root-dir). Note: execute_bash runs unsandboxed on the host — run inside a container/VM or pass --disable-bash.\n\n" +
			"By default, stdout is unused and operational logs are written to stderr.",
		Example: "  gemini-api environments connect --environment-key ek_live_abc123\n" +
			"  gemini-api environments connect --environment-key ek_live_abc123 --root-dir ./sandbox --workdir ./sandbox/project",
		Args: noPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if usageRequested(cmd) {
				return emitUsageKDL(cmd, cmd.OutOrStdout())
			}
			resolvedEnvKey, resolvedWorkerID, err := common.validate()
			if err != nil {
				return err
			}
			if maxConcurrent <= 0 {
				return usageError("--max-concurrent must be >= 1")
			}
			if pollInterval <= 0 {
				return usageError("--poll-interval must be > 0")
			}

			rt, err := newRuntimeTransport(cmd, resolvedEnvKey != "")
			if err != nil {
				return output.Error(cmd, err)
			}

			envClient := NewHTTPClient(HTTPClientOptions{
				Transport:      rt,
				EnvironmentKey: resolvedEnvKey,
			})

			if isDryRun(cmd) {
				_, err := envClient.LeaseInteractions(cmd.Context(), firstNonEmpty(resolvedWorkerID, "worker-dry-run"), int32(maxConcurrent))
				if err != nil {
					return output.Error(cmd, err)
				}
				return nil
			}

			if resolvedEnvKey == "" {
				return output.UsageHelpError(cmd, errors.New(
					"missing credentials: pass --environment-key (or GEMINI_ENVIRONMENT_KEY) for 'environments connect'",
				))
			}

			absRoot, absWork, err := resolveSandboxDirs(common.rootDir, common.workDir, true)
			if err != nil {
				return usageError(err.Error())
			}

			ctx, stop := runtimeSignalContext(cmd.Context())
			defer stop()

			worker := newPullWorker(envClient, newFSServer(absRoot, absWork), newShellServer(absWork), pullWorkerOptions{
				WorkerID:          resolvedWorkerID,
				MaxConcurrent:     maxConcurrent,
				PollInterval:      pollInterval,
				HeartbeatInterval: common.heartbeatInterval,
				CommandTimeout:    common.commandTimeout,
				DisableBash:       common.disableBash,
				NoRetries:         resolveNoRetries(cmd),
				Logger:            newRuntimeLogger(cmd, common.quiet),
			})

			if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return output.Error(cmd, err)
			}
			return nil
		},
	}

	helpMeta(
		cmd,
		"root-dir . · workdir . · max-concurrent 4 · poll-interval 2s",
		"https://ai.google.dev/gemini-api/docs/agent-environment",
		"multi-tenant orchestration via gemini-api environments poll",
	)

	common.register(cmd)
	cmd.Flags().IntVar(&maxConcurrent, "max-concurrent", 4, "Maximum concurrent interactions to execute on this machine")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 2*time.Second, "Base polling interval between LeaseInteractions requests")

	return cmd
}

func newEnvironmentsPollCmd() *cobra.Command {
	var (
		envKey                   string
		environmentID            string
		pollerID                 string
		maxInteractions          int
		pollInterval             time.Duration
		provisionCmd             string
		provisionTimeout         time.Duration
		handoffHeartbeatInterval time.Duration
		quiet                    bool
	)

	cmd := &cobra.Command{
		Use:   "poll",
		Short: "Poll for queued interactions and dispatch them to sandbox workers",
		Long: "Run a stateless control-plane poller that dispatches leased interactions to external workers.\n\n" +
			"Polls LeaseInteractions and invokes --provision-cmd with a v1 JSON payload on stdin " +
			"({\"schema_version\":\"v1\",\"interaction_id\",\"environment_id\",\"lease_token\",\"server_url\",\"environment_config\"}) " +
			"while maintaining handoff heartbeats until --provision-cmd completes.\n\n" +
			"By default, stdout is unused and operational logs are written to stderr.",
		Example: "  gemini-api environments poll --provision-cmd ./scripts/provision-worker.sh\n" +
			"  gemini-api environments poll --environment-key ek_live_abc123 --provision-cmd ./scripts/provision.sh",
		Args: noPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if usageRequested(cmd) {
				return emitUsageKDL(cmd, cmd.OutOrStdout())
			}
			if maxInteractions <= 0 {
				return usageError("--max-interactions must be >= 1")
			}
			if pollInterval <= 0 {
				return usageError("--poll-interval must be > 0")
			}
			if provisionTimeout <= 0 {
				return usageError("--provision-timeout must be > 0")
			}
			if handoffHeartbeatInterval <= 0 {
				return usageError("--handoff-heartbeat-interval must be > 0")
			}

			resolvedEnvKey := firstNonEmpty(envKey, os.Getenv("GEMINI_ENVIRONMENT_KEY"))
			if err := validateHeaderSecret("--environment-key", resolvedEnvKey); err != nil {
				return usageError(err.Error())
			}
			rawEnvID := firstNonEmpty(environmentID, os.Getenv("GEMINI_ENVIRONMENT_ID"))
			var resolvedEnvID string
			if rawEnvID != "" {
				normID, err := normalizeInteractionID(strings.TrimPrefix(strings.TrimSpace(rawEnvID), "environments/"))
				if err != nil {
					return usageError(fmt.Sprintf("invalid environment id %q; expected environments/<id> or a bare id", strings.TrimSpace(rawEnvID)))
				}
				resolvedEnvID = normID
			}
			resolvedPollerID := firstNonEmpty(pollerID, os.Getenv("GEMINI_WORKER_ID"))

			rt, err := newRuntimeTransport(cmd, resolvedEnvKey != "")
			if err != nil {
				return output.Error(cmd, err)
			}

			envClient := NewHTTPClient(HTTPClientOptions{
				Transport:      rt,
				EnvironmentID:  resolvedEnvID,
				EnvironmentKey: resolvedEnvKey,
			})

			if isDryRun(cmd) {
				_, err := envClient.LeaseInteractions(cmd.Context(), firstNonEmpty(resolvedPollerID, "poller-dry-run"), int32(maxInteractions))
				if err != nil {
					return output.Error(cmd, err)
				}
				return nil
			}

			if strings.TrimSpace(provisionCmd) == "" {
				return output.UsageHelpError(cmd, errors.New("--provision-cmd is required for 'environments poll'"))
			}
			if resolvedEnvKey == "" && strings.TrimSpace(rt.apiKey) == "" && strings.TrimSpace(rt.accessToken) == "" {
				return output.UsageHelpError(cmd, errors.New(
					"missing credentials: pass --api-key (GEMINI_API_KEY), --access-token (GEMINI_ACCESS_TOKEN), or --environment-key (GEMINI_ENVIRONMENT_KEY) for 'environments poll'",
				))
			}

			ctx, stop := runtimeSignalContext(cmd.Context())
			defer stop()

			poller := NewPollerController(envClient, NewCommandWorkerProvisioner(provisionCmd), PollerOptions{
				PollerID:                 resolvedPollerID,
				MaxInteractions:          maxInteractions,
				PollInterval:             pollInterval,
				ProvisionTimeout:         provisionTimeout,
				HandoffHeartbeatInterval: handoffHeartbeatInterval,
				NoRetries:                resolveNoRetries(cmd),
				Logger:                   newRuntimeLogger(cmd, quiet),
			})

			if err := poller.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return output.Error(cmd, err)
			}
			return nil
		},
	}

	helpMeta(
		cmd,
		"max-interactions 10 · poll-interval 2s · provision-timeout 2m · handoff-heartbeat-interval 5s",
		"https://ai.google.dev/gemini-api/docs/agent-environment",
		"single-node execution via gemini-api environments connect",
	)

	cmd.Flags().StringVar(&envKey, "environment-key", "", "Self-hosted environment key (or GEMINI_ENVIRONMENT_KEY)")
	cmd.Flags().StringVar(&environmentID, "environment", "", "Optional environment_id scope for polling (or GEMINI_ENVIRONMENT_ID)")
	cmd.Flags().StringVar(&pollerID, "poller-id", "", "Unique poller identifier (or GEMINI_WORKER_ID; default: poller-<hostname>-<pid>)")
	cmd.Flags().IntVar(&maxInteractions, "max-interactions", 10, "Maximum interactions to request per LeaseInteractions call")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 2*time.Second, "Polling interval between LeaseInteractions requests")
	cmd.Flags().StringVar(&provisionCmd, "provision-cmd", "", "Shell command executed via sh -c to provision a worker and dispatch a leased interaction (receives v1 JSON on stdin)")
	cmd.Flags().DurationVar(&provisionTimeout, "provision-timeout", 2*time.Minute, "Timeout for --provision-cmd handoff")
	cmd.Flags().Lookup("provision-timeout").DefValue = "2m"
	cmd.Flags().DurationVar(&handoffHeartbeatInterval, "handoff-heartbeat-interval", defaultHeartbeatInterval, "Heartbeat interval while provisioning and handing off a lease to a cold-starting worker")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Suppress operational progress logs on stderr")

	return cmd
}

func newEnvironmentsServeCmd() *cobra.Command {
	var (
		common        workerCommonFlags
		interactionID string
		leaseToken    string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Execute a pre-leased interaction in a sandbox worker",
		Long: "Run a one-shot sandbox worker for a pre-leased interaction.\n\n" +
			"Attaches to --interaction-id with --lease-token, executes tool calls in --workdir (file tools confined to --root-dir), " +
			"and exits when complete. Note: execute_bash runs unsandboxed on the host — run inside a container/VM or pass --disable-bash.\n\n" +
			"By default, stdout is unused and operational logs are written to stderr.",
		Example: "  gemini-api environments serve --interaction-id int_abc --lease-token tok_xyz",
		Args:    noPositionalArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if usageRequested(cmd) {
				return emitUsageKDL(cmd, cmd.OutOrStdout())
			}
			resolvedEnvKey, resolvedWorkerID, err := common.validate()
			if err != nil {
				return err
			}

			startupLeases, err := parseStartupLease(interactionID, leaseToken)
			if err != nil {
				return usageError(err.Error())
			}

			rt, err := newRuntimeTransport(cmd, resolvedEnvKey != "" || len(startupLeases) > 0)
			if err != nil {
				return output.Error(cmd, err)
			}

			envClient := NewHTTPClient(HTTPClientOptions{
				Transport:      rt,
				EnvironmentKey: resolvedEnvKey,
			})

			if isDryRun(cmd) {
				sampleTok := "lease_dry_run"
				if len(startupLeases) > 0 {
					sampleTok = startupLeases[0].LeaseToken
				}
				if err := envClient.PreviewConnect(cmd.Context(), sampleTok); err != nil {
					return output.Error(cmd, err)
				}
				return nil
			}

			if len(startupLeases) == 0 {
				return output.UsageHelpError(cmd, errors.New("missing lease: pass --interaction-id and --lease-token for 'environments serve'"))
			}

			absRoot, absWork, err := resolveSandboxDirs(common.rootDir, common.workDir, true)
			if err != nil {
				return usageError(err.Error())
			}

			ctx, stop := runtimeSignalContext(cmd.Context())
			defer stop()

			worker := newPullWorker(envClient, newFSServer(absRoot, absWork), newShellServer(absWork), pullWorkerOptions{
				WorkerID:          resolvedWorkerID,
				HeartbeatInterval: common.heartbeatInterval,
				CommandTimeout:    common.commandTimeout,
				DisableBash:       common.disableBash,
				NoRetries:         resolveNoRetries(cmd),
				Logger:            newRuntimeLogger(cmd, common.quiet),
			})

			if err := runStartupLeases(ctx, worker, startupLeases); err != nil && !errors.Is(err, context.Canceled) {
				return output.Error(cmd, err)
			}
			return nil
		},
	}

	helpMeta(
		cmd,
		"root-dir . · workdir . · heartbeat-interval 5s · command-timeout 2m",
		"https://ai.google.dev/gemini-api/docs/agent-environment",
		"single-node polling + worker via gemini-api environments connect",
	)

	common.register(cmd)
	cmd.Flags().StringVar(&interactionID, "interaction-id", "", "Pre-leased interaction_id to execute (requires --lease-token)")
	cmd.Flags().StringVar(&leaseToken, "lease-token", "", "Lease token for --interaction-id")

	return cmd
}

func parseStartupLease(interactionID, leaseToken string) ([]*LeasedInteraction, error) {
	rawInt := strings.TrimSpace(interactionID)
	tok := strings.TrimSpace(leaseToken)

	if (rawInt != "") != (tok != "") {
		return nil, fmt.Errorf("--interaction-id and --lease-token must be provided together")
	}
	if rawInt == "" {
		return nil, nil
	}
	normInt, err := normalizeInteractionID(rawInt)
	if err != nil {
		return nil, err
	}
	if err := validateHeaderSecret("--lease-token", tok); err != nil {
		return nil, err
	}
	return []*LeasedInteraction{{
		InteractionID: normInt,
		LeaseToken:    tok,
	}}, nil
}
