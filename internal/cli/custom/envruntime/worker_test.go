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
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type scriptedStream struct {
	mu       sync.Mutex
	sent     []*ClientEventBatch
	recvFn   func(call int) (*ServerEventBatch, error)
	sendFn   func(call int, batch *ClientEventBatch) error
	recvCall int
	sendCall int
	closed   bool
}

func (s *scriptedStream) Send(_ context.Context, batch *ClientEventBatch) error {
	s.mu.Lock()
	s.sendCall++
	call := s.sendCall
	s.sent = append(s.sent, batch)
	fn := s.sendFn
	s.mu.Unlock()
	if fn != nil {
		return fn(call, batch)
	}
	return nil
}

func (s *scriptedStream) Recv(ctx context.Context) (*ServerEventBatch, error) {
	s.mu.Lock()
	s.recvCall++
	call := s.recvCall
	fn := s.recvFn
	s.mu.Unlock()
	if fn != nil {
		return fn(call)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *scriptedStream) CloseSend() error { return nil }

func (s *scriptedStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

type fakeEnvClient struct {
	leaseFn   func(ctx context.Context, req *LeaseInteractionsRequest) (*LeaseInteractionsResult, error)
	connectFn func(ctx context.Context, req *ConnectInteractionRequest) (InteractionStream, error)
}

func (f *fakeEnvClient) LeaseInteractions(ctx context.Context, req *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
	return f.leaseFn(ctx, req)
}

func (f *fakeEnvClient) ConnectInteraction(ctx context.Context, req *ConnectInteractionRequest) (InteractionStream, error) {
	return f.connectFn(ctx, req)
}

type hookHandlerFunc func(ctx context.Context, req *CallHookRequest) (*CallHookResponse, error)

func (f hookHandlerFunc) CallHook(ctx context.Context, req *CallHookRequest) (*CallHookResponse, error) {
	return f(ctx, req)
}

type recordingObserver struct {
	mu                  sync.Mutex
	leasePolledCounts   []int
	leasePolledErrs     []error
	interactionStarted  []string
	interactionFinished []string
	workerProvisioned   []bool
	handoffStatuses     []string
}

func (o *recordingObserver) LeasePolled(leasedCount int, err error) {
	o.mu.Lock()
	o.leasePolledCounts = append(o.leasePolledCounts, leasedCount)
	o.leasePolledErrs = append(o.leasePolledErrs, err)
	o.mu.Unlock()
}

func (o *recordingObserver) InteractionStarted(interactionID string) {
	o.mu.Lock()
	o.interactionStarted = append(o.interactionStarted, interactionID)
	o.mu.Unlock()
}

func (o *recordingObserver) InteractionFinished(interactionID string, _ error) {
	o.mu.Lock()
	o.interactionFinished = append(o.interactionFinished, interactionID)
	o.mu.Unlock()
}

func (o *recordingObserver) WorkerProvisioned(shared bool, _ error) {
	o.mu.Lock()
	o.workerProvisioned = append(o.workerProvisioned, shared)
	o.mu.Unlock()
}

func (o *recordingObserver) HandoffFinished(status string) {
	o.mu.Lock()
	o.handoffStatuses = append(o.handoffStatuses, status)
	o.mu.Unlock()
}

func TestPullWorkerReconnectReplayHooksYieldAndOversizedResult(t *testing.T) {
	var toolCalls atomic.Int32
	var fnCalls atomic.Int32
	var hookCalls atomic.Int32
	var connectReqs []*ConnectInteractionRequest
	var attempt3Stream, attempt4Stream *scriptedStream
	var mu sync.Mutex

	tools := NewToolDispatcher(ToolDispatcherConfig{
		ShellExec: func(_ context.Context, cmd, _ string, _ time.Duration) (string, string, int, error) {
			if cmd == "huge" {
				return strings.Repeat("Z", MaxToolResultWireBytes+8192), "", 0, nil
			}
			if cmd == "fn_once" {
				fnCalls.Add(1)
				return "fn:" + cmd, "", 0, nil
			}
			toolCalls.Add(1)
			return "ran:" + cmd, "", 0, nil
		},
	})

	var attempt atomic.Int32
	client := &fakeEnvClient{
		connectFn: func(_ context.Context, req *ConnectInteractionRequest) (InteractionStream, error) {
			mu.Lock()
			reqCopy := *req
			connectReqs = append(connectReqs, &reqCopy)
			mu.Unlock()

			switch attempt.Add(1) {
			case 1:
				// Attempt 1: transient connect error -> retried because RetryConnectErrors=true.
				return nil, errors.New("transient dial error")
			case 2:
				// Attempt 2: delivers empty payload + FunctionInvocation("fi_once") + Step.ToolCall("call_1"), Send fails -> rolls back seq while caching both!
				return &scriptedStream{
					sendFn: func(call int, _ *ClientEventBatch) error {
						if call == 1 {
							return errors.New("broken pipe during tool reply")
						}
						return nil
					},
					recvFn: func(call int) (*ServerEventBatch, error) {
						if call == 1 {
							return &ServerEventBatch{
								Events: []*ServerEvent{
									nil,
									{Payload: nil},
									{SeqNum: 1, Payload: &EventPayload{}},
									{SeqNum: 1, Payload: &EventPayload{Step: &Step{}}},
									{SeqNum: 1, Payload: &EventPayload{Step: &Step{ToolCall: &ToolCallStep{ID: "no_fn"}}}},
									{
										SeqNum: 2,
										Payload: &EventPayload{
											FunctionInvocation: &FunctionInvocation{
												CallID:    "fi_once",
												Name:      "execute_bash",
												Arguments: map[string]any{"command": "fn_once"},
											},
										},
									},
									{
										SeqNum: 3,
										Payload: &EventPayload{
											Step: &Step{
												ToolCall: &ToolCallStep{
													ID:           "call_1",
													FunctionCall: &FunctionCallStep{Name: "execute_bash", Arguments: map[string]any{"command": "once"}},
												},
											},
										},
									},
								},
							}, nil
						}
						return nil, io.EOF
					},
				}, nil
			case 3:
				// Attempt 3:
				// - Call 1: ack-only event (SeqNum=1, Payload=nil).
				// - Call 2: replays "fi_once" (SeqNum=2) and "call_1" (SeqNum=3) verifying rolled-back client seq 1 & 2.
				// - Call 3: InteractionReconnected{NextInputSeqNum: 10} + CallHookRequest("hk_1") + FunctionInvocation("fi_big", "huge") + non-terminal Yield{NextInputSeqNum: 1}.
				// - Call 4: ReconnectionRequired.
				s := &scriptedStream{
					recvFn: func(call int) (*ServerEventBatch, error) {
						switch call {
						case 1:
							return &ServerEventBatch{
								Events: []*ServerEvent{
									{SeqNum: 1, Payload: nil},
								},
							}, nil
						case 2:
							return &ServerEventBatch{
								Events: []*ServerEvent{
									{
										SeqNum: 2,
										Payload: &EventPayload{
											FunctionInvocation: &FunctionInvocation{
												CallID:    "fi_once",
												Name:      "execute_bash",
												Arguments: map[string]any{"command": "fn_once"},
											},
										},
									},
									{
										SeqNum: 3,
										Payload: &EventPayload{
											Step: &Step{
												ToolCall: &ToolCallStep{
													ID:           "call_1",
													FunctionCall: &FunctionCallStep{Name: "execute_bash", Arguments: map[string]any{"command": "once"}},
												},
											},
										},
									},
								},
							}, nil
						case 3:
							return &ServerEventBatch{
								InteractionReconnected: &InteractionReconnectedEvent{NextInputSeqNum: 10},
								Events: []*ServerEvent{
									{
										SeqNum: 4,
										Payload: &EventPayload{
											CallHookRequest: &CallHookRequest{RequestID: "hk_1", Name: "audit"},
										},
									},
									{
										SeqNum: 5,
										Payload: &EventPayload{
											FunctionInvocation: &FunctionInvocation{
												CallID:    "fi_big",
												Name:      "execute_bash",
												Arguments: map[string]any{"command": "huge"},
											},
										},
									},
								},
								Yield: &YieldEvent{NextInputSeqNum: 1},
							}, nil
						case 4:
							return &ServerEventBatch{
								ReconnectionRequired: &ReconnectionRequiredEvent{},
							}, nil
						default:
							return nil, io.EOF
						}
					},
				}
				mu.Lock()
				attempt3Stream = s
				mu.Unlock()
				return s, nil
			default:
				// Attempt 4:
				// - Call 1: waits for at least one in-band heartbeat tick, replays hk_1 with SeqNum=0 (verifying len(outEvents) > 0 sends even when ackUpdated==false), and sends Yield{NextInputSeqNum: 15} (verifying batch.Yield.NextInputSeqNum >= nextSeq disconnects when len(outEvents) > 0).
				s := &scriptedStream{
					recvFn: func(call int) (*ServerEventBatch, error) {
						if call == 1 {
							time.Sleep(25 * time.Millisecond)
							return &ServerEventBatch{
								Events: []*ServerEvent{{
									SeqNum:  0,
									Payload: &EventPayload{CallHookRequest: &CallHookRequest{RequestID: "hk_1", Name: "audit"}},
								}},
								Yield: &YieldEvent{NextInputSeqNum: 15},
							}, nil
						}
						return nil, io.EOF
					},
				}
				mu.Lock()
				attempt4Stream = s
				mu.Unlock()
				return s, nil
			}
		},
	}

	obs := &recordingObserver{}
	worker := NewPullWorker(PullWorkerConfig{
		Client: client,
		Tools:  tools,
		Hooks: hookHandlerFunc(func(_ context.Context, req *CallHookRequest) (*CallHookResponse, error) {
			hookCalls.Add(1)
			return &CallHookResponse{RequestID: req.RequestID, EmptyResult: true}, nil
		}),
		Observer:              obs,
		EnvironmentKey:        "ek_w",
		WorkerID:              "w-1",
		RetryConnectErrors:    true,
		InBandStreamHeartbeat: true,
		HeartbeatInterval:     10 * time.Millisecond,
		Logger:                log.New(io.Discard, "", 0),
	})

	if err := worker.ServeInteraction(t.Context(), LeasedInteraction{
		InteractionID: "  int_replay  ",
		LeaseToken:    "tok_replay",
	}); err != nil {
		t.Fatalf("ServeInteraction() failed: %v", err)
	}

	if got := toolCalls.Load(); got != 1 {
		t.Errorf("side-effecting tool execution count = %d, want 1 (replay cache must prevent duplicate execution)", got)
	}
	if got := fnCalls.Load(); got != 1 {
		t.Errorf("FunctionInvocation execution count = %d, want 1 (replay cache must prevent duplicate execution)", got)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Errorf("hook execution count = %d, want 1 (replay cache must prevent duplicate hook run)", got)
	}
	obs.mu.Lock()
	startedCopy := append([]string(nil), obs.interactionStarted...)
	obs.mu.Unlock()
	if len(startedCopy) != 1 || startedCopy[0] != "int_replay" {
		t.Errorf("observer.interactionStarted = %v, want [\"int_replay\"]", startedCopy)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(connectReqs) != 4 {
		t.Fatalf("len(connectReqs) = %d, want 4", len(connectReqs))
	}
	if connectReqs[0].InteractionID != "int_replay" || connectReqs[0].HeartbeatInterval != 0 {
		t.Errorf("attempt 1 req = %+v, want trimmed InteractionID and HeartbeatInterval=0 when InBandStreamHeartbeat=true", connectReqs[0])
	}
	// Attempt 3 followed failed Send on attempt 2, so AckOutputSeqNum must have rolled back to 0!
	if connectReqs[2].AckOutputSeqNum != 0 || connectReqs[2].NextOutputSeqNum != 1 {
		t.Errorf("attempt 3 seq after rolled-back Send = (ack=%d, next=%d), want (0, 1)", connectReqs[2].AckOutputSeqNum, connectReqs[2].NextOutputSeqNum)
	}
	// Attempt 4 followed acked seq 5, so AckOutputSeqNum=5, NextOutputSeqNum=6!
	if connectReqs[3].AckOutputSeqNum != 5 || connectReqs[3].NextOutputSeqNum != 6 {
		t.Errorf("attempt 4 seq = (ack=%d, next=%d), want (5, 6)", connectReqs[3].AckOutputSeqNum, connectReqs[3].NextOutputSeqNum)
	}

	// Inspect attempt3Stream sent batches:
	// - Batch 0 (or after any in-band heartbeat): ack-only batch with AckOutputSeqNum=1 and len(Events)==0.
	// - Replay batch: Events[0].SeqNum==1 (FunctionResult IsError=false), Events[1].SeqNum==2.
	// - Reconnected batch: Events[0].SeqNum==10 (CallHookResponse), Events[1].SeqNum==11 (FunctionResult IsError=true for oversized output).
	var sawAckOnly, sawRolledBackSeq, sawReconnectedSeqAndOversized bool
	for _, b := range attempt3Stream.sent {
		if b.AckOutputSeqNum == 1 && len(b.Events) == 0 && b.Disconnect == nil {
			sawAckOnly = true
		}
		if len(b.Events) == 2 && b.Events[0].SeqNum == 1 && b.Events[1].SeqNum == 2 {
			if b.Events[0].Payload != nil && b.Events[0].Payload.FunctionResult != nil && !b.Events[0].Payload.FunctionResult.IsError {
				sawRolledBackSeq = true
			}
		}
		if len(b.Events) == 2 && b.Events[0].SeqNum == 10 && b.Events[1].SeqNum == 11 {
			if b.Events[1].Payload != nil && b.Events[1].Payload.FunctionResult != nil && b.Events[1].Payload.FunctionResult.IsError {
				sawReconnectedSeqAndOversized = true
			}
		}
	}
	if !sawAckOnly || !sawRolledBackSeq || !sawReconnectedSeqAndOversized {
		t.Errorf("attempt3Stream.sent checks: ackOnly=%v rolledBackSeq=%v reconnectedSeqAndOversized=%v", sawAckOnly, sawRolledBackSeq, sawReconnectedSeqAndOversized)
	}

	var sawInBandHeartbeat, sawAttempt4HookReply, sawAttempt4Disconnect bool
	for _, b := range attempt4Stream.sent {
		if len(b.Events) == 0 && b.Disconnect == nil {
			sawInBandHeartbeat = true
		}
		if len(b.Events) == 1 && b.Events[0].Payload != nil && b.Events[0].Payload.CallHookResponse != nil {
			sawAttempt4HookReply = true
		}
		if b.Disconnect != nil {
			sawAttempt4Disconnect = true
		}
	}
	if !sawInBandHeartbeat || !sawAttempt4HookReply || !sawAttempt4Disconnect {
		t.Errorf("attempt4Stream.sent: sawInBandHeartbeat=%v sawAttempt4HookReply=%v sawAttempt4Disconnect=%v, want all true", sawInBandHeartbeat, sawAttempt4HookReply, sawAttempt4Disconnect)
	}
}

func TestPullWorkerValidationServeLeasesAndActiveLeaseRenewal(t *testing.T) {
	// 1. ServeInteraction identifier validation and NewPullWorker defaults.
	wVal := NewPullWorker(PullWorkerConfig{Logger: log.New(io.Discard, "", 0)})
	if !strings.HasPrefix(wVal.workerID, "worker-") || wVal.leaseTTL != DefaultLeaseTTL || wVal.maxConcurrent != 4 || wVal.pollInterval != 2*time.Second {
		t.Errorf("wVal defaults: workerID=%q leaseTTL=%v maxConcurrent=%d pollInterval=%v", wVal.workerID, wVal.leaseTTL, wVal.maxConcurrent, wVal.pollInterval)
	}
	wCustom := NewPullWorker(PullWorkerConfig{
		WorkerID: "custom-w",
		LeaseTTL: 9 * time.Second,
		Logger:   log.New(io.Discard, "", 0),
	})
	if wCustom.workerID != "custom-w" || wCustom.leaseTTL != 9*time.Second {
		t.Errorf("wCustom = (workerID=%q, leaseTTL=%v)", wCustom.workerID, wCustom.leaseTTL)
	}
	for _, bad := range []struct {
		lease   LeasedInteraction
		wantErr string
	}{
		{lease: LeasedInteraction{}, wantErr: "interaction_id is required"},
		{lease: LeasedInteraction{InteractionID: "a..b"}, wantErr: "interaction_id contains invalid characters"},
		{lease: LeasedInteraction{InteractionID: "int_ok", EnvironmentID: "bad/env"}, wantErr: "environment_id contains invalid characters"},
	} {
		if err := wVal.ServeInteraction(t.Context(), bad.lease); err == nil || !strings.Contains(err.Error(), bad.wantErr) {
			t.Errorf("ServeInteraction(%+v) = %v, want %q", bad.lease, err, bad.wantErr)
		}
	}

	// 2. ServeLeases, NoRetries with RetryConnectErrors=true, bounded recv retries, and HeartbeatInterval resolution.
	var connectCalls atomic.Int32
	var lastConnectHB time.Duration
	var nonInBandStream *scriptedStream
	errClient := &fakeEnvClient{
		connectFn: func(_ context.Context, req *ConnectInteractionRequest) (InteractionStream, error) {
			connectCalls.Add(1)
			lastConnectHB = req.HeartbeatInterval
			if req.InteractionID == "int_broken" {
				return nil, errors.New("stream broken")
			}
			if req.InteractionID == "int_recv_err" {
				callN := connectCalls.Load()
				return &scriptedStream{
					recvFn: func(int) (*ServerEventBatch, error) {
						if callN == 1 {
							return nil, errors.New("transient recv error 1")
						}
						return nil, errors.New("fatal custom recv error 2")
					},
				}, nil
			}
			var s *scriptedStream
			s = &scriptedStream{
				recvFn: func(call int) (*ServerEventBatch, error) {
					if call == 1 {
						if req.InteractionID == "int_hb_wait" {
							time.Sleep(25 * time.Millisecond)
						}
						if req.InteractionID == "int_yield_empty" {
							// Standalone Yield with NextInputSeqNum=0 (< nextSeq=5) and no outEvents -> must disconnect via len(outEvents)==0!
							return &ServerEventBatch{
								InteractionReconnected: &InteractionReconnectedEvent{NextInputSeqNum: 5},
								Yield:                  &YieldEvent{NextInputSeqNum: 0},
							}, nil
						}
						return &ServerEventBatch{
							InteractionCompleted: &InteractionCompletedEvent{
								Status: &RPCStatus{Code: 13, Message: "model failed"},
							},
						}, nil
					}
					if req.InteractionID == "int_yield_empty" {
						s.mu.Lock()
						disconnected := len(s.sent) > 0 && s.sent[len(s.sent)-1].Disconnect != nil
						s.mu.Unlock()
						if !disconnected {
							return nil, errors.New("standalone Yield did not send Disconnect")
						}
					}
					return nil, io.EOF
				},
			}
			nonInBandStream = s
			return s, nil
		},
	}
	wErr := NewPullWorker(PullWorkerConfig{
		Client:               errClient,
		NoRetries:            true,
		RetryConnectErrors:   true,
		MaxReconnectAttempts: 3,
		Logger:               log.New(io.Discard, "", 0),
	})
	if err := wErr.ServeLeases(t.Context(), []*LeasedInteraction{
		nil,
		{InteractionID: "int_done", LeaseToken: "tok"},
		{InteractionID: "int_yield_empty", LeaseToken: "tok_y"},
		{InteractionID: "int_broken", LeaseToken: "tok2"},
	}); err == nil || !strings.Contains(err.Error(), "stream broken") {
		t.Errorf("ServeLeases(int_done, int_yield_empty, int_broken) = %v, want stream broken error", err)
	}
	if got := connectCalls.Load(); got != 3 {
		t.Errorf("connectCalls = %d, want 3 (NoRetries=true must set maxReconnects=0)", got)
	}
	if lastConnectHB != DefaultHeartbeatInterval {
		t.Errorf("lastConnectHB(no envKey, with LeaseToken) = %v, want %v", lastConnectHB, DefaultHeartbeatInterval)
	}
	// With EnvironmentKey set and HeartbeatInterval=0, reqHB must be 0; with LeaseToken="" and envKey="", reqHB must be 0.
	_ = wErr.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_done", EnvironmentKey: "ek_set", LeaseToken: "tok"})
	if lastConnectHB != 0 {
		t.Errorf("lastConnectHB(with envKey) = %v, want 0", lastConnectHB)
	}
	_ = wErr.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_done"})
	if lastConnectHB != 0 {
		t.Errorf("lastConnectHB(without LeaseToken) = %v, want 0", lastConnectHB)
	}

	// Verify InBandStreamHeartbeat=false with HeartbeatInterval>0 passes HeartbeatInterval in ConnectInteractionRequest and does NOT run consumeStream ticker.
	wStreamHB := NewPullWorker(PullWorkerConfig{
		Client:                errClient,
		InBandStreamHeartbeat: false,
		HeartbeatInterval:     10 * time.Millisecond,
		NoRetries:             true,
		Logger:                log.New(io.Discard, "", 0),
	})
	if err := wStreamHB.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_hb_wait", EnvironmentKey: "ek"}); err != nil {
		t.Fatalf("wStreamHB.ExecuteLease() failed: %v", err)
	}
	if lastConnectHB != 10*time.Millisecond || len(nonInBandStream.sent) != 0 {
		t.Errorf("wStreamHB: lastConnectHB=%v sent=%d, want 10ms and 0 consumeStream sends", lastConnectHB, len(nonInBandStream.sent))
	}

	// Verify RetryConnectErrors=true with MaxReconnectAttempts=1 stops after 2 attempts on recv/connect error and returns the underlying error,
	// while RetryConnectErrors=false or custom NonRetryableCheck stops after 1 attempt.
	connectCalls.Store(0)
	wRecvRetry := NewPullWorker(PullWorkerConfig{
		Client:               errClient,
		RetryConnectErrors:   true,
		MaxReconnectAttempts: 1,
		Logger:               log.New(io.Discard, "", 0),
	})
	if err := wRecvRetry.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_recv_err", EnvironmentKey: "ek"}); err == nil || !strings.Contains(err.Error(), "fatal custom recv error 2") || connectCalls.Load() != 2 {
		t.Errorf("wRecvRetry.ExecuteLease(int_recv_err) = %v, connectCalls=%d, want fatal custom recv error 2 after 2 attempts", err, connectCalls.Load())
	}
	connectCalls.Store(0)
	if err := wRecvRetry.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_broken", EnvironmentKey: "ek"}); err == nil || !strings.Contains(err.Error(), "stream broken") || connectCalls.Load() != 2 {
		t.Errorf("wRecvRetry.ExecuteLease(int_broken) = %v, connectCalls=%d, want stream broken after 2 attempts", err, connectCalls.Load())
	}
	connectCalls.Store(0)
	wRecvNoRetry := NewPullWorker(PullWorkerConfig{
		Client:               errClient,
		RetryConnectErrors:   false,
		MaxReconnectAttempts: 3,
		Logger:               log.New(io.Discard, "", 0),
	})
	if err := wRecvNoRetry.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_recv_err", EnvironmentKey: "ek"}); err == nil || !strings.Contains(err.Error(), "transient recv error 1") || connectCalls.Load() != 1 {
		t.Errorf("wRecvNoRetry.ExecuteLease(int_recv_err) = %v, connectCalls=%d, want transient recv error 1 after 1 attempt", err, connectCalls.Load())
	}
	connectCalls.Store(0)
	if err := wRecvNoRetry.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_broken", EnvironmentKey: "ek"}); err == nil || !strings.Contains(err.Error(), `ConnectInteraction("int_broken") failed: stream broken`) || connectCalls.Load() != 1 {
		t.Errorf("wRecvNoRetry.ExecuteLease(int_broken) = %v, connectCalls=%d, want wrapped ConnectInteraction error after 1 attempt", err, connectCalls.Load())
	}
	connectCalls.Store(0)
	wRecvCustomNonRetry := NewPullWorker(PullWorkerConfig{
		Client:               errClient,
		RetryConnectErrors:   true,
		MaxReconnectAttempts: 3,
		NonRetryableCheck: func(err error) bool {
			return strings.Contains(err.Error(), "fatal custom recv")
		},
		Logger: log.New(io.Discard, "", 0),
	})
	if err := wRecvCustomNonRetry.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_recv_err", EnvironmentKey: "ek"}); err == nil || connectCalls.Load() != 2 {
		t.Errorf("wRecvCustomNonRetry.ExecuteLease() = %v, connectCalls=%d, want error after 2 attempts (attempt 1 retryable, attempt 2 non-retryable)", err, connectCalls.Load())
	}

	// Verify forward progress (advancing ackSeq) resets the reconnect attempt budget when MaxReconnectAttempts > 0,
	// and stalls still fail once consecutive unproductive reconnects exceed MaxReconnectAttempts.
	var progressAttempt atomic.Int64
	progressClient := &fakeEnvClient{
		connectFn: func(_ context.Context, req *ConnectInteractionRequest) (InteractionStream, error) {
			n := progressAttempt.Add(1)
			return &scriptedStream{
				recvFn: func(call int) (*ServerEventBatch, error) {
					if call > 1 {
						return nil, io.EOF
					}
					if req.InteractionID == "int_progress_complete" {
						if n <= 5 {
							return &ServerEventBatch{
								Events:               []*ServerEvent{{SeqNum: n}},
								ReconnectionRequired: &ReconnectionRequiredEvent{},
							}, nil
						}
						return &ServerEventBatch{
							Events:               []*ServerEvent{{SeqNum: n}},
							InteractionCompleted: &InteractionCompletedEvent{},
						}, nil
					}
					// For "int_progress_then_stall", advance on attempt 1, then stall (no new SeqNum) on subsequent reconnects.
					if n == 1 {
						return &ServerEventBatch{
							Events:               []*ServerEvent{{SeqNum: 1}},
							ReconnectionRequired: &ReconnectionRequiredEvent{},
						}, nil
					}
					return &ServerEventBatch{
						ReconnectionRequired: &ReconnectionRequiredEvent{},
					}, nil
				},
			}, nil
		},
	}
	wProgress := NewPullWorker(PullWorkerConfig{
		Client:               progressClient,
		MaxReconnectAttempts: 2,
		Logger:               log.New(io.Discard, "", 0),
	})
	if err := wProgress.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_progress_complete", EnvironmentKey: "ek"}); err != nil || progressAttempt.Load() != 6 {
		t.Errorf("wProgress.ExecuteLease(int_progress_complete) = %v, attempts=%d, want nil after 6 productive streams", err, progressAttempt.Load())
	}
	progressAttempt.Store(0)
	if err := wProgress.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_progress_then_stall", EnvironmentKey: "ek"}); err == nil || !strings.Contains(err.Error(), "exceeded max stream reconnect attempts (2)") || progressAttempt.Load() != 4 {
		t.Errorf("wProgress.ExecuteLease(int_progress_then_stall) = %v, attempts=%d, want exceeded max stream reconnect attempts (2) after 1 productive + 3 unproductive streams", err, progressAttempt.Load())
	}
	progressAttempt.Store(0)
	wProgressNoRetries := NewPullWorker(PullWorkerConfig{
		Client:    progressClient,
		NoRetries: true,
		Logger:    log.New(io.Discard, "", 0),
	})
	if err := wProgressNoRetries.ExecuteLease(t.Context(), &LeasedInteraction{InteractionID: "int_progress_complete", EnvironmentKey: "ek"}); err == nil || progressAttempt.Load() != 1 {
		t.Errorf("wProgressNoRetries.ExecuteLease() = %v, attempts=%d, want error after 1 stream when NoRetries=true", err, progressAttempt.Load())
	}

	// 3. TrackActiveLeases=true mode: RegisterEnvironment + active lease renewal + rotated token on reconnect + lost-lease cancellation!
	streamStarted := make(chan struct{})
	streamCanceled := make(chan struct{})
	tokenRotatedCh := make(chan struct{})
	var leasePollCount atomic.Int32
	var sawActiveRenewal atomic.Bool
	var firstPollMaxNew atomic.Int32
	var activeConnectTokens []string
	var activeMu sync.Mutex

	activeClient := &fakeEnvClient{
		leaseFn: func(_ context.Context, req *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
			n := leasePollCount.Add(1)
			switch n {
			case 1:
				firstPollMaxNew.Store(req.MaxNewInteractions)
				return &LeaseInteractionsResult{
					Leases: []LeasedInteraction{{
						InteractionID: "int_active_1",
						EnvironmentID: req.EnvironmentID,
						LeaseToken:    "tok_v1",
					}},
					PollBackoff: 15 * time.Millisecond,
				}, nil
			case 2:
				hasDirect := false
				hasActiveToken := false
				for _, al := range req.ActiveLeases {
					if al.InteractionID == "int_direct_notoken" && al.LeaseToken == "" {
						hasDirect = true
					}
					if al.InteractionID == "int_active_1" && al.LeaseToken == "tok_v1" {
						hasActiveToken = true
					}
				}
				if len(req.ActiveLeases) == 2 && hasDirect && hasActiveToken && req.MaxNewInteractions == 0 && req.SendZeroMaxNew {
					sawActiveRenewal.Store(true)
				}
				defer close(tokenRotatedCh)
				return &LeaseInteractionsResult{
					Leases: []LeasedInteraction{{
						InteractionID: "int_active_1",
						EnvironmentID: req.EnvironmentID,
						LeaseToken:    "tok_v2_rotated",
					}},
					PollBackoff: 15 * time.Millisecond,
				}, nil
			default:
				// Omit int_active_1 -> worker must cancel lost session!
				return &LeaseInteractionsResult{PollBackoff: 15 * time.Millisecond}, nil
			}
		},
		connectFn: func(_ context.Context, req *ConnectInteractionRequest) (InteractionStream, error) {
			if req.InteractionID == "int_direct_notoken" {
				return &ctxBlockingStream{started: make(chan struct{}), canceled: make(chan struct{})}, nil
			}
			activeMu.Lock()
			activeConnectTokens = append(activeConnectTokens, req.LeaseToken)
			callNum := len(activeConnectTokens)
			activeMu.Unlock()
			if callNum == 1 {
				// First stream waits until poll 2 rotates the token, then triggers ReconnectionRequired so attempt 2 uses rotated token!
				return &scriptedStream{
					recvFn: func(call int) (*ServerEventBatch, error) {
						if call == 1 {
							close(streamStarted)
							<-tokenRotatedCh
							return &ServerEventBatch{ReconnectionRequired: &ReconnectionRequiredEvent{}}, nil
						}
						return nil, io.EOF
					},
				}, nil
			}
			return &ctxBlockingStream{started: make(chan struct{}), canceled: streamCanceled}, nil
		},
	}

	runCtx, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()

	activeObs := &recordingObserver{}
	wActive := NewPullWorker(PullWorkerConfig{
		Client:              activeClient,
		Observer:            activeObs,
		TrackActiveLeases:   true,
		DirectInteractionID: "int_direct_notoken",
		MaxConcurrent:       2,
		PollInterval:        15 * time.Millisecond,
		LeaseTTL:            45 * time.Millisecond,
		RegisterEnvironment: func(context.Context) (string, string, error) {
			return "env_registered", "ek_registered", nil
		},
		Logger: log.New(io.Discard, "", 0),
	})

	runDone := make(chan error, 1)
	go func() {
		runDone <- wActive.Run(runCtx)
	}()

	select {
	case <-streamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for active-lease session stream to start")
	}
	select {
	case <-streamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lost lease to cancel session stream")
	}
	cancelRun()
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("wActive.Run() = %v, want nil or context.Canceled", err)
	}
	if wActive.EnvironmentID() != "env_registered" || wActive.EnvironmentKey() != "ek_registered" {
		t.Errorf("registered env = (%q, %q)", wActive.EnvironmentID(), wActive.EnvironmentKey())
	}
	if firstPollMaxNew.Load() != 1 {
		t.Errorf("firstPollMaxNew = %d, want 1", firstPollMaxNew.Load())
	}
	if !sawActiveRenewal.Load() {
		t.Error("sawActiveRenewal = false, want ActiveLeases sent on poll 2")
	}
	activeMu.Lock()
	tokensCopy := append([]string(nil), activeConnectTokens...)
	activeMu.Unlock()
	if len(tokensCopy) < 2 || tokensCopy[0] != "tok_v1" || tokensCopy[1] != "tok_v2_rotated" {
		t.Errorf("activeConnectTokens = %v, want [\"tok_v1\", \"tok_v2_rotated\"]", tokensCopy)
	}
	activeObs.mu.Lock()
	polledCounts := append([]int(nil), activeObs.leasePolledCounts...)
	activeObs.mu.Unlock()
	if len(polledCounts) == 0 || polledCounts[0] != 1 {
		t.Errorf("activeObs.leasePolledCounts = %v, want first poll count 1", polledCounts)
	}

	// Verify RegisterEnvironment is skipped when EnvironmentKey is already configured, and nil RegisterEnvironment with empty EnvironmentKey does not panic.
	var regCalled bool
	wPresetKey := NewPullWorker(PullWorkerConfig{
		Client:              errClient,
		TrackActiveLeases:   true,
		EnvironmentKey:      "ek_already_set",
		DirectInteractionID: "int_done",
		RegisterEnvironment: func(context.Context) (string, string, error) {
			regCalled = true
			return "env_no", "ek_no", nil
		},
		Logger: log.New(io.Discard, "", 0),
	})
	presetCtx, cancelPreset := context.WithCancel(t.Context())
	cancelPreset()
	_ = wPresetKey.Run(presetCtx)
	if regCalled {
		t.Error("RegisterEnvironment was called when EnvironmentKey was already non-empty")
	}

	var directSessionCompleted atomic.Bool
	wNilReg := NewPullWorker(PullWorkerConfig{
		Client: &fakeEnvClient{
			leaseFn: func(context.Context, *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
				return nil, &TransportError{StatusCode: http.StatusForbidden, Message: "unexpected lease poll without environment key"}
			},
			connectFn: func(ctx context.Context, req *ConnectInteractionRequest) (InteractionStream, error) {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if req.InteractionID != "int_done" || req.LeaseToken != "lt_direct_1" {
					return nil, errors.New("unexpected direct connect req")
				}
				return &scriptedStream{
					recvFn: func(call int) (*ServerEventBatch, error) {
						if call == 1 {
							directSessionCompleted.Store(true)
							return &ServerEventBatch{InteractionCompleted: &InteractionCompletedEvent{}}, nil
						}
						return nil, io.EOF
					},
				}, nil
			},
		},
		TrackActiveLeases:   true,
		DirectInteractionID: "int_done",
		DirectLeaseToken:    "lt_direct_1",
		Logger:              log.New(io.Discard, "", 0),
	})
	if err := wNilReg.Run(t.Context()); err != nil {
		t.Errorf("wNilReg.Run(DirectInteractionID without EnvironmentKey) = %v, want nil", err)
	}
	if !directSessionCompleted.Load() {
		t.Error("directSessionCompleted = false, want direct session to complete before Run returns")
	}
}

func TestPullWorkerPresetsAndDecoupledTrackActiveLeases(t *testing.T) {
	cliCfg := CLIPullWorkerDefaults()
	if cliCfg.MaxConcurrent != 4 || cliCfg.PollInterval != 2*time.Second || cliCfg.CommandTimeout != 2*time.Minute || !cliCfg.InBandStreamHeartbeat || !cliCfg.RetryConnectErrors || cliCfg.TrackActiveLeases {
		t.Errorf("CLIPullWorkerDefaults() = %+v", cliCfg)
	}
	trackedCfg := TrackedPullWorkerDefaults()
	if trackedCfg.MaxConcurrent != 1 || trackedCfg.PollInterval != DefaultPollBackoff || trackedCfg.LeaseTTL != DefaultLeaseTTL || trackedCfg.CommandTimeout != DefaultCommandTimeout || !trackedCfg.TrackActiveLeases || trackedCfg.RetryConnectErrors {
		t.Errorf("TrackedPullWorkerDefaults() = %+v", trackedCfg)
	}

	// Verify runTrackedLoop returns immediately on non-retryable LeaseInteractions errors instead of spinning forever.
	wFatalTracked := NewPullWorker(PullWorkerConfig{
		Client: &fakeEnvClient{
			leaseFn: func(context.Context, *LeaseInteractionsRequest) (*LeaseInteractionsResult, error) {
				return nil, &TransportError{StatusCode: http.StatusForbidden, Message: "forbidden key"}
			},
		},
		TrackActiveLeases: true,
		EnvironmentKey:    "ek_bad",
		PollInterval:      10 * time.Millisecond,
		Logger:            log.New(io.Discard, "", 0),
	})
	if err := wFatalTracked.Run(t.Context()); err == nil || !IsNonRetryableError(err) {
		t.Errorf("wFatalTracked.Run(403) = %v, want non-retryable 403 error", err)
	}

	// Enabling TrackActiveLeases alone must NOT implicitly enable extended FS tools or disable write_to_file overwrite,
	// and omitted CommandTimeout must default to DefaultCommandTimeout rather than leaving bash unbounded.
	rootDir := t.TempDir()
	fs := NewSandboxFS(rootDir, rootDir)
	wDecoupled := NewPullWorker(PullWorkerConfig{
		FS:                fs,
		TrackActiveLeases: true,
		Logger:            log.New(io.Discard, "", 0),
	})
	if wDecoupled.tools.cfg.CommandTimeout != DefaultCommandTimeout {
		t.Errorf("wDecoupled.tools.cfg.CommandTimeout = %v, want %v", wDecoupled.tools.cfg.CommandTimeout, DefaultCommandTimeout)
	}
	if _, isErr := wDecoupled.tools.Dispatch(t.Context(), "mkdir", map[string]any{"path": "sub"}); !isErr {
		t.Error("Dispatch(mkdir) succeeded with TrackActiveLeases=true and default Tools, want unsupported tool error")
	}
	wCustomTimeout := NewPullWorker(PullWorkerConfig{
		FS:             fs,
		CommandTimeout: 7 * time.Second,
		Logger:         log.New(io.Discard, "", 0),
	})
	if wCustomTimeout.tools.cfg.CommandTimeout != 7*time.Second {
		t.Errorf("wCustomTimeout.tools.cfg.CommandTimeout = %v, want 7s", wCustomTimeout.tools.cfg.CommandTimeout)
	}
}

type ctxBlockingStream struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
	cancOnce sync.Once
}

func (s *ctxBlockingStream) Send(context.Context, *ClientEventBatch) error { return nil }

func (s *ctxBlockingStream) Recv(ctx context.Context) (*ServerEventBatch, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	s.cancOnce.Do(func() { close(s.canceled) })
	return nil, ctx.Err()
}

func (s *ctxBlockingStream) CloseSend() error { return nil }
func (s *ctxBlockingStream) Close() error     { return nil }
