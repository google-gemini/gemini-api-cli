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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	workerpb "github.com/google-gemini/gemini-api-cli/docs/self-hosted/agent-substrate/pkg/workerpb"
	ategrpc "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	// DefaultMaxConcurrentInteractions is the default concurrency limit passed to
	// the poller.
	DefaultMaxConcurrentInteractions = 10

	// DefaultPollInterval is the default polling interval passed to the poller.
	DefaultPollInterval = 1 * time.Second

	// DefaultWorkerIdleTimeout is the default continuous idle duration after which
	// the controller suspends a RUNNING actor via ateapi.SuspendActor.
	DefaultWorkerIdleTimeout = 60 * time.Second

	// DefaultIdleSweepInterval is the default interval between background
	// WorkerService.GetIdleStatus sweeps across RUNNING actors.
	DefaultIdleSweepInterval = 10 * time.Second
)

// ControllerConfig configures the ate-gemini controller wrapper around
// `gemini-api environments poll` and the background idle-actor sweeper.
// Per-environment Substrate options (such as `atespace`, `template`, `egress_policy`,
// `env`, and `worker_url_template`) are read dynamically from
// `environment_config.custom_config` by `ate-gemini provision`, subject to the
// operator's `AllowedAtespaces` and `AllowedTemplates` allowlists.
type ControllerConfig struct {
	RuntimeBin                string
	SelfBin                   string
	Atespace                  string
	Template                  string
	AllowedAtespaces          []string
	AllowedTemplates          []string
	AteapiAddr                string
	AtenetRouterAddr          string
	AteapiInsecure            bool
	AteapiCAFile              string
	AtenetRouterInsecure      bool
	AtenetClientBundle        string
	TokenPath                 string
	ControlPlaneURL           string
	EnvironmentID             string
	WorkerID                  string
	MaxConcurrentInteractions int
	PollInterval              time.Duration
	WorkerIdleTimeout         time.Duration
	IdleSweepInterval         time.Duration
	ResumeCooldown            time.Duration
	ExtraRuntimeArgs          []string
	ControlClient             ategrpc.ControlClient
	RouterDialOpts            []grpc.DialOption
	NowFunc                   func() time.Time
	Logger                    *slog.Logger
	Stdout                    io.Writer
	Stderr                    io.Writer
}

// Defaults fills in default values on cfg.
func (cfg *ControllerConfig) Defaults() error {
	if strings.TrimSpace(cfg.RuntimeBin) == "" {
		cfg.RuntimeBin = DefaultRuntimeBin
	}
	if strings.TrimSpace(cfg.SelfBin) == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolving current executable path: %w", err)
		}
		cfg.SelfBin = exe
	}
	if strings.TrimSpace(cfg.AteapiAddr) == "" {
		cfg.AteapiAddr = DefaultAteapiAddr
	}
	if strings.TrimSpace(cfg.AtenetRouterAddr) == "" {
		cfg.AtenetRouterAddr = DefaultAtenetRouterAddr
	}
	if strings.TrimSpace(cfg.ControlPlaneURL) == "" {
		cfg.ControlPlaneURL = DefaultControlPlaneURL
	}
	if cfg.MaxConcurrentInteractions <= 0 {
		cfg.MaxConcurrentInteractions = DefaultMaxConcurrentInteractions
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	return nil
}

func shellQuoteToken(token string) (string, error) {
	if token == "" {
		return "", errors.New("provision command token must not be empty")
	}
	if strings.ContainsAny(token, "\x00\r\n") {
		return "", fmt.Errorf("provision command token %q must not contain newline or NUL characters", token)
	}
	if isSafeUnquotedToken(token) {
		return token, nil
	}
	return "'" + strings.ReplaceAll(token, "'", `'\''`) + "'", nil
}

func isSafeUnquotedToken(token string) bool {
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
		case c == '_' || c == '.' || c == '/' || c == ':' || c == '@' || c == '+' || c == '=' || c == '-':
		default:
			return false
		}
	}
	return true
}

func normalizeCSVFlagValues(entries []string) string {
	var cleaned []string
	for _, raw := range entries {
		cleaned = append(cleaned, ParseAllowlistCSV(raw)...)
	}
	return strings.Join(cleaned, ",")
}

// BuildPollerArgs builds the command-line flags passed to the Gemini API CLI
// (`gemini-api environments poll`) binary.
func BuildPollerArgs(cfg ControllerConfig) ([]string, error) {
	if err := cfg.Defaults(); err != nil {
		return nil, err
	}
	if strings.ContainsAny(cfg.SelfBin, " \t\n\r\x00") {
		return nil, fmt.Errorf("self binary path %q must not contain whitespace or control characters", cfg.SelfBin)
	}

	rawProvisionFields := []string{
		cfg.SelfBin,
		"provision",
		"--ateapi-addr=" + cfg.AteapiAddr,
		"--atenet-router-addr=" + cfg.AtenetRouterAddr,
		// Always forward the effective defaults so the provision hook never
		// diverges from the idle sweeper via inherited ATE_NAMESPACE/ATE_TEMPLATE.
		"--atespace=" + firstNonEmpty(cfg.Atespace, DefaultAtespace),
		"--template=" + firstNonEmpty(cfg.Template, DefaultTemplate),
	}
	if allowedAS := normalizeCSVFlagValues(cfg.AllowedAtespaces); allowedAS != "" {
		rawProvisionFields = append(rawProvisionFields, "--allowed-atespaces="+allowedAS)
	}
	if allowedTmpl := normalizeCSVFlagValues(cfg.AllowedTemplates); allowedTmpl != "" {
		rawProvisionFields = append(rawProvisionFields, "--allowed-templates="+allowedTmpl)
	}
	if cfg.AteapiInsecure {
		rawProvisionFields = append(rawProvisionFields, "--ateapi-insecure")
	}
	if caFile := strings.TrimSpace(cfg.AteapiCAFile); caFile != "" {
		rawProvisionFields = append(rawProvisionFields, "--ateapi-ca-file="+caFile)
	}
	if cfg.AtenetRouterInsecure {
		rawProvisionFields = append(rawProvisionFields, "--atenet-router-insecure")
	}
	if bundle := strings.TrimSpace(cfg.AtenetClientBundle); bundle != "" {
		rawProvisionFields = append(rawProvisionFields, "--atenet-client-bundle="+bundle)
	}
	if tokenPath := strings.TrimSpace(cfg.TokenPath); tokenPath != "" {
		rawProvisionFields = append(rawProvisionFields, "--token-path="+tokenPath)
	}

	quotedFields := make([]string, 0, len(rawProvisionFields))
	for _, field := range rawProvisionFields {
		quoted, err := shellQuoteToken(field)
		if err != nil {
			return nil, err
		}
		quotedFields = append(quotedFields, quoted)
	}

	args := []string{
		"environments",
		"poll",
		fmt.Sprintf("--max-interactions=%d", cfg.MaxConcurrentInteractions),
		"--poll-interval=" + cfg.PollInterval.String(),
		"--provision-cmd=" + strings.Join(quotedFields, " "),
	}
	if u := strings.TrimSpace(cfg.ControlPlaneURL); u != "" {
		args = append(args, "--server-url="+u)
	}
	if envID := strings.TrimSpace(cfg.EnvironmentID); envID != "" {
		args = append(args, "--environment="+envID)
	}
	if workerID := strings.TrimSpace(cfg.WorkerID); workerID != "" {
		args = append(args, "--poller-id="+workerID)
	}
	args = append(args, cfg.ExtraRuntimeArgs...)
	return args, nil
}

// RunController runs the Gemini API CLI (`gemini-api environments poll`) wired
// to the ate-gemini provision hook command, alongside a background idle-actor
// sweeper when `cfg.WorkerIdleTimeout > 0`.
func RunController(ctx context.Context, cfg ControllerConfig) error {
	if err := cfg.Defaults(); err != nil {
		return err
	}

	args, err := BuildPollerArgs(cfg)
	if err != nil {
		return err
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	sweeperDone := make(chan struct{})
	if cfg.WorkerIdleTimeout > 0 {
		go func() {
			defer close(sweeperDone)
			runIdleSweeper(runCtx, cfg)
		}()
	} else {
		close(sweeperDone)
	}
	defer func() {
		cancelRun()
		<-sweeperDone
	}()

	cfg.Logger.Info("Starting gemini-api environments poll controller",
		slog.String("runtime_bin", cfg.RuntimeBin),
		slog.String("ateapi_addr", cfg.AteapiAddr),
		slog.String("atenet_router_addr", cfg.AtenetRouterAddr),
		slog.Duration("worker_idle_timeout", cfg.WorkerIdleTimeout),
	)

	cmd := exec.CommandContext(runCtx, cfg.RuntimeBin, args...)
	cmd.Stdout = cfg.Stdout
	cmd.Stderr = cfg.Stderr
	cmd.Env = os.Environ()
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 30 * time.Second

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return fmt.Errorf("running %s: %w", cfg.RuntimeBin, err)
	}
	return nil
}

func resolveSweepInterval(cfg ControllerConfig) time.Duration {
	interval := cfg.IdleSweepInterval
	if interval <= 0 {
		interval = DefaultIdleSweepInterval
	}
	if cfg.WorkerIdleTimeout > 0 && interval > cfg.WorkerIdleTimeout/2 {
		half := cfg.WorkerIdleTimeout / 2
		if half < 25*time.Millisecond {
			half = 25 * time.Millisecond
		}
		interval = half
	}
	return interval
}

func runIdleSweeper(ctx context.Context, cfg ControllerConfig) {
	nowFunc := cfg.NowFunc
	if nowFunc == nil {
		nowFunc = time.Now
	}

	var (
		ateClient  = cfg.ControlClient
		ateConn    *grpc.ClientConn
		mgr        *Manager
		routerConn *grpc.ClientConn
	)
	defer func() {
		if routerConn != nil {
			_ = routerConn.Close()
		}
		if ateConn != nil {
			_ = ateConn.Close()
		}
	}()

	ensureClients := func() bool {
		if ateClient == nil {
			c, conn, err := DialATE(cfg.AteapiAddr, cfg.AteapiInsecure, cfg.TokenPath, cfg.AteapiCAFile)
			if err != nil {
				cfg.Logger.Warn("Idle sweeper failed to dial ateapi; retrying next sweep",
					slog.String("error", err.Error()),
				)
				return false
			}
			ateClient = c
			ateConn = conn
		}
		if mgr == nil {
			mgr = NewManager(ateClient, ManagerConfig{
				Atespace:               cfg.Atespace,
				Template:               cfg.Template,
				AllowedAtespaces:       cfg.AllowedAtespaces,
				AllowedTemplates:       cfg.AllowedTemplates,
				RouterAddr:             cfg.AtenetRouterAddr,
				RouterInsecure:         cfg.AtenetRouterInsecure,
				RouterCAFile:           cfg.AteapiCAFile,
				RouterClientBundleFile: cfg.AtenetClientBundle,
				ResumeCooldown:         cfg.ResumeCooldown,
				Logger:                 cfg.Logger,
				NowFunc:                nowFunc,
			})
		}
		if routerConn == nil {
			var (
				conn *grpc.ClientConn
				err  error
			)
			if len(cfg.RouterDialOpts) > 0 {
				routerTarget := strings.TrimSpace(mgr.RouterAddr())
				routerTarget = strings.TrimPrefix(routerTarget, "http://")
				routerTarget = strings.TrimPrefix(routerTarget, "https://")
				routerTarget = strings.TrimRight(routerTarget, "/")
				conn, err = grpc.NewClient(routerTarget, cfg.RouterDialOpts...)
			} else {
				conn, err = mgr.DialRouter()
			}
			if err != nil {
				cfg.Logger.Warn("Idle sweeper failed to dial atenet-router; retrying next sweep",
					slog.String("error", err.Error()),
				)
				return false
			}
			routerConn = conn
		}
		return true
	}

	idleSince := make(map[string]time.Time)
	ticker := time.NewTicker(resolveSweepInterval(cfg))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !ensureClients() {
				continue
			}
			sweepIdleActorsOnce(ctx, mgr, workerpb.NewWorkerServiceClient(routerConn), cfg.WorkerIdleTimeout, nowFunc, idleSince, cfg.Logger)
		}
	}
}

func sweepIdleActorsOnce(
	ctx context.Context,
	mgr *Manager,
	workerClient workerpb.WorkerServiceClient,
	idleTimeout time.Duration,
	nowFunc func() time.Time,
	idleSince map[string]time.Time,
	logger *slog.Logger,
) {
	actors, err := mgr.ListRunningActors(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("Idle sweeper failed to list running actors", slog.String("error", err.Error()))
		}
		return
	}

	seenRunning := make(map[string]bool, len(actors))
	for _, actor := range actors {
		if ctx.Err() != nil {
			return
		}
		as := actor.GetMetadata().GetAtespace()
		name := actor.GetMetadata().GetName()
		key := as + "/" + name
		seenRunning[key] = true

		rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		rpcCtx = metadata.AppendToOutgoingContext(
			rpcCtx,
			TargetActorHeader, key,
			EnvoyTimeoutHeader, "10000",
		)
		resp, err := workerClient.GetIdleStatus(rpcCtx, &workerpb.GetIdleStatusRequest{})
		cancel()
		if err != nil {
			delete(idleSince, key)
			if ctx.Err() == nil {
				logger.Debug("Idle sweeper GetIdleStatus probe failed",
					slog.String("actor", key),
					slog.String("error", err.Error()),
				)
			}
			continue
		}

		if resp.GetRunning() || resp.GetActiveInteractions() > 0 {
			delete(idleSince, key)
			continue
		}

		now := nowFunc()
		firstIdle, ok := idleSince[key]
		if !ok {
			idleSince[key] = now
			continue
		}
		if now.Sub(firstIdle) < idleTimeout {
			continue
		}

		if err := mgr.SuspendInAtespace(ctx, as, "", name); err != nil {
			if ctx.Err() == nil {
				logger.Warn("Idle sweeper failed to suspend idle actor",
					slog.String("actor", key),
					slog.String("error", err.Error()),
				)
			}
			continue
		}
		delete(idleSince, key)
	}

	for key := range idleSince {
		if !seenRunning[key] {
			delete(idleSince, key)
		}
	}
}
