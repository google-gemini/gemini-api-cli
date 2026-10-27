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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// WorkerProvisionSchemaVersionV1 is the v1 schema version for worker provision and suspend JSON contracts.
	WorkerProvisionSchemaVersionV1 = "v1"

	// WorkerStatusAccepted is the status returned by StartWork when an interaction dispatch is accepted.
	WorkerStatusAccepted = "accepted"

	// DefaultPollerMaxConcurrentInteractions is the default maximum concurrent in-flight handoffs for Tier2Poller.
	DefaultPollerMaxConcurrentInteractions = 16

	// DefaultMaxHandoffRetries is the default consecutive StartWork failure limit before poison-pill cooldown.
	DefaultMaxHandoffRetries = 3

	// DefaultHandoffRetryBackoff is the default initial backoff between handoff retries.
	DefaultHandoffRetryBackoff = 50 * time.Millisecond

	// DefaultPoisonCooldown is the default duration during which an interaction that exhausted retries is skipped.
	DefaultPoisonCooldown = 30 * time.Second

	// DefaultSweepInterval is the default interval between idle worker sweeps.
	DefaultSweepInterval = 10 * time.Second

	// DefaultWorkerIdleTimeout is the default idle duration before a Tier-2 worker is suspended.
	DefaultWorkerIdleTimeout = 60 * time.Second

	// DefaultWorkerResumeGracePeriod is the default cooldown window after provisioning before idle sweeps apply.
	DefaultWorkerResumeGracePeriod = 3 * time.Second

	// DefaultDrainTimeout is the default graceful shutdown drain timeout for in-flight handoffs.
	DefaultDrainTimeout = 15 * time.Second

	// DefaultLeaseCallTimeout bounds a single LeaseInteractions RPC call in Tier2Poller.
	DefaultLeaseCallTimeout = 10 * time.Second

	// DefaultHandoffAttemptTimeout bounds a single worker provisioning + StartWork handoff cycle.
	DefaultHandoffAttemptTimeout = 2 * time.Minute

	maxCommandErrorBytes = 256
)

// SingleflightResult holds the outcome delivered on a Singleflight.DoChan channel.
type SingleflightResult struct {
	Val    any
	Err    error
	Shared bool
}

type singleflightCall struct {
	wg    sync.WaitGroup
	val   any
	err   error
	dups  int
	chans []chan<- SingleflightResult
}

// Singleflight deduplicates concurrent in-flight function calls keyed by string using only the Go standard library.
type Singleflight struct {
	mu sync.Mutex
	m  map[string]*singleflightCall
}

// Do executes fn once for concurrent callers sharing key, returning (val, shared, err).
func (g *Singleflight) Do(key string, fn func() (any, error)) (any, bool, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*singleflightCall)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, true, c.err
	}
	c := &singleflightCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	g.doCall(c, key, fn)
	return c.val, c.dups > 0, c.err
}

// DoChan executes fn once for concurrent callers sharing key and delivers the result on a 1-buffered channel.
func (g *Singleflight) DoChan(key string, fn func() (any, error)) <-chan SingleflightResult {
	ch := make(chan SingleflightResult, 1)
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*singleflightCall)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		c.chans = append(c.chans, ch)
		g.mu.Unlock()
		return ch
	}
	c := &singleflightCall{chans: []chan<- SingleflightResult{ch}}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	go g.doCall(c, key, fn)
	return ch
}

func (g *Singleflight) doCall(c *singleflightCall, key string, fn func() (any, error)) {
	normalReturn := false
	defer func() {
		if !normalReturn {
			if r := recover(); r != nil {
				c.err = fmt.Errorf("singleflight panic: %v", r)
			} else if c.err == nil {
				c.err = errors.New("singleflight goroutine exited unexpectedly")
			}
		}
		g.mu.Lock()
		delete(g.m, key)
		chans := c.chans
		shared := c.dups > 0
		g.mu.Unlock()

		c.wg.Done()
		for _, ch := range chans {
			ch <- SingleflightResult{Val: c.val, Err: c.err, Shared: shared}
		}
	}()
	c.val, c.err = fn()
	normalReturn = true
}

// LeasePollLoopConfig configures RunLeasePollLoop for CLI workers and stateless pollers.
type LeasePollLoopConfig struct {
	Client            EnvironmentClient
	CallerID          string
	EnvironmentID     string
	EnvironmentKey    string
	MaxSlots          int
	PollInterval      time.Duration
	NoRetries         bool
	NonRetryableCheck func(error) bool
	Logger            *log.Logger
	Observer          Observer
	ErrLabel          string
	StartMsg          string
}

// RunLeasePollLoop polls LeaseInteractions up to MaxSlots concurrency and dispatches each claimed lease to handle.
func RunLeasePollLoop(ctx context.Context, cfg LeasePollLoopConfig, handle func(context.Context, *LeasedInteraction)) error {
	maxSlots := cfg.MaxSlots
	if maxSlots <= 0 {
		maxSlots = 1
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	obs := cfg.Observer
	if obs == nil {
		obs = NopObserver{}
	}

	sem := make(chan struct{}, maxSlots)
	inFlight := make(map[string]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	defer wg.Wait()

	startedLogged := false
	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		availSlots := maxSlots - len(sem)
		if availSlots <= 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		res, err := cfg.Client.LeaseInteractions(ctx, &LeaseInteractionsRequest{
			WorkerID:           cfg.CallerID,
			EnvironmentID:      cfg.EnvironmentID,
			EnvironmentKey:     cfg.EnvironmentKey,
			MaxNewInteractions: int32(availSlots),
			SendZeroMaxNew:     true,
		})
		if err != nil {
			obs.LeasePolled(0, err)
			nonRetryable := IsNonRetryableError(err) || (cfg.NonRetryableCheck != nil && cfg.NonRetryableCheck(err))
			if nonRetryable || cfg.NoRetries {
				return err
			}
			consecutiveErrors++
			delay := RetryBackoff(pollInterval, consecutiveErrors)
			if ctx.Err() == nil {
				logger.Printf("%s (retrying in %v): %v", cfg.ErrLabel, delay, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				continue
			}
		}
		obs.LeasePolled(len(res.Leases), nil)
		consecutiveErrors = 0
		if !startedLogged {
			startedLogged = true
			logger.Print(cfg.StartMsg)
		}

		if len(res.Leases) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval):
				continue
			}
		}

		for i := range res.Leases {
			item := res.Leases[i]
			if item.InteractionID == "" {
				continue
			}
			mu.Lock()
			if inFlight[item.InteractionID] {
				mu.Unlock()
				continue
			}
			inFlight[item.InteractionID] = true
			mu.Unlock()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				delete(inFlight, item.InteractionID)
				mu.Unlock()
				return ctx.Err()
			}

			wg.Add(1)
			leaseCopy := item
			go func(lease *LeasedInteraction) {
				defer wg.Done()
				defer func() { <-sem }()
				defer func() {
					mu.Lock()
					delete(inFlight, lease.InteractionID)
					mu.Unlock()
				}()
				handle(ctx, lease)
			}(&leaseCopy)
		}
	}
}

// ProvisionInput is the v1 JSON payload passed to --provision-cmd on stdin by StatelessPoller.
type ProvisionInput struct {
	SchemaVersion     string          `json:"schema_version"`
	InteractionID     string          `json:"interaction_id"`
	EnvironmentID     string          `json:"environment_id"`
	LeaseToken        string          `json:"lease_token"`
	ServerURL         string          `json:"server_url,omitempty"`
	EnvironmentConfig json.RawMessage `json:"environment_config,omitempty"`
}

// ShellCommandProvisioner dispatches a leased interaction by invoking a shell command with ProvisionInput JSON on stdin.
type ShellCommandProvisioner struct {
	provisionCmd string
}

// NewShellCommandProvisioner creates a ShellCommandProvisioner for provisionCmd.
func NewShellCommandProvisioner(provisionCmd string) *ShellCommandProvisioner {
	return &ShellCommandProvisioner{
		provisionCmd: strings.TrimSpace(provisionCmd),
	}
}

// ProvisionAndDispatch invokes provisionCmd with input serialized as JSON on stdin and LeaseToken redacted from stderr.
func (p *ShellCommandProvisioner) ProvisionAndDispatch(ctx context.Context, input ProvisionInput) error {
	if p.provisionCmd == "" {
		return fmt.Errorf("--provision-cmd is required to dispatch interaction %q (environment %q)", input.InteractionID, input.EnvironmentID)
	}
	if input.SchemaVersion == "" {
		input.SchemaVersion = WorkerProvisionSchemaVersionV1
	}

	stdinBytes, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshaling provision input: %w", err)
	}

	shellBin, shellArgs := ResolveProvisionShell(runtime.GOOS, p.provisionCmd)
	cmd := exec.CommandContext(ctx, shellBin, shellArgs...)
	cmd.Stdin = bytes.NewReader(stdinBytes)
	cmd.Env = FilterScrubbedEnv(os.Environ())
	ConfigureSubprocessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	stderr := &CappedBuffer{MaxBytes: MaxBashOutputBytes}
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		trimmed := strings.TrimSpace(stderr.String())
		if input.LeaseToken != "" {
			trimmed = strings.ReplaceAll(trimmed, input.LeaseToken, "[REDACTED]")
		}
		if trimmed != "" {
			return fmt.Errorf("provision command failed (%s): %w", trimmed, err)
		}
		return fmt.Errorf("provision command failed: %w", err)
	}
	return nil
}

// ResolveProvisionShell selects sh or cmd.exe to execute a stateless --provision-cmd string.
func ResolveProvisionShell(goos, provisionCmd string) (string, []string) {
	if pth, err := exec.LookPath("sh"); err == nil {
		return pth, []string{"-c", provisionCmd}
	}
	if goos == "windows" {
		return "cmd.exe", []string{"/C", provisionCmd}
	}
	return "sh", []string{"-c", provisionCmd}
}

// HeartbeatFunc renews a lease while a stateless poller dispatches an interaction to a cold-starting worker.
type HeartbeatFunc func(ctx context.Context, pollerID, interactionID, envID, leaseToken string) (*HeartbeatResult, error)

// StatelessPollerConfig configures a StatelessPoller (`gemini-api environments poll`).
type StatelessPollerConfig struct {
	Client                   EnvironmentClient
	Heartbeat                HeartbeatFunc
	ServerURL                string
	Provisioner              *ShellCommandProvisioner
	ProvisionFunc            func(ctx context.Context, input ProvisionInput) error
	PollerID                 string
	EnvironmentID            string
	EnvironmentKey           string
	MaxInteractions          int
	PollInterval             time.Duration
	ProvisionTimeout         time.Duration
	HandoffHeartbeatInterval time.Duration
	NoRetries                bool
	NonRetryableCheck        func(error) bool
	Logger                   *log.Logger
	Observer                 Observer
}

type statelessFailureState struct {
	consecutive int
	cooldownEnd time.Time
}

// StatelessPoller polls LeaseInteractions and dispatches leases via ShellCommandProvisioner while heartbeating cold starts.
type StatelessPoller struct {
	cfg                      StatelessPollerConfig
	pollerID                 string
	maxInteractions          int
	pollInterval             time.Duration
	provisionTimeout         time.Duration
	handoffHeartbeatInterval time.Duration
	logger                   *log.Logger
	observer                 Observer

	mu       sync.Mutex
	failures map[string]*statelessFailureState
}

// NewStatelessPoller creates a new StatelessPoller from cfg.
func NewStatelessPoller(cfg StatelessPollerConfig) *StatelessPoller {
	pollerID := strings.TrimSpace(cfg.PollerID)
	if pollerID == "" {
		pollerID = DefaultRuntimeID("poller")
	}
	maxInteractions := cfg.MaxInteractions
	if maxInteractions <= 0 {
		maxInteractions = 10
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	provisionTimeout := cfg.ProvisionTimeout
	if provisionTimeout <= 0 {
		provisionTimeout = 2 * time.Minute
	}
	hbInterval := cfg.HandoffHeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = DefaultHeartbeatInterval
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	obs := cfg.Observer
	if obs == nil {
		obs = NopObserver{}
	}
	return &StatelessPoller{
		cfg:                      cfg,
		pollerID:                 pollerID,
		maxInteractions:          maxInteractions,
		pollInterval:             pollInterval,
		provisionTimeout:         provisionTimeout,
		handoffHeartbeatInterval: hbInterval,
		logger:                   logger,
		observer:                 obs,
		failures:                 make(map[string]*statelessFailureState),
	}
}

// Run executes the stateless poller loop until ctx is canceled or a fatal non-retryable error occurs.
func (p *StatelessPoller) Run(ctx context.Context) error {
	return RunLeasePollLoop(ctx, LeasePollLoopConfig{
		Client:            p.cfg.Client,
		CallerID:          p.pollerID,
		EnvironmentID:     p.cfg.EnvironmentID,
		EnvironmentKey:    p.cfg.EnvironmentKey,
		MaxSlots:          p.maxInteractions,
		PollInterval:      p.pollInterval,
		NoRetries:         p.cfg.NoRetries,
		NonRetryableCheck: p.cfg.NonRetryableCheck,
		Logger:            p.logger,
		Observer:          p.observer,
		ErrLabel:          "Poller LeaseInteractions error",
		StartMsg:          fmt.Sprintf("PollerController started (maxInteractions=%d)", p.maxInteractions),
	}, p.dispatchLease)
}

func (p *StatelessPoller) heartbeatOnce(ctx context.Context, interactionID, envID, leaseToken string) (*HeartbeatResult, error) {
	if p.cfg.Heartbeat != nil {
		return p.cfg.Heartbeat(ctx, p.pollerID, interactionID, envID, leaseToken)
	}
	if hc, ok := p.cfg.Client.(*HTTPClient); ok {
		return hc.HeartbeatInteraction(ctx, p.pollerID, interactionID, envID, leaseToken)
	}
	resolvedEnvID := cmp.Or(envID, p.cfg.EnvironmentID)
	res, err := p.cfg.Client.LeaseInteractions(ctx, &LeaseInteractionsRequest{
		WorkerID:       p.pollerID,
		EnvironmentID:  resolvedEnvID,
		EnvironmentKey: p.cfg.EnvironmentKey,
		ActiveLeases: []LeaseEntry{{
			InteractionID: interactionID,
			LeaseToken:    leaseToken,
		}},
		MaxNewInteractions: 0,
		SendZeroMaxNew:     true,
	})
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

func (p *StatelessPoller) dispatchLease(ctx context.Context, lease *LeasedInteraction) {
	envID := cmp.Or(lease.EnvironmentID, "default")
	p.logger.Printf("[%s] Claimed interaction lease (env=%s)", lease.InteractionID, envID)

	p.mu.Lock()
	if st, ok := p.failures[lease.InteractionID]; ok && time.Now().Before(st.cooldownEnd) {
		waitDur := time.Until(st.cooldownEnd)
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(waitDur):
		}
	} else {
		p.mu.Unlock()
	}

	dispatchCtx, cancel := context.WithTimeout(ctx, p.provisionTimeout)
	defer cancel()

	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(p.handoffHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-dispatchCtx.Done():
				return
			case <-ticker.C:
				resp, err := p.heartbeatOnce(dispatchCtx, lease.InteractionID, lease.EnvironmentID, lease.LeaseToken)
				if err == nil && resp != nil && resp.ShouldCancel {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-hbDone
	}()

	serverURL := p.cfg.ServerURL
	if serverURL == "" {
		if hc, ok := p.cfg.Client.(*HTTPClient); ok {
			serverURL = hc.ServerURL()
		}
	}
	rawCfg := lease.RawEnvironmentConfig
	if len(rawCfg) == 0 && lease.EnvironmentConfig != nil {
		rawCfg = lease.EnvironmentConfig.RawJSON
	}
	input := ProvisionInput{
		SchemaVersion:     WorkerProvisionSchemaVersionV1,
		InteractionID:     lease.InteractionID,
		EnvironmentID:     envID,
		LeaseToken:        lease.LeaseToken,
		ServerURL:         serverURL,
		EnvironmentConfig: rawCfg,
	}

	var err error
	switch {
	case p.cfg.ProvisionFunc != nil:
		err = p.cfg.ProvisionFunc(dispatchCtx, input)
	case p.cfg.Provisioner != nil:
		err = p.cfg.Provisioner.ProvisionAndDispatch(dispatchCtx, input)
	default:
		err = fmt.Errorf("provisioner is not configured")
	}
	if err != nil {
		p.recordDispatchFailure(lease.InteractionID)
		p.observer.HandoffFinished("error")
		p.logger.Printf("[%s] ProvisionAndDispatch failed for env %s: %v", lease.InteractionID, envID, err)
		return
	}

	p.mu.Lock()
	delete(p.failures, lease.InteractionID)
	p.mu.Unlock()
	p.observer.HandoffFinished("accepted")
	p.logger.Printf("[%s] Dispatched interaction lease for environment %s", lease.InteractionID, envID)
}

func (p *StatelessPoller) recordDispatchFailure(interactionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for id, entry := range p.failures {
		if id != interactionID && now.Sub(entry.cooldownEnd) > 10*time.Minute {
			delete(p.failures, id)
		}
	}
	st, ok := p.failures[interactionID]
	if !ok {
		st = &statelessFailureState{}
		p.failures[interactionID] = st
	}
	st.consecutive++
	shift := min(st.consecutive-1, 5)
	st.cooldownEnd = now.Add(min(time.Duration(1<<shift)*500*time.Millisecond, 15*time.Second))
}

// WorkerProvisionEnvironmentConfig carries sandbox-only environment settings in a WorkerProvisionRequest JSON payload.
type WorkerProvisionEnvironmentConfig struct {
	NetworkMode      string              `json:"network_mode,omitempty"`
	AllowlistDomains []string            `json:"allowlist_domains,omitempty"`
	DenylistDomains  []string            `json:"denylist_domains,omitempty"`
	Env              map[string]string   `json:"env,omitempty"`
	Sources          []EnvironmentSource `json:"sources,omitempty"`
}

// WorkerProvisionRequest is the v1 JSON request sent on stdin to a Tier-2 worker provisioner.
type WorkerProvisionRequest struct {
	SchemaVersion     string                            `json:"schema_version"`
	EnvironmentID     string                            `json:"environment_id"`
	InteractionID     string                            `json:"interaction_id,omitempty"`
	EnvironmentConfig *WorkerProvisionEnvironmentConfig `json:"environment_config,omitempty"`
}

// WorkerProvisionResponse is the v1 JSON response emitted on stdout by a Tier-2 worker provisioner.
type WorkerProvisionResponse struct {
	SchemaVersion string            `json:"schema_version"`
	URI           string            `json:"uri"`
	Headers       map[string]string `json:"headers,omitempty"`
	WorkerID      string            `json:"worker_id,omitempty"`
}

// WorkerSuspendRequest is the v1 JSON request sent on stdin to a Tier-2 worker suspend command.
type WorkerSuspendRequest struct {
	SchemaVersion string `json:"schema_version"`
	EnvironmentID string `json:"environment_id"`
	WorkerID      string `json:"worker_id"`
	IdleDuration  string `json:"idle_duration,omitempty"`
}

// WorkerProvisioner provisions/resumes and suspends Tier-2 workers on behalf of Tier2Poller.
type WorkerProvisioner interface {
	ProvisionWorker(ctx context.Context, req WorkerProvisionRequest) (*WorkerProvisionResponse, error)
	SuspendWorker(ctx context.Context, req WorkerSuspendRequest) error
}

// ActiveInteractionLease represents an interaction lease held in Tier2Poller handoffLeases during worker startup.
type ActiveInteractionLease struct {
	InteractionID     string
	EnvironmentID     string
	LeaseToken        string
	EnvironmentConfig *EnvironmentConfig
}

// DirectCommandProvisioner implements WorkerProvisioner by executing configured binaries directly without a shell.
type DirectCommandProvisioner struct {
	provisionBinary string
	provisionArgs   []string
	suspendBinary   string
	suspendArgs     []string
}

// NewDirectCommandProvisioner constructs a DirectCommandProvisioner splitting command strings via strings.Fields.
func NewDirectCommandProvisioner(provisionCmd, suspendCmd string) (*DirectCommandProvisioner, error) {
	provFields := strings.Fields(provisionCmd)
	if len(provFields) == 0 {
		return nil, &PathError{Kind: PathErrInvalidArgument, Message: "worker provision command is required"}
	}
	var suspBin string
	var suspArgs []string
	if suspFields := strings.Fields(suspendCmd); len(suspFields) > 0 {
		suspBin = suspFields[0]
		suspArgs = suspFields[1:]
	}
	return &DirectCommandProvisioner{
		provisionBinary: provFields[0],
		provisionArgs:   provFields[1:],
		suspendBinary:   suspBin,
		suspendArgs:     suspArgs,
	}, nil
}

func truncateCommandOutput(s string) string {
	if trimmed := strings.TrimSpace(s); len(trimmed) > maxCommandErrorBytes {
		return strings.ToValidUTF8(trimmed[:maxCommandErrorBytes], "") + "...(truncated)"
	}
	return strings.TrimSpace(s)
}

// ProvisionWorker executes the provision binary with WorkerProvisionRequest JSON on stdin and parses WorkerProvisionResponse from stdout.
func (p *DirectCommandProvisioner) ProvisionWorker(ctx context.Context, req WorkerProvisionRequest) (*WorkerProvisionResponse, error) {
	envID := strings.TrimSpace(req.EnvironmentID)
	if envID == "" {
		return nil, &PathError{Kind: PathErrInvalidArgument, Message: "environment_id is required"}
	}
	req.SchemaVersion = WorkerProvisionSchemaVersionV1
	req.EnvironmentID = envID
	req.InteractionID = strings.TrimSpace(req.InteractionID)

	stdinBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling WorkerProvisionRequest: %w", err)
	}

	cmd := exec.CommandContext(ctx, p.provisionBinary, p.provisionArgs...)
	cmd.Stdin = bytes.NewReader(stdinBytes)
	cmd.Env = FilterScrubbedEnv(os.Environ())
	ConfigureSubprocessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	stdout := &CappedBuffer{MaxBytes: MaxBashOutputBytes}
	stderr := &CappedBuffer{MaxBytes: MaxBashOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("worker provision command failed (%v): %s", err, truncateCommandOutput(stderr.buf.String()))
	}
	if stdout.truncated {
		return nil, &PathError{Kind: PathErrInternal, Message: "worker provision command output exceeded maximum size"}
	}

	var resp WorkerProvisionResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout.buf.Bytes()), &resp); err != nil {
		return nil, fmt.Errorf("invalid WorkerProvisionResponse JSON from provision command: %w", err)
	}
	resp.URI = strings.TrimSpace(resp.URI)
	if resp.URI == "" {
		return nil, &PathError{Kind: PathErrInternal, Message: "worker provision command returned empty uri"}
	}
	if strings.TrimSpace(resp.SchemaVersion) == "" {
		resp.SchemaVersion = WorkerProvisionSchemaVersionV1
	}
	return &resp, nil
}

// SuspendWorker executes the optional suspend binary passing WorkerSuspendRequest JSON on stdin.
func (p *DirectCommandProvisioner) SuspendWorker(ctx context.Context, req WorkerSuspendRequest) error {
	if p.suspendBinary == "" {
		return nil
	}
	envID := strings.TrimSpace(req.EnvironmentID)
	workerID := strings.TrimSpace(req.WorkerID)
	if envID == "" && workerID == "" {
		return &PathError{Kind: PathErrInvalidArgument, Message: "environment_id or worker_id is required"}
	}
	req.SchemaVersion = WorkerProvisionSchemaVersionV1
	req.EnvironmentID = envID
	req.WorkerID = workerID
	req.IdleDuration = strings.TrimSpace(req.IdleDuration)

	stdinBytes, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling WorkerSuspendRequest: %w", err)
	}

	cmd := exec.CommandContext(ctx, p.suspendBinary, p.suspendArgs...)
	cmd.Stdin = bytes.NewReader(stdinBytes)
	cmd.Env = FilterScrubbedEnv(os.Environ())
	ConfigureSubprocessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	stderr := &CappedBuffer{MaxBytes: MaxBashOutputBytes}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("worker suspend command failed (%v): %s", err, truncateCommandOutput(stderr.buf.String()))
	}
	return nil
}

// ConvertToProvisionEnvironmentConfig converts a parsed *EnvironmentConfig into a *WorkerProvisionEnvironmentConfig.
func ConvertToProvisionEnvironmentConfig(cfg *EnvironmentConfig) *WorkerProvisionEnvironmentConfig {
	if cfg == nil {
		return nil
	}
	var envMap map[string]string
	for k, v := range cfg.Env {
		if envMap == nil {
			envMap = make(map[string]string)
		}
		envMap[k] = v
	}
	for k, ev := range cfg.EnvVars {
		if _, exists := envMap[k]; !exists {
			if val := cmp.Or(ev.Value, ev.Credential); val != "" {
				if envMap == nil {
					envMap = make(map[string]string)
				}
				envMap[k] = val
			}
		}
	}

	seenAllow := make(map[string]bool)
	var allowlist []string
	addAllow := func(d string) {
		d = strings.TrimSpace(d)
		if d != "" && !seenAllow[d] {
			seenAllow[d] = true
			allowlist = append(allowlist, d)
		}
	}

	seenDeny := make(map[string]bool)
	var denylist []string
	addDeny := func(d string) {
		d = strings.TrimSpace(d)
		if d != "" && !seenDeny[d] {
			seenDeny[d] = true
			denylist = append(denylist, d)
		}
	}

	for _, d := range cfg.AllowlistDomains {
		addAllow(d)
	}
	for _, d := range cfg.DenylistDomains {
		addDeny(d)
	}
	for _, rule := range cfg.NetworkAllowlist {
		if strings.EqualFold(strings.TrimSpace(rule.Mode), "deny") {
			addDeny(rule.Domain)
		} else {
			addAllow(rule.Domain)
		}
	}

	networkMode := strings.TrimSpace(cfg.NetworkMode)
	if len(bytes.TrimSpace(cfg.RawJSON)) > 0 {
		var raw rawEnvironmentConfigWire
		if err := json.Unmarshal(cfg.RawJSON, &raw); err == nil {
			for _, d := range raw.AllowlistDomains {
				addAllow(d)
			}
			for _, d := range raw.DenylistDomains {
				addDeny(d)
			}
			for _, rule := range parseNetworkAllowlist(raw.NetworkDenylist) {
				addDeny(rule.Domain)
			}
			networkMode = cmp.Or(networkMode, strings.TrimSpace(raw.NetworkMode))
		}
	}

	var sources []EnvironmentSource
	if len(cfg.Sources) > 0 {
		sources = append([]EnvironmentSource(nil), cfg.Sources...)
	}

	if len(envMap) == 0 && len(allowlist) == 0 && len(denylist) == 0 && networkMode == "" && len(sources) == 0 {
		return nil
	}
	return &WorkerProvisionEnvironmentConfig{
		NetworkMode:      networkMode,
		AllowlistDomains: allowlist,
		DenylistDomains:  denylist,
		Env:              envMap,
		Sources:          sources,
	}
}

// StartWorkRequest carries parameters dispatched from Tier2Poller to a Tier-2 WorkerRPCClient.
type StartWorkRequest struct {
	InteractionID   string
	EnvironmentID   string
	LeaseToken      string
	ControlPlaneURL string
	GRPCTarget      string
}

// StartWorkResponse carries the dispatch status returned by a Tier-2 WorkerRPCClient.
type StartWorkResponse struct {
	Status string
}

// IdleStatus carries worker activity state returned by WorkerRPCClient.GetIdleStatus.
type IdleStatus struct {
	Running      bool
	IdleDuration time.Duration
}

// WorkerRPCClient abstracts StartWork and GetIdleStatus RPC calls to a provisioned Tier-2 worker.
type WorkerRPCClient interface {
	StartWork(ctx context.Context, req *StartWorkRequest, headers map[string]string) (*StartWorkResponse, error)
	GetIdleStatus(ctx context.Context, headers map[string]string) (*IdleStatus, error)
	Close() error
}

// WorkerDialer opens a WorkerRPCClient connection to uri.
type WorkerDialer func(uri string) (WorkerRPCClient, error)

type provisionedWorkerState struct {
	EnvironmentID string
	WorkerID      string
	URI           string
	Headers       map[string]string
	LastActiveAt  time.Time
}

// Tier2PollerConfig configures a stateful Tier2Poller controller.
type Tier2PollerConfig struct {
	Client                    EnvironmentClient
	Provisioner               WorkerProvisioner
	WorkerProvisionCmd        string
	WorkerSuspendCmd          string
	DialWorker                WorkerDialer
	IdleTimeout               time.Duration
	ResumeGracePeriod         time.Duration
	EnvironmentID             string
	EnvironmentKey            string
	WorkerID                  string
	ControlPlaneURL           string
	GRPCTarget                string
	MaxConcurrentInteractions int
	PollInterval              time.Duration
	LeaseTTL                  time.Duration
	Labels                    map[string]string
	MaxHandoffRetries         int
	HandoffRetryBackoff       time.Duration
	PoisonCooldown            time.Duration
	SweepInterval             time.Duration
	DrainTimeout              time.Duration
	Now                       func() time.Time
	NewTimer                  func(d time.Duration) *time.Timer
	Logger                    *slog.Logger
	Observer                  Observer
}

// Defaults populates default values for unset fields in Tier2PollerConfig.
func (c *Tier2PollerConfig) Defaults() {
	if c.MaxConcurrentInteractions <= 0 {
		c.MaxConcurrentInteractions = DefaultPollerMaxConcurrentInteractions
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollBackoff
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = DefaultLeaseTTL
	}
	if strings.TrimSpace(c.WorkerID) == "" {
		host, _ := os.Hostname()
		c.WorkerID = cmp.Or(host, "poller-controller")
	}
	if c.MaxHandoffRetries <= 0 {
		c.MaxHandoffRetries = DefaultMaxHandoffRetries
	}
	if c.HandoffRetryBackoff <= 0 {
		c.HandoffRetryBackoff = DefaultHandoffRetryBackoff
	}
	if c.PoisonCooldown <= 0 {
		c.PoisonCooldown = DefaultPoisonCooldown
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = DefaultSweepInterval
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultWorkerIdleTimeout
	}
	if c.ResumeGracePeriod <= 0 {
		c.ResumeGracePeriod = DefaultWorkerResumeGracePeriod
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.NewTimer == nil {
		c.NewTimer = time.NewTimer
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Observer == nil {
		c.Observer = NopObserver{}
	}
}

type provFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	shared  bool
	resp    *WorkerProvisionResponse
	err     error
}

// Tier2Poller orchestrates stateful multi-environment polling, singleflight worker provisioning,
// poison-pill cooldowns, idle worker sweeps, and graceful drain heartbeats.
type Tier2Poller struct {
	cfg         Tier2PollerConfig
	client      EnvironmentClient
	provisioner WorkerProvisioner
	dialWorker  WorkerDialer
	logger      *slog.Logger
	observer    Observer

	mu             sync.Mutex
	handoffLeases  map[string]ActiveInteractionLease
	failureCounts  map[string]int
	cooldownUntil  map[string]time.Time
	deadLettered   map[string]bool
	activeWorkers  map[string]*provisionedWorkerState
	provFlights    map[string]*provFlight
	dialGroup      Singleflight
	workerConns    map[string]WorkerRPCClient
	draining       bool
	handoffWg      sync.WaitGroup
	slotReleasedCh chan struct{}
}

// NewTier2Poller constructs a Tier2Poller from cfg.
func NewTier2Poller(cfg Tier2PollerConfig) (*Tier2Poller, error) {
	if cfg.Client == nil {
		return nil, &PathError{Kind: PathErrInvalidArgument, Message: "EnvironmentClient is required for PollerController"}
	}
	provisioner := cfg.Provisioner
	if provisioner == nil && strings.TrimSpace(cfg.WorkerProvisionCmd) != "" {
		cmdProv, err := NewDirectCommandProvisioner(cfg.WorkerProvisionCmd, cfg.WorkerSuspendCmd)
		if err != nil {
			return nil, err
		}
		provisioner = cmdProv
	}
	if provisioner == nil {
		return nil, &PathError{Kind: PathErrInvalidArgument, Message: "WorkerProvisioner or WorkerProvisionCmd is required for PollerController"}
	}
	if cfg.DialWorker == nil {
		return nil, &PathError{Kind: PathErrInvalidArgument, Message: "DialWorker is required for PollerController"}
	}
	cfg.Defaults()

	return &Tier2Poller{
		cfg:            cfg,
		client:         cfg.Client,
		provisioner:    provisioner,
		dialWorker:     cfg.DialWorker,
		logger:         cfg.Logger,
		observer:       cfg.Observer,
		handoffLeases:  make(map[string]ActiveInteractionLease),
		failureCounts:  make(map[string]int),
		cooldownUntil:  make(map[string]time.Time),
		deadLettered:   make(map[string]bool),
		activeWorkers:  make(map[string]*provisionedWorkerState),
		provFlights:    make(map[string]*provFlight),
		workerConns:    make(map[string]WorkerRPCClient),
		slotReleasedCh: make(chan struct{}, 1),
	}, nil
}

// Close closes any cached WorkerRPCClient connections.
func (c *Tier2Poller) Close() error {
	c.mu.Lock()
	conns := make([]WorkerRPCClient, 0, len(c.workerConns))
	for uri, conn := range c.workerConns {
		conns = append(conns, conn)
		delete(c.workerConns, uri)
	}
	c.mu.Unlock()

	var firstErr error
	for _, conn := range conns {
		if err := conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *Tier2Poller) now() time.Time {
	return c.cfg.Now()
}

func (c *Tier2Poller) newTimer(d time.Duration) *time.Timer {
	return c.cfg.NewTimer(d)
}

func (c *Tier2Poller) notifySlotReleased() {
	select {
	case c.slotReleasedCh <- struct{}{}:
	default:
	}
}

// HandoffLeases returns a snapshot copy of leases currently held in handoffLeases.
func (c *Tier2Poller) HandoffLeases() map[string]ActiveInteractionLease {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]ActiveInteractionLease, len(c.handoffLeases))
	for k, v := range c.handoffLeases {
		out[k] = v
	}
	return out
}

// InFlightCount returns the number of interactions currently in handoff.
func (c *Tier2Poller) InFlightCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.handoffLeases)
}

// FailureCount returns the consecutive handoff failure count for interactionID.
func (c *Tier2Poller) FailureCount(interactionID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failureCounts[interactionID]
}

// IsDeadLettered reports whether interactionID exhausted MaxHandoffRetries.
func (c *Tier2Poller) IsDeadLettered(interactionID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadLettered[interactionID]
}

// InCooldown reports whether interactionID is currently in its poison-pill cooldown window.
func (c *Tier2Poller) InCooldown(interactionID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.cooldownUntil[interactionID]
	return ok && c.now().Before(until)
}

// WaitHandoffs blocks until all active handoff goroutines complete.
func (c *Tier2Poller) WaitHandoffs() {
	c.handoffWg.Wait()
}

// Run executes the Tier2Poller main loop and idle worker sweep loop until ctx is canceled, then drains in-flight handoffs.
func (c *Tier2Poller) Run(ctx context.Context) error {
	defer func() {
		_ = c.Close()
	}()

	stopHandoffsCh := make(chan struct{})
	var stopOnce sync.Once
	cancelHandoffs := func() {
		stopOnce.Do(func() {
			close(stopHandoffsCh)
		})
	}
	defer cancelHandoffs()

	bgCtx, cancelBg := context.WithCancel(ctx)
	var bgWg sync.WaitGroup
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		c.runSweepLoop(bgCtx)
	}()
	defer func() {
		cancelBg()
		bgWg.Wait()
	}()

	timer := c.newTimer(0)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()

	for {
		if ctx.Err() != nil {
			break
		}

		wasAtCapacity := c.InFlightCount() >= c.cfg.MaxConcurrentInteractions
		nextBackoff, err := c.pollOnce(ctx, stopHandoffsCh)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			c.logger.Warn("PollerController LeaseInteractions cycle failed", slog.Any("error", err))
			nextBackoff = c.cfg.PollInterval
		}
		if nextBackoff <= 0 || c.cfg.PollInterval < nextBackoff {
			nextBackoff = c.cfg.PollInterval
		}
		if c.InFlightCount() > 0 && c.cfg.LeaseTTL > 0 {
			if hbInterval := c.cfg.LeaseTTL / 3; hbInterval > 0 && hbInterval < nextBackoff {
				nextBackoff = hbInterval
			}
		}

		timer.Reset(nextBackoff)
		if wasAtCapacity {
			select {
			case <-ctx.Done():
				stopAndDrainTimer(timer)
			case <-c.slotReleasedCh:
				stopAndDrainTimer(timer)
			case <-timer.C:
			}
		} else {
			select {
			case <-ctx.Done():
				stopAndDrainTimer(timer)
			case <-timer.C:
			}
		}
	}

	c.drainInFlightHandoffs(stopHandoffsCh, cancelHandoffs)
	return nil
}

func stopAndDrainTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

func (c *Tier2Poller) runSweepLoop(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.sweepProvisionedWorkers(ctx); err != nil && ctx.Err() == nil {
				c.logger.Warn("PollerController sweepProvisionedWorkers failed", slog.Any("error", err))
			}
		}
	}
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (c *Tier2Poller) waitContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		d = 10 * time.Millisecond
	}
	t := c.newTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Tier2Poller) getOrDialWorkerClient(uri string) (WorkerRPCClient, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, &PathError{Kind: PathErrInvalidArgument, Message: "worker uri is required"}
	}

	c.mu.Lock()
	if conn := c.workerConns[uri]; conn != nil {
		c.mu.Unlock()
		return conn, nil
	}
	c.mu.Unlock()

	val, _, err := c.dialGroup.Do(uri, func() (any, error) {
		c.mu.Lock()
		if existing := c.workerConns[uri]; existing != nil {
			c.mu.Unlock()
			return existing, nil
		}
		c.mu.Unlock()

		conn, err := c.dialWorker(uri)
		if err != nil {
			return nil, fmt.Errorf("failed to dial worker uri %q: %w", uri, err)
		}
		if conn == nil {
			return nil, &PathError{Kind: PathErrInternal, Message: fmt.Sprintf("worker dialer returned nil client for %q", uri)}
		}

		c.mu.Lock()
		if existing := c.workerConns[uri]; existing != nil {
			c.mu.Unlock()
			_ = conn.Close()
			return existing, nil
		}
		c.workerConns[uri] = conn
		c.mu.Unlock()
		return conn, nil
	})
	if err != nil {
		return nil, err
	}
	client, ok := val.(WorkerRPCClient)
	if !ok || client == nil {
		return nil, &PathError{Kind: PathErrInternal, Message: fmt.Sprintf("invalid worker client for %q", uri)}
	}
	return client, nil
}

func (c *Tier2Poller) provisionEnvironmentWorker(ctx context.Context, stopHandoffsCh <-chan struct{}, envID, interactionID string, cfg *EnvironmentConfig) (*WorkerProvisionResponse, error) {
	c.mu.Lock()
	call := c.provFlights[envID]
	if call != nil {
		call.waiters++
		call.shared = true
		c.mu.Unlock()
	} else {
		provCtx, provCancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultHandoffAttemptTimeout)
		if stopHandoffsCh != nil {
			go func() {
				select {
				case <-stopHandoffsCh:
					provCancel()
				case <-provCtx.Done():
				}
			}()
		}
		call = &provFlight{
			done:    make(chan struct{}),
			cancel:  provCancel,
			waiters: 1,
		}
		c.provFlights[envID] = call
		c.mu.Unlock()

		go func() {
			normalReturn := false
			defer func() {
				if !normalReturn {
					if r := recover(); r != nil {
						call.err = fmt.Errorf("provision worker panic: %v", r)
					} else if call.err == nil {
						call.err = errors.New("provision worker goroutine exited unexpectedly")
					}
				}
				call.cancel()
				c.mu.Lock()
				if c.provFlights[envID] == call {
					delete(c.provFlights, envID)
				}
				c.mu.Unlock()
				close(call.done)
			}()
			call.resp, call.err = c.provisioner.ProvisionWorker(provCtx, WorkerProvisionRequest{
				SchemaVersion:     WorkerProvisionSchemaVersionV1,
				EnvironmentID:     envID,
				InteractionID:     interactionID,
				EnvironmentConfig: ConvertToProvisionEnvironmentConfig(cfg),
			})
			if call.err == nil && call.resp == nil {
				call.err = &PathError{Kind: PathErrInternal, Message: "worker provisioner returned nil response"}
			}
			normalReturn = true
		}()
	}

	select {
	case <-call.done:
		c.mu.Lock()
		shared := call.shared
		c.mu.Unlock()
		c.observer.WorkerProvisioned(shared, call.err)
		if call.err != nil {
			return nil, call.err
		}
		return call.resp, nil
	case <-ctx.Done():
		c.mu.Lock()
		call.waiters--
		if call.waiters <= 0 {
			if c.provFlights[envID] == call {
				delete(c.provFlights, envID)
			}
			call.cancel()
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// SweepOnce executes a single idle worker sweep pass, suspending workers idle beyond IdleTimeout.
func (c *Tier2Poller) SweepOnce(ctx context.Context) error {
	return c.sweepProvisionedWorkers(ctx)
}

func (c *Tier2Poller) sweepProvisionedWorkers(ctx context.Context) error {
	now := c.now()

	c.mu.Lock()
	inFlightEnvs := make(map[string]bool, len(c.handoffLeases))
	for _, lease := range c.handoffLeases {
		if envID := strings.TrimSpace(lease.EnvironmentID); envID != "" {
			inFlightEnvs[envID] = true
		}
	}
	workers := make([]provisionedWorkerState, 0, len(c.activeWorkers))
	for _, w := range c.activeWorkers {
		if w == nil || inFlightEnvs[w.EnvironmentID] {
			continue
		}
		if now.Sub(w.LastActiveAt) < c.cfg.ResumeGracePeriod {
			continue
		}
		workers = append(workers, provisionedWorkerState{
			EnvironmentID: w.EnvironmentID,
			WorkerID:      w.WorkerID,
			URI:           w.URI,
			Headers:       cloneStringMap(w.Headers),
			LastActiveAt:  w.LastActiveAt,
		})
	}
	c.mu.Unlock()

	var firstErr error
	for _, w := range workers {
		client, err := c.getOrDialWorkerClient(w.URI)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		idleResp, err := client.GetIdleStatus(ctx, w.Headers)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if idleResp.Running || idleResp.IdleDuration < c.cfg.IdleTimeout {
			continue
		}

		c.mu.Lock()
		current, ok := c.activeWorkers[w.EnvironmentID]
		if !ok || !current.LastActiveAt.Equal(w.LastActiveAt) {
			c.mu.Unlock()
			continue
		}
		delete(c.activeWorkers, w.EnvironmentID)
		uriStillUsed := false
		for _, other := range c.activeWorkers {
			if other.URI == w.URI {
				uriStillUsed = true
				break
			}
		}
		var staleConn WorkerRPCClient
		if !uriStillUsed {
			staleConn = c.workerConns[w.URI]
			delete(c.workerConns, w.URI)
		}
		c.mu.Unlock()
		if staleConn != nil {
			_ = staleConn.Close()
		}

		if err := c.provisioner.SuspendWorker(ctx, WorkerSuspendRequest{
			SchemaVersion: WorkerProvisionSchemaVersionV1,
			EnvironmentID: w.EnvironmentID,
			WorkerID:      w.WorkerID,
			IdleDuration:  idleResp.IdleDuration.String(),
		}); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		c.logger.Info("PollerController suspended idle worker",
			slog.String("environment_id", w.EnvironmentID),
			slog.String("worker_id", w.WorkerID),
			slog.Duration("idle_duration", idleResp.IdleDuration),
		)
	}
	return firstErr
}

func (c *Tier2Poller) drainInFlightHandoffs(stopHandoffsCh <-chan struct{}, cancelHandoffs func()) {
	c.mu.Lock()
	c.draining = true
	c.mu.Unlock()

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), c.cfg.DrainTimeout)
	defer cancelDrain()

	allDoneCh := make(chan struct{})
	go func() {
		c.handoffWg.Wait()
		close(allDoneCh)
	}()

	waitDur := min(c.cfg.PollInterval, max(c.cfg.LeaseTTL/3, time.Millisecond))
	for c.InFlightCount() > 0 && drainCtx.Err() == nil {
		_, _ = c.pollOnce(drainCtx, stopHandoffsCh)
		if c.InFlightCount() == 0 {
			break
		}
		timer := c.newTimer(waitDur)
		select {
		case <-allDoneCh:
			stopAndDrainTimer(timer)
		case <-drainCtx.Done():
			stopAndDrainTimer(timer)
		case <-timer.C:
		}
	}

	cancelHandoffs()
	<-allDoneCh
}

// PollOnce executes a single LeaseInteractions poll and launches non-blocking handoff goroutines for claimed leases.
func (c *Tier2Poller) PollOnce(ctx context.Context) (time.Duration, error) {
	return c.pollOnce(ctx, nil)
}

func (c *Tier2Poller) pollOnce(ctx context.Context, stopHandoffsCh <-chan struct{}) (time.Duration, error) {
	now := c.now()

	c.mu.Lock()
	for id, until := range c.cooldownUntil {
		if !now.Before(until) {
			delete(c.cooldownUntil, id)
		}
	}

	draining := c.draining
	inFlightCount := len(c.handoffLeases)
	maxNew := 0
	if !draining {
		maxNew = max(c.cfg.MaxConcurrentInteractions-inFlightCount, 0)
	}

	activeLeases := make([]LeaseEntry, 0, len(c.handoffLeases))
	for _, lease := range c.handoffLeases {
		activeLeases = append(activeLeases, LeaseEntry{
			InteractionID: lease.InteractionID,
			LeaseToken:    lease.LeaseToken,
		})
	}
	c.mu.Unlock()

	slices.SortFunc(activeLeases, func(a, b LeaseEntry) int {
		return strings.Compare(a.InteractionID, b.InteractionID)
	})

	if draining && len(activeLeases) == 0 {
		return 0, nil
	}

	leaseCtx, cancelLease := context.WithTimeout(ctx, DefaultLeaseCallTimeout)
	defer cancelLease()

	res, err := c.client.LeaseInteractions(leaseCtx, &LeaseInteractionsRequest{
		EnvironmentID:      c.cfg.EnvironmentID,
		EnvironmentKey:     c.cfg.EnvironmentKey,
		WorkerID:           c.cfg.WorkerID,
		ActiveLeases:       activeLeases,
		MaxNewInteractions: int32(maxNew),
		SendZeroMaxNew:     maxNew == 0,
		RequestedLeaseTTL:  c.cfg.LeaseTTL,
		Labels:             c.cfg.Labels,
	})
	if err != nil {
		c.observer.LeasePolled(0, err)
		return 0, err
	}
	c.observer.LeasePolled(len(res.Leases), nil)

	for _, leased := range res.Leases {
		id := strings.TrimSpace(leased.InteractionID)
		if id == "" {
			continue
		}
		envID := cmp.Or(strings.TrimSpace(leased.EnvironmentID), c.cfg.EnvironmentID)

		c.mu.Lock()
		if existing, ok := c.handoffLeases[id]; ok {
			if leased.LeaseToken != "" {
				existing.LeaseToken = leased.LeaseToken
			}
			if envID != "" {
				existing.EnvironmentID = envID
			}
			if leased.EnvironmentConfig != nil {
				existing.EnvironmentConfig = leased.EnvironmentConfig
			}
			c.handoffLeases[id] = existing
			c.mu.Unlock()
			continue
		}

		if draining {
			c.mu.Unlock()
			continue
		}
		if until, inCooldown := c.cooldownUntil[id]; inCooldown && c.now().Before(until) {
			c.mu.Unlock()
			continue
		}
		if len(c.handoffLeases) >= c.cfg.MaxConcurrentInteractions {
			c.mu.Unlock()
			continue
		}

		active := ActiveInteractionLease{
			InteractionID:     id,
			EnvironmentID:     envID,
			LeaseToken:        leased.LeaseToken,
			EnvironmentConfig: leased.EnvironmentConfig,
		}
		c.handoffLeases[id] = active
		c.handoffWg.Add(1)
		c.mu.Unlock()

		c.logger.Info("PollerController claimed interaction lease",
			slog.String("interaction_id", id),
			slog.String("environment_id", envID),
		)
		go c.runHandoff(ctx, stopHandoffsCh, active)
	}

	return res.PollBackoff, nil
}

func (c *Tier2Poller) runHandoff(parentCtx context.Context, stopHandoffsCh <-chan struct{}, initial ActiveInteractionLease) {
	defer c.handoffWg.Done()
	id := initial.InteractionID

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if stopHandoffsCh != nil {
		ctx, cancel = context.WithTimeout(context.WithoutCancel(parentCtx), DefaultHandoffAttemptTimeout)
		go func() {
			select {
			case <-stopHandoffsCh:
				cancel()
			case <-ctx.Done():
			}
		}()
	} else {
		ctx, cancel = context.WithTimeout(parentCtx, DefaultHandoffAttemptTimeout)
	}
	defer cancel()

	defer func() {
		c.mu.Lock()
		delete(c.handoffLeases, id)
		c.mu.Unlock()
		c.notifySlotReleased()
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		c.mu.Lock()
		current := c.handoffLeases[id]
		c.mu.Unlock()

		envID := cmp.Or(strings.TrimSpace(current.EnvironmentID), c.cfg.EnvironmentID)

		var (
			resp     *StartWorkResponse
			provResp *WorkerProvisionResponse
			err      error
		)
		provResp, err = c.provisionEnvironmentWorker(ctx, stopHandoffsCh, envID, id, current.EnvironmentConfig)
		if err == nil {
			var workerClient WorkerRPCClient
			workerClient, err = c.getOrDialWorkerClient(provResp.URI)
			if err == nil {
				c.mu.Lock()
				current = c.handoffLeases[id]
				c.mu.Unlock()
				resp, err = c.callStartWork(ctx, workerClient, &StartWorkRequest{
					InteractionID:   id,
					EnvironmentID:   envID,
					LeaseToken:      current.LeaseToken,
					ControlPlaneURL: c.cfg.ControlPlaneURL,
					GRPCTarget:      c.cfg.GRPCTarget,
				}, provResp.Headers)
				if err == nil {
					c.mu.Lock()
					c.activeWorkers[envID] = &provisionedWorkerState{
						EnvironmentID: envID,
						WorkerID:      cmp.Or(strings.TrimSpace(provResp.WorkerID), envID),
						URI:           provResp.URI,
						Headers:       cloneStringMap(provResp.Headers),
						LastActiveAt:  c.now(),
					}
					c.mu.Unlock()
				}
			}
		}
		if err == nil && (resp == nil || resp.Status == "" || strings.EqualFold(resp.Status, WorkerStatusAccepted)) {
			c.mu.Lock()
			delete(c.failureCounts, id)
			delete(c.cooldownUntil, id)
			delete(c.deadLettered, id)
			c.mu.Unlock()
			c.observer.HandoffFinished("accepted")
			c.logger.Info("PollerController dispatched interaction to worker",
				slog.String("interaction_id", id),
				slog.String("environment_id", envID),
				slog.String("worker_id", cmp.Or(strings.TrimSpace(provResp.WorkerID), envID)),
			)
			return
		}

		if err == nil {
			err = fmt.Errorf("unexpected StartWork status %q", resp.Status)
		}
		if ctx.Err() != nil {
			return
		}

		c.mu.Lock()
		c.failureCounts[id]++
		failures := c.failureCounts[id]
		if failures >= c.cfg.MaxHandoffRetries {
			c.cooldownUntil[id] = c.now().Add(c.cfg.PoisonCooldown)
			c.deadLettered[id] = true
			c.mu.Unlock()
			c.observer.HandoffFinished("dead_lettered")
			c.logger.Warn("PollerController dropping interaction lease after max handoff retries",
				slog.String("interaction_id", id),
				slog.String("environment_id", envID),
				slog.Int("failures", failures),
				slog.Any("error", err),
			)
			return
		}
		c.mu.Unlock()
		c.observer.HandoffFinished("retry")
		c.logger.Warn("PollerController interaction handoff attempt failed; retrying",
			slog.String("interaction_id", id),
			slog.String("environment_id", envID),
			slog.Int("failures", failures),
			slog.Any("error", err),
		)

		backoff := min(RetryBackoff(c.cfg.HandoffRetryBackoff, failures), 2*time.Second)
		if waitErr := c.waitContext(ctx, backoff); waitErr != nil {
			return
		}
	}
}

func (c *Tier2Poller) callStartWork(ctx context.Context, workerClient WorkerRPCClient, req *StartWorkRequest, headers map[string]string) (*StartWorkResponse, error) {
	rpcCtx, cancelRPC := context.WithTimeout(ctx, DefaultLeaseCallTimeout)
	defer cancelRPC()
	return workerClient.StartWork(rpcCtx, req, headers)
}
