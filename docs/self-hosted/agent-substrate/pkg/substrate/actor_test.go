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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	// The Control gRPC stubs are imported under a second alias so builds that
	// generate messages and gRPC stubs into separate packages can map each.
	ategrpc "go.ate.dev/substrate/api/ateapi/v1"
	atepb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/local"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type fakeControlServer struct {
	ategrpc.UnimplementedControlServer

	mu            sync.Mutex
	atespaces     map[string]*atepb.Atespace
	actors        map[string]*atepb.Actor
	egressByActor map[string]*atepb.EgressPolicy
	stateSequence map[string][]atepb.ActorState
	calls         []string
}

func newFakeControlServer() *fakeControlServer {
	f := new(fakeControlServer)
	f.atespaces = make(map[string]*atepb.Atespace)
	f.actors = make(map[string]*atepb.Actor)
	f.egressByActor = make(map[string]*atepb.EgressPolicy)
	f.stateSequence = make(map[string][]atepb.ActorState)
	f.addAtespace(DefaultAtespace)
	return f
}

func (f *fakeControlServer) addAtespace(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atespaces[name] = &atepb.Atespace{
		Metadata: &atepb.ResourceMetadata{Name: name},
	}
}

func actorKey(atespace, name string) string {
	return atespace + "/" + name
}

func (f *fakeControlServer) recordCall(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeControlServer) recordedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeControlServer) GetAtespace(ctx context.Context, req *atepb.GetAtespaceRequest) (*atepb.Atespace, error) {
	f.recordCall("GetAtespace")
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return nil, status.Error(codes.InvalidArgument, "missing context deadline")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	as, ok := f.atespaces[req.GetAtespace().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "atespace not found")
	}
	return as, nil
}

func (f *fakeControlServer) CreateAtespace(ctx context.Context, req *atepb.CreateAtespaceRequest) (*atepb.Atespace, error) {
	f.recordCall("CreateAtespace")
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return nil, status.Error(codes.InvalidArgument, "missing context deadline")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetAtespace().GetMetadata().GetName()
	if _, ok := f.atespaces[name]; ok {
		return nil, status.Error(codes.AlreadyExists, "atespace already exists")
	}
	f.atespaces[name] = req.GetAtespace()
	return req.GetAtespace(), nil
}

func (f *fakeControlServer) GetActor(ctx context.Context, req *atepb.GetActorRequest) (*atepb.Actor, error) {
	f.recordCall("GetActor")
	f.mu.Lock()
	defer f.mu.Unlock()
	key := actorKey(req.GetActor().GetAtespace(), req.GetActor().GetName())
	actor, ok := f.actors[key]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	if seq := f.stateSequence[key]; len(seq) > 0 {
		next := seq[0]
		f.stateSequence[key] = seq[1:]
		actor.Status = &atepb.ActorStatus{State: next}
	}
	return actor, nil
}

func (f *fakeControlServer) ListActors(ctx context.Context, req *atepb.ListActorsRequest) (*atepb.ListActorsResponse, error) {
	f.recordCall("ListActors")
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return nil, status.Error(codes.InvalidArgument, "missing context deadline")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	keys := make([]string, 0, len(f.actors))
	for k := range f.actors {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	scope := strings.TrimSpace(req.GetAtespace())
	if scope != "" {
		if _, ok := f.atespaces[scope]; !ok {
			return nil, status.Error(codes.NotFound, "atespace not found")
		}
	}
	var matching []*atepb.Actor
	for _, k := range keys {
		actor := f.actors[k]
		if scope != "" && actor.GetMetadata().GetAtespace() != scope {
			continue
		}
		matching = append(matching, actor)
	}

	if req.GetOptions().GetPageToken() == "page2" {
		if len(matching) <= 1 {
			return &atepb.ListActorsResponse{}, nil
		}
		return &atepb.ListActorsResponse{Actors: matching[1:]}, nil
	}
	if len(matching) > 2 {
		return &atepb.ListActorsResponse{
			Actors:        matching[:1],
			NextPageToken: "page2",
		}, nil
	}
	return &atepb.ListActorsResponse{Actors: matching}, nil
}

func (f *fakeControlServer) CreateActor(ctx context.Context, req *atepb.CreateActorRequest) (*atepb.Actor, error) {
	f.recordCall("CreateActor")
	f.mu.Lock()
	defer f.mu.Unlock()
	key := actorKey(req.GetActor().GetMetadata().GetAtespace(), req.GetActor().GetMetadata().GetName())
	if _, ok := f.actors[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "actor already exists")
	}
	actor := &atepb.Actor{
		Metadata:      req.GetActor().GetMetadata(),
		ActorTemplate: req.GetActor().GetActorTemplate(),
		Status: &atepb.ActorStatus{
			State: atepb.ActorState_ACTOR_STATE_SUSPENDED,
		},
	}
	f.actors[key] = actor
	return actor, nil
}

func (f *fakeControlServer) CreateActorEgressPolicy(ctx context.Context, req *atepb.CreateActorEgressPolicyRequest) (*atepb.EgressPolicy, error) {
	f.recordCall("CreateActorEgressPolicy")
	if err := checkEgressRuleTies(req.GetEgressPolicy().GetRules()); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := actorKey(req.GetActor().GetAtespace(), req.GetActor().GetName())
	if _, exists := f.egressByActor[key]; exists {
		return nil, status.Error(codes.AlreadyExists, "egress policy already exists")
	}
	f.egressByActor[key] = req.GetEgressPolicy()
	return req.GetEgressPolicy(), nil
}

// checkEgressRuleTies mirrors ate-api-server's admission check rejecting two
// rules that match the same hostname on the same port, or both on all ports
// (HttpRule defaults to port 80 and HttpsRule to 443 when Ports is unset).
func checkEgressRuleTies(rules []*atepb.EgressRule) error {
	portKeys := func(p *atepb.Ports, def int32) []string {
		switch {
		case p == nil:
			return []string{fmt.Sprint(def)}
		case p.GetAll() != nil:
			return []string{"all"}
		}
		var keys []string
		for _, n := range p.GetNumbers() {
			keys = append(keys, fmt.Sprint(n))
		}
		return keys
	}
	seen := make(map[string]bool)
	for _, r := range rules {
		var hosts, keys []string
		switch {
		case r.GetHttp() != nil:
			hosts, keys = r.GetHttp().GetHostnames(), portKeys(r.GetHttp().GetPorts(), 80)
		case r.GetHttps() != nil:
			hosts, keys = r.GetHttps().GetHostnames(), portKeys(r.GetHttps().GetPorts(), 443)
		case r.GetTlsPassthrough() != nil:
			hosts, keys = r.GetTlsPassthrough().GetHostnames(), portKeys(r.GetTlsPassthrough().GetPorts(), 0)
		}
		for _, h := range hosts {
			for _, key := range keys {
				k := h + "|" + key
				if seen[k] {
					return status.Errorf(codes.InvalidArgument, "hostname %q ties on port %s", h, key)
				}
				seen[k] = true
			}
		}
	}
	return nil
}

func (f *fakeControlServer) ResumeActor(ctx context.Context, req *atepb.ResumeActorRequest) (*atepb.ResumeActorResponse, error) {
	f.recordCall("ResumeActor")
	f.mu.Lock()
	defer f.mu.Unlock()
	key := actorKey(req.GetActor().GetAtespace(), req.GetActor().GetName())
	actor, ok := f.actors[key]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	actor.Status = &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_RUNNING}
	return &atepb.ResumeActorResponse{Actor: actor, Resumed: true}, nil
}

func (f *fakeControlServer) RevertActor(ctx context.Context, req *atepb.RevertActorRequest) (*atepb.RevertActorResponse, error) {
	f.recordCall("RevertActor")
	f.mu.Lock()
	defer f.mu.Unlock()
	key := actorKey(req.GetActor().GetAtespace(), req.GetActor().GetName())
	actor, ok := f.actors[key]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	actor.Status = &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_SUSPENDED}
	return &atepb.RevertActorResponse{Actor: actor}, nil
}

func (f *fakeControlServer) SuspendActor(ctx context.Context, req *atepb.SuspendActorRequest) (*atepb.SuspendActorResponse, error) {
	f.recordCall("SuspendActor")
	f.mu.Lock()
	defer f.mu.Unlock()
	key := actorKey(req.GetActor().GetAtespace(), req.GetActor().GetName())
	actor, ok := f.actors[key]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	actor.Status = &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_SUSPENDED}
	return &atepb.SuspendActorResponse{Actor: actor}, nil
}

func startFakeControlServer(t *testing.T) (*fakeControlServer, ategrpc.ControlClient, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(local.NewCredentials()))
	fake := newFakeControlServer()
	ategrpc.RegisterControlServer(srv, fake)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///"+lis.Addr().String(),
		grpc.WithTransportCredentials(local.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return fake, ategrpc.NewControlClient(conn), lis.Addr().String()
}

func TestActorNameDeterministicAndRFC1123(t *testing.T) {
	tests := []struct {
		name  string
		envID string
	}{
		{name: "simple_id", envID: "env_abc123"},
		{name: "numeric_start", envID: "123456-env"},
		{name: "very_long_id", envID: strings.Repeat("environment-segment-", 5)},
		{name: "special_chars", envID: "projects/123/environments/my.env_01"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got1 := ActorName(tc.envID)
			got2 := ActorName(tc.envID)
			if got1 != got2 {
				t.Errorf("ActorName(%q) not deterministic: %q != %q", tc.envID, got1, got2)
			}
			if len(got1) == 0 || len(got1) > 63 {
				t.Errorf("ActorName(%q) len = %d, want 1..63 (%q)", tc.envID, len(got1), got1)
			}
			if got1[0] < 'a' || got1[0] > 'z' {
				t.Errorf("ActorName(%q) = %q, must start with lowercase letter", tc.envID, got1)
			}
			for _, r := range got1 {
				if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
					t.Errorf("ActorName(%q) = %q, contains invalid char %q", tc.envID, got1, r)
				}
			}
			if isGoldenActorUUID(got1) {
				t.Errorf("ActorName(%q) = %q unexpectedly matched golden actor UUID format", tc.envID, got1)
			}
		})
	}
}

func TestManagerEnsureAtespace(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	fake.addAtespace("my-atespace")
	mgr := NewManager(client, ManagerConfig{Atespace: "my-atespace"})

	if err := mgr.EnsureAtespace(t.Context()); err != nil {
		t.Fatalf("EnsureAtespace() existing atespace error = %v", err)
	}
	if err := mgr.EnsureAtespaceByName(t.Context(), "missing-atespace"); err == nil {
		t.Fatalf("EnsureAtespaceByName(missing-atespace) error = nil, want non-nil NotFound error")
	}
	if err := mgr.EnsureAtespaceByName(t.Context(), "bad/atespace"); err == nil {
		t.Fatalf("EnsureAtespaceByName(bad/atespace) error = nil, want invalid DNS label error")
	}

	wantCalls := []string{"GetAtespace", "GetAtespace"}
	if diff := cmp.Diff(wantCalls, fake.recordedCalls()); diff != "" {
		t.Errorf("EnsureAtespace() calls diff (-want +got):\n%s", diff)
	}
}

func TestManagerOperatorAllowlistsFailClosedByDefault(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	fake.addAtespace("other-atespace")
	mgr := NewManager(client, ManagerConfig{})

	if _, err := mgr.Provision(t.Context(), "env_unauth_as", &ProvisionEnvironmentConfig{
		Atespace: "other-atespace",
	}); err == nil || !strings.Contains(err.Error(), "not permitted by --allowed-atespaces") {
		t.Fatalf("Provision(unallowed atespace) error = %v, want --allowed-atespaces rejection", err)
	}
	if _, err := mgr.Provision(t.Context(), "env_unauth_tmpl", &ProvisionEnvironmentConfig{
		Template: "privileged-template",
	}); err == nil || !strings.Contains(err.Error(), "not permitted by --allowed-templates") {
		t.Fatalf("Provision(unallowed template) error = %v, want --allowed-templates rejection", err)
	}
	if err := mgr.SuspendInAtespace(t.Context(), "other-atespace", "env_unauth_as", ""); err == nil || !strings.Contains(err.Error(), "not permitted by --allowed-atespaces") {
		t.Fatalf("SuspendInAtespace(unallowed atespace) error = %v, want --allowed-atespaces rejection", err)
	}
	if len(fake.recordedCalls()) != 0 {
		t.Errorf("expected zero ateapi RPCs on allowlist rejection, got %v", fake.recordedCalls())
	}
}

func TestManagerProvisionColdStartAndImmutableInitialEgress(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	fake.addAtespace("tenant-atespace")
	mgr := NewManager(client, ManagerConfig{
		AllowedAtespaces: []string{"gemini-self-hosted", "tenant-atespace"},
		AllowedTemplates: []string{"gemini-env-actor", "tenant-actor-template"},
	})

	res, err := mgr.ProvisionWithServerURL(
		t.Context(),
		"env_cold_1",
		"https://custom-control-plane.example.com",
		&ProvisionEnvironmentConfig{
			Atespace: "tenant-atespace",
			Template: "tenant-actor-template",
			EgressPolicy: &atepb.EgressPolicy{Rules: []*atepb.EgressRule{
				{Http: &atepb.HttpRule{Hostnames: []string{"api.example.com", "*.github.com"}, Ports: &atepb.Ports{Numbers: []int32{80, 8080}}}},
			}},
		},
	)
	if err != nil {
		t.Fatalf("ProvisionWithServerURL() error = %v", err)
	}
	actorName := res.ActorName
	if want := ActorName("env_cold_1"); actorName != want {
		t.Errorf("Provision() actorName = %q, want %q", actorName, want)
	}
	if res.Atespace != "tenant-atespace" {
		t.Errorf("Provision() Atespace = %q, want %q", res.Atespace, "tenant-atespace")
	}
	if res.Template != "tenant-actor-template" {
		t.Errorf("Provision() Template = %q, want %q", res.Template, "tenant-actor-template")
	}
	if res.Action != "created" {
		t.Errorf("Provision() Action = %q, want %q", res.Action, "created")
	}

	wantCalls := []string{"GetAtespace", "GetActor", "CreateActor", "CreateActorEgressPolicy", "ResumeActor"}
	if diff := cmp.Diff(wantCalls, fake.recordedCalls()); diff != "" {
		t.Errorf("Provision() calls diff (-want +got):\n%s", diff)
	}

	fake.mu.Lock()
	egress := fake.egressByActor[actorKey("tenant-atespace", actorName)]
	fake.mu.Unlock()
	if egress == nil {
		t.Fatalf("egress policy was not created for actor %q in tenant-atespace", actorName)
	}
	if len(egress.GetRules()) != 2 {
		t.Fatalf("len(egress.Rules) = %d, want 2", len(egress.GetRules()))
	}
	gotHTTPPatterns := egress.GetRules()[0].GetHttp().GetHostnames()
	wantHTTPPatterns := []string{"api.example.com", "*.github.com"}
	if diff := cmp.Diff(wantHTTPPatterns, gotHTTPPatterns); diff != "" {
		t.Errorf("egress HTTP patterns diff (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]int32{80, 8080}, egress.GetRules()[0].GetHttp().GetPorts().GetNumbers()); diff != "" {
		t.Errorf("passthrough HTTP rule ports diff (-want +got):\n%s", diff)
	}
	gotTLSPatterns := egress.GetRules()[1].GetTlsPassthrough().GetHostnames()
	wantTLSPatterns := []string{
		DefaultGeminiControlPlaneHost,
		"custom-control-plane.example.com",
	}
	if diff := cmp.Diff(wantTLSPatterns, gotTLSPatterns); diff != "" {
		t.Errorf("egress TLS passthrough patterns diff (-want +got):\n%s", diff)
	}
	if egress.GetRules()[1].GetTlsPassthrough().GetPorts().GetAll() == nil {
		t.Errorf("expected TLS passthrough rule Ports.All to be set")
	}

	// Second turn on the RUNNING actor: initial egress policy remains immutable
	// (AlreadyExists on CreateActorEgressPolicy preserves the original rules).
	res2, err := mgr.ProvisionWithServerURL(
		t.Context(),
		"env_cold_1",
		"https://custom-control-plane.example.com",
		&ProvisionEnvironmentConfig{
			Atespace: "tenant-atespace",
			Template: "tenant-actor-template",
			EgressPolicy: &atepb.EgressPolicy{Rules: []*atepb.EgressRule{
				{TlsPassthrough: &atepb.TlsPassthroughRule{Hostnames: []string{"*"}, Ports: &atepb.Ports{All: &atepb.AllPorts{}}}},
			}},
		},
	)
	if err != nil {
		t.Fatalf("second ProvisionWithServerURL() error = %v", err)
	}
	if res2.Action != "running" {
		t.Errorf("second Provision() Action = %q, want %q", res2.Action, "running")
	}
	fake.mu.Lock()
	egressAfter := fake.egressByActor[actorKey("tenant-atespace", actorName)]
	fake.mu.Unlock()
	if diff := cmp.Diff(wantHTTPPatterns, egressAfter.GetRules()[0].GetHttp().GetHostnames()); diff != "" {
		t.Errorf("initial egress HTTP patterns mutated on turn 2 (-want +got):\n%s", diff)
	}
}

func TestBuildEgressRulesPassthrough(t *testing.T) {
	allPorts := &atepb.Ports{All: &atepb.AllPorts{}}
	tests := []struct {
		name        string
		policy      *atepb.EgressPolicy
		serverURL   string
		wantLen     int
		wantCPHosts []string // hostnames of the appended control-plane rule; nil if none appended
	}{
		{
			name:        "no_policy_default_deny_plus_control_plane",
			wantLen:     1,
			wantCPHosts: []string{DefaultGeminiControlPlaneHost},
		},
		{
			name:        "custom_server_url_host_appended",
			serverURL:   "https://gemini-proxy.example.com",
			wantLen:     1,
			wantCPHosts: []string{DefaultGeminiControlPlaneHost, "gemini-proxy.example.com"},
		},
		{
			name: "caller_rules_passed_through_with_control_plane_appended",
			policy: &atepb.EgressPolicy{Rules: []*atepb.EgressRule{
				{Http: &atepb.HttpRule{Hostnames: []string{"*"}}},
				{TlsPassthrough: &atepb.TlsPassthroughRule{Hostnames: []string{"*"}, Ports: allPorts}},
			}},
			wantLen:     3,
			wantCPHosts: []string{DefaultGeminiControlPlaneHost},
		},
		{
			name: "caller_names_control_plane_on_all_ports_not_duplicated",
			policy: &atepb.EgressPolicy{Rules: []*atepb.EgressRule{
				{TlsPassthrough: &atepb.TlsPassthroughRule{Hostnames: []string{DefaultGeminiControlPlaneHost}, Ports: allPorts}},
			}},
			wantLen: 1,
		},
		{
			name:      "caller_http_rule_on_all_ports_skips_only_that_host",
			serverURL: "https://gemini-proxy.example.com",
			policy: &atepb.EgressPolicy{Rules: []*atepb.EgressRule{
				{Http: &atepb.HttpRule{Hostnames: []string{DefaultGeminiControlPlaneHost}, Ports: allPorts}},
			}},
			wantLen:     2,
			wantCPHosts: []string{"gemini-proxy.example.com"},
		},
		{
			name: "caller_https_rule_on_all_ports_skips_control_plane",
			policy: &atepb.EgressPolicy{Rules: []*atepb.EgressRule{
				{Https: &atepb.HttpsRule{Hostnames: []string{DefaultGeminiControlPlaneHost}, Ports: allPorts}},
			}},
			wantLen: 1,
		},
		{
			name:        "ipv6_zone_server_url_host_not_appended",
			serverURL:   "http://[fe80::1%25eth0]:8080",
			wantLen:     1,
			wantCPHosts: []string{DefaultGeminiControlPlaneHost},
		},
		{
			name: "caller_names_control_plane_on_specific_port_still_appended",
			policy: &atepb.EgressPolicy{Rules: []*atepb.EgressRule{
				{Https: &atepb.HttpsRule{Hostnames: []string{DefaultGeminiControlPlaneHost}}},
			}},
			wantLen:     2,
			wantCPHosts: []string{DefaultGeminiControlPlaneHost},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := proto.Clone(tc.policy).(*atepb.EgressPolicy)
			rules := BuildEgressRules(tc.policy, tc.serverURL)
			if len(rules) != tc.wantLen {
				t.Fatalf("len(BuildEgressRules()) = %d, want %d (%v)", len(rules), tc.wantLen, rules)
			}
			for i, r := range tc.policy.GetRules() {
				if !proto.Equal(r, rules[i]) {
					t.Errorf("rule %d = %v, want caller rule passed through unchanged %v", i, rules[i], r)
				}
			}
			if tc.wantCPHosts != nil {
				last := rules[len(rules)-1].GetTlsPassthrough()
				if diff := cmp.Diff(tc.wantCPHosts, last.GetHostnames()); diff != "" {
					t.Errorf("control-plane hostnames diff (-want +got):\n%s", diff)
				}
				if last.GetPorts().GetAll() == nil {
					t.Errorf("control-plane rule Ports.All unset")
				}
			}
			if err := checkEgressRuleTies(rules); err != nil {
				t.Errorf("BuildEgressRules() produced tied rules: %v", err)
			}
			if !proto.Equal(before, tc.policy) {
				t.Errorf("BuildEgressRules() mutated caller policy")
			}
		})
	}
}

func TestManagerProvisionRejectsExistingActorOnRevokedTemplate(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	actorName := ActorName("env_revoked")
	fake.actors[actorKey(DefaultAtespace, actorName)] = &atepb.Actor{
		Metadata:      &atepb.ResourceMetadata{Atespace: DefaultAtespace, Name: actorName},
		ActorTemplate: &atepb.ObjectRef{Atespace: DefaultAtespace, Name: "revoked-template"},
		Status:        &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_SUSPENDED},
	}
	mgr := NewManager(client, ManagerConfig{})
	if _, err := mgr.Provision(t.Context(), "env_revoked", nil); err == nil || !strings.Contains(err.Error(), "not permitted by --allowed-templates") {
		t.Fatalf("Provision(existing actor on revoked template) error = %v, want --allowed-templates rejection", err)
	}
	for _, call := range fake.recordedCalls() {
		if call == "ResumeActor" {
			t.Errorf("ResumeActor called for actor on revoked template")
		}
	}
}

func TestManagerSuspendRejectsGoldenActorUUID(t *testing.T) {
	_, client, _ := startFakeControlServer(t)
	mgr := NewManager(client, ManagerConfig{})
	if err := mgr.SuspendInAtespace(t.Context(), "", "", "550e8400-e29b-41d4-a716-446655440000"); err == nil {
		t.Fatalf("SuspendInAtespace(golden UUID) error = nil, want rejection")
	}
}

func TestListRunningActorsSkipsMissingAllowedAtespace(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	actorName := ActorName("env_running")
	fake.actors[actorKey(DefaultAtespace, actorName)] = &atepb.Actor{
		Metadata:      &atepb.ResourceMetadata{Atespace: DefaultAtespace, Name: actorName},
		ActorTemplate: &atepb.ObjectRef{Atespace: DefaultAtespace, Name: DefaultTemplate},
		Status:        &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_RUNNING},
	}
	mgr := NewManager(client, ManagerConfig{AllowedAtespaces: []string{"aaa-not-created", DefaultAtespace}})
	got, err := mgr.ListRunningActors(t.Context())
	if err != nil {
		t.Fatalf("ListRunningActors() error = %v", err)
	}
	if len(got) != 1 || got[0].GetMetadata().GetName() != actorName {
		t.Errorf("ListRunningActors() = %v, want only %q", got, actorName)
	}
}

func TestTokenAuthRequireTransportSecurity(t *testing.T) {
	secureAuth := tokenAuth{tokenFile: "/tmp/token", allowInsecure: false}
	if !secureAuth.RequireTransportSecurity() {
		t.Errorf("RequireTransportSecurity() = false when allowInsecure=false, want true")
	}

	insecureAuth := tokenAuth{tokenFile: "/tmp/token", allowInsecure: true}
	if insecureAuth.RequireTransportSecurity() {
		t.Errorf("RequireTransportSecurity() = true when allowInsecure=true, want false")
	}
}

func TestManagerProvisionSuspendingThenSuspended(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	actorName := ActorName("env_trans_1")
	key := actorKey("gemini-self-hosted", actorName)
	fake.actors[key] = &atepb.Actor{
		Metadata: &atepb.ResourceMetadata{Atespace: "gemini-self-hosted", Name: actorName},
		Status:   &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_SUSPENDING},
	}
	fake.stateSequence[key] = []atepb.ActorState{
		atepb.ActorState_ACTOR_STATE_SUSPENDING,
		atepb.ActorState_ACTOR_STATE_SUSPENDED,
	}

	mgr := NewManager(client, ManagerConfig{
		Atespace:          "gemini-self-hosted",
		StatePollInterval: 5 * time.Millisecond,
	})
	if _, err := mgr.Provision(t.Context(), "env_trans_1", nil); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	wantCalls := []string{"GetAtespace", "GetActor", "CreateActorEgressPolicy", "GetActor", "ResumeActor"}
	if diff := cmp.Diff(wantCalls, fake.recordedCalls()); diff != "" {
		t.Errorf("Provision() calls diff (-want +got):\n%s", diff)
	}
}

func TestManagerProvisionCrashedRevertsThenResumes(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	actorName := ActorName("env_crashed_1")
	fake.actors[actorKey("gemini-self-hosted", actorName)] = &atepb.Actor{
		Metadata: &atepb.ResourceMetadata{Atespace: "gemini-self-hosted", Name: actorName},
		Status:   &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_CRASHED},
	}

	mgr := NewManager(client, ManagerConfig{Atespace: "gemini-self-hosted"})
	if _, err := mgr.Provision(t.Context(), "env_crashed_1", nil); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	wantCalls := []string{"GetAtespace", "GetActor", "CreateActorEgressPolicy", "RevertActor", "ResumeActor"}
	if diff := cmp.Diff(wantCalls, fake.recordedCalls()); diff != "" {
		t.Errorf("Provision() calls diff (-want +got):\n%s", diff)
	}
}

func TestManagerSuspendWithCooldownAndListRunningActors(t *testing.T) {
	fake, client, _ := startFakeControlServer(t)
	fake.addAtespace("other-atespace")
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	mgr := NewManager(client, ManagerConfig{
		Atespace:         "gemini-self-hosted",
		AllowedAtespaces: []string{"gemini-self-hosted", "missing-atespace"},
		ResumeCooldown:   10 * time.Second,
		NowFunc:          func() time.Time { return now },
	})

	res, err := mgr.Provision(t.Context(), "env_susp_1", nil)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	actorName := res.ActorName

	// Seed a golden UUID actor and an unrelated template actor; neither should be returned.
	fake.actors[actorKey("gemini-self-hosted", "01234567-89ab-cdef-0123-456789abcdef")] = &atepb.Actor{
		Metadata:      &atepb.ResourceMetadata{Atespace: "gemini-self-hosted", Name: "01234567-89ab-cdef-0123-456789abcdef"},
		ActorTemplate: &atepb.ObjectRef{Atespace: "gemini-self-hosted", Name: DefaultTemplate},
		Status:        &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_RUNNING},
	}
	fake.actors[actorKey("gemini-self-hosted", "other-workload-0123456789abcdef")] = &atepb.Actor{
		Metadata:      &atepb.ResourceMetadata{Atespace: "gemini-self-hosted", Name: "other-workload-0123456789abcdef"},
		ActorTemplate: &atepb.ObjectRef{Atespace: "gemini-self-hosted", Name: "unrelated-template"},
		Status:        &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_RUNNING},
	}

	// Within cooldown: ListRunningActors and Suspend both skip actorName.
	now = now.Add(3 * time.Second)
	mgrSingle := NewManager(client, ManagerConfig{
		Atespace:       "gemini-self-hosted",
		ResumeCooldown: 10 * time.Second,
		NowFunc:        func() time.Time { return now },
	})
	mgrSingle.recordResume("gemini-self-hosted", actorName)
	now = now.Add(2 * time.Second)
	runningCooldown, err := mgrSingle.ListRunningActors(t.Context())
	if err != nil {
		t.Fatalf("ListRunningActors(within cooldown) error = %v", err)
	}
	if len(runningCooldown) != 0 {
		t.Errorf("ListRunningActors(within cooldown) = %v, want empty", runningCooldown)
	}

	if err := mgr.Suspend(t.Context(), "env_susp_1", actorName); err != nil {
		t.Fatalf("Suspend() within cooldown error = %v", err)
	}
	for _, call := range fake.recordedCalls() {
		if call == "SuspendActor" {
			t.Fatalf("SuspendActor called within cooldown; calls = %v", fake.recordedCalls())
		}
	}

	// Reject invalid actor_name DNS label on SuspendInAtespace.
	if err := mgr.SuspendInAtespace(t.Context(), "gemini-self-hosted", "env_susp_1", "bad/actor"); err == nil {
		t.Fatalf("SuspendInAtespace(bad/actor) error = nil, want invalid DNS label error")
	}

	// Reject nonexistent (but allowlisted) atespace on SuspendInAtespace.
	if err := mgr.SuspendInAtespace(t.Context(), "missing-atespace", "env_susp_1", actorName); err == nil {
		t.Fatalf("SuspendInAtespace(missing-atespace) error = nil, want NotFound error")
	}

	// Past cooldown: ListRunningActors returns only actorName (paginating past golden & unrelated).
	now = now.Add(15 * time.Second)
	runningReady, err := mgrSingle.ListRunningActors(t.Context())
	if err != nil {
		t.Fatalf("ListRunningActors(past cooldown) error = %v", err)
	}
	if len(runningReady) != 1 || runningReady[0].GetMetadata().GetName() != actorName {
		t.Fatalf("ListRunningActors(past cooldown) = %+v, want [%s]", runningReady, actorName)
	}

	// Past cooldown: Suspend calls GetAtespace, GetActor, and SuspendActor.
	if err := mgr.Suspend(t.Context(), "env_susp_1", actorName); err != nil {
		t.Fatalf("Suspend() past cooldown error = %v", err)
	}
	gotCalls := fake.recordedCalls()
	if gotCalls[len(gotCalls)-1] != "SuspendActor" {
		t.Errorf("last call = %q, want SuspendActor (calls = %v)", gotCalls[len(gotCalls)-1], gotCalls)
	}
}

func TestDialATEAndDialRouterCredentialsAndValidation(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("test-sa-token\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile(token) error = %v", err)
	}

	// 1. Missing CA file returns error on both DialATE and DialRouter.
	if _, _, err := DialATE("api.ate-system.svc.cluster.local:443", false, tokenFile, filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatalf("DialATE(missing CA) error = nil, want non-nil")
	}
	if _, err := DialRouter("atenet-router.ate-system.svc.cluster.local:443", false, filepath.Join(dir, "missing.pem"), ""); err == nil {
		t.Fatalf("DialRouter(missing CA) error = nil, want non-nil")
	}

	// 2. Invalid PEM bundle returns error.
	badCA := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badCA, []byte("not-a-pem"), 0600); err != nil {
		t.Fatalf("os.WriteFile(badCA) error = %v", err)
	}
	if _, _, err := DialATE("api.ate-system.svc.cluster.local:443", false, tokenFile, badCA); err == nil {
		t.Fatalf("DialATE(bad CA) error = nil, want non-nil")
	}

	// 3. Valid CA + podidentity credential bundle succeed on DialATE and DialRouter.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey() error = %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: DefaultAteapiServerName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:              []string{DefaultAteapiServerName, "api.ate-system.svc.cluster.local", DefaultAtenetRouterServerName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate() error = %v", err)
	}
	caFile := filepath.Join(dir, "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(caFile, certPEM, 0600); err != nil {
		t.Fatalf("os.WriteFile(caFile) error = %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("x509.MarshalECPrivateKey() error = %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	credBundleFile := filepath.Join(dir, "credential-bundle.pem")
	if err := os.WriteFile(credBundleFile, append(keyPEM, certPEM...), 0600); err != nil {
		t.Fatalf("os.WriteFile(credBundleFile) error = %v", err)
	}

	_, tlsConn, err := DialATE("passthrough:///api.ate-system.svc.cluster.local:443", false, tokenFile, caFile)
	if err != nil {
		t.Fatalf("DialATE(valid CA + passthrough scheme) error = %v", err)
	}
	_ = tlsConn.Close()

	routerConn, err := DialRouter("atenet-router.ate-system.svc.cluster.local:443", false, caFile, credBundleFile)
	if err != nil {
		t.Fatalf("DialRouter(valid mTLS bundle) error = %v", err)
	}
	_ = routerConn.Close()

	if _, err := DialRouter("atenet-router.ate-system.svc.cluster.local:443", false, caFile, badCA); err == nil {
		t.Fatalf("DialRouter(invalid client bundle) error = nil, want non-nil")
	}

	// 4. Loopback insecureConn=true with token file succeeds against fake control server.
	fake, _, addr := startFakeControlServer(t)
	client, insecureConn, err := DialATE(addr, true, tokenFile, "")
	if err != nil {
		t.Fatalf("DialATE(insecureConn=true) error = %v", err)
	}
	defer func() {
		_ = insecureConn.Close()
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := client.GetAtespace(ctx, &atepb.GetAtespaceRequest{
		Atespace: &atepb.ObjectRef{Name: DefaultAtespace},
	}); err != nil {
		t.Fatalf("client.GetAtespace() via DialATE error = %v", err)
	}
	if len(fake.recordedCalls()) == 0 {
		t.Errorf("expected GetAtespace call recorded on fakeControlServer")
	}
}
