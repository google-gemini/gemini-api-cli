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
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxStreamReconnects = 3
	maxToolResultWireBytes     = maxGRPCFrameBytes - 4096
)

type pullWorkerOptions struct {
	WorkerID          string
	MaxConcurrent     int
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	CommandTimeout    time.Duration
	DisableBash       bool
	NoRetries         bool
	Logger            *log.Logger
}

type pullWorker struct {
	client            *HTTPClient
	fs                *fsServer
	shell             *shellServer
	workerID          string
	maxConcurrent     int
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	commandTimeout    time.Duration
	disableBash       bool
	noRetries         bool
	logger            *log.Logger
}

func newPullWorker(client *HTTPClient, fs *fsServer, shell *shellServer, opts pullWorkerOptions) *pullWorker {
	if opts.WorkerID == "" {
		opts.WorkerID = defaultRuntimeID("worker")
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = defaultHeartbeatInterval
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 2 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	return &pullWorker{
		client:            client,
		fs:                fs,
		shell:             shell,
		workerID:          opts.WorkerID,
		maxConcurrent:     opts.MaxConcurrent,
		pollInterval:      opts.PollInterval,
		heartbeatInterval: opts.HeartbeatInterval,
		commandTimeout:    opts.CommandTimeout,
		disableBash:       opts.DisableBash,
		noRetries:         opts.NoRetries,
		logger:            opts.Logger,
	}
}

func (w *pullWorker) Run(ctx context.Context) error {
	return runLeasePollLoop(ctx, leasePollLoopConfig{
		client:       w.client,
		callerID:     w.workerID,
		maxSlots:     w.maxConcurrent,
		pollInterval: w.pollInterval,
		noRetries:    w.noRetries,
		logger:       w.logger,
		errLabel:     "LeaseInteractions error",
		startMsg:     fmt.Sprintf("Worker %s polling (maxConcurrent=%d)", w.workerID, w.maxConcurrent),
	}, func(ctx context.Context, item *LeasedInteraction) {
		w.logger.Printf("[%s] Claimed interaction lease (env=%s)", item.InteractionID, firstNonEmpty(item.EnvironmentID, "default"))
		if err := w.executeLease(ctx, item); err != nil && ctx.Err() == nil {
			w.logger.Printf("[%s] session ended with error: %v", item.InteractionID, err)
			return
		}
		if ctx.Err() == nil {
			w.logger.Printf("[%s] Completed interaction session", item.InteractionID)
		}
	})
}

func runStartupLeases(ctx context.Context, w *pullWorker, leases []*LeasedInteraction) error {
	for _, lease := range leases {
		w.logger.Printf("[%s] Attached interaction session", lease.InteractionID)
		if err := w.executeLease(ctx, lease); err != nil {
			return err
		}
		w.logger.Printf("[%s] Completed interaction session", lease.InteractionID)
	}
	return nil
}

type sessionState struct {
	interactionID string
	mu            sync.Mutex
	lastAckSeq    int64
	nextClientSeq int64
	executedCalls map[string]*wirePayload
}

func (s *sessionState) ackSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAckSeq
}

func (w *pullWorker) executeLease(ctx context.Context, lease *LeasedInteraction) error {
	state := &sessionState{
		interactionID: lease.InteractionID,
		nextClientSeq: 1,
		executedCalls: make(map[string]*wirePayload),
	}
	maxReconnects := defaultMaxStreamReconnects
	if w.noRetries {
		maxReconnects = 0
	}

	for attempt := 0; attempt <= maxReconnects; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		stream, err := w.client.connectStream(ctx, connectStreamRequest{
			InteractionID:    lease.InteractionID,
			EnvironmentID:    lease.EnvironmentID,
			LeaseToken:       lease.LeaseToken,
			NextOutputSeqNum: state.ackSeq() + 1,
			AckOutputSeqNum:  state.ackSeq(),
		})
		if err != nil {
			if isNonRetryableControlPlaneError(err) || attempt >= maxReconnects {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryBackoff(200*time.Millisecond, attempt+1)):
				continue
			}
		}

		reconnect, runErr := w.consumeStream(ctx, stream, state)
		_ = stream.Close()
		if runErr != nil {
			if isNonRetryableControlPlaneError(runErr) || attempt >= maxReconnects {
				return runErr
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryBackoff(200*time.Millisecond, attempt+1)):
				continue
			}
		}
		if !reconnect {
			return nil
		}
	}
	return fmt.Errorf("exceeded max stream reconnect attempts for interaction %q", lease.InteractionID)
}

func (w *pullWorker) consumeStream(ctx context.Context, stream *grpcInteractionStream, state *sessionState) (bool, error) {
	// Renew the server lease in-band on the authenticated bidi stream every
	// heartbeatInterval (default 5s = 15s server TTL / 3) so --lease-token-only
	// sandbox workers renew leases without needing environment/API keys.
	stopHB := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(w.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopHB:
				return
			case <-ticker.C:
				_ = stream.Send(&clientEventBatch{AckOutputSeqNum: state.ackSeq()})
			}
		}
	}()
	defer func() {
		close(stopHB)
		<-hbDone
	}()

	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}

		if batch.InteractionReconnected != nil {
			state.mu.Lock()
			if batch.InteractionReconnected.NextInputSeqNum > state.nextClientSeq {
				state.nextClientSeq = batch.InteractionReconnected.NextInputSeqNum
			}
			state.mu.Unlock()
		}

		state.mu.Lock()
		pendingAck := state.lastAckSeq
		startClientSeq := state.nextClientSeq
		state.mu.Unlock()

		var outEvents []*clientEvent
		ackUpdated := false
		for _, ev := range batch.Events {
			if ev.SeqNum > pendingAck {
				pendingAck = ev.SeqNum
				ackUpdated = true
			}

			if respPayload := w.handleServerEvent(ctx, ev, state); respPayload != nil {
				state.mu.Lock()
				seq := state.nextClientSeq
				state.nextClientSeq++
				state.mu.Unlock()
				outEvents = append(outEvents, &clientEvent{
					SeqNum:          seq,
					TimestampMicros: time.Now().UnixMicro(),
					Payload:         respPayload,
				})
			}
		}

		if len(outEvents) > 0 || ackUpdated {
			if err := stream.Send(&clientEventBatch{
				AckOutputSeqNum: pendingAck,
				Events:          outEvents,
			}); err != nil {
				state.mu.Lock()
				state.nextClientSeq = startClientSeq
				state.mu.Unlock()
				return false, err
			}
			state.mu.Lock()
			state.lastAckSeq = pendingAck
			state.mu.Unlock()
		}

		if batch.InteractionCompleted != nil {
			_ = stream.CloseSend()
			return false, nil
		}
		if batch.ReconnectionRequired != nil {
			return true, nil
		}
		if batch.Yield != nil {
			state.mu.Lock()
			nextSeq := state.nextClientSeq
			lastAck := state.lastAckSeq
			state.mu.Unlock()
			if len(outEvents) == 0 || batch.Yield.NextInputSeqNum >= nextSeq {
				_ = stream.Send(&clientEventBatch{
					AckOutputSeqNum: lastAck,
					Disconnect:      &disconnectEvent{},
				})
				_ = stream.CloseSend()
				_, _ = stream.Recv()
				return false, nil
			}
		}
	}
}

func (w *pullWorker) handleServerEvent(ctx context.Context, ev *serverEvent, state *sessionState) *wirePayload {
	if ev.Payload == nil {
		return nil
	}
	if ev.Payload.Step != nil && ev.Payload.Step.ToolCall != nil && ev.Payload.Step.ToolCall.FunctionCall != nil {
		tc := ev.Payload.Step.ToolCall
		fnCall := tc.FunctionCall
		callKey := "step:" + firstNonEmpty(tc.ID, fmt.Sprintf("seq-%d", ev.SeqNum))
		state.mu.Lock()
		cached, ok := state.executedCalls[callKey]
		state.mu.Unlock()
		if ok {
			return cached
		}
		w.logger.Printf("[%s] Executing tool %s", state.interactionID, fnCall.Name)
		resMap, isErr := w.dispatchTool(ctx, fnCall.Name, fnCall.Arguments)
		out := &wirePayload{
			Step: &wireStep{
				ToolResult: &toolResultStep{
					CallID: tc.ID,
					FunctionResult: &functionResultStep{
						Name:    fnCall.Name,
						IsError: isErr,
						Result:  resMap,
					},
				},
			},
		}
		if len((&clientEvent{SeqNum: 1, TimestampMicros: 1, Payload: out}).marshalWire()) > maxToolResultWireBytes {
			out = &wirePayload{
				Step: &wireStep{
					ToolResult: &toolResultStep{
						CallID: tc.ID,
						FunctionResult: &functionResultStep{
							Name:    fnCall.Name,
							IsError: true,
							Result: map[string]any{
								"error": fmt.Sprintf("tool %q result exceeds maximum frame size (%d bytes); narrow the query or specify start_line and end_line", fnCall.Name, maxGRPCFrameBytes),
							},
						},
					},
				},
			}
		}
		state.mu.Lock()
		state.executedCalls[callKey] = out
		state.mu.Unlock()
		return out
	}

	return nil
}

// Dispatch across the 7 canonical sandbox client tools.
func (w *pullWorker) dispatchTool(ctx context.Context, toolName string, args map[string]any) (map[string]any, bool) {
	normalized := strings.ToLower(strings.TrimSpace(toolName))
	switch normalized {
	case "execute_bash", "run_command", "run_shell_command", "bash", "shell":
		return w.toolExecuteBash(ctx, args)
	case "view_file", "read_file":
		return w.toolViewFile(args)
	case "write_to_file", "write_file":
		return w.toolWriteFile(args)
	case "replace_file_content", "edit_file":
		return w.toolReplaceFileContent(args)
	case "list_dir", "read_dir", "list_directory":
		return w.toolListDir(args)
	case "grep_search":
		return w.toolGrepSearch(args)
	case "find_by_name", "find_file":
		return w.toolFindByName(args)
	default:
		return map[string]any{"error": fmt.Sprintf("unsupported sandbox tool %q", toolName)}, true
	}
}

func (w *pullWorker) toolExecuteBash(ctx context.Context, args map[string]any) (map[string]any, bool) {
	if w.disableBash {
		return map[string]any{"error": "execute_bash is disabled (--disable-bash)"}, true
	}
	cmdLine := stringArg(args, "command", "CommandLine", "cmd")
	resolvedWorkDir, err := w.fs.ResolvePath(stringArg(args, "Cwd", "cwd", "working_dir"))
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	timeout := w.commandTimeout
	if timeoutMS := intArg(args, 0, "timeout_ms"); timeoutMS > 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	} else if timeoutSec := intArg(args, 0, "timeout_seconds", "timeout"); timeoutSec > 0 {
		timeout = time.Duration(timeoutSec) * time.Second
	}
	if w.commandTimeout > 0 && timeout > w.commandTimeout {
		timeout = w.commandTimeout
	}
	stdout, stderr, exitCode, err := w.shell.ExecuteBash(ctx, cmdLine, resolvedWorkDir, timeout)
	if err != nil {
		return map[string]any{
			"error":     err.Error(),
			"stdout":    stdout,
			"stderr":    stderr,
			"exit_code": exitCode,
		}, true
	}
	return map[string]any{
		"stdout":    stdout,
		"stderr":    stderr,
		"exit_code": exitCode,
	}, false
}

func (w *pullWorker) toolViewFile(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "AbsolutePath", "path", "file_path")
	if path == "" {
		return map[string]any{"error": "path is required"}, true
	}
	startLine := intArg(args, 0, "StartLine", "start_line")
	endLine := intArg(args, 0, "EndLine", "end_line")
	var content string
	if startLine > 0 || endLine > 0 {
		sliced, err := w.fs.ReadFileRange(path, startLine, endLine)
		if err != nil {
			return map[string]any{"error": err.Error()}, true
		}
		content = sliced
	} else {
		data, err := w.fs.ReadRegularFile(path)
		if err != nil {
			return map[string]any{"error": err.Error()}, true
		}
		content = string(data)
	}
	return map[string]any{
		"path":    path,
		"content": content,
	}, false
}

func (w *pullWorker) toolWriteFile(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "TargetFile", "path", "file_path")
	if path == "" {
		return map[string]any{"error": "path is required"}, true
	}
	content := stringArg(args, "CodeContent", "content")
	overwrite := boolArg(args, true, "Overwrite", "overwrite")
	appendMode := boolArg(args, false, "Append", "append")
	n, err := w.fs.WriteRegularFile(path, content, overwrite, appendMode)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{
		"status":        "ok",
		"path":          path,
		"bytes_written": n,
	}, false
}

func (w *pullWorker) toolReplaceFileContent(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "TargetFile", "path", "file_path")
	if path == "" {
		return map[string]any{"error": "TargetFile is required"}, true
	}
	target := stringArg(args, "TargetContent", "old_string")
	if target == "" {
		return map[string]any{"error": "TargetContent is required"}, true
	}
	replacement := stringArg(args, "ReplacementContent", "new_string")
	allowMultiple := boolArg(args, false, "AllowMultiple", "allow_multiple")
	startLine := intArg(args, 0, "StartLine", "start_line")
	endLine := intArg(args, 0, "EndLine", "end_line")

	raw, err := w.fs.ReadRegularFile(path)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	updated, count, err := replaceInText(string(raw), target, replacement, startLine, endLine, allowMultiple)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	if _, err := w.fs.WriteRegularFile(path, updated, true, false); err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{
		"status":       "ok",
		"path":         path,
		"replacements": count,
	}, false
}

func (w *pullWorker) toolListDir(args map[string]any) (map[string]any, bool) {
	path := stringArg(args, "DirectoryPath", "path", "dir_path")
	if path == "" {
		path = "."
	}
	entries, err := w.fs.ListEntries(path)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	return map[string]any{
		"path":    path,
		"entries": entries,
	}, false
}

func (w *pullWorker) toolGrepSearch(args map[string]any) (map[string]any, bool) {
	searchPath := firstNonEmpty(stringArg(args, "SearchPath", "path"), ".")
	query := stringArg(args, "Query", "query")
	caseInsensitive := boolArg(args, false, "CaseInsensitive", "case_insensitive")
	isRegex := boolArg(args, false, "IsRegex", "is_regex")
	matches, truncated, err := w.fs.Grep(searchPath, query, caseInsensitive, isRegex)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	res := map[string]any{"matches": matches}
	if truncated {
		res["truncated"] = true
	}
	return res, false
}

func (w *pullWorker) toolFindByName(args map[string]any) (map[string]any, bool) {
	searchDir := firstNonEmpty(stringArg(args, "SearchDirectory", "path"), ".")
	pattern := stringArg(args, "Pattern", "pattern")
	maxDepth := intArg(args, 0, "MaxDepth", "max_depth")
	extensions := stringSliceArg(args, "Extensions", "extensions")
	files, truncated, err := w.fs.FindFiles(searchDir, pattern, maxDepth, extensions...)
	if err != nil {
		return map[string]any{"error": err.Error()}, true
	}
	res := map[string]any{"files": files}
	if truncated {
		res["truncated"] = true
	}
	return res, false
}

func replaceInText(content, target, replacement string, startLine, endLine int, allowMultiple bool) (string, int, error) {
	preserveCRLF := strings.Contains(content, "\r\n") && !strings.Contains(target, "\r\n")
	if preserveCRLF {
		content = strings.ReplaceAll(content, "\r\n", "\n")
		replacement = strings.ReplaceAll(replacement, "\r\n", "\n")
	}
	restore := func(s string) string {
		if preserveCRLF {
			return strings.ReplaceAll(s, "\n", "\r\n")
		}
		return s
	}

	lines := strings.Split(content, "\n")
	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > endLine {
		return "", 0, fmt.Errorf("invalid line range [%d, %d]", startLine, endLine)
	}

	region := strings.Join(lines[startLine-1:endLine], "\n")
	count := strings.Count(region, target)
	if count == 0 {
		return "", 0, fmt.Errorf("target content not found in lines [%d, %d]", startLine, endLine)
	}
	if count > 1 && !allowMultiple {
		return "", 0, fmt.Errorf("found %d occurrences in lines [%d, %d] when AllowMultiple is false", count, startLine, endLine)
	}

	var parts []string
	if startLine > 1 {
		parts = append(parts, strings.Join(lines[:startLine-1], "\n"))
	}
	parts = append(parts, strings.ReplaceAll(region, target, replacement))
	if endLine < len(lines) {
		parts = append(parts, strings.Join(lines[endLine:], "\n"))
	}
	return restore(strings.Join(parts, "\n")), count, nil
}

func stringArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := args[k]; v != nil {
			return fmt.Sprint(v)
		}
	}
	return ""
}

func intArg(args map[string]any, fallback int, keys ...string) int {
	for _, k := range keys {
		switch val := args[k].(type) {
		case int:
			return val
		case int32:
			return int(val)
		case int64:
			return int(val)
		case float64:
			return int(val)
		}
	}
	return fallback
}

func boolArg(args map[string]any, fallback bool, keys ...string) bool {
	for _, k := range keys {
		if b, ok := args[k].(bool); ok {
			return b
		}
	}
	return fallback
}

func stringSliceArg(args map[string]any, keys ...string) []string {
	for _, k := range keys {
		switch val := args[k].(type) {
		case []string:
			return val
		case []any:
			out := make([]string, 0, len(val))
			for _, item := range val {
				if item != nil {
					out = append(out, fmt.Sprint(item))
				}
			}
			return out
		}
	}
	return nil
}
