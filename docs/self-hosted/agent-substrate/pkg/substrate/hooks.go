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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	workerpb "github.com/google-gemini/gemini-api-cli/docs/self-hosted/agent-substrate/pkg/workerpb"
	atepb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	// HookSchemaVersionV1 is the JSON schema version spoken by
	// `gemini-api environments poll --provision-cmd`.
	HookSchemaVersionV1 = "v1"
)

// ProvisionEnvironmentConfig holds the options ate-gemini reads from
// `environment_config.custom_config` in ProvisionRequest and SuspendRequest.
//
// The Gemini API treats `custom_config` as an opaque JSON object for
// self-hosted environments, and ate-gemini reads only `custom_config`; every
// other `environment_config` field is ignored. Recognized keys are `atespace`,
// `template`, `worker_url_template`, `env`, and `egress_policy`. Unknown keys
// are rejected so typos never silently change behavior.
//
// `egress_policy` is an Agent Substrate `EgressPolicy` in protobuf JSON form
// (only `rules`; `metadata` is managed by ate-gemini). Its `http`, `https`, and
// `tlsPassthrough` rules are passed through to ateapi.CreateActorEgressPolicy
// unchanged, so callers choose which hosts and ports the actor may reach. Rule
// `effects` (credential header injection and actor JWTs) are rejected: any
// caller who can create interactions controls `custom_config`, so allowing
// effects would let them send operator credentials or actor identity tokens to
// a host of their choosing. It configures the actor's initial, immutable
// `default` EgressPolicy upon creation.
type ProvisionEnvironmentConfig struct {
	Atespace          string
	Template          string
	WorkerURLTemplate string
	Env               map[string]string
	EgressPolicy      *atepb.EgressPolicy
}

// UnmarshalJSON parses `environment_config` into ProvisionEnvironmentConfig,
// reading only `custom_config` and ignoring all other fields.
func (c *ProvisionEnvironmentConfig) UnmarshalJSON(data []byte) error {
	var envWrapper struct {
		CustomConfig json.RawMessage `json:"custom_config"`
	}
	if err := json.Unmarshal(data, &envWrapper); err != nil {
		return fmt.Errorf("invalid argument: parsing environment_config: %w", err)
	}

	var out ProvisionEnvironmentConfig
	if len(envWrapper.CustomConfig) > 0 && string(envWrapper.CustomConfig) != "null" {
		if !strings.HasPrefix(strings.TrimSpace(string(envWrapper.CustomConfig)), "{") {
			return errors.New("invalid argument: environment_config.custom_config must be a JSON object")
		}
		if err := parseCustomConfigJSON(envWrapper.CustomConfig, &out); err != nil {
			return fmt.Errorf("invalid argument: parsing environment_config.custom_config: %w", err)
		}
	}

	*c = out
	return nil
}

func parseCustomConfigJSON(data []byte, out *ProvisionEnvironmentConfig) error {
	var raw struct {
		Atespace          string                     `json:"atespace"`
		Template          string                     `json:"template"`
		WorkerURLTemplate string                     `json:"worker_url_template"`
		EgressPolicy      json.RawMessage            `json:"egress_policy"`
		Env               map[string]json.RawMessage `json:"env"`
	}
	if err := decodeStrictJSON(strings.NewReader(string(data)), &raw); err != nil {
		return err
	}

	out.Atespace = strings.TrimSpace(raw.Atespace)
	out.Template = strings.TrimSpace(raw.Template)
	out.WorkerURLTemplate = strings.TrimSpace(raw.WorkerURLTemplate)

	if len(raw.EgressPolicy) > 0 && string(raw.EgressPolicy) != "null" {
		policy, err := parseEgressPolicy(raw.EgressPolicy)
		if err != nil {
			return err
		}
		out.EgressPolicy = policy
	}

	if len(raw.Env) > 0 {
		out.Env = make(map[string]string, len(raw.Env))
		for k, rawVal := range raw.Env {
			val, hasValue, err := parseEnvVarEntry(k, rawVal)
			if err != nil {
				return err
			}
			if hasValue {
				out.Env[k] = val
			}
		}
		if len(out.Env) == 0 {
			out.Env = nil
		}
	}

	return nil
}

// parseEgressPolicy decodes a Substrate EgressPolicy from protobuf JSON
// (accepting both lowerCamelCase and snake_case field names and rejecting
// unknown fields) and rejects rule effects. Rule semantics are validated by
// ate-api-server.
func parseEgressPolicy(data []byte) (*atepb.EgressPolicy, error) {
	policy := &atepb.EgressPolicy{}
	if err := protojson.Unmarshal(data, policy); err != nil {
		return nil, fmt.Errorf("parsing custom_config.egress_policy: %w", err)
	}
	if policy.GetMetadata() != nil {
		return nil, errors.New("custom_config.egress_policy.metadata must not be set; ate-gemini manages it")
	}
	for i, r := range policy.GetRules() {
		if r.GetHttp().GetEffects() != nil || r.GetHttps().GetEffects() != nil {
			return nil, fmt.Errorf("custom_config.egress_policy.rules[%d]: effects (credential injection and actor JWTs) are not allowed in caller-supplied egress policies", i)
		}
	}
	return policy, nil
}

func parseEnvVarEntry(key string, raw json.RawMessage) (string, bool, error) {
	if !isValidEnvKey(key) {
		return "", false, fmt.Errorf("invalid environment variable name %q", key)
	}
	if isBlockedEnvKey(key) {
		return "", false, fmt.Errorf("reserved environment variable %q cannot be overridden", key)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, fmt.Errorf("environment variable %q must not be null", key)
	}
	trimmedRaw := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmedRaw, "\"") {
		var strVal string
		if err := decodeStrictJSON(strings.NewReader(trimmedRaw), &strVal); err != nil {
			return "", false, err
		}
		if strings.ContainsRune(strVal, '\x00') {
			return "", false, fmt.Errorf("environment variable %q contains NUL byte", key)
		}
		return strVal, true, nil
	}
	var entry struct {
		Value      *string `json:"value"`
		Credential *string `json:"credential"`
	}
	if err := decodeStrictJSON(strings.NewReader(trimmedRaw), &entry); err != nil {
		return "", false, fmt.Errorf("environment variable %q must be a string or EnvVar object: %w", key, err)
	}
	if entry.Value != nil && entry.Credential != nil {
		return "", false, fmt.Errorf("environment variable %q cannot specify both 'value' and 'credential'", key)
	}
	if entry.Value != nil {
		if strings.ContainsRune(*entry.Value, '\x00') {
			return "", false, fmt.Errorf("environment variable %q contains NUL byte", key)
		}
		return *entry.Value, true, nil
	}
	if entry.Credential != nil && strings.TrimSpace(*entry.Credential) != "" {
		// Server-managed credential reference with no plaintext value to inject locally.
		return "", false, nil
	}
	return "", false, fmt.Errorf("environment variable %q object must contain string 'value' or non-empty 'credential'", key)
}

// ProvisionRequest is the v1 JSON payload sent on stdin by
// `gemini-api environments poll` to `--provision-cmd`.
type ProvisionRequest struct {
	SchemaVersion     string                      `json:"schema_version"`
	EnvironmentID     string                      `json:"environment_id"`
	InteractionID     string                      `json:"interaction_id"`
	LeaseToken        string                      `json:"lease_token,omitempty"`
	ServerURL         string                      `json:"server_url,omitempty"`
	EnvironmentConfig *ProvisionEnvironmentConfig `json:"environment_config,omitempty"`
}

// ProvisionResponse is the v1 JSON payload written to stdout by
// `ate-gemini provision`.
type ProvisionResponse struct {
	SchemaVersion string            `json:"schema_version"`
	URI           string            `json:"uri"`
	Headers       map[string]string `json:"headers,omitempty"`
	WorkerID      string            `json:"worker_id,omitempty"`
	WorkerURL     string            `json:"worker_url,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// ProvisionOptions configures optional gRPC dial behavior used by RunProvisionWithOptions.
type ProvisionOptions struct {
	DialOpts []grpc.DialOption
}

// SuspendRequest is the v1 JSON payload read by `ate-gemini suspend`.
type SuspendRequest struct {
	SchemaVersion     string                      `json:"schema_version"`
	EnvironmentID     string                      `json:"environment_id"`
	WorkerID          string                      `json:"worker_id,omitempty"`
	IdleDuration      string                      `json:"idle_duration,omitempty"`
	EnvironmentConfig *ProvisionEnvironmentConfig `json:"environment_config,omitempty"`
}

// RunProvision reads a ProvisionRequest JSON payload from r, creates or resumes
// the corresponding Substrate actor via m, dispatches the leased interaction to
// the actor's `ate-gemini actor-daemon` via gRPC `WorkerService.StartWork` when
// `lease_token` is present, and writes a ProvisionResponse JSON payload to w.
func RunProvision(ctx context.Context, m *Manager, r io.Reader, w io.Writer) error {
	return RunProvisionWithOptions(ctx, m, r, w, ProvisionOptions{})
}

// RunProvisionWithOptions reads a ProvisionRequest JSON payload from r, creates
// or resumes the corresponding Substrate actor via m, dispatches work to the
// actor over gRPC if `lease_token` is provided, and writes a ProvisionResponse
// JSON payload to w using opts.
func RunProvisionWithOptions(ctx context.Context, m *Manager, r io.Reader, w io.Writer, opts ProvisionOptions) error {
	if m == nil {
		return errors.New("actor manager is nil")
	}

	var req ProvisionRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		return fmt.Errorf("decoding provision request: %w", err)
	}
	if v := strings.TrimSpace(req.SchemaVersion); v != "" && v != HookSchemaVersionV1 {
		return fmt.Errorf("unsupported provision schema_version %q (want %q)", v, HookSchemaVersionV1)
	}
	envID := strings.TrimSpace(req.EnvironmentID)
	if envID == "" {
		return errors.New("provision request environment_id is required")
	}
	if strings.ContainsAny(envID, "\x00\r\n") {
		return errors.New("provision request environment_id contains invalid control characters")
	}
	intID := strings.TrimSpace(req.InteractionID)
	tok := strings.TrimSpace(req.LeaseToken)
	if strings.ContainsAny(intID, "\x00\r\n") || strings.ContainsAny(tok, "\x00\r\n") {
		return errors.New("provision request interaction_id and lease_token must not contain control characters")
	}
	if tok != "" && intID == "" {
		return errors.New("provision request interaction_id is required when lease_token is set")
	}
	serverURL := strings.TrimSpace(req.ServerURL)
	if serverURL != "" {
		if err := validateControlPlaneURL(serverURL); err != nil {
			return err
		}
	}

	envCfg := req.EnvironmentConfig
	var workerURLTemplate string
	if envCfg != nil {
		workerURLTemplate = strings.TrimSpace(envCfg.WorkerURLTemplate)
	}
	if strings.ContainsAny(workerURLTemplate, " \t\r\n\x00") {
		return errors.New("worker_url_template must not contain whitespace or control characters")
	}

	res, err := m.ProvisionWithServerURL(ctx, envID, serverURL, envCfg)
	if err != nil {
		return err
	}

	atespace := firstNonEmpty(res.Atespace, m.Atespace())
	template := firstNonEmpty(res.Template, m.Template())

	if tok != "" {
		if err := dispatchWorkToActorGRPC(ctx, m, atespace, res.ActorName, req, opts.DialOpts); err != nil {
			return err
		}
	}

	metadataMap := map[string]string{
		"atespace": atespace,
		"template": template,
		"action":   res.Action,
	}
	if res.ActorUID != "" {
		metadataMap["actor_uid"] = res.ActorUID
	}

	resp := ProvisionResponse{
		SchemaVersion: HookSchemaVersionV1,
		URI:           m.RouterAddr(),
		WorkerID:      res.ActorName,
		WorkerURL:     ExpandWorkerURLTemplate(workerURLTemplate, res.ActorName, atespace, envID, res.ActorUID),
		Metadata:      metadataMap,
		Headers: map[string]string{
			TargetActorHeader:  fmt.Sprintf("%s/%s", atespace, res.ActorName),
			EnvoyTimeoutHeader: DefaultRouterTimeoutMs,
		},
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		return fmt.Errorf("encoding provision response: %w", err)
	}
	return nil
}

func validateControlPlaneURL(rawURL string) error {
	if strings.ContainsAny(rawURL, " \t\r\n\x00") {
		return errors.New("server_url must not contain whitespace or control characters")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return fmt.Errorf("invalid server_url %q: expected http(s) URL with non-empty host", rawURL)
	}
	return nil
}

func dispatchWorkToActorGRPC(ctx context.Context, m *Manager, atespace, actorName string, req ProvisionRequest, dialOpts []grpc.DialOption) error {
	routerAddr := strings.TrimSpace(m.RouterAddr())
	routerAddr = strings.TrimPrefix(routerAddr, "http://")
	routerAddr = strings.TrimPrefix(routerAddr, "https://")
	routerAddr = strings.TrimRight(routerAddr, "/")

	var (
		conn *grpc.ClientConn
		err  error
	)
	if len(dialOpts) > 0 {
		conn, err = grpc.NewClient(routerAddr, dialOpts...)
		if err != nil {
			return fmt.Errorf("dialing atenet-router %s: %w", routerAddr, err)
		}
	} else {
		conn, err = m.DialRouter()
		if err != nil {
			return err
		}
	}
	defer conn.Close()

	client := workerpb.NewWorkerServiceClient(conn)
	targetActor := fmt.Sprintf("%s/%s", atespace, actorName)

	pbReq := &workerpb.StartWorkRequest{
		InteractionId:   strings.TrimSpace(req.InteractionID),
		EnvironmentId:   strings.TrimSpace(req.EnvironmentID),
		LeaseToken:      strings.TrimSpace(req.LeaseToken),
		ControlPlaneUrl: strings.TrimSpace(req.ServerURL),
	}
	if env := req.EnvironmentConfig.GetEnv(); len(env) > 0 {
		pbReq.EnvironmentConfig = &workerpb.EnvironmentConfig{Env: env}
	}

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, 300*time.Millisecond); err != nil {
				return err
			}
		}

		rpcCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rpcCtx = metadata.AppendToOutgoingContext(
			rpcCtx,
			TargetActorHeader, targetActor,
			EnvoyTimeoutHeader, DefaultRouterTimeoutMs,
		)
		_, err := client.StartWork(rpcCtx, pbReq)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("WorkerService.StartWork to actor %s via %s: %w", targetActor, routerAddr, err)
		switch status.Code(err) {
		case codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied:
			return lastErr
		}
	}
	return lastErr
}

// ExpandWorkerURLTemplate expands {worker_id}, {atespace}, {environment_id}, and
// {actor_uid} placeholders in tmpl. It returns "" when tmpl is blank.
func ExpandWorkerURLTemplate(tmpl, workerID, atespace, environmentID, actorUID string) string {
	tmpl = strings.TrimSpace(tmpl)
	if tmpl == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"{worker_id}", workerID,
		"{atespace}", atespace,
		"{environment_id}", environmentID,
		"{actor_uid}", actorUID,
	)
	return replacer.Replace(tmpl)
}

// RunSuspend reads a SuspendRequest JSON payload from r and suspends the
// corresponding Substrate actor via m.
func RunSuspend(ctx context.Context, m *Manager, r io.Reader) error {
	if m == nil {
		return errors.New("actor manager is nil")
	}

	var req SuspendRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		return fmt.Errorf("decoding suspend request: %w", err)
	}
	if v := strings.TrimSpace(req.SchemaVersion); v != "" && v != HookSchemaVersionV1 {
		return fmt.Errorf("unsupported suspend schema_version %q (want %q)", v, HookSchemaVersionV1)
	}
	workerID := strings.TrimSpace(req.WorkerID)
	envID := strings.TrimSpace(req.EnvironmentID)
	if workerID == "" && envID == "" {
		return errors.New("suspend request worker_id or environment_id is required")
	}
	if workerID != "" {
		if err := validateDNSLabel("worker_id", workerID); err != nil {
			return err
		}
	}

	var atespace string
	if req.EnvironmentConfig != nil {
		atespace = strings.TrimSpace(req.EnvironmentConfig.Atespace)
	}

	return m.SuspendInAtespace(ctx, atespace, envID, workerID)
}

// GetEnv returns the parsed custom_config.env map, or nil when c is nil.
func (c *ProvisionEnvironmentConfig) GetEnv() map[string]string {
	if c == nil {
		return nil
	}
	return c.Env
}

func decodeStrictJSON(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("unexpected trailing data after JSON object")
	}
	return nil
}
