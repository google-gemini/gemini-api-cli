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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// NewRootCommand creates the top-level ate-gemini CLI command.
func NewRootCommand(version string) *cobra.Command {
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}

	rootCmd := &cobra.Command{
		Use:   "ate-gemini",
		Short: "Gemini self-hosted Agent Substrate integration CLI",
		Long: `ate-gemini integrates the Gemini API CLI (gemini-api) self-hosted runtime
with Agent Substrate v1 (ateapi + atenet-router).

It provides subcommands to:
- Generate Kubernetes and Substrate ActorTemplate manifests (manifest, manifest template)
- Run the poller controller and idle-actor sweeper wrapping gemini-api environments poll (controller)
- Run the in-actor gRPC WorkerService + /healthz daemon (actor-daemon)
- Handle worker provision and suspend JSON hooks (provision, suspend)`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.AddCommand(
		newManifestCommand(),
		newControllerCommand(),
		newActorDaemonCommand(),
		newProvisionCommand(),
		newSuspendCommand(),
		newVersionCommand(version),
	)

	return rootCmd
}

func newVersionCommand(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of ate-gemini",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version)
			return err
		},
	}
}

func newManifestCommand() *cobra.Command {
	var (
		cfg              ManifestConfig
		allowedAtespaces string
		allowedTemplates string
	)

	cmd := &cobra.Command{
		Use:     "manifest",
		Aliases: []string{"deploy"},
		Short:   "Generate Kubernetes manifests to deploy the Gemini Substrate controller and WorkerPool",
		Long: `Generate Kubernetes manifests for running Gemini self-hosted environments
on a cluster with Agent Substrate v1 installed:
- Namespace (labeled with ate.dev/api-access=true)
- ServiceAccount for the controller (gemini-substrate-controller)
- Secret for Gemini credentials (GEMINI_API_KEY)
- Substrate ate.dev/v1 WorkerPool of pre-warmed gVisor pods
- Deployment running ate-gemini controller (poller + idle-actor sweeper)

It prints YAML to stdout without touching the cluster; pipe to kubectl apply -f -.
To generate the Substrate ActorTemplate manifest (for kubectl ate), use:
  ate-gemini manifest template`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.AllowedAtespaces = ParseAllowlistCSV(firstNonEmpty(allowedAtespaces, os.Getenv("ATE_ALLOWED_ATESPACES")))
			cfg.AllowedTemplates = ParseAllowlistCSV(firstNonEmpty(allowedTemplates, os.Getenv("ATE_ALLOWED_TEMPLATES")))
			resolveManifestEnv(&cfg)
			if err := cfg.Validate(); err != nil {
				return err
			}
			return WriteManifests(cmd.OutOrStdout(), BuildManifests(cfg))
		},
	}

	cmd.Flags().StringVar(&cfg.Namespace, "namespace", "", "Kubernetes namespace and default Substrate atespace (default $ATE_NAMESPACE or "+DefaultAtespace+")")
	cmd.Flags().StringVar(&cfg.Template, "template", "", "Default Substrate ActorTemplate name (default $ATE_TEMPLATE or "+DefaultTemplate+")")
	cmd.Flags().StringVar(&allowedAtespaces, "allowed-atespaces", "", "Comma-separated allowlist of custom_config.atespace values (or '*' for any; defaults fail-closed to --namespace)")
	cmd.Flags().StringVar(&allowedTemplates, "allowed-templates", "", "Comma-separated allowlist of custom_config.template values (or '*' for any; defaults fail-closed to --template)")
	cmd.Flags().StringVar(&cfg.WorkerImage, "worker-image", "", "Digest-pinned container image containing gemini-api and ate-gemini")
	cmd.Flags().StringVar(&cfg.ControllerImage, "controller-image", "", "Controller container image (defaults to --worker-image)")
	cmd.Flags().StringVar(&cfg.WorkerPoolImage, "worker-pool-image", "", "Digest-pinned ateworker-gvisor image for the Substrate WorkerPool")
	cmd.Flags().StringVar(&cfg.AteomImage, "ateom-image", "", "Alias for --worker-pool-image")
	cmd.Flags().StringVar(&cfg.SubstrateVersion, "substrate-version", "", "Optional Agent Substrate node selector version (e.g. v1.0.0-rc.1, or $ATE_SUBSTRATE_VERSION)")
	cmd.Flags().Int32Var(&cfg.Replicas, "replicas", DefaultReplicas, "Number of pre-warmed WorkerPool pods")
	cmd.Flags().Int32Var(&cfg.ControllerReplicas, "controller-replicas", 1, "Number of controller Deployment replicas")
	cmd.Flags().StringVar(&cfg.EnvironmentID, "gemini-env-id", "", "Optional Gemini environment ID filter (or $GEMINI_ENVIRONMENT_ID)")
	cmd.Flags().StringVar(&cfg.EnvironmentKey, "gemini-env-key", "", "Optional Gemini environment key (or $GEMINI_ENVIRONMENT_KEY)")
	cmd.Flags().StringVar(&cfg.APIKey, "gemini-api-key", "", "Gemini API key for project-scoped LeaseInteractions (prefer $GEMINI_API_KEY or $GOOGLE_API_KEY over this flag)")
	cmd.Flags().StringVar(&cfg.BaseURL, "gemini-base-url", "", "Custom Gemini API base URL (or $GEMINI_API_BASE_URL)")
	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&cfg.AtenetRouterAddr, "atenet-router-addr", DefaultAtenetRouterAddr, "Substrate atenet-router mTLS endpoint")
	cmd.Flags().StringVar(&cfg.RuntimeBin, "runtime-bin", DefaultRuntimeBin, "Executable name or path of the gemini-api CLI binary")
	cmd.Flags().Int32Var(&cfg.WorkerPort, "worker-port", DefaultWorkerPort, "Port where ate-gemini actor-daemon listens inside the actor")
	cmd.Flags().IntVar(&cfg.MaxConcurrentInteractions, "max-concurrent-interactions", DefaultMaxConcurrentInteractions, "Maximum concurrent interactions leased by the poller")
	cmd.Flags().DurationVar(&cfg.WorkerIdleTimeout, "worker-idle-timeout", DefaultWorkerIdleTimeout, "Continuous idle duration before suspending RUNNING actors (must be positive; to disable the sweeper, run `ate-gemini controller --worker-idle-timeout=0`)")

	_ = cmd.MarkFlagRequired("worker-image")

	cmd.AddCommand(newManifestTemplateCommand())
	return cmd
}

func newManifestTemplateCommand() *cobra.Command {
	var cfg ManifestConfig

	cmd := &cobra.Command{
		Use:   "template",
		Short: "Generate Substrate ActorTemplate manifest for kubectl ate",
		Long: `Generate the Substrate v1 ActorTemplate YAML manifest running ate-gemini
actor-daemon and gemini-api environments serve.
It prints YAML to stdout without touching the cluster.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolveManifestEnv(&cfg)
			if strings.TrimSpace(cfg.WorkerImage) == "" {
				return errors.New("--worker-image is required")
			}
			cfg.Defaults()
			if err := validateDNSLabel("namespace", cfg.Namespace); err != nil {
				return err
			}
			if err := validateDNSLabel("template", cfg.Template); err != nil {
				return err
			}
			return WriteActorTemplate(cmd.OutOrStdout(), BuildActorTemplate(cfg))
		},
	}

	cmd.Flags().StringVar(&cfg.Namespace, "namespace", "", "Substrate atespace for the ActorTemplate (default $ATE_NAMESPACE or "+DefaultAtespace+")")
	cmd.Flags().StringVar(&cfg.Template, "template", "", "Substrate ActorTemplate name (default $ATE_TEMPLATE or "+DefaultTemplate+")")
	cmd.Flags().StringVar(&cfg.WorkerImage, "worker-image", "", "Digest-pinned worker container image containing gemini-api and ate-gemini")
	cmd.Flags().StringVar(&cfg.SnapshotsBucket, "snapshots-bucket", "", "Optional per-template snapshot storage URI (omit unless PerTemplateSnapshotLocation preview is enabled)")
	cmd.Flags().StringVar(&cfg.BaseURL, "gemini-base-url", "", "Custom Gemini API base URL (or $GEMINI_API_BASE_URL)")
	cmd.Flags().StringVar(&cfg.RuntimeBin, "runtime-bin", DefaultRuntimeBin, "Executable name or path of the gemini-api CLI binary")
	cmd.Flags().Int32Var(&cfg.WorkerPort, "worker-port", DefaultWorkerPort, "Port where ate-gemini actor-daemon listens inside the actor")
	cmd.Flags().StringVar(&cfg.DefaultWorkDir, "default-workdir", DefaultWorkDir, "Default durable workspace directory inside the actor")

	_ = cmd.MarkFlagRequired("worker-image")
	return cmd
}

func newControllerCommand() *cobra.Command {
	var (
		cfg              ControllerConfig
		allowedAtespaces string
		allowedTemplates string
	)

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the poller controller and idle-actor sweeper wrapping gemini-api environments poll",
		Long: `Launch and supervise 'gemini-api environments poll' wired with the
ate-gemini provision hook command alongside a background WorkerService.GetIdleStatus
sweeper that suspends idle actors via ateapi.SuspendActor.

Per-environment Substrate options (atespace, template, egress_policy, env, and
worker_url_template) are read dynamically from environment_config.custom_config
and validated against --allowed-atespaces and --allowed-templates.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.AllowedAtespaces = ParseAllowlistCSV(firstNonEmpty(allowedAtespaces, os.Getenv("ATE_ALLOWED_ATESPACES")))
			cfg.AllowedTemplates = ParseAllowlistCSV(firstNonEmpty(allowedTemplates, os.Getenv("ATE_ALLOWED_TEMPLATES")))
			resolveControllerEnv(&cfg)
			cfg.Stdout = cmd.OutOrStdout()
			cfg.Stderr = cmd.ErrOrStderr()
			cfg.Logger = newStderrLogger()

			if err := cfg.Defaults(); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return RunController(ctx, cfg)
		},
	}

	cmd.Flags().StringVar(&cfg.Atespace, "atespace", "", "Default Substrate atespace when omitted from custom_config (default $ATE_NAMESPACE or "+DefaultAtespace+")")
	cmd.Flags().StringVar(&cfg.Template, "template", "", "Default Substrate ActorTemplate when omitted from custom_config (default $ATE_TEMPLATE or "+DefaultTemplate+")")
	cmd.Flags().StringVar(&allowedAtespaces, "allowed-atespaces", "", "Comma-separated allowlist of custom_config.atespace values (or '*' for any; default permits only --atespace)")
	cmd.Flags().StringVar(&allowedTemplates, "allowed-templates", "", "Comma-separated allowlist of custom_config.template values (or '*' for any; default permits only --template)")
	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", "", "Substrate ateapi gRPC endpoint (default $ATEAPI_ADDR or "+DefaultAteapiAddr+")")
	cmd.Flags().StringVar(&cfg.AtenetRouterAddr, "atenet-router-addr", "", "Substrate atenet-router endpoint (default $ATENET_ROUTER_ADDR or "+DefaultAtenetRouterAddr+")")
	cmd.Flags().BoolVar(&cfg.AteapiInsecure, "ateapi-insecure", false, "Use insecure plaintext gRPC when dialing ateapi")
	cmd.Flags().StringVar(&cfg.AteapiCAFile, "ateapi-ca-file", "", "Path to CA bundle for ateapi and atenet-router TLS (default $ATEAPI_CA_FILE or "+DefaultAteapiCAFile+" if mounted)")
	cmd.Flags().BoolVar(&cfg.AtenetRouterInsecure, "atenet-router-insecure", false, "Use insecure plaintext h2c when dialing atenet-router (local testing only)")
	cmd.Flags().StringVar(&cfg.AtenetClientBundle, "atenet-client-bundle", "", "Path to podidentity client cert/key PEM bundle for atenet-router mTLS (default $ATENET_CLIENT_BUNDLE or "+DefaultPodIdentityBundleFile+" if mounted)")
	cmd.Flags().StringVar(&cfg.TokenPath, "token-path", DefaultTokenPath, "Path to projected ateapi ServiceAccount token")
	cmd.Flags().StringVar(&cfg.RuntimeBin, "runtime-bin", "", "Executable name or path of the gemini-api CLI binary (default $GEMINI_RUNTIME_BIN or "+DefaultRuntimeBin+")")
	cmd.Flags().StringVar(&cfg.ControlPlaneURL, "control-plane-url", "", "Gemini API control-plane URL (default $GEMINI_API_BASE_URL)")
	cmd.Flags().StringVar(&cfg.EnvironmentID, "environment-id", "", "Optional Gemini environment ID filter (default $GEMINI_ENVIRONMENT_ID)")
	cmd.Flags().StringVar(&cfg.WorkerID, "worker-id", "", "Optional worker ID reported in LeaseInteractions")
	cmd.Flags().IntVar(&cfg.MaxConcurrentInteractions, "max-concurrent-interactions", DefaultMaxConcurrentInteractions, "Maximum concurrent interactions")
	cmd.Flags().DurationVar(&cfg.PollInterval, "poll-interval", DefaultPollInterval, "Polling interval for LeaseInteractions")
	cmd.Flags().DurationVar(&cfg.WorkerIdleTimeout, "worker-idle-timeout", DefaultWorkerIdleTimeout, "Continuous idle duration before suspending RUNNING actors (0 to disable)")
	cmd.Flags().DurationVar(&cfg.IdleSweepInterval, "idle-sweep-interval", DefaultIdleSweepInterval, "Interval between WorkerService.GetIdleStatus sweeps")

	return cmd
}

func newActorDaemonCommand() *cobra.Command {
	var cfg ActorDaemonConfig

	cmd := &cobra.Command{
		Use:   "actor-daemon",
		Short: "Run the in-actor HTTP wrapper that serves /healthz and spawns gemini-api environments serve",
		Long: `Run the lightweight HTTP/gRPC server inside a Substrate actor container.
Exposes GET /healthz for Substrate's wakeup probe and gRPC WorkerService.StartWork to spawn
one-shot 'gemini-api environments serve' processes for leased interactions.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg.Stderr = cmd.ErrOrStderr()
			cfg.Logger = newStderrLogger()
			cfg.Defaults()

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return RunActorDaemon(ctx, cfg)
		},
	}

	cmd.Flags().IntVar(&cfg.Port, "port", DefaultWorkerPort, "Port for gRPC WorkerService and /healthz inside the actor")
	cmd.Flags().StringVar(&cfg.RuntimeBin, "runtime-bin", DefaultRuntimeBin, "Executable name or path of the gemini-api CLI binary")
	cmd.Flags().StringVar(&cfg.RootDir, "root-dir", "/", "Filesystem root confinement directory passed to gemini-api environments serve")
	cmd.Flags().StringVar(&cfg.WorkDir, "workdir", DefaultWorkDir, "Working directory passed to gemini-api environments serve")
	cmd.Flags().StringVar(&cfg.ServerURL, "server-url", "", "Default Gemini API base URL passed to gemini-api environments serve")

	return cmd
}

func newProvisionCommand() *cobra.Command {
	var (
		atespace           string
		template           string
		allowedAtespaces   string
		allowedTemplates   string
		ateapiAddr         string
		routerAddr         string
		ateapiInsecure     bool
		ateapiCAFile       string
		routerInsecure     bool
		routerClientBundle string
		tokenPath          string
		resumeTimeout      time.Duration
	)

	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Provision or resume a Substrate actor (invoked by gemini-api environments poll --provision-cmd)",
		Long: `Read a v1 ProvisionRequest JSON object from stdin, extract Substrate
options (atespace, template, egress_policy, env, worker_url_template) from
environment_config.custom_config, validate against --allowed-atespaces and
--allowed-templates, create or resume the corresponding Substrate actor via
ateapi, and print a v1 ProvisionResponse JSON object to stdout.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			atespace = firstNonEmpty(atespace, os.Getenv("ATE_NAMESPACE"), DefaultAtespace)
			template = firstNonEmpty(template, os.Getenv("ATE_TEMPLATE"), DefaultTemplate)
			ateapiAddr = firstNonEmpty(ateapiAddr, os.Getenv("ATEAPI_ADDR"), DefaultAteapiAddr)
			ateapiCAFile = firstNonEmpty(ateapiCAFile, os.Getenv("ATEAPI_CA_FILE"))
			routerAddr = firstNonEmpty(routerAddr, os.Getenv("ATENET_ROUTER_ADDR"), DefaultAtenetRouterAddr)
			routerClientBundle = firstNonEmpty(routerClientBundle, os.Getenv("ATENET_CLIENT_BUNDLE"))

			client, conn, err := DialATE(ateapiAddr, ateapiInsecure, tokenPath, ateapiCAFile)
			if err != nil {
				return err
			}
			defer func() {
				_ = conn.Close()
			}()

			mgr := NewManager(client, ManagerConfig{
				Atespace:               atespace,
				Template:               template,
				AllowedAtespaces:       ParseAllowlistCSV(firstNonEmpty(allowedAtespaces, os.Getenv("ATE_ALLOWED_ATESPACES"))),
				AllowedTemplates:       ParseAllowlistCSV(firstNonEmpty(allowedTemplates, os.Getenv("ATE_ALLOWED_TEMPLATES"))),
				RouterAddr:             routerAddr,
				RouterInsecure:         routerInsecure,
				RouterCAFile:           ateapiCAFile,
				RouterClientBundleFile: routerClientBundle,
				ResumeTimeout:          resumeTimeout,
				Logger:                 newStderrLogger(),
			})

			return RunProvision(cmd.Context(), mgr, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&atespace, "atespace", "", "Default Substrate atespace when omitted from custom_config")
	cmd.Flags().StringVar(&template, "template", "", "Default Substrate ActorTemplate when omitted from custom_config")
	cmd.Flags().StringVar(&allowedAtespaces, "allowed-atespaces", "", "Comma-separated allowlist of custom_config.atespace values (or '*' for any)")
	cmd.Flags().StringVar(&allowedTemplates, "allowed-templates", "", "Comma-separated allowlist of custom_config.template values (or '*' for any)")
	cmd.Flags().StringVar(&ateapiAddr, "ateapi-addr", "", "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&routerAddr, "atenet-router-addr", "", "Substrate atenet-router endpoint")
	cmd.Flags().BoolVar(&ateapiInsecure, "ateapi-insecure", false, "Use insecure plaintext gRPC when dialing ateapi")
	cmd.Flags().StringVar(&ateapiCAFile, "ateapi-ca-file", "", "Path to CA bundle for ateapi and atenet-router TLS")
	cmd.Flags().BoolVar(&routerInsecure, "atenet-router-insecure", false, "Use insecure plaintext h2c when dialing atenet-router")
	cmd.Flags().StringVar(&routerClientBundle, "atenet-client-bundle", "", "Path to podidentity client cert/key PEM bundle for atenet-router mTLS")
	cmd.Flags().StringVar(&tokenPath, "token-path", DefaultTokenPath, "Path to projected ateapi ServiceAccount token")
	cmd.Flags().DurationVar(&resumeTimeout, "resume-timeout", DefaultResumeTimeout, "Timeout for creating and resuming the actor")

	return cmd
}

func newSuspendCommand() *cobra.Command {
	var (
		atespace         string
		allowedAtespaces string
		ateapiAddr       string
		ateapiInsecure   bool
		ateapiCAFile     string
		tokenPath        string
		suspendTimeout   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "suspend",
		Short: "Suspend an idle Substrate actor via ateapi.SuspendActor",
		Long: `Read a v1 SuspendRequest JSON object from stdin, resolve the target
atespace from environment_config.custom_config.atespace (defaulting to --atespace),
validate against --allowed-atespaces, and suspend the corresponding Substrate actor.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			atespace = firstNonEmpty(atespace, os.Getenv("ATE_NAMESPACE"), DefaultAtespace)
			ateapiAddr = firstNonEmpty(ateapiAddr, os.Getenv("ATEAPI_ADDR"), DefaultAteapiAddr)
			ateapiCAFile = firstNonEmpty(ateapiCAFile, os.Getenv("ATEAPI_CA_FILE"))

			client, conn, err := DialATE(ateapiAddr, ateapiInsecure, tokenPath, ateapiCAFile)
			if err != nil {
				return err
			}
			defer func() {
				_ = conn.Close()
			}()

			mgr := NewManager(client, ManagerConfig{
				Atespace:         atespace,
				AllowedAtespaces: ParseAllowlistCSV(firstNonEmpty(allowedAtespaces, os.Getenv("ATE_ALLOWED_ATESPACES"))),
				SuspendTimeout:   suspendTimeout,
				Logger:           newStderrLogger(),
			})

			return RunSuspend(cmd.Context(), mgr, cmd.InOrStdin())
		},
	}

	cmd.Flags().StringVar(&atespace, "atespace", "", "Default Substrate atespace when omitted from custom_config")
	cmd.Flags().StringVar(&allowedAtespaces, "allowed-atespaces", "", "Comma-separated allowlist of custom_config.atespace values (or '*' for any)")
	cmd.Flags().StringVar(&ateapiAddr, "ateapi-addr", "", "Substrate ateapi gRPC endpoint")
	cmd.Flags().BoolVar(&ateapiInsecure, "ateapi-insecure", false, "Use insecure plaintext gRPC when dialing ateapi")
	cmd.Flags().StringVar(&ateapiCAFile, "ateapi-ca-file", "", "Path to CA bundle for ateapi TLS")
	cmd.Flags().StringVar(&tokenPath, "token-path", DefaultTokenPath, "Path to projected ateapi ServiceAccount token")
	cmd.Flags().DurationVar(&suspendTimeout, "suspend-timeout", DefaultSuspendTimeout, "Timeout for suspending the actor")

	return cmd
}

func resolveManifestEnv(cfg *ManifestConfig) {
	cfg.Namespace = firstNonEmpty(cfg.Namespace, os.Getenv("ATE_NAMESPACE"), DefaultAtespace)
	cfg.Template = firstNonEmpty(cfg.Template, os.Getenv("ATE_TEMPLATE"), DefaultTemplate)
	cfg.SubstrateVersion = firstNonEmpty(cfg.SubstrateVersion, os.Getenv("ATE_SUBSTRATE_VERSION"))
	cfg.APIKey = firstNonEmpty(cfg.APIKey, os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY"))
	cfg.EnvironmentID = firstNonEmpty(cfg.EnvironmentID, os.Getenv("GEMINI_ENVIRONMENT_ID"))
	cfg.EnvironmentKey = firstNonEmpty(cfg.EnvironmentKey, os.Getenv("GEMINI_ENVIRONMENT_KEY"))
	cfg.BaseURL = firstNonEmpty(cfg.BaseURL, os.Getenv("GEMINI_API_BASE_URL"))
}

func resolveControllerEnv(cfg *ControllerConfig) {
	cfg.Atespace = firstNonEmpty(cfg.Atespace, os.Getenv("ATE_NAMESPACE"), DefaultAtespace)
	cfg.Template = firstNonEmpty(cfg.Template, os.Getenv("ATE_TEMPLATE"), DefaultTemplate)
	cfg.AteapiAddr = firstNonEmpty(cfg.AteapiAddr, os.Getenv("ATEAPI_ADDR"), DefaultAteapiAddr)
	cfg.AteapiCAFile = firstNonEmpty(cfg.AteapiCAFile, os.Getenv("ATEAPI_CA_FILE"))
	cfg.AtenetRouterAddr = firstNonEmpty(cfg.AtenetRouterAddr, os.Getenv("ATENET_ROUTER_ADDR"), DefaultAtenetRouterAddr)
	cfg.AtenetClientBundle = firstNonEmpty(cfg.AtenetClientBundle, os.Getenv("ATENET_CLIENT_BUNDLE"))
	cfg.RuntimeBin = firstNonEmpty(cfg.RuntimeBin, os.Getenv("GEMINI_RUNTIME_BIN"), DefaultRuntimeBin)
	cfg.ControlPlaneURL = firstNonEmpty(cfg.ControlPlaneURL, os.Getenv("GEMINI_API_BASE_URL"))
	cfg.EnvironmentID = firstNonEmpty(cfg.EnvironmentID, os.Getenv("GEMINI_ENVIRONMENT_ID"))
}

func newStderrLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}
