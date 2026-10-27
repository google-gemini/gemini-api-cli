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
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

type failAfterNWriter struct {
	limit int
	wrote int
	err   error
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	if w.wrote+len(p) > w.limit {
		return 0, w.err
	}
	w.wrote += len(p)
	return len(p), nil
}

type errReader struct {
	prefix []byte
	err    error
	used   bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if !r.used && len(r.prefix) > 0 {
		r.used = true
		n := copy(p, r.prefix)
		return n, nil
	}
	return 0, r.err
}

type customStringer struct {
	label string
}

func (s customStringer) String() string {
	return s.label
}

func TestGRPCFramingAndDecodeMessage(t *testing.T) {
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
		t.Error("GRPCWriteFrame(MaxGRPCFrameBytes+1) = nil, want error")
	}

	writeBoom := errors.New("write boom")
	if err := GRPCWriteFrame(&failAfterNWriter{limit: 2, err: writeBoom}, []byte("abc")); !errors.Is(err, writeBoom) {
		t.Errorf("GRPCWriteFrame(header fail) = %v, want %v", err, writeBoom)
	}
	if err := GRPCWriteFrame(&failAfterNWriter{limit: 5, err: writeBoom}, []byte("abc")); !errors.Is(err, writeBoom) {
		t.Errorf("GRPCWriteFrame(payload fail) = %v, want %v", err, writeBoom)
	}

	var compressedHdr [5]byte
	compressedHdr[0] = 1
	if _, err := GRPCReadFrame(bytes.NewReader(compressedHdr[:])); err == nil {
		t.Error("GRPCReadFrame(compressed) = nil, want error")
	}

	var oversizedHdr [5]byte
	binary.BigEndian.PutUint32(oversizedHdr[1:5], MaxGRPCFrameBytes+1)
	if _, err := GRPCReadFrame(bytes.NewReader(oversizedHdr[:])); err == nil {
		t.Error("GRPCReadFrame(oversized) = nil, want error")
	}

	if _, err := GRPCReadFrame(bytes.NewReader([]byte{0, 0, 1})); err == nil {
		t.Error("GRPCReadFrame(short header) = nil, want error")
	}

	var truncatedHdr [5]byte
	binary.BigEndian.PutUint32(truncatedHdr[1:5], 10)
	if _, err := GRPCReadFrame(bytes.NewReader(append(truncatedHdr[:], 1, 2, 3))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("GRPCReadFrame(truncated payload) = %v, want ErrUnexpectedEOF", err)
	}

	readBoom := errors.New("read boom")
	var validHdr [5]byte
	binary.BigEndian.PutUint32(validHdr[1:5], 4)
	if _, err := GRPCReadFrame(&errReader{prefix: validHdr[:], err: readBoom}); !errors.Is(err, readBoom) {
		t.Errorf("GRPCReadFrame(payload read error) = %v, want %v", err, readBoom)
	}

	if got := DecodeGRPCMessage("  invalid%20lease%20token  "); got != "invalid lease token" {
		t.Errorf("DecodeGRPCMessage(escaped) = %q, want %q", got, "invalid lease token")
	}
	if got := DecodeGRPCMessage("  bad%zzescape  "); got != "bad%zzescape" {
		t.Errorf("DecodeGRPCMessage(malformed escape) = %q, want %q", got, "bad%zzescape")
	}
}

func TestMarshalAndUnmarshalClientEventBatch(t *testing.T) {
	if got := MarshalClientEventBatch(nil); got != nil {
		t.Errorf("MarshalClientEventBatch(nil) = %x, want nil", got)
	}
	if got := MarshalClientEventBatch(&ClientEventBatch{}); len(got) != 0 {
		t.Errorf("MarshalClientEventBatch(empty) = %x, want empty", got)
	}
	if got := MarshalClientEvent(nil); got != nil {
		t.Errorf("MarshalClientEvent(nil) = %x, want nil", got)
	}
	if got := MarshalClientEvent(&ClientEvent{}); len(got) != 0 {
		t.Errorf("MarshalClientEvent(empty) = %x, want empty", got)
	}

	emptyStep := MarshalClientEvent(&ClientEvent{Payload: &EventPayload{Step: &Step{}}})
	if !bytes.Equal(emptyStep, []byte{0x1a, 0x00}) {
		t.Errorf("MarshalClientEvent(empty Step) = %x, want 1a00", emptyStep)
	}
	noFnStep := MarshalClientEvent(&ClientEvent{Payload: &EventPayload{Step: &Step{ToolCall: &ToolCallStep{ID: "tc_0"}}}})
	if len(noFnStep) == 0 {
		t.Error("MarshalClientEvent(ToolCall with nil FunctionCall) returned empty")
	}

	batch := &ClientEventBatch{
		AckOutputSeqNum: 7,
		ReconnectInteraction: &ReconnectInteractionEvent{
			InteractionID:    "int_123",
			NextOutputSeqNum: 8,
			Background:       true,
			ClientID:         DefaultEnvironmentClientID,
		},
		Disconnect: &DisconnectEvent{},
		Events: []*ClientEvent{
			nil,
			{
				SeqNum:          1,
				TimestampMicros: 1700000001,
				Payload: &EventPayload{
					SubInteractionID: "sub_1",
					Step: &Step{
						ToolCall: &ToolCallStep{
							ID: "tc_1",
							FunctionCall: &FunctionCallStep{
								Name:      "view_file",
								Arguments: map[string]any{"path": "README.md"},
							},
						},
						ToolResult: &ToolResultStep{
							CallID: "tc_1",
							FunctionResult: &FunctionResultStep{
								Name:    "view_file",
								IsError: true,
								Result:  map[string]any{"error": "missing"},
							},
						},
					},
				},
			},
			{
				SeqNum: 2,
				Payload: &EventPayload{
					FunctionInvocation: &FunctionInvocation{
						CallID:    "fi_1",
						Name:      "list_dir",
						Arguments: map[string]any{"dir": "."},
					},
					FunctionResult: &FunctionResultEvent{
						CallID:  "fi_1",
						Name:    "list_dir",
						IsError: true,
						Result:  map[string]any{"entries": []any{"a.txt"}},
					},
				},
			},
			{
				SeqNum: 3,
				Payload: &EventPayload{
					CallHookRequest: &CallHookRequest{
						RequestID: "hr_1",
						Name:      "pre_tool",
						HookType:  2,
					},
					CallHookResponse: &CallHookResponse{
						RequestID:    "hr_1",
						EmptyResult:  true,
						ErrorMessage: "hook err",
					},
				},
			},
			{
				SeqNum: 4,
				Payload: &EventPayload{
					CallHookRequest:  &CallHookRequest{RawWire: appendStringField(appendStringField(nil, 1, "hr_raw"), 2, "raw_hook")},
					CallHookResponse: &CallHookResponse{RawWire: appendBoolField(appendStringField(nil, 1, "hr_raw"), 2, true)},
				},
			},
		},
	}

	raw := MarshalClientEventBatch(batch)

	// Verify ReconnectInteraction encodes ClientID on field 5 on the wire.
	rest := raw
	sawReconnectClientIDField5 := false
	for len(rest) > 0 {
		num, _, _, sub, n, err := ConsumeField(rest)
		if err != nil {
			t.Fatalf("ConsumeField(ClientEventBatch) failed: %v", err)
		}
		if num == 8 {
			inner := sub
			for len(inner) > 0 {
				inum, _, _, ib, in, ierr := ConsumeField(inner)
				if ierr != nil {
					t.Fatalf("ConsumeField(ReconnectInteraction) failed: %v", ierr)
				}
				if inum == 5 && string(ib) == DefaultEnvironmentClientID {
					sawReconnectClientIDField5 = true
				}
				if inum == 4 {
					t.Errorf("ReconnectInteraction unexpectedly wrote field 4 on wire")
				}
				inner = inner[in:]
			}
		}
		rest = rest[n:]
	}
	if !sawReconnectClientIDField5 {
		t.Error("MarshalClientEventBatch did not encode ReconnectInteraction.ClientID on field 5")
	}

	decoded, err := UnmarshalClientEventBatch(raw)
	if err != nil {
		t.Fatalf("UnmarshalClientEventBatch() failed: %v", err)
	}
	if decoded.AckOutputSeqNum != 7 || decoded.Disconnect == nil || decoded.ReconnectInteraction == nil {
		t.Fatalf("decoded control fields mismatch: %+v", decoded)
	}
	if got := decoded.ReconnectInteraction; got.InteractionID != "int_123" || got.NextOutputSeqNum != 8 || !got.Background || got.ClientID != DefaultEnvironmentClientID {
		t.Errorf("decoded ReconnectInteraction = %+v", got)
	}
	if len(decoded.Events) != 4 {
		t.Fatalf("len(decoded.Events) = %d, want 4", len(decoded.Events))
	}
	if decoded.Events[0].SeqNum != 1 || decoded.Events[0].TimestampMicros != 1700000001 {
		t.Errorf("decoded.Events[0] seq/ts = (%d, %d), want (1, 1700000001)", decoded.Events[0].SeqNum, decoded.Events[0].TimestampMicros)
	}
	if got := decoded.Events[0].Payload; got.SubInteractionID != "sub_1" ||
		got.Step.ToolCall.ID != "tc_1" ||
		got.Step.ToolCall.FunctionCall.Name != "view_file" ||
		got.Step.ToolCall.FunctionCall.Arguments["path"] != "README.md" ||
		got.Step.ToolResult.CallID != "tc_1" ||
		got.Step.ToolResult.FunctionResult.Name != "view_file" ||
		!got.Step.ToolResult.FunctionResult.IsError ||
		!reflect.DeepEqual(got.Step.ToolResult.FunctionResult.Result, map[string]any{"error": "missing"}) {
		t.Errorf("decoded Events[0] = %+v", got)
	}
	if got := decoded.Events[1].Payload; got.FunctionInvocation.CallID != "fi_1" ||
		got.FunctionInvocation.Name != "list_dir" ||
		got.FunctionInvocation.Arguments["dir"] != "." ||
		got.FunctionResult.CallID != "fi_1" ||
		got.FunctionResult.Name != "list_dir" ||
		!got.FunctionResult.IsError ||
		!reflect.DeepEqual(got.FunctionResult.Result, map[string]any{"entries": []any{"a.txt"}}) {
		t.Errorf("decoded Events[1] = %+v", got)
	}
	if got := decoded.Events[2].Payload; got.CallHookRequest.RequestID != "hr_1" ||
		got.CallHookRequest.Name != "pre_tool" ||
		got.CallHookRequest.HookType != 2 ||
		got.CallHookResponse.RequestID != "hr_1" ||
		!got.CallHookResponse.EmptyResult ||
		got.CallHookResponse.ErrorMessage != "hook err" {
		t.Errorf("decoded Events[2] = %+v", got)
	}
	if got := decoded.Events[3].Payload; got.CallHookRequest.RequestID != "hr_raw" || got.CallHookResponse.RequestID != "hr_raw" {
		t.Errorf("decoded Events[3] RawWire = %+v", got)
	}

	// Verify legacy field 4 ClientID fallback and field 5 precedence.
	legacyReconnect := appendStringField(nil, 1, "int_legacy")
	legacyReconnect = appendStringField(legacyReconnect, 4, "legacy-client")
	legacyBatch, err := UnmarshalClientEventBatch(appendBytesField(nil, 8, legacyReconnect))
	if err != nil || legacyBatch.ReconnectInteraction.ClientID != "legacy-client" {
		t.Errorf("legacy field 4 ReconnectInteraction = (%+v, %v), want ClientID=legacy-client", legacyBatch, err)
	}
	bothReconnect := appendStringField(legacyReconnect, 5, "canonical-client")
	bothBatch, err := UnmarshalClientEventBatch(appendBytesField(nil, 8, bothReconnect))
	if err != nil || bothBatch.ReconnectInteraction.ClientID != "canonical-client" {
		t.Errorf("field 5 precedence ReconnectInteraction = (%+v, %v), want ClientID=canonical-client", bothBatch, err)
	}

	if _, err := UnmarshalClientEventBatch([]byte{0x0a, 0x05, 0x01}); err == nil {
		t.Error("UnmarshalClientEventBatch(truncated) = nil, want error")
	}
}

func TestMarshalAndUnmarshalServerEventBatchAndHandshakeCheck(t *testing.T) {
	if got := MarshalServerEventBatch(nil); got != nil {
		t.Errorf("MarshalServerEventBatch(nil) = %x, want nil", got)
	}

	batch := &ServerEventBatch{
		AckInputSeqNum: 11,
		InteractionCompleted: &InteractionCompletedEvent{
			Status: &RPCStatus{Code: 13, Message: "backend failure"},
		},
		InteractionReconnected: &InteractionReconnectedEvent{NextInputSeqNum: 12},
		ReconnectionRequired:   &ReconnectionRequiredEvent{},
		Yield:                  &YieldEvent{NextInputSeqNum: 15},
		Events: []*ServerEvent{
			nil,
			{
				SeqNum:          9,
				TimestampMicros: 1700000099,
				Payload: &EventPayload{
					Step: &Step{
						ToolCall: &ToolCallStep{
							ID: "call_9",
							FunctionCall: &FunctionCallStep{
								Name:      "execute_bash",
								Arguments: map[string]any{"command": "pwd"},
							},
						},
					},
				},
			},
		},
	}

	raw := MarshalServerEventBatch(batch)
	decoded, err := UnmarshalServerEventBatch(raw)
	if err != nil {
		t.Fatalf("UnmarshalServerEventBatch() failed: %v", err)
	}
	if decoded.AckInputSeqNum != 11 || decoded.ReconnectionRequired == nil || decoded.Yield == nil || decoded.Yield.NextInputSeqNum != 15 {
		t.Fatalf("decoded ServerEventBatch mismatch: %+v", decoded)
	}
	if decoded.InteractionCompleted == nil || decoded.InteractionCompleted.Status == nil || decoded.InteractionCompleted.Status.Code != 13 || decoded.InteractionCompleted.Status.Message != "backend failure" {
		t.Errorf("decoded InteractionCompleted = %+v", decoded.InteractionCompleted)
	}
	if decoded.InteractionReconnected == nil || decoded.InteractionReconnected.NextInputSeqNum != 12 {
		t.Errorf("decoded InteractionReconnected = %+v", decoded.InteractionReconnected)
	}
	if len(decoded.Events) != 1 ||
		decoded.Events[0].SeqNum != 9 ||
		decoded.Events[0].TimestampMicros != 1700000099 ||
		decoded.Events[0].Payload.Step.ToolCall.FunctionCall.Name != "execute_bash" ||
		decoded.Events[0].Payload.Step.ToolCall.FunctionCall.Arguments["command"] != "pwd" {
		t.Errorf("decoded Events = %+v", decoded.Events)
	}

	// Verify ServerEvent with zero SeqNum and TimestampMicros does not emit non-zero values.
	zeroEvBatch, err := UnmarshalServerEventBatch(MarshalServerEventBatch(&ServerEventBatch{
		Events: []*ServerEvent{{Payload: &EventPayload{SubInteractionID: "s0"}}},
	}))
	if err != nil || len(zeroEvBatch.Events) != 1 || zeroEvBatch.Events[0].SeqNum != 0 || zeroEvBatch.Events[0].TimestampMicros != 0 {
		t.Errorf("zero SeqNum/TimestampMicros ServerEvent = (%+v, %v)", zeroEvBatch, err)
	}
	zeroCodeWire := MarshalServerEventBatch(&ServerEventBatch{
		InteractionCompleted: &InteractionCompletedEvent{Status: &RPCStatus{Code: 0, Message: "ok"}},
	})
	if bytes.Contains(zeroCodeWire, []byte{0x08, 0x00}) {
		t.Errorf("MarshalServerEventBatch(Status.Code=0) unexpectedly wrote varint field 1: %x", zeroCodeWire)
	}
	zeroRecWire := MarshalServerEventBatch(&ServerEventBatch{
		InteractionReconnected: &InteractionReconnectedEvent{NextInputSeqNum: 0},
	})
	if !bytes.Equal(zeroRecWire, []byte{0x42, 0x00}) {
		t.Errorf("MarshalServerEventBatch(InteractionReconnected NextInputSeqNum=0) = %x, want 4200", zeroRecWire)
	}

	// Verify legacy field 2 fallback and field 1 precedence on InteractionReconnected, plus unknown field tolerance on Completed/Yield.
	legacyReconnected := appendBytesField(nil, 8, appendVarintField(nil, 2, 42))
	decLegacy, err := UnmarshalServerEventBatch(legacyReconnected)
	if err != nil || decLegacy.InteractionReconnected == nil || decLegacy.InteractionReconnected.NextInputSeqNum != 42 {
		t.Errorf("legacy field 2 InteractionReconnected = (%+v, %v), want NextInputSeqNum=42", decLegacy, err)
	}
	bothReconnected := appendBytesField(nil, 8, appendVarintField(appendVarintField(nil, 1, 12), 2, 42))
	decBothRec, err := UnmarshalServerEventBatch(bothReconnected)
	if err != nil || decBothRec.InteractionReconnected == nil || decBothRec.InteractionReconnected.NextInputSeqNum != 12 {
		t.Errorf("field 1+2 InteractionReconnected = (%+v, %v), want NextInputSeqNum=12", decBothRec, err)
	}
	compWithExtra := appendBytesField(nil, 1, appendStringField(appendVarintField(nil, 1, 7), 2, "msg"))
	compWithExtra = appendStringField(compWithExtra, 99, "ignored-extra-bytes")
	yieldWithExtra := appendVarintField(appendVarintField(nil, 1, 15), 99, 999)
	extraBatchWire := append(appendBytesField(nil, 6, compWithExtra), appendBytesField(nil, 10, yieldWithExtra)...)
	decExtra, err := UnmarshalServerEventBatch(extraBatchWire)
	if err != nil || decExtra.InteractionCompleted.Status.Code != 7 || decExtra.Yield.NextInputSeqNum != 15 {
		t.Errorf("UnmarshalServerEventBatch(extra fields) = (%+v, %v)", decExtra, err)
	}

	negStatusWire := MarshalServerEventBatch(&ServerEventBatch{
		InteractionCompleted: &InteractionCompletedEvent{Status: &RPCStatus{Code: -1, Message: "neg-code"}},
	})
	decNeg, err := UnmarshalServerEventBatch(negStatusWire)
	if err != nil || decNeg.InteractionCompleted == nil || decNeg.InteractionCompleted.Status == nil || decNeg.InteractionCompleted.Status.Code != -1 {
		t.Errorf("negative RPCStatus.Code round-trip = (%+v, %v), want Code=-1", decNeg, err)
	}
	// Standard protobuf negative int32 varint uses 10 bytes with sign extension, not a 5-byte uint32 varint.
	if len(negStatusWire) < 12 {
		t.Errorf("MarshalServerEventBatch(Status.Code=-1) wire len = %d (%x), want >= 12 bytes with 10-byte int32 varint", len(negStatusWire), negStatusWire)
	}

	if _, err := UnmarshalServerEventBatch([]byte{0x0a, 0x09, 0x01}); err == nil {
		t.Error("UnmarshalServerEventBatch(truncated) = nil, want error")
	}
}

func TestProtoStructAndValueRoundTripAndDepthLimits(t *testing.T) {
	input := map[string]any{
		"nil_val":    nil,
		"bool_true":  true,
		"bool_false": false,
		"int_val":    int(-7),
		"int32_val":  int32(12),
		"int64_val":  int64(99),
		"uint_val":   uint(5),
		"f32_val":    float32(1.5),
		"f64_val":    float64(2.25),
		"str_val":    "hello",
		"str_slice":  []string{"x", "y"},
		"any_slice":  []any{"a", float64(3)},
		"nested":     map[string]any{"inner": "ok"},
		"stringer":   customStringer{label: "custom-val"},
	}

	encoded1 := EncodeProtoStruct(input)
	encoded2 := EncodeProtoStruct(input)
	if !bytes.Equal(encoded1, encoded2) {
		t.Error("EncodeProtoStruct is not deterministic across invocations")
	}
	orderedKeys := []string{"any_slice", "bool_false", "bool_true", "f32_val", "f64_val", "int32_val", "int64_val", "int_val", "nested", "nil_val", "str_slice", "str_val", "stringer", "uint_val"}
	lastPos := -1
	for _, k := range orderedKeys {
		pos := bytes.Index(encoded1, []byte(k))
		if pos <= lastPos {
			t.Fatalf("EncodeProtoStruct key %q at byte %d is not in sorted order (lastPos=%d)", k, pos, lastPos)
		}
		lastPos = pos
	}

	decoded, err := DecodeProtoStruct(encoded1, 0)
	if err != nil {
		t.Fatalf("DecodeProtoStruct() failed: %v", err)
	}
	if decoded["bool_true"] != true || decoded["bool_false"] != false || decoded["nil_val"] != nil {
		t.Errorf("decoded booleans/nil mismatch: %v", decoded)
	}
	if decoded["int_val"] != float64(-7) || decoded["uint_val"] != float64(5) || decoded["f32_val"] != float64(1.5) {
		t.Errorf("decoded numbers mismatch: %v", decoded)
	}
	if decoded["stringer"] != "custom-val" {
		t.Errorf("decoded stringer = %v, want \"custom-val\"", decoded["stringer"])
	}
	if !reflect.DeepEqual(decoded["str_slice"], []any{"x", "y"}) {
		t.Errorf("decoded str_slice = %v, want [x y]", decoded["str_slice"])
	}
	if !reflect.DeepEqual(decoded["any_slice"], []any{"a", float64(3)}) {
		t.Errorf("decoded any_slice = %v, want [a 3]", decoded["any_slice"])
	}
	if !reflect.DeepEqual(decoded["nested"], map[string]any{"inner": "ok"}) {
		t.Errorf("decoded nested = %v, want map[inner:ok]", decoded["nested"])
	}
	// Verify oneof override where null_value (tag 1) follows string_value (tag 3).
	overriddenNull, err := DecodeProtoValue(append(EncodeProtoValue("old"), EncodeProtoValue(nil)...), 0)
	if err != nil || overriddenNull != nil {
		t.Errorf("DecodeProtoValue(string then null) = (%v, %v), want (nil, nil)", overriddenNull, err)
	}

	// Verify tag 7 Content -> tag 10 TextContent -> tag 3 text string decoding (ignoring extra fields).
	textContent := appendStringField(nil, 1, "ignored-inner-1")
	textContent = appendStringField(textContent, 3, "extracted-content-text")
	textContent = appendStringField(textContent, 9, "ignored-inner-9")
	contentMsg := appendStringField(nil, 1, "ignored-outer-1")
	contentMsg = appendBytesField(contentMsg, 10, textContent)
	contentVal := appendBytesField(nil, 7, contentMsg)
	gotText, err := DecodeProtoValue(contentVal, 0)
	if err != nil || gotText != "extracted-content-text" {
		t.Errorf("DecodeProtoValue(content_value) = (%v, %v), want (\"extracted-content-text\", nil)", gotText, err)
	}

	// Verify exact MaxProtoNestingDepth boundary and depth+1 propagation across Struct, Value, and ListValue.
	if _, err := DecodeProtoStruct(nil, MaxProtoNestingDepth); err != nil {
		t.Errorf("DecodeProtoStruct(depth=MaxProtoNestingDepth) = %v, want nil", err)
	}
	if _, err := DecodeProtoStruct(nil, MaxProtoNestingDepth+1); err == nil {
		t.Error("DecodeProtoStruct(depth=MaxProtoNestingDepth+1) = nil, want error")
	}
	structWithScalar := EncodeProtoStruct(map[string]any{"k": "v"})
	if _, err := DecodeProtoStruct(structWithScalar, MaxProtoNestingDepth-1); err != nil {
		t.Errorf("DecodeProtoStruct(structWithScalar, MaxProtoNestingDepth-1) = %v, want nil", err)
	}
	if _, err := DecodeProtoStruct(structWithScalar, MaxProtoNestingDepth); err == nil {
		t.Error("DecodeProtoStruct(structWithScalar, MaxProtoNestingDepth) = nil, want depth error")
	}
	valWithStruct := EncodeProtoValue(map[string]any{"k": "v"})
	if _, err := DecodeProtoValue(valWithStruct, MaxProtoNestingDepth-2); err != nil {
		t.Errorf("DecodeProtoValue(valWithStruct, MaxProtoNestingDepth-2) = %v, want nil", err)
	}
	if _, err := DecodeProtoValue(valWithStruct, MaxProtoNestingDepth-1); err == nil {
		t.Error("DecodeProtoValue(valWithStruct, MaxProtoNestingDepth-1) = nil, want depth error")
	}
	valWithEmptyList := EncodeProtoValue([]any{})
	if _, err := DecodeProtoValue(valWithEmptyList, MaxProtoNestingDepth-1); err != nil {
		t.Errorf("DecodeProtoValue(valWithEmptyList, MaxProtoNestingDepth-1) = %v, want nil", err)
	}
	if _, err := DecodeProtoValue(valWithEmptyList, MaxProtoNestingDepth); err == nil {
		t.Error("DecodeProtoValue(valWithEmptyList, MaxProtoNestingDepth) = nil, want depth error")
	}
	valWithOneElemList := EncodeProtoValue([]any{"leaf"})
	if _, err := DecodeProtoValue(valWithOneElemList, MaxProtoNestingDepth-2); err != nil {
		t.Errorf("DecodeProtoValue(valWithOneElemList, MaxProtoNestingDepth-2) = %v, want nil", err)
	}
	if _, err := DecodeProtoValue(valWithOneElemList, MaxProtoNestingDepth-1); err == nil {
		t.Error("DecodeProtoValue(valWithOneElemList, MaxProtoNestingDepth-1) = nil, want depth error")
	}
	scalarVal := EncodeProtoValue("leaf")
	if got, err := DecodeProtoValue(scalarVal, MaxProtoNestingDepth); err != nil || got != "leaf" {
		t.Errorf("DecodeProtoValue(depth=MaxProtoNestingDepth) = (%v, %v), want (\"leaf\", nil)", got, err)
	}
	if _, err := DecodeProtoValue(scalarVal, MaxProtoNestingDepth+1); err == nil {
		t.Error("DecodeProtoValue(depth=MaxProtoNestingDepth+1) = nil, want error")
	}

	// Verify CallHookRequest with HookType==0 omits field 7 on the wire, and decodeProtoList ignores
	// field 1 when wireType != wireBytes and wireBytes when field != 1.
	hookZeroBytes := MarshalClientEvent(&ClientEvent{
		Payload: &EventPayload{
			CallHookRequest: &CallHookRequest{RequestID: "r", Name: "n", HookType: 0},
		},
	})
	if bytes.Contains(hookZeroBytes, []byte{0x38, 0x00}) {
		t.Errorf("MarshalClientEvent(CallHookRequest HookType=0) encoded zero field 7: %x", hookZeroBytes)
	}

	var listWithIgnoredFields []byte
	listWithIgnoredFields = appendBytesField(listWithIgnoredFields, 1, EncodeProtoValue("only-item"))
	listWithIgnoredFields = appendVarintField(listWithIgnoredFields, 1, 99)
	listWithIgnoredFields = appendBytesField(listWithIgnoredFields, 2, []byte{0xFF})
	if gotList, err := DecodeProtoValue(appendBytesField(nil, 6, listWithIgnoredFields), 0); err != nil || !reflect.DeepEqual(gotList, []any{"only-item"}) {
		t.Errorf("DecodeProtoValue(listWithIgnoredFields) = (%v, %v), want [only-item]", gotList, err)
	}

	var deepStruct []byte
	for i := 0; i < MaxProtoNestingDepth+2; i++ {
		val := appendBytesField(nil, 5, deepStruct)
		entry := appendStringField(nil, 1, "k")
		entry = appendBytesField(entry, 2, val)
		deepStruct = appendBytesField(nil, 1, entry)
	}
	if _, err := DecodeProtoStruct(deepStruct, 0); err == nil || !strings.Contains(err.Error(), "maximum depth") {
		t.Errorf("DecodeProtoStruct(deepStruct) = %v, want maximum depth error", err)
	}
}

func TestConsumeFieldWireTypesAndMalformedInputs(t *testing.T) {
	// wireFixed64 (type 1)
	var f64 []byte
	f64 = appendTag(f64, 2, wireFixed64)
	f64 = binary.LittleEndian.AppendUint64(f64, math.Float64bits(3.5))
	num, wt, val, _, n, err := ConsumeField(f64)
	if err != nil || num != 2 || wt != wireFixed64 || math.Float64frombits(val) != 3.5 || n != len(f64) {
		t.Errorf("ConsumeField(wireFixed64) = (%d, %d, %d, %d, %v)", num, wt, val, n, err)
	}
	if _, _, _, _, _, err := ConsumeField(f64[:len(f64)-1]); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ConsumeField(truncated fixed64) = %v, want ErrUnexpectedEOF", err)
	}

	// wireFixed32 (type 5)
	var f32 []byte
	f32 = appendTag(f32, 3, wireFixed32)
	f32 = binary.LittleEndian.AppendUint32(f32, 0x12345678)
	num, wt, val, _, n, err = ConsumeField(f32)
	if err != nil || num != 3 || wt != wireFixed32 || val != 0x12345678 || n != len(f32) {
		t.Errorf("ConsumeField(wireFixed32) = (%d, %d, %x, %d, %v)", num, wt, val, n, err)
	}
	if _, _, _, _, _, err := ConsumeField(f32[:len(f32)-1]); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ConsumeField(truncated fixed32) = %v, want ErrUnexpectedEOF", err)
	}

	// Field 0, field > (1<<29)-1, unsupported wire type 3, truncated varint, overflow varint.
	maxValidFieldWire := appendVarintField(nil, (1<<29)-1, 7)
	if num, _, val, _, _, err := ConsumeField(maxValidFieldWire); err != nil || num != (1<<29)-1 || val != 7 {
		t.Errorf("ConsumeField(max valid field (1<<29)-1) = (num=%d, val=%d, err=%v)", num, val, err)
	}
	var tooLargeFieldWire []byte
	tooLargeFieldWire = binary.AppendUvarint(tooLargeFieldWire, uint64(1<<29)<<3)
	tooLargeFieldWire = append(tooLargeFieldWire, 0x01)
	badInputs := [][]byte{
		{0x00, 0x01},
		tooLargeFieldWire,
		{0x0b},
		{0x08, 0x80},
		{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02},
	}
	for i, in := range badInputs {
		if _, _, _, _, _, err := ConsumeField(in); err == nil {
			t.Errorf("ConsumeField(badInputs[%d]=%x) = nil, want error", i, in)
		}
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

	var obs NopObserver
	obs.LeasePolled(1, nil)
	obs.WorkerProvisioned(true, nil)
	obs.HandoffFinished("accepted")
	obs.InteractionStarted("int_1")
	obs.InteractionFinished("int_1", nil)
}
