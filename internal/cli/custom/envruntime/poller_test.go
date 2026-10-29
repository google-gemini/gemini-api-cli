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
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSingleflightDoAndDoChan(t *testing.T) {
	var sf Singleflight
	var runs atomic.Int32
	entered := make(chan struct{})
	gate := make(chan struct{})

	var wg sync.WaitGroup
	var sharedCount atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, shared, err := sf.Do("k1", func() (any, error) {
				runs.Add(1)
				close(entered)
				<-gate
				return "v1", nil
			})
			if err != nil || val != "v1" {
				t.Errorf("sf.Do() = (%v, %v), want (\"v1\", nil)", val, err)
			}
			if shared {
				sharedCount.Add(1)
			}
		}()
	}
	<-entered
	for {
		sf.mu.Lock()
		allJoined := sf.m["k1"] != nil && sf.m["k1"].dups == 3
		sf.mu.Unlock()
		if allJoined {
			break
		}
		runtime.Gosched()
	}
	close(gate)
	wg.Wait()

	if runs.Load() != 1 || sharedCount.Load() != 4 {
		t.Errorf("sf.Do runs=%d sharedCount=%d, want runs=1 sharedCount=4", runs.Load(), sharedCount.Load())
	}

	// Sequential call after completion is unshared.
	val, shared, err := sf.Do("k1", func() (any, error) { return "v2", nil })
	if err != nil || val != "v2" || shared {
		t.Errorf("sequential sf.Do() = (%v, %v, %v), want (\"v2\", false, nil)", val, shared, err)
	}

	// DoChan deduplication & error propagation.
	gate2 := make(chan struct{})
	boom := errors.New("provision boom")
	ch1 := sf.DoChan("k2", func() (any, error) {
		<-gate2
		return nil, boom
	})
	ch2 := sf.DoChan("k2", func() (any, error) {
		return "unexpected", nil
	})
	close(gate2)
	r1, r2 := <-ch1, <-ch2
	if !errors.Is(r1.Err, boom) || !errors.Is(r2.Err, boom) || !r1.Shared || !r2.Shared {
		t.Errorf("DoChan results = (%+v, %+v), want shared boom errors", r1, r2)
	}

	// Panic recovery on Do and DoChan must return an error without deadlocking or poisoning the key.
	if _, _, pErr := sf.Do("k_panic", func() (any, error) { panic("boom-do") }); pErr == nil || !strings.Contains(pErr.Error(), "singleflight panic") {
		t.Errorf("sf.Do(panic) = %v, want singleflight panic error", pErr)
	}
	pRes := <-sf.DoChan("k_panic", func() (any, error) { panic("boom-chan") })
	if pRes.Err == nil || !strings.Contains(pRes.Err.Error(), "singleflight panic") {
		t.Errorf("sf.DoChan(panic) = %+v, want singleflight panic error", pRes)
	}
	if okVal, _, okErr := sf.Do("k_panic", func() (any, error) { return "recovered", nil }); okErr != nil || okVal != "recovered" {
		t.Errorf("sf.Do(after panic) = (%v, %v), want (\"recovered\", nil)", okVal, okErr)
	}
}

func TestShellCommandProvisionerAndStatelessPoller(t *testing.T) {
	origPath := os.Getenv("PATH")
	tempBin := t.TempDir()
	for _, shName := range []string{"sh", "sh.exe"} {
		_ = os.WriteFile(filepath.Join(tempBin, shName), []byte("#!/bin/sh\n"), 0o755)
	}
	t.Setenv("PATH", tempBin)
	if bin, args := ResolveProvisionShell("windows", "echo ok"); !strings.Contains(bin, "sh") || len(args) != 2 || args[0] != "-c" {
		t.Errorf("ResolveProvisionShell(windows with PATH sh) = (%q, %v), want sh -c", bin, args)
	}
	t.Setenv("PATH", "")
	if bin, args := ResolveProvisionShell("windows", "echo ok"); bin != "cmd.exe" || len(args) != 2 || args[0] != "/C" {
		t.Errorf("ResolveProvisionShell(windows without PATH sh) = (%q, %v), want cmd.exe /C", bin, args)
	}
	if bin, args := ResolveProvisionShell("linux", "echo ok"); bin != "sh" || len(args) != 2 || args[0] != "-c" {
		t.Errorf("ResolveProvisionShell(linux without PATH sh) = (%q, %v), want sh -c", bin, args)
	}
	t.Setenv("PATH", origPath)

	emptyProv := NewShellCommandProvisioner("")
	if err := emptyProv.ProvisionAndDispatch(t.Context(), ProvisionInput{InteractionID: "i1", EnvironmentID: "e1"}); err == nil {
		t.Error("ProvisionAndDispatch(empty cmd) = nil, want error")
	}

	tempDir := t.TempDir()
	outFile := filepath.Join(tempDir, "stdin.json")
	statFile := filepath.Join(tempDir, "stat.txt")
	t.Setenv("GEMINI_API_KEY", "secret-must-scrub")
	okProv := NewShellCommandProvisioner(`if [ -n "$GEMINI_API_KEY" ]; then exit 9; fi; cat /proc/self/stat > ` + statFile + `; cat > ` + outFile)
	if err := okProv.ProvisionAndDispatch(t.Context(), ProvisionInput{
		InteractionID:     "int_p1",
		EnvironmentID:     "env_p1",
		LeaseToken:        "tok_secret_123",
		ServerURL:         "https://generativelanguage.googleapis.com",
		EnvironmentConfig: json.RawMessage(`{"network_mode":"OFF"}`),
	}); err != nil {
		t.Fatalf("ProvisionAndDispatch(ok) failed: %v", err)
	}
	written, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("ReadFile(outFile) failed: %v", err)
	}
	var gotIn ProvisionInput
	if err := json.Unmarshal(written, &gotIn); err != nil || gotIn.SchemaVersion != WorkerProvisionSchemaVersionV1 || gotIn.InteractionID != "int_p1" {
		t.Errorf("ProvisionInput stdin = (%+v, %v)", gotIn, err)
	}
	if statBytes, err := os.ReadFile(statFile); err == nil {
		if idx := strings.LastIndexByte(string(statBytes), ')'); idx >= 0 {
			fields := strings.Fields(string(statBytes[idx+1:]))
			if len(fields) >= 3 {
				if childPgid, err := strconv.Atoi(fields[2]); err == nil && childPgid == os.Getpid() {
					t.Errorf("childPgid = %d, want != parent pid %d (ConfigureSubprocessGroup must set new process group)", childPgid, os.Getpid())
				}
			}
		}
	}

	// Verify LeaseToken is redacted from stderr on failure, and empty stderr omits parentheses.
	failProv := NewShellCommandProvisioner(`echo "leaked tok_secret_999" >&2; exit 2`)
	err = failProv.ProvisionAndDispatch(t.Context(), ProvisionInput{
		InteractionID: "int_f",
		EnvironmentID: "env_f",
		LeaseToken:    "tok_secret_999",
	})
	if err == nil || strings.Contains(err.Error(), "tok_secret_999") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("ProvisionAndDispatch(stderr redaction) = %v, want [REDACTED] without tok_secret_999", err)
	}
	silentFailProv := NewShellCommandProvisioner(`exit 3`)
	if err := silentFailProv.ProvisionAndDispatch(t.Context(), ProvisionInput{InteractionID: "int_sf", EnvironmentID: "env_sf"}); err == nil || strings.Contains(err.Error(), "():") {
		t.Errorf("ProvisionAndDispatch(empty stderr) = %v, want error without '():'", err)
	}

	// Test RunLeasePollLoop error backoff, reset after success, StartMsg logging, inFlight deduplication, and non-retryable termination.
	var loopLog bytes.Buffer
	var loopObs recordingObserver
	var loopPoll atomic.Int32
	var dupHandled atomic.Int32
	dupGate := make(chan struct{})
	loopErr := RunLeasePollLoop(t.Context(), LeasePollLoopConfig{
		Client: &fakeEnvClient{
			leaseFn: func(_ context.Context, req *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
				switch loopPoll.Add(1) {
				case 1:
					if req.MaxNewInteractions != 2 {
						t.Errorf("poll 1 MaxNewInteractions = %d, want 2", req.MaxNewInteractions)
					}
					return nil, errors.New("transient 1")
				case 2:
					return nil, errors.New("transient 2")
				case 3:
					return &LeaseInteractionsResult{
						Leases: []LeasedInteraction{
							{InteractionID: "int_dup", LeaseToken: "t1"},
							{InteractionID: "int_dup", LeaseToken: "t1"},
						},
					}, nil
				case 4:
					close(dupGate)
					return nil, errors.New("transient after reset")
				default:
					return nil, &TransportError{StatusCode: http.StatusForbidden, Message: "fatal 403"}
				}
			},
		},
		MaxSlots:     2,
		PollInterval: 10 * time.Millisecond,
		Logger:       log.New(&loopLog, "", 0),
		Observer:     &loopObs,
		ErrLabel:     "TestLoopErr",
		StartMsg:     "TestLoopStartedBanner",
	}, func(_ context.Context, _ *LeasedInteraction) {
		dupHandled.Add(1)
		<-dupGate
	})
	if loopErr == nil || !IsNonRetryableError(loopErr) {
		t.Errorf("RunLeasePollLoop() = %v, want non-retryable 403 error", loopErr)
	}
	if dupHandled.Load() != 1 {
		t.Errorf("dupHandled = %d, want 1 (inFlight must deduplicate concurrent identical lease IDs)", dupHandled.Load())
	}
	logStr := loopLog.String()
	// Poll 1 -> 10ms, Poll 2 -> 20ms, Poll 3 -> resets consecutiveErrors & logs StartMsg, Poll 4 -> 10ms (not 40ms!).
	if !strings.Contains(logStr, "TestLoopStartedBanner") || !strings.Contains(logStr, "(retrying in 20ms): transient 2") || !strings.Contains(logStr, "(retrying in 10ms): transient after reset") {
		t.Errorf("unexpected RunLeasePollLoop log:\n%s", logStr)
	}
	loopObs.mu.Lock()
	if len(loopObs.leasePolledCounts) < 4 || loopObs.leasePolledErrs[0] == nil || loopObs.leasePolledCounts[2] != 2 {
		t.Errorf("loopObs = counts:%v errs:%v", loopObs.leasePolledCounts, loopObs.leasePolledErrs)
	}
	loopObs.mu.Unlock()

	// StatelessPoller defaults, custom fields, ServerURL precedence, recordDispatchFailure shift, and dispatchLease recovery.
	customSP := NewStatelessPoller(StatelessPollerConfig{
		PollerID:        "custom-poller-id",
		MaxInteractions: 4,
	})
	if customSP.pollerID != "custom-poller-id" || customSP.maxInteractions != 4 {
		t.Errorf("customSP = (pollerID=%q, maxInteractions=%d)", customSP.pollerID, customSP.maxInteractions)
	}

	var hbCalls atomic.Int32
	provisionAborted := make(chan error, 1)
	var polledOnce atomic.Bool
	var firstSPMaxNew atomic.Int32
	pollerCtx, cancelPoller := context.WithCancel(t.Context())
	defer cancelPoller()

	spObs := &recordingObserver{}
	sp := NewStatelessPoller(StatelessPollerConfig{
		Client: &fakeEnvClient{
			leaseFn: func(_ context.Context, req *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
				if !polledOnce.Swap(true) {
					firstSPMaxNew.Store(req.MaxNewInteractions)
					return &LeaseInteractionsResult{
						Leases: []LeasedInteraction{{
							InteractionID: "int_cancel_me",
							EnvironmentID: "env_c",
							LeaseToken:    "tok_c",
						}},
					}, nil
				}
				return &LeaseInteractionsResult{}, nil
			},
		},
		Heartbeat: func(context.Context, string, string, string, string) (*HeartbeatResult, error) {
			if hbCalls.Add(1) >= 2 {
				return &HeartbeatResult{ShouldCancel: true}, nil
			}
			return &HeartbeatResult{Renewed: true}, nil
		},
		ProvisionFunc: func(ctx context.Context, _ ProvisionInput) error {
			<-ctx.Done()
			provisionAborted <- ctx.Err()
			return ctx.Err()
		},
		Observer:                 spObs,
		PollInterval:             15 * time.Millisecond,
		HandoffHeartbeatInterval: 15 * time.Millisecond,
		ProvisionTimeout:         5 * time.Second,
		Logger:                   log.New(io.Discard, "", 0),
	})
	if !strings.HasPrefix(sp.pollerID, "poller-") || sp.maxInteractions != 10 {
		t.Errorf("sp defaults = (pollerID=%q, maxInteractions=%d)", sp.pollerID, sp.maxInteractions)
	}

	// Seed an expired failure entry (>10m old) to verify recordDispatchFailure evicts it.
	sp.failures["stale_int"] = &statelessFailureState{
		consecutive: 1,
		cooldownEnd: time.Now().Add(-11 * time.Minute),
	}

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- sp.Run(pollerCtx)
	}()

	select {
	case abortErr := <-provisionAborted:
		if !errors.Is(abortErr, context.Canceled) {
			t.Errorf("ProvisionFunc abort err = %v, want context.Canceled", abortErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Heartbeat ShouldCancel to abort ProvisionFunc")
	}
	cancelPoller()
	<-runErrCh

	if firstSPMaxNew.Load() != 10 {
		t.Errorf("firstSPMaxNew = %d, want 10", firstSPMaxNew.Load())
	}
	sp.mu.Lock()
	_, staleStillPresent := sp.failures["stale_int"]
	sp.mu.Unlock()
	if staleStillPresent {
		t.Error("recordDispatchFailure did not evict >10m stale failure entry")
	}

	// Verify recordDispatchFailure backoff shift for consecutive=2 (~1s, not 2s) and cleanup on subsequent success.
	sp.recordDispatchFailure("int_shift")
	sp.recordDispatchFailure("int_shift")
	sp.mu.Lock()
	remShift := time.Until(sp.failures["int_shift"].cooldownEnd)
	sp.failures["int_shift"].cooldownEnd = time.Now().Add(-time.Millisecond)
	sp.mu.Unlock()
	if remShift <= 500*time.Millisecond || remShift > 1300*time.Millisecond {
		t.Errorf("recordDispatchFailure(consecutive=2) remaining cooldown = %v, want ~1s", remShift)
	}

	// Test StatelessPoller with nil Heartbeat (falling back to Client), ServerURL preservation vs HTTPClient fallback, and successful dispatch clearing failures.
	var gotServerURL string
	spSuccess := NewStatelessPoller(StatelessPollerConfig{
		Client:    NewHTTPClient(HTTPClientConfig{BaseURL: "https://fallback.example.com"}),
		ServerURL: "https://explicit.example.com",
		ProvisionFunc: func(_ context.Context, in ProvisionInput) error {
			gotServerURL = in.ServerURL
			return nil
		},
		Observer: spObs,
		Logger:   log.New(io.Discard, "", 0),
	})
	spSuccess.failures["int_recover"] = &statelessFailureState{consecutive: 1, cooldownEnd: time.Now().Add(-time.Second)}
	spSuccess.dispatchLease(t.Context(), &LeasedInteraction{InteractionID: "int_recover", EnvironmentID: "env_r", LeaseToken: "tok_r"})
	if gotServerURL != "https://explicit.example.com" {
		t.Errorf("dispatchLease(explicit ServerURL) = %q, want https://explicit.example.com", gotServerURL)
	}
	if _, stillFailed := spSuccess.failures["int_recover"]; stillFailed {
		t.Error("expected successful dispatchLease to delete failures[int_recover]")
	}
	spSuccess.cfg.ServerURL = ""
	spSuccess.dispatchLease(t.Context(), &LeasedInteraction{InteractionID: "int_recover_2", EnvironmentID: "env_r", LeaseToken: "tok_r"})
	if gotServerURL != "https://fallback.example.com" {
		t.Errorf("dispatchLease(fallback ServerURL) = %q, want https://fallback.example.com", gotServerURL)
	}
	// Verify heartbeatOnce when p.cfg.Heartbeat == nil invokes Client rather than panicking.
	spNilHB := NewStatelessPoller(StatelessPollerConfig{
		Client: &fakeEnvClient{
			leaseFn: func(_ context.Context, _ *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
				return &LeaseInteractionsResult{Leases: []LeasedInteraction{{InteractionID: "int_hb"}}}, nil
			},
		},
		Logger: log.New(io.Discard, "", 0),
	})
	if hbRes, err := spNilHB.heartbeatOnce(t.Context(), "int_hb", "env_1", "tok_1"); err != nil || !hbRes.Renewed {
		t.Errorf("spNilHB.heartbeatOnce() = (%+v, %v), want Renewed=true", hbRes, err)
	}

	spObs.mu.Lock()
	statuses := append([]string(nil), spObs.handoffStatuses...)
	spObs.mu.Unlock()
	if !slices.Contains(statuses, "error") || !slices.Contains(statuses, "accepted") {
		t.Errorf("spObs.handoffStatuses = %v, want both 'error' and 'accepted'", statuses)
	}
}

func TestDirectCommandProvisionerAndConfigConversion(t *testing.T) {
	if _, err := NewDirectCommandProvisioner("   ", ""); err == nil {
		t.Error("NewDirectCommandProvisioner(empty) = nil, want error")
	}

	t.Setenv("GEMINI_API_KEY", "secret-must-scrub")
	dir := t.TempDir()
	provScript := filepath.Join(dir, "prov.sh")
	suspScript := filepath.Join(dir, "susp.sh")
	provStdin := filepath.Join(dir, "prov_stdin.json")
	suspLog := filepath.Join(dir, "suspended.json")

	provBody := "#!/bin/sh\n" +
		"if [ \"$1\" != \"--prov-arg\" ] || [ -n \"$GEMINI_API_KEY\" ]; then echo \"bad prov env or arg\" >&2; exit 8; fi\n" +
		"cat > " + provStdin + "\n" +
		"if grep -q '\"env_fail\"' " + provStdin + "; then echo \"prov_stderr_detail\" >&2; exit 2; fi\n" +
		"echo '{\"uri\":\"  http://127.0.0.1:9001  \",\"worker_id\":\"w-99\",\"headers\":{\"X-Auth\":\"ok\"}}'\n"
	if err := os.WriteFile(provScript, []byte(provBody), 0o755); err != nil {
		t.Fatalf("WriteFile(provScript) failed: %v", err)
	}

	suspBody := "#!/bin/sh\n" +
		"if [ \"$1\" != \"--susp-arg\" ] || [ -n \"$GEMINI_API_KEY\" ]; then echo \"bad susp env or arg\" >&2; exit 8; fi\n" +
		"cat > " + suspLog + "\n" +
		"if grep -q '\"env_susp_fail\"' " + suspLog + "; then echo \"susp_stderr_detail\" >&2; exit 2; fi\n"
	if err := os.WriteFile(suspScript, []byte(suspBody), 0o755); err != nil {
		t.Fatalf("WriteFile(suspScript) failed: %v", err)
	}

	dcp, err := NewDirectCommandProvisioner(provScript+" --prov-arg", suspScript+" --susp-arg")
	if err != nil {
		t.Fatalf("NewDirectCommandProvisioner() failed: %v", err)
	}
	if _, err := dcp.ProvisionWorker(t.Context(), WorkerProvisionRequest{}); err == nil {
		t.Error("ProvisionWorker(empty EnvironmentID) = nil, want error")
	}
	resp, err := dcp.ProvisionWorker(t.Context(), WorkerProvisionRequest{
		EnvironmentID: "  env_d1  ",
		InteractionID: "  int_d1  ",
	})
	if err != nil || resp.URI != "http://127.0.0.1:9001" || resp.WorkerID != "w-99" || resp.SchemaVersion != WorkerProvisionSchemaVersionV1 {
		t.Fatalf("ProvisionWorker() = (%+v, %v)", resp, err)
	}
	rawProvIn, err := os.ReadFile(provStdin)
	if err != nil {
		t.Fatalf("ReadFile(provStdin) failed: %v", err)
	}
	var gotProvReq WorkerProvisionRequest
	if err := json.Unmarshal(rawProvIn, &gotProvReq); err != nil || gotProvReq.SchemaVersion != WorkerProvisionSchemaVersionV1 || gotProvReq.EnvironmentID != "env_d1" || gotProvReq.InteractionID != "int_d1" {
		t.Errorf("ProvisionWorker stdin = (%+v, %v)", gotProvReq, err)
	}
	if _, err := dcp.ProvisionWorker(t.Context(), WorkerProvisionRequest{EnvironmentID: "env_fail"}); err == nil || !strings.Contains(err.Error(), "prov_stderr_detail") {
		t.Errorf("ProvisionWorker(env_fail) = %v, want prov_stderr_detail", err)
	}

	if err := dcp.SuspendWorker(t.Context(), WorkerSuspendRequest{}); err == nil {
		t.Error("SuspendWorker(empty ids) = nil, want error")
	}
	if err := dcp.SuspendWorker(t.Context(), WorkerSuspendRequest{
		EnvironmentID: "  env_d1  ",
		WorkerID:      "  w-99  ",
		IdleDuration:  "  65s  ",
	}); err != nil {
		t.Fatalf("SuspendWorker() failed: %v", err)
	}
	rawSusp, err := os.ReadFile(suspLog)
	if err != nil {
		t.Fatalf("ReadFile(suspLog) failed: %v", err)
	}
	var gotSuspReq WorkerSuspendRequest
	if err := json.Unmarshal(rawSusp, &gotSuspReq); err != nil || gotSuspReq.SchemaVersion != WorkerProvisionSchemaVersionV1 || gotSuspReq.EnvironmentID != "env_d1" || gotSuspReq.WorkerID != "w-99" || gotSuspReq.IdleDuration != "65s" {
		t.Errorf("SuspendWorker stdin = (%+v, %v)", gotSuspReq, err)
	}
	// SuspendWorker succeeds when only EnvironmentID or only WorkerID is provided.
	if err := dcp.SuspendWorker(t.Context(), WorkerSuspendRequest{EnvironmentID: "env_only"}); err != nil {
		t.Errorf("SuspendWorker(EnvironmentID only) = %v, want nil", err)
	}
	if err := dcp.SuspendWorker(t.Context(), WorkerSuspendRequest{WorkerID: "w-only"}); err != nil {
		t.Errorf("SuspendWorker(WorkerID only) = %v, want nil", err)
	}
	if err := dcp.SuspendWorker(t.Context(), WorkerSuspendRequest{EnvironmentID: "env_susp_fail", WorkerID: "w-99"}); err == nil || !strings.Contains(err.Error(), "susp_stderr_detail") {
		t.Errorf("SuspendWorker(env_susp_fail) = %v, want susp_stderr_detail", err)
	}

	if ConvertToProvisionEnvironmentConfig(nil) != nil {
		t.Error("ConvertToProvisionEnvironmentConfig(nil) != nil")
	}
	if ConvertToProvisionEnvironmentConfig(&EnvironmentConfig{}) != nil {
		t.Error("ConvertToProvisionEnvironmentConfig(empty) != nil")
	}
	if envOnly := ConvertToProvisionEnvironmentConfig(&EnvironmentConfig{Env: map[string]string{"ONLY_ENV": "V1"}}); envOnly == nil || envOnly.Env["ONLY_ENV"] != "V1" {
		t.Errorf("ConvertToProvisionEnvironmentConfig(Env only) = %+v", envOnly)
	}
	if allowStructOnly := ConvertToProvisionEnvironmentConfig(&EnvironmentConfig{AllowlistDomains: []string{"struct.example.com"}}); allowStructOnly == nil || len(allowStructOnly.AllowlistDomains) != 1 || allowStructOnly.AllowlistDomains[0] != "struct.example.com" {
		t.Errorf("ConvertToProvisionEnvironmentConfig(AllowlistDomains struct only) = %+v", allowStructOnly)
	}
	if allowRawOnly := ConvertToProvisionEnvironmentConfig(&EnvironmentConfig{RawJSON: json.RawMessage(`{"allowlist_domains":["raw.example.com"]}`)}); allowRawOnly == nil || len(allowRawOnly.AllowlistDomains) != 1 || allowRawOnly.AllowlistDomains[0] != "raw.example.com" {
		t.Errorf("ConvertToProvisionEnvironmentConfig(RawJSON allowlist_domains only) = %+v", allowRawOnly)
	}
	if denyOnly := ConvertToProvisionEnvironmentConfig(&EnvironmentConfig{DenylistDomains: []string{"bad.example.com"}}); denyOnly == nil || len(denyOnly.DenylistDomains) != 1 {
		t.Errorf("ConvertToProvisionEnvironmentConfig(DenylistDomains only) = %+v", denyOnly)
	}
	if modeOnly := ConvertToProvisionEnvironmentConfig(&EnvironmentConfig{NetworkMode: "OFF"}); modeOnly == nil || modeOnly.NetworkMode != "OFF" {
		t.Errorf("ConvertToProvisionEnvironmentConfig(NetworkMode only) = %+v", modeOnly)
	}
	if modeRawOnly := ConvertToProvisionEnvironmentConfig(&EnvironmentConfig{RawJSON: json.RawMessage(`{"network_mode":"ALLOWLIST"}`)}); modeRawOnly == nil || modeRawOnly.NetworkMode != "ALLOWLIST" {
		t.Errorf("ConvertToProvisionEnvironmentConfig(RawJSON network_mode only) = %+v", modeRawOnly)
	}
	if got := truncateCommandOutput("  short  "); got != "short" {
		t.Errorf("truncateCommandOutput(short) = %q, want short", got)
	}
	if got := truncateCommandOutput(strings.Repeat("x", maxCommandErrorBytes+10)); !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("truncateCommandOutput(long) = %q, want ...(truncated)", got)
	}
	if srcOnly := ConvertToProvisionEnvironmentConfig(&EnvironmentConfig{Sources: []EnvironmentSource{{Type: "inline", Target: "main.py"}}}); srcOnly == nil || len(srcOnly.Sources) != 1 {
		t.Errorf("ConvertToProvisionEnvironmentConfig(Sources only) = %+v", srcOnly)
	}
	parsed := ParseEnvironmentConfig(json.RawMessage(`{
		"network_mode": "ALLOWLIST",
		"allowlist_domains": ["a.example.com"],
		"network_allowlist": [{"domain": "b.example.com"}, {"domain": "bad.example.com", "mode": "deny"}],
		"network_denylist": ["evil.example.com"],
		"env": {"K1": "V1"},
		"env_vars": {"K2": {"value": "V2"}},
		"sources": [{"type": "inline", "target": "main.py", "content": "print(1)"}]
	}`))
	conv := ConvertToProvisionEnvironmentConfig(parsed)
	if conv == nil || len(conv.AllowlistDomains) != 2 || len(conv.DenylistDomains) != 2 || conv.Env["K1"] != "V1" || conv.Env["K2"] != "V2" || len(conv.Sources) != 1 {
		t.Errorf("ConvertToProvisionEnvironmentConfig() = %+v", conv)
	}
}

type fakeWorkerProvisioner struct {
	mu          sync.Mutex
	provisions  int
	suspensions []WorkerSuspendRequest
	provFn      func(req WorkerProvisionRequest) (*WorkerProvisionResponse, error)
	provCtxFn   func(ctx context.Context, req WorkerProvisionRequest) (*WorkerProvisionResponse, error)
}

func (f *fakeWorkerProvisioner) ProvisionWorker(ctx context.Context, req WorkerProvisionRequest) (*WorkerProvisionResponse, error) {
	f.mu.Lock()
	f.provisions++
	ctxFn := f.provCtxFn
	fn := f.provFn
	f.mu.Unlock()
	if ctxFn != nil {
		return ctxFn(ctx, req)
	}
	if fn != nil {
		return fn(req)
	}
	return &WorkerProvisionResponse{
		SchemaVersion: WorkerProvisionSchemaVersionV1,
		URI:           "  worker://" + req.EnvironmentID + "  ",
		WorkerID:      "wk-" + req.EnvironmentID,
		Headers:       map[string]string{"X-Worker": "1"},
	}, nil
}

func (f *fakeWorkerProvisioner) SuspendWorker(_ context.Context, req WorkerSuspendRequest) error {
	f.mu.Lock()
	f.suspensions = append(f.suspensions, req)
	f.mu.Unlock()
	return nil
}

type fakeWorkerRPCClient struct {
	startFn func(ctx context.Context, req *StartWorkRequest, headers map[string]string) (*StartWorkResponse, error)
	idleFn  func(headers map[string]string) (*IdleStatus, error)
	closed  atomic.Bool
}

func (f *fakeWorkerRPCClient) StartWork(ctx context.Context, req *StartWorkRequest, headers map[string]string) (*StartWorkResponse, error) {
	return f.startFn(ctx, req, headers)
}

func (f *fakeWorkerRPCClient) GetIdleStatus(_ context.Context, headers map[string]string) (*IdleStatus, error) {
	if headers["X-Worker"] != "1" {
		return nil, errors.New("missing cloned X-Worker header in GetIdleStatus")
	}
	if f.idleFn != nil {
		return f.idleFn(headers)
	}
	return &IdleStatus{Running: false, IdleDuration: 2 * time.Minute}, nil
}

func (f *fakeWorkerRPCClient) Close() error {
	f.closed.Store(true)
	return nil
}

func TestTier2PollerSingleflightPoisonPillSweepAndDrain(t *testing.T) {
	if _, err := NewTier2Poller(Tier2PollerConfig{}); err == nil {
		t.Error("NewTier2Poller(missing Client) = nil, want error")
	}
	dummyClient := &fakeEnvClient{leaseFn: func(context.Context, *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
		return &LeaseInteractionsResult{}, nil
	}}
	if _, err := NewTier2Poller(Tier2PollerConfig{Client: dummyClient}); err == nil {
		t.Error("NewTier2Poller(missing Provisioner) = nil, want error")
	}
	if _, err := NewTier2Poller(Tier2PollerConfig{Client: dummyClient, Provisioner: &fakeWorkerProvisioner{}}); err == nil {
		t.Error("NewTier2Poller(missing DialWorker) = nil, want error")
	}

	emptyCfg := Tier2PollerConfig{}
	emptyCfg.Defaults()
	if emptyCfg.MaxConcurrentInteractions != DefaultPollerMaxConcurrentInteractions ||
		emptyCfg.PollInterval != DefaultPollBackoff ||
		emptyCfg.LeaseTTL != DefaultLeaseTTL ||
		emptyCfg.WorkerID == "" ||
		emptyCfg.MaxHandoffRetries != DefaultMaxHandoffRetries ||
		emptyCfg.HandoffRetryBackoff != DefaultHandoffRetryBackoff ||
		emptyCfg.PoisonCooldown != DefaultPoisonCooldown ||
		emptyCfg.SweepInterval != DefaultSweepInterval ||
		emptyCfg.DrainTimeout != DefaultDrainTimeout ||
		emptyCfg.IdleTimeout != DefaultWorkerIdleTimeout ||
		emptyCfg.ResumeGracePeriod != DefaultWorkerResumeGracePeriod ||
		emptyCfg.Now == nil ||
		emptyCfg.NewTimer == nil ||
		emptyCfg.Logger == nil ||
		emptyCfg.Observer == nil {
		t.Errorf("emptyCfg.Defaults() did not populate all defaults: %+v", emptyCfg)
	}

	var customTimerCalled bool
	customCfg := Tier2PollerConfig{
		MaxConcurrentInteractions: 7,
		PollInterval:              3 * time.Second,
		LeaseTTL:                  12 * time.Second,
		WorkerID:                  "explicit-worker-id",
		MaxHandoffRetries:         9,
		HandoffRetryBackoff:       4 * time.Second,
		PoisonCooldown:            20 * time.Second,
		SweepInterval:             11 * time.Second,
		DrainTimeout:              13 * time.Second,
		IdleTimeout:               14 * time.Second,
		ResumeGracePeriod:         15 * time.Second,
		NewTimer: func(d time.Duration) *time.Timer {
			customTimerCalled = true
			return time.NewTimer(d)
		},
	}
	customCfg.Defaults()
	if customCfg.MaxConcurrentInteractions != 7 ||
		customCfg.PollInterval != 3*time.Second ||
		customCfg.LeaseTTL != 12*time.Second ||
		customCfg.WorkerID != "explicit-worker-id" ||
		customCfg.MaxHandoffRetries != 9 ||
		customCfg.HandoffRetryBackoff != 4*time.Second ||
		customCfg.PoisonCooldown != 20*time.Second ||
		customCfg.SweepInterval != 11*time.Second ||
		customCfg.DrainTimeout != 13*time.Second ||
		customCfg.IdleTimeout != 14*time.Second ||
		customCfg.ResumeGracePeriod != 15*time.Second ||
		customCfg.Observer == nil {
		t.Errorf("customCfg.Defaults() overwrote custom values: %+v", customCfg)
	}
	tm := customCfg.NewTimer(time.Millisecond)
	tm.Stop()
	if !customTimerCalled {
		t.Error("customCfg.Defaults() overwrote custom NewTimer")
	}

	nowMu := sync.Mutex{}
	fakeNow := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	getNow := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return fakeNow
	}
	advanceNow := func(d time.Duration) {
		nowMu.Lock()
		fakeNow = fakeNow.Add(d)
		nowMu.Unlock()
	}

	prov := &fakeWorkerProvisioner{
		provFn: func(req WorkerProvisionRequest) (*WorkerProvisionResponse, error) {
			uri := "worker://shared"
			if req.EnvironmentID == "env_poison" {
				uri = "worker://poison"
			}
			return &WorkerProvisionResponse{
				SchemaVersion: WorkerProvisionSchemaVersionV1,
				URI:           uri,
				WorkerID:      "wk-" + req.EnvironmentID,
				Headers:       map[string]string{"X-Worker": "1", "X-Env": req.EnvironmentID},
			}, nil
		},
	}
	var dials atomic.Int32
	var accepted atomic.Int32
	var allowPoison atomic.Bool
	var env2Busy atomic.Bool
	var env2ShortIdle atomic.Bool
	env2Busy.Store(true)
	inFlightStarted := make(chan struct{})
	inFlightGate := make(chan struct{})
	var closeInFlightOnce sync.Once
	drainStarted := make(chan struct{})
	var closeDrainOnce sync.Once

	sharedConn := &fakeWorkerRPCClient{
		startFn: func(ctx context.Context, req *StartWorkRequest, headers map[string]string) (*StartWorkResponse, error) {
			if req.InteractionID == "int_poison" && !allowPoison.Load() {
				return nil, errors.New("worker rejected start_work")
			}
			if headers["X-Worker"] != "1" {
				return nil, errors.New("missing worker header")
			}
			if req.InteractionID == "int_ok_1" {
				closeInFlightOnce.Do(func() { close(inFlightStarted) })
				<-inFlightGate
			}
			if req.InteractionID == "int_drain" {
				closeDrainOnce.Do(func() { close(drainStarted) })
				time.Sleep(25 * time.Millisecond)
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			accepted.Add(1)
			return &StartWorkResponse{Status: WorkerStatusAccepted}, nil
		},
		idleFn: func(headers map[string]string) (*IdleStatus, error) {
			if headers["X-Env"] == "env_shared_2" {
				if env2Busy.Load() {
					return &IdleStatus{Running: true, IdleDuration: 2 * time.Minute}, nil
				}
				if env2ShortIdle.Load() {
					return &IdleStatus{Running: false, IdleDuration: 10 * time.Second}, nil
				}
			}
			return &IdleStatus{Running: false, IdleDuration: 2 * time.Minute}, nil
		},
	}
	poisonConn := &fakeWorkerRPCClient{
		startFn: sharedConn.startFn,
		idleFn: func(map[string]string) (*IdleStatus, error) {
			return &IdleStatus{Running: true}, nil
		},
	}

	var pollStep atomic.Int32
	var sawDrainZeroMaxNew atomic.Bool
	client := &fakeEnvClient{
		leaseFn: func(_ context.Context, req *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
			switch pollStep.Add(1) {
			case 1:
				// Two concurrent interactions in env_shared + one in env_shared_2 (sharing worker://shared URI) + one poison interaction!
				return &LeaseInteractionsResult{
					Leases: []LeasedInteraction{
						{InteractionID: "int_ok_1", EnvironmentID: "env_shared", LeaseToken: "tok_1"},
						{InteractionID: "int_ok_2", EnvironmentID: "env_shared", LeaseToken: "tok_2"},
						{InteractionID: "int_ok_3", EnvironmentID: "env_shared_2", LeaseToken: "tok_3"},
						{InteractionID: "int_poison", EnvironmentID: "env_poison", LeaseToken: "tok_bad"},
					},
					PollBackoff: 20 * time.Millisecond,
				}, nil
			case 2:
				// Poll 2 occurs while int_ok_1 is still inFlight -> must update token without spawning a second handoff!
				return &LeaseInteractionsResult{
					Leases: []LeasedInteraction{
						{InteractionID: "int_ok_1", EnvironmentID: "env_shared", LeaseToken: "tok_1_renewed"},
					},
					PollBackoff: 20 * time.Millisecond,
				}, nil
			case 3:
				// Poll 3 occurs after cooldown expires and allowPoison=true -> int_poison succeeds and clears failure/cooldown/deadLetter state!
				return &LeaseInteractionsResult{
					Leases: []LeasedInteraction{
						{InteractionID: "int_poison", EnvironmentID: "env_poison", LeaseToken: "tok_recovered"},
					},
					PollBackoff: 20 * time.Millisecond,
				}, nil
			case 4:
				// Poll 4 occurs inside Run() -> claims int_drain so drainInFlightHandoffs is exercised with an active handoff!
				return &LeaseInteractionsResult{
					Leases: []LeasedInteraction{
						{InteractionID: "int_drain", EnvironmentID: "env_shared", LeaseToken: "tok_drain"},
					},
					PollBackoff: 5 * time.Second,
				}, nil
			default:
				if req.MaxNewInteractions == 0 && req.SendZeroMaxNew {
					sawDrainZeroMaxNew.Store(true)
				}
				return &LeaseInteractionsResult{PollBackoff: 20 * time.Millisecond}, nil
			}
		},
	}

	tpObs := &recordingObserver{}
	var poisonDials atomic.Int32
	tp, err := NewTier2Poller(Tier2PollerConfig{
		Client:      client,
		Provisioner: prov,
		Observer:    tpObs,
		DialWorker: func(uri string) (WorkerRPCClient, error) {
			if uri != strings.TrimSpace(uri) {
				return nil, errors.New("untrimmed worker uri: " + uri)
			}
			dials.Add(1)
			if uri == "worker://poison" {
				if poisonDials.Add(1) == 1 {
					return nil, errors.New("dial poison failed on attempt 1")
				}
				return poisonConn, nil
			}
			return sharedConn, nil
		},
		ControlPlaneURL:     "https://generativelanguage.googleapis.com",
		GRPCTarget:          "generativelanguage.googleapis.com:443",
		MaxHandoffRetries:   2,
		HandoffRetryBackoff: 5 * time.Millisecond,
		PoisonCooldown:      30 * time.Second,
		IdleTimeout:         60 * time.Second,
		ResumeGracePeriod:   3 * time.Second,
		PollInterval:        5 * time.Second,
		SweepInterval:       1 * time.Hour,
		DrainTimeout:        2 * time.Second,
		Now:                 getNow,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewTier2Poller() failed: %v", err)
	}
	if tp.cfg.LeaseTTL != DefaultLeaseTTL || tp.cfg.WorkerID == "" {
		t.Errorf("tp.cfg defaults: LeaseTTL=%v WorkerID=%q", tp.cfg.LeaseTTL, tp.cfg.WorkerID)
	}

	if _, err := tp.PollOnce(t.Context()); err != nil {
		t.Fatalf("PollOnce(1) failed: %v", err)
	}
	// Wait for int_ok_1 to enter StartWork, then run PollOnce(2) while int_ok_1 is still inFlight[id]==true!
	<-inFlightStarted
	if _, err := tp.PollOnce(t.Context()); err != nil {
		t.Fatalf("PollOnce(2) failed: %v", err)
	}
	// Seed a future cooldown on int_ok_1 before releasing inFlightGate so we verify accepted handoff deletes cooldownUntil[id].
	tp.mu.Lock()
	tp.cooldownUntil["int_ok_1"] = getNow().Add(time.Hour)
	tp.mu.Unlock()
	close(inFlightGate)
	tp.WaitHandoffs()

	// If inFlight["int_ok_1"] was not tracked, PollOnce(2) would have spawned a duplicate handoff and accepted would be 4 instead of 3!
	if accepted.Load() != 3 {
		t.Errorf("accepted = %d, want 3", accepted.Load())
	}
	// Verify getOrDialWorkerClient trims whitespace around URI and hits cached connection.
	if cachedConn, err := tp.getOrDialWorkerClient("  worker://shared  "); err != nil || cachedConn != sharedConn {
		t.Errorf("getOrDialWorkerClient(whitespace URI) = (%v, %v), want cached sharedConn", cachedConn, err)
	}
	if tp.InCooldown("int_ok_1") {
		t.Error("expected accepted handoff to clear cooldownUntil[int_ok_1]")
	}
	if tp.InFlightCount() != 0 || len(tp.HandoffLeases()) != 0 {
		t.Errorf("InFlightCount=%d HandoffLeases=%v, want 0", tp.InFlightCount(), tp.HandoffLeases())
	}
	if !tp.IsDeadLettered("int_poison") || !tp.InCooldown("int_poison") || tp.FailureCount("int_poison") != 2 {
		t.Errorf("int_poison state: dead=%v cooldown=%v failures=%d", tp.IsDeadLettered("int_poison"), tp.InCooldown("int_poison"), tp.FailureCount("int_poison"))
	}

	// Within ResumeGracePeriod (0s elapsed < 3s), SweepOnce must NOT suspend active workers.
	if err := tp.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce(grace period) failed: %v", err)
	}
	prov.mu.Lock()
	if len(prov.suspensions) != 0 {
		t.Errorf("suspensions during grace period = %v, want empty", prov.suspensions)
	}
	prov.mu.Unlock()

	// Advance clock past ResumeGracePeriod & PoisonCooldown.
	advanceNow(40 * time.Second)
	if tp.InCooldown("int_poison") {
		t.Error("InCooldown(int_poison) = true after 40s, want false")
	}
	// Even after cooldown expires, deadLettered["int_poison"] must remain true until a handoff succeeds!
	if !tp.IsDeadLettered("int_poison") {
		t.Error("IsDeadLettered(int_poison) = false after cooldown expiry, want true")
	}

	// Sweep 1: env_shared is idle, but env_shared_2 is still Running=true (sharing worker://shared URI), so sharedConn must NOT be closed yet!
	if err := tp.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce(1 after grace) failed: %v", err)
	}
	prov.mu.Lock()
	suspAfter1 := len(prov.suspensions)
	prov.mu.Unlock()
	if suspAfter1 != 1 || sharedConn.closed.Load() {
		t.Errorf("after Sweep 1: suspensions=%d sharedConn.closed=%v, want 1 and false (uriStillUsed=true)", suspAfter1, sharedConn.closed.Load())
	}

	// Sweep 1b: env_shared_2 has Running=false but IdleDuration (10s) < IdleTimeout (60s) -> must NOT be suspended!
	env2Busy.Store(false)
	env2ShortIdle.Store(true)
	if err := tp.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce(1b short idle) failed: %v", err)
	}
	prov.mu.Lock()
	suspAfter1b := len(prov.suspensions)
	prov.mu.Unlock()
	if suspAfter1b != 1 || sharedConn.closed.Load() {
		t.Errorf("after Sweep 1b: suspensions=%d sharedConn.closed=%v, want 1 and false (IdleDuration < IdleTimeout)", suspAfter1b, sharedConn.closed.Load())
	}

	// Poll 3: allow int_poison (worker://poison) to succeed before Sweep 2 so activeWorkers contains a worker with a different URI,
	// and verify failureCounts and deadLettered are cleared!
	allowPoison.Store(true)
	if _, err := tp.PollOnce(t.Context()); err != nil {
		t.Fatalf("PollOnce(3 recovery) failed: %v", err)
	}
	tp.WaitHandoffs()
	if tp.FailureCount("int_poison") != 0 || tp.IsDeadLettered("int_poison") {
		t.Errorf("after recovery: FailureCount=%d IsDeadLettered=%v, want 0 and false", tp.FailureCount("int_poison"), tp.IsDeadLettered("int_poison"))
	}

	// Sweep 2: env_shared_2 becomes idle (>= IdleTimeout) while env_poison (worker://poison) is Running=true ->
	// env_shared_2 is suspended and sharedConn is closed because no remaining active worker uses worker://shared!
	env2ShortIdle.Store(false)
	if err := tp.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce(2) failed: %v", err)
	}
	prov.mu.Lock()
	suspAfter2 := len(prov.suspensions)
	prov.mu.Unlock()
	if suspAfter2 != 2 || !sharedConn.closed.Load() {
		t.Errorf("after Sweep 2: suspensions=%d sharedConn.closed=%v, want 2 and true", suspAfter2, sharedConn.closed.Load())
	}

	// Sweep 3: suspended workers were deleted from activeWorkers and env_poison is Running=true, so no further suspensions occur.
	if err := tp.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce(3) failed: %v", err)
	}
	prov.mu.Lock()
	suspAfter3 := len(prov.suspensions)
	prov.mu.Unlock()
	if suspAfter3 != 2 {
		t.Errorf("after Sweep 3: suspensions=%d, want 2 (deleted activeWorkers must not be re-suspended)", suspAfter3)
	}

	// Run() + in-flight drain + Close().
	sharedConn.closed.Store(false)
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- tp.Run(runCtx)
	}()
	// Wait until int_drain is mid-handoff, then cancel runCtx to trigger drainInFlightHandoffs!
	select {
	case <-drainStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for int_drain handoff to start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("tp.Run() = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("tp.Run() did not finish draining rapidly when stateChangedCh fired")
	}
	if accepted.Load() != 5 {
		t.Errorf("accepted after drain = %d, want 5 (drainInFlightHandoffs must wait for in-flight handoff)", accepted.Load())
	}
	if !sawDrainZeroMaxNew.Load() {
		t.Error("expected drainInFlightHandoffs pollOnce to set MaxNewInteractions=0 (c.draining=true)")
	}
	if !sharedConn.closed.Load() || !poisonConn.closed.Load() {
		t.Error("expected Close() to close cached WorkerRPCClient connections")
	}
	// Verify Close() unlocked c.mu by calling a lock-acquiring method after Run() returns.
	if tp.InFlightCount() != 0 {
		t.Errorf("tp.InFlightCount() after Run() = %d, want 0", tp.InFlightCount())
	}

	tpObs.mu.Lock()
	if len(tpObs.workerProvisioned) == 0 || len(tpObs.leasePolledCounts) == 0 || !slices.Contains(tpObs.handoffStatuses, "accepted") {
		t.Errorf("tpObs = prov:%v polled:%v handoff:%v", tpObs.workerProvisioned, tpObs.leasePolledCounts, tpObs.handoffStatuses)
	}
	tpObs.mu.Unlock()
}

func TestTier2PollerHandoffCancellationAndTokenRefresh(t *testing.T) {
	// 1. Public PollOnce(pollCtx) without Run(): canceling pollCtx must cancel in-flight handoff immediately
	// without dead-lettering or waiting for DefaultHandoffAttemptTimeout.
	pollOnceStarted := make(chan struct{})
	tpDirect, err := NewTier2Poller(Tier2PollerConfig{
		Client: &fakeEnvClient{
			leaseFn: func(context.Context, *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
				return &LeaseInteractionsResult{
					Leases: []LeasedInteraction{{InteractionID: "int_direct_cancel", EnvironmentID: "env_d", LeaseToken: "tok_d"}},
				}, nil
			},
		},
		Provisioner: &fakeWorkerProvisioner{
			provFn: func(WorkerProvisionRequest) (*WorkerProvisionResponse, error) {
				return &WorkerProvisionResponse{SchemaVersion: WorkerProvisionSchemaVersionV1, URI: "worker://d"}, nil
			},
		},
		DialWorker: func(string) (WorkerRPCClient, error) {
			return &fakeWorkerRPCClient{
				startFn: func(ctx context.Context, _ *StartWorkRequest, _ map[string]string) (*StartWorkResponse, error) {
					close(pollOnceStarted)
					<-ctx.Done()
					return nil, ctx.Err()
				},
			}, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewTier2Poller(tpDirect) failed: %v", err)
	}
	pollCtx, cancelPoll := context.WithCancel(t.Context())
	if _, err := tpDirect.PollOnce(pollCtx); err != nil {
		t.Fatalf("tpDirect.PollOnce() failed: %v", err)
	}
	<-pollOnceStarted
	cancelPoll()
	waitDone := make(chan struct{})
	go func() {
		tpDirect.WaitHandoffs()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("PollOnce handoff did not respect caller context cancellation promptly")
	}
	tpDirect.mu.Lock()
	directDead := tpDirect.deadLettered["int_direct_cancel"]
	tpDirect.mu.Unlock()
	if directDead {
		t.Error("expected canceled PollOnce handoff not to be dead-lettered")
	}

	// 2. Run(runCtx) with short DrainTimeout: when StartWork blocks past DrainTimeout, closing stopHandoffsCh
	// cancels the detached handoff context so Run() and WaitHandoffs() return promptly.
	runBlockedStarted := make(chan struct{})
	var closeRunBlockedOnce sync.Once
	tpRunTimeout, err := NewTier2Poller(Tier2PollerConfig{
		Client: &fakeEnvClient{
			leaseFn: func(_ context.Context, req *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
				if req.MaxNewInteractions == 0 {
					return &LeaseInteractionsResult{}, nil
				}
				return &LeaseInteractionsResult{
					Leases:      []LeasedInteraction{{InteractionID: "int_drain_timeout", EnvironmentID: "env_dt", LeaseToken: "tok_dt"}},
					PollBackoff: 5 * time.Second,
				}, nil
			},
		},
		Provisioner: &fakeWorkerProvisioner{
			provFn: func(WorkerProvisionRequest) (*WorkerProvisionResponse, error) {
				return &WorkerProvisionResponse{SchemaVersion: WorkerProvisionSchemaVersionV1, URI: "worker://dt"}, nil
			},
		},
		DialWorker: func(string) (WorkerRPCClient, error) {
			return &fakeWorkerRPCClient{
				startFn: func(ctx context.Context, _ *StartWorkRequest, _ map[string]string) (*StartWorkResponse, error) {
					closeRunBlockedOnce.Do(func() { close(runBlockedStarted) })
					<-ctx.Done()
					return nil, ctx.Err()
				},
			}, nil
		},
		DrainTimeout:  25 * time.Millisecond,
		SweepInterval: time.Hour,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewTier2Poller(tpRunTimeout) failed: %v", err)
	}
	runCtx, cancelRun := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() {
		runDone <- tpRunTimeout.Run(runCtx)
	}()
	<-runBlockedStarted
	cancelRun()
	select {
	case runErr := <-runDone:
		if runErr != nil {
			t.Errorf("tpRunTimeout.Run() = %v, want nil", runErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("tpRunTimeout.Run() did not cancel blocked handoff after DrainTimeout")
	}
	waitRunDone := make(chan struct{})
	go func() {
		tpRunTimeout.WaitHandoffs()
		close(waitRunDone)
	}()
	select {
	case <-waitRunDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("tpRunTimeout.WaitHandoffs() remained blocked after stopHandoffsCh closed")
	}

	// 3. Cold-start provisioning token rotation: if a concurrent PollOnce refreshes handoffLeases[id]
	// while ProvisionWorker is in flight, StartWork must receive the refreshed LeaseToken.
	provEntered := make(chan struct{})
	provRelease := make(chan struct{})
	var coldPollCount atomic.Int32
	var gotStartToken atomic.Value
	tpCold, err := NewTier2Poller(Tier2PollerConfig{
		Client: &fakeEnvClient{
			leaseFn: func(context.Context, *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
				if coldPollCount.Add(1) == 1 {
					return &LeaseInteractionsResult{
						Leases: []LeasedInteraction{{InteractionID: "int_cold", EnvironmentID: "env_cold", LeaseToken: "tok_v1"}},
					}, nil
				}
				return &LeaseInteractionsResult{
					Leases: []LeasedInteraction{{InteractionID: "int_cold", EnvironmentID: "env_cold", LeaseToken: "tok_v2_refreshed"}},
				}, nil
			},
		},
		Provisioner: &fakeWorkerProvisioner{
			provFn: func(WorkerProvisionRequest) (*WorkerProvisionResponse, error) {
				close(provEntered)
				<-provRelease
				return &WorkerProvisionResponse{SchemaVersion: WorkerProvisionSchemaVersionV1, URI: "worker://cold"}, nil
			},
		},
		DialWorker: func(string) (WorkerRPCClient, error) {
			return &fakeWorkerRPCClient{
				startFn: func(_ context.Context, req *StartWorkRequest, _ map[string]string) (*StartWorkResponse, error) {
					gotStartToken.Store(req.LeaseToken)
					return &StartWorkResponse{Status: WorkerStatusAccepted}, nil
				},
			}, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewTier2Poller(tpCold) failed: %v", err)
	}
	if _, err := tpCold.PollOnce(t.Context()); err != nil {
		t.Fatalf("tpCold.PollOnce(1) failed: %v", err)
	}
	<-provEntered
	if _, err := tpCold.PollOnce(t.Context()); err != nil {
		t.Fatalf("tpCold.PollOnce(2) failed: %v", err)
	}
	close(provRelease)
	tpCold.WaitHandoffs()
	if got, _ := gotStartToken.Load().(string); got != "tok_v2_refreshed" {
		t.Errorf("StartWork LeaseToken after cold-start refresh = %q, want %q", got, "tok_v2_refreshed")
	}

	// 4. Shared singleflight provisioning caller-cancellation isolation:
	// if waiter 1 cancels while waiter 2 is still waiting on the same envID, ProvisionWorker continues and waiter 2 succeeds;
	// if the sole waiter cancels, ProvisionWorker's context cancels promptly.
	sfProvStarted := make(chan struct{})
	sfProvRelease := make(chan struct{})
	var sfPollNum atomic.Int32
	var sfProvCtxErrOnRelease atomic.Value
	var sfAcceptedIDs []string
	var sfMu sync.Mutex
	tpSharedProv, err := NewTier2Poller(Tier2PollerConfig{
		Client: &fakeEnvClient{
			leaseFn: func(context.Context, *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
				switch sfPollNum.Add(1) {
				case 1:
					return &LeaseInteractionsResult{
						Leases: []LeasedInteraction{{InteractionID: "int_sf_1", EnvironmentID: "env_sf", LeaseToken: "t1"}},
					}, nil
				case 2:
					return &LeaseInteractionsResult{
						Leases: []LeasedInteraction{{InteractionID: "int_sf_2", EnvironmentID: "env_sf", LeaseToken: "t2"}},
					}, nil
				default:
					return &LeaseInteractionsResult{
						Leases: []LeasedInteraction{{InteractionID: "int_sf_sole", EnvironmentID: "env_sole", LeaseToken: "t3"}},
					}, nil
				}
			},
		},
		Provisioner: &fakeWorkerProvisioner{
			provCtxFn: func(ctx context.Context, req WorkerProvisionRequest) (*WorkerProvisionResponse, error) {
				if req.EnvironmentID == "env_sole" {
					close(sfProvStarted)
					<-ctx.Done()
					return nil, ctx.Err()
				}
				close(sfProvStarted)
				<-sfProvRelease
				if ctx.Err() != nil {
					sfProvCtxErrOnRelease.Store(ctx.Err())
					return nil, ctx.Err()
				}
				return &WorkerProvisionResponse{SchemaVersion: WorkerProvisionSchemaVersionV1, URI: "worker://sf"}, nil
			},
		},
		DialWorker: func(string) (WorkerRPCClient, error) {
			return &fakeWorkerRPCClient{
				startFn: func(_ context.Context, req *StartWorkRequest, _ map[string]string) (*StartWorkResponse, error) {
					sfMu.Lock()
					sfAcceptedIDs = append(sfAcceptedIDs, req.InteractionID)
					sfMu.Unlock()
					return &StartWorkResponse{Status: WorkerStatusAccepted}, nil
				},
			}, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewTier2Poller(tpSharedProv) failed: %v", err)
	}
	ctxW1, cancelW1 := context.WithCancel(t.Context())
	if _, err := tpSharedProv.PollOnce(ctxW1); err != nil {
		t.Fatalf("tpSharedProv.PollOnce(W1) failed: %v", err)
	}
	<-sfProvStarted
	ctxW2, cancelW2 := context.WithCancel(t.Context())
	defer cancelW2()
	if _, err := tpSharedProv.PollOnce(ctxW2); err != nil {
		t.Fatalf("tpSharedProv.PollOnce(W2) failed: %v", err)
	}
	for {
		tpSharedProv.mu.Lock()
		bothWaiting := tpSharedProv.provFlights["env_sf"] != nil && tpSharedProv.provFlights["env_sf"].waiters == 2
		tpSharedProv.mu.Unlock()
		if bothWaiting {
			break
		}
		runtime.Gosched()
	}
	cancelW1()
	for {
		tpSharedProv.mu.Lock()
		oneRemaining := tpSharedProv.provFlights["env_sf"] != nil && tpSharedProv.provFlights["env_sf"].waiters == 1
		tpSharedProv.mu.Unlock()
		if oneRemaining {
			break
		}
		runtime.Gosched()
	}
	close(sfProvRelease)
	tpSharedProv.WaitHandoffs()
	if gotErr := sfProvCtxErrOnRelease.Load(); gotErr != nil {
		t.Errorf("shared ProvisionWorker ctx canceled when waiter 1 canceled while waiter 2 remained: %v", gotErr)
	}
	sfMu.Lock()
	gotAccepted := slices.Clone(sfAcceptedIDs)
	sfMu.Unlock()
	if !slices.Equal(gotAccepted, []string{"int_sf_2"}) {
		t.Errorf("sfAcceptedIDs = %v, want [\"int_sf_2\"]", gotAccepted)
	}

	sfProvStarted = make(chan struct{})
	ctxSole, cancelSole := context.WithCancel(t.Context())
	if _, err := tpSharedProv.PollOnce(ctxSole); err != nil {
		t.Fatalf("tpSharedProv.PollOnce(sole) failed: %v", err)
	}
	<-sfProvStarted
	cancelSole()
	soleDone := make(chan struct{})
	go func() {
		tpSharedProv.WaitHandoffs()
		close(soleDone)
	}()
	select {
	case <-soleDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("sole waiter cancellation did not abort ProvisionWorker promptly")
	}
}
