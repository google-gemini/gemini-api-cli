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
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ProvisionInput is the v1 JSON payload passed to --provision-cmd on stdin.
type ProvisionInput struct {
	SchemaVersion     string          `json:"schema_version"`
	InteractionID     string          `json:"interaction_id"`
	EnvironmentID     string          `json:"environment_id"`
	LeaseToken        string          `json:"lease_token"`
	ServerURL         string          `json:"server_url,omitempty"`
	EnvironmentConfig json.RawMessage `json:"environment_config,omitempty"`
}

// CommandWorkerProvisioner dispatches a leased interaction to a sandbox worker
// by invoking an external shell command (--provision-cmd) with a v1 JSON
// ProvisionInput payload on stdin.
type CommandWorkerProvisioner struct {
	provisionCmd string
}

// NewCommandWorkerProvisioner creates a stateless CommandWorkerProvisioner for provisionCmd.
func NewCommandWorkerProvisioner(provisionCmd string) *CommandWorkerProvisioner {
	return &CommandWorkerProvisioner{
		provisionCmd: strings.TrimSpace(provisionCmd),
	}
}

// ProvisionAndDispatch invokes provisionCmd with input serialized as JSON on stdin.
func (p *CommandWorkerProvisioner) ProvisionAndDispatch(ctx context.Context, input ProvisionInput) error {
	if p.provisionCmd == "" {
		return fmt.Errorf("--provision-cmd is required to dispatch interaction %q (environment %q)", input.InteractionID, input.EnvironmentID)
	}
	if input.SchemaVersion == "" {
		input.SchemaVersion = "v1"
	}

	stdinBytes, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshaling provision input: %w", err)
	}

	shellBin, shellArgs := resolveProvisionShell(runtime.GOOS, p.provisionCmd)

	cmd := exec.CommandContext(ctx, shellBin, shellArgs...)
	cmd.Stdin = bytes.NewReader(stdinBytes)
	cmd.Env = filterScrubbedEnv(os.Environ())
	configureSubprocessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	stderr := &cappedBuffer{maxBytes: maxBashOutputBytes}
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

func resolveProvisionShell(goos, provisionCmd string) (string, []string) {
	if pth, err := exec.LookPath("sh"); err == nil {
		return pth, []string{"-c", provisionCmd}
	}
	if goos == "windows" {
		return "cmd.exe", []string{"/C", provisionCmd}
	}
	return "sh", []string{"-c", provisionCmd}
}

type leasePollLoopConfig struct {
	client       *HTTPClient
	callerID     string
	maxSlots     int
	pollInterval time.Duration
	noRetries    bool
	logger       *log.Logger
	errLabel     string
	startMsg     string
}

func runLeasePollLoop(ctx context.Context, cfg leasePollLoopConfig, handle func(context.Context, *LeasedInteraction)) error {
	sem := make(chan struct{}, cfg.maxSlots)
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

		availSlots := cfg.maxSlots - len(sem)
		if availSlots <= 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		leased, err := cfg.client.LeaseInteractions(ctx, cfg.callerID, int32(availSlots))
		if err != nil {
			if isNonRetryableControlPlaneError(err) || cfg.noRetries {
				return err
			}
			consecutiveErrors++
			delay := retryBackoff(cfg.pollInterval, consecutiveErrors)
			if ctx.Err() == nil {
				cfg.logger.Printf("%s (retrying in %v): %v", cfg.errLabel, delay, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				continue
			}
		}
		consecutiveErrors = 0
		if !startedLogged {
			startedLogged = true
			cfg.logger.Print(cfg.startMsg)
		}

		if len(leased) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(cfg.pollInterval):
				continue
			}
		}

		for _, item := range leased {
			if item == nil || item.InteractionID == "" {
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
			go func(lease *LeasedInteraction) {
				defer wg.Done()
				defer func() { <-sem }()
				defer func() {
					mu.Lock()
					delete(inFlight, lease.InteractionID)
					mu.Unlock()
				}()
				handle(ctx, lease)
			}(item)
		}
	}
}

// PollerOptions configures a stateless PollerController (`gemini-api environments poll`).
type PollerOptions struct {
	PollerID                 string
	MaxInteractions          int
	PollInterval             time.Duration
	ProvisionTimeout         time.Duration
	HandoffHeartbeatInterval time.Duration
	NoRetries                bool
	Logger                   *log.Logger
}

type failureState struct {
	consecutive int
	cooldownEnd time.Time
}

// PollerController runs the stateless control-plane poller that leases queued
// interactions and dispatches them via CommandWorkerProvisioner (--provision-cmd)
// while heartbeating the lease during cold-start provisioning.
type PollerController struct {
	client                   *HTTPClient
	provisioner              *CommandWorkerProvisioner
	pollerID                 string
	maxInteractions          int
	pollInterval             time.Duration
	provisionTimeout         time.Duration
	handoffHeartbeatInterval time.Duration
	noRetries                bool
	logger                   *log.Logger

	mu       sync.Mutex
	failures map[string]*failureState
}

// NewPollerController creates a new stateless PollerController.
func NewPollerController(client *HTTPClient, provisioner *CommandWorkerProvisioner, opts PollerOptions) *PollerController {
	if opts.PollerID == "" {
		opts.PollerID = defaultRuntimeID("poller")
	}
	if opts.MaxInteractions <= 0 {
		opts.MaxInteractions = 10
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.ProvisionTimeout <= 0 {
		opts.ProvisionTimeout = 2 * time.Minute
	}
	if opts.HandoffHeartbeatInterval <= 0 {
		opts.HandoffHeartbeatInterval = defaultHeartbeatInterval
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	return &PollerController{
		client:                   client,
		provisioner:              provisioner,
		pollerID:                 opts.PollerID,
		maxInteractions:          opts.MaxInteractions,
		pollInterval:             opts.PollInterval,
		provisionTimeout:         opts.ProvisionTimeout,
		handoffHeartbeatInterval: opts.HandoffHeartbeatInterval,
		noRetries:                opts.NoRetries,
		logger:                   opts.Logger,
		failures:                 make(map[string]*failureState),
	}
}

// Run executes the stateless poller loop until ctx is canceled or a non-retryable error occurs.
func (p *PollerController) Run(ctx context.Context) error {
	return runLeasePollLoop(ctx, leasePollLoopConfig{
		client:       p.client,
		callerID:     p.pollerID,
		maxSlots:     p.maxInteractions,
		pollInterval: p.pollInterval,
		noRetries:    p.noRetries,
		logger:       p.logger,
		errLabel:     "Poller LeaseInteractions error",
		startMsg:     fmt.Sprintf("PollerController started (maxInteractions=%d)", p.maxInteractions),
	}, p.dispatchLease)
}

func (p *PollerController) dispatchLease(ctx context.Context, lease *LeasedInteraction) {
	envID := firstNonEmpty(lease.EnvironmentID, "default")
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
				resp, err := p.client.HeartbeatInteraction(dispatchCtx, p.pollerID, lease.InteractionID, lease.EnvironmentID, lease.LeaseToken)
				if err == nil && resp.ShouldCancel {
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

	err := p.provisioner.ProvisionAndDispatch(dispatchCtx, ProvisionInput{
		SchemaVersion:     "v1",
		InteractionID:     lease.InteractionID,
		EnvironmentID:     envID,
		LeaseToken:        lease.LeaseToken,
		ServerURL:         p.client.ServerURL(),
		EnvironmentConfig: lease.EnvironmentConfig,
	})
	if err != nil {
		p.recordDispatchFailure(lease.InteractionID)
		p.logger.Printf("[%s] ProvisionAndDispatch failed for env %s: %v", lease.InteractionID, envID, err)
		return
	}

	p.mu.Lock()
	delete(p.failures, lease.InteractionID)
	p.mu.Unlock()
	p.logger.Printf("[%s] Dispatched interaction lease for environment %s", lease.InteractionID, envID)
}

func (p *PollerController) recordDispatchFailure(interactionID string) {
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
		st = &failureState{}
		p.failures[interactionID] = st
	}
	st.consecutive++
	shift := min(st.consecutive-1, 5)
	st.cooldownEnd = now.Add(min(time.Duration(1<<shift)*500*time.Millisecond, 15*time.Second))
}
