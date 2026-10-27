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
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	contentpb "google3/learning/genai/api/interactions/proto/content_go_proto"
	eventspb "google3/learning/genai/api/interactions/proto/events_go_proto"
	hookspb "google3/learning/genai/api/interactions/proto/hooks_go_proto"
	stepspb "google3/learning/genai/api/interactions/proto/steps_go_proto"
	"google3/third_party/golang/protobuf/v2/proto/proto"
)

type customStringer struct{}

func (customStringer) String() string { return "custom-val" }

type failAfterNWriter struct {
	remaining int
	err       error
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	w.remaining--
	return len(p), nil
}

func TestHasNonHandshakeContent(t *testing.T) {
	if HasNonHandshakeContent(nil) {
		t.Error("HasNonHandshakeContent(nil) = true, want false")
	}
	if HasNonHandshakeContent(&ServerEventBatch{
		InteractionReconnected: &InteractionReconnectedEvent{NextInputSeqNum: 1},
	}) {
		t.Error("HasNonHandshakeContent(handshake-only) = true, want false")
	}
	cases := []*ServerEventBatch{
		{Events: []*ServerEvent{{SeqNum: 1}}},
		{AckInputSeqNum: 2},
		{InteractionCompleted: &InteractionCompletedEvent{}},
		{ReconnectionRequired: &ReconnectionRequiredEvent{}},
		{Yield: &YieldEvent{NextInputSeqNum: 1}},
	}
	for i, tc := range cases {
		if !HasNonHandshakeContent(tc) {
			t.Errorf("case %d: HasNonHandshakeContent(%+v) = false, want true", i, tc)
		}
	}
}

func TestGRPCFramingAndStatusMessageDecode(t *testing.T) {
	var buf bytes.Buffer
	wantPayload := []byte("hello-grpc-frame")
	if err := GRPCWriteFrame(&buf, wantPayload); err != nil {
		t.Fatalf("GRPCWriteFrame(non-empty) failed: %v", err)
	}
	if err := GRPCWriteFrame(&buf, nil); err != nil {
		t.Fatalf("GRPCWriteFrame(empty) failed: %v", err)
	}

	got1, err := GRPCReadFrame(&buf)
	if err != nil || !bytes.Equal(got1, wantPayload) {
		t.Fatalf("GRPCReadFrame(1) = (%q, %v), want (%q, nil)", got1, err, wantPayload)
	}
	got2, err := GRPCReadFrame(&buf)
	if err != nil || len(got2) != 0 {
		t.Fatalf("GRPCReadFrame(2) = (%x, %v), want (empty, nil)", got2, err)
	}

	oversized := make([]byte, MaxGRPCFrameBytes+1)
	if err := GRPCWriteFrame(&buf, oversized); err == nil {
		t.Error("GRPCWriteFrame(oversized) = nil, want error")
	}

	// Compressed frame flag (1) must be rejected.
	compressedHdr := []byte{1, 0, 0, 0, 0}
	if _, err := GRPCReadFrame(bytes.NewReader(compressedHdr)); err == nil {
		t.Error("GRPCReadFrame(compressed flag=1) = nil, want error")
	}

	// Oversized frame header must be rejected before allocation.
	var hugeHdr [5]byte
	binary.BigEndian.PutUint32(hugeHdr[1:5], MaxGRPCFrameBytes+1)
	if _, err := GRPCReadFrame(bytes.NewReader(hugeHdr[:])); err == nil {
		t.Error("GRPCReadFrame(oversized header) = nil, want error")
	}

	// Truncated header, zero-byte body after non-zero header, and partial body must fail with io.ErrUnexpectedEOF.
	if _, err := GRPCReadFrame(bytes.NewReader([]byte{0, 0})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("GRPCReadFrame(short header) = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if _, err := GRPCReadFrame(bytes.NewReader([]byte{0, 0, 0, 0, 5})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("GRPCReadFrame(EOF after non-zero header) = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if _, err := GRPCReadFrame(bytes.NewReader([]byte{0, 0, 0, 0, 5, 'a'})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("GRPCReadFrame(short body) = %v, want %v", err, io.ErrUnexpectedEOF)
	}

	wErr := errors.New("write failed")
	if err := GRPCWriteFrame(failingWriter{err: wErr}, []byte("x")); !errors.Is(err, wErr) {
		t.Errorf("GRPCWriteFrame(failingWriter) = %v, want %v", err, wErr)
	}
	if err := GRPCWriteFrame(&failAfterNWriter{remaining: 1, err: wErr}, []byte("x")); !errors.Is(err, wErr) {
		t.Errorf("GRPCWriteFrame(failAfterNWriter payload) = %v, want %v", err, wErr)
	}

	for _, tc := range []struct {
		in   string
		want string
	}{
		{in: "", want: ""},
		{in: "  plain message  ", want: "plain message"},
		{in: "hello%20world%21%3a%3A", want: "hello world!::"},
		{in: "bad%2Ghex%a", want: "bad%2Ghex%a"},
		{in: "%41%66%46%30%39", want: "AfF09"},
	} {
		if got := DecodeGRPCMessage(tc.in); got != tc.want {
			t.Errorf("DecodeGRPCMessage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMarshalAndUnmarshalClientEventBatch(t *testing.T) {
	if got := MarshalClientEventBatch(nil); got != nil {
		t.Fatalf("MarshalClientEventBatch(nil) = %x, want nil", got)
	}
	if got := MarshalClientEvent(nil); got != nil {
		t.Fatalf("MarshalClientEvent(nil) = %x, want nil", got)
	}
	if got := MarshalClientEvent(&ClientEvent{SeqNum: 1}); len(got) == 0 {
		t.Error("MarshalClientEvent(SeqNum=1) = empty, want non-empty wire bytes")
	}

	rawHookReq, err := proto.Marshal(hookspb.CallHookRequest_builder{
		RequestId: "hr_raw",
		Name:      "RawHook",
	}.Build())
	if err != nil {
		t.Fatalf("proto.Marshal(CallHookRequest) failed: %v", err)
	}
	rawHookResp, err := proto.Marshal(hookspb.CallHookResponse_builder{
		RequestId:    "hr_raw_resp",
		ErrorMessage: new("raw error"),
	}.Build())
	if err != nil {
		t.Fatalf("proto.Marshal(CallHookResponse) failed: %v", err)
	}

	batch := &ClientEventBatch{
		AckOutputSeqNum: 42,
		ReconnectInteraction: &ReconnectInteractionEvent{
			InteractionID:    "int_abc",
			NextOutputSeqNum: 7,
			Background:       true,
		},
		Events: []*ClientEvent{
			nil,
			{
				SeqNum:          1,
				TimestampMicros: 1700000000,
				Payload: &EventPayload{
					SubInteractionID: "sub_1",
					Step: &Step{
						ToolCall: &ToolCallStep{
							ID: "tc_1",
							FunctionCall: &FunctionCallStep{
								Name: "execute_bash",
								Arguments: map[string]any{
									"cmd":  "echo hi",
									"flag": true,
									"num":  float64(3),
								},
							},
						},
					},
				},
			},
			{
				SeqNum:          2,
				TimestampMicros: 1700000001,
				Payload: &EventPayload{
					Step: &Step{
						ToolResult: &ToolResultStep{
							CallID: "tc_1",
							FunctionResult: &FunctionResultStep{
								Name:    "execute_bash",
								IsError: true,
								Result: map[string]any{
									"exit_code": float64(1),
									"stderr":    "boom",
								},
							},
						},
					},
				},
			},
			{
				SeqNum: 3,
				Payload: &EventPayload{
					FunctionInvocation: &FunctionInvocation{
						CallID:    "fi_1",
						Name:      "read_file",
						Arguments: map[string]any{"path": "a.txt"},
					},
				},
			},
			{
				SeqNum: 4,
				Payload: &EventPayload{
					FunctionResult: &FunctionResultEvent{
						CallID:  "fi_1",
						Name:    "read_file",
						IsError: false,
						Result:  "contents",
					},
				},
			},
			{
				SeqNum: 5,
				Payload: &EventPayload{
					CallHookRequest: &CallHookRequest{
						RequestID: "hr_1",
						Name:      "pre_tool",
						HookType:  2,
					},
				},
			},
			{
				SeqNum: 6,
				Payload: &EventPayload{
					CallHookResponse: &CallHookResponse{
						RequestID:    "hr_1",
						EmptyResult:  true,
						ErrorMessage: "hook err",
					},
				},
			},
			{
				SeqNum: 7,
				Payload: &EventPayload{
					CallHookRequest: &CallHookRequest{
						RawWire: rawHookReq,
					},
				},
			},
			{
				SeqNum: 8,
				Payload: &EventPayload{
					CallHookResponse: &CallHookResponse{
						RawWire: rawHookResp,
					},
				},
			},
			{
				SeqNum: 9,
				Payload: &EventPayload{
					CallHookResponse: &CallHookResponse{
						RequestID:   "hr_empty_only",
						EmptyResult: true,
					},
				},
			},
		},
	}

	raw := MarshalClientEventBatch(batch)
	got, err := UnmarshalClientEventBatch(raw)
	if err != nil {
		t.Fatalf("UnmarshalClientEventBatch() failed: %v", err)
	}
	if got.AckOutputSeqNum != 42 {
		t.Errorf("AckOutputSeqNum = %d, want 42", got.AckOutputSeqNum)
	}
	if got.ReconnectInteraction == nil || got.ReconnectInteraction.InteractionID != "int_abc" || got.ReconnectInteraction.NextOutputSeqNum != 7 || !got.ReconnectInteraction.Background {
		t.Errorf("ReconnectInteraction = %+v", got.ReconnectInteraction)
	}
	if len(got.Events) != 9 {
		t.Fatalf("len(Events) = %d, want 9", len(got.Events))
	}

	ev0 := got.Events[0]
	if ev0.SeqNum != 1 || ev0.TimestampMicros != 1700000000 || ev0.Payload == nil || ev0.Payload.SubInteractionID != "sub_1" {
		t.Errorf("Events[0] = %+v", ev0)
	}
	if ev0.Payload.Step == nil || ev0.Payload.Step.ToolCall == nil || ev0.Payload.Step.ToolCall.ID != "tc_1" || ev0.Payload.Step.ToolCall.FunctionCall.Name != "execute_bash" {
		t.Errorf("Events[0].Step.ToolCall = %+v", ev0.Payload.Step)
	}

	ev1 := got.Events[1]
	if ev1.Payload.Step == nil || ev1.Payload.Step.ToolResult == nil || !ev1.Payload.Step.ToolResult.FunctionResult.IsError {
		t.Errorf("Events[1].Step.ToolResult = %+v", ev1.Payload.Step)
	}

	ev2 := got.Events[2]
	if ev2.Payload.FunctionInvocation == nil || ev2.Payload.FunctionInvocation.CallID != "fi_1" || ev2.Payload.FunctionInvocation.Arguments["path"] != "a.txt" {
		t.Errorf("Events[2].FunctionInvocation = %+v", ev2.Payload.FunctionInvocation)
	}

	ev3 := got.Events[3]
	if ev3.Payload.FunctionResult == nil || ev3.Payload.FunctionResult.Result != "contents" {
		t.Errorf("Events[3].FunctionResult = %+v", ev3.Payload.FunctionResult)
	}

	ev4 := got.Events[4]
	if ev4.Payload.CallHookRequest == nil || ev4.Payload.CallHookRequest.RequestID != "hr_1" || ev4.Payload.CallHookRequest.Name != "pre_tool" || ev4.Payload.CallHookRequest.HookType != 2 {
		t.Errorf("Events[4].CallHookRequest = %+v", ev4.Payload.CallHookRequest)
	}

	ev5 := got.Events[5]
	if ev5.Payload.CallHookResponse == nil || ev5.Payload.CallHookResponse.RequestID != "hr_1" || ev5.Payload.CallHookResponse.ErrorMessage != "hook err" {
		t.Errorf("Events[5].CallHookResponse = %+v", ev5.Payload.CallHookResponse)
	}

	ev6 := got.Events[6]
	if ev6.Payload.CallHookRequest == nil || ev6.Payload.CallHookRequest.RequestID != "hr_raw" || ev6.Payload.CallHookRequest.Name != "RawHook" {
		t.Errorf("Events[6].CallHookRequest(RawWire) = %+v", ev6.Payload.CallHookRequest)
	}

	ev7 := got.Events[7]
	if ev7.Payload.CallHookResponse == nil || ev7.Payload.CallHookResponse.RequestID != "hr_raw_resp" || ev7.Payload.CallHookResponse.ErrorMessage != "raw error" {
		t.Errorf("Events[7].CallHookResponse(RawWire) = %+v", ev7.Payload.CallHookResponse)
	}

	ev8 := got.Events[8]
	if ev8.Payload.CallHookResponse == nil || ev8.Payload.CallHookResponse.RequestID != "hr_empty_only" || !ev8.Payload.CallHookResponse.EmptyResult {
		t.Errorf("Events[8].CallHookResponse(EmptyResult) = %+v", ev8.Payload.CallHookResponse)
	}

	// Verify standalone Disconnect in ClientEventBatch.
	discRaw := MarshalClientEventBatch(&ClientEventBatch{
		AckOutputSeqNum: 10,
		Disconnect:      &DisconnectEvent{},
	})
	discGot, err := UnmarshalClientEventBatch(discRaw)
	if err != nil || discGot.Disconnect == nil || discGot.AckOutputSeqNum != 10 {
		t.Errorf("UnmarshalClientEventBatch(Disconnect) = (%+v, %v)", discGot, err)
	}
}

func TestMarshalAndUnmarshalServerEventBatch(t *testing.T) {
	if got := MarshalServerEventBatch(nil); got != nil {
		t.Fatalf("MarshalServerEventBatch(nil) = %x, want nil", got)
	}

	srvBatch := &ServerEventBatch{
		AckInputSeqNum: 19,
		InteractionReconnected: &InteractionReconnectedEvent{
			NextInputSeqNum: 4,
		},
		InteractionCompleted: &InteractionCompletedEvent{
			Status: &RPCStatus{Code: 3, Message: "invalid argument"},
		},
		ReconnectionRequired: &ReconnectionRequiredEvent{},
		Yield:                &YieldEvent{NextInputSeqNum: 8},
		Events: []*ServerEvent{
			nil,
			{
				SeqNum:          11,
				TimestampMicros: 99,
				Payload: &EventPayload{
					FunctionInvocation: &FunctionInvocation{
						CallID:    "c_srv",
						Name:      "list_dir",
						Arguments: map[string]any{"dir": "."},
					},
				},
			},
		},
	}

	raw := MarshalServerEventBatch(srvBatch)
	got, err := UnmarshalServerEventBatch(raw)
	if err != nil {
		t.Fatalf("UnmarshalServerEventBatch() failed: %v", err)
	}
	if got.AckInputSeqNum != 19 {
		t.Errorf("AckInputSeqNum = %d, want 19", got.AckInputSeqNum)
	}
	if got.Yield == nil || got.Yield.NextInputSeqNum != 8 {
		t.Errorf("Yield = %+v", got.Yield)
	}
	if len(got.Events) != 1 || got.Events[0].SeqNum != 11 || got.Events[0].Payload.FunctionInvocation.Name != "list_dir" {
		t.Errorf("Events = %+v", got.Events)
	}

	// Verify individual control-event branches on ServerEventBatch.
	reconnBatch, err := UnmarshalServerEventBatch(MarshalServerEventBatch(&ServerEventBatch{
		InteractionReconnected: &InteractionReconnectedEvent{NextInputSeqNum: 5},
	}))
	if err != nil || reconnBatch.InteractionReconnected == nil || reconnBatch.InteractionReconnected.NextInputSeqNum != 5 {
		t.Errorf("InteractionReconnected = (%+v, %v)", reconnBatch, err)
	}

	compBatch, err := UnmarshalServerEventBatch(MarshalServerEventBatch(&ServerEventBatch{
		InteractionCompleted: &InteractionCompletedEvent{
			Status: &RPCStatus{Code: 3, Message: "invalid argument"},
		},
	}))
	if err != nil || compBatch.InteractionCompleted == nil || compBatch.InteractionCompleted.Status == nil || compBatch.InteractionCompleted.Status.Code != 3 || compBatch.InteractionCompleted.Status.Message != "invalid argument" {
		t.Errorf("InteractionCompleted(with Status) = (%+v, %v)", compBatch, err)
	}

	compNoStatusBatch, err := UnmarshalServerEventBatch(MarshalServerEventBatch(&ServerEventBatch{
		InteractionCompleted: &InteractionCompletedEvent{},
	}))
	if err != nil || compNoStatusBatch.InteractionCompleted == nil || compNoStatusBatch.InteractionCompleted.Status != nil {
		t.Errorf("InteractionCompleted(without Status) = (%+v, %v)", compNoStatusBatch, err)
	}

	reconnReqBatch, err := UnmarshalServerEventBatch(MarshalServerEventBatch(&ServerEventBatch{
		ReconnectionRequired: &ReconnectionRequiredEvent{},
	}))
	if err != nil || reconnReqBatch.ReconnectionRequired == nil {
		t.Errorf("ReconnectionRequired = (%+v, %v)", reconnReqBatch, err)
	}

	// Verify ContentValue.Text inside FunctionResultStep.Result decodes to string.
	pbWithContentText := eventspb.ServerEventBatch_builder{
		Events: []*eventspb.ServerEvent{
			eventspb.ServerEvent_builder{
				SeqNum: 1,
				Payload: eventspb.Payload_builder{
					Step: stepspb.Step_builder{
						ToolResult: stepspb.ToolResultStep_builder{
							CallId: new("c1"),
							FunctionResult: stepspb.FunctionResultStep_builder{
								Name: new("read_file"),
								Result: contentpb.Value_builder{
									ContentValue: contentpb.Content_builder{
										Text: contentpb.TextContent_builder{
											Text: new("hello from content"),
										}.Build(),
									}.Build(),
								}.Build(),
							}.Build(),
						}.Build(),
					}.Build(),
				}.Build(),
			}.Build(),
		},
	}.Build()
	contentWire, err := proto.Marshal(pbWithContentText)
	if err != nil {
		t.Fatalf("proto.Marshal(pbWithContentText) failed: %v", err)
	}
	gotContentBatch, err := UnmarshalServerEventBatch(contentWire)
	if err != nil || len(gotContentBatch.Events) != 1 || gotContentBatch.Events[0].Payload.Step.ToolResult.FunctionResult.Result != "hello from content" {
		t.Errorf("UnmarshalServerEventBatch(ContentValue.Text) = (%+v, %v)", gotContentBatch, err)
	}
}

func TestProtoStructAndValueConversionsAndCorruptWire(t *testing.T) {
	if got := mapToProtoStruct(nil); len(got.GetFields()) != 0 {
		t.Errorf("mapToProtoStruct(nil) = %v, want empty", got)
	}
	if got := protoStructToMap(nil); len(got) != 0 {
		t.Errorf("protoStructToMap(nil) = %v, want empty map", got)
	}
	if got := protoValueToAny(nil); got != nil {
		t.Errorf("protoValueToAny(nil) = %v, want nil", got)
	}
	if got := protoValueToAny(&contentpb.Value{}); got != nil {
		t.Errorf("protoValueToAny(empty Value) = %v, want nil", got)
	}
	if got := protoValueToAny(contentpb.Value_builder{ContentValue: &contentpb.Content{}}.Build()); got != "" {
		t.Errorf("protoValueToAny(empty ContentValue) = %v, want empty string", got)
	}

	in := map[string]any{
		"nil_val":    nil,
		"str_val":    "text",
		"bool_t":     true,
		"bool_f":     false,
		"int_v":      int(-5),
		"int32_v":    int32(12),
		"int64_v":    int64(34),
		"uint_v":     uint(56),
		"uint32_v":   uint32(78),
		"uint64_v":   uint64(90),
		"float32_v":  float32(1.5),
		"float64_v":  float64(2.5),
		"json_num":   json.Number("42.25"),
		"json_bad":   json.Number("not-a-float"),
		"str_map":    map[string]string{"k1": "v1", "k2": "v2"},
		"nested_map": map[string]any{"inner": "ok"},
		"any_slice":  []any{"a", float64(2), true, nil},
		"str_slice":  []string{"s1", "s2"},
		"stringer":   customStringer{},
	}

	got := protoStructToMap(mapToProtoStruct(in))
	want := map[string]any{
		"nil_val":    nil,
		"str_val":    "text",
		"bool_t":     true,
		"bool_f":     false,
		"int_v":      float64(-5),
		"int32_v":    float64(12),
		"int64_v":    float64(34),
		"uint_v":     float64(56),
		"uint32_v":   float64(78),
		"uint64_v":   float64(90),
		"float32_v":  float64(1.5),
		"float64_v":  float64(2.5),
		"json_num":   float64(42.25),
		"json_bad":   "not-a-float",
		"str_map":    map[string]any{"k1": "v1", "k2": "v2"},
		"nested_map": map[string]any{"inner": "ok"},
		"any_slice":  []any{"a", float64(2), true, nil},
		"str_slice":  []any{"s1", "s2"},
		"stringer":   "custom-val",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("protoStructToMap(mapToProtoStruct()) = %#v\nwant %#v", got, want)
	}

	// Corrupt wire inputs must return an error without panicking.
	for _, bad := range [][]byte{
		{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01},
		{0x0a, 0x10, 'x'},
		{0x0e},
	} {
		if _, err := UnmarshalServerEventBatch(bad); err == nil {
			t.Errorf("UnmarshalServerEventBatch(%x) = nil, want error", bad)
		}
		if _, err := UnmarshalClientEventBatch(bad); err == nil {
			t.Errorf("UnmarshalClientEventBatch(%x) = nil, want error", bad)
		}
	}
}

func TestEventPayloadCloneAndNopObserver(t *testing.T) {
	var nilPayload *EventPayload
	if nilPayload.Clone() != nil {
		t.Error("(*EventPayload)(nil).Clone() != nil")
	}

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
		SubInteractionID: "sub",
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
		FunctionInvocation: &FunctionInvocation{
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
	cloned.FunctionInvocation.Name = "mutated_fn2"
	cloned.FunctionInvocation.Arguments["a"] = "mutated"
	cloned.FunctionResult.Name = "mutated_fn2"
	cloned.FunctionResult.Result.([]string)[0] = "mutated"
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
	if orig.FunctionInvocation.Name != "fn2" || orig.FunctionInvocation.Arguments["a"] != "b" {
		t.Errorf("Clone() shared FunctionInvocation with original: %+v", orig.FunctionInvocation)
	}
	if orig.FunctionResult.Name != "fn2" || orig.FunctionResult.Result.([]string)[0] != "s1" {
		t.Errorf("Clone() shared FunctionResult with original: %+v", orig.FunctionResult)
	}
	if orig.CallHookRequest.RawWire[0] != 1 || orig.CallHookResponse.RawWire[0] != 3 {
		t.Error("Clone() shared RawWire byte slice with original")
	}

	rawBytesPayload := &EventPayload{
		FunctionInvocation: &FunctionInvocation{CallID: "f_nil_map"},
		FunctionResult:     &FunctionResultEvent{CallID: "f_bytes", Result: []byte{7, 8}},
	}
	clonedBytesPayload := rawBytesPayload.Clone()
	clonedBytesPayload.FunctionResult.Result.([]byte)[0] = 77
	if rawBytesPayload.FunctionResult.Result.([]byte)[0] != 7 {
		t.Error("Clone() shared []byte Result slice with original")
	}
	if clonedBytesPayload.FunctionInvocation.Arguments != nil {
		t.Errorf("Clone() with nil Arguments = %#v, want nil", clonedBytesPayload.FunctionInvocation.Arguments)
	}

	fallbackBatch := MarshalClientEventBatch(&ClientEventBatch{
		Events: []*ClientEvent{
			{
				SeqNum: 1,
				Payload: &EventPayload{
					CallHookRequest: &CallHookRequest{
						RequestID: "hr_fallback",
						Name:      "FallbackHook",
						RawWire:   []byte{0x0e},
					},
				},
			},
			{
				SeqNum: 2,
				Payload: &EventPayload{
					CallHookResponse: &CallHookResponse{
						RequestID:   "hr_resp_fallback",
						EmptyResult: true,
						RawWire:     []byte{0x0e},
					},
				},
			},
		},
	})
	decodedFallback, err := UnmarshalClientEventBatch(fallbackBatch)
	if err != nil {
		t.Fatalf("UnmarshalClientEventBatch(fallbackBatch) failed: %v", err)
	}
	if len(decodedFallback.Events) != 2 ||
		decodedFallback.Events[0].Payload.CallHookRequest.RequestID != "hr_fallback" ||
		decodedFallback.Events[1].Payload.CallHookResponse.RequestID != "hr_resp_fallback" {
		t.Errorf("decodedFallback = %+v, want fallback CallHookRequest/CallHookResponse fields", decodedFallback.Events)
	}

	var obs NopObserver
	obs.LeasePolled(1, nil)
	obs.WorkerProvisioned(true, nil)
	obs.HandoffFinished("accepted")
	obs.InteractionStarted("int_1")
	obs.InteractionFinished("int_1", nil)
}
