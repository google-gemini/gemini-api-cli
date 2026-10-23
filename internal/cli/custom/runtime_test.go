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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google-gemini/gemini-api-cli/internal/clierrors"
	"github.com/google-gemini/gemini-api-cli/internal/config"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/spf13/cobra"
)

func newTestRuntimeCmd(t *testing.T, serverURL string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "gemini-api"}
	pf := root.PersistentFlags()
	pf.String("server-url", "", "")
	pf.String("api-version", "v1beta", "")
	pf.String("api-key", "", "")
	pf.String("access-token", "", "")
	pf.String("user-project", "", "")
	pf.String("api-revision", "", "")
	pf.StringArray("header", nil, "")
	pf.String("timeout", "", "")
	pf.Bool("debug", false, "")
	pf.Bool("dry-run", false, "")
	pf.Bool("no-keyring", true, "")
	sub := &cobra.Command{Use: "test"}
	root.AddCommand(sub)
	sub.SetContext(t.Context())
	if serverURL != "" {
		if err := pf.Set("server-url", serverURL); err != nil {
			t.Fatalf("Set(server-url=%q) failed: %v", serverURL, err)
		}
	}
	return sub
}

func startH2CTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:   handler,
		Protocols: &protos,
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return "http://" + ln.Addr().String()
}

func buildServerStepToolCallFrame(seq int64, callID, fnName string, args map[string]any) []byte {
	fnCall := appendStringField(nil, 1, fnName)
	fnCall = appendBytesField(fnCall, 2, encodeProtoStruct(args))
	toolCall := appendStringField(nil, 1, callID)
	toolCall = appendBytesField(toolCall, 3, fnCall)
	step := appendBytesField(nil, 4, toolCall)
	payload := appendBytesField(nil, 11, step)
	ev := appendBytesField(nil, 3, payload)
	ev = appendVarintField(ev, 10, uint64(seq))
	return appendBytesField(nil, 1, ev)
}

func buildServerReconnectRequiredFrame() []byte {
	return appendBytesField(nil, 9, nil)
}

func buildServerYieldFrame(nextInputSeq int64) []byte {
	return appendBytesField(nil, 10, appendVarintField(nil, 1, uint64(nextInputSeq)))
}

type parsedHandshake struct {
	ackSeq        int64
	interactionID string
	nextOutSeq    int64
	hasNextOutSeq bool
	background    bool
	clientID      string
}

func parseHandshakeFrame(t *testing.T, frame []byte) parsedHandshake {
	t.Helper()
	var hs parsedHandshake
	rest := frame
	for len(rest) > 0 {
		num, _, v, sub, n, err := consumeField(rest)
		if err != nil {
			t.Fatalf("consumeField(handshake) failed: %v", err)
		}
		switch num {
		case 2:
			hs.ackSeq = int64(v)
		case 8:
			inner := sub
			for len(inner) > 0 {
				inum, _, iv, ib, in, ierr := consumeField(inner)
				if ierr != nil {
					t.Fatalf("consumeField(ReconnectInteraction) failed: %v", ierr)
				}
				switch inum {
				case 1:
					hs.interactionID = string(ib)
				case 2:
					hs.nextOutSeq = int64(iv)
					hs.hasNextOutSeq = true
				case 3:
					hs.background = iv != 0
				case 4:
					hs.clientID = string(ib)
				}
				inner = inner[in:]
			}
		}
		rest = rest[n:]
	}
	return hs
}

func TestWireFrameBoundsAndErrors(t *testing.T) {
	var compressedHdr [5]byte
	compressedHdr[0] = 1

	var oversizedHdr [5]byte
	binary.BigEndian.PutUint32(oversizedHdr[1:5], maxGRPCFrameBytes+1)

	var truncatedHdr [5]byte
	binary.BigEndian.PutUint32(truncatedHdr[1:5], 16)

	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "compressed_frame_rejected", raw: compressedHdr[:]},
		{name: "oversized_frame_rejected", raw: oversizedHdr[:]},
		{name: "truncated_frame_rejected", raw: append(truncatedHdr[:], 1, 2, 3)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := grpcReadFrame(bytes.NewReader(tc.raw)); err == nil {
				t.Errorf("grpcReadFrame(%x) = %x, want non-nil error", tc.raw, got)
			}
		})
	}

	// Build a deeply nested google.protobuf.Struct exceeding maxProtoNestingDepth.
	var deepStruct []byte
	for i := 0; i < maxProtoNestingDepth+4; i++ {
		val := appendBytesField(nil, 5, deepStruct)
		entry := appendStringField(nil, 1, "k")
		entry = appendBytesField(entry, 2, val)
		deepStruct = appendBytesField(nil, 1, entry)
	}
	if got, err := decodeProtoStruct(deepStruct, 0); err == nil {
		t.Errorf("decodeProtoStruct(depth>%d) = %v, want depth limit error", maxProtoNestingDepth, got)
	}
}

func TestOutboundWireFieldNumbers(t *testing.T) {
	batch := &clientEventBatch{
		AckOutputSeqNum: 7,
		ReconnectInteraction: &reconnectInteractionEvent{
			InteractionID:    "int_123",
			NextOutputSeqNum: 8,
			ClientID:         "environment",
		},
		Disconnect: &disconnectEvent{},
		Events: []*clientEvent{{
			SeqNum:          1,
			TimestampMicros: 1700000000,
			Payload: &wirePayload{
				Step: &wireStep{
					ToolResult: &toolResultStep{
						CallID: "call_1",
						FunctionResult: &functionResultStep{
							Name:    "view_file",
							IsError: false,
							Result:  map[string]any{"content": "ok"},
						},
					},
				},
			},
		}},
	}
	var buf bytes.Buffer
	if err := grpcWriteFrame(&buf, batch.marshalWire()); err != nil {
		t.Fatalf("grpcWriteFrame() unexpected error: %v", err)
	}
	frame, err := grpcReadFrame(&buf)
	if err != nil {
		t.Fatalf("grpcReadFrame() unexpected error: %v", err)
	}

	seenFields := make(map[int]bool)
	rest := frame
	for len(rest) > 0 {
		num, _, _, _, n, err := consumeField(rest)
		if err != nil {
			t.Fatalf("consumeField() unexpected error: %v", err)
		}
		seenFields[num] = true
		rest = rest[n:]
	}
	for _, wantField := range []int{1, 2, 8, 9} {
		if !seenFields[wantField] {
			t.Errorf("clientEventBatch.marshalWire() missing protobuf tag %d in %v", wantField, seenFields)
		}
	}

	gotHS := parseHandshakeFrame(t, frame)
	wantHS := parsedHandshake{
		ackSeq:        7,
		interactionID: "int_123",
		nextOutSeq:    8,
		hasNextOutSeq: true,
		background:    false,
		clientID:      "environment",
	}
	if gotHS != wantHS {
		t.Errorf("parseHandshakeFrame(NextOutputSeqNum=8) = %+v, want %+v", gotHS, wantHS)
	}

	zeroSeqFrame := (&clientEventBatch{
		ReconnectInteraction: &reconnectInteractionEvent{
			InteractionID:    "int_zero",
			NextOutputSeqNum: 0,
			Background:       true,
			ClientID:         "environment",
		},
	}).marshalWire()
	gotZeroHS := parseHandshakeFrame(t, zeroSeqFrame)
	wantZeroHS := parsedHandshake{
		interactionID: "int_zero",
		hasNextOutSeq: false,
		background:    true,
		clientID:      "environment",
	}
	if gotZeroHS != wantZeroHS {
		t.Errorf("parseHandshakeFrame(NextOutputSeqNum=0) = %+v, want %+v", gotZeroHS, wantZeroHS)
	}
	if gotNil := (*clientEventBatch)(nil).marshalWire(); gotNil != nil {
		t.Errorf("(*clientEventBatch)(nil).marshalWire() = %x, want nil", gotNil)
	}
}

func TestSandboxConfinementWorkdirAndEnvAllowlist(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "sandbox")
	workDir := filepath.Join(rootDir, "project")
	outsideDir := filepath.Join(base, "outside")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(workDir) failed: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(outsideDir) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("WriteFile(secret.txt) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "shared.txt"), []byte("from-root"), 0o600); err != nil {
		t.Fatalf("WriteFile(shared.txt) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(main.go) failed: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "sym_out")); err != nil {
		t.Fatalf("Symlink(sym_out) failed: %v", err)
	}
	if err := os.Symlink(filepath.Join(outsideDir, "missing_target"), filepath.Join(workDir, "dangling_link")); err != nil {
		t.Fatalf("Symlink(dangling_link) failed: %v", err)
	}

	fsSrv := newFSServer(rootDir, workDir)

	// Relative paths resolve against workDir while allowing parent navigation up to rootDir.
	if got, err := fsSrv.ReadRegularFile("main.go"); err != nil || string(got) != "package main\n" {
		t.Errorf("ReadRegularFile(main.go) = (%q, %v), want %q", got, err, "package main\n")
	}
	if got, err := fsSrv.ReadRegularFile("../shared.txt"); err != nil || string(got) != "from-root" {
		t.Errorf("ReadRegularFile(../shared.txt) = (%q, %v), want %q", got, err, "from-root")
	}

	escapePaths := []struct {
		name string
		path string
	}{
		{name: "dotdot_escape_from_workdir", path: "../../outside/secret.txt"},
		{name: "symlink_existing_target", path: "sym_out/secret.txt"},
		{name: "symlink_nonexistent_leaf", path: "sym_out/new_subdir/pwn.txt"},
		{name: "dangling_symlink", path: "dangling_link"},
		{name: "nul_byte_path", path: "safe\x00evil.txt"},
	}
	for _, tc := range escapePaths {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := fsSrv.ResolvePath(tc.path); err == nil {
				t.Errorf("ResolvePath(%q) = %q, want confinement error", tc.path, got)
			}
		})
	}

	if _, _, err := resolveSandboxDirs(rootDir, outsideDir, false); err == nil {
		t.Errorf("resolveSandboxDirs(%q, %q) = nil error, want escape error", rootDir, outsideDir)
	}

	workerEnv := buildWorkerBashEnv([]string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"USER=user",
		"LANG=en_US.UTF-8",
		"LC_ALL=C.UTF-8",
		"TERM=xterm-256color",
		"TMPDIR=/tmp",
		"AWS_SECRET_ACCESS_KEY=aws_secret",
		"GITHUB_TOKEN=gh_secret",
		"GH_TOKEN=gh2_secret",
		"OPENAI_API_KEY=sk_secret",
		"ANTHROPIC_API_KEY=ant_secret",
		"GOOGLE_APPLICATION_CREDENTIALS=/creds.json",
		"GEMINI_API_KEY=gem_secret",
		"GEMINI_ENVIRONMENT_KEY=ek_secret",
		"LEASE_TOKEN=lease_secret",
		"UNLISTED_VAR=drop_me",
	})
	joinedWorkerEnv := strings.Join(workerEnv, "\n")
	for _, secret := range []string{"aws_secret", "gh_secret", "gh2_secret", "sk_secret", "ant_secret", "/creds.json", "gem_secret", "ek_secret", "lease_secret", "drop_me"} {
		if strings.Contains(joinedWorkerEnv, secret) {
			t.Errorf("buildWorkerBashEnv() leaked %q in %q", secret, joinedWorkerEnv)
		}
	}
	for _, keep := range []string{"PATH=/usr/bin", "HOME=/home/user", "USER=user", "LANG=en_US.UTF-8", "LC_ALL=C.UTF-8", "TERM=xterm-256color", "TMPDIR=/tmp"} {
		if !strings.Contains(joinedWorkerEnv, keep) {
			t.Errorf("buildWorkerBashEnv() dropped baseline var %q in %q", keep, joinedWorkerEnv)
		}
	}

	t.Setenv("GITHUB_TOKEN", "ghp_live_secret_token")
	shellSrv := newShellServer(workDir)
	stdout, _, exitCode, err := shellSrv.ExecuteBash(t.Context(), "env", "", 5*time.Second)
	if err != nil || exitCode != 0 {
		t.Fatalf("ExecuteBash(env) = (%d, %v), want exit 0", exitCode, err)
	}
	if strings.Contains(stdout, "ghp_live_secret_token") {
		t.Errorf("ExecuteBash(env) leaked GITHUB_TOKEN in stdout:\n%s", stdout)
	}
}

func TestWorkerAllSandboxToolsBoundsAndBashControls(t *testing.T) {
	rootDir := t.TempDir()
	w := newPullWorker(nil, newFSServer(rootDir, rootDir), newShellServer(rootDir), pullWorkerOptions{
		WorkerID:       "worker-tools",
		CommandTimeout: 400 * time.Millisecond,
		Logger:         log.New(io.Discard, "", 0),
	})

	if res, isErr := w.dispatchTool(t.Context(), "write_to_file", map[string]any{
		"TargetFile":  "notes/hello.txt",
		"CodeContent": "alpha\nbeta\ngamma\n",
	}); isErr {
		t.Fatalf("dispatchTool(write_to_file) error = %v, want success", res)
	}

	if res, isErr := w.dispatchTool(t.Context(), "replace_file_content", map[string]any{
		"TargetFile":         "notes/hello.txt",
		"TargetContent":      "beta",
		"ReplacementContent": "BETA",
	}); isErr {
		t.Fatalf("dispatchTool(replace_file_content) error = %v, want success", res)
	}

	viewRes, isErr := w.dispatchTool(t.Context(), "view_file", map[string]any{
		"AbsolutePath": "notes/hello.txt",
		"StartLine":    2,
		"EndLine":      2,
	})
	if isErr || viewRes["content"] != "BETA" {
		t.Errorf("dispatchTool(view_file) = (%v, %v), want content=\"BETA\"", viewRes, isErr)
	}

	// Oversized file (>4 MiB) rejects full view_file asking for start_line/end_line, while ranged view_file succeeds.
	bigPath := filepath.Join(rootDir, "huge.log")
	var bigBuilder strings.Builder
	bigBuilder.WriteString("header-line-1\nheader-line-2\n")
	chunk := strings.Repeat("x", 1024) + "\n"
	for bigBuilder.Len() <= maxReadFileBytes+4096 {
		bigBuilder.WriteString(chunk)
	}
	if err := os.WriteFile(bigPath, []byte(bigBuilder.String()), 0o600); err != nil {
		t.Fatalf("WriteFile(huge.log) failed: %v", err)
	}
	fullBigRes, fullBigErr := w.dispatchTool(t.Context(), "view_file", map[string]any{"path": "huge.log"})
	if !fullBigErr || !strings.Contains(fmt.Sprint(fullBigRes["error"]), "start_line") {
		t.Errorf("view_file(huge.log without line range) = (%v, %v), want error requesting start_line/end_line", fullBigRes, fullBigErr)
	}
	rangeBigRes, rangeBigErr := w.dispatchTool(t.Context(), "view_file", map[string]any{
		"path":       "huge.log",
		"start_line": 2,
		"end_line":   2,
	})
	if rangeBigErr || rangeBigRes["content"] != "header-line-2" {
		t.Errorf("view_file(huge.log lines 2..2) = (%v, %v), want \"header-line-2\"", rangeBigRes, rangeBigErr)
	}

	// Grep truncates at maxGrepMatches (1,000) and reports truncated: true.
	var manyLines strings.Builder
	for i := 0; i < maxGrepMatches+50; i++ {
		fmt.Fprintf(&manyLines, "match-token-%d\n", i)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "many.txt"), []byte(manyLines.String()), 0o600); err != nil {
		t.Fatalf("WriteFile(many.txt) failed: %v", err)
	}
	grepTruncRes, grepTruncErr := w.dispatchTool(t.Context(), "grep_search", map[string]any{"Query": "match-token-"})
	if grepTruncErr {
		t.Fatalf("grep_search(many.txt) unexpected error: %v", grepTruncRes)
	}
	if gotMatches := len(grepTruncRes["matches"].([]any)); gotMatches != maxGrepMatches {
		t.Errorf("grep_search match count = %d, want %d", gotMatches, maxGrepMatches)
	}
	if grepTruncRes["truncated"] != true {
		t.Errorf("grep_search truncated = %v, want true", grepTruncRes["truncated"])
	}

	listRes, isErr := w.dispatchTool(t.Context(), "list_dir", map[string]any{"DirectoryPath": "notes"})
	if isErr || len(listRes["entries"].([]any)) != 1 {
		t.Errorf("dispatchTool(list_dir) = (%v, %v), want 1 entry", listRes, isErr)
	}

	findRes, isErr := w.dispatchTool(t.Context(), "find_by_name", map[string]any{"Pattern": "hello.txt"})
	if isErr || len(findRes["files"].([]any)) != 1 {
		t.Errorf("dispatchTool(find_by_name) = (%v, %v), want 1 file", findRes, isErr)
	}

	// WaitMsBeforeAsync must NOT kill a command early, while timeout_seconds > CommandTimeout is clamped.
	bashRes, isErr := w.dispatchTool(t.Context(), "run_command", map[string]any{
		"CommandLine":       "sleep 0.12 && printf 'ok'",
		"WaitMsBeforeAsync": 20,
	})
	if isErr || bashRes["stdout"] != "ok" {
		t.Errorf("run_command with WaitMsBeforeAsync=20 = (%v, %v), want stdout=\"ok\"", bashRes, isErr)
	}

	start := time.Now()
	_, timedOutErr := w.dispatchTool(t.Context(), "execute_bash", map[string]any{
		"command":         "sleep 5",
		"timeout_seconds": 30,
	})
	if !timedOutErr || time.Since(start) > 3*time.Second {
		t.Errorf("execute_bash with timeout_seconds=30 took %v (isErr=%v), want clamped to CommandTimeout=400ms", time.Since(start), timedOutErr)
	}

	// DisableBash rejects execute_bash immediately without spawning a shell.
	disabledWorker := newPullWorker(nil, newFSServer(rootDir, rootDir), newShellServer(rootDir), pullWorkerOptions{
		WorkerID:    "worker-no-bash",
		DisableBash: true,
		Logger:      log.New(io.Discard, "", 0),
	})
	if res, disabledErr := disabledWorker.dispatchTool(t.Context(), "execute_bash", map[string]any{"command": "echo nope"}); !disabledErr {
		t.Errorf("dispatchTool(execute_bash) with DisableBash=true = (%v, false), want error", res)
	}
}

func TestBidiStreamDebugReconnectRetryAndAckOrdering(t *testing.T) {
	var connections atomic.Int32
	var inBandHeartbeats atomic.Int32
	var sawDisconnect atomic.Bool
	rootDir := t.TempDir()
	counterFile := filepath.Join(rootDir, "executions.txt")

	var mu sync.Mutex
	var handshakes []parsedHandshake

	srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/google.learning.gemini.api.interactions.v1beta.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey" {
			http.Error(w, "bad path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("x-goog-lease-token") != "tok_live_1" {
			http.Error(w, "missing lease token", http.StatusUnauthorized)
			return
		}
		connNum := connections.Add(1)

		// Connection 1 simulates a transient 503 during connectStream so executeLease must retry connectStream.
		if connNum == 1 {
			http.Error(w, `{"error":{"code":503,"message":"unavailable"}}`, http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()

		hsFrame, err := grpcReadFrame(r.Body)
		if err != nil {
			t.Errorf("conn %d handshake read failed: %v", connNum, err)
			return
		}
		hs := parseHandshakeFrame(t, hsFrame)
		mu.Lock()
		handshakes = append(handshakes, hs)
		mu.Unlock()

		// Connection 2: send a slow side-effecting tool call so in-band heartbeats fire BEFORE completion,
		// then abort the stream right before reading the response so Send/ack must retry cleanly!
		if connNum == 2 {
			_ = grpcWriteFrame(w, buildServerStepToolCallFrame(1, "call_once", "execute_bash", map[string]any{
				"command": "sleep 0.12 && printf 'x' >> " + counterFile + " && printf 'done'",
			}))
			flusher.Flush()

			for {
				frame, err := grpcReadFrame(r.Body)
				if err != nil {
					return
				}
				hasEvent := false
				rest := frame
				for len(rest) > 0 {
					num, _, _, _, n, err := consumeField(rest)
					if err != nil {
						return
					}
					if num == 1 {
						hasEvent = true
					}
					rest = rest[n:]
				}
				if !hasEvent {
					inBandHeartbeats.Add(1)
					continue
				}
				break
			}

			_ = grpcWriteFrame(w, buildServerReconnectRequiredFrame())
			flusher.Flush()
			return
		}

		// Connection 3: replay call_once (must hit executedCalls cache without re-running bash!) and send Yield.
		_ = grpcWriteFrame(w, buildServerStepToolCallFrame(2, "call_once", "execute_bash", map[string]any{
			"command": "printf 'y' >> " + counterFile,
		}))
		flusher.Flush()
		if _, err := grpcReadFrame(r.Body); err != nil {
			t.Errorf("conn 3 cached response read failed: %v", err)
			return
		}
		_ = grpcWriteFrame(w, buildServerYieldFrame(99))
		flusher.Flush()

		for {
			frame, err := grpcReadFrame(r.Body)
			if err != nil {
				break
			}
			rest := frame
			for len(rest) > 0 {
				num, _, _, _, n, err := consumeField(rest)
				if err != nil {
					break
				}
				if num == 9 {
					sawDisconnect.Store(true)
				}
				rest = rest[n:]
			}
			if sawDisconnect.Load() {
				break
			}
		}
	}))

	cmd := newTestRuntimeCmd(t, srvURL)
	var debugBuf bytes.Buffer
	cmd.SetErr(&debugBuf)
	// Enable --debug specifically to guard against DebugClient deadlocking on stream io.Pipe!
	_ = cmd.Root().PersistentFlags().Set("debug", "true")

	rt, err := newRuntimeTransport(cmd, true)
	if err != nil {
		t.Fatalf("newRuntimeTransport() failed: %v", err)
	}
	var workerLog bytes.Buffer
	client := NewHTTPClient(HTTPClientOptions{Transport: rt})
	worker := newPullWorker(client, newFSServer(rootDir, rootDir), newShellServer(rootDir), pullWorkerOptions{
		WorkerID:          "worker-test",
		HeartbeatInterval: 35 * time.Millisecond,
		CommandTimeout:    5 * time.Second,
		Logger:            log.New(&workerLog, "", 0),
	})

	if err := runStartupLeases(t.Context(), worker, []*LeasedInteraction{{
		InteractionID: "int_abc",
		EnvironmentID: "env_1",
		LeaseToken:    "tok_live_1",
	}}); err != nil {
		t.Fatalf("runStartupLeases() with --debug unexpected error: %v", err)
	}

	for _, wantLog := range []string{
		"[int_abc] Attached interaction session",
		"[int_abc] Executing tool execute_bash",
		"[int_abc] Completed interaction session",
	} {
		if !strings.Contains(workerLog.String(), wantLog) {
			t.Errorf("worker log missing %q:\n%s", wantLog, workerLog.String())
		}
	}

	if got := connections.Load(); got != 3 {
		t.Errorf("connections = %d, want 3 (1 transient 503 + 2 stream sessions)", got)
	}
	wantHandshakes := []parsedHandshake{
		{ackSeq: 0, interactionID: "int_abc", nextOutSeq: 1, hasNextOutSeq: true, clientID: "environment"},
		{ackSeq: 1, interactionID: "int_abc", nextOutSeq: 2, hasNextOutSeq: true, clientID: "environment"},
	}
	if !slices.Equal(handshakes, wantHandshakes) {
		t.Errorf("handshakes = %+v, want %+v", handshakes, wantHandshakes)
	}
	if got := inBandHeartbeats.Load(); got < 1 {
		t.Errorf("inBandHeartbeats = %d, want >= 1 during slow bash tool execution", got)
	}
	if !sawDisconnect.Load() {
		t.Errorf("sawDisconnect = false, want Disconnect frame sent on server Yield")
	}
	execBytes, err := os.ReadFile(counterFile)
	if err != nil || string(execBytes) != "x" {
		t.Errorf("side-effect execution file = (%q, %v), want exactly 1 execution (\"x\")", execBytes, err)
	}
	if strings.Contains(debugBuf.String(), "tok_live_1") {
		t.Errorf("--debug stderr leaked raw x-goog-lease-token:\n%s", debugBuf.String())
	}
}

func TestStreamAckNotAdvancedWhenSendFails(t *testing.T) {
	// Directly test consumeStream + sessionState when Send fails because the pipe reader closed mid-call.
	rootDir := t.TempDir()
	counterFile := filepath.Join(rootDir, "once.txt")
	w := newPullWorker(nil, newFSServer(rootDir, rootDir), newShellServer(rootDir), pullWorkerOptions{
		WorkerID:          "worker-ack-test",
		HeartbeatInterval: time.Hour,
		CommandTimeout:    5 * time.Second,
		Logger:            log.New(io.Discard, "", 0),
	})

	// Build a single server frame carrying seq=1 tool call followed by immediate EOF on server body,
	// while the client->server pipe reader is closed so stream.Send returns an error!
	var serverBuf bytes.Buffer
	_ = grpcWriteFrame(&serverBuf, buildServerStepToolCallFrame(1, "call_send_fail", "execute_bash", map[string]any{
		"command": "printf '1' >> " + counterFile,
	}))
	pr, pw := io.Pipe()
	_ = pr.Close() // Force stream.Send to fail with io.ErrClosedPipe!

	stream := &grpcInteractionStream{
		resp: &http.Response{Body: io.NopCloser(&serverBuf)},
		pw:   pw,
	}
	state := &sessionState{
		nextClientSeq: 1,
		executedCalls: make(map[string]*wirePayload),
	}

	_, err := w.consumeStream(t.Context(), stream, state)
	if err == nil {
		t.Fatalf("consumeStream() = nil error when Send failed, want non-nil error")
	}
	if gotAck := state.ackSeq(); gotAck != 0 {
		t.Errorf("state.ackSeq() after failed Send = %d, want 0 so reconnect requests seq 1 again", gotAck)
	}
	if state.nextClientSeq != 1 {
		t.Errorf("state.nextClientSeq after failed Send = %d, want rolled back to 1", state.nextClientSeq)
	}
	if len(state.executedCalls) != 1 {
		t.Errorf("len(state.executedCalls) = %d, want 1 cached result ready for replay", len(state.executedCalls))
	}
}

func TestPollerControllerHandoffHeartbeatAndRedaction(t *testing.T) {
	var heartbeats atomic.Int32
	var leasedOnce atomic.Bool
	var mu sync.Mutex
	var capturedStdin ProvisionInput

	srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req leaseInteractionsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if len(req.ActiveLeases) > 0 {
			heartbeats.Add(1)
			_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{
				Leases: []*LeasedInteraction{{
					InteractionID: req.ActiveLeases[0].InteractionID,
					LeaseToken:    req.ActiveLeases[0].LeaseToken,
				}},
			})
			return
		}
		if !leasedOnce.Swap(true) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"leases": [{
					"interaction_id": "int_handoff",
					"environment_id": "env_alpha",
					"lease_token": "secret_lease_tok_999",
					"lease_generation": "2",
					"lease_expiration_time": "2026-10-22T10:00:00Z",
					"environment_config": {
						"network": {
							"allowlist": [{"domain": "api.example.com", "credential": "cred_net"}]
						},
						"sources": [
							{"type": "GCS", "source": "gs://bucket/prefix", "target": "/workspace/data"}
						],
						"env": {
							"PLAIN_KEY": "plain_val",
							"OBJ_KEY": {"value": "obj_val"},
							"SECRET_KEY": {"credential": "cred_secret"}
						},
						"type": "self_hosted"
					}
				}],
				"poll_backoff": "5s"
			}`))
			return
		}
		_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{PollBackoff: "5s"})
	}))

	outFile := filepath.Join(t.TempDir(), "provision.json")
	cmd := newTestRuntimeCmd(t, srvURL)
	var debugBuf bytes.Buffer
	cmd.SetErr(&debugBuf)
	_ = cmd.Root().PersistentFlags().Set("debug", "true")

	rt, err := newRuntimeTransport(cmd, true)
	if err != nil {
		t.Fatalf("newRuntimeTransport() failed: %v", err)
	}
	client := NewHTTPClient(HTTPClientOptions{
		Transport:      rt,
		EnvironmentKey: "ek_test_secret",
	})

	ctx, cancel := context.WithCancel(t.Context())
	poller := NewPollerController(client, NewCommandWorkerProvisioner("sleep 0.12 && cat > "+outFile), PollerOptions{
		PollerID:                 "poller-1",
		MaxInteractions:          2,
		PollInterval:             30 * time.Millisecond,
		ProvisionTimeout:         5 * time.Second,
		HandoffHeartbeatInterval: 35 * time.Millisecond,
		Logger:                   log.New(io.Discard, "", 0),
	})

	done := make(chan error, 1)
	go func() {
		done <- poller.Run(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(outFile); err == nil && len(raw) > 0 {
			mu.Lock()
			_ = json.Unmarshal(raw, &capturedStdin)
			mu.Unlock()
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	cancel()
	<-done

	if capturedStdin.InteractionID != "int_handoff" || capturedStdin.EnvironmentID != "env_alpha" || capturedStdin.SchemaVersion != "v1" {
		t.Errorf("ProvisionInput = %+v, want v1 int_handoff / env_alpha", capturedStdin)
	}
	var forwardedCfg map[string]any
	if err := json.Unmarshal(capturedStdin.EnvironmentConfig, &forwardedCfg); err != nil {
		t.Fatalf("json.Unmarshal(ProvisionInput.EnvironmentConfig) failed: %v", err)
	}
	envMap, _ := forwardedCfg["env"].(map[string]any)
	if envMap["PLAIN_KEY"] != "plain_val" {
		t.Errorf("forwarded env.PLAIN_KEY = %v, want %q", envMap["PLAIN_KEY"], "plain_val")
	}
	if objKey, _ := envMap["OBJ_KEY"].(map[string]any); objKey["value"] != "obj_val" {
		t.Errorf("forwarded env.OBJ_KEY = %v, want {\"value\":\"obj_val\"}", envMap["OBJ_KEY"])
	}
	if netObj, _ := forwardedCfg["network"].(map[string]any); netObj["allowlist"] == nil {
		t.Errorf("forwarded network = %v, want allowlist preserved", forwardedCfg["network"])
	}
	if got := heartbeats.Load(); got < 1 {
		t.Errorf("handoff heartbeats = %d, want >= 1 during slow provision command", got)
	}
	if strings.Contains(debugBuf.String(), "secret_lease_tok_999") {
		t.Errorf("--debug stderr leaked raw lease_token:\n%s", debugBuf.String())
	}
}

func TestLeaseInteractionsWireParsing(t *testing.T) {
	tests := []struct {
		name           string
		envID          string
		respBody       string
		wantGeneration int64
		wantSubstr     string
	}{
		{
			name:  "gaos_allowlist_sources_and_envvar_objects",
			envID: "env_scoped",
			respBody: `{
				"leases": [{
					"interaction_id": "int_1",
					"environment_id": "env_scoped",
					"lease_token": "tok_1",
					"lease_generation": "7",
					"lease_expiration_time": "2026-10-22T12:00:00Z",
					"environment_config": {
						"network": {
							"allowlist": [
								{"domain": "example.com", "credential": "cred_a"},
								{"domain": "*.googleapis.com"}
							]
						},
						"sources": [
							{"type": "GCS", "source": "gs://my-bucket/src", "target": "/workspace/src"}
						],
						"env": {
							"PLAIN": "hello",
							"CRED": {"credential": "cred_env"}
						}
					}
				}],
				"poll_backoff": "5s"
			}`,
			wantGeneration: 7,
			wantSubstr:     `"CRED": {"credential": "cred_env"}`,
		},
		{
			name:  "gaos_disabled_network_and_numeric_generation",
			envID: "env_disabled",
			respBody: `{
				"leases": [{
					"interaction_id": "int_2",
					"environment_id": "env_disabled",
					"lease_token": "tok_2",
					"lease_generation": 3,
					"environment_config": {
						"network": "disabled"
					}
				}]
			}`,
			wantGeneration: 3,
			wantSubstr:     `"network": "disabled"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotRawReq map[string]any
			srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotRawReq)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.respBody))
			}))

			cmd := newTestRuntimeCmd(t, srvURL)
			rt, err := newRuntimeTransport(cmd, true)
			if err != nil {
				t.Fatalf("newRuntimeTransport() failed: %v", err)
			}
			client := NewHTTPClient(HTTPClientOptions{
				Transport:      rt,
				EnvironmentID:  tc.envID,
				EnvironmentKey: "ek_live_1",
			})

			leases, err := client.LeaseInteractions(t.Context(), "worker-1", 4)
			if err != nil {
				t.Fatalf("LeaseInteractions() unexpected error: %v", err)
			}
			if gotRawReq["environment_id"] != tc.envID {
				t.Errorf("request environment_id = %v, want %q", gotRawReq["environment_id"], tc.envID)
			}
			if len(leases) != 1 {
				t.Fatalf("len(leases) = %d, want 1", len(leases))
			}
			got := leases[0]
			if got.LeaseGeneration != tc.wantGeneration {
				t.Errorf("LeaseGeneration = %d, want %d", got.LeaseGeneration, tc.wantGeneration)
			}
			if !strings.Contains(string(got.EnvironmentConfig), tc.wantSubstr) {
				t.Errorf("EnvironmentConfig = %s, want substring %q", got.EnvironmentConfig, tc.wantSubstr)
			}
		})
	}
}

func TestHeartbeatInteractionRequestShape(t *testing.T) {
	var rawReq map[string]any
	srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&rawReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"leases":[{"interaction_id":"int_hb","lease_token":"tok_hb"}]}`))
	}))

	cmd := newTestRuntimeCmd(t, srvURL)
	rt, err := newRuntimeTransport(cmd, true)
	if err != nil {
		t.Fatalf("newRuntimeTransport() failed: %v", err)
	}
	client := NewHTTPClient(HTTPClientOptions{
		Transport:      rt,
		EnvironmentKey: "ek_live_hb",
	})

	res, err := client.HeartbeatInteraction(t.Context(), "poller-hb", "int_hb", "env_hb", "tok_hb")
	if err != nil {
		t.Fatalf("HeartbeatInteraction() unexpected error: %v", err)
	}
	if !res.Renewed || res.ShouldCancel {
		t.Errorf("HeartbeatInteraction() = %+v, want Renewed=true ShouldCancel=false", res)
	}
	if _, badField := rawReq["heartbeat_leases"]; badField {
		t.Errorf("request sent obsolete field heartbeat_leases: %v", rawReq)
	}
	if _, ok := rawReq["active_leases"]; !ok {
		t.Errorf("request missing active_leases: %v", rawReq)
	}
	maxNew, hasMaxNew := rawReq["max_new_interactions"]
	if !hasMaxNew || maxNew != float64(0) {
		t.Errorf("request max_new_interactions = (%v, %v), want (0, true)", maxNew, hasMaxNew)
	}
	if rawReq["environment_id"] != "env_hb" {
		t.Errorf("request environment_id = %v, want %q", rawReq["environment_id"], "env_hb")
	}
}

func TestWindowsSandboxFixes(t *testing.T) {
	rootDir := t.TempDir()
	fs := newFSServer(rootDir, rootDir)
	worker := &pullWorker{fs: fs}

	// 1. Write a CRLF file and verify ReadFileRange, Grep ($ anchor), and replace_file_content (LF target -> CRLF preserved).
	crlfContent := "[server]\r\nhost = 127.0.0.1\r\nport = 8080\r\nmode = debug\r\n"
	if _, err := fs.WriteRegularFile("src/settings.ini", crlfContent, true, false); err != nil {
		t.Fatalf("WriteRegularFile(src/settings.ini) failed: %v", err)
	}
	if _, err := fs.WriteRegularFile("logs/app.log", "2026-10-22 ERROR bind failed\r\n", true, false); err != nil {
		t.Fatalf("WriteRegularFile(logs/app.log) failed: %v", err)
	}
	if _, err := fs.WriteRegularFile("README.md", "# Service\r\n", true, false); err != nil {
		t.Fatalf("WriteRegularFile(README.md) failed: %v", err)
	}

	ranged, err := fs.ReadFileRange("src/settings.ini", 2, 3)
	if err != nil {
		t.Fatalf("ReadFileRange() failed: %v", err)
	}
	if ranged != "host = 127.0.0.1\nport = 8080" {
		t.Errorf("ReadFileRange() = %q, want %q", ranged, "host = 127.0.0.1\nport = 8080")
	}

	matches, _, err := fs.Grep(".", "port = 8080$", false, true)
	if err != nil {
		t.Fatalf("Grep() failed: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("Grep('port = 8080$') len(matches) = %d, want 1", len(matches))
	}

	res, isErr := worker.toolReplaceFileContent(map[string]any{
		"TargetFile":         "src/settings.ini",
		"TargetContent":      "host = 127.0.0.1\nport = 8080",
		"ReplacementContent": "host = 127.0.0.1\nport = 9090",
		"StartLine":          2,
		"EndLine":            3,
	})
	if isErr {
		t.Fatalf("toolReplaceFileContent() failed: %v", res)
	}
	updatedBytes, err := fs.ReadRegularFile("src/settings.ini")
	if err != nil {
		t.Fatalf("ReadRegularFile() failed: %v", err)
	}
	wantCRLF := "[server]\r\nhost = 127.0.0.1\r\nport = 9090\r\nmode = debug\r\n"
	if string(updatedBytes) != wantCRLF {
		t.Errorf("updated CRLF file = %q, want %q", string(updatedBytes), wantCRLF)
	}

	// 2. Verify find_by_name Extensions filter excludes directories and non-matching extensions.
	findRes, findErr := worker.toolFindByName(map[string]any{
		"SearchDirectory": ".",
		"Pattern":         "*",
		"Extensions":      []any{"ini", ".LOG"},
	})
	if findErr {
		t.Fatalf("toolFindByName() failed: %v", findRes)
	}
	gotFiles, _ := findRes["files"].([]any)
	if len(gotFiles) != 2 || gotFiles[0] != "logs/app.log" || gotFiles[1] != "src/settings.ini" {
		t.Errorf("toolFindByName(Extensions=[ini, .LOG]) = %v, want [logs/app.log src/settings.ini]", gotFiles)
	}

	// 3. Verify Windows environment allowlist preserves COMPUTERNAME, USERNAME, OS, SYSTEMDRIVE, PSMODULEPATH.
	filteredEnv := buildWorkerBashEnv([]string{
		"COMPUTERNAME=WIN-BYOS",
		"USERNAME=svc_user",
		"OS=Windows_NT",
		"SYSTEMDRIVE=C:",
		"PSMODULEPATH=C:\\Program Files\\WindowsPowerShell\\Modules",
		"GEMINI_API_KEY=secret",
		"UNLISTED_VAR=drop_me",
	})
	if len(filteredEnv) != 5 {
		t.Errorf("buildWorkerBashEnv() = %v, want 5 allowed Windows env vars", filteredEnv)
	}
}

func newRuntimeCLIRootForTest(t *testing.T, serverURL string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := &cobra.Command{Use: "gemini-api", SilenceErrors: true, SilenceUsage: true}
	pf := root.PersistentFlags()
	pf.StringP("output-format", "o", "pretty", "")
	pf.String("color", "never", "")
	pf.StringP("jq", "q", "", "")
	pf.Bool("raw-output", true, "")
	pf.String("server-url", serverURL, "")
	pf.StringArrayP("header", "H", nil, "")
	pf.Bool("include-headers", false, "")
	pf.String("timeout", "", "")
	pf.Bool("interactive", false, "")
	pf.Bool("no-interactive", true, "")
	pf.Bool("usage", false, "")
	pf.Bool("dry-run", false, "")
	pf.BoolP("debug", "d", false, "")
	pf.Bool("agent-mode", false, "")
	pf.Bool("no-retries", false, "")
	pf.Bool("no-keyring", true, "")
	pf.String("api-key", "", "")
	pf.String("access-token", "", "")
	pf.String("api-version", "v1beta", "")
	pf.String("api-revision", "", "")
	pf.String("user-project", "", "")

	envs := &cobra.Command{Use: "environments"}
	envs.AddCommand(newEnvironmentsConnectCmd(), newEnvironmentsPollCmd(), newEnvironmentsServeCmd())
	root.AddCommand(envs)
	root.SetContext(t.Context())

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	return root, &stdout, &stderr
}

func TestRegisterMountsRuntimeCommandsAsDynamic(t *testing.T) {
	noopRunE := func(*cobra.Command, []string) error { return nil }
	buildSkeleton := func() (*cobra.Command, *cobra.Command) {
		root := &cobra.Command{Use: "gemini-api"}
		for _, customName := range []string{"tts", "analyze", "transcribe"} {
			root.AddCommand(&cobra.Command{Use: customName})
		}

		filesGroup := &cobra.Command{Use: "files"}
		for _, leaf := range []string{"get", "delete"} {
			c := &cobra.Command{Use: leaf, RunE: noopRunE}
			c.Flags().String("file", "", "")
			filesGroup.AddCommand(c)
		}
		root.AddCommand(filesGroup)

		modelsGroup := &cobra.Command{Use: "models"}
		modelsGet := &cobra.Command{Use: "get", RunE: noopRunE}
		modelsGet.Flags().String("model", "", "")
		modelsGroup.AddCommand(modelsGet)
		root.AddCommand(modelsGroup)

		envsGroup := &cobra.Command{Use: "environments"}
		envFilesGroup := &cobra.Command{Use: "files"}
		envFilesList := &cobra.Command{Use: "list", RunE: noopRunE}
		envFilesList.Flags().String("environment", "", "")
		envFilesList.Flags().String("path", "", "")
		envFilesGroup.AddCommand(envFilesList)
		envsGroup.AddCommand(envFilesGroup)
		root.AddCommand(envsGroup)

		for _, intent := range []string{"generate", "image", "video", "music"} {
			c := &cobra.Command{Use: intent, RunE: noopRunE}
			c.Flags().String("body", "", "")
			root.AddCommand(c)
		}
		agentGroup := &cobra.Command{Use: "agent"}
		agentRun := &cobra.Command{Use: "run", RunE: noopRunE}
		agentRun.Flags().String("body", "", "")
		agentGroup.AddCommand(agentRun)
		root.AddCommand(agentGroup)
		return root, envsGroup
	}

	root, envsGroup := buildSkeleton()
	Register(root)

	for _, subName := range []string{"connect", "poll", "serve"} {
		sub := findChild(envsGroup, subName)
		if sub == nil {
			t.Fatalf("findChild(environments, %q) = nil, want registered subcommand", subName)
		}
		if got := sub.Annotations["speakeasy_usage_dynamic"]; got != "true" {
			t.Errorf("%s Annotations[speakeasy_usage_dynamic] = %q, want \"true\"", subName, got)
		}
	}

	// Pre-existing subcommands under environments must not be overwritten or duplicated.
	root2, envsGroup2 := buildSkeleton()
	preExisting := &cobra.Command{Use: "connect", Short: "pre-existing"}
	envsGroup2.AddCommand(preExisting)
	Register(root2)
	if got := findChild(envsGroup2, "connect"); got != preExisting {
		t.Errorf("findChild(environments, \"connect\") = %p, want pre-existing %p", got, preExisting)
	}
	connectCount := 0
	for _, c := range envsGroup2.Commands() {
		if c.Name() == "connect" {
			connectCount++
		}
	}
	if connectCount != 1 {
		t.Errorf("environments connect command count = %d, want 1", connectCount)
	}
	if findChild(envsGroup2, "poll") == nil || findChild(envsGroup2, "serve") == nil {
		t.Errorf("Register() did not mount remaining runtime subcommands")
	}

	t.Run("missing_environments_group_panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("Register(incomplete root) did not panic, want panic")
			}
		}()
		incompleteRoot, missingEnvs := buildSkeleton()
		incompleteRoot.RemoveCommand(missingEnvs)
		Register(incompleteRoot)
	})
}

func TestEnvironmentsRuntimeCommandsFlagsDryRunAndExecution(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GEMINI_ACCESS_TOKEN", "")
	t.Setenv("GEMINI_ENVIRONMENT_KEY", "")
	t.Setenv("GEMINI_ENVIRONMENT_ID", "")
	t.Setenv("GEMINI_WORKER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	t.Run("flag_defaults_and_usage_kdl", func(t *testing.T) {
		connectCmd := newEnvironmentsConnectCmd()
		if got := connectCmd.Flags().Lookup("command-timeout").DefValue; got != "2m" {
			t.Errorf("connect --command-timeout DefValue = %q, want \"2m\"", got)
		}
		if connectCmd.Flags().Lookup("worker-id") == nil {
			t.Errorf("connect missing --worker-id flag")
		}
		if connectCmd.Flags().Lookup("disable-bash") == nil {
			t.Errorf("connect missing --disable-bash flag")
		}
		pollCmd := newEnvironmentsPollCmd()
		if got := pollCmd.Flags().Lookup("provision-timeout").DefValue; got != "2m" {
			t.Errorf("poll --provision-timeout DefValue = %q, want \"2m\"", got)
		}

		for _, sub := range []string{"connect", "poll", "serve"} {
			root, stdout, _ := newRuntimeCLIRootForTest(t, "")
			root.SetArgs([]string{"environments", sub, "--usage", "ignored-positional"})
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(environments %s --usage) unexpected error: %v", sub, err)
			}
			if !strings.Contains(stdout.String(), `cmd "`+sub+`"`) {
				t.Errorf("environments %s --usage output missing cmd header:\n%s", sub, stdout.String())
			}
		}
	})

	t.Run("validation_errors", func(t *testing.T) {
		sandboxDir := t.TempDir()
		outsideDir := t.TempDir()
		cases := []struct {
			name    string
			args    []string
			wantSub string
		}{
			{
				name:    "connect_positional_arg",
				args:    []string{"environments", "connect", "extra"},
				wantSub: "accepts 0 arguments, received 1",
			},
			{
				name:    "connect_workdir_escapes_root",
				args:    []string{"environments", "connect", "--root-dir", sandboxDir, "--workdir", outsideDir},
				wantSub: "escapes root-dir",
			},
			{
				name:    "connect_zero_heartbeat",
				args:    []string{"environments", "connect", "--heartbeat-interval", "0s"},
				wantSub: "--heartbeat-interval must be > 0",
			},
			{
				name:    "connect_zero_command_timeout",
				args:    []string{"environments", "connect", "--command-timeout", "0s"},
				wantSub: "--command-timeout must be > 0",
			},
			{
				name:    "connect_control_char_env_key",
				args:    []string{"environments", "connect", "--environment-key", "bad\x00key"},
				wantSub: "--environment-key contains control characters",
			},
			{
				name:    "connect_zero_max_concurrent",
				args:    []string{"environments", "connect", "--max-concurrent", "0"},
				wantSub: "--max-concurrent must be >= 1",
			},
			{
				name:    "connect_zero_poll_interval",
				args:    []string{"environments", "connect", "--poll-interval", "0s"},
				wantSub: "--poll-interval must be > 0",
			},
			{
				name:    "connect_missing_credentials",
				args:    []string{"environments", "connect"},
				wantSub: "missing credentials",
			},
			{
				name:    "poll_zero_max_interactions",
				args:    []string{"environments", "poll", "--max-interactions", "0"},
				wantSub: "--max-interactions must be >= 1",
			},
			{
				name:    "poll_zero_poll_interval",
				args:    []string{"environments", "poll", "--poll-interval", "0s"},
				wantSub: "--poll-interval must be > 0",
			},
			{
				name:    "poll_zero_provision_timeout",
				args:    []string{"environments", "poll", "--provision-timeout", "0s"},
				wantSub: "--provision-timeout must be > 0",
			},
			{
				name:    "poll_zero_handoff_heartbeat",
				args:    []string{"environments", "poll", "--handoff-heartbeat-interval", "0s"},
				wantSub: "--handoff-heartbeat-interval must be > 0",
			},
			{
				name:    "poll_control_char_env_key",
				args:    []string{"environments", "poll", "--environment-key", "bad\nkey"},
				wantSub: "--environment-key contains control characters",
			},
			{
				name:    "poll_invalid_environment_id",
				args:    []string{"environments", "poll", "--environment", "environments/a/b"},
				wantSub: `invalid environment id "environments/a/b"`,
			},
			{
				name:    "poll_missing_provision_cmd",
				args:    []string{"environments", "poll", "--environment-key", "ek_1"},
				wantSub: "--provision-cmd is required",
			},
			{
				name:    "poll_missing_credentials",
				args:    []string{"environments", "poll", "--provision-cmd", "true"},
				wantSub: "missing credentials",
			},
			{
				name:    "serve_only_interaction_id",
				args:    []string{"environments", "serve", "--interaction-id", "int_1"},
				wantSub: "--interaction-id and --lease-token must be provided together",
			},
			{
				name:    "serve_invalid_interaction_id",
				args:    []string{"environments", "serve", "--interaction-id", "interactions/a/b", "--lease-token", "tok_1"},
				wantSub: "invalid interaction id",
			},
			{
				name:    "serve_control_char_lease_token",
				args:    []string{"environments", "serve", "--interaction-id", "int_1", "--lease-token", "tok\r1"},
				wantSub: "--lease-token contains control characters",
			},
			{
				name:    "serve_missing_lease",
				args:    []string{"environments", "serve"},
				wantSub: "missing lease",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				root, _, _ := newRuntimeCLIRootForTest(t, "http://127.0.0.1:1")
				root.SetArgs(tc.args)
				rawErr := root.Execute()
				if rawErr == nil {
					t.Fatalf("Execute(%v) = nil, want error containing %q", tc.args, tc.wantSub)
				}
				err := output.CLIError(root, rawErr)
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("Execute(%v) error = %q, want substring %q", tc.args, err.Error(), tc.wantSub)
				}
				if got := clierrors.ExitCode(err); got != clierrors.ExitUsage {
					t.Errorf("ExitCode(%v) = %d, want %d (ExitUsage)", tc.args, got, clierrors.ExitUsage)
				}
			})
		}
	})

	t.Run("dry_run_previews", func(t *testing.T) {
		dryCases := []struct {
			name     string
			args     []string
			wantPath string
			wantBody string
		}{
			{
				name:     "connect_dry_run",
				args:     []string{"-o", "json", "--dry-run", "environments", "connect", "--environment-key", "ek_c", "--worker-id", "w-custom", "--max-concurrent", "2"},
				wantPath: "/v1beta/environments:leaseInteractions",
				wantBody: "w-custom",
			},
			{
				name:     "poll_dry_run_with_scoped_environment",
				args:     []string{"-o", "json", "--dry-run", "environments", "poll", "--environment", "environments/env_77", "--environment-key", "ek_p", "--poller-id", "p-custom", "--max-interactions", "3"},
				wantPath: "/v1beta/environments:leaseInteractions",
				wantBody: "env_77",
			},
			{
				name:     "serve_dry_run_default_sample",
				args:     []string{"-o", "json", "--dry-run", "environments", "serve"},
				wantPath: "/google.learning.gemini.api.interactions.v1beta.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey",
			},
			{
				name:     "serve_dry_run_explicit_lease",
				args:     []string{"-o", "json", "--dry-run", "environments", "serve", "--interaction-id", "interactions/int_s1", "--lease-token", "tok_s1"},
				wantPath: "/google.learning.gemini.api.interactions.v1beta.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey",
			},
		}
		for _, tc := range dryCases {
			t.Run(tc.name, func(t *testing.T) {
				root, stdout, _ := newRuntimeCLIRootForTest(t, "http://127.0.0.1:1")
				output.PreparseRenderingFlags(root, []string{"-o", "json"})
				root.SetArgs(tc.args)
				if err := root.Execute(); err != nil {
					t.Fatalf("Execute(%v) unexpected error: %v", tc.args, err)
				}
				var preview map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil {
					t.Fatalf("json.Unmarshal(dry-run output %q) failed: %v", stdout.String(), err)
				}
				reqObj, _ := preview["request"].(map[string]any)
				if gotURL, _ := reqObj["url"].(string); !strings.HasSuffix(gotURL, tc.wantPath) {
					t.Errorf("dry-run url = %q, want suffix %q", gotURL, tc.wantPath)
				}
				if tc.wantBody != "" && !strings.Contains(stdout.String(), tc.wantBody) {
					t.Errorf("dry-run output = %s, want substring %q", stdout.String(), tc.wantBody)
				}
			})
		}
	})

	t.Run("live_connect_poll_and_serve_execution", func(t *testing.T) {
		var connectWorkerID atomic.Value
		var pollEnvID atomic.Value
		var serveCompleted atomic.Bool
		connectDone := make(chan struct{}, 1)
		provisionFile := filepath.Join(t.TempDir(), "polled.json")

		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1beta/environments:leaseInteractions":
				var req leaseInteractionsRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				if strings.HasPrefix(req.WorkerID, "poller") {
					pollEnvID.Store(req.EnvironmentID)
					if len(req.ActiveLeases) == 0 && pollEnvID.Load() == "env_live_poll" {
						_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{
							Leases: []*LeasedInteraction{{
								InteractionID: "int_polled",
								EnvironmentID: "env_live_poll",
								LeaseToken:    "tok_polled",
							}},
						})
						return
					}
					_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{})
					return
				}
				if prev := connectWorkerID.Swap(req.WorkerID); prev == nil {
					_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{
						Leases: []*LeasedInteraction{{
							InteractionID: "int_connected",
							EnvironmentID: "env_conn",
							LeaseToken:    "tok_conn",
						}},
					})
					return
				}
				_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{})

			case "/google.learning.gemini.api.interactions.v1beta.InteractionsService/CreateInteractionBidiStreamByEnvironmentKey":
				w.Header().Set("Content-Type", "application/grpc")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				flusher.Flush()
				hsFrame, err := grpcReadFrame(r.Body)
				if err != nil {
					return
				}
				hs := parseHandshakeFrame(t, hsFrame)
				if hs.interactionID == "int_serve_bash_off" {
					// Verify --disable-bash rejects execute_bash over the live serve command!
					_ = grpcWriteFrame(w, buildServerStepToolCallFrame(1, "c_bash", "execute_bash", map[string]any{"command": "echo nope"}))
					flusher.Flush()
					if _, err := grpcReadFrame(r.Body); err == nil {
						serveCompleted.Store(true)
					}
				}
				// Terminate session with InteractionCompleted (field 6).
				_ = grpcWriteFrame(w, appendBytesField(nil, 6, nil))
				flusher.Flush()
				if hs.interactionID == "int_connected" {
					select {
					case connectDone <- struct{}{}:
					default:
					}
				}
			default:
				http.NotFound(w, r)
			}
		}))

		// 1. Live `environments serve --disable-bash`
		serveWork := filepath.Join(t.TempDir(), "serve-work")
		serveRoot, _, _ := newRuntimeCLIRootForTest(t, srvURL)
		serveRoot.SetArgs([]string{
			"--no-retries",
			"environments", "serve",
			"--interaction-id", "interactions/int_serve_bash_off",
			"--lease-token", "tok_serve",
			"--root-dir", serveWork,
			"--workdir", serveWork,
			"--disable-bash",
			"--quiet",
		})
		if err := serveRoot.Execute(); err != nil {
			t.Fatalf("Execute(environments serve) unexpected error: %v", err)
		}
		if !serveCompleted.Load() {
			t.Errorf("serveCompleted = false, want tool error frame sent before InteractionCompleted")
		}

		// 2. Live `environments connect`
		connCtx, cancelConn := context.WithCancel(t.Context())
		connDir := filepath.Join(t.TempDir(), "conn-work")
		connRoot, _, _ := newRuntimeCLIRootForTest(t, srvURL)
		connRoot.SetContext(connCtx)
		connRoot.SetArgs([]string{
			"environments", "connect",
			"--environment-key", "ek_conn",
			"--worker-id", "worker-cli-1",
			"--root-dir", connDir,
			"--poll-interval", "20ms",
			"--quiet",
		})
		connErrCh := make(chan error, 1)
		go func() {
			connErrCh <- connRoot.Execute()
		}()
		select {
		case <-connectDone:
		case <-time.After(3 * time.Second):
			t.Errorf("timed out waiting for environments connect to complete leased session")
		}
		cancelConn()
		if err := <-connErrCh; err != nil {
			t.Errorf("Execute(environments connect) = %v, want nil on context cancel", err)
		}
		if got, _ := connectWorkerID.Load().(string); got != "worker-cli-1" {
			t.Errorf("connect WorkerID = %q, want \"worker-cli-1\"", got)
		}

		// 3. Live `environments poll`
		pollCtx, cancelPoll := context.WithCancel(t.Context())
		pollRoot, _, _ := newRuntimeCLIRootForTest(t, srvURL)
		pollRoot.SetContext(pollCtx)
		pollRoot.SetArgs([]string{
			"environments", "poll",
			"--environment", "environments/env_live_poll",
			"--environment-key", "ek_poll",
			"--poller-id", "poller-cli-1",
			"--poll-interval", "20ms",
			"--provision-cmd", "cat > " + provisionFile,
			"--quiet",
		})
		pollErrCh := make(chan error, 1)
		go func() {
			pollErrCh <- pollRoot.Execute()
		}()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if raw, err := os.ReadFile(provisionFile); err == nil && len(raw) > 0 {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		cancelPoll()
		if err := <-pollErrCh; err != nil {
			t.Errorf("Execute(environments poll) = %v, want nil on context cancel", err)
		}
		if got, _ := pollEnvID.Load().(string); got != "env_live_poll" {
			t.Errorf("poll EnvironmentID = %q, want \"env_live_poll\"", got)
		}
	})

	t.Run("helpers_logger_and_no_retries", func(t *testing.T) {
		output.ResetAgentMode()
		output.ResetPreparsedRendering()
		config.Reset()
		t.Cleanup(func() {
			output.ResetAgentMode()
			output.ResetPreparsedRendering()
			config.Reset()
		})

		root, _, stderr := newRuntimeCLIRootForTest(t, "")
		logNormal := newRuntimeLogger(root, false)
		logNormal.Print("visible-msg")
		if !strings.Contains(stderr.String(), "visible-msg") {
			t.Errorf("newRuntimeLogger(quiet=false) did not write to stderr: %q", stderr.String())
		}
		stderr.Reset()
		logQuiet := newRuntimeLogger(root, true)
		logQuiet.Print("hidden-msg")
		if stderr.Len() != 0 {
			t.Errorf("newRuntimeLogger(quiet=true) wrote %q, want empty", stderr.String())
		}
		_ = root.PersistentFlags().Set("output-format", "json")
		logMachine := newRuntimeLogger(root, false)
		logMachine.Print("machine-hidden-msg")
		if stderr.Len() != 0 {
			t.Errorf("newRuntimeLogger(output-format=json) wrote %q, want empty", stderr.String())
		}

		if got := resolveNoRetries(root); got {
			t.Errorf("resolveNoRetries(default) = true, want false")
		}
		_ = root.PersistentFlags().Set("no-retries", "true")
		if got := resolveNoRetries(root); !got {
			t.Errorf("resolveNoRetries(--no-retries=true) = false, want true")
		}
	})
}

func TestRuntimeTransportCredentialsHeadersAndValidation(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GEMINI_ACCESS_TOKEN", "")
	t.Setenv("GEMINI_NO_KEYRING", "true")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	config.Reset()
	t.Cleanup(config.Reset)

	t.Run("credentials_missing_and_headers", func(t *testing.T) {
		cases := []struct {
			name         string
			explicitCred bool
			apiKey       string
			accessToken  string
			userProject  string
			apiRevision  string
			header       string
			wantMissing  bool
			wantAPIKey   string
			wantBearer   string
		}{
			{
				name:         "no_creds_without_explicit_runtime_cred",
				explicitCred: false,
				wantMissing:  true,
			},
			{
				name:         "no_creds_with_explicit_runtime_cred",
				explicitCred: true,
				wantMissing:  false,
			},
			{
				name:         "api_key_credential",
				explicitCred: false,
				apiKey:       "key-abc",
				userProject:  "proj-123",
				apiRevision:  "rev-2",
				header:       "X-Custom-Trace: trace-99",
				wantMissing:  false,
				wantAPIKey:   "key-abc",
			},
			{
				name:         "access_token_credential",
				explicitCred: false,
				accessToken:  "oauth-tok-xyz",
				wantMissing:  false,
				wantBearer:   "Bearer oauth-tok-xyz",
			},
			{
				name:         "api_key_wins_same_rank_tie",
				explicitCred: false,
				apiKey:       "tie-key-123",
				accessToken:  "tie-tok-456",
				wantMissing:  false,
				wantAPIKey:   "tie-key-123",
				wantBearer:   "",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cmd := newTestRuntimeCmd(t, "https://example.googleapis.com/")
				pf := cmd.Root().PersistentFlags()
				if tc.apiKey != "" {
					_ = pf.Set("api-key", tc.apiKey)
				}
				if tc.accessToken != "" {
					_ = pf.Set("access-token", tc.accessToken)
				}
				if tc.userProject != "" {
					_ = pf.Set("user-project", tc.userProject)
				}
				if tc.apiRevision != "" {
					_ = pf.Set("api-revision", tc.apiRevision)
				}
				if tc.header != "" {
					_ = pf.Set("header", tc.header)
				}
				_ = pf.Set("timeout", "3s")

				rt, err := newRuntimeTransport(cmd, tc.explicitCred)
				if err != nil {
					t.Fatalf("newRuntimeTransport() unexpected error: %v", err)
				}
				if got := flagutil.CredentialsMissing(cmd); got != tc.wantMissing {
					t.Errorf("CredentialsMissing() = %v, want %v", got, tc.wantMissing)
				}
				if rt.timeout != 3*time.Second {
					t.Errorf("rt.timeout = %v, want 3s", rt.timeout)
				}

				req, err := rt.newRequest(t.Context(), http.MethodPost, "/v1beta/test", http.NoBody)
				if err != nil {
					t.Fatalf("rt.newRequest() unexpected error: %v", err)
				}
				if got := req.Header.Get("x-goog-api-key"); got != tc.wantAPIKey {
					t.Errorf("x-goog-api-key = %q, want %q", got, tc.wantAPIKey)
				}
				if got := req.Header.Get("Authorization"); got != tc.wantBearer {
					t.Errorf("Authorization = %q, want %q", got, tc.wantBearer)
				}
				if tc.userProject != "" && req.Header.Get("x-goog-user-project") != tc.userProject {
					t.Errorf("x-goog-user-project = %q, want %q", req.Header.Get("x-goog-user-project"), tc.userProject)
				}
				if tc.apiRevision != "" && req.Header.Get("Api-Revision") != tc.apiRevision {
					t.Errorf("Api-Revision = %q, want %q", req.Header.Get("Api-Revision"), tc.apiRevision)
				}
				if tc.header != "" && req.Header.Get("X-Custom-Trace") != "trace-99" {
					t.Errorf("X-Custom-Trace = %q, want \"trace-99\"", req.Header.Get("X-Custom-Trace"))
				}
			})
		}
	})

	t.Run("transport_validation_errors", func(t *testing.T) {
		badCases := []struct {
			name      string
			serverURL string
			apiVer    string
			header    string
			timeout   string
			wantSub   string
		}{
			{name: "ftp_scheme", serverURL: "ftp://example.com", wantSub: "invalid --server-url"},
			{name: "url_with_credentials", serverURL: "https://user:pass@example.com", wantSub: "without credentials, query, or fragment"},
			{name: "url_with_query", serverURL: "https://example.com?foo=bar", wantSub: "without credentials, query, or fragment"},
			{name: "url_with_fragment", serverURL: "https://example.com#sec", wantSub: "without credentials, query, or fragment"},
			{name: "invalid_api_version", apiVer: "..", wantSub: "--api-version"},
			{name: "header_missing_colon", header: "BadHeader", wantSub: "invalid header format"},
			{name: "header_empty_key", header: " : val", wantSub: "invalid header name"},
			{name: "header_space_in_key", header: "Bad Key: val", wantSub: "invalid header name"},
			{name: "header_control_char_val", header: "X-Key: bad\x7fval", wantSub: "--header contains control characters"},
			{name: "invalid_timeout", timeout: "not-a-duration", wantSub: "invalid --timeout"},
		}
		for _, tc := range badCases {
			t.Run(tc.name, func(t *testing.T) {
				cmd := newTestRuntimeCmd(t, tc.serverURL)
				pf := cmd.Root().PersistentFlags()
				if tc.apiVer != "" {
					_ = pf.Set("api-version", tc.apiVer)
				}
				if tc.header != "" {
					_ = pf.Set("header", tc.header)
				}
				if tc.timeout != "" {
					_ = pf.Set("timeout", tc.timeout)
				}
				if got, err := newRuntimeTransport(cmd, true); err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("newRuntimeTransport() = (%v, %v), want error containing %q", got, err, tc.wantSub)
				}
			})
		}
	})

	t.Run("leased_interaction_unmarshal_edge_cases", func(t *testing.T) {
		validInputs := []struct {
			raw     string
			wantGen int64
		}{
			{raw: `{"interaction_id":"i1","lease_token":"t1","lease_generation":null}`, wantGen: 0},
			{raw: `{"interaction_id":"i2","lease_token":"t2","lease_generation":""}`, wantGen: 0},
		}
		for _, tc := range validInputs {
			var li LeasedInteraction
			if err := json.Unmarshal([]byte(tc.raw), &li); err != nil {
				t.Errorf("json.Unmarshal(%s) unexpected error: %v", tc.raw, err)
			} else if li.LeaseGeneration != tc.wantGen {
				t.Errorf("LeaseGeneration(%s) = %d, want %d", tc.raw, li.LeaseGeneration, tc.wantGen)
			}
		}

		for _, bad := range []string{
			`{not-json`,
			`{"interaction_id":"i1","lease_generation":"not-an-int"}`,
			`{"interaction_id":"i1","lease_generation":true}`,
		} {
			var li LeasedInteraction
			if err := json.Unmarshal([]byte(bad), &li); err == nil {
				t.Errorf("json.Unmarshal(%s) = %+v, want error", bad, li)
			}
		}
	})
}

func TestGRPCStatusParsingHeartbeatsAndPlainHTTPProbe(t *testing.T) {
	t.Run("decode_grpc_message", func(t *testing.T) {
		cases := []struct {
			in   string
			want string
		}{
			{in: "", want: ""},
			{in: "   ", want: ""},
			{in: "permission%20denied%3A%20expired", want: "permission denied: expired"},
			{in: "invalid%ZZpercent", want: "invalid%ZZpercent"},
		}
		for _, tc := range cases {
			if got := decodeGRPCMessage(tc.in); got != tc.want {
				t.Errorf("decodeGRPCMessage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("parse_grpc_status_and_code_mapping", func(t *testing.T) {
		for _, okCode := range []string{"", "0", " 0 "} {
			if err := parseGRPCStatus(okCode, "ignored", nil); err != nil {
				t.Errorf("parseGRPCStatus(%q) = %v, want nil", okCode, err)
			}
		}

		statusCases := []struct {
			rawCode    string
			rawMsg     string
			wantHTTP   int
			wantMsgSub string
		}{
			{rawCode: "3", rawMsg: "bad%20arg", wantHTTP: http.StatusBadRequest, wantMsgSub: "bad arg"},
			{rawCode: "4", rawMsg: "", wantHTTP: http.StatusGatewayTimeout, wantMsgSub: "gRPC status 4"},
			{rawCode: "5", rawMsg: "missing", wantHTTP: http.StatusNotFound, wantMsgSub: "missing"},
			{rawCode: "6", rawMsg: "exists", wantHTTP: http.StatusConflict, wantMsgSub: "exists"},
			{rawCode: "7", rawMsg: "denied", wantHTTP: http.StatusForbidden, wantMsgSub: "denied"},
			{rawCode: "8", rawMsg: "quota", wantHTTP: http.StatusTooManyRequests, wantMsgSub: "quota"},
			{rawCode: "9", rawMsg: "precondition", wantHTTP: http.StatusBadRequest, wantMsgSub: "precondition"},
			{rawCode: "10", rawMsg: "aborted", wantHTTP: http.StatusConflict, wantMsgSub: "aborted"},
			{rawCode: "11", rawMsg: "range", wantHTTP: http.StatusBadRequest, wantMsgSub: "range"},
			{rawCode: "12", rawMsg: "unimplemented", wantHTTP: http.StatusNotImplemented, wantMsgSub: "unimplemented"},
			{rawCode: "14", rawMsg: "unavailable", wantHTTP: http.StatusServiceUnavailable, wantMsgSub: "unavailable"},
			{rawCode: "16", rawMsg: "unauthenticated", wantHTTP: http.StatusUnauthorized, wantMsgSub: "unauthenticated"},
			{rawCode: "13", rawMsg: "internal", wantHTTP: http.StatusInternalServerError, wantMsgSub: "internal"},
			{rawCode: "not-a-number", rawMsg: "", wantHTTP: http.StatusInternalServerError, wantMsgSub: "gRPC status 2"},
		}
		for _, tc := range statusCases {
			err := parseGRPCStatus(tc.rawCode, tc.rawMsg, nil)
			if err == nil {
				t.Fatalf("parseGRPCStatus(%q, %q) = nil, want HTTP %d error", tc.rawCode, tc.rawMsg, tc.wantHTTP)
			}
			wantStatusText := fmt.Sprintf("Status %d", tc.wantHTTP)
			if !strings.Contains(err.Error(), wantStatusText) || !strings.Contains(err.Error(), tc.wantMsgSub) {
				t.Errorf("parseGRPCStatus(%q, %q) = %q, want substrings %q and %q", tc.rawCode, tc.rawMsg, err.Error(), wantStatusText, tc.wantMsgSub)
			}
		}
	})

	t.Run("grpc_headers_and_trailers_on_stream", func(t *testing.T) {
		var callNum atomic.Int32
		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := callNum.Add(1)
			if n == 1 {
				// Trailers-only gRPC error in initial response headers.
				w.Header().Set("Content-Type", "application/grpc")
				w.Header().Set("Grpc-Status", "16")
				w.Header().Set("Grpc-Message", "invalid%20lease%20token")
				w.WriteHeader(http.StatusOK)
				return
			}
			// Stream opens 200 OK and terminates with non-zero gRPC trailer status.
			w.Header().Set("Content-Type", "application/grpc")
			w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			_, _ = grpcReadFrame(r.Body)
			w.Header().Set("Grpc-Status", "14")
			w.Header().Set("Grpc-Message", "backend%20restarting")
		}))

		cmd := newTestRuntimeCmd(t, srvURL)
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport() failed: %v", err)
		}
		client := NewHTTPClient(HTTPClientOptions{Transport: rt})

		// Call 1: Grpc-Status in HTTP headers fails connectStream with non-retryable 401.
		_, err = client.connectStream(t.Context(), connectStreamRequest{InteractionID: "int_1", LeaseToken: "tok_1", NextOutputSeqNum: 1})
		if err == nil || !strings.Contains(err.Error(), "invalid lease token") || !isNonRetryableControlPlaneError(err) {
			t.Errorf("connectStream(header Grpc-Status=16) = %v, want non-retryable 401 error", err)
		}

		// Call 2: Grpc-Status in HTTP trailers surfaces on stream.Recv() EOF as retryable 503.
		stream, err := client.connectStream(t.Context(), connectStreamRequest{InteractionID: "int_2", LeaseToken: "tok_2", NextOutputSeqNum: 1})
		if err != nil {
			t.Fatalf("connectStream(call 2) unexpected error: %v", err)
		}
		defer stream.Close()
		_, recvErr := stream.Recv()
		if recvErr == nil || !strings.Contains(recvErr.Error(), "backend restarting") || isNonRetryableControlPlaneError(recvErr) {
			t.Errorf("stream.Recv(trailer Grpc-Status=14) = %v, want retryable 503 error", recvErr)
		}
	})

	t.Run("plain_http1_server_probe_surfaces_http_error", func(t *testing.T) {
		http1Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"code":403,"message":"forbidden plain http"}}`, http.StatusForbidden)
		}))
		defer http1Srv.Close()

		cmd := newTestRuntimeCmd(t, http1Srv.URL)
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport() failed: %v", err)
		}
		client := NewHTTPClient(HTTPClientOptions{Transport: rt})
		_, err = client.connectStream(t.Context(), connectStreamRequest{InteractionID: "int_h1", LeaseToken: "tok_h1", NextOutputSeqNum: 1})
		if err == nil || !strings.Contains(err.Error(), "Status 403") {
			t.Errorf("connectStream(HTTP/1.1 403) = %v, want probed Status 403 error", err)
		}
	})

	t.Run("heartbeat_cancel_and_error_branches_and_backoff", func(t *testing.T) {
		var mode atomic.Int32
		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch mode.Load() {
			case 1:
				http.Error(w, `{"error":{"code":409,"message":"lease stolen"}}`, http.StatusConflict)
			case 2:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"leases":[]}`))
			default:
				http.Error(w, `{"error":{"code":500,"message":"boom"}}`, http.StatusInternalServerError)
			}
		}))

		cmd := newTestRuntimeCmd(t, srvURL)
		_ = cmd.Root().PersistentFlags().Set("timeout", "2s")
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport() failed: %v", err)
		}
		client := NewHTTPClient(HTTPClientOptions{Transport: rt, EnvironmentKey: "ek_hb"})

		mode.Store(1)
		res409, err := client.HeartbeatInteraction(t.Context(), "p1", "int_1", "env_1", "tok_1")
		if err != nil || !res409.ShouldCancel || res409.Renewed {
			t.Errorf("HeartbeatInteraction(409) = (%+v, %v), want ShouldCancel=true", res409, err)
		}

		mode.Store(2)
		resEmpty, err := client.HeartbeatInteraction(t.Context(), "p1", "int_1", "env_1", "tok_1")
		if err != nil || !resEmpty.ShouldCancel || resEmpty.Renewed {
			t.Errorf("HeartbeatInteraction(empty leases) = (%+v, %v), want ShouldCancel=true", resEmpty, err)
		}

		mode.Store(3)
		if _, err := client.HeartbeatInteraction(t.Context(), "p1", "int_1", "env_1", "tok_1"); err == nil {
			t.Errorf("HeartbeatInteraction(500) = nil error, want 500 error")
		}

		if isNonRetryableControlPlaneError(nil) {
			t.Errorf("isNonRetryableControlPlaneError(nil) = true, want false")
		}
		if isNonRetryableControlPlaneError(errors.New("network reset")) {
			t.Errorf("isNonRetryableControlPlaneError(plain error) = true, want false")
		}
		if got := retryBackoff(0, 1); got != time.Second {
			t.Errorf("retryBackoff(0, 1) = %v, want 1s", got)
		}
		if got := retryBackoff(2*time.Second, 10); got != 30*time.Second {
			t.Errorf("retryBackoff(2s, 10) = %v, want capped 30s", got)
		}
	})
}

func TestProvisionAndDispatchPollerDefaultsAndSubprocessGroup(t *testing.T) {
	t.Run("resolve_provision_and_worker_shells", func(t *testing.T) {
		bin, args := resolveProvisionShell("linux", "echo hi")
		if !filepath.IsAbs(bin) || !slices.Equal(args, []string{"-c", "echo hi"}) {
			t.Errorf("resolveProvisionShell(linux, normal PATH) = (%q, %v), want abs sh path and [-c echo hi]", bin, args)
		}

		shOnlyDir := t.TempDir()
		fakeSh := filepath.Join(shOnlyDir, "sh")
		if err := os.WriteFile(fakeSh, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("WriteFile(fakeSh) failed: %v", err)
		}
		t.Setenv("PATH", shOnlyDir)
		wBinSh, wArgsSh := resolveShellCommand("linux", "pwd")
		if wBinSh != fakeSh || !slices.Equal(wArgsSh, []string{"-c", "pwd"}) {
			t.Errorf("resolveShellCommand(sh-only PATH) = (%q, %v), want (%q, [-c pwd])", wBinSh, wArgsSh, fakeSh)
		}

		t.Setenv("PATH", "")
		winBin, winArgs := resolveProvisionShell("windows", "echo win")
		if winBin != "cmd.exe" || !slices.Equal(winArgs, []string{"/C", "echo win"}) {
			t.Errorf("resolveProvisionShell(windows, empty PATH) = (%q, %v), want (cmd.exe, [/C echo win])", winBin, winArgs)
		}
		linBin, linArgs := resolveProvisionShell("linux", "echo lin")
		if linBin != "sh" || !slices.Equal(linArgs, []string{"-c", "echo lin"}) {
			t.Errorf("resolveProvisionShell(linux, empty PATH) = (%q, %v), want (sh, [-c echo lin])", linBin, linArgs)
		}

		wWinBin, wWinArgs := resolveShellCommand("windows", "dir")
		if wWinBin != "cmd.exe" || !slices.Equal(wWinArgs, []string{"/C", "dir"}) {
			t.Errorf("resolveShellCommand(windows, empty PATH) = (%q, %v), want (cmd.exe, [/C dir])", wWinBin, wWinArgs)
		}
		wLinBin, wLinArgs := resolveShellCommand("linux", "ls")
		if wLinBin != "sh" || !slices.Equal(wLinArgs, []string{"-c", "ls"}) {
			t.Errorf("resolveShellCommand(linux, empty PATH) = (%q, %v), want (sh, [-c ls])", wLinBin, wLinArgs)
		}
	})

	t.Run("provision_env_scrubbing_redaction_and_process_group_kill", func(t *testing.T) {
		emptyProv := NewCommandWorkerProvisioner("   ")
		if err := emptyProv.ProvisionAndDispatch(t.Context(), ProvisionInput{InteractionID: "i1", EnvironmentID: "e1"}); err == nil {
			t.Errorf("ProvisionAndDispatch(empty cmd) = nil, want error")
		}

		// Verify GEMINI_API_KEY and GEMINI_ENVIRONMENT_KEY are scrubbed from --provision-cmd's environment
		// while non-credential environment variables are preserved, and empty SchemaVersion defaults to "v1".
		t.Setenv("GEMINI_API_KEY", "scrub_this_api_key")
		t.Setenv("GEMINI_ENVIRONMENT_KEY", "scrub_this_env_key")
		t.Setenv("LEASE_TOKEN", "scrub_this_lease_tok")
		t.Setenv("CUSTOM_ORCHESTRATOR_VAR", "keep_orchestrator_val")

		envDump := filepath.Join(t.TempDir(), "prov.env")
		stdinDump := filepath.Join(t.TempDir(), "prov.json")
		t.Setenv("TEST_STDIN_DUMP", stdinDump)
		t.Setenv("TEST_ENV_DUMP", envDump)
		prov := NewCommandWorkerProvisioner(`cat > "$TEST_STDIN_DUMP" && env > "$TEST_ENV_DUMP"`)
		if err := prov.ProvisionAndDispatch(t.Context(), ProvisionInput{
			InteractionID: "int_env_test",
			EnvironmentID: "env_test",
			LeaseToken:    "lt_secret_123",
		}); err != nil {
			t.Fatalf("ProvisionAndDispatch() unexpected error: %v", err)
		}

		envBytes, err := os.ReadFile(envDump)
		if err != nil {
			t.Fatalf("ReadFile(envDump) failed: %v", err)
		}
		envText := string(envBytes)
		for _, leaked := range []string{"scrub_this_api_key", "scrub_this_env_key", "scrub_this_lease_tok"} {
			if strings.Contains(envText, leaked) {
				t.Errorf("ProvisionAndDispatch leaked credential %q into child env:\n%s", leaked, envText)
			}
		}
		if !strings.Contains(envText, "CUSTOM_ORCHESTRATOR_VAR=keep_orchestrator_val") {
			t.Errorf("ProvisionAndDispatch dropped non-credential var CUSTOM_ORCHESTRATOR_VAR:\n%s", envText)
		}

		var gotInput ProvisionInput
		stdinBytes, err := os.ReadFile(stdinDump)
		if err != nil || json.Unmarshal(stdinBytes, &gotInput) != nil {
			t.Fatalf("reading ProvisionInput dump failed: %v", err)
		}
		if gotInput.SchemaVersion != "v1" {
			t.Errorf("ProvisionInput.SchemaVersion = %q, want default \"v1\"", gotInput.SchemaVersion)
		}

		// Verify stderr redaction of LeaseToken and empty-stderr failure formatting.
		redactProv := NewCommandWorkerProvisioner("printf 'failed with lt_secret_abc\\n' >&2; exit 2")
		err = redactProv.ProvisionAndDispatch(t.Context(), ProvisionInput{
			InteractionID: "int_fail",
			EnvironmentID: "env_fail",
			LeaseToken:    "lt_secret_abc",
		})
		if err == nil || strings.Contains(err.Error(), "lt_secret_abc") || !strings.Contains(err.Error(), "[REDACTED]") {
			t.Errorf("ProvisionAndDispatch(stderr leak) error = %v, want [REDACTED] without raw token", err)
		}

		silentFailProv := NewCommandWorkerProvisioner("exit 3")
		if err := silentFailProv.ProvisionAndDispatch(t.Context(), ProvisionInput{InteractionID: "i3"}); err == nil ||
			!strings.HasPrefix(err.Error(), "provision command failed: ") || strings.Contains(err.Error(), "()") {
			t.Errorf("ProvisionAndDispatch(exit 3, empty stderr) = %v, want 'provision command failed: ...' without empty '()'", err)
		}

		// Verify configureSubprocessGroup kills background grandchild processes holding stderr open on timeout:
		// without process-group SIGKILL, cmd.Run would block ~4s waiting for the grandchild's stderr pipe to close!
		timeoutCtx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
		defer cancel()
		start := time.Now()
		bgProv := NewCommandWorkerProvisioner("(sleep 4; echo orphan >&2) & sleep 4")
		if err := bgProv.ProvisionAndDispatch(timeoutCtx, ProvisionInput{InteractionID: "i_bg"}); err == nil {
			t.Errorf("ProvisionAndDispatch(timeout) = nil, want deadline error")
		}
		if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
			t.Errorf("ProvisionAndDispatch(background child) took %v after 80ms timeout, want immediate process-group kill", elapsed)
		}
	})

	t.Run("subprocess_group_unstarted_cancel_and_signals", func(t *testing.T) {
		sigs := runtimeShutdownSignals()
		if len(sigs) == 0 {
			t.Errorf("runtimeShutdownSignals() = empty, want OS shutdown signals")
		}
		unstarted := exec.Command("sh", "-c", "true")
		configureSubprocessGroup(unstarted)
		if unstarted.Cancel == nil {
			t.Fatalf("configureSubprocessGroup() left cmd.Cancel == nil")
		}
		if err := unstarted.Cancel(); err != nil {
			t.Errorf("unstarted cmd.Cancel() = %v, want nil when cmd.Process is nil", err)
		}
	})

	t.Run("poller_defaults_cooldown_eviction_cancel_and_no_retries", func(t *testing.T) {
		defaultPoller := NewPollerController(nil, nil, PollerOptions{})
		if defaultPoller.pollerID == "" || defaultPoller.maxInteractions != 10 || defaultPoller.pollInterval != 2*time.Second ||
			defaultPoller.provisionTimeout != 2*time.Minute || defaultPoller.handoffHeartbeatInterval != defaultHeartbeatInterval || defaultPoller.logger == nil {
			t.Errorf("NewPollerController(zero opts) = %+v, want populated defaults", defaultPoller)
		}

		// Verify stale failure entries (>10m past cooldownEnd) are evicted while recent entries (<10m) are retained.
		defaultPoller.failures["stale_int"] = &failureState{
			consecutive: 2,
			cooldownEnd: time.Now().Add(-15 * time.Minute),
		}
		defaultPoller.failures["recent_int"] = &failureState{
			consecutive: 1,
			cooldownEnd: time.Now().Add(-5 * time.Minute),
		}
		defaultPoller.recordDispatchFailure("fresh_int")
		if _, exists := defaultPoller.failures["stale_int"]; exists {
			t.Errorf("recordDispatchFailure() kept stale_int past 10m eviction window")
		}
		if _, exists := defaultPoller.failures["recent_int"]; !exists {
			t.Errorf("recordDispatchFailure() evicted recent_int within 10m eviction window")
		}
		if st := defaultPoller.failures["fresh_int"]; st == nil || st.consecutive != 1 {
			t.Errorf("recordDispatchFailure(fresh_int) = %+v, want consecutive=1", st)
		}

		// Verify exponential cooldown progression (500ms -> 1s -> 2s -> 4s -> 8s -> 15s cap -> 15s cap).
		wantCooldowns := []time.Duration{
			500 * time.Millisecond,
			1 * time.Second,
			2 * time.Second,
			4 * time.Second,
			8 * time.Second,
			15 * time.Second,
			15 * time.Second,
		}
		for i, want := range wantCooldowns {
			before := time.Now()
			defaultPoller.recordDispatchFailure("backoff_seq")
			after := time.Now()
			st := defaultPoller.failures["backoff_seq"]
			if st == nil || st.consecutive != i+1 {
				t.Fatalf("recordDispatchFailure(backoff_seq #%d) = %+v, want consecutive=%d", i+1, st, i+1)
			}
			minEnd := before.Add(want - 50*time.Millisecond)
			maxEnd := after.Add(want + 50*time.Millisecond)
			if st.cooldownEnd.Before(minEnd) || st.cooldownEnd.After(maxEnd) {
				t.Errorf("recordDispatchFailure(backoff_seq #%d) cooldown ≈ %v, want %v", i+1, st.cooldownEnd.Sub(before), want)
			}
		}

		// Verify ShouldCancel heartbeat cancels an active slow ProvisionAndDispatch immediately,
		// and NoRetries=true returns transient 503 from Run() without retrying.
		var leaseCount atomic.Int32
		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req leaseInteractionsRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.ActiveLeases) > 0 {
				http.Error(w, `{"error":{"code":404,"message":"canceled"}}`, http.StatusNotFound)
				return
			}
			if leaseCount.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"leases":[{"interaction_id":"int_cancel_me","lease_token":"tok_c"}]}`))
				return
			}
			http.Error(w, `{"error":{"code":503,"message":"transient"}}`, http.StatusServiceUnavailable)
		}))

		cmd := newTestRuntimeCmd(t, srvURL)
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport() failed: %v", err)
		}
		client := NewHTTPClient(HTTPClientOptions{Transport: rt, EnvironmentKey: "ek_1"})
		poller := NewPollerController(client, NewCommandWorkerProvisioner("sleep 5"), PollerOptions{
			PollerID:                 "poller-cancel-test",
			MaxInteractions:          1,
			PollInterval:             20 * time.Millisecond,
			ProvisionTimeout:         10 * time.Second,
			HandoffHeartbeatInterval: 25 * time.Millisecond,
			NoRetries:                true,
			Logger:                   log.New(io.Discard, "", 0),
		})

		start := time.Now()
		runErr := poller.Run(t.Context())
		if runErr == nil || !strings.Contains(runErr.Error(), "Status 503") {
			t.Errorf("PollerController.Run(NoRetries=true) = %v, want Status 503 error", runErr)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("PollerController ShouldCancel handoff abort took %v, want < 2s", elapsed)
		}
	})
}

func TestSandboxPathsCappedBufferAndFileOperations(t *testing.T) {
	t.Run("resolve_sandbox_dirs_symlinks_dangling_and_create", func(t *testing.T) {
		base := t.TempDir()
		realRoot := filepath.Join(base, "real_root")
		if err := os.MkdirAll(filepath.Join(realRoot, "sub"), 0o755); err != nil {
			t.Fatalf("MkdirAll(realRoot/sub) failed: %v", err)
		}
		evalRealRoot, err := filepath.EvalSymlinks(realRoot)
		if err != nil {
			t.Fatalf("EvalSymlinks(realRoot) failed: %v", err)
		}

		symRoot := filepath.Join(base, "sym_root")
		if err := os.Symlink(realRoot, symRoot); err != nil {
			t.Fatalf("Symlink(symRoot) failed: %v", err)
		}

		// Symlinked rootDir with workDir under symlinked rootDir must canonicalize both!
		gotRoot, gotWork, err := resolveSandboxDirs(symRoot, filepath.Join(symRoot, "sub"), false)
		if err != nil {
			t.Fatalf("resolveSandboxDirs(symRoot, symRoot/sub, false) failed: %v", err)
		}
		if gotRoot != evalRealRoot {
			t.Errorf("gotRoot = %q, want canonical %q", gotRoot, evalRealRoot)
		}
		wantWork := filepath.Join(evalRealRoot, "sub")
		if gotWork != wantWork {
			t.Errorf("gotWork = %q, want canonical %q", gotWork, wantWork)
		}

		// Dangling symlink as rootDir must be rejected by rejectDanglingSymlinkPrefix!
		danglingRoot := filepath.Join(base, "dangling_root")
		if err := os.Symlink(filepath.Join(base, "does_not_exist"), danglingRoot); err != nil {
			t.Fatalf("Symlink(danglingRoot) failed: %v", err)
		}
		if _, _, err := resolveSandboxDirs(filepath.Join(danglingRoot, "nested"), "", false); err == nil || !strings.Contains(err.Error(), "dangling symlink") {
			t.Errorf("resolveSandboxDirs(dangling symlink) = %v, want dangling symlink error", err)
		}

		// create=true creates missing rootDir and workDir.
		newRoot := filepath.Join(evalRealRoot, "auto_created_root")
		newWork := filepath.Join(newRoot, "nested_work")
		createdRoot, createdWork, err := resolveSandboxDirs(newRoot, newWork, true)
		if err != nil {
			t.Fatalf("resolveSandboxDirs(create=true) failed: %v", err)
		}
		if createdRoot != newRoot || createdWork != newWork {
			t.Errorf("resolveSandboxDirs(create=true) = (%q, %q), want (%q, %q)", createdRoot, createdWork, newRoot, newWork)
		}

		// Empty rootDir ("") and relative workDir ("rel_sub") resolve against cwd.
		t.Chdir(evalRealRoot)
		cwdRoot, cwdWork, err := resolveSandboxDirs("", "rel_sub", true)
		if err != nil {
			t.Fatalf("resolveSandboxDirs(\"\", \"rel_sub\", true) failed: %v", err)
		}
		if cwdRoot != evalRealRoot || cwdWork != filepath.Join(evalRealRoot, "rel_sub") {
			t.Errorf("resolveSandboxDirs(cwd) = (%q, %q), want (%q, %q)", cwdRoot, cwdWork, evalRealRoot, filepath.Join(evalRealRoot, "rel_sub"))
		}
	})

	t.Run("virtual_absolute_paths_and_file_edge_cases", func(t *testing.T) {
		rootDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("EvalSymlinks(TempDir) failed: %v", err)
		}
		workDir := filepath.Join(rootDir, "app")
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(workDir) failed: %v", err)
		}
		// newFSServer falls back to rootDir when workDir escapes rootDir.
		if fallbackFS := newFSServer(rootDir, t.TempDir()); fallbackFS.workDir != rootDir {
			t.Errorf("newFSServer(outside workDir).workDir = %q, want %q", fallbackFS.workDir, rootDir)
		}
		fs := newFSServer(rootDir, workDir)

		// Dangling symlink under an evaluated directory suffix is rejected.
		realTargetDir := filepath.Join(rootDir, "real_target")
		if err := os.MkdirAll(realTargetDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(realTargetDir) failed: %v", err)
		}
		if err := os.Symlink(filepath.Join(rootDir, "missing_leaf"), filepath.Join(realTargetDir, "dangling_leaf")); err != nil {
			t.Fatalf("Symlink(dangling_leaf) failed: %v", err)
		}
		if _, err := fs.ResolvePath(filepath.Join("..", "real_target", "dangling_leaf", "sub")); err == nil || !strings.Contains(err.Error(), "dangling symlink") {
			t.Errorf("ResolvePath(dangling suffix) = %v, want dangling symlink error", err)
		}

		// Virtual absolute path "/workspace/hello.txt" outside rootDir maps cleanly inside rootDir!
		gotVirt, err := fs.ResolvePath("/workspace/hello.txt")
		wantVirt := filepath.Join(rootDir, "workspace", "hello.txt")
		if err != nil || gotVirt != wantVirt {
			t.Errorf("ResolvePath(/workspace/hello.txt) = (%q, %v), want (%q, nil)", gotVirt, err, wantVirt)
		}

		// Real absolute path already inside rootDir stays unchanged.
		realInside := filepath.Join(rootDir, "app", "main.py")
		if gotReal, err := fs.ResolvePath(realInside); err != nil || gotReal != realInside {
			t.Errorf("ResolvePath(%q) = (%q, %v), want (%q, nil)", realInside, gotReal, err, realInside)
		}
		if gotDot, err := fs.ResolvePath("."); err != nil || gotDot != workDir {
			t.Errorf("ResolvePath(.) = (%q, %v), want (%q, nil)", gotDot, err, workDir)
		}
		if gotRoot, err := fs.ResolvePath(rootDir); err != nil || gotRoot != rootDir {
			t.Errorf("ResolvePath(rootDir) = (%q, %v), want (%q, nil)", gotRoot, err, rootDir)
		}

		// WriteRegularFile modes: overwrite=false (O_EXCL) and appendMode=true (O_APPEND).
		if _, err := fs.WriteRegularFile("log.txt", "line1\n", false, false); err != nil {
			t.Fatalf("WriteRegularFile(create excl) failed: %v", err)
		}
		if _, err := fs.WriteRegularFile("log.txt", "dup\n", false, false); err == nil {
			t.Errorf("WriteRegularFile(overwrite=false on existing file) = nil, want O_EXCL error")
		}
		if _, err := fs.WriteRegularFile("log.txt", "line2\n", false, true); err != nil {
			t.Fatalf("WriteRegularFile(appendMode=true) failed: %v", err)
		}
		data, err := fs.ReadRegularFile("log.txt")
		if err != nil || string(data) != "line1\nline2\n" {
			t.Errorf("ReadRegularFile(log.txt) = (%q, %v), want \"line1\\nline2\\n\"", data, err)
		}

		// Directory read errors and reversed ReadFileRange.
		if _, err := fs.ReadRegularFile("."); err == nil || !strings.Contains(err.Error(), "directory") {
			t.Errorf("ReadRegularFile(.) = %v, want directory error", err)
		}
		if _, err := fs.ReadFileRange(".", 1, 2); err == nil || !strings.Contains(err.Error(), "directory") {
			t.Errorf("ReadFileRange(.) = %v, want directory error", err)
		}
		if emptySlice, err := fs.ReadFileRange("log.txt", 5, 2); err != nil || emptySlice != "" {
			t.Errorf("ReadFileRange(start=5, end=2) = (%q, %v), want (\"\", nil)", emptySlice, err)
		}

		// Grep literal + case-insensitive, empty query error, and invalid regex error.
		if _, err := fs.WriteRegularFile("a/b/deep.txt", "Alpha+Beta\n", true, false); err != nil {
			t.Fatalf("WriteRegularFile(a/b/deep.txt) failed: %v", err)
		}
		matches, _, err := fs.Grep(".", "alpha+beta", true, false)
		if err != nil || len(matches) != 1 {
			t.Errorf("Grep(literal caseInsensitive) = (%v, %v), want 1 match", matches, err)
		}
		if _, _, err := fs.Grep(".", "", false, false); err == nil {
			t.Errorf("Grep(empty query) = nil, want error")
		}
		if _, _, err := fs.Grep(".", "[unclosed", false, true); err == nil {
			t.Errorf("Grep(invalid regex) = nil, want error")
		}

		// FindFiles maxDepth=1 skips deeper directories.
		shallow, _, err := fs.FindFiles(".", "", 1)
		if err != nil {
			t.Fatalf("FindFiles(maxDepth=1) failed: %v", err)
		}
		for _, item := range shallow {
			if strings.Contains(fmt.Sprint(item), "/") {
				t.Errorf("FindFiles(maxDepth=1) returned nested entry %v in %v", item, shallow)
			}
		}

		// Escape, missing-file, non-regular-write, single-file Grep, and ListEntries error paths.
		if firstLine, err := fs.ReadFileRange("log.txt", 0, 1); err != nil || firstLine != "line1" {
			t.Errorf("ReadFileRange(start=0, end=1) = (%q, %v), want (\"line1\", nil)", firstLine, err)
		}
		if _, err := fs.ReadRegularFile("missing.txt"); err == nil {
			t.Errorf("ReadRegularFile(missing) = nil, want error")
		}
		if _, err := fs.ReadRegularFile("../../escape"); err == nil {
			t.Errorf("ReadRegularFile(escape) = nil, want error")
		}
		if _, err := fs.ReadFileRange("missing.txt", 1, 2); err == nil {
			t.Errorf("ReadFileRange(missing) = nil, want error")
		}
		if _, err := fs.ReadFileRange("../../escape", 1, 2); err == nil {
			t.Errorf("ReadFileRange(escape) = nil, want error")
		}
		if _, err := fs.WriteRegularFile("../../escape", "x", true, false); err == nil {
			t.Errorf("WriteRegularFile(escape) = nil, want error")
		}
		if _, err := fs.WriteRegularFile("a", "x", true, false); err == nil {
			t.Errorf("WriteRegularFile(existing dir) = nil, want error")
		}
		if _, err := fs.ListEntries("missing_dir"); err == nil {
			t.Errorf("ListEntries(missing_dir) = nil, want error")
		}
		if _, err := fs.ListEntries("../../escape"); err == nil {
			t.Errorf("ListEntries(escape) = nil, want error")
		}
		if _, err := fs.ListEntries("log.txt"); err == nil {
			t.Errorf("ListEntries(regular file) = nil, want error")
		}
		if singleMatch, _, err := fs.Grep("log.txt", "line1", false, false); err != nil || len(singleMatch) != 1 {
			t.Errorf("Grep(single file) = (%v, %v), want 1 match", singleMatch, err)
		}
		if _, _, err := fs.Grep("../../escape", "x", false, false); err == nil {
			t.Errorf("Grep(escape) = nil, want error")
		}
		if _, _, err := fs.FindFiles("../../escape", "*", 0); err == nil {
			t.Errorf("FindFiles(escape) = nil, want error")
		}
	})

	t.Run("capped_buffer_partial_and_full_truncation", func(t *testing.T) {
		cb := &cappedBuffer{maxBytes: 10}
		if n, err := cb.Write([]byte("123456")); err != nil || n != 6 {
			t.Fatalf("cappedBuffer.Write(6 bytes) = (%d, %v), want (6, nil)", n, err)
		}
		if cb.String() != "123456" {
			t.Errorf("cappedBuffer.String() before cap = %q, want \"123456\"", cb.String())
		}
		// Partial write overflow (remaining = 4, len(p) = 8).
		if n, err := cb.Write([]byte("ABCDEFGH")); err != nil || n != 8 {
			t.Fatalf("cappedBuffer.Write(partial overflow) = (%d, %v), want (8, nil)", n, err)
		}
		// Full write overflow when remaining == 0.
		if n, err := cb.Write([]byte("XYZ")); err != nil || n != 3 {
			t.Fatalf("cappedBuffer.Write(zero remaining) = (%d, %v), want (3, nil)", n, err)
		}
		want := "123456ABCD\n...[output truncated at 4 MiB]..."
		if got := cb.String(); got != want {
			t.Errorf("cappedBuffer.String() = %q, want %q", got, want)
		}
	})
}

func TestWirePrimitivesProtoValuesAndWorkerConsumption(t *testing.T) {
	t.Run("frame_limits_and_empty_frame", func(t *testing.T) {
		var buf bytes.Buffer
		oversized := make([]byte, maxGRPCFrameBytes+1)
		if err := grpcWriteFrame(&buf, oversized); err == nil {
			t.Errorf("grpcWriteFrame(oversized) = nil, want limit error")
		}

		if err := grpcWriteFrame(&buf, nil); err != nil {
			t.Fatalf("grpcWriteFrame(empty) failed: %v", err)
		}
		gotEmpty, err := grpcReadFrame(&buf)
		if err != nil || gotEmpty != nil {
			t.Errorf("grpcReadFrame(empty) = (%x, %v), want (nil, nil)", gotEmpty, err)
		}
	})

	t.Run("proto_value_round_trip_and_wire_primitives", func(t *testing.T) {
		type customStringer struct{ v string }
		inStruct := map[string]any{
			"nil_val":    nil,
			"bool_true":  true,
			"bool_false": false,
			"int_val":    int(12),
			"int32_val":  int32(34),
			"int64_val":  int64(56),
			"float_val":  float64(7.5),
			"str_val":    "hello",
			"any_list":   []any{"a", float64(2), nil},
			"str_list":   []string{"x", "y"},
			"nested":     map[string]any{"k": "v"},
			"fallback":   customStringer{v: "custom"},
		}
		decoded, err := decodeProtoStruct(encodeProtoStruct(inStruct), 0)
		if err != nil {
			t.Fatalf("decodeProtoStruct() failed: %v", err)
		}
		if decoded["nil_val"] != nil || decoded["bool_true"] != true || decoded["bool_false"] != false ||
			decoded["int_val"] != float64(12) || decoded["int32_val"] != float64(34) || decoded["int64_val"] != float64(56) ||
			decoded["float_val"] != float64(7.5) || decoded["str_val"] != "hello" || decoded["fallback"] != "{custom}" {
			t.Errorf("decodeProtoStruct() scalar mismatch: %+v", decoded)
		}
		if !reflect.DeepEqual(decoded["str_list"], []any{"x", "y"}) {
			t.Errorf("decoded str_list = %#v, want []any{\"x\", \"y\"}", decoded["str_list"])
		}

		if _, err := decodeProtoValue(nil, maxProtoNestingDepth+1); err == nil {
			t.Errorf("decodeProtoValue(depth overflow) = nil, want error")
		}
		if _, err := decodeProtoList(nil, maxProtoNestingDepth+1); err == nil {
			t.Errorf("decodeProtoList(depth overflow) = nil, want error")
		}

		// Exercise consumeVarint and consumeField wire types (fixed64, fixed32, overflow, invalid field/type).
		if _, _, err := consumeVarint(nil); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("consumeVarint(nil) = %v, want ErrUnexpectedEOF", err)
		}
		overflowVarint := bytes.Repeat([]byte{0x80}, 11)
		if _, _, err := consumeVarint(overflowVarint); err == nil {
			t.Errorf("consumeVarint(11-byte overflow) = nil, want overflow error")
		}
		if _, _, _, _, _, err := consumeField([]byte{0x00}); err == nil {
			t.Errorf("consumeField(field 0) = nil, want invalid field number error")
		}

		fixed64Buf := binary.LittleEndian.AppendUint64(appendTag(nil, 2, wireFixed64), math.Float64bits(42.25))
		if num, wt, vv, _, _, err := consumeField(fixed64Buf); err != nil || num != 2 || wt != wireFixed64 || math.Float64frombits(vv) != 42.25 {
			t.Errorf("consumeField(fixed64) = (%d, %d, %v, %v), want field 2 = 42.25", num, wt, math.Float64frombits(vv), err)
		}
		if _, _, _, _, _, err := consumeField(appendTag(nil, 2, wireFixed64)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("consumeField(truncated fixed64) = %v, want ErrUnexpectedEOF", err)
		}

		fixed32Buf := binary.LittleEndian.AppendUint32(appendTag(nil, 3, wireFixed32), 0x12345678)
		if num, wt, vv, _, _, err := consumeField(fixed32Buf); err != nil || num != 3 || wt != wireFixed32 || vv != 0x12345678 {
			t.Errorf("consumeField(fixed32) = (%d, %d, %#x, %v), want field 3 = 0x12345678", num, wt, vv, err)
		}
		if _, _, _, _, _, err := consumeField(appendTag(nil, 3, wireFixed32)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("consumeField(truncated fixed32) = %v, want ErrUnexpectedEOF", err)
		}
		if _, _, _, _, _, err := consumeField(appendTag(nil, 4, 3)); err == nil {
			t.Errorf("consumeField(wireType=3) = nil, want unsupported wire type error")
		}
	})

	t.Run("worker_defaults_reconnected_and_completed_stream_events", func(t *testing.T) {
		defWorker := newPullWorker(nil, nil, nil, pullWorkerOptions{})
		if defWorker.workerID == "" || !strings.HasPrefix(defWorker.workerID, "worker-") {
			t.Errorf("default workerID = %q, want worker-<host>-<pid>", defWorker.workerID)
		}
		if defWorker.maxConcurrent != 4 {
			t.Errorf("default maxConcurrent = %d, want 4", defWorker.maxConcurrent)
		}
		if defWorker.pollInterval != 2*time.Second {
			t.Errorf("default pollInterval = %v, want 2s", defWorker.pollInterval)
		}
		if defWorker.heartbeatInterval != defaultHeartbeatInterval {
			t.Errorf("default heartbeatInterval = %v, want %v", defWorker.heartbeatInterval, defaultHeartbeatInterval)
		}
		if defWorker.commandTimeout != 2*time.Minute {
			t.Errorf("default commandTimeout = %v, want 2m", defWorker.commandTimeout)
		}
		if defWorker.logger == nil {
			t.Errorf("default logger = nil, want non-nil")
		}

		customWorker := newPullWorker(nil, nil, nil, pullWorkerOptions{
			WorkerID:          "w-explicit",
			MaxConcurrent:     2,
			PollInterval:      100 * time.Millisecond,
			HeartbeatInterval: 80 * time.Millisecond,
			CommandTimeout:    5 * time.Second,
			Logger:            log.New(io.Discard, "", 0),
		})
		if customWorker.maxConcurrent != 2 || customWorker.heartbeatInterval != 80*time.Millisecond {
			t.Errorf("explicit pullWorkerOptions = %+v, want maxConcurrent=2 heartbeatInterval=80ms", customWorker)
		}

		// Build a ServerEventBatch stream carrying:
		// 1. InteractionReconnected(next_input_seq_num=5) + AckInputSeqNum=4
		// 2. InteractionCompleted (field 6)
		var serverFrames bytes.Buffer
		reconnectedBatch := appendVarintField(nil, 2, 4)
		reconnectedBatch = appendBytesField(reconnectedBatch, 8, appendVarintField(nil, 2, 5))
		_ = grpcWriteFrame(&serverFrames, reconnectedBatch)
		_ = grpcWriteFrame(&serverFrames, appendBytesField(nil, 6, nil))

		pr, pw := io.Pipe()
		go func() {
			_, _ = io.Copy(io.Discard, pr)
		}()
		stream := &grpcInteractionStream{
			resp: &http.Response{Body: io.NopCloser(&serverFrames)},
			pw:   pw,
		}
		state := &sessionState{
			interactionID: "int_comp",
			nextClientSeq: 1,
			executedCalls: make(map[string]*wirePayload),
		}

		reconnect, err := customWorker.consumeStream(t.Context(), stream, state)
		if err != nil || reconnect {
			t.Errorf("consumeStream(InteractionReconnected + InteractionCompleted) = (%v, %v), want (false, nil)", reconnect, err)
		}
		if state.nextClientSeq != 5 {
			t.Errorf("state.nextClientSeq after InteractionReconnected(5) = %d, want 5", state.nextClientSeq)
		}
		if !stream.closed {
			t.Errorf("stream.closed = false, want CloseSend called on InteractionCompleted")
		}
	})

	t.Run("tool_helpers_and_replace_in_text_edges", func(t *testing.T) {
		rootDir := t.TempDir()
		subDir := filepath.Join(rootDir, "sub")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(subDir) failed: %v", err)
		}
		w := newPullWorker(nil, newFSServer(rootDir, rootDir), newShellServer(rootDir), pullWorkerOptions{
			CommandTimeout: 5 * time.Second,
			Logger:         log.New(io.Discard, "", 0),
		})

		if res, isErr := w.dispatchTool(t.Context(), "nonexistent_tool", nil); !isErr || !strings.Contains(fmt.Sprint(res["error"]), "unsupported sandbox tool") {
			t.Errorf("dispatchTool(nonexistent_tool) = (%v, %v), want unsupported tool error", res, isErr)
		}
		if _, isErr := w.toolExecuteBash(t.Context(), map[string]any{"command": "pwd", "Cwd": "../../escape"}); !isErr {
			t.Errorf("toolExecuteBash(escaping Cwd) = false, want error")
		}
		bashOK, isErr := w.toolExecuteBash(t.Context(), map[string]any{"command": "printf ok", "Cwd": "sub", "timeout_ms": int64(2000)})
		if isErr || bashOK["stdout"] != "ok" {
			t.Errorf("toolExecuteBash(valid Cwd + timeout_ms) = (%v, %v), want stdout=\"ok\"", bashOK, isErr)
		}

		if _, isErr := w.toolViewFile(nil); !isErr {
			t.Errorf("toolViewFile(nil) = false, want missing path error")
		}
		if _, isErr := w.toolWriteFile(nil); !isErr {
			t.Errorf("toolWriteFile(nil) = false, want missing path error")
		}
		if _, isErr := w.toolReplaceFileContent(nil); !isErr {
			t.Errorf("toolReplaceFileContent(nil) = false, want missing TargetFile error")
		}
		if _, isErr := w.toolReplaceFileContent(map[string]any{"TargetFile": "a.txt"}); !isErr {
			t.Errorf("toolReplaceFileContent(missing TargetContent) = false, want error")
		}

		// replaceInText edge cases.
		if _, _, err := replaceInText("hello", "missing", "x", 0, 0, false); err == nil {
			t.Errorf("replaceInText(missing) = nil, want not found error")
		}
		if _, _, err := replaceInText("dup dup", "dup", "x", 0, 0, false); err == nil {
			t.Errorf("replaceInText(multiple with allowMultiple=false) = nil, want error")
		}
		gotMulti, cnt, err := replaceInText("dup dup", "dup", "ok", 0, 0, true)
		if err != nil || cnt != 2 || gotMulti != "ok ok" {
			t.Errorf("replaceInText(allowMultiple=true) = (%q, %d, %v), want (\"ok ok\", 2, nil)", gotMulti, cnt, err)
		}
		if _, _, err := replaceInText("a\nb\nc", "b", "B", 5, 2, false); err == nil {
			t.Errorf("replaceInText(invalid range 5..2) = nil, want invalid line range error")
		}
		if _, _, err := replaceInText("a\nb\nb\nc", "b", "B", 2, 3, false); err == nil {
			t.Errorf("replaceInText(ranged multiple with allowMultiple=false) = nil, want error")
		}
		if _, _, err := replaceInText("a\nb\nc", "z", "Z", 2, 2, false); err == nil {
			t.Errorf("replaceInText(ranged missing) = nil, want error")
		}

		// Type conversion helpers: stringArg, intArg, boolArg, stringSliceArg.
		args := map[string]any{
			"num_str":   42,
			"i32":       int32(11),
			"i64":       int64(22),
			"f64":       float64(33),
			"bad_bool":  "not-a-bool",
			"str_slice": []string{"a", "b"},
			"any_slice": []any{"c", 7, nil},
		}
		if got := stringArg(args, "missing", "num_str"); got != "42" {
			t.Errorf("stringArg(num_str) = %q, want \"42\"", got)
		}
		if got := intArg(args, 9, "i32"); got != 11 {
			t.Errorf("intArg(i32) = %d, want 11", got)
		}
		if got := intArg(args, 9, "i64"); got != 22 {
			t.Errorf("intArg(i64) = %d, want 22", got)
		}
		if got := intArg(args, 9, "f64"); got != 33 {
			t.Errorf("intArg(f64) = %d, want 33", got)
		}
		if got := boolArg(args, true, "bad_bool"); !got {
			t.Errorf("boolArg(bad_bool, fallback=true) = false, want true")
		}
		if got := stringSliceArg(args, "str_slice"); !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("stringSliceArg(str_slice) = %v, want [a b]", got)
		}
		if got := stringSliceArg(args, "any_slice"); !slices.Equal(got, []string{"c", "7"}) {
			t.Errorf("stringSliceArg(any_slice) = %v, want [c 7]", got)
		}
	})
}

type fakeHTTPDoer func(*http.Request) (*http.Response, error)

func (f fakeHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type decodedClientEvent struct {
	seqNum          int64
	hasSeqNum       bool
	timestampMicros int64
	hasTimestamp    bool
	hasPayload      bool
	hasStep         bool
	hasToolResult   bool
	callID          string
	hasFnResult     bool
	fnName          string
	isError         bool
	result          any
}

func decodeClientEventWire(t *testing.T, data []byte) decodedClientEvent {
	t.Helper()
	var out decodedClientEvent
	for len(data) > 0 {
		num, wt, vv, vb, n, err := consumeField(data)
		if err != nil {
			t.Fatalf("consumeField(clientEvent) failed: %v", err)
		}
		data = data[n:]
		switch {
		case num == 3 && wt == wireBytes:
			out.hasPayload = true
			pData := vb
			for len(pData) > 0 {
				pnum, pwt, _, pvb, pn, perr := consumeField(pData)
				if perr != nil {
					t.Fatalf("consumeField(wirePayload) failed: %v", perr)
				}
				pData = pData[pn:]
				if pnum == 11 && pwt == wireBytes {
					out.hasStep = true
					sData := pvb
					for len(sData) > 0 {
						snum, swt, _, svb, sn, serr := consumeField(sData)
						if serr != nil {
							t.Fatalf("consumeField(wireStep) failed: %v", serr)
						}
						sData = sData[sn:]
						if snum == 5 && swt == wireBytes {
							out.hasToolResult = true
							trData := svb
							for len(trData) > 0 {
								tnum, twt, _, tvb, tn, terr := consumeField(trData)
								if terr != nil {
									t.Fatalf("consumeField(toolResultStep) failed: %v", terr)
								}
								trData = trData[tn:]
								switch {
								case tnum == 1 && twt == wireBytes:
									out.callID = string(tvb)
								case tnum == 3 && twt == wireBytes:
									out.hasFnResult = true
									frData := tvb
									for len(frData) > 0 {
										fnum, fwt, fvv, fvb, fn, ferr := consumeField(frData)
										if ferr != nil {
											t.Fatalf("consumeField(functionResultStep) failed: %v", ferr)
										}
										frData = frData[fn:]
										switch {
										case fnum == 2 && fwt == wireBytes:
											out.fnName = string(fvb)
										case fnum == 3 && fwt == wireVarint:
											out.isError = fvv != 0
										case fnum == 7 && fwt == wireBytes:
											v, derr := decodeProtoValue(fvb, 0)
											if derr != nil {
												t.Fatalf("decodeProtoValue(Result) failed: %v", derr)
											}
											out.result = v
										}
									}
								}
							}
						}
					}
				}
			}
		case num == 10 && wt == wireVarint:
			out.seqNum = int64(vv)
			out.hasSeqNum = true
		case num == 11 && wt == wireVarint:
			out.timestampMicros = int64(vv)
			out.hasTimestamp = true
		}
	}
	return out
}

func TestRuntimeClientBranchingHeadersAndStatusBoundaries(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GEMINI_ACCESS_TOKEN", "")
	t.Setenv("GEMINI_NO_KEYRING", "true")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	config.Reset()
	t.Cleanup(config.Reset)

	t.Run("default_server_url_stream_client_timeout_and_credential_completeness", func(t *testing.T) {
		t.Setenv("GEMINI_TIMEOUT", "7s")
		config.Reset()
		if err := config.Init("gemini-api-cli", "GEMINI"); err != nil {
			t.Fatalf("config.Init() unexpected error: %v", err)
		}

		// Default --server-url falls back to sdk.ServerList[0] ("https://generativelanguage.googleapis.com")
		// and --debug=true without --dry-run keeps streamClient distinct from DebugClient-wrapped httpClient.
		defaultCmd := newTestRuntimeCmd(t, "")
		_ = defaultCmd.Root().PersistentFlags().Set("debug", "true")
		rtDefault, err := newRuntimeTransport(defaultCmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport(default serverURL) unexpected error: %v", err)
		}
		if rtDefault.baseURL != "https://generativelanguage.googleapis.com" || rtDefault.scheme != "https" {
			t.Errorf("default baseURL/scheme = (%q, %q), want (https://generativelanguage.googleapis.com, https)", rtDefault.baseURL, rtDefault.scheme)
		}
		if rtDefault.timeout != 7*time.Second {
			t.Errorf("rtDefault.timeout from GEMINI_TIMEOUT = %v, want 7s", rtDefault.timeout)
		}
		if rtDefault.streamClient == nil || rtDefault.streamClient == rtDefault.httpClient {
			t.Errorf("rtDefault.streamClient = %v, want non-nil unwrapped client distinct from debug httpClient", rtDefault.streamClient)
		}

		// --dry-run=true uses httpClient as streamClient so dry-run preview intercepts stream requests.
		dryCmd := newTestRuntimeCmd(t, "")
		_ = dryCmd.Root().PersistentFlags().Set("dry-run", "true")
		_ = dryCmd.Root().PersistentFlags().Set("timeout", "3s")
		// Explicit empty --api-key "" must be treated as incomplete so --access-token wins.
		_ = dryCmd.Root().PersistentFlags().Set("api-key", "")
		_ = dryCmd.Root().PersistentFlags().Set("access-token", "tok-complete")
		rtDry, err := newRuntimeTransport(dryCmd, false)
		if err != nil {
			t.Fatalf("newRuntimeTransport(dry-run) unexpected error: %v", err)
		}
		if rtDry.streamClient == nil || rtDry.streamClient != rtDry.httpClient {
			t.Errorf("rtDry.streamClient = %v, want equal to rtDry.httpClient (%v)", rtDry.streamClient, rtDry.httpClient)
		}
		if rtDry.timeout != 3*time.Second {
			t.Errorf("rtDry.timeout with explicit --timeout=3s = %v, want 3s", rtDry.timeout)
		}
		req, err := rtDry.newRequest(t.Context(), http.MethodPost, "/v1beta/check", http.NoBody)
		if err != nil {
			t.Fatalf("rtDry.newRequest() unexpected error: %v", err)
		}
		if gotUA := req.Header.Get("User-Agent"); gotUA != rawUserAgent || gotUA == "" {
			t.Errorf("User-Agent = %q, want non-empty %q", gotUA, rawUserAgent)
		}
		if gotAuth := req.Header.Get("Authorization"); gotAuth != "Bearer tok-complete" {
			t.Errorf("Authorization with empty --api-key = %q, want \"Bearer tok-complete\"", gotAuth)
		}
	})

	t.Run("lease_and_heartbeat_headers_empty_body_and_status_boundaries", func(t *testing.T) {
		var gotHeaders http.Header
		var statusCode atomic.Int32
		var respPayload atomic.Value
		statusCode.Store(http.StatusOK)
		respPayload.Store("")

		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			w.WriteHeader(int(statusCode.Load()))
			if body, _ := respPayload.Load().(string); body != "" {
				_, _ = w.Write([]byte(body))
			}
		}))

		cmd := newTestRuntimeCmd(t, srvURL)
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport() unexpected error: %v", err)
		}

		// Empty EnvironmentID and EnvironmentKey must NOT emit blank header keys.
		bareClient := NewHTTPClient(HTTPClientOptions{Transport: rt})
		leasesEmpty, err := bareClient.LeaseInteractions(t.Context(), "w1", 1)
		if err != nil || leasesEmpty != nil {
			t.Errorf("LeaseInteractions(200 empty body) = (%v, %v), want (nil, nil)", leasesEmpty, err)
		}
		if gotCT := gotHeaders.Get("Content-Type"); gotCT != "application/json" {
			t.Errorf("LeaseInteractions Content-Type = %q, want \"application/json\"", gotCT)
		}
		for _, absentHdr := range []string{"X-Goog-Environment-Key", "X-Goog-Environment-Id", "X-Goog-Lease-Token"} {
			if vals := gotHeaders.Values(absentHdr); len(vals) != 0 {
				t.Errorf("header %s = %v when unset, want absent", absentHdr, vals)
			}
		}

		// Populated EnvironmentID and EnvironmentKey must set exact headers.
		scopedClient := NewHTTPClient(HTTPClientOptions{
			Transport:      rt,
			EnvironmentID:  "env_hdr",
			EnvironmentKey: "ek_hdr",
		})
		respPayload.Store(`{"leases":[{"interaction_id":"i299","lease_token":"t299"}]}`)
		statusCode.Store(299)
		leases299, err := scopedClient.LeaseInteractions(t.Context(), "w1", 1)
		if err != nil || len(leases299) != 1 || leases299[0].InteractionID != "i299" {
			t.Errorf("LeaseInteractions(status=299) = (%v, %v), want 1 lease i299", leases299, err)
		}
		if gotHeaders.Get("X-Goog-Environment-Key") != "ek_hdr" || gotHeaders.Get("X-Goog-Environment-Id") != "env_hdr" {
			t.Errorf("scoped headers = %v, want x-goog-environment-key=ek_hdr and x-goog-environment-id=env_hdr", gotHeaders)
		}

		statusCode.Store(300)
		if got, err := scopedClient.LeaseInteractions(t.Context(), "w1", 1); err == nil {
			t.Errorf("LeaseInteractions(status=300) = %v, want non-2xx error", got)
		}
		rt199 := &runtimeTransport{
			baseURL:    "https://example.googleapis.com",
			scheme:     "https",
			apiVersion: "v1beta",
			headers:    make(map[string]string),
			httpClient: fakeHTTPDoer(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 199, Body: io.NopCloser(strings.NewReader("info"))}, nil
			}),
		}
		if got, err := NewHTTPClient(HTTPClientOptions{Transport: rt199}).LeaseInteractions(t.Context(), "w1", 1); err == nil {
			t.Errorf("LeaseInteractions(status=199) = %v, want non-2xx error", got)
		}

		// HeartbeatInteraction handles nil, non-matching, and empty-body (parsed == nil) responses cleanly.
		statusCode.Store(http.StatusOK)
		respPayload.Store(`{"leases":[null,{"interaction_id":"other_id"},{"interaction_id":"target_id"}]}`)
		hbMatch, err := scopedClient.HeartbeatInteraction(t.Context(), "p1", "target_id", "", "lt_1")
		if err != nil || !hbMatch.Renewed || hbMatch.ShouldCancel {
			t.Errorf("HeartbeatInteraction(mixed leases with match) = (%+v, %v), want Renewed=true", hbMatch, err)
		}
		respPayload.Store(`{"leases":[null,{"interaction_id":"other_id"}]}`)
		hbMiss, err := scopedClient.HeartbeatInteraction(t.Context(), "p1", "target_id", "", "lt_1")
		if err != nil || hbMiss.Renewed || !hbMiss.ShouldCancel {
			t.Errorf("HeartbeatInteraction(mixed leases without match) = (%+v, %v), want ShouldCancel=true", hbMiss, err)
		}
		respPayload.Store("")
		hbEmpty, err := scopedClient.HeartbeatInteraction(t.Context(), "p1", "target_id", "", "lt_1")
		if err != nil || hbEmpty.Renewed || !hbEmpty.ShouldCancel {
			t.Errorf("HeartbeatInteraction(empty 200 body) = (%+v, %v), want ShouldCancel=true", hbEmpty, err)
		}

		// Explicit --access-token flag (rank 0) outranks GEMINI_API_KEY env var (rank 1).
		t.Setenv("GEMINI_API_KEY", "env-api-key-rank1")
		rankCmd := newTestRuntimeCmd(t, srvURL)
		_ = rankCmd.Root().PersistentFlags().Set("access-token", "flag-oauth-rank0")
		rankRT, err := newRuntimeTransport(rankCmd, false)
		if err != nil {
			t.Fatalf("newRuntimeTransport(rank precedence) failed: %v", err)
		}
		rankReq, err := rankRT.newRequest(t.Context(), http.MethodPost, "/v1beta/test", http.NoBody)
		if err != nil {
			t.Fatalf("rankRT.newRequest() failed: %v", err)
		}
		if gotKey, gotAuth := rankReq.Header.Get("x-goog-api-key"), rankReq.Header.Get("Authorization"); gotKey != "" || gotAuth != "Bearer flag-oauth-rank0" {
			t.Errorf("headers with GEMINI_API_KEY + --access-token = (api-key=%q, auth=%q), want (\"\", \"Bearer flag-oauth-rank0\")", gotKey, gotAuth)
		}
	})

	t.Run("preview_connect_and_stream_close_headers_and_nil_safety", func(t *testing.T) {
		var capturedReq *http.Request
		bodyTracker := &trackingReadCloser{Reader: strings.NewReader("")}
		rt := &runtimeTransport{
			baseURL:    "https://example.googleapis.com",
			scheme:     "https",
			apiVersion: "v1beta",
			headers:    make(map[string]string),
			httpClient: fakeHTTPDoer(func(req *http.Request) (*http.Response, error) {
				capturedReq = req
				return &http.Response{StatusCode: http.StatusOK, Body: bodyTracker}, nil
			}),
		}
		c := NewHTTPClient(HTTPClientOptions{
			Transport:      rt,
			EnvironmentID:  "env_prev",
			EnvironmentKey: "ek_prev",
		})
		if err := c.PreviewConnect(t.Context(), "lt_prev"); err != nil {
			t.Fatalf("PreviewConnect() unexpected error: %v", err)
		}
		if !bodyTracker.closed {
			t.Errorf("PreviewConnect() did not close response body")
		}
		if capturedReq.Header.Get("Content-Type") != "application/grpc" ||
			capturedReq.Header.Get("TE") != "trailers" ||
			capturedReq.Header.Get("X-Goog-Environment-Id") != "env_prev" ||
			capturedReq.Header.Get("X-Goog-Environment-Key") != "ek_prev" ||
			capturedReq.Header.Get("X-Goog-Lease-Token") != "lt_prev" {
			t.Errorf("PreviewConnect headers = %v, want grpc/trailers and environment+lease headers", capturedReq.Header)
		}

		// Nil response and nil body branches in PreviewConnect.
		rt.httpClient = fakeHTTPDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: nil}, nil
		})
		if err := c.PreviewConnect(t.Context(), "lt_prev"); err != nil {
			t.Errorf("PreviewConnect(Body=nil) = %v, want nil", err)
		}
		rt.httpClient = fakeHTTPDoer(func(*http.Request) (*http.Response, error) {
			return nil, nil
		})
		if err := c.PreviewConnect(t.Context(), "lt_prev"); err != nil {
			t.Errorf("PreviewConnect(resp=nil) = %v, want nil", err)
		}

		// grpcInteractionStream.Close closes non-nil Body and tolerates nil Body / nil resp.
		_, pw1 := io.Pipe()
		streamTracker := &trackingReadCloser{Reader: strings.NewReader("")}
		s1 := &grpcInteractionStream{resp: &http.Response{Body: streamTracker}, pw: pw1}
		if err := s1.Close(); err != nil || !streamTracker.closed {
			t.Errorf("stream.Close(non-nil Body) = (%v, closed=%v), want (nil, true)", err, streamTracker.closed)
		}
		_, pw2 := io.Pipe()
		if err := (&grpcInteractionStream{resp: &http.Response{Body: nil}, pw: pw2}).Close(); err != nil {
			t.Errorf("stream.Close(Body=nil) = %v, want nil", err)
		}
		_, pw3 := io.Pipe()
		if err := (&grpcInteractionStream{resp: nil, pw: pw3}).Close(); err != nil {
			t.Errorf("stream.Close(resp=nil) = %v, want nil", err)
		}
	})

	t.Run("connect_stream_headers_debug_status_bounds_and_probe_conditions", func(t *testing.T) {
		var streamHeaders http.Header
		var streamStatus atomic.Int32
		streamStatus.Store(http.StatusOK)

		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			streamHeaders = r.Header.Clone()
			code := int(streamStatus.Load())
			if code < 200 || code >= 300 {
				w.WriteHeader(code)
				_, _ = w.Write([]byte("stream rejected"))
				return
			}
			w.Header().Set("Content-Type", "application/grpc")
			w.WriteHeader(code)
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			_, _ = grpcReadFrame(r.Body)
		}))

		// 1. --debug=true writes both stream request and stream response debug banners.
		dbgCmd := newTestRuntimeCmd(t, srvURL)
		var dbgErr bytes.Buffer
		dbgCmd.SetErr(&dbgErr)
		_ = dbgCmd.Root().PersistentFlags().Set("debug", "true")
		rtDbg, err := newRuntimeTransport(dbgCmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport(debug) failed: %v", err)
		}
		dbgClient := NewHTTPClient(HTTPClientOptions{Transport: rtDbg, EnvironmentID: "env_s", EnvironmentKey: "ek_s"})
		st, err := dbgClient.connectStream(t.Context(), connectStreamRequest{InteractionID: "i1", LeaseToken: "lt_s", NextOutputSeqNum: 1})
		if err != nil {
			t.Fatalf("connectStream(200) unexpected error: %v", err)
		}
		_ = st.Close()
		if streamHeaders.Get("Content-Type") != "application/grpc" || streamHeaders.Get("TE") != "trailers" {
			t.Errorf("connectStream headers = %v, want Content-Type=application/grpc and TE=trailers", streamHeaders)
		}
		if !strings.Contains(dbgErr.String(), "=== DEBUG STREAM ===") || !strings.Contains(dbgErr.String(), "=== DEBUG STREAM RESPONSE ===") {
			t.Errorf("debug stderr = %q, want both DEBUG STREAM banners", dbgErr.String())
		}

		// 2. --debug=false writes neither banner, and debug=true with errOut=nil does not panic.
		quietCmd := newTestRuntimeCmd(t, srvURL)
		var quietErr bytes.Buffer
		quietCmd.SetErr(&quietErr)
		rtQuiet, err := newRuntimeTransport(quietCmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport(quiet) failed: %v", err)
		}
		rtQuiet.errOut = &quietErr
		quietClient := NewHTTPClient(HTTPClientOptions{Transport: rtQuiet})
		streamStatus.Store(299)
		st299, err := quietClient.connectStream(t.Context(), connectStreamRequest{InteractionID: "i299", LeaseToken: "lt_299", NextOutputSeqNum: 1})
		if err != nil {
			t.Fatalf("connectStream(299) unexpected error: %v", err)
		}
		_ = st299.Close()
		if strings.Contains(quietErr.String(), "DEBUG STREAM") {
			t.Errorf("connectStream(debug=false) wrote debug output: %q", quietErr.String())
		}
		rtNilOut := *rtQuiet
		rtNilOut.debug = true
		rtNilOut.errOut = nil
		stNilOut, err := NewHTTPClient(HTTPClientOptions{Transport: &rtNilOut}).connectStream(t.Context(), connectStreamRequest{InteractionID: "inil", LeaseToken: "lt_nil", NextOutputSeqNum: 1})
		if err != nil {
			t.Fatalf("connectStream(debug=true, errOut=nil) unexpected error: %v", err)
		}
		_ = stNilOut.Close()

		// 3. Status boundary 199 and 300 fail connectStream.
		streamStatus.Store(300)
		if _, err := quietClient.connectStream(t.Context(), connectStreamRequest{InteractionID: "ibad", LeaseToken: "lt_bad", NextOutputSeqNum: 1}); err == nil {
			t.Errorf("connectStream(status=300) = nil error, want non-2xx error")
		}
		rtStream199 := &runtimeTransport{
			baseURL:    "https://example.googleapis.com",
			scheme:     "https",
			apiVersion: "v1beta",
			headers:    make(map[string]string),
			streamClient: fakeHTTPDoer(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 199, Body: io.NopCloser(strings.NewReader("info"))}, nil
			}),
		}
		if _, err := NewHTTPClient(HTTPClientOptions{Transport: rtStream199}).connectStream(t.Context(), connectStreamRequest{InteractionID: "i199", LeaseToken: "lt_199", NextOutputSeqNum: 1}); err == nil {
			t.Errorf("connectStream(status=199) = nil error, want non-2xx error")
		}

		// 4. probePlainHTTPError headers, canceled-context skip, and https-scheme skip.
		var probeHeaders http.Header
		var http1Hits atomic.Int32
		http1Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http1Hits.Add(1)
			probeHeaders = r.Header.Clone()
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}))
		defer http1Srv.Close()

		probeCmd := newTestRuntimeCmd(t, http1Srv.URL)
		rtProbe, err := newRuntimeTransport(probeCmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport(http1) failed: %v", err)
		}
		probeClient := NewHTTPClient(HTTPClientOptions{
			Transport:      rtProbe,
			EnvironmentID:  "env_probe",
			EnvironmentKey: "ek_probe",
		})
		if _, err := probeClient.connectStream(t.Context(), connectStreamRequest{InteractionID: "iprobe", LeaseToken: "lt_probe", NextOutputSeqNum: 1}); err == nil || !strings.Contains(err.Error(), "Status 502") {
			t.Errorf("connectStream(HTTP/1.1 502) = %v, want Status 502", err)
		}
		if probeHeaders.Get("Content-Type") != "application/grpc" ||
			probeHeaders.Get("X-Goog-Environment-Id") != "env_probe" ||
			probeHeaders.Get("X-Goog-Environment-Key") != "ek_probe" ||
			probeHeaders.Get("X-Goog-Lease-Token") != "lt_probe" {
			t.Errorf("probePlainHTTPError headers = %v, want grpc + environment/lease headers", probeHeaders)
		}

		// When ctx is already canceled, connectStream on http:// must NOT fire probePlainHTTPError.
		var canceledProbeHits atomic.Int32
		rtHTTPCanceled := &runtimeTransport{
			baseURL:    "http://127.0.0.1:8080",
			scheme:     "http",
			apiVersion: "v1beta",
			headers:    make(map[string]string),
			httpClient: fakeHTTPDoer(func(*http.Request) (*http.Response, error) {
				canceledProbeHits.Add(1)
				return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("probe hit"))}, nil
			}),
			streamClient: fakeHTTPDoer(func(*http.Request) (*http.Response, error) {
				return nil, context.Canceled
			}),
		}
		canceledCtx, cancelNow := context.WithCancel(t.Context())
		cancelNow()
		_, cancelErr := NewHTTPClient(HTTPClientOptions{Transport: rtHTTPCanceled}).connectStream(canceledCtx, connectStreamRequest{InteractionID: "icancel", LeaseToken: "lt_c", NextOutputSeqNum: 1})
		if !errors.Is(cancelErr, context.Canceled) || canceledProbeHits.Load() != 0 {
			t.Errorf("connectStream(canceled ctx) = (%v, probeHits=%d), want (context.Canceled, 0)", cancelErr, canceledProbeHits.Load())
		}

		// When scheme is https, streamClient failure must NOT call httpClient probe.
		var httpsProbeHits atomic.Int32
		rtHTTPSFail := &runtimeTransport{
			baseURL:    "https://example.googleapis.com",
			scheme:     "https",
			apiVersion: "v1beta",
			headers:    make(map[string]string),
			httpClient: fakeHTTPDoer(func(*http.Request) (*http.Response, error) {
				httpsProbeHits.Add(1)
				return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("nope"))}, nil
			}),
			streamClient: fakeHTTPDoer(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("tls dial failed")
			}),
		}
		_, _ = NewHTTPClient(HTTPClientOptions{Transport: rtHTTPSFail}).connectStream(t.Context(), connectStreamRequest{InteractionID: "ihttps", LeaseToken: "lt_h", NextOutputSeqNum: 1})
		if got := httpsProbeHits.Load(); got != 0 {
			t.Errorf("https stream failure called probePlainHTTPError %d times, want 0", got)
		}
	})

	t.Run("retry_backoff_shift_and_cap_progression", func(t *testing.T) {
		cases := []struct {
			base        time.Duration
			consecutive int
			want        time.Duration
		}{
			{base: 100 * time.Millisecond, consecutive: -1, want: 100 * time.Millisecond},
			{base: 100 * time.Millisecond, consecutive: 0, want: 100 * time.Millisecond},
			{base: 100 * time.Millisecond, consecutive: 1, want: 100 * time.Millisecond},
			{base: 100 * time.Millisecond, consecutive: 2, want: 200 * time.Millisecond},
			{base: 100 * time.Millisecond, consecutive: 3, want: 400 * time.Millisecond},
			{base: 100 * time.Millisecond, consecutive: 6, want: 3200 * time.Millisecond},
			{base: 100 * time.Millisecond, consecutive: 7, want: 3200 * time.Millisecond},
			{base: 2 * time.Second, consecutive: 6, want: 30 * time.Second},
		}
		for _, tc := range cases {
			if got := retryBackoff(tc.base, tc.consecutive); got != tc.want {
				t.Errorf("retryBackoff(%v, %d) = %v, want %v", tc.base, tc.consecutive, got, tc.want)
			}
		}
	})
}

func TestPollerLoopRetryBackoffDeduplicationAndRecovery(t *testing.T) {
	t.Run("empty_lease_token_stderr_not_corrupted_by_redactor", func(t *testing.T) {
		prov := NewCommandWorkerProvisioner("printf 'plain boom' >&2; exit 2")
		err := prov.ProvisionAndDispatch(t.Context(), ProvisionInput{
			InteractionID: "int_empty_tok",
			EnvironmentID: "env_1",
			LeaseToken:    "",
		})
		if err == nil || !strings.Contains(err.Error(), "plain boom") || strings.Contains(err.Error(), "[REDACTED]") {
			t.Errorf("ProvisionAndDispatch(empty LeaseToken) = %v, want 'plain boom' without '[REDACTED]'", err)
		}
	})

	t.Run("non_retryable_401_aborts_even_when_retries_enabled", func(t *testing.T) {
		var calls atomic.Int32
		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, `{"error":{"code":401,"message":"unauthorized"}}`, http.StatusUnauthorized)
		}))
		cmd := newTestRuntimeCmd(t, srvURL)
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport() failed: %v", err)
		}
		poller := NewPollerController(NewHTTPClient(HTTPClientOptions{Transport: rt, EnvironmentKey: "ek_bad"}), NewCommandWorkerProvisioner("true"), PollerOptions{
			PollerID:     "p-401",
			PollInterval: 20 * time.Millisecond,
			NoRetries:    false,
			Logger:       log.New(io.Discard, "", 0),
		})
		if err := poller.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "Status 401") {
			t.Errorf("PollerController.Run(401) = %v, want Status 401", err)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("401 call count = %d, want 1 (no retries on 401)", got)
		}
	})

	t.Run("retry_backoff_reset_single_start_log_and_idle_sleep", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var step atomic.Int32
		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch step.Add(1) {
			case 1, 2:
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			case 3, 4:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"leases":[]}`))
			case 5:
				http.Error(w, "unavailable again", http.StatusServiceUnavailable)
			default:
				cancel()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"leases":[]}`))
			}
		}))

		cmd := newTestRuntimeCmd(t, srvURL)
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport() failed: %v", err)
		}
		var logBuf bytes.Buffer
		poller := NewPollerController(NewHTTPClient(HTTPClientOptions{Transport: rt, EnvironmentKey: "ek_ok"}), NewCommandWorkerProvisioner("true"), PollerOptions{
			PollerID:        "p-backoff",
			MaxInteractions: 2,
			PollInterval:    20 * time.Millisecond,
			NoRetries:       false,
			Logger:          log.New(&logBuf, "", 0),
		})
		_ = poller.Run(ctx)

		logs := logBuf.String()
		if gotStarts := strings.Count(logs, "PollerController started"); gotStarts != 1 {
			t.Errorf("start log count = %d, want exactly 1 in logs:\n%s", gotStarts, logs)
		}
		if got20 := strings.Count(logs, "(retrying in 20ms)"); got20 != 2 {
			t.Errorf("'(retrying in 20ms)' count = %d, want 2 (initial error + reset after success) in logs:\n%s", got20, logs)
		}
		if got40 := strings.Count(logs, "(retrying in 40ms)"); got40 != 1 {
			t.Errorf("'(retrying in 40ms)' count = %d, want 1 (2nd consecutive error) in logs:\n%s", got40, logs)
		}

		// Verify empty lease responses sleep PollInterval instead of busy-spinning.
		var idlePolls atomic.Int32
		idleSrv := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			idlePolls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"leases":[]}`))
		}))
		idleCmd := newTestRuntimeCmd(t, idleSrv)
		idleRT, err := newRuntimeTransport(idleCmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport(idle) failed: %v", err)
		}
		idleCtx, idleCancel := context.WithTimeout(t.Context(), 45*time.Millisecond)
		defer idleCancel()
		idlePoller := NewPollerController(NewHTTPClient(HTTPClientOptions{Transport: idleRT, EnvironmentKey: "ek_idle"}), NewCommandWorkerProvisioner("true"), PollerOptions{
			PollerID:     "p-idle",
			PollInterval: 120 * time.Millisecond,
			Logger:       log.New(io.Discard, "", 0),
		})
		_ = idlePoller.Run(idleCtx)
		if got := idlePolls.Load(); got != 1 {
			t.Errorf("idle poll count in 45ms with 120ms PollInterval = %d, want 1", got)
		}
	})

	t.Run("in_flight_deduplication_and_failure_state_recovery", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		markerFile := filepath.Join(t.TempDir(), "attempt_marker")
		countFile := filepath.Join(t.TempDir(), "dup_count")
		round2File := filepath.Join(t.TempDir(), "round2_seen")
		t.Setenv("TEST_MARKER_FILE", markerFile)
		t.Setenv("TEST_DUP_COUNT_FILE", countFile)
		t.Setenv("TEST_ROUND2_FILE", round2File)

		var poller *PollerController
		var mu sync.Mutex
		sawFailureRecorded := false
		recoverySent := false
		var pollRound atomic.Int32
		var round1MaxNew, round2MaxNew atomic.Int32

		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req leaseInteractionsRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			if len(req.ActiveLeases) > 0 {
				_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{
					Leases: []*LeasedInteraction{{InteractionID: req.ActiveLeases[0].InteractionID, LeaseToken: req.ActiveLeases[0].LeaseToken}},
				})
				return
			}
			switch pollRound.Add(1) {
			case 1:
				if req.MaxNewInteractions != nil {
					round1MaxNew.Store(int32(*req.MaxNewInteractions))
				}
				// Duplicate dup_1 entries in the same response + nil/empty entries: must dispatch dup_1 only ONCE!
				_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{
					Leases: []*LeasedInteraction{
						nil,
						{InteractionID: ""},
						{InteractionID: "dup_1", EnvironmentID: "env_d", LeaseToken: "tok_d"},
						{InteractionID: "dup_1", EnvironmentID: "env_d", LeaseToken: "tok_d"},
					},
				})
			case 2:
				if req.MaxNewInteractions != nil {
					round2MaxNew.Store(int32(*req.MaxNewInteractions))
				}
				_ = os.WriteFile(round2File, []byte("ok"), 0o600)
				// Return int_flaky which fails on its first attempt.
				_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{
					Leases: []*LeasedInteraction{{InteractionID: "int_flaky", EnvironmentID: "env_f", LeaseToken: "tok_f"}},
				})
			default:
				poller.mu.Lock()
				st := poller.failures["int_flaky"]
				if st != nil {
					mu.Lock()
					sawFailureRecorded = true
					mu.Unlock()
					// Fast-forward cooldown so recovery attempt runs without waiting 500ms.
					st.cooldownEnd = time.Now()
				}
				poller.mu.Unlock()
				mu.Lock()
				sendRecovery := sawFailureRecorded && !recoverySent
				if sendRecovery {
					recoverySent = true
				}
				mu.Unlock()
				if sendRecovery {
					_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{
						Leases: []*LeasedInteraction{{InteractionID: "int_flaky", EnvironmentID: "env_f", LeaseToken: "tok_f"}},
					})
					return
				}
				_ = json.NewEncoder(w).Encode(leaseInteractionsResponse{})
			}
		}))

		// Script:
		// - For dup_1: immediately append "1" to TEST_DUP_COUNT_FILE and stay in-flight until round 2 arrives.
		// - For int_flaky: fail on first invocation (creating TEST_MARKER_FILE), succeed on second invocation (creating TEST_MARKER_FILE.ok).
		provScript := `input=$(cat); case "$input" in *dup_1*) printf '1' >> "$TEST_DUP_COUNT_FILE"; i=0; while [ ! -f "$TEST_ROUND2_FILE" ] && [ "$i" -lt 100 ]; do sleep 0.01; i=$((i+1)); done ;; *int_flaky*) if [ ! -f "$TEST_MARKER_FILE" ]; then touch "$TEST_MARKER_FILE"; exit 1; fi; touch "$TEST_MARKER_FILE.ok" ;; esac`

		cmd := newTestRuntimeCmd(t, srvURL)
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport() failed: %v", err)
		}
		var logBuf bytes.Buffer
		poller = NewPollerController(NewHTTPClient(HTTPClientOptions{Transport: rt, EnvironmentKey: "ek_1"}), NewCommandWorkerProvisioner(provScript), PollerOptions{
			PollerID:        "p-dedup",
			MaxInteractions: 4,
			PollInterval:    20 * time.Millisecond,
			Logger:          log.New(&logBuf, "", 0),
		})

		done := make(chan error, 1)
		go func() {
			done <- poller.Run(ctx)
		}()

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			_, okErr := os.Stat(markerFile + ".ok")
			_, dupErr := os.Stat(countFile)
			if okErr == nil && dupErr == nil {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		// Give dispatchLease's post-provision cleanup a brief moment to clear failures and log success.
		deadline2 := time.Now().Add(time.Second)
		for time.Now().Before(deadline2) {
			poller.mu.Lock()
			_, stillFailed := poller.failures["int_flaky"]
			poller.mu.Unlock()
			if !stillFailed && strings.Contains(logBuf.String(), "[int_flaky] Dispatched interaction lease") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done

		dupBytes, err := os.ReadFile(countFile)
		if err != nil || string(dupBytes) != "1" {
			t.Errorf("dup_1 execution count bytes = (%q, %v), want \"1\" (deduplicated)", dupBytes, err)
		}
		if r1, r2 := round1MaxNew.Load(), round2MaxNew.Load(); r1 != 4 || r2 != 3 {
			t.Errorf("max_new_interactions across polls = (round1=%d, round2=%d), want (4, 3) with 1 in-flight slot", r1, r2)
		}
		mu.Lock()
		gotSawFailure := sawFailureRecorded
		mu.Unlock()
		if !gotSawFailure {
			t.Errorf("poller.failures[int_flaky] was never recorded on failed dispatch")
		}
		poller.mu.Lock()
		_, residualFailure := poller.failures["int_flaky"]
		poller.mu.Unlock()
		if residualFailure {
			t.Errorf("poller.failures[int_flaky] was not cleared after successful recovery dispatch")
		}
		if !strings.Contains(logBuf.String(), "[int_flaky] ProvisionAndDispatch failed") || !strings.Contains(logBuf.String(), "[int_flaky] Dispatched interaction lease") {
			t.Errorf("poller logs missing failure or recovery log lines:\n%s", logBuf.String())
		}
	})
}

func TestSandboxSymlinkBoundariesAndShellExecution(t *testing.T) {
	t.Run("whitespace_root_symlink_sibling_escape_and_symlink_workdir", func(t *testing.T) {
		evalBase, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("EvalSymlinks(TempDir) failed: %v", err)
		}
		d1d2 := filepath.Join(evalBase, "d1", "d2")
		siblingD2 := filepath.Join(evalBase, "d2")
		subInside := filepath.Join(d1d2, "real_work")
		for _, dir := range []string{d1d2, siblingD2, subInside} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("MkdirAll(%q) failed: %v", dir, err)
			}
		}
		symRoot := filepath.Join(evalBase, "sym_d1d2")
		if err := os.Symlink(d1d2, symRoot); err != nil {
			t.Fatalf("Symlink(symRoot) failed: %v", err)
		}
		symWorkInside := filepath.Join(d1d2, "sym_work")
		if err := os.Symlink(subInside, symWorkInside); err != nil {
			t.Fatalf("Symlink(symWorkInside) failed: %v", err)
		}

		// Sibling escape: filepath.Rel(evalBase/sym_d1d2, evalBase/d2) == "../d2", which if joined onto
		// evalBase/d1/d2 without checking isWithinRoot(origAbsRoot, absWork) would falsely resolve to d1/d2!
		if gotR, gotW, err := resolveSandboxDirs(symRoot, siblingD2, false); err == nil {
			t.Errorf("resolveSandboxDirs(symRoot=%q, escaping sibling=%q) = (%q, %q, nil), want escape error", symRoot, siblingD2, gotR, gotW)
		}

		// Symlinked workDir inside rootDir must canonicalize to its real target path.
		gotRoot, gotWork, err := resolveSandboxDirs(d1d2, symWorkInside, false)
		if err != nil || gotRoot != d1d2 || gotWork != subInside {
			t.Errorf("resolveSandboxDirs(symlinked workDir) = (%q, %q, %v), want (%q, %q, nil)", gotRoot, gotWork, err, d1d2, subInside)
		}

		// Whitespace-only rootDir ("   ") and workDir ("   ") normalize to cwd (".").
		t.Chdir(subInside)
		wsRoot, wsWork, err := resolveSandboxDirs("   ", "   ", false)
		if err != nil || wsRoot != subInside || wsWork != subInside {
			t.Errorf("resolveSandboxDirs(\"   \", \"   \") = (%q, %q, %v), want (%q, %q, nil)", wsRoot, wsWork, err, subInside, subInside)
		}

		// newFSServer canonicalizes symlinked rootDir/workDir and treats empty/whitespace workDir as rootDir even when cwd is a child of rootDir.
		fsSym := newFSServer(symRoot, filepath.Join(symRoot, "real_work"))
		if fsSym.rootDir != d1d2 || fsSym.workDir != subInside {
			t.Errorf("newFSServer(symRoot, symWork) = (%q, %q), want (%q, %q)", fsSym.rootDir, fsSym.workDir, d1d2, subInside)
		}
		for _, blankWork := range []string{"", "   "} {
			fsBlank := newFSServer(d1d2, blankWork)
			if fsBlank.workDir != d1d2 {
				t.Errorf("newFSServer(d1d2, %q).workDir = %q (with cwd=%q), want %q", blankWork, fsBlank.workDir, subInside, d1d2)
			}
		}
		if gotEmptyRel, err := fsSym.ResolvePath(""); err != nil || gotEmptyRel != subInside {
			t.Errorf("ResolvePath(\"\") = (%q, %v), want (%q, nil)", gotEmptyRel, err, subInside)
		}
	})

	t.Run("exact_read_size_boundaries_and_list_entries_size", func(t *testing.T) {
		rootDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("EvalSymlinks(TempDir) failed: %v", err)
		}
		fs := newFSServer(rootDir, rootDir)

		// Exact maxReadFileBytes file must succeed without truncating the final byte.
		exactPayload := bytes.Repeat([]byte("z"), maxReadFileBytes)
		if err := os.WriteFile(filepath.Join(rootDir, "exact.bin"), exactPayload, 0o600); err != nil {
			t.Fatalf("WriteFile(exact.bin) failed: %v", err)
		}
		gotExact, err := fs.ReadRegularFile("exact.bin")
		if err != nil || len(gotExact) != maxReadFileBytes {
			t.Errorf("ReadRegularFile(exact maxReadFileBytes) len=%d err=%v, want len=%d nil", len(gotExact), err, maxReadFileBytes)
		}

		// Direct unit assertions on readLimitedAll for exact limit, +1 overflow, exact maxBytes+1 read cap, and read error.
		if got, exceeded, err := readLimitedAll(strings.NewReader("1234"), 4); err != nil || exceeded || string(got) != "1234" {
			t.Errorf("readLimitedAll(exact 4) = (%q, %v, %v), want (\"1234\", false, nil)", got, exceeded, err)
		}
		tr := &countingReader{r: strings.NewReader("1234567")}
		if _, exceeded, err := readLimitedAll(tr, 4); err != nil || !exceeded || tr.bytesRead != 5 {
			t.Errorf("readLimitedAll(7 with max=4) = (exceeded=%v, err=%v, bytesRead=%d), want (true, nil, 5)", exceeded, err, tr.bytesRead)
		}
		if _, _, err := readLimitedAll(errReader{err: errors.New("disk read failed")}, 4); err == nil {
			t.Errorf("readLimitedAll(errReader) = nil, want error")
		}

		// Exact ReadFileRange byte-budget boundary:
		// 2 lines of length (maxReadFileBytes/2 - 1) each -> totalBytes == (maxReadFileBytes/2)*2 == maxReadFileBytes (succeeds).
		// Adding 1 byte to line 2 -> totalBytes == maxReadFileBytes + 1 (fails).
		exactHalfLine := strings.Repeat("b", maxReadFileBytes/2-1)
		if err := os.WriteFile(filepath.Join(rootDir, "two_exact_halves.txt"), []byte(exactHalfLine+"\n"+exactHalfLine), 0o600); err != nil {
			t.Fatalf("WriteFile(two_exact_halves.txt) failed: %v", err)
		}
		if gotRangeExact, err := fs.ReadFileRange("two_exact_halves.txt", 1, 2); err != nil || len(gotRangeExact) != maxReadFileBytes-1 {
			t.Errorf("ReadFileRange(exact maxReadFileBytes budget) len=%d err=%v, want len=%d nil", len(gotRangeExact), err, maxReadFileBytes-1)
		}
		if err := os.WriteFile(filepath.Join(rootDir, "two_over_by_one.txt"), []byte(exactHalfLine+"\n"+exactHalfLine+"x"), 0o600); err != nil {
			t.Fatalf("WriteFile(two_over_by_one.txt) failed: %v", err)
		}
		if _, err := fs.ReadFileRange("two_over_by_one.txt", 1, 2); err == nil {
			t.Errorf("ReadFileRange(maxReadFileBytes+1 budget) = nil, want size error")
		}

		// Calling ReadFileRange with startLine=0, endLine=0 normalizes startLine=1 and formats "line range [1, 0]".
		halfLine := strings.Repeat("a", maxReadFileBytes/2)
		if err := os.WriteFile(filepath.Join(rootDir, "two_halves.txt"), []byte(halfLine+"\n"+halfLine+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile(two_halves.txt) failed: %v", err)
		}
		_, rangeErr := fs.ReadFileRange("two_halves.txt", 0, 0)
		if rangeErr == nil || !strings.Contains(rangeErr.Error(), "line range [1, 0]") {
			t.Errorf("ReadFileRange(two_halves.txt, 0, 0) = %v, want 'line range [1, 0]' size error", rangeErr)
		}

		// Multi-line ReadFileRange stops at endLine > 0, and stops cleanly at io.EOF when endLine == 0.
		if _, err := fs.WriteRegularFile("sub/five.txt", "1\n2\n3\n4\n5\n", true, false); err != nil {
			t.Fatalf("WriteRegularFile(sub/five.txt) failed: %v", err)
		}
		gotTwo, err := fs.ReadFileRange("sub/five.txt", 1, 2)
		if err != nil || gotTwo != "1\n2" {
			t.Errorf("ReadFileRange(1, 2) = (%q, %v), want (\"1\\n2\", nil)", gotTwo, err)
		}
		gotTailEOF, err := fs.ReadFileRange("sub/five.txt", 4, 0)
		if err != nil || gotTailEOF != "4\n5\n" {
			t.Errorf("ReadFileRange(4, 0 to EOF) = (%q, %v), want (\"4\\n5\\n\", nil)", gotTailEOF, err)
		}

		// ListEntries populates exact byte size for regular files.
		entries, err := fs.ListEntries("sub")
		if err != nil || len(entries) != 1 {
			t.Fatalf("ListEntries(sub) = (%v, %v), want 1 entry", entries, err)
		}
		entryMap, _ := entries[0].(map[string]any)
		if entryMap["name"] != "five.txt" || entryMap["is_dir"] != false || entryMap["size"] != 10 {
			t.Errorf("ListEntries(sub)[0] = %v, want name=five.txt is_dir=false size=10", entryMap)
		}
	})

	t.Run("capped_buffer_isolated_zero_remaining_and_partial_overflow", func(t *testing.T) {
		// Case 1: Exact fill (no overflow), followed by write when remaining == 0.
		cbZero := &cappedBuffer{maxBytes: 6}
		_, _ = cbZero.Write([]byte("123456"))
		if strings.Contains(cbZero.String(), "truncated") {
			t.Errorf("exact-capacity cappedBuffer marked truncated prematurely: %q", cbZero.String())
		}
		_, _ = cbZero.Write([]byte("!"))
		if want := "123456\n...[output truncated at 4 MiB]..."; cbZero.String() != want {
			t.Errorf("cbZero.String() = %q, want %q", cbZero.String(), want)
		}

		// Case 2: Single partial overflow write (len(p) > remaining) with no subsequent write.
		cbPartial := &cappedBuffer{maxBytes: 6}
		_, _ = cbPartial.Write([]byte("123456789"))
		if want := "123456\n...[output truncated at 4 MiB]..."; cbPartial.String() != want {
			t.Errorf("cbPartial.String() = %q, want %q", cbPartial.String(), want)
		}
	})

	t.Run("shell_server_workdir_stderr_and_process_group_kill", func(t *testing.T) {
		outerDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("EvalSymlinks(TempDir) failed: %v", err)
		}
		defaultWork := filepath.Join(outerDir, "default_work")
		overrideWork := filepath.Join(outerDir, "override_work")
		for _, d := range []string{defaultWork, overrideWork} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatalf("MkdirAll(%q) failed: %v", d, err)
			}
		}
		t.Chdir(outerDir)
		sh := newShellServer(defaultWork)

		outDefault, errOut, code, err := sh.ExecuteBash(t.Context(), "pwd && printf 'stderr-text' >&2", "", 5*time.Second)
		if err != nil || code != 0 || strings.TrimSpace(outDefault) != defaultWork || errOut != "stderr-text" {
			t.Errorf("ExecuteBash(default workDir) = (stdout=%q, stderr=%q, code=%d, err=%v), want (%q, \"stderr-text\", 0, nil)", outDefault, errOut, code, err, defaultWork)
		}

		outOverride, _, code, err := sh.ExecuteBash(t.Context(), "pwd", overrideWork, 5*time.Second)
		if err != nil || code != 0 || strings.TrimSpace(outOverride) != overrideWork {
			t.Errorf("ExecuteBash(override workDir) = (stdout=%q, code=%d, err=%v), want (%q, 0, nil)", outOverride, code, err, overrideWork)
		}

		start := time.Now()
		_, _, timeoutCode, timeoutErr := sh.ExecuteBash(t.Context(), "(sleep 4; echo child >&2) & sleep 4", "", 80*time.Millisecond)
		if timeoutErr == nil || timeoutCode != -1 || time.Since(start) > 1500*time.Millisecond {
			t.Errorf("ExecuteBash(process group timeout) = (code=%d, err=%v, elapsed=%v), want (-1, timeoutErr, <1.5s)", timeoutCode, timeoutErr, time.Since(start))
		}
	})
}

func TestWireEncodingFieldNumbersSortingAndDepthBoundaries(t *testing.T) {
	t.Run("client_event_and_batch_field_fidelity", func(t *testing.T) {
		zeroBatch := (&clientEventBatch{
			AckOutputSeqNum: 0,
			ReconnectInteraction: &reconnectInteractionEvent{
				InteractionID: "int_0",
			},
		}).marshalWire()
		for rest := zeroBatch; len(rest) > 0; {
			num, _, _, _, n, err := consumeField(rest)
			if err != nil {
				t.Fatalf("consumeField(zeroBatch) failed: %v", err)
			}
			if num == 2 {
				t.Errorf("clientEventBatch(AckOutputSeqNum=0) unexpectedly emitted tag 2")
			}
			rest = rest[n:]
		}

		errEv := (&clientEvent{
			SeqNum:          9,
			TimestampMicros: 1234567,
			Payload: &wirePayload{
				Step: &wireStep{
					ToolResult: &toolResultStep{
						CallID: "call_err_1",
						FunctionResult: &functionResultStep{
							Name:    "execute_bash",
							IsError: true,
							Result:  map[string]any{"error": "failed"},
						},
					},
				},
			},
		}).marshalWire()
		gotErrEv := decodeClientEventWire(t, errEv)
		if !gotErrEv.hasSeqNum || gotErrEv.seqNum != 9 ||
			!gotErrEv.hasTimestamp || gotErrEv.timestampMicros != 1234567 ||
			!gotErrEv.hasPayload || !gotErrEv.hasStep || !gotErrEv.hasToolResult ||
			gotErrEv.callID != "call_err_1" || !gotErrEv.hasFnResult ||
			gotErrEv.fnName != "execute_bash" || !gotErrEv.isError ||
			!reflect.DeepEqual(gotErrEv.result, map[string]any{"error": "failed"}) {
			t.Errorf("decodeClientEventWire(full error event) = %+v, want all fields populated", gotErrEv)
		}

		// Empty Step (ToolResult == nil), nil Step, and nil clientEvent batch element omit tags cleanly without panicking.
		emptyPayloadEv := (&clientEvent{
			SeqNum:          0,
			TimestampMicros: 0,
			Payload:         &wirePayload{Step: &wireStep{ToolResult: nil}},
		}).marshalWire()
		gotEmptyEv := decodeClientEventWire(t, emptyPayloadEv)
		if gotEmptyEv.hasSeqNum || gotEmptyEv.hasTimestamp || !gotEmptyEv.hasPayload || gotEmptyEv.hasStep || gotEmptyEv.hasToolResult {
			t.Errorf("decodeClientEventWire(empty step, zero seq/ts) = %+v, want only empty payload tag 3", gotEmptyEv)
		}

		nilStepEv := (&clientEvent{Payload: &wirePayload{Step: nil}}).marshalWire()
		gotNilStepEv := decodeClientEventWire(t, nilStepEv)
		if !gotNilStepEv.hasPayload || gotNilStepEv.hasStep || gotNilStepEv.hasToolResult {
			t.Errorf("decodeClientEventWire(nil Step) = %+v, want hasPayload=true, hasStep=false", gotNilStepEv)
		}
		if gotNilElem := (&clientEventBatch{Events: []*clientEvent{nil}}).marshalWire(); len(gotNilElem) != 0 {
			t.Errorf("clientEventBatch{Events:[nil]}.marshalWire() = %v, want empty", gotNilElem)
		}
		if gotEmptyStr := appendStringField(nil, 1, ""); len(gotEmptyStr) != 0 {
			t.Errorf("appendStringField(empty) = %v, want proto3 omission (empty slice)", gotEmptyStr)
		}
		if gotFalseBool := appendBoolField(nil, 1, false); len(gotFalseBool) != 0 {
			t.Errorf("appendBoolField(false) = %v, want proto3 omission (empty slice)", gotFalseBool)
		}
		for _, numIn := range []any{int32(7), int64(8), float64(9)} {
			decNum, err := decodeProtoValue(encodeProtoValue(numIn), 0)
			if err != nil || decNum == nil {
				t.Errorf("decodeProtoValue(encodeProtoValue(%T(%v))) = (%v, %v), want numeric float64", numIn, numIn, decNum, err)
			}
		}
		decStrList, err := decodeProtoValue(encodeProtoValue([]string{"s1", "s2"}), 0)
		if err != nil || !reflect.DeepEqual(decStrList, []any{"s1", "s2"}) {
			t.Errorf("decodeProtoValue([]string) = (%#v, %v), want []any{\"s1\", \"s2\"}", decStrList, err)
		}
		decFallbackStr, err := decodeProtoValue(encodeProtoValue(struct{ N int }{5}), 0)
		if err != nil || decFallbackStr != "{5}" {
			t.Errorf("decodeProtoValue(custom struct) = (%#v, %v), want \"{5}\"", decFallbackStr, err)
		}
	})

	t.Run("sorted_struct_keys_proto_lists_fixed_consumed_and_depth_boundaries", func(t *testing.T) {
		// Verify encodeProtoStruct sorts keys deterministically across repeated runs.
		unsorted := map[string]any{
			"k9": 1, "k2": 1, "k7": 1, "k0": 1, "k5": 1,
			"k8": 1, "k1": 1, "k4": 1, "k6": 1, "k3": 1,
		}
		wantOrder := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9"}
		for run := 0; run < 5; run++ {
			encoded := encodeProtoStruct(unsorted)
			var gotKeys []string
			for rest := encoded; len(rest) > 0; {
				num, wt, _, vb, n, err := consumeField(rest)
				if err != nil || num != 1 || wt != wireBytes {
					t.Fatalf("consumeField(struct entry) = (%d, %d, %v)", num, wt, err)
				}
				rest = rest[n:]
				enum, ewt, _, evb, _, eerr := consumeField(vb)
				if eerr != nil || enum != 1 || ewt != wireBytes {
					t.Fatalf("consumeField(entry key) = (%d, %d, %v)", enum, ewt, eerr)
				}
				gotKeys = append(gotKeys, string(evb))
			}
			if !slices.Equal(gotKeys, wantOrder) {
				t.Fatalf("encodeProtoStruct key order = %v, want sorted %v", gotKeys, wantOrder)
			}
		}

		// Verify []any elements, nested struct value, NullValue overwrite, and unknown/wrong-wire-type list tags.
		roundTrip, err := decodeProtoStruct(encodeProtoStruct(map[string]any{
			"any_list": []any{"a", float64(2), nil},
			"nested":   map[string]any{"k": "v"},
		}), 0)
		if err != nil {
			t.Fatalf("decodeProtoStruct() failed: %v", err)
		}
		if !reflect.DeepEqual(roundTrip["any_list"], []any{"a", float64(2), nil}) {
			t.Errorf("decoded any_list = %#v, want []any{\"a\", 2.0, nil}", roundTrip["any_list"])
		}
		if !reflect.DeepEqual(roundTrip["nested"], map[string]any{"k": "v"}) {
			t.Errorf("decoded nested = %#v, want map[string]any{\"k\": \"v\"}", roundTrip["nested"])
		}

		overwrittenNull, err := decodeProtoValue(append(encodeProtoValue("stale"), encodeProtoValue(nil)...), 0)
		if err != nil || overwrittenNull != nil {
			t.Errorf("decodeProtoValue(string then NullValue) = (%v, %v), want (nil, nil)", overwrittenNull, err)
		}

		var listWithExtraTag []byte
		listWithExtraTag = appendBytesField(listWithExtraTag, 1, encodeProtoValue("only"))
		listWithExtraTag = appendVarintField(listWithExtraTag, 1, 77)
		listWithExtraTag = appendVarintField(listWithExtraTag, 2, 77)
		listWithExtraTag = appendBytesField(listWithExtraTag, 3, []byte{0xff})
		gotList, err := decodeProtoList(listWithExtraTag, 0)
		if err != nil || !reflect.DeepEqual(gotList, []any{"only"}) {
			t.Errorf("decodeProtoList(with mismatched wireType + unknown tags) = (%#v, %v), want []any{\"only\"}", gotList, err)
		}

		// grpcWriteFrame skips second Write call when len(msg) == 0, and propagates payload Write error when len(msg) > 0.
		cwEmpty := &countingWriter{}
		if err := grpcWriteFrame(cwEmpty, nil); err != nil || cwEmpty.writes != 1 || !bytes.Equal(cwEmpty.buf.Bytes(), []byte{0, 0, 0, 0, 0}) {
			t.Errorf("grpcWriteFrame(empty) = (err=%v, writes=%d, bytes=%v), want (nil, 1, [0 0 0 0 0])", err, cwEmpty.writes, cwEmpty.buf.Bytes())
		}
		cwFailPayload := &countingWriter{failAtWrite: 2}
		if err := grpcWriteFrame(cwFailPayload, []byte{0x42}); err == nil || cwFailPayload.writes != 2 {
			t.Errorf("grpcWriteFrame(fail payload write) = (err=%v, writes=%d), want (error, 2)", err, cwFailPayload.writes)
		}

		// consumeField consumed byte count for wireFixed32 (1-byte tag + 4-byte uint32 = 5).
		fixed32WithTrailing := append(binary.LittleEndian.AppendUint32(appendTag(nil, 3, wireFixed32), 0xabcdef01), appendVarintField(nil, 4, 9)...)
		_, _, vv32, _, consumed32, err := consumeField(fixed32WithTrailing)
		if err != nil || vv32 != 0xabcdef01 || consumed32 != 5 {
			t.Errorf("consumeField(wireFixed32) = (vv=%#x, consumed=%d, err=%v), want (0xabcdef01, 5, nil)", vv32, consumed32, err)
		}

		// Verify strict (num == X && wt == wireY) matching across every wire decoder when unknown tags or mismatched wire types trail valid fields.
		var recWire []byte
		recWire = appendVarintField(recWire, 2, 13)
		recWire = appendBytesField(recWire, 2, []byte("wrong_wt"))
		recWire = appendVarintField(recWire, 99, 88)
		rec, err := unmarshalInteractionReconnected(recWire)
		if err != nil || rec.NextInputSeqNum != 13 {
			t.Errorf("unmarshalInteractionReconnected(strict tag/wt) = (%+v, %v), want NextInputSeqNum=13", rec, err)
		}

		var yieldWire []byte
		yieldWire = appendVarintField(yieldWire, 1, 42)
		yieldWire = appendBytesField(yieldWire, 1, []byte("wrong_wt"))
		yieldWire = appendVarintField(yieldWire, 99, 7)
		yieldParsed, err := unmarshalYield(yieldWire)
		if err != nil || yieldParsed.NextInputSeqNum != 42 {
			t.Errorf("unmarshalYield(strict tag/wt) = (%+v, %v), want NextInputSeqNum=42", yieldParsed, err)
		}

		var fnCallWire []byte
		fnCallWire = appendStringField(fnCallWire, 1, "view_file")
		fnCallWire = appendBytesField(fnCallWire, 2, encodeProtoStruct(map[string]any{"p": "x"}))
		fnCallWire = appendVarintField(fnCallWire, 1, 9)
		fnCallWire = appendVarintField(fnCallWire, 2, 9)
		fnCallWire = appendBytesField(fnCallWire, 99, []byte{0xff})
		fnCallParsed, err := unmarshalFunctionCallStep(fnCallWire)
		if err != nil || fnCallParsed.Name != "view_file" || fnCallParsed.Arguments["p"] != "x" {
			t.Errorf("unmarshalFunctionCallStep(strict tag/wt) = (%+v, %v), want Name=view_file, p=x", fnCallParsed, err)
		}

		var tcWire []byte
		tcWire = appendStringField(tcWire, 1, "cid_strict")
		tcWire = appendBytesField(tcWire, 3, fnCallWire)
		tcWire = appendVarintField(tcWire, 1, 9)
		tcWire = appendVarintField(tcWire, 3, 9)
		tcWire = appendBytesField(tcWire, 99, []byte{0xff})
		tcParsed, err := unmarshalToolCallStep(tcWire)
		if err != nil || tcParsed.ID != "cid_strict" || tcParsed.FunctionCall == nil || tcParsed.FunctionCall.Name != "view_file" {
			t.Errorf("unmarshalToolCallStep(strict tag/wt) = (%+v, %v), want ID=cid_strict, Name=view_file", tcParsed, err)
		}

		var stepWire []byte
		stepWire = appendBytesField(stepWire, 4, tcWire)
		stepWire = appendVarintField(stepWire, 4, 9)
		stepWire = appendBytesField(stepWire, 99, []byte{0xff})
		stepParsed, err := unmarshalStep(stepWire)
		if err != nil || stepParsed.ToolCall == nil || stepParsed.ToolCall.ID != "cid_strict" {
			t.Errorf("unmarshalStep(strict tag/wt) = (%+v, %v), want ToolCall.ID=cid_strict", stepParsed, err)
		}

		var payloadWire []byte
		payloadWire = appendBytesField(payloadWire, 11, stepWire)
		payloadWire = appendVarintField(payloadWire, 11, 9)
		payloadWire = appendBytesField(payloadWire, 99, []byte{0xff})
		payloadParsed, err := unmarshalPayload(payloadWire)
		if err != nil || payloadParsed.Step == nil || payloadParsed.Step.ToolCall == nil || payloadParsed.Step.ToolCall.ID != "cid_strict" {
			t.Errorf("unmarshalPayload(strict tag/wt) = (%+v, %v), want Step.ToolCall.ID=cid_strict", payloadParsed, err)
		}

		var sevWire []byte
		sevWire = appendBytesField(sevWire, 3, payloadWire)
		sevWire = appendVarintField(sevWire, 10, 29)
		sevWire = appendVarintField(sevWire, 3, 9)
		sevWire = appendBytesField(sevWire, 10, []byte("wrong_wt"))
		sevWire = appendVarintField(sevWire, 99, 77)
		sevWire = appendBytesField(sevWire, 98, []byte{0xff})
		sevParsed, err := unmarshalServerEvent(sevWire)
		if err != nil || sevParsed.SeqNum != 29 || sevParsed.Payload == nil || sevParsed.Payload.Step == nil {
			t.Errorf("unmarshalServerEvent(strict tag/wt) = (%+v, %v), want SeqNum=29 and Step!=nil", sevParsed, err)
		}

		var batchWire []byte
		batchWire = appendBytesField(batchWire, 1, sevWire)
		batchWire = appendVarintField(batchWire, 2, 31)
		batchWire = appendBytesField(batchWire, 8, recWire)
		batchWire = appendBytesField(batchWire, 10, yieldWire)
		batchWire = appendVarintField(batchWire, 1, 9)
		batchWire = appendBytesField(batchWire, 2, []byte("wrong_wt"))
		batchWire = appendVarintField(batchWire, 6, 9)
		batchWire = appendVarintField(batchWire, 8, 9)
		batchWire = appendVarintField(batchWire, 9, 9)
		batchWire = appendVarintField(batchWire, 10, 9)
		batchWire = appendVarintField(batchWire, 99, 77)
		batchWire = appendBytesField(batchWire, 98, []byte{0xff})
		batch, err := unmarshalServerEventBatch(batchWire)
		if err != nil || len(batch.Events) != 1 || batch.AckInputSeqNum != 31 ||
			batch.InteractionCompleted != nil || batch.ReconnectionRequired != nil ||
			batch.InteractionReconnected == nil || batch.InteractionReconnected.NextInputSeqNum != 13 ||
			batch.Yield == nil || batch.Yield.NextInputSeqNum != 42 {
			t.Errorf("unmarshalServerEventBatch(strict tag/wt) = (%+v, %v), want exact fields preserved", batch, err)
		}

		var mapEntry []byte
		mapEntry = appendStringField(mapEntry, 1, "k1")
		mapEntry = appendBytesField(mapEntry, 2, encodeProtoValue("v1"))
		mapEntry = appendVarintField(mapEntry, 1, 9)
		mapEntry = appendVarintField(mapEntry, 2, 9)
		mapEntry = appendBytesField(mapEntry, 99, []byte{0xff})
		var mapWire []byte
		mapWire = appendBytesField(mapWire, 1, mapEntry)
		mapWire = appendVarintField(mapWire, 1, 5)
		mapWire = appendBytesField(mapWire, 99, []byte{0xff})
		strictStruct, err := decodeProtoStruct(mapWire, 0)
		if err != nil || len(strictStruct) != 1 || strictStruct["k1"] != "v1" {
			t.Errorf("decodeProtoStruct(strict tag/wt) = (%#v, %v), want map[k1:v1]", strictStruct, err)
		}

		var valWire []byte
		valWire = append(valWire, encodeProtoValue("keep")...)
		valWire = appendBytesField(valWire, 1, []byte("x"))
		valWire = appendVarintField(valWire, 2, 1)
		valWire = appendVarintField(valWire, 3, 1)
		valWire = appendBytesField(valWire, 4, []byte("x"))
		valWire = appendVarintField(valWire, 5, 1)
		valWire = appendVarintField(valWire, 6, 1)
		valWire = appendVarintField(valWire, 99, 1)
		valWire = binary.LittleEndian.AppendUint64(appendTag(valWire, 98, wireFixed64), math.Float64bits(9.5))
		valWire = appendBytesField(valWire, 97, []byte{0xff})
		gotStrictVal, err := decodeProtoValue(valWire, 0)
		if err != nil || gotStrictVal != "keep" {
			t.Errorf("decodeProtoValue(strict tag/wt) = (%#v, %v), want \"keep\"", gotStrictVal, err)
		}

		// Exact depth+1 boundary assertions at maxProtoNestingDepth.
		structWire := encodeProtoStruct(map[string]any{"k": "v"})
		if _, err := decodeProtoStruct(structWire, maxProtoNestingDepth-1); err != nil {
			t.Errorf("decodeProtoStruct(depth=max-1) unexpected error: %v", err)
		}
		if _, err := decodeProtoStruct(structWire, maxProtoNestingDepth); err == nil {
			t.Errorf("decodeProtoStruct(depth=max) = nil, want depth+1 overflow error")
		}

		nestedStructVal := encodeProtoValue(map[string]any{"k": "v"})
		if _, err := decodeProtoValue(nestedStructVal, maxProtoNestingDepth-2); err != nil {
			t.Errorf("decodeProtoValue(struct, depth=max-2) unexpected error: %v", err)
		}
		if _, err := decodeProtoValue(nestedStructVal, maxProtoNestingDepth-1); err == nil {
			t.Errorf("decodeProtoValue(struct, depth=max-1) = nil, want inner decodeProtoStruct(depth=max) overflow error")
		}
		if _, err := decodeProtoValue(nestedStructVal, maxProtoNestingDepth); err == nil {
			t.Errorf("decodeProtoValue(struct, depth=max) = nil, want depth+1 overflow error")
		}

		nestedListVal := encodeProtoValue([]any{"item"})
		if _, err := decodeProtoValue(nestedListVal, maxProtoNestingDepth-2); err != nil {
			t.Errorf("decodeProtoValue(list, depth=max-2) unexpected error: %v", err)
		}
		if _, err := decodeProtoValue(nestedListVal, maxProtoNestingDepth-1); err == nil {
			t.Errorf("decodeProtoValue(list, depth=max-1) = nil, want inner decodeProtoList(depth=max) overflow error")
		}
		if _, err := decodeProtoValue(nestedListVal, maxProtoNestingDepth); err == nil {
			t.Errorf("decodeProtoValue(list, depth=max) = nil, want depth+1 overflow error")
		}

		rawListWire := appendBytesField(nil, 1, encodeProtoValue("item"))
		if _, err := decodeProtoList(rawListWire, maxProtoNestingDepth-1); err != nil {
			t.Errorf("decodeProtoList(depth=max-1) unexpected error: %v", err)
		}
		if _, err := decodeProtoList(rawListWire, maxProtoNestingDepth); err == nil {
			t.Errorf("decodeProtoList(depth=max) = nil, want depth+1 overflow error")
		}
	})
}

func TestWorkerStreamSequencingYieldAndToolBranching(t *testing.T) {
	t.Run("no_retries_disables_reconnection_required_loop", func(t *testing.T) {
		var conns atomic.Int32
		srvURL := startH2CTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conns.Add(1)
			w.Header().Set("Content-Type", "application/grpc")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			_, _ = grpcReadFrame(r.Body)
			_ = grpcWriteFrame(w, buildServerReconnectRequiredFrame())
			flusher.Flush()
		}))

		cmd := newTestRuntimeCmd(t, srvURL)
		rt, err := newRuntimeTransport(cmd, true)
		if err != nil {
			t.Fatalf("newRuntimeTransport() failed: %v", err)
		}
		w := newPullWorker(NewHTTPClient(HTTPClientOptions{Transport: rt}), nil, nil, pullWorkerOptions{
			WorkerID:  "w-no-reconnect",
			NoRetries: true,
			Logger:    log.New(io.Discard, "", 0),
		})
		err = w.executeLease(t.Context(), &LeasedInteraction{InteractionID: "int_nr", LeaseToken: "tok_nr"})
		if err == nil || !strings.Contains(err.Error(), "exceeded max stream reconnect attempts") {
			t.Errorf("executeLease(NoRetries=true on ReconnectionRequired) = %v, want max reconnect error", err)
		}
		if got := conns.Load(); got != 1 {
			t.Errorf("stream connection count with NoRetries=true = %d, want 1", got)
		}
	})

	t.Run("ack_only_same_seq_tool_call_multi_seq_and_yield_next_input_seq_gate", func(t *testing.T) {
		rootDir := t.TempDir()
		w := newPullWorker(nil, newFSServer(rootDir, rootDir), newShellServer(rootDir), pullWorkerOptions{
			WorkerID:          "w-seq",
			HeartbeatInterval: time.Hour,
			Logger:            log.New(io.Discard, "", 0),
		})

		// Build 3 inbound ServerEventBatches:
		// 1. SeqNum=5 with non-function Step (ToolCall without FunctionCall) -> ackUpdated=true, len(outEvents)==0 -> sends Ack=5.
		// 2. SeqNum=5 (same seq -> ackUpdated=false!) with list_dir ToolCall (gets client SeqNum=1 -> nextSeq=2) + Yield(NextInputSeqNum=1 < 2 -> MUST NOT disconnect!).
		// 3. SeqNum=6 with list_dir ToolCall (gets client SeqNum=2 -> nextSeq=3) + Yield(NextInputSeqNum=3 >= 3 -> MUST send Disconnect and exit!).
		var serverBuf bytes.Buffer
		nonFnStepPayload := appendBytesField(nil, 11, appendBytesField(nil, 4, appendStringField(nil, 1, "tc_no_fn")))
		batch1Ev := appendVarintField(appendBytesField(nil, 3, nonFnStepPayload), 10, 5)
		_ = grpcWriteFrame(&serverBuf, appendBytesField(nil, 1, batch1Ev))

		batch2 := append(buildServerStepToolCallFrame(5, "c_seq_1", "list_dir", nil), buildServerYieldFrame(1)...)
		_ = grpcWriteFrame(&serverBuf, batch2)

		batch3 := append(buildServerStepToolCallFrame(6, "c_seq_2", "list_dir", nil), buildServerYieldFrame(3)...)
		_ = grpcWriteFrame(&serverBuf, batch3)

		pr, pw := io.Pipe()
		var outboundFrames [][]byte
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			for {
				f, err := grpcReadFrame(pr)
				if err != nil {
					return
				}
				outboundFrames = append(outboundFrames, f)
			}
		}()

		stream := &grpcInteractionStream{
			resp: &http.Response{Body: io.NopCloser(&serverBuf)},
			pw:   pw,
		}
		state := &sessionState{
			interactionID: "int_seq",
			nextClientSeq: 1,
			executedCalls: make(map[string]*wirePayload),
		}
		reconnect, err := w.consumeStream(t.Context(), stream, state)
		_ = pw.Close()
		<-readDone

		if err != nil || reconnect {
			t.Fatalf("consumeStream() = (%v, %v), want (false, nil)", reconnect, err)
		}
		// Expect 4 outbound frames:
		// Frame 0: AckOutputSeqNum=5 (0 events)
		// Frame 1: AckOutputSeqNum=5, Events=[SeqNum=1]
		// Frame 2: AckOutputSeqNum=6, Events=[SeqNum=2]
		// Frame 3: AckOutputSeqNum=6, Disconnect
		if len(outboundFrames) != 4 {
			t.Fatalf("len(outboundFrames) = %d, want 4", len(outboundFrames))
		}

		parseBatch := func(frame []byte) (ack int64, eventSeqs []int64, hasDisconnect bool) {
			for rest := frame; len(rest) > 0; {
				num, _, vv, vb, n, err := consumeField(rest)
				if err != nil {
					t.Fatalf("consumeField(outbound) failed: %v", err)
				}
				rest = rest[n:]
				switch num {
				case 1:
					ev := decodeClientEventWire(t, vb)
					eventSeqs = append(eventSeqs, ev.seqNum)
				case 2:
					ack = int64(vv)
				case 9:
					hasDisconnect = true
				}
			}
			return ack, eventSeqs, hasDisconnect
		}

		ack0, ev0, disc0 := parseBatch(outboundFrames[0])
		if ack0 != 5 || len(ev0) != 0 || disc0 {
			t.Errorf("frame 0 = (ack=%d, ev=%v, disc=%v), want (5, [], false)", ack0, ev0, disc0)
		}
		ack1, ev1, disc1 := parseBatch(outboundFrames[1])
		if ack1 != 5 || !slices.Equal(ev1, []int64{1}) || disc1 {
			t.Errorf("frame 1 = (ack=%d, ev=%v, disc=%v), want (5, [1], false)", ack1, ev1, disc1)
		}
		ack2, ev2, disc2 := parseBatch(outboundFrames[2])
		if ack2 != 6 || !slices.Equal(ev2, []int64{2}) || disc2 {
			t.Errorf("frame 2 = (ack=%d, ev=%v, disc=%v), want (6, [2], false)", ack2, ev2, disc2)
		}
		ack3, ev3, disc3 := parseBatch(outboundFrames[3])
		if ack3 != 6 || len(ev3) != 0 || !disc3 {
			t.Errorf("frame 3 = (ack=%d, ev=%v, disc=%v), want (6, [], true)", ack3, ev3, disc3)
		}

		// Also verify Yield with len(outEvents) == 0 disconnects even when NextInputSeqNum < state.nextClientSeq.
		var yieldOnlyBuf bytes.Buffer
		_ = grpcWriteFrame(&yieldOnlyBuf, buildServerYieldFrame(1))
		// Append a compressed-flag poison frame so failing to return on Yield immediately surfaces an error.
		yieldOnlyBuf.Write([]byte{1, 0, 0, 0, 0})
		pr2, pw2 := io.Pipe()
		var yieldOutbound [][]byte
		readDone2 := make(chan struct{})
		go func() {
			defer close(readDone2)
			for {
				f, err := grpcReadFrame(pr2)
				if err != nil {
					return
				}
				yieldOutbound = append(yieldOutbound, f)
			}
		}()
		stream2 := &grpcInteractionStream{
			resp: &http.Response{Body: io.NopCloser(&yieldOnlyBuf)},
			pw:   pw2,
		}
		state2 := &sessionState{
			interactionID: "int_yield_empty",
			nextClientSeq: 3,
			lastAckSeq:    6,
			executedCalls: make(map[string]*wirePayload),
		}
		reconnect2, err2 := w.consumeStream(t.Context(), stream2, state2)
		_ = pw2.Close()
		<-readDone2
		if err2 != nil || reconnect2 || len(yieldOutbound) != 1 {
			t.Fatalf("consumeStream(empty outEvents with Yield=1 < nextSeq=3) = (reconnect=%v, err=%v, frames=%d), want (false, nil, 1)", reconnect2, err2, len(yieldOutbound))
		}
		if ackY, evY, discY := parseBatch(yieldOutbound[0]); ackY != 6 || len(evY) != 0 || !discY {
			t.Errorf("yield-only outbound frame = (ack=%d, ev=%v, disc=%v), want (6, [], true)", ackY, evY, discY)
		}
	})

	t.Run("tool_cwd_zero_worker_timeout_view_endline_replace_and_arg_helpers", func(t *testing.T) {
		rootDir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatalf("EvalSymlinks(TempDir) failed: %v", err)
		}
		subDir := filepath.Join(rootDir, "nested")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(subDir) failed: %v", err)
		}
		fs := newFSServer(rootDir, rootDir)
		sh := newShellServer(rootDir)

		// Zero commandTimeout on pullWorker must honor per-call timeout_ms without clamping to 0 (2m).
		zeroTimeoutWorker := &pullWorker{
			fs:             fs,
			shell:          sh,
			commandTimeout: 0,
			logger:         log.New(io.Discard, "", 0),
		}
		cwdRes, isErr := zeroTimeoutWorker.toolExecuteBash(t.Context(), map[string]any{"command": "pwd", "Cwd": "nested"})
		if isErr || strings.TrimSpace(fmt.Sprint(cwdRes["stdout"])) != subDir {
			t.Errorf("toolExecuteBash(Cwd=nested, pwd) = (%v, %v), want stdout=%q", cwdRes, isErr, subDir)
		}

		boundedCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		timeoutRes, isErr := zeroTimeoutWorker.toolExecuteBash(boundedCtx, map[string]any{"command": "sleep 2", "timeout_ms": 60})
		if !isErr || !strings.Contains(fmt.Sprint(timeoutRes["error"]), "60ms") {
			t.Errorf("toolExecuteBash(commandTimeout=0, timeout_ms=60) = (%v, %v), want '60ms' timeout error", timeoutRes, isErr)
		}

		// Positive commandTimeout (5s) must NOT overwrite a smaller per-call timeout_ms (60ms).
		cappedWorker := &pullWorker{
			fs:             fs,
			shell:          sh,
			commandTimeout: 5 * time.Second,
			logger:         log.New(io.Discard, "", 0),
		}
		boundedCtx2, cancel2 := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel2()
		smallerTimeoutRes, isErr := cappedWorker.toolExecuteBash(boundedCtx2, map[string]any{"command": "sleep 2", "timeout_ms": 60})
		if !isErr || !strings.Contains(fmt.Sprint(smallerTimeoutRes["error"]), "60ms") {
			t.Errorf("toolExecuteBash(commandTimeout=5s, timeout_ms=60) = (%v, %v), want '60ms' timeout preserved", smallerTimeoutRes, isErr)
		}

		// toolViewFile full read (StartLine==0 && EndLine==0) preserves exact CRLF bytes, plus EndLine-only and StartLine-only.
		if _, err := fs.WriteRegularFile("lines.txt", "a\nb\nc\nd\n", true, false); err != nil {
			t.Fatalf("WriteRegularFile(lines.txt) failed: %v", err)
		}
		viewFull, isErr := zeroTimeoutWorker.toolViewFile(map[string]any{"path": "lines.txt"})
		if isErr || viewFull["content"] != "a\nb\nc\nd\n" || viewFull["path"] != "lines.txt" {
			t.Errorf("toolViewFile(full) = (%v, %v), want content=\"a\\nb\\nc\\nd\\n\"", viewFull, isErr)
		}
		if _, err := fs.WriteRegularFile("crlf_full.txt", "crlf1\r\ncrlf2\r\n", true, false); err != nil {
			t.Fatalf("WriteRegularFile(crlf_full.txt) failed: %v", err)
		}
		viewCRLF, isErr := zeroTimeoutWorker.toolViewFile(map[string]any{"path": "crlf_full.txt"})
		if isErr || viewCRLF["content"] != "crlf1\r\ncrlf2\r\n" {
			t.Errorf("toolViewFile(full CRLF) = (%v, %v), want exact \"crlf1\\r\\ncrlf2\\r\\n\" without range normalization", viewCRLF, isErr)
		}
		viewEndOnly, isErr := zeroTimeoutWorker.toolViewFile(map[string]any{"path": "lines.txt", "EndLine": 2})
		if isErr || viewEndOnly["content"] != "a\nb" {
			t.Errorf("toolViewFile(StartLine=0, EndLine=2) = (%v, %v), want content=\"a\\nb\"", viewEndOnly, isErr)
		}
		viewStartOnly, isErr := zeroTimeoutWorker.toolViewFile(map[string]any{"path": "lines.txt", "StartLine": 3})
		if isErr || viewStartOnly["content"] != "c\nd\n" {
			t.Errorf("toolViewFile(StartLine=3, EndLine=0) = (%v, %v), want content=\"c\\nd\\n\"", viewStartOnly, isErr)
		}

		// Explicit empty-path / empty-target error messages across file tools and default "." path for list_dir.
		viewNoPath, isErr := zeroTimeoutWorker.toolViewFile(map[string]any{})
		if !isErr || viewNoPath["error"] != "path is required" {
			t.Errorf("toolViewFile(missing path) = (%v, %v), want error=\"path is required\"", viewNoPath, isErr)
		}
		writeNoPath, isErr := zeroTimeoutWorker.toolWriteFile(map[string]any{})
		if !isErr || writeNoPath["error"] != "path is required" {
			t.Errorf("toolWriteFile(missing path) = (%v, %v), want error=\"path is required\"", writeNoPath, isErr)
		}
		replaceNoPath, isErr := zeroTimeoutWorker.toolReplaceFileContent(map[string]any{"TargetContent": "a"})
		if !isErr || replaceNoPath["error"] != "TargetFile is required" {
			t.Errorf("toolReplaceFileContent(missing TargetFile) = (%v, %v), want error=\"TargetFile is required\"", replaceNoPath, isErr)
		}
		replaceNoTarget, isErr := zeroTimeoutWorker.toolReplaceFileContent(map[string]any{"TargetFile": "lines.txt"})
		if !isErr || replaceNoTarget["error"] != "TargetContent is required" {
			t.Errorf("toolReplaceFileContent(missing TargetContent) = (%v, %v), want error=\"TargetContent is required\"", replaceNoTarget, isErr)
		}
		openRoot, err := os.OpenRoot(rootDir)
		if err != nil {
			t.Fatalf("os.OpenRoot(rootDir) failed: %v", err)
		}
		defer openRoot.Close()
		if err := mkdirAllInRoot(openRoot, "lines.txt"); err == nil {
			t.Errorf("mkdirAllInRoot(regular file) = nil, want EEXIST error")
		}
		listDefault, isErr := zeroTimeoutWorker.toolListDir(map[string]any{})
		if isErr || listDefault["path"] != "." {
			t.Errorf("toolListDir(empty args) = (%v, %v), want path=\".\"", listDefault, isErr)
		}

		// replaceInText when CRLF content is matched with explicit CRLF target, and when StartLine==0 && EndLine>0.
		gotCRLFTarget, cnt, err := replaceInText("a\r\nb\r\n", "a\r\nb", "x\r\ny", 0, 0, false)
		if err != nil || cnt != 1 || gotCRLFTarget != "x\r\ny\r\n" {
			t.Errorf("replaceInText(explicit CRLF target) = (%q, %d, %v), want (\"x\\r\\ny\\r\\n\", 1, nil)", gotCRLFTarget, cnt, err)
		}
		gotEndOnly, cnt, err := replaceInText("dup\nkeep\ndup", "dup", "NEW", 0, 2, false)
		if err != nil || cnt != 1 || gotEndOnly != "NEW\nkeep\ndup" {
			t.Errorf("replaceInText(StartLine=0, EndLine=2) = (%q, %d, %v), want (\"NEW\\nkeep\\ndup\", 1, nil)", gotEndOnly, cnt, err)
		}
		gotStartOnly, cnt, err := replaceInText("dup\nkeep\ndup", "dup", "NEW", 2, 0, false)
		if err != nil || cnt != 1 || gotStartOnly != "dup\nkeep\nNEW" {
			t.Errorf("replaceInText(StartLine=2, EndLine=0) = (%q, %d, %v), want (\"dup\\nkeep\\nNEW\", 1, nil)", gotStartOnly, cnt, err)
		}
		if invRange, err := fs.ReadFileRange("lines.txt", 4, 2); err != nil || invRange != "" {
			t.Errorf("ReadFileRange(4, 2) = (%q, %v), want (\"\", nil)", invRange, err)
		}
		if rEmpty, wEmpty, err := resolveSandboxDirs(subDir, "", false); err != nil || rEmpty != subDir || wEmpty != subDir {
			t.Errorf("resolveSandboxDirs(subDir, \"\") = (%q, %q, %v), want (%q, %q, nil)", rEmpty, wEmpty, err, subDir, subDir)
		}
		if rDot, wDot, err := resolveSandboxDirs(subDir, ".", false); err != nil || rDot != subDir || wDot != subDir {
			t.Errorf("resolveSandboxDirs(subDir, \".\") = (%q, %q, %v), want (%q, %q, nil)", rDot, wDot, err, subDir, subDir)
		}
		if genZero, err := parseProtoInt64(json.RawMessage(`""`)); err != nil || genZero != 0 {
			t.Errorf("parseProtoInt64(\"\") = (%d, %v), want (0, nil)", genZero, err)
		}
		schemaOutFile := filepath.Join(t.TempDir(), "schema.json")
		t.Setenv("TEST_SCHEMA_OUT", schemaOutFile)
		schemaProv := NewCommandWorkerProvisioner(`cat > "$TEST_SCHEMA_OUT"`)
		if err := schemaProv.ProvisionAndDispatch(t.Context(), ProvisionInput{SchemaVersion: "v2", InteractionID: "i_v2"}); err != nil {
			t.Fatalf("ProvisionAndDispatch(v2) failed: %v", err)
		}
		if rawV2, err := os.ReadFile(schemaOutFile); err != nil || !strings.Contains(string(rawV2), `"schema_version":"v2"`) {
			t.Errorf("ProvisionAndDispatch(v2) payload = %q (err=%v), want schema_version=v2", rawV2, err)
		}

		// stringArg skips nil values instead of formatting "<nil>", and boolArg returns explicit bool opposite fallback.
		if got := stringArg(map[string]any{"first": nil, "second": "real"}, "first", "second"); got != "real" {
			t.Errorf("stringArg(nil then real) = %q, want \"real\"", got)
		}
		if got := boolArg(map[string]any{"nil_v": nil, "explicit_false": false}, true, "nil_v", "explicit_false"); got {
			t.Errorf("boolArg(explicit_false, fallback=true) = true, want false")
		}
		if got := boolArg(map[string]any{"explicit_true": true}, false, "explicit_true"); !got {
			t.Errorf("boolArg(explicit_true, fallback=false) = false, want true")
		}
		if got := intArg(map[string]any{"nil_i": nil, "valid_i": 17}, 3, "nil_i", "valid_i"); got != 17 {
			t.Errorf("intArg(nil then 17) = %d, want 17", got)
		}
		if got := stringSliceArg(map[string]any{"nil_s": nil, "valid_s": []string{"x"}}, "nil_s", "valid_s"); !slices.Equal(got, []string{"x"}) {
			t.Errorf("stringSliceArg(nil then [x]) = %v, want [x]", got)
		}

		// Dangling symlink root-dir rejection in resolveSandboxDirs (both direct and nested under dangling prefix).
		danglingParent := t.TempDir()
		danglingRoot := filepath.Join(danglingParent, "dangling_root")
		if err := os.Symlink(filepath.Join(danglingParent, "nonexistent_target"), danglingRoot); err != nil {
			t.Fatalf("os.Symlink(danglingRoot) failed: %v", err)
		}
		if _, _, err := resolveSandboxDirs(danglingRoot, "", false); err == nil || !strings.Contains(err.Error(), "dangling symlink") {
			t.Errorf("resolveSandboxDirs(danglingRoot, create=false) = %v, want dangling symlink error", err)
		}
		if _, _, err := resolveSandboxDirs(danglingRoot, "", true); err == nil || !strings.Contains(err.Error(), "dangling symlink") {
			t.Errorf("resolveSandboxDirs(danglingRoot, create=true) = %v, want dangling symlink error", err)
		}
		if _, _, err := resolveSandboxDirs(filepath.Join(danglingRoot, "nested"), "", false); err == nil || !strings.Contains(err.Error(), "dangling symlink") {
			t.Errorf("resolveSandboxDirs(danglingRoot/nested, create=false) = %v, want dangling symlink error", err)
		}

		// Nil-safe wire marshaling across clientEventBatch and clientEvent sub-structures.
		if got := (*clientEventBatch)(nil).marshalWire(); len(got) != 0 {
			t.Errorf("(*clientEventBatch)(nil).marshalWire() = %x, want empty", got)
		}
		nilPayloadWire := (&clientEvent{SeqNum: 7, TimestampMicros: 9, Payload: nil}).marshalWire()
		if len(nilPayloadWire) == 0 || bytes.Contains(nilPayloadWire, []byte{0x1a}) {
			t.Errorf("clientEvent{Payload:nil}.marshalWire() = %x, want SeqNum/Timestamp without field 3 tag 0x1a", nilPayloadWire)
		}
		emptyPayloadWire := (&clientEvent{Payload: &wirePayload{Step: nil}}).marshalWire()
		if !bytes.Equal(emptyPayloadWire, []byte{0x1a, 0x00}) {
			t.Errorf("clientEvent{Step:nil}.marshalWire() = %x, want 1a00", emptyPayloadWire)
		}
		emptyStepWire := (&clientEvent{Payload: &wirePayload{Step: &wireStep{ToolResult: nil}}}).marshalWire()
		if !bytes.Equal(emptyStepWire, []byte{0x1a, 0x00}) {
			t.Errorf("clientEvent{ToolResult:nil}.marshalWire() = %x, want 1a00", emptyStepWire)
		}
		nilFnResWire := (&clientEvent{Payload: &wirePayload{Step: &wireStep{ToolResult: &toolResultStep{CallID: "c1", FunctionResult: nil}}}}).marshalWire()
		if len(nilFnResWire) == 0 {
			t.Errorf("clientEvent{FunctionResult:nil}.marshalWire() = empty, want CallID encoded")
		}
		batchWithNilEv := (&clientEventBatch{
			AckOutputSeqNum: 4,
			Events:          []*clientEvent{nil, {SeqNum: 2}},
			ReconnectInteraction: &reconnectInteractionEvent{
				InteractionID:    "i_rec",
				NextOutputSeqNum: 3,
				Background:       true,
				ClientID:         "environment",
			},
			Disconnect: &disconnectEvent{},
		}).marshalWire()
		if len(batchWithNilEv) == 0 {
			t.Errorf("clientEventBatch(with nil event + reconnect + disconnect) = empty")
		}

		// Nil-chain guards in handleServerEvent (Payload==nil, Step==nil, ToolCall==nil, FunctionCall==nil).
		dummyState := &sessionState{interactionID: "i_nil", executedCalls: make(map[string]*wirePayload)}
		for idx, sev := range []*serverEvent{
			{SeqNum: 1, Payload: nil},
			{SeqNum: 2, Payload: &wirePayload{Step: nil}},
			{SeqNum: 3, Payload: &wirePayload{Step: &wireStep{ToolCall: nil}}},
			{SeqNum: 4, Payload: &wirePayload{Step: &wireStep{ToolCall: &toolCallStep{ID: "tc_nil", FunctionCall: nil}}}},
		} {
			if got := zeroTimeoutWorker.handleServerEvent(t.Context(), sev, dummyState); got != nil {
				t.Errorf("handleServerEvent(case %d) = %+v, want nil", idx, got)
			}
		}

		// HeartbeatInteraction skips nil elements in parsed.Leases without panicking.
		rtHB := &runtimeTransport{
			baseURL:    "https://example.googleapis.com",
			scheme:     "https",
			apiVersion: "v1beta",
			headers:    make(map[string]string),
			httpClient: fakeHTTPDoer(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"leases":[null,{"interaction_id":"int_hb_ok"}]}`)),
				}, nil
			}),
		}
		hbRes, hbErr := NewHTTPClient(HTTPClientOptions{Transport: rtHB}).HeartbeatInteraction(t.Context(), "p1", "int_hb_ok", "env1", "lt1")
		if hbErr != nil || !hbRes.Renewed || hbRes.ShouldCancel {
			t.Errorf("HeartbeatInteraction(with null lease entry) = (%+v, %v), want Renewed=true", hbRes, hbErr)
		}

		// Empty gRPC frame read and unstarted subprocess group Cancel guard.
		if emptyFrame, err := grpcReadFrame(bytes.NewReader([]byte{0, 0, 0, 0, 0})); err != nil || len(emptyFrame) != 0 {
			t.Errorf("grpcReadFrame(empty frame) = (%x, %v), want (empty, nil)", emptyFrame, err)
		}
		unstarted := exec.Command("true")
		configureSubprocessGroup(unstarted)
		if err := unstarted.Cancel(); err != nil {
			t.Errorf("unstarted.Cancel() = %v, want nil", err)
		}
		if pwdDefault, _, code, err := sh.ExecuteBash(t.Context(), "pwd", "", time.Second); err != nil || code != 0 || strings.TrimSpace(pwdDefault) != rootDir {
			t.Errorf("ExecuteBash(workDir=\"\") = (%q, %d, %v), want (%q, 0, nil)", strings.TrimSpace(pwdDefault), code, err, rootDir)
		}
		if pwdSub, _, code, err := sh.ExecuteBash(t.Context(), "pwd", subDir, time.Second); err != nil || code != 0 || strings.TrimSpace(pwdSub) != subDir {
			t.Errorf("ExecuteBash(workDir=subDir) = (%q, %d, %v), want (%q, 0, nil)", strings.TrimSpace(pwdSub), code, err, subDir)
		}
	})
}

type countingReader struct {
	r         io.Reader
	bytesRead int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytesRead += n
	return n, err
}

type errReader struct {
	err error
}

func (e errReader) Read([]byte) (int, error) {
	return 0, e.err
}

type countingWriter struct {
	buf         bytes.Buffer
	writes      int
	failAtWrite int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	if c.failAtWrite > 0 && c.writes == c.failAtWrite {
		return 0, errors.New("injected write failure")
	}
	return c.buf.Write(p)
}
