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
	"cmp"
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type stubStreamClient struct {
	gotReq *ConnectInteractionRequest
	stream InteractionStream
	err    error
}

func (s *stubStreamClient) ConnectInteraction(_ context.Context, req *ConnectInteractionRequest) (InteractionStream, error) {
	cp := *req
	s.gotReq = &cp
	return s.stream, s.err
}

func TestStreamClientDefaultAndRPCDelegation(t *testing.T) {
	defaultClient := NewHTTPClient(HTTPClientConfig{BaseURL: "https://generativelanguage.googleapis.com"})
	if _, ok := defaultClient.RPCStreamer().(EmptyStreamClient); !ok {
		t.Errorf("defaultClient.RPCStreamer() = %T, want EmptyStreamClient", defaultClient.RPCStreamer())
	}
	if _, err := (EmptyStreamClient{}).ConnectInteraction(t.Context(), &ConnectInteractionRequest{InteractionID: "int_1"}); !errors.Is(err, ErrStreamClientUnconfigured) {
		t.Errorf("EmptyStreamClient.ConnectInteraction() err = %v, want %v", err, ErrStreamClientUnconfigured)
	}
	if !IsNonRetryableError(ErrStreamClientUnconfigured) {
		t.Errorf("IsNonRetryableError(ErrStreamClientUnconfigured) = false, want true")
	}

	nilDialerClient := NewHTTPClient(HTTPClientConfig{
		BaseURL: "https://generativelanguage.googleapis.com",
		StreamDialer: func(_ context.Context, _ string, _ http.Header, _ *ConnectInteractionRequest) (InteractionStream, error) {
			return nil, nil
		},
	})
	if gotStream, err := nilDialerClient.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID:     "int_nil_dialer",
		HeartbeatInterval: time.Second,
	}); err != nil || gotStream != nil {
		if gotStream != nil {
			_ = gotStream.Close()
		}
		t.Errorf("ConnectInteraction(nil StreamDialer result with HeartbeatInterval > 0) = (%v, %v), want (nil, nil)", gotStream, err)
	}

	unconfiguredRPC := NewHTTPClient(HTTPClientConfig{
		BaseURL:        "https://generativelanguage.googleapis.com",
		UseRPCStreamer: true,
	})
	if _, err := unconfiguredRPC.ConnectInteraction(t.Context(), &ConnectInteractionRequest{InteractionID: "int_1"}); !errors.Is(err, ErrStreamClientUnconfigured) {
		t.Errorf("HTTPClient(UseRPCStreamer with default EmptyStreamClient).ConnectInteraction() err = %v, want %v", err, ErrStreamClientUnconfigured)
	}

	stubRPC := &stubStreamClient{}
	configuredRPC := NewHTTPClient(HTTPClientConfig{
		BaseURL:        "https://generativelanguage.googleapis.com",
		EnvironmentID:  "env_cfg",
		EnvironmentKey: "ek_cfg",
		WorkerID:       "worker_cfg",
		RPCStreamer:    stubRPC,
		UseRPCStreamer: true,
	})
	if _, err := configuredRPC.ConnectInteraction(t.Context(), &ConnectInteractionRequest{
		InteractionID: "int_rpc_1",
		LeaseToken:    "lt_rpc_1",
	}); err != nil {
		t.Fatalf("HTTPClient(UseRPCStreamer with custom StreamClient).ConnectInteraction() err = %v, want nil", err)
	}
	if stubRPC.gotReq == nil || stubRPC.gotReq.InteractionID != "int_rpc_1" || stubRPC.gotReq.EnvironmentID != "env_cfg" || stubRPC.gotReq.EnvironmentKey != "ek_cfg" || stubRPC.gotReq.WorkerID != "worker_cfg" || stubRPC.gotReq.ClientID != DefaultEnvironmentClientID || stubRPC.gotReq.NextOutputSeqNum != 1 {
		t.Errorf("stubRPC.gotReq = %+v, want populated ConnectInteractionRequest", stubRPC.gotReq)
	}
}

func TestDecodeGRPCMessage(t *testing.T) {
	if got := DecodeGRPCMessage("  invalid%20lease%20token  "); got != "invalid lease token" {
		t.Errorf("DecodeGRPCMessage(escaped) = %q, want %q", got, "invalid lease token")
	}
	if got := DecodeGRPCMessage("  bad%zzescape  "); got != "bad%zzescape" {
		t.Errorf("DecodeGRPCMessage(malformed escape) = %q, want %q", got, "bad%zzescape")
	}
}

func TestEventPayloadCloneAndNopObserver(t *testing.T) {
	var nilPayload *EventPayload
	if nilPayload.Clone() != nil {
		t.Error("(*EventPayload)(nil).Clone() != nil")
	}

	// Verify Clone handles nil FunctionCall and nil FunctionResult inside Step without panicking.
	nilStepChildren := (&EventPayload{
		Step: &Step{
			ToolCall:   &ToolCallStep{ID: "c0"},
			ToolResult: &ToolResultStep{CallID: "c0"},
		},
	}).Clone()
	if nilStepChildren.Step.ToolCall.FunctionCall != nil || nilStepChildren.Step.ToolResult.FunctionResult != nil {
		t.Errorf("Clone(nil FunctionCall/FunctionResult) = %+v, want nil children", nilStepChildren.Step)
	}

	orig := &EventPayload{
		Step: &Step{
			ToolCall: &ToolCallStep{
				ID: "c1",
				FunctionCall: &FunctionCallStep{
					Name:      "fn",
					Arguments: map[string]any{"nested": map[string]any{"k": "v"}, "arr": []any{map[string]any{"inner": "x"}}},
				},
			},
			ToolResult: &ToolResultStep{
				CallID: "c1",
				FunctionResult: &FunctionResultStep{
					Name:   "fn",
					Result: map[string]any{"out": "ok"},
				},
			},
		},
		FunctionCall: &FunctionCallEvent{
			CallID:    "f1",
			Name:      "fn2",
			Arguments: map[string]any{"a": "b"},
		},
		FunctionResult: &FunctionResultEvent{
			CallID: "f1",
			Name:   "fn2",
			Result: []string{"s1"},
		},
		CallHookRequest:  &CallHookRequest{RequestID: "h1", RawWire: []byte{1, 2}},
		CallHookResponse: &CallHookResponse{RequestID: "h1", RawWire: []byte{3, 4}},
	}

	cloned := orig.Clone()
	cloned.Step.ToolCall.FunctionCall.Arguments["nested"].(map[string]any)["k"] = "mutated"
	cloned.Step.ToolCall.FunctionCall.Arguments["arr"].([]any)[0].(map[string]any)["inner"] = "mutated"
	cloned.Step.ToolResult.FunctionResult.Name = "mutated_fn"
	cloned.Step.ToolResult.FunctionResult.Result.(map[string]any)["out"] = "mutated"
	cloned.FunctionCall.Name = "mutated_fn2"
	cloned.FunctionCall.Arguments["a"] = "mutated"
	cloned.FunctionResult.Name = "mutated_fn2"
	cloned.FunctionResult.Result.([]string)[0] = "s1_mutated"
	cloned.CallHookRequest.RawWire[0] = 99
	cloned.CallHookResponse.RawWire[0] = 88

	if orig.Step.ToolCall.FunctionCall.Arguments["nested"].(map[string]any)["k"] != "v" {
		t.Error("Clone() shared nested map reference with original")
	}
	if orig.Step.ToolCall.FunctionCall.Arguments["arr"].([]any)[0].(map[string]any)["inner"] != "x" {
		t.Error("Clone() shared nested slice-element map reference with original")
	}
	if orig.Step.ToolResult.FunctionResult.Name != "fn" || orig.Step.ToolResult.FunctionResult.Result.(map[string]any)["out"] != "ok" {
		t.Errorf("Clone() shared Step.ToolResult.FunctionResult with original: %+v", orig.Step.ToolResult.FunctionResult)
	}
	if orig.FunctionCall.Name != "fn2" || orig.FunctionCall.Arguments["a"] != "b" {
		t.Errorf("Clone() shared FunctionCall with original: %+v", orig.FunctionCall)
	}
	if orig.FunctionResult.Name != "fn2" || orig.FunctionResult.Result.([]string)[0] != "s1" {
		t.Errorf("Clone() shared FunctionResult with original: %+v", orig.FunctionResult)
	}
	if orig.CallHookRequest.RawWire[0] != 1 || orig.CallHookResponse.RawWire[0] != 3 {
		t.Error("Clone() shared RawWire byte slice with original")
	}

	var obs NopObserver
	obs.LeasePolled(1, nil)
	obs.WorkerProvisioned(true, nil)
	obs.HandoffFinished("accepted")
	obs.InteractionStarted("int_1")
	obs.InteractionFinished("int_1", nil)
}

func TestEventBatchJSONRoundTripAndWSCloseMapping(t *testing.T) {
	// Test wsCloseToTransportError mapping across close codes and reason strings.
	closeMappingCases := []struct {
		code       int
		reason     string
		wantStatus int
		wantGRPC   int
		wantMsg    string
	}{
		{code: 1002, reason: "protocol error", wantStatus: http.StatusBadRequest, wantGRPC: 3},
		{code: 1003, reason: "unsupported data", wantStatus: http.StatusBadRequest, wantGRPC: 3},
		{code: 1007, reason: "invalid frame", wantStatus: http.StatusBadRequest, wantGRPC: 3},
		{code: 1009, reason: "message too big", wantStatus: http.StatusBadRequest, wantGRPC: 3},
		{code: 1001, reason: "going away", wantStatus: http.StatusServiceUnavailable, wantGRPC: 14},
		{code: 1012, reason: "service restart", wantStatus: http.StatusServiceUnavailable, wantGRPC: 14},
		{code: 1013, reason: "try again later", wantStatus: http.StatusServiceUnavailable, wantGRPC: 14},
		{code: 1014, reason: "bad gateway", wantStatus: http.StatusServiceUnavailable, wantGRPC: 14},
		{code: 1008, reason: "UNAUTHENTICATED: bad key", wantStatus: http.StatusUnauthorized, wantGRPC: 16},
		{code: 1008, reason: "missing required credentials", wantStatus: http.StatusUnauthorized, wantGRPC: 16},
		{code: 1008, reason: "NOT_FOUND: missing interaction", wantStatus: http.StatusNotFound, wantGRPC: 5},
		{code: 1008, reason: "interaction not found", wantStatus: http.StatusNotFound, wantGRPC: 5},
		{code: 1008, reason: "ALREADY_EXISTS: duplicate", wantStatus: http.StatusConflict, wantGRPC: 10},
		{code: 1008, reason: "aborted: conflict", wantStatus: http.StatusConflict, wantGRPC: 10},
		{code: 1008, reason: "UNIMPLEMENTED: method unsupported", wantStatus: http.StatusNotImplemented, wantGRPC: 12},
		{code: 1008, reason: "feature not enabled", wantStatus: http.StatusNotImplemented, wantGRPC: 12},
		{code: 1008, reason: "endpoint is disabled", wantStatus: http.StatusNotImplemented, wantGRPC: 12},
		{code: 1008, reason: "PERMISSION_DENIED: invalid lease", wantStatus: http.StatusForbidden, wantGRPC: 7},
		{code: 1011, reason: "RESOURCE_EXHAUSTED: limit hit", wantStatus: http.StatusTooManyRequests, wantGRPC: 8},
		{code: 1011, reason: "monthly quota exceeded", wantStatus: http.StatusTooManyRequests, wantGRPC: 8},
		{code: 1011, reason: "rate limit exceeded", wantStatus: http.StatusTooManyRequests, wantGRPC: 8},
		{code: 1011, reason: "DEADLINE_EXCEEDED: timed out", wantStatus: http.StatusGatewayTimeout, wantGRPC: 4},
		{code: 1011, reason: "deadline exceeded", wantStatus: http.StatusGatewayTimeout, wantGRPC: 4},
		{code: 1011, reason: "UNAVAILABLE: backend down", wantStatus: http.StatusServiceUnavailable, wantGRPC: 14},
		{code: 1011, reason: "UNAVAILABLE: backend address not found", wantStatus: http.StatusServiceUnavailable, wantGRPC: 14},
		{code: 1011, reason: "UNAUTHENTICATED: expired", wantStatus: http.StatusUnauthorized, wantGRPC: 16},
		{code: 1011, reason: "PERMISSION_DENIED: denied", wantStatus: http.StatusForbidden, wantGRPC: 7},
		{code: 1011, reason: "permission denied", wantStatus: http.StatusForbidden, wantGRPC: 7},
		{code: 1011, reason: "NOT_FOUND: gone", wantStatus: http.StatusNotFound, wantGRPC: 5},
		{code: 1011, reason: "ALREADY_EXISTS: dup", wantStatus: http.StatusConflict, wantGRPC: 10},
		{code: 1011, reason: "ABORTED: conflict", wantStatus: http.StatusConflict, wantGRPC: 10},
		{code: 1011, reason: "upstream endpoint not found", wantStatus: http.StatusInternalServerError, wantGRPC: 13},
		{code: 1011, reason: "", wantStatus: http.StatusInternalServerError, wantGRPC: 13, wantMsg: "WebSocket closed with code 1011"},
	}
	for _, tc := range closeMappingCases {
		err := wsCloseToTransportError(tc.code, tc.reason)
		var te *TransportError
		if !errors.As(err, &te) {
			t.Errorf("wsCloseToTransportError(%d, %q) = %v, want *TransportError", tc.code, tc.reason, err)
			continue
		}
		wantMsg := cmp.Or(tc.wantMsg, tc.reason)
		if te.StatusCode != tc.wantStatus || te.GRPCCode != tc.wantGRPC || te.Message != wantMsg {
			t.Errorf("wsCloseToTransportError(%d, %q) = (status=%d, grpc=%d, msg=%q), want (status=%d, grpc=%d, msg=%q)", tc.code, tc.reason, te.StatusCode, te.GRPCCode, te.Message, tc.wantStatus, tc.wantGRPC, wantMsg)
		}
	}

	// Test ClientEventBatch JSON marshal/unmarshal (empty, AckOutputSeqNum-only heartbeat, single, multi-event, golden wire literals).
	if raw, err := MarshalClientEventBatchJSON(nil); err != nil || string(raw) != "{}" {
		t.Errorf("MarshalClientEventBatchJSON(nil) = (%s, %v), want ({}, nil)", raw, err)
	}
	if rawHB, err := MarshalClientEventBatchJSON(&ClientEventBatch{AckOutputSeqNum: 42}); err != nil || string(rawHB) != "{}" {
		t.Errorf("MarshalClientEventBatchJSON(AckOutputSeqNum-only heartbeat) = (%s, %v), want ({}, nil)", rawHB, err)
	}
	if emptyBatch, err := UnmarshalClientEventBatchJSON([]byte("{}")); err != nil || emptyBatch.ReconnectInteraction != nil || len(emptyBatch.Events) != 0 {
		t.Errorf("UnmarshalClientEventBatchJSON({}) = (%+v, %v), want empty batch and nil error", emptyBatch, err)
	}

	singleConnect, err := MarshalClientEventBatchJSON(&ClientEventBatch{
		AckOutputSeqNum: 5,
		ReconnectInteraction: &ReconnectInteractionEvent{
			InteractionID:    "int_ws_1",
			NextOutputSeqNum: 4,
			Background:       true,
			ClientID:         "environment",
		},
	})
	if err != nil {
		t.Fatalf("MarshalClientEventBatchJSON(singleConnect) = %v, want nil", err)
	}
	const wantConnectJSON = `{"background":true,"event_type":"interaction.connect","id":"int_ws_1","next_output_seq_num":4}`
	if string(singleConnect) != wantConnectJSON {
		t.Errorf("MarshalClientEventBatchJSON(singleConnect) = %s, want %s", singleConnect, wantConnectJSON)
	}
	decConnect, err := UnmarshalClientEventBatchJSON(singleConnect)
	if err != nil || decConnect.ReconnectInteraction == nil || decConnect.ReconnectInteraction.InteractionID != "int_ws_1" || decConnect.ReconnectInteraction.NextOutputSeqNum != 4 || !decConnect.ReconnectInteraction.Background || decConnect.ReconnectInteraction.ClientID != "" || decConnect.AckOutputSeqNum != 0 {
		t.Errorf("UnmarshalClientEventBatchJSON(%s) = (%+v, %v), want ReconnectInteraction{id=\"int_ws_1\", next=4, bg=true, client_id=\"\"} and ack=0", singleConnect, decConnect, err)
	}

	singleFnRes, err := MarshalClientEventBatchJSON(&ClientEventBatch{
		Events: []*ClientEvent{{
			Payload: &EventPayload{
				FunctionResult: &FunctionResultEvent{
					CallID:  "call_g1",
					Name:    "execute_bash",
					IsError: false,
					Result:  map[string]any{"output": "ok"},
				},
			},
		}},
	})
	const wantFnResJSON = `{"call_id":"call_g1","event_type":"function_result","is_error":false,"name":"execute_bash","result":{"output":"ok"}}`
	if err != nil || string(singleFnRes) != wantFnResJSON {
		t.Errorf("MarshalClientEventBatchJSON(singleFnRes) = (%s, %v), want (%s, nil)", singleFnRes, err, wantFnResJSON)
	}

	// Deprecated and hidden client wire event types/fields must be ignored rather than decoded.
	legacyTypesBatch, err := UnmarshalClientEventBatchJSON([]byte(`{"ack_output_seq_num":"11","events":[{"event_type":"interaction.reconnect","id":"int_alias","interaction_id":"int_alias","next_output_seq_num":"2"},{"event_type":"tool_result","call_id":"c_t","name":"view_file","result":"hello"},{"event_type":"call_hook_response","request_id":"req_1","empty_result":true}]}`))
	if err != nil || legacyTypesBatch.AckOutputSeqNum != 0 || legacyTypesBatch.ReconnectInteraction != nil || len(legacyTypesBatch.Events) != 0 {
		t.Errorf("UnmarshalClientEventBatchJSON(deprecated client types) = (%+v, %v), want empty batch and nil error", legacyTypesBatch, err)
	}
	legacyConnectFields, err := UnmarshalClientEventBatchJSON([]byte(`{"event_type":"interaction.connect","interaction_id":"legacy_only","client_id":"environment"}`))
	if err != nil || legacyConnectFields.ReconnectInteraction == nil || legacyConnectFields.ReconnectInteraction.InteractionID != "" || legacyConnectFields.ReconnectInteraction.ClientID != "" {
		t.Errorf("UnmarshalClientEventBatchJSON(legacy connect fields) = (%+v, %v), want ReconnectInteraction with empty InteractionID and ClientID", legacyConnectFields, err)
	}

	multiClientRaw, err := MarshalClientEventBatchJSON(&ClientEventBatch{
		ReconnectInteraction: &ReconnectInteractionEvent{InteractionID: "int_m", NextOutputSeqNum: 1},
		Events: []*ClientEvent{
			{
				Payload: &EventPayload{
					Step: &Step{
						ToolResult: &ToolResultStep{
							CallID: "call_1",
							FunctionResult: &FunctionResultStep{
								Name:    "execute_bash",
								IsError: true,
								Result:  map[string]any{"output": "failed"},
							},
						},
					},
				},
			},
			{
				Payload: &EventPayload{
					CallHookResponse: &CallHookResponse{
						RequestID:    "req_ignored",
						EmptyResult:  true,
						ErrorMessage: "hook err",
					},
				},
			},
		},
		Disconnect: &DisconnectEvent{},
	})
	if err != nil {
		t.Fatalf("MarshalClientEventBatchJSON(multiClient) = %v, want nil", err)
	}
	if strings.Contains(string(multiClientRaw), "call_hook_response") {
		t.Errorf("MarshalClientEventBatchJSON(multiClient) = %s, unexpectedly emitted hidden call_hook_response", multiClientRaw)
	}
	decMultiClient, err := UnmarshalClientEventBatchJSON(multiClientRaw)
	if err != nil || decMultiClient.ReconnectInteraction == nil || len(decMultiClient.Events) != 1 || decMultiClient.Disconnect == nil {
		t.Fatalf("UnmarshalClientEventBatchJSON(%s) = (%+v, %v), want ReconnectInteraction, 1 event, and Disconnect", multiClientRaw, decMultiClient, err)
	}
	if decMultiClient.Events[0].Payload.FunctionResult == nil || !decMultiClient.Events[0].Payload.FunctionResult.IsError || decMultiClient.Events[0].Payload.FunctionResult.CallID != "call_1" {
		t.Errorf("UnmarshalClientEventBatchJSON(multiClient).Events[0].Payload = %+v, want FunctionResult{CallID:\"call_1\", IsError:true}", decMultiClient.Events[0].Payload)
	}

	// Test ServerEventBatch JSON marshal/unmarshal across canonical server events and verify deprecated/hidden wire types are omitted/ignored.
	srvMultiRaw, err := MarshalServerEventBatchJSON(&ServerEventBatch{
		AckInputSeqNum:         8,
		InteractionReconnected: &InteractionReconnectedEvent{NextInputSeqNum: 2},
		Events: []*ServerEvent{
			{
				SeqNum:          5,
				TimestampMicros: 123456,
				Payload: &EventPayload{
					Step: &Step{
						ToolCall: &ToolCallStep{
							ID: "tc_1",
							FunctionCall: &FunctionCallStep{
								Name:      "read_file",
								Arguments: map[string]any{"path": "a.txt"},
							},
						},
					},
				},
			},
			{
				SeqNum: 6,
				Payload: &EventPayload{
					CallHookRequest: &CallHookRequest{
						RequestID: "hk_ignored",
						Name:      "pre_tool",
						HookType:  2,
					},
				},
			},
		},
		InteractionCompleted: &InteractionCompletedEvent{
			Status: &RPCStatus{Code: 0, Message: "done"},
		},
		ReconnectionRequired: &ReconnectionRequiredEvent{},
		Yield:                &YieldEvent{NextInputSeqNum: 9},
	})
	if err != nil {
		t.Fatalf("MarshalServerEventBatchJSON(srvMulti) = %v, want nil", err)
	}
	if strings.Contains(string(srvMultiRaw), "call_hook_request") {
		t.Errorf("MarshalServerEventBatchJSON(srvMulti) = %s, unexpectedly emitted hidden call_hook_request", srvMultiRaw)
	}
	decSrvMulti, err := UnmarshalServerEventBatchJSON(srvMultiRaw)
	if err != nil || decSrvMulti.AckInputSeqNum != 8 || decSrvMulti.InteractionReconnected == nil || decSrvMulti.InteractionReconnected.NextInputSeqNum != 2 || len(decSrvMulti.Events) != 1 || decSrvMulti.InteractionCompleted == nil || decSrvMulti.InteractionCompleted.Status == nil || decSrvMulti.InteractionCompleted.Status.Message != "done" || decSrvMulti.ReconnectionRequired == nil || decSrvMulti.Yield == nil || decSrvMulti.Yield.NextInputSeqNum != 9 {
		t.Fatalf("UnmarshalServerEventBatchJSON(%s) = (%+v, %v), want ack=8, reconnected=2, 1 event, completed=\"done\", reconnection_required, and yield=9", srvMultiRaw, decSrvMulti, err)
	}
	if rawReconnZero, err := MarshalServerEventBatchJSON(&ServerEventBatch{InteractionReconnected: &InteractionReconnectedEvent{}}); err != nil || string(rawReconnZero) != `{"event_type":"interaction.connected"}` {
		t.Errorf("MarshalServerEventBatchJSON(InteractionReconnected zero seq) = (%s, %v), want {\"event_type\":\"interaction.connected\"}", rawReconnZero, err)
	}
	if decCreated, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"interaction.created","next_input_seq_num":"3"}`)); err != nil || decCreated.InteractionReconnected == nil || decCreated.InteractionReconnected.NextInputSeqNum != 3 {
		t.Errorf("UnmarshalServerEventBatchJSON(interaction.created) = (%+v, %v), want InteractionReconnected{NextInputSeqNum:3}", decCreated, err)
	}
	for _, emptyClient := range [][]byte{nil, []byte("  "), []byte("null")} {
		if b, err := UnmarshalClientEventBatchJSON(emptyClient); err != nil || b.ReconnectInteraction != nil || len(b.Events) != 0 {
			t.Errorf("UnmarshalClientEventBatchJSON(%q) = (%+v, %v), want empty batch and nil error", emptyClient, b, err)
		}
	}
	for _, emptySrv := range [][]byte{nil, []byte("  "), []byte(`{"interaction_id":"int_noop"}`)} {
		if b, err := UnmarshalServerEventBatchJSON(emptySrv); err != nil || b.InteractionReconnected != nil || len(b.Events) != 0 {
			t.Errorf("UnmarshalServerEventBatchJSON(%q) = (%+v, %v), want empty batch and nil error", emptySrv, b, err)
		}
	}
	if decSrvMulti.Events[0].Payload.FunctionCall == nil || decSrvMulti.Events[0].Payload.FunctionCall.Name != "read_file" {
		t.Errorf("UnmarshalServerEventBatchJSON(srvMulti).Events[0].Payload = %+v, want FunctionCall{Name:\"read_file\"}", decSrvMulti.Events[0].Payload)
	}
	if !strings.Contains(string(srvMultiRaw), `"event_type":"function_call"`) || strings.Contains(string(srvMultiRaw), `"event_type":"function_invocation"`) {
		t.Errorf("MarshalServerEventBatchJSON(srvMulti) = %s, want event_type=function_call", srvMultiRaw)
	}

	// Canonical JSON server events: function_call, interaction.cancelled, interaction.status_update, string error.code, and bare error object.
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"function_call","seq_num":"13","call_id":"call_fc_1","name":"read_file","arguments":{"path":"README.md"}}`)); err != nil || len(b.Events) != 1 || b.Events[0].SeqNum != 13 || b.Events[0].Payload.FunctionCall == nil || b.Events[0].Payload.FunctionCall.CallID != "call_fc_1" || b.Events[0].Payload.FunctionCall.Name != "read_file" || b.Events[0].Payload.FunctionCall.Arguments["path"] != "README.md" {
		t.Errorf("UnmarshalServerEventBatchJSON(function_call) = (%+v, %v), want seq=13 call_id=\"call_fc_1\" name=\"read_file\"", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"function_call","id":"legacy_id_ignored","name":"read_file"}`)); err != nil || len(b.Events) != 1 || b.Events[0].Payload.FunctionCall == nil || b.Events[0].Payload.FunctionCall.CallID != "" {
		t.Errorf("UnmarshalServerEventBatchJSON(function_call with legacy id only) = (%+v, %v), want empty CallID", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"interaction.cancelled","message":"legacy_ignored","usage":{"total_input_tokens":12}}`)); err != nil || b.InteractionCompleted == nil || b.InteractionCompleted.Status == nil || b.InteractionCompleted.Status.Code != 1 || b.InteractionCompleted.Status.Message != "" {
		t.Errorf("UnmarshalServerEventBatchJSON(interaction.cancelled) = (%+v, %v), want InteractionCompleted with Status{Code:1, Message:\"\"}}", b, err)
	}

	// Verify deprecated and hidden server wire event types/fields are ignored rather than decoded.
	for _, legacySrvJSON := range []string{
		`{"event_type":"function_invocation","seq_num":"13","call_id":"call_fi_legacy","name":"read_file","arguments":{"path":"README.md"}}`,
		`{"event_type":"step.start","seq_num":"14","step":{"type":"function_call","id":"call_99","name":"execute_bash","arguments":{"command":"ls"}}}`,
		`{"event_type":"step","seq_num":"15","type":"function_call","id":"call_flat_1","name":"execute_bash","arguments":{"command":"pwd"}}`,
		`{"event_type":"step","step":{"function_call":{"call_id":"fc_cid","name":"view_file","arguments":{"p":"a"}}}}`,
		`{"event_type":"call_hook_request","request_id":"hr_1","name":"hook","hook_type":1}`,
		`{"event_type":"interaction.reconnected","next_input_seq_num":"4"}`,
		`{"event_type":"interaction.reconnect","next_input_seq_num":"4"}`,
		`{"event_type":"interaction.complete","status":{"code":0,"message":"ok"}}`,
		`{"event_type":"interaction.canceled","message":"aborted by caller"}`,
		`{"event_type":"interaction.status_update","interaction_status":"completed"}`,
	} {
		b, err := UnmarshalServerEventBatchJSON([]byte(legacySrvJSON))
		if err != nil {
			t.Fatalf("UnmarshalServerEventBatchJSON(%s) unexpected error: %v", legacySrvJSON, err)
		}
		if len(b.Events) != 0 || b.InteractionReconnected != nil || b.InteractionCompleted != nil || b.InteractionStatusUpdate != nil {
			t.Errorf("UnmarshalServerEventBatchJSON(%s) = %+v, want legacy/hidden server event ignored", legacySrvJSON, b)
		}
	}

	for _, nonTermStatus := range []string{"in_progress", "requires_action", "idle"} {
		rawStatusJSON, err := MarshalServerEventBatchJSON(&ServerEventBatch{
			InteractionStatusUpdate: &InteractionStatusUpdate{
				InteractionID: "int_1",
				Status:        nonTermStatus,
			},
		})
		if err != nil {
			t.Fatalf("MarshalServerEventBatchJSON(status=%s) = %v, want nil", nonTermStatus, err)
		}
		b, err := UnmarshalServerEventBatchJSON(rawStatusJSON)
		if err != nil || b.InteractionCompleted != nil || b.InteractionStatusUpdate == nil || b.InteractionStatusUpdate.InteractionID != "int_1" || b.InteractionStatusUpdate.Status != nonTermStatus {
			t.Errorf("UnmarshalServerEventBatchJSON(%s) = (%+v, %v), want InteractionCompleted=nil and InteractionStatusUpdate{int_1, %s}", rawStatusJSON, b, err, nonTermStatus)
		}
	}
	if rawStatusNoID, err := MarshalServerEventBatchJSON(&ServerEventBatch{
		InteractionStatusUpdate: &InteractionStatusUpdate{Status: "idle"},
	}); err != nil || string(rawStatusNoID) != `{"event_type":"interaction.status_update","status":"idle"}` {
		t.Errorf("MarshalServerEventBatchJSON(status without interaction_id) = (%s, %v), want omitted interaction_id", rawStatusNoID, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"interaction.status_update","id":"legacy_id_ignored","status":"canceled"}`)); err != nil || b.InteractionCompleted != nil || b.InteractionStatusUpdate == nil || b.InteractionStatusUpdate.InteractionID != "" || b.InteractionStatusUpdate.Status != "canceled" {
		t.Errorf("UnmarshalServerEventBatchJSON(status_update legacy id + single-L canceled) = (%+v, %v), want InteractionCompleted=nil and InteractionID=\"\"", b, err)
	}
	for _, tc := range []struct {
		st         string
		wantStatus *RPCStatus
	}{
		{st: "completed", wantStatus: nil},
		{st: "failed", wantStatus: &RPCStatus{Code: 13}},
		{st: "cancelled", wantStatus: &RPCStatus{Code: 1}},
	} {
		payload := `{"event_type":"interaction.status_update","interaction_id":"int_1","status":"` + tc.st + `","message":"legacy_ignored"}`
		b, err := UnmarshalServerEventBatchJSON([]byte(payload))
		if err != nil || b.InteractionCompleted == nil {
			t.Errorf("UnmarshalServerEventBatchJSON(%s) = (%+v, %v), want non-nil InteractionCompleted", payload, b, err)
			continue
		}
		if !reflect.DeepEqual(b.InteractionCompleted.Status, tc.wantStatus) {
			t.Errorf("UnmarshalServerEventBatchJSON(%s).Status = %+v, want %+v", payload, b.InteractionCompleted.Status, tc.wantStatus)
		}
	}
	for _, errJSON := range []string{
		`{"event_type":"error","error":{"code":7,"message":"access denied"}}`,
		`{"event_type":"error","error":{"code":"PERMISSION_DENIED","message":"access denied"}}`,
		`{"error":{"code":"AUTHORIZATION_ERROR","message":"access denied"}}`,
	} {
		_, err := UnmarshalServerEventBatchJSON([]byte(errJSON))
		var te *TransportError
		if !errors.As(err, &te) || te.StatusCode != http.StatusForbidden || te.GRPCCode != 7 || !strings.Contains(te.Error(), "access denied") {
			t.Errorf("UnmarshalServerEventBatchJSON(%s) = %v, want 403 TransportError", errJSON, err)
		}
	}
	if _, err := UnmarshalClientEventBatchJSON([]byte(`{bad json`)); err == nil {
		t.Error("UnmarshalClientEventBatchJSON(bad json) = nil, want error")
	}
	if _, err := UnmarshalServerEventBatchJSON([]byte(`{bad json`)); err == nil {
		t.Error("UnmarshalServerEventBatchJSON(bad json) = nil, want error")
	}

	// Test nil/empty entries, zero/nil field omission, and Ack-only / single-event Ack branches in MarshalClientEventBatchJSON & MarshalServerEventBatchJSON.
	rawCliZeroConnect, err := MarshalClientEventBatchJSON(&ClientEventBatch{
		ReconnectInteraction: &ReconnectInteractionEvent{InteractionID: "int_zero"},
	})
	if err != nil || string(rawCliZeroConnect) != `{"event_type":"interaction.connect","id":"int_zero"}` {
		t.Errorf("MarshalClientEventBatchJSON(zero connect) = (%s, %v), want {\"event_type\":\"interaction.connect\",\"id\":\"int_zero\"}", rawCliZeroConnect, err)
	}
	clientWithNilEvents, err := MarshalClientEventBatchJSON(&ClientEventBatch{
		Events: []*ClientEvent{
			nil,
			{},
			{
				Payload: &EventPayload{
					FunctionResult: &FunctionResultEvent{CallID: "c_skip", Name: "fn"},
				},
			},
			{
				Payload: &EventPayload{
					Step: &Step{
						ToolResult: &ToolResultStep{
							CallID:         "c_step_skip",
							FunctionResult: &FunctionResultStep{Name: "fn_step"},
						},
					},
				},
			},
		},
	})
	if err != nil || !strings.Contains(string(clientWithNilEvents), `"call_id":"c_skip"`) || !strings.Contains(string(clientWithNilEvents), `"call_id":"c_step_skip"`) || strings.Contains(string(clientWithNilEvents), `"result"`) {
		t.Errorf("MarshalClientEventBatchJSON(with nil events & nil results) = (%s, %v), want call_id=\"c_skip\" and \"c_step_skip\" without \"result\"", clientWithNilEvents, err)
	}

	rawSrvZeroMeta, err := MarshalServerEventBatchJSON(&ServerEventBatch{
		Events: []*ServerEvent{
			nil,
			{},
			{
				SeqNum:          5,
				TimestampMicros: 9,
				Payload:         &EventPayload{},
			},
			{
				Payload: &EventPayload{
					FunctionCall: &FunctionCallEvent{CallID: "fc_z", Name: "fn"},
				},
			},
			{
				Payload: &EventPayload{
					Step: &Step{
						ToolCall: &ToolCallStep{
							ID:           "tc_z",
							FunctionCall: &FunctionCallStep{Name: "fn"},
						},
					},
				},
			},
			{
				Payload: &EventPayload{
					CallHookRequest: &CallHookRequest{RequestID: "hr_z", Name: "hk"},
				},
			},
		},
		Yield: &YieldEvent{},
	})
	for _, omittedKey := range []string{`"arguments"`, `"seq_num"`, `"timestamp_micros"`, `"next_input_seq_num"`, `"call_hook_request"`} {
		if err != nil || strings.Contains(string(rawSrvZeroMeta), omittedKey) {
			t.Errorf("MarshalServerEventBatchJSON(zero metadata) = (%s, %v), unexpectedly contains %s", rawSrvZeroMeta, err, omittedKey)
		}
	}

	if rawSrvNil, err := MarshalServerEventBatchJSON(nil); err != nil || string(rawSrvNil) != "{}" {
		t.Errorf("MarshalServerEventBatchJSON(nil) = (%s, %v), want {}", rawSrvNil, err)
	}
	if rawSrvEmpty, err := MarshalServerEventBatchJSON(&ServerEventBatch{}); err != nil || string(rawSrvEmpty) != "{}" {
		t.Errorf("MarshalServerEventBatchJSON(empty) = (%s, %v), want {}", rawSrvEmpty, err)
	}
	rawSrvAckOnly, err := MarshalServerEventBatchJSON(&ServerEventBatch{AckInputSeqNum: 7})
	if err != nil || string(rawSrvAckOnly) != `{"ack_input_seq_num":"7"}` {
		t.Errorf("MarshalServerEventBatchJSON(ack-only) = (%s, %v), want {\"ack_input_seq_num\":\"7\"}", rawSrvAckOnly, err)
	}
	rawSrvSingleAck, err := MarshalServerEventBatchJSON(&ServerEventBatch{
		AckInputSeqNum:       9,
		ReconnectionRequired: &ReconnectionRequiredEvent{},
	})
	if err != nil || string(rawSrvSingleAck) != `{"ack_input_seq_num":"9","event_type":"reconnection_required"}` {
		t.Errorf("MarshalServerEventBatchJSON(single+ack) = (%s, %v), want {\"ack_input_seq_num\":\"9\",\"event_type\":\"reconnection_required\"}", rawSrvSingleAck, err)
	}
	rawSrvDirectEvents, err := MarshalServerEventBatchJSON(&ServerEventBatch{
		Events: []*ServerEvent{
			nil,
			{},
			{
				SeqNum:          3,
				TimestampMicros: 999,
				Payload: &EventPayload{
					FunctionCall: &FunctionCallEvent{
						CallID:    "fc_1",
						Name:      "exec",
						Arguments: map[string]any{"x": 1},
					},
				},
			},
			{
				SeqNum:          4,
				TimestampMicros: 1001,
				Payload: &EventPayload{
					FunctionCall: &FunctionCallEvent{
						CallID:    "fc_2",
						Name:      "read_file",
						Arguments: map[string]any{"path": "b.txt"},
					},
				},
			},
		},
	})
	if err != nil || strings.Contains(string(rawSrvDirectEvents), "ack_input_seq_num") || !strings.Contains(string(rawSrvDirectEvents), `"timestamp_micros":"999"`) || !strings.Contains(string(rawSrvDirectEvents), `"timestamp_micros":"1001"`) {
		t.Errorf("MarshalServerEventBatchJSON(direct FunctionCall events) = (%s, %v), want timestamp_micros 999 and 1001 without ack_input_seq_num", rawSrvDirectEvents, err)
	}
	rawSrvMultiWithAck, err := MarshalServerEventBatchJSON(&ServerEventBatch{
		AckInputSeqNum:       11,
		ReconnectionRequired: &ReconnectionRequiredEvent{},
		Yield:                &YieldEvent{NextInputSeqNum: 12},
	})
	if err != nil || !strings.Contains(string(rawSrvMultiWithAck), `"ack_input_seq_num":"11"`) {
		t.Errorf("MarshalServerEventBatchJSON(multi+ack) = (%s, %v), want ack_input_seq_num=11", rawSrvMultiWithAck, err)
	}
	decSrvDirect, err := UnmarshalServerEventBatchJSON(rawSrvDirectEvents)
	wantSrvDirect := &ServerEventBatch{
		Events: []*ServerEvent{
			{
				SeqNum:          3,
				TimestampMicros: 999,
				Payload: &EventPayload{
					FunctionCall: &FunctionCallEvent{
						CallID:    "fc_1",
						Name:      "exec",
						Arguments: map[string]any{"x": float64(1)},
					},
				},
			},
			{
				SeqNum:          4,
				TimestampMicros: 1001,
				Payload: &EventPayload{
					FunctionCall: &FunctionCallEvent{
						CallID:    "fc_2",
						Name:      "read_file",
						Arguments: map[string]any{"path": "b.txt"},
					},
				},
			},
		},
	}
	if err != nil || !reflect.DeepEqual(decSrvDirect, wantSrvDirect) {
		t.Errorf("UnmarshalServerEventBatchJSON(%s) = (%+v, %v), want (%+v, nil)", rawSrvDirectEvents, decSrvDirect, err, wantSrvDirect)
	}

	// Test wrapper.Events precedence, empty/null server batches, error status/code/redaction variants, and invalid proto int64 fields.
	if b, err := UnmarshalClientEventBatchJSON([]byte(`{"events":[{"event_type":"function_result","call_id":"c1","name":"fn"}],"event_type":"interaction.connect","id":"ignored"}`)); err != nil || len(b.Events) != 1 || b.ReconnectInteraction != nil {
		t.Errorf("UnmarshalClientEventBatchJSON(events precedence) = (%+v, %v), want 1 event and nil ReconnectInteraction", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"events":[{"event_type":"reconnection_required"}],"event_type":"yield"}`)); err != nil || b.ReconnectionRequired == nil || b.Yield != nil {
		t.Errorf("UnmarshalServerEventBatchJSON(events precedence) = (%+v, %v), want ReconnectionRequired and nil Yield", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"reconnection_required","error":{"code":500,"message":"ignored"}}`)); err != nil || b.ReconnectionRequired == nil {
		t.Errorf("UnmarshalServerEventBatchJSON(explicit event_type with error field) = (%+v, %v), want ReconnectionRequired", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte("null")); err != nil || len(b.Events) != 0 {
		t.Errorf("UnmarshalServerEventBatchJSON(null) = (%+v, %v), want 0 events and nil error", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"interaction.completed","status":[1,2]}`)); err != nil || b.InteractionCompleted == nil || b.InteractionCompleted.Status != nil {
		t.Errorf("UnmarshalServerEventBatchJSON(array status) = (%+v, %v), want InteractionCompleted with nil Status", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"interaction.completed","status":null}`)); err != nil || b.InteractionCompleted == nil || b.InteractionCompleted.Status != nil {
		t.Errorf("UnmarshalServerEventBatchJSON(null status) = (%+v, %v), want InteractionCompleted with nil Status", b, err)
	}

	// Verify struct-field and Error() secret redaction (including JSON-escaped inner secrets) in wsCloseToTransportError, UnmarshalServerEventBatchJSON("error"), and NewLeaseTokenRedactor.
	closeRedactErr := wsCloseToTransportError(1008, `denied: {"lease_token":"lt_close_sec","environment_key":"ek_close_sec"}`)
	var closeTE *TransportError
	if !errors.As(closeRedactErr, &closeTE) {
		t.Fatalf("wsCloseToTransportError(1008, secret) = %v, want *TransportError", closeRedactErr)
	}
	for _, secret := range []string{"lt_close_sec", "ek_close_sec"} {
		if strings.Contains(closeTE.Op, secret) || strings.Contains(closeTE.Message, secret) || strings.Contains(closeTE.Body, secret) || strings.Contains(closeRedactErr.Error(), secret) {
			t.Errorf("wsCloseToTransportError(1008) leaked %q in (op=%q, msg=%q, body=%q, err=%q)", secret, closeTE.Op, closeTE.Message, closeTE.Body, closeRedactErr.Error())
		}
	}
	_, srvRedactErr := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"error","lease_token":"lt_body_sec","error":{"code":7,"message":"bad {\"lease_token\":\"a\\\\\\\"b_lt_srv_sec\",\"environment_key\":\"ek_srv_sec\"}"}}`))
	var srvRedactTE *TransportError
	if !errors.As(srvRedactErr, &srvRedactTE) {
		t.Fatalf("UnmarshalServerEventBatchJSON(error with secrets) = %v, want *TransportError", srvRedactErr)
	}
	for _, secret := range []string{"lt_body_sec", "lt_srv_sec", "ek_srv_sec"} {
		if strings.Contains(srvRedactTE.Op, secret) || strings.Contains(srvRedactTE.Message, secret) || strings.Contains(srvRedactTE.Body, secret) || strings.Contains(srvRedactErr.Error(), secret) {
			t.Errorf("UnmarshalServerEventBatchJSON(error) leaked %q in (op=%q, msg=%q, body=%q, err=%q)", secret, srvRedactTE.Op, srvRedactTE.Message, srvRedactTE.Body, srvRedactErr.Error())
		}
	}

	serverErrorCases := []struct {
		raw      string
		wantCode int
		wantSub  string
	}{
		{raw: `{"event_type":"error","error":{"code":429,"message":"too many"}}`, wantCode: 8, wantSub: "too many"},
		{raw: `{"event_type":"error","error":{"code":0,"status":"PERMISSION_DENIED","message":"zero code"}}`, wantCode: 7, wantSub: "zero code"},
		{raw: `{"event_type":"error","error":{"code":"INVALID_ARGUMENT","message":"bad arg"}}`, wantCode: 3, wantSub: "bad arg"},
		{raw: `{"event_type":"error","error":{"code":"invalid_request","message":"bad req"}}`, wantCode: 3, wantSub: "bad req"},
		{raw: `{"event_type":"error","error":{"code":"parameter_unknown","message":"unknown param"}}`, wantCode: 3, wantSub: "unknown param"},
		{raw: `{"event_type":"error","error":{"code":"agent_max_token_limit","message":"token cap"}}`, wantCode: 3, wantSub: "token cap"},
		{raw: `{"event_type":"error","error":{"code":"DEADLINE_EXCEEDED","message":"timeout"}}`, wantCode: 4, wantSub: "timeout"},
		{raw: `{"event_type":"error","error":{"code":"NOT_FOUND","message":"missing"}}`, wantCode: 5, wantSub: "missing"},
		{raw: `{"event_type":"error","error":{"code":"model_not_found","message":"missing model"}}`, wantCode: 5, wantSub: "missing model"},
		{raw: `{"event_type":"error","error":{"code":"ALREADY_EXISTS","message":"dup"}}`, wantCode: 10, wantSub: "dup"},
		{raw: `{"event_type":"error","error":{"code":"RESOURCE_EXHAUSTED","message":"quota"}}`, wantCode: 8, wantSub: "quota"},
		{raw: `{"event_type":"error","error":{"code":"rate_limit_exceeded","message":"rate"}}`, wantCode: 8, wantSub: "rate"},
		{raw: `{"event_type":"error","error":{"code":"too_many_requests","message":"tmr"}}`, wantCode: 8, wantSub: "tmr"},
		{raw: `{"event_type":"error","error":{"code":"UNIMPLEMENTED","message":"unimp"}}`, wantCode: 12, wantSub: "unimp"},
		{raw: `{"event_type":"error","error":{"code":"UNAVAILABLE","message":"down"}}`, wantCode: 14, wantSub: "down"},
		{raw: `{"event_type":"error","error":{"code":"service_unavailable","message":"svc down"}}`, wantCode: 14, wantSub: "svc down"},
		{raw: `{"event_type":"error","error":{"code":"UNAUTHENTICATED","message":"unauth"}}`, wantCode: 16, wantSub: "unauth"},
		{raw: `{"event_type":"error","error":{"code":"authentication","message":"bad auth"}}`, wantCode: 16, wantSub: "bad auth"},
		{raw: `{"event_type":"error","error":{"code":"INTERNAL","message":"internal"}}`, wantCode: 13, wantSub: "internal"},
		{raw: `{"event_type":"error","error":{"code":null,"status":"UNRECOGNIZED_CODE"}}`, wantCode: 13, wantSub: "server stream error"},
		{raw: `{"event_type":"error","message":"legacy_outer_ignored","error":{"code":7}}`, wantCode: 7, wantSub: "server stream error"},
		{raw: `{"event_type":"error","status":{"code":0,"message":"zero status code"}}`, wantCode: 13, wantSub: "zero status code"},
		{raw: `{"event_type":"error","status":{"code":14,"message":"status rpc unavailable"}}`, wantCode: 14, wantSub: "status rpc unavailable"},
		{raw: `{"event_type":"error","status":"RESOURCE_EXHAUSTED"}`, wantCode: 8, wantSub: "server stream error"},
	}
	for _, tc := range serverErrorCases {
		_, err := UnmarshalServerEventBatchJSON([]byte(tc.raw))
		var te *TransportError
		if !errors.As(err, &te) || te.GRPCCode != tc.wantCode || !strings.Contains(te.Message, tc.wantSub) {
			t.Errorf("UnmarshalServerEventBatchJSON(%s) = %v, want GRPCCode=%d sub=%q", tc.raw, err, tc.wantCode, tc.wantSub)
		}
	}

	for _, badClientJSON := range []string{
		`{"events":[],"event_type":123}`,
		`{"event_type":"interaction.connect","id":"i","next_output_seq_num":"not_int"}`,
	} {
		if _, err := UnmarshalClientEventBatchJSON([]byte(badClientJSON)); err == nil {
			t.Errorf("UnmarshalClientEventBatchJSON(%s) = nil, want error", badClientJSON)
		}
	}
	for _, badSrvJSON := range []string{
		`{"ack_input_seq_num":"not_int"}`,
		`{"events":[],"event_type":123}`,
		`{"event_type":"function_call","seq_num":"not_int"}`,
		`{"event_type":"function_call","timestamp_micros":"not_int"}`,
		`{"event_type":"interaction.connected","next_input_seq_num":"not_int"}`,
		`{"event_type":"yield","next_input_seq_num":"not_int"}`,
	} {
		if _, err := UnmarshalServerEventBatchJSON([]byte(badSrvJSON)); err == nil {
			t.Errorf("UnmarshalServerEventBatchJSON(%s) = nil, want error", badSrvJSON)
		}
	}
}
