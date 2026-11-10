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

// Package substrate integrates the Gemini API CLI (`gemini-api`) self-hosted runtime
// with Agent Substrate (`substrate.ateapi.v1` and `atenet-router`).
package substrate

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	// The Control gRPC stubs are imported under a second alias so builds that
	// generate messages and gRPC stubs into separate packages can map each.
	ategrpc "go.ate.dev/substrate/api/ateapi/v1"
	atepb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/local"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	// DefaultAtespace is the default Kubernetes namespace / Substrate atespace.
	DefaultAtespace = "gemini-self-hosted"

	// DefaultTemplate is the default Substrate ActorTemplate name.
	DefaultTemplate = "gemini-env-actor"

	// DefaultAteapiAddr is the default in-cluster Substrate control-plane endpoint.
	DefaultAteapiAddr = "api.ate-system.svc.cluster.local:443"

	// DefaultAteapiServerName is the TLS SAN served by ate-api-server pod certificates.
	DefaultAteapiServerName = "api.ate-system.svc"

	// DefaultAteapiCAFile is the mounted ClusterTrustBundle path for servicedns.podcert.ate.dev/identity.
	DefaultAteapiCAFile = "/var/run/secrets/servicedns-ca/trust-bundle.pem"

	// DefaultAtenetRouterAddr is the default in-cluster Substrate mTLS router endpoint.
	DefaultAtenetRouterAddr = "atenet-router.ate-system.svc.cluster.local:443"

	// DefaultAtenetRouterServerName is the TLS SAN served by atenet-router pod certificates.
	DefaultAtenetRouterServerName = "atenet-router.ate-system.svc"

	// DefaultPodIdentityBundleFile is the mounted PodCertificate PEM bundle path
	// for podidentity.podcert.ate.dev/identity used when dialing atenet-router over mTLS.
	DefaultPodIdentityBundleFile = "/var/run/secrets/podidentity/credential-bundle.pem"

	// DefaultTokenPath is the path to the projected Kubernetes ServiceAccount token
	// used to authenticate to ate-api-server.
	DefaultTokenPath = "/var/run/secrets/tokens/ateapi-token"

	// DefaultGeminiControlPlaneHost is the Gemini API hostname that ate-gemini
	// adds a TLS passthrough egress rule for, so actors can open their bidi
	// stream unless the caller's own rules for that host take precedence.
	DefaultGeminiControlPlaneHost = "generativelanguage.googleapis.com"

	// DefaultControlPlaneURL is the default Gemini API control-plane URL.
	DefaultControlPlaneURL = "https://generativelanguage.googleapis.com"

	// DefaultRuntimeBin is the default executable name for the Gemini API CLI
	// (`gemini-api`) binary providing `environments poll` and `environments serve`.
	DefaultRuntimeBin = "gemini-api"

	// DefaultWorkerPort is the port where ate-gemini actor-daemon listens inside
	// the Substrate actor container (atenet-router forwards actor traffic to port 80).
	DefaultWorkerPort = 80

	// DefaultWorkDir is the default durable workspace mount inside the actor.
	DefaultWorkDir = "/workspace"

	// DefaultResumeTimeout bounds how long Provision waits for an actor to wake up.
	DefaultResumeTimeout = 60 * time.Second

	// DefaultSuspendTimeout bounds SuspendActor and RevertActor RPCs.
	DefaultSuspendTimeout = 30 * time.Second

	// DefaultResumeCooldown prevents a freshly resumed actor from being suspended
	// before the controller dispatches StartWork.
	DefaultResumeCooldown = 15 * time.Second

	// DefaultStatePollInterval is the interval used when waiting for a transitional
	// actor state (such as SUSPENDING or REVERTING) to settle before resuming.
	DefaultStatePollInterval = 200 * time.Millisecond

	// TargetActorHeader is the header read by atenet-router to route gRPC/HTTP
	// traffic to a specific Substrate actor.
	TargetActorHeader = "ate-target-actor"

	// EnvoyTimeoutHeader sets the upstream request timeout on atenet-router.
	EnvoyTimeoutHeader = "x-envoy-upstream-rq-timeout-ms"

	// DefaultRouterTimeoutMs is the default upstream timeout in milliseconds sent
	// to atenet-router when dispatching StartWork.
	DefaultRouterTimeoutMs = "60000"
)

// ManagerConfig configures a Substrate Actor Manager.
//
// When AllowedAtespaces or AllowedTemplates are omitted, NewManager defaults
// fail-closed to permitting only Atespace (default DefaultAtespace) and Template
// (default DefaultTemplate), respectively. Pass explicit names or "*" to permit
// additional pre-created Atespaces or ActorTemplates.
type ManagerConfig struct {
	Atespace               string
	Template               string
	AllowedAtespaces       []string
	AllowedTemplates       []string
	RouterAddr             string
	RouterInsecure         bool
	RouterCAFile           string
	RouterClientBundleFile string
	ResumeTimeout          time.Duration
	SuspendTimeout         time.Duration
	ResumeCooldown         time.Duration
	StatePollInterval      time.Duration
	Logger                 *slog.Logger
	NowFunc                func() time.Time
}

// Manager manages the lifecycle of Substrate actors backing Gemini self-hosted environments.
type Manager struct {
	client                 ategrpc.ControlClient
	atespace               string
	template               string
	allowAllAtespaces      bool
	allowedAtespaces       map[string]bool
	sortedAllowedAtespaces []string
	allowAllTemplates      bool
	allowedTemplates       map[string]bool
	routerAddr             string
	routerInsecure         bool
	routerCAFile           string
	routerClientBundleFile string
	resumeTimeout          time.Duration
	suspendTimeout         time.Duration
	resumeCooldown         time.Duration
	statePollInterval      time.Duration
	logger                 *slog.Logger
	nowFunc                func() time.Time

	mu          sync.Mutex
	lastResumed map[string]time.Time
}

// ParseAllowlistCSV splits a comma-separated allowlist flag or environment
// variable into trimmed non-empty entries.
func ParseAllowlistCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func buildAllowlistSet(entries []string, fallback string) (bool, map[string]bool, []string) {
	set := make(map[string]bool)
	for _, raw := range entries {
		for _, item := range ParseAllowlistCSV(raw) {
			if item == "*" {
				return true, nil, []string{"*"}
			}
			set[item] = true
		}
	}
	if len(set) == 0 {
		set[fallback] = true
	}
	sorted := make([]string, 0, len(set))
	for k := range set {
		sorted = append(sorted, k)
	}
	slices.Sort(sorted)
	return false, set, sorted
}

// NewManager creates a new Substrate Actor Manager.
func NewManager(client ategrpc.ControlClient, cfg ManagerConfig) *Manager {
	atespace := strings.TrimSpace(cfg.Atespace)
	if atespace == "" {
		atespace = DefaultAtespace
	}
	template := strings.TrimSpace(cfg.Template)
	if template == "" {
		template = DefaultTemplate
	}
	allowAllAtespaces, allowedAtespaces, sortedAtespaces := buildAllowlistSet(cfg.AllowedAtespaces, atespace)
	allowAllTemplates, allowedTemplates, _ := buildAllowlistSet(cfg.AllowedTemplates, template)

	routerAddr := strings.TrimSpace(cfg.RouterAddr)
	if routerAddr == "" {
		routerAddr = DefaultAtenetRouterAddr
	}
	resumeTimeout := cfg.ResumeTimeout
	if resumeTimeout <= 0 {
		resumeTimeout = DefaultResumeTimeout
	}
	suspendTimeout := cfg.SuspendTimeout
	if suspendTimeout <= 0 {
		suspendTimeout = DefaultSuspendTimeout
	}
	resumeCooldown := cfg.ResumeCooldown
	if resumeCooldown <= 0 {
		resumeCooldown = DefaultResumeCooldown
	}
	statePollInterval := cfg.StatePollInterval
	if statePollInterval <= 0 {
		statePollInterval = DefaultStatePollInterval
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFunc := cfg.NowFunc
	if nowFunc == nil {
		nowFunc = time.Now
	}

	return &Manager{
		client:                 client,
		atespace:               atespace,
		template:               template,
		allowAllAtespaces:      allowAllAtespaces,
		allowedAtespaces:       allowedAtespaces,
		sortedAllowedAtespaces: sortedAtespaces,
		allowAllTemplates:      allowAllTemplates,
		allowedTemplates:       allowedTemplates,
		routerAddr:             routerAddr,
		routerInsecure:         cfg.RouterInsecure,
		routerCAFile:           strings.TrimSpace(cfg.RouterCAFile),
		routerClientBundleFile: strings.TrimSpace(cfg.RouterClientBundleFile),
		resumeTimeout:          resumeTimeout,
		suspendTimeout:         suspendTimeout,
		resumeCooldown:         resumeCooldown,
		statePollInterval:      statePollInterval,
		logger:                 logger,
		nowFunc:                nowFunc,
		lastResumed:            make(map[string]time.Time),
	}
}

// Atespace returns the default Substrate atespace managed by m.
func (m *Manager) Atespace() string {
	return m.atespace
}

// Template returns the default Substrate ActorTemplate name managed by m.
func (m *Manager) Template() string {
	return m.template
}

// RouterAddr returns the Substrate atenet-router endpoint managed by m.
func (m *Manager) RouterAddr() string {
	return m.routerAddr
}

// AllowedAtespaces returns a copy of the configured atespace allowlist ("*" when wildcarded).
func (m *Manager) AllowedAtespaces() []string {
	return slices.Clone(m.sortedAllowedAtespaces)
}

// IsAtespaceAllowed reports whether atespace is permitted by the operator's allowlist.
func (m *Manager) IsAtespaceAllowed(atespace string) bool {
	atespace = strings.TrimSpace(atespace)
	if atespace == "" {
		return false
	}
	if m.allowAllAtespaces {
		return true
	}
	return m.allowedAtespaces[atespace]
}

// IsTemplateAllowed reports whether template is permitted by the operator's allowlist.
func (m *Manager) IsTemplateAllowed(template string) bool {
	template = strings.TrimSpace(template)
	if template == "" {
		return false
	}
	if m.allowAllTemplates {
		return true
	}
	return m.allowedTemplates[template]
}

func (m *Manager) checkAllowedAtespace(atespace string) error {
	if err := validateDNSLabel("atespace", atespace); err != nil {
		return err
	}
	if !m.IsAtespaceAllowed(atespace) {
		return fmt.Errorf("atespace %q is not permitted by --allowed-atespaces", atespace)
	}
	return nil
}

func (m *Manager) checkAllowedTemplate(template string) error {
	if err := validateDNSLabel("template", template); err != nil {
		return err
	}
	if !m.IsTemplateAllowed(template) {
		return fmt.Errorf("template %q is not permitted by --allowed-templates", template)
	}
	return nil
}

// DialRouter opens a gRPC client connection to m's configured atenet-router endpoint.
func (m *Manager) DialRouter() (*grpc.ClientConn, error) {
	return DialRouter(m.routerAddr, m.routerInsecure, m.routerCAFile, m.routerClientBundleFile)
}

// EnsureAtespace verifies the default Substrate atespace configured on m exists.
func (m *Manager) EnsureAtespace(ctx context.Context) error {
	return m.EnsureAtespaceByName(ctx, m.atespace)
}

// EnsureAtespaceByName verifies the named Substrate atespace exists without creating it.
func (m *Manager) EnsureAtespaceByName(ctx context.Context, atespace string) error {
	atespace = firstNonEmpty(atespace, m.atespace, DefaultAtespace)
	if err := validateDNSLabel("atespace", atespace); err != nil {
		return err
	}

	opCtx, cancel := context.WithTimeout(ctx, m.suspendTimeout)
	defer cancel()

	_, err := m.client.GetAtespace(opCtx, &atepb.GetAtespaceRequest{
		Atespace: &atepb.ObjectRef{Name: atespace},
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Errorf("atespace %q does not exist: %w", atespace, err)
		}
		return fmt.Errorf("getting atespace %q: %w", atespace, err)
	}
	return nil
}

// ProvisionResult reports the actor provisioned or resumed by Manager.Provision.
type ProvisionResult struct {
	ActorName string
	Atespace  string
	Template  string
	Action    string
	ActorUID  string
}

// Provision ensures the Substrate actor for environmentID exists, has its initial
// egress policy attached, and is in the RUNNING state, returning its ProvisionResult.
func (m *Manager) Provision(ctx context.Context, environmentID string, envCfg *ProvisionEnvironmentConfig) (ProvisionResult, error) {
	return m.ProvisionWithServerURL(ctx, environmentID, "", envCfg)
}

// ProvisionWithServerURL ensures the Substrate actor for environmentID exists,
// attaches its initial EgressPolicy (the caller's custom_config.egress_policy
// rules plus TLS passthrough to the default Gemini API endpoint and any custom
// serverURL host; see BuildEgressRules), and transitions the actor to RUNNING.
//
// An environment's initial egress configuration is immutable after creation,
// matching the Gemini API control plane invariant rejecting custom_config
// overrides when resuming an existing SELF_HOSTED environment: if the actor's
// "default" EgressPolicy already exists, CreateActorEgressPolicy returns
// AlreadyExists and preserves the initial policy unchanged.
func (m *Manager) ProvisionWithServerURL(ctx context.Context, environmentID, serverURL string, envCfg *ProvisionEnvironmentConfig) (ProvisionResult, error) {
	environmentID = strings.TrimSpace(environmentID)
	if environmentID == "" {
		return ProvisionResult{}, errors.New("environment_id is required")
	}
	if strings.ContainsAny(environmentID, "\x00\r\n") {
		return ProvisionResult{}, errors.New("environment_id contains invalid control characters")
	}

	atespace := m.atespace
	template := m.template
	if envCfg != nil {
		if v := strings.TrimSpace(envCfg.Atespace); v != "" {
			atespace = v
		}
		if v := strings.TrimSpace(envCfg.Template); v != "" {
			template = v
		}
	}
	if err := m.checkAllowedAtespace(atespace); err != nil {
		return ProvisionResult{}, err
	}
	if err := m.checkAllowedTemplate(template); err != nil {
		return ProvisionResult{}, err
	}

	var policy *atepb.EgressPolicy
	if envCfg != nil {
		policy = envCfg.EgressPolicy
	}
	rules := BuildEgressRules(policy, serverURL)

	opCtx, cancel := context.WithTimeout(ctx, m.resumeTimeout)
	defer cancel()

	if err := m.EnsureAtespaceByName(opCtx, atespace); err != nil {
		return ProvisionResult{}, err
	}

	actorName := ActorName(environmentID)
	actorRef := &atepb.ObjectRef{
		Atespace: atespace,
		Name:     actorName,
	}

	action := "resumed"
	actor, err := m.client.GetActor(opCtx, &atepb.GetActorRequest{Actor: actorRef})
	if status.Code(err) == codes.NotFound {
		created, createErr := m.createActorWithEgress(opCtx, actorRef, template, rules)
		if createErr != nil {
			return ProvisionResult{}, createErr
		}
		actor = created
		action = "created"
	} else if err != nil {
		return ProvisionResult{}, fmt.Errorf("getting actor %s: %w", actorName, err)
	} else {
		resolved, err := m.resolveExistingActorTemplate(actor, template)
		if err != nil {
			return ProvisionResult{}, err
		}
		template = resolved
		if actor.GetStatus().GetState() == atepb.ActorState_ACTOR_STATE_RUNNING {
			action = "running"
		}
		if err := m.ensureActorEgress(opCtx, actorRef, rules); err != nil {
			return ProvisionResult{}, err
		}
	}

	if err := m.ensureActorRunning(opCtx, actorRef, actor); err != nil {
		return ProvisionResult{}, err
	}

	m.recordResume(atespace, actorName)
	return ProvisionResult{
		ActorName: actorName,
		Atespace:  atespace,
		Template:  template,
		Action:    action,
		ActorUID:  actor.GetMetadata().GetUid(),
	}, nil
}

func (m *Manager) createActorWithEgress(ctx context.Context, actorRef *atepb.ObjectRef, template string, rules []*atepb.EgressRule) (*atepb.Actor, error) {
	m.logger.Info("Creating new Substrate actor for Gemini environment",
		slog.String("actor", actorRef.GetName()),
		slog.String("atespace", actorRef.GetAtespace()),
		slog.String("template", template),
	)

	actor, err := m.client.CreateActor(ctx, &atepb.CreateActorRequest{
		Actor: &atepb.Actor{
			Metadata: &atepb.ResourceMetadata{
				Atespace: actorRef.GetAtespace(),
				Name:     actorRef.GetName(),
			},
			ActorTemplate: &atepb.ObjectRef{
				Atespace: actorRef.GetAtespace(),
				Name:     template,
			},
		},
	})
	if err != nil {
		if status.Code(err) != codes.AlreadyExists {
			return nil, fmt.Errorf("creating actor %s: %w", actorRef.GetName(), err)
		}
		actor, err = m.client.GetActor(ctx, &atepb.GetActorRequest{Actor: actorRef})
		if err != nil {
			return nil, fmt.Errorf("getting existing actor %s after AlreadyExists: %w", actorRef.GetName(), err)
		}
		if _, err := m.resolveExistingActorTemplate(actor, template); err != nil {
			return nil, err
		}
	}

	if err := m.ensureActorEgress(ctx, actorRef, rules); err != nil {
		return nil, err
	}

	return actor, nil
}

// resolveExistingActorTemplate returns the ActorTemplate name recorded on an
// existing actor (or fallback when unset), failing closed when that template is
// no longer permitted by m's template allowlist (for example, after an operator
// revokes a template via --allowed-templates).
func (m *Manager) resolveExistingActorTemplate(actor *atepb.Actor, fallback string) (string, error) {
	existing := strings.TrimSpace(actor.GetActorTemplate().GetName())
	if existing == "" {
		return fallback, nil
	}
	if err := m.checkAllowedTemplate(existing); err != nil {
		return "", fmt.Errorf("existing actor %s: %w", actor.GetMetadata().GetName(), err)
	}
	return existing, nil
}

// ensureActorEgress creates the singleton "default" EgressPolicy for actorRef
// when none exists yet. Initial egress policies are immutable for the lifetime
// of the environment, so AlreadyExists is treated as idempotent success without
// overwriting existing rules.
func (m *Manager) ensureActorEgress(ctx context.Context, actorRef *atepb.ObjectRef, rules []*atepb.EgressRule) error {
	_, err := m.client.CreateActorEgressPolicy(ctx, &atepb.CreateActorEgressPolicyRequest{
		Actor: actorRef,
		EgressPolicy: &atepb.EgressPolicy{
			Metadata: &atepb.ResourceMetadata{
				Atespace: actorRef.GetAtespace(),
				Name:     "default",
			},
			Rules: rules,
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("creating egress policy for actor %s: %w", actorRef.GetName(), err)
	}
	return nil
}

func (m *Manager) ensureActorRunning(ctx context.Context, actorRef *atepb.ObjectRef, actor *atepb.Actor) error {
	for {
		state := actor.GetStatus().GetState()
		switch state {
		case atepb.ActorState_ACTOR_STATE_RUNNING:
			return nil

		case atepb.ActorState_ACTOR_STATE_SUSPENDED,
			atepb.ActorState_ACTOR_STATE_RESUMING,
			atepb.ActorState_ACTOR_STATE_UNSPECIFIED:
			_, err := m.client.ResumeActor(ctx, &atepb.ResumeActorRequest{Actor: actorRef})
			if err != nil {
				return fmt.Errorf("resuming actor %s: %w", actorRef.GetName(), err)
			}
			return nil

		case atepb.ActorState_ACTOR_STATE_SUSPENDING,
			atepb.ActorState_ACTOR_STATE_REVERTING:
			if err := sleepContext(ctx, m.statePollInterval); err != nil {
				return fmt.Errorf("waiting for actor %s state %s to settle: %w", actorRef.GetName(), state, err)
			}
			updated, err := m.client.GetActor(ctx, &atepb.GetActorRequest{Actor: actorRef})
			if err != nil {
				return fmt.Errorf("polling actor %s state: %w", actorRef.GetName(), err)
			}
			actor = updated

		case atepb.ActorState_ACTOR_STATE_CRASHED:
			m.logger.Warn("Actor is CRASHED; reverting before resume",
				slog.String("actor", actorRef.GetName()),
			)
			if _, err := m.client.RevertActor(ctx, &atepb.RevertActorRequest{Actor: actorRef}); err != nil {
				return fmt.Errorf("reverting crashed actor %s: %w", actorRef.GetName(), err)
			}
			if _, err := m.client.ResumeActor(ctx, &atepb.ResumeActorRequest{Actor: actorRef}); err != nil {
				return fmt.Errorf("resuming reverted actor %s: %w", actorRef.GetName(), err)
			}
			return nil

		default:
			return fmt.Errorf("actor %s is in non-resumable state %s", actorRef.GetName(), state)
		}
	}
}

// ListRunningActors returns all RUNNING non-golden actors in m's allowed
// Atespaces whose ActorTemplate is permitted by m's template allowlist and that
// are not currently within m's post-resume cooldown window.
func (m *Manager) ListRunningActors(ctx context.Context) ([]*atepb.Actor, error) {
	atespaceScopes := m.sortedAllowedAtespaces
	if m.allowAllAtespaces {
		atespaceScopes = []string{""}
	}

	now := m.nowFunc()
	var running []*atepb.Actor
	for _, scope := range atespaceScopes {
		pageToken := ""
		for {
			opCtx, cancel := context.WithTimeout(ctx, m.suspendTimeout)
			resp, err := m.client.ListActors(opCtx, &atepb.ListActorsRequest{
				Atespace: scope,
				Options:  &atepb.ListOptions{PageToken: pageToken},
			})
			cancel()
			if err != nil {
				if scope != "" && status.Code(err) == codes.NotFound {
					// An allowlisted Atespace that does not exist (yet) must not
					// block sweeping the remaining Atespaces.
					break
				}
				return nil, fmt.Errorf("listing actors in atespace %q: %w", scope, err)
			}
			for _, actor := range resp.GetActors() {
				if actor.GetStatus().GetState() != atepb.ActorState_ACTOR_STATE_RUNNING {
					continue
				}
				as := actor.GetMetadata().GetAtespace()
				name := actor.GetMetadata().GetName()
				if as == "" || name == "" || isGoldenActorUUID(name) {
					continue
				}
				if !m.IsAtespaceAllowed(as) || !m.IsTemplateAllowed(actor.GetActorTemplate().GetName()) {
					continue
				}
				if m.inResumeCooldown(as, name, now) {
					continue
				}
				running = append(running, actor)
			}
			pageToken = resp.GetNextPageToken()
			if pageToken == "" {
				break
			}
		}
	}
	return running, nil
}

// isGoldenActorUUID reports whether s is a standard 36-char UUID (8-4-4-4-12
// hex), which Substrate uses to name temporary golden-snapshot bake actors
// (whereas ActorName always ends with a 16-hex-character SHA-256 suffix).
func isGoldenActorUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

func (m *Manager) inResumeCooldown(atespace, actorName string, now time.Time) bool {
	m.mu.Lock()
	lastResume, ok := m.lastResumed[atespace+"/"+actorName]
	m.mu.Unlock()
	return ok && now.Sub(lastResume) < m.resumeCooldown
}

// Suspend suspends the Substrate actor identified by actorName (or derived from
// environmentID when actorName is empty) in m's default atespace. It ignores
// NotFound and already-suspended actors.
func (m *Manager) Suspend(ctx context.Context, environmentID, actorName string) error {
	return m.SuspendInAtespace(ctx, "", environmentID, actorName)
}

// SuspendInAtespace suspends the Substrate actor identified by actorName (or
// derived from environmentID when actorName is empty) in the specified atespace
// (falling back to m's default atespace when empty). It verifies that the
// atespace is permitted by the operator allowlist and exists, and ignores
// NotFound or already-suspended actors.
func (m *Manager) SuspendInAtespace(ctx context.Context, atespace, environmentID, actorName string) error {
	atespace = firstNonEmpty(atespace, m.atespace, DefaultAtespace)
	if err := m.checkAllowedAtespace(atespace); err != nil {
		return err
	}

	environmentID = strings.TrimSpace(environmentID)
	if strings.ContainsAny(environmentID, "\x00\r\n") {
		return errors.New("environment_id contains invalid control characters")
	}

	actorName = strings.TrimSpace(actorName)
	if actorName == "" {
		if environmentID == "" {
			return errors.New("actor_name or environment_id is required")
		}
		actorName = ActorName(environmentID)
	}
	if err := validateDNSLabel("actor_name", actorName); err != nil {
		return err
	}
	if isGoldenActorUUID(actorName) {
		return fmt.Errorf("refusing to suspend golden-snapshot actor %q", actorName)
	}

	opCtx, cancel := context.WithTimeout(ctx, m.suspendTimeout)
	defer cancel()

	if err := m.EnsureAtespaceByName(opCtx, atespace); err != nil {
		return err
	}

	if m.inResumeCooldown(atespace, actorName, m.nowFunc()) {
		m.logger.Debug("Ignoring suspend for actor within resume cooldown",
			slog.String("atespace", atespace),
			slog.String("actor", actorName),
		)
		return nil
	}

	actorRef := &atepb.ObjectRef{
		Atespace: atespace,
		Name:     actorName,
	}

	actor, err := m.client.GetActor(opCtx, &atepb.GetActorRequest{Actor: actorRef})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting actor %s/%s before suspend: %w", atespace, actorName, err)
	}
	switch actor.GetStatus().GetState() {
	case atepb.ActorState_ACTOR_STATE_SUSPENDED, atepb.ActorState_ACTOR_STATE_SUSPENDING:
		return nil
	}

	_, err = m.client.SuspendActor(opCtx, &atepb.SuspendActorRequest{Actor: actorRef})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("suspending actor %s/%s: %w", atespace, actorName, err)
	}

	m.logger.Info("Suspended Substrate actor",
		slog.String("atespace", atespace),
		slog.String("actor", actorName),
	)
	return nil
}

func (m *Manager) recordResume(atespace, actorName string) {
	now := m.nowFunc()
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, ts := range m.lastResumed {
		if now.Sub(ts) >= m.resumeCooldown {
			delete(m.lastResumed, key)
		}
	}
	m.lastResumed[atespace+"/"+actorName] = now
}

func validateDNSLabel(fieldName, val string) error {
	val = strings.TrimSpace(val)
	if len(val) == 0 || len(val) > 63 {
		return fmt.Errorf("%s %q must be an RFC 1123 DNS label (1..63 characters)", fieldName, val)
	}
	for i := 0; i < len(val); i++ {
		c := val[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if (i == 0 || i == len(val)-1) && !isAlphaNum {
			return fmt.Errorf("%s %q must start and end with a lowercase alphanumeric character", fieldName, val)
		}
		if !isAlphaNum && c != '-' {
			return fmt.Errorf("%s %q contains invalid character %q (allowed: [a-z0-9-])", fieldName, val, c)
		}
	}
	return nil
}

// BuildEgressRules returns the rules for an actor's initial, immutable
// "default" EgressPolicy.
//
// The caller's Substrate EgressPolicy rules (from custom_config.egress_policy)
// are passed through unchanged. ate-gemini then appends one TlsPassthroughRule
// on every port for the Gemini API control-plane hosts (the default endpoint
// plus any custom serverURL hostname) so the worker can always open its bidi
// stream, skipping any host the caller already names on every port, which
// ate-api-server would reject as a tie. With no caller policy, the actor gets
// Substrate's default deny plus only the control-plane hosts.
func BuildEgressRules(policy *atepb.EgressPolicy, serverURL string) []*atepb.EgressRule {
	rules := make([]*atepb.EgressRule, 0, len(policy.GetRules())+1)
	for _, r := range policy.GetRules() {
		rules = append(rules, proto.Clone(r).(*atepb.EgressRule))
	}
	var hosts []string
	for _, h := range resolveControlPlaneHosts(serverURL) {
		if !namesHostOnAllPorts(rules, h) {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) > 0 {
		rules = append(rules, buildTLSPassthroughRule(hosts))
	}
	return rules
}

// namesHostOnAllPorts reports whether any rule lists the exact hostname
// pattern host with Ports.All, in which case adding another all-ports rule
// for host would tie at ate-api-server admission. ate-api-server rejects ties
// on a pattern and port whatever the rules' protocols, so http rules count.
func namesHostOnAllPorts(rules []*atepb.EgressRule, host string) bool {
	for _, r := range rules {
		var hostnames []string
		var ports *atepb.Ports
		switch {
		case r.GetHttp() != nil:
			hostnames, ports = r.GetHttp().GetHostnames(), r.GetHttp().GetPorts()
		case r.GetHttps() != nil:
			hostnames, ports = r.GetHttps().GetHostnames(), r.GetHttps().GetPorts()
		case r.GetTlsPassthrough() != nil:
			hostnames, ports = r.GetTlsPassthrough().GetHostnames(), r.GetTlsPassthrough().GetPorts()
		}
		if ports.GetAll() != nil && slices.Contains(hostnames, host) {
			return true
		}
	}
	return false
}

func resolveControlPlaneHosts(serverURL string) []string {
	hosts := []string{DefaultGeminiControlPlaneHost}
	trimmed := strings.TrimSpace(serverURL)
	if trimmed == "" {
		return hosts
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return hosts
	}
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
	if _, err := netip.ParseAddr(h); err == nil || h == "" || slices.Contains(hosts, h) {
		// IP literals (including IPv6 zones) are not valid SNI hostname patterns.
		return hosts
	}
	return append(hosts, h)
}

func buildTLSPassthroughRule(hostnames []string) *atepb.EgressRule {
	return &atepb.EgressRule{
		TlsPassthrough: &atepb.TlsPassthroughRule{
			Hostnames: hostnames,
			Ports:     &atepb.Ports{All: &atepb.AllPorts{}},
		},
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ActorName returns a deterministic RFC 1123 DNS label (max 63 chars) for a
// Gemini environment ID.
func ActorName(environmentID string) string {
	h := sha256.Sum256([]byte(environmentID))
	hashSuffix := fmt.Sprintf("%x", h[:8]) // 16 hex chars

	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(environmentID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && b.Len() > 0 {
			b.WriteByte('-')
			lastHyphen = true
		}
	}

	prefix := strings.Trim(b.String(), "-")
	if len(prefix) > 40 {
		prefix = strings.TrimRight(prefix[:40], "-")
	}
	if prefix == "" || prefix[0] < 'a' || prefix[0] > 'z' {
		prefix = "env-" + prefix
		prefix = strings.TrimRight(prefix, "-")
		if len(prefix) > 40 {
			prefix = strings.TrimRight(prefix[:40], "-")
		}
	}

	return fmt.Sprintf("%s-%s", prefix, hashSuffix)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type tokenAuth struct {
	tokenFile     string
	allowInsecure bool
}

func (t tokenAuth) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	data, err := os.ReadFile(t.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading ateapi token %q: %w", t.tokenFile, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, fmt.Errorf("ateapi token %q is empty", t.tokenFile)
	}
	return map[string]string{
		"authorization": "Bearer " + token,
	}, nil
}

func (t tokenAuth) RequireTransportSecurity() bool {
	return !t.allowInsecure
}

func extractTargetHost(targetAddr string) string {
	addrWithoutScheme := targetAddr
	if idx := strings.Index(addrWithoutScheme, "://"); idx >= 0 {
		addrWithoutScheme = strings.TrimLeft(addrWithoutScheme[idx+3:], "/")
	}
	if h, _, err := net.SplitHostPort(addrWithoutScheme); err == nil {
		return h
	}
	return addrWithoutScheme
}

// DialATE dials the Substrate ateapi gRPC control plane endpoint, attaching the
// projected Kubernetes service account token and ClusterTrustBundle CA when present.
func DialATE(endpoint string, insecureConn bool, tokenPath string, caFile string) (ategrpc.ControlClient, *grpc.ClientConn, error) {
	targetAddr := strings.TrimSpace(endpoint)
	if targetAddr == "" {
		targetAddr = DefaultAteapiAddr
	}

	var creds credentials.TransportCredentials
	if insecureConn {
		creds = local.NewCredentials()
	} else {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS13,
		}
		resolvedCA := strings.TrimSpace(caFile)
		if resolvedCA == "" {
			if _, err := os.Stat(DefaultAteapiCAFile); err == nil {
				resolvedCA = DefaultAteapiCAFile
			}
		}
		if resolvedCA != "" {
			pemBytes, err := os.ReadFile(resolvedCA)
			if err != nil {
				return nil, nil, fmt.Errorf("reading ateapi CA file %q: %w", resolvedCA, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				return nil, nil, fmt.Errorf("parsing ateapi CA bundle %q", resolvedCA)
			}
			tlsCfg.RootCAs = pool
		}
		if host := extractTargetHost(targetAddr); host == "api.ate-system.svc.cluster.local" || host == DefaultAteapiServerName {
			tlsCfg.ServerName = DefaultAteapiServerName
		}
		creds = credentials.NewTLS(tlsCfg)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	if tokenPath == "" {
		tokenPath = DefaultTokenPath
	}
	if _, err := os.Stat(tokenPath); err == nil {
		opts = append(opts, grpc.WithPerRPCCredentials(tokenAuth{
			tokenFile:     tokenPath,
			allowInsecure: insecureConn,
		}))
	}

	target := targetAddr
	if !strings.Contains(target, "://") {
		target = "passthrough:///" + target
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing ateapi %q: %w", endpoint, err)
	}
	return ategrpc.NewControlClient(conn), conn, nil
}

// DialRouter dials the Substrate atenet-router ingress endpoint over mTLS (using
// the servicedns ClusterTrustBundle CA and podidentity client credential bundle,
// reloaded dynamically on certificate rotation), or plaintext h2c when insecureConn is true.
func DialRouter(endpoint string, insecureConn bool, caFile string, clientBundleFile string) (*grpc.ClientConn, error) {
	targetAddr := strings.TrimSpace(endpoint)
	targetAddr = strings.TrimPrefix(targetAddr, "http://")
	targetAddr = strings.TrimPrefix(targetAddr, "https://")
	targetAddr = strings.TrimRight(targetAddr, "/")
	if targetAddr == "" {
		targetAddr = DefaultAtenetRouterAddr
	}

	var creds credentials.TransportCredentials
	if insecureConn {
		creds = newRouterCredentials()
	} else {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS13,
		}
		resolvedCA := strings.TrimSpace(caFile)
		if resolvedCA == "" {
			if _, err := os.Stat(DefaultAteapiCAFile); err == nil {
				resolvedCA = DefaultAteapiCAFile
			}
		}
		if resolvedCA != "" {
			pemBytes, err := os.ReadFile(resolvedCA)
			if err != nil {
				return nil, fmt.Errorf("reading atenet-router CA file %q: %w", resolvedCA, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				return nil, fmt.Errorf("parsing atenet-router CA bundle %q", resolvedCA)
			}
			tlsCfg.RootCAs = pool
		}
		if host := extractTargetHost(targetAddr); host == "atenet-router.ate-system.svc.cluster.local" || host == DefaultAtenetRouterServerName {
			tlsCfg.ServerName = DefaultAtenetRouterServerName
		}

		resolvedBundle := strings.TrimSpace(clientBundleFile)
		if resolvedBundle == "" {
			if _, err := os.Stat(DefaultPodIdentityBundleFile); err == nil {
				resolvedBundle = DefaultPodIdentityBundleFile
			}
		}
		if resolvedBundle != "" {
			if _, err := tls.LoadX509KeyPair(resolvedBundle, resolvedBundle); err != nil {
				return nil, fmt.Errorf("loading atenet-router client credential bundle %q: %w", resolvedBundle, err)
			}
			bundlePath := resolvedBundle
			tlsCfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				cert, err := tls.LoadX509KeyPair(bundlePath, bundlePath)
				if err != nil {
					return nil, fmt.Errorf("reloading podidentity credential bundle %q: %w", bundlePath, err)
				}
				return &cert, nil
			}
		}
		creds = credentials.NewTLS(tlsCfg)
	}

	target := targetAddr
	if !strings.Contains(target, "://") {
		target = "passthrough:///" + target
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dialing atenet-router %q: %w", endpoint, err)
	}
	return conn, nil
}
