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
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	workerpb "github.com/google-gemini/gemini-api-cli/docs/self-hosted/agent-substrate/pkg/workerpb"
	atepb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/local"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestBuildPollerArgs(t *testing.T) {
	cfg := ControllerConfig{
		RuntimeBin:                "/usr/local/bin/gemini-api",
		SelfBin:                   "/usr/local/bin/ate-gemini",
		Atespace:                  "team-a",
		Template:                  "custom-actor",
		AllowedAtespaces:          []string{"team-a", "team-b"},
		AllowedTemplates:          []string{"custom-actor", "gpu-actor"},
		AteapiAddr:                "api.ate-system.svc.cluster.local:443",
		AtenetRouterAddr:          "atenet-router.ate-system.svc.cluster.local:443",
		AteapiInsecure:            true,
		AteapiCAFile:              "/var/run/secrets/servicedns-ca/trust-bundle.pem",
		AtenetRouterInsecure:      true,
		AtenetClientBundle:        "/var/run/secrets/podidentity/credential-bundle.pem",
		TokenPath:                 "/tmp/token",
		ControlPlaneURL:           "https://generativelanguage.googleapis.com",
		EnvironmentID:             "env_abc",
		WorkerID:                  "poller-0",
		MaxConcurrentInteractions: 8,
		PollInterval:              2 * time.Second,
	}

	got, err := BuildPollerArgs(cfg)
	if err != nil {
		t.Fatalf("BuildPollerArgs() error = %v", err)
	}

	joined := strings.Join(got, "\n")
	for _, wantSubstr := range []string{
		"environments",
		"poll",
		"--max-interactions=8",
		"--poll-interval=2s",
		"--provision-cmd=/usr/local/bin/ate-gemini provision --ateapi-addr=api.ate-system.svc.cluster.local:443 --atenet-router-addr=atenet-router.ate-system.svc.cluster.local:443 --atespace=team-a --template=custom-actor '--allowed-atespaces=team-a,team-b' '--allowed-templates=custom-actor,gpu-actor' --ateapi-insecure --ateapi-ca-file=/var/run/secrets/servicedns-ca/trust-bundle.pem --atenet-router-insecure --atenet-client-bundle=/var/run/secrets/podidentity/credential-bundle.pem --token-path=/tmp/token",
		"--server-url=https://generativelanguage.googleapis.com",
		"--environment=env_abc",
		"--poller-id=poller-0",
	} {
		if !strings.Contains(joined, wantSubstr) {
			t.Errorf("BuildPollerArgs() missing %q in:\n%s", wantSubstr, joined)
		}
	}
	for _, unwanted := range []string{"--idle-timeout", "--suspend-cmd", "--worker-url-template"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("BuildPollerArgs() unexpectedly contains %q in:\n%s", unwanted, joined)
		}
	}
}

func TestBuildPollerArgsAlwaysForwardsEffectiveAtespaceAndTemplate(t *testing.T) {
	cfg := ControllerConfig{
		RuntimeBin: "/usr/local/bin/gemini-api",
		SelfBin:    "/usr/local/bin/ate-gemini",
	}

	got, err := BuildPollerArgs(cfg)
	if err != nil {
		t.Fatalf("BuildPollerArgs() error = %v", err)
	}

	joined := strings.Join(got, "\n")
	for _, want := range []string{"--atespace=" + DefaultAtespace, "--template=" + DefaultTemplate} {
		if !strings.Contains(joined, want) {
			t.Errorf("BuildPollerArgs() missing %q in:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{"--allowed-atespaces=", "--allowed-templates="} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("BuildPollerArgs() unexpectedly contains %q in:\n%s", unwanted, joined)
		}
	}
}

func TestBuildPollerArgsShellQuotesMetacharacters(t *testing.T) {
	cfg := ControllerConfig{
		RuntimeBin:       "/usr/local/bin/gemini-api",
		SelfBin:          "/usr/local/bin/ate-gemini",
		AllowedAtespaces: []string{"*"},
		TokenPath:        "/tmp/token with space",
		AteapiCAFile:     "/var/run/secrets/ca?name=bundle&v=1",
	}

	got, err := BuildPollerArgs(cfg)
	if err != nil {
		t.Fatalf("BuildPollerArgs() error = %v", err)
	}

	joined := strings.Join(got, "\n")
	for _, wantQuoted := range []string{
		"'--allowed-atespaces=*'",
		"'--token-path=/tmp/token with space'",
		"'--ateapi-ca-file=/var/run/secrets/ca?name=bundle&v=1'",
	} {
		if !strings.Contains(joined, wantQuoted) {
			t.Errorf("BuildPollerArgs() missing quoted token %s in:\n%s", wantQuoted, joined)
		}
	}
}

func TestRunControllerInvokesOpaqueBinary(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "runtime_args.txt")
	fakeRuntime := filepath.Join(dir, "fake_gemini_api.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\n"
	if err := os.WriteFile(fakeRuntime, []byte(script), 0755); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfg := ControllerConfig{
		RuntimeBin:       fakeRuntime,
		SelfBin:          "/usr/local/bin/ate-gemini",
		AteapiAddr:       "127.0.0.1:9443",
		AtenetRouterAddr: "127.0.0.1:8443",
		AteapiInsecure:   true,
		Stdout:           &stdout,
		Stderr:           &stderr,
	}

	if err := RunController(t.Context(), cfg); err != nil {
		t.Fatalf("RunController() error = %v; stderr = %s", err, stderr.String())
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("os.ReadFile(argsFile) error = %v", err)
	}
	if !strings.Contains(string(data), "environments\npoll") {
		t.Errorf("fake runtime args missing 'environments poll':\n%s", string(data))
	}
}

type fakeRouterWorkerServer struct {
	workerpb.UnimplementedWorkerServiceServer

	mu            sync.Mutex
	statusByActor map[string]*workerpb.GetIdleStatusResponse
	probedActors  []string
}

func (s *fakeRouterWorkerServer) GetIdleStatus(ctx context.Context, _ *workerpb.GetIdleStatusRequest) (*workerpb.GetIdleStatusResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	targets := md.Get(TargetActorHeader)
	if len(targets) == 0 {
		return nil, status.Error(codes.InvalidArgument, "missing ate-target-actor header")
	}
	target := targets[0]

	s.mu.Lock()
	defer s.mu.Unlock()
	s.probedActors = append(s.probedActors, target)
	resp, ok := s.statusByActor[target]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "unknown actor %s", target)
	}
	return resp, nil
}

func (s *fakeRouterWorkerServer) probedTargets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.probedActors))
	copy(out, s.probedActors)
	return out
}

func TestSweepIdleActorsSuspendsOnlyContinuouslyIdleApprovedRunningActors(t *testing.T) {
	fakeCtrl, ctrlClient, _ := startFakeControlServer(t)

	const (
		idleName       = "env-idle-0123456789abcdef"
		busyName       = "env-busy-0123456789abcdef"
		suspendedName  = "env-susp-0123456789abcdef"
		goldenUUID     = "550e8400-e29b-41d4-a716-446655440000"
		unapprovedName = "env-other-0123456789abcdef"
	)

	fakeCtrl.mu.Lock()
	fakeCtrl.actors[actorKey(DefaultAtespace, idleName)] = &atepb.Actor{
		Metadata:      &atepb.ResourceMetadata{Atespace: DefaultAtespace, Name: idleName},
		ActorTemplate: &atepb.ObjectRef{Atespace: DefaultAtespace, Name: DefaultTemplate},
		Status:        &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_RUNNING},
	}
	fakeCtrl.actors[actorKey(DefaultAtespace, busyName)] = &atepb.Actor{
		Metadata:      &atepb.ResourceMetadata{Atespace: DefaultAtespace, Name: busyName},
		ActorTemplate: &atepb.ObjectRef{Atespace: DefaultAtespace, Name: DefaultTemplate},
		Status:        &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_RUNNING},
	}
	fakeCtrl.actors[actorKey(DefaultAtespace, suspendedName)] = &atepb.Actor{
		Metadata:      &atepb.ResourceMetadata{Atespace: DefaultAtespace, Name: suspendedName},
		ActorTemplate: &atepb.ObjectRef{Atespace: DefaultAtespace, Name: DefaultTemplate},
		Status:        &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_SUSPENDED},
	}
	fakeCtrl.actors[actorKey(DefaultAtespace, goldenUUID)] = &atepb.Actor{
		Metadata:      &atepb.ResourceMetadata{Atespace: DefaultAtespace, Name: goldenUUID},
		ActorTemplate: &atepb.ObjectRef{Atespace: DefaultAtespace, Name: DefaultTemplate},
		Status:        &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_RUNNING},
	}
	fakeCtrl.actors[actorKey(DefaultAtespace, unapprovedName)] = &atepb.Actor{
		Metadata:      &atepb.ResourceMetadata{Atespace: DefaultAtespace, Name: unapprovedName},
		ActorTemplate: &atepb.ObjectRef{Atespace: DefaultAtespace, Name: "other-template"},
		Status:        &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_RUNNING},
	}
	fakeCtrl.mu.Unlock()

	routerLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	routerSrv := grpc.NewServer(grpc.Creds(local.NewCredentials()))
	workerFake := &fakeRouterWorkerServer{
		statusByActor: map[string]*workerpb.GetIdleStatusResponse{
			actorKey(DefaultAtespace, idleName): {Running: false, ActiveInteractions: 0},
			actorKey(DefaultAtespace, busyName): {Running: true, ActiveInteractions: 1},
		},
	}
	workerpb.RegisterWorkerServiceServer(routerSrv, workerFake)
	go func() {
		_ = routerSrv.Serve(routerLis)
	}()
	t.Cleanup(routerSrv.Stop)

	routerConn, err := grpc.NewClient(
		"passthrough:///"+routerLis.Addr().String(),
		grpc.WithTransportCredentials(local.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() {
		_ = routerConn.Close()
	})

	var clockMu sync.Mutex
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	mgr := NewManager(ctrlClient, ManagerConfig{
		Atespace: DefaultAtespace,
		Template: DefaultTemplate,
		NowFunc:  nowFunc,
	})
	workerClient := workerpb.NewWorkerServiceClient(routerConn)
	idleSince := make(map[string]time.Time)

	// Sweep 1 (t=0s): records initial idle timestamp; must not suspend yet.
	sweepIdleActorsOnce(t.Context(), mgr, workerClient, 60*time.Second, nowFunc, idleSince, newStderrLogger())
	fakeCtrl.mu.Lock()
	if got := fakeCtrl.actors[actorKey(DefaultAtespace, idleName)].GetStatus().GetState(); got != atepb.ActorState_ACTOR_STATE_RUNNING {
		fakeCtrl.mu.Unlock()
		t.Fatalf("after first sweep idle actor state = %v, want RUNNING", got)
	}
	fakeCtrl.mu.Unlock()

	// Sweep 2 (t=30s < 60s): still below idle timeout; must not suspend yet.
	advanceClock(30 * time.Second)
	sweepIdleActorsOnce(t.Context(), mgr, workerClient, 60*time.Second, nowFunc, idleSince, newStderrLogger())
	fakeCtrl.mu.Lock()
	if got := fakeCtrl.actors[actorKey(DefaultAtespace, idleName)].GetStatus().GetState(); got != atepb.ActorState_ACTOR_STATE_RUNNING {
		fakeCtrl.mu.Unlock()
		t.Fatalf("at t=30s idle actor state = %v, want RUNNING", got)
	}
	fakeCtrl.mu.Unlock()

	// Sweep 3 (t=61s >= 60s): idle actor is suspended; busy actor remains RUNNING.
	advanceClock(31 * time.Second)
	sweepIdleActorsOnce(t.Context(), mgr, workerClient, 60*time.Second, nowFunc, idleSince, newStderrLogger())

	fakeCtrl.mu.Lock()
	idleState := fakeCtrl.actors[actorKey(DefaultAtespace, idleName)].GetStatus().GetState()
	busyState := fakeCtrl.actors[actorKey(DefaultAtespace, busyName)].GetStatus().GetState()
	unapprovedState := fakeCtrl.actors[actorKey(DefaultAtespace, unapprovedName)].GetStatus().GetState()
	goldenState := fakeCtrl.actors[actorKey(DefaultAtespace, goldenUUID)].GetStatus().GetState()
	fakeCtrl.mu.Unlock()

	if idleState != atepb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("idle actor state = %v, want SUSPENDED", idleState)
	}
	if busyState != atepb.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("busy actor state = %v, want RUNNING", busyState)
	}
	if unapprovedState != atepb.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("unapproved template actor state = %v, want RUNNING", unapprovedState)
	}
	if goldenState != atepb.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("golden bake actor state = %v, want RUNNING", goldenState)
	}

	// Verify SUSPENDED, UUID golden-bake, and unapproved-template actors were NEVER probed via atenet-router.
	for _, probed := range workerFake.probedTargets() {
		switch probed {
		case actorKey(DefaultAtespace, suspendedName),
			actorKey(DefaultAtespace, goldenUUID),
			actorKey(DefaultAtespace, unapprovedName):
			t.Errorf("unexpected GetIdleStatus probe sent to %q", probed)
		}
	}

	// After a memory-snapshot resume, a large wall-clock jump must not suspend the
	// actor on its first idle probe: the idle window restarts from the resume.
	if _, ok := idleSince[actorKey(DefaultAtespace, idleName)]; ok {
		t.Fatalf("idleSince still tracks suspended actor %q", idleName)
	}
	fakeCtrl.mu.Lock()
	fakeCtrl.actors[actorKey(DefaultAtespace, idleName)].Status = &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_RUNNING}
	fakeCtrl.mu.Unlock()
	advanceClock(2 * time.Hour)
	sweepIdleActorsOnce(t.Context(), mgr, workerClient, 60*time.Second, nowFunc, idleSince, newStderrLogger())
	fakeCtrl.mu.Lock()
	resumedState := fakeCtrl.actors[actorKey(DefaultAtespace, idleName)].GetStatus().GetState()
	fakeCtrl.mu.Unlock()
	if resumedState != atepb.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("resumed actor state after first post-resume sweep = %v, want RUNNING", resumedState)
	}
}
