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
	"cmp"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
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
		ReconnectInteraction: &ReconnectInteractionEvent{
			InteractionID:    "int_ws_1",
			NextOutputSeqNum: 4,
			Background:       true,
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
	if err != nil || decConnect.ReconnectInteraction == nil || decConnect.ReconnectInteraction.InteractionID != "int_ws_1" || decConnect.ReconnectInteraction.NextOutputSeqNum != 4 || !decConnect.ReconnectInteraction.Background {
		t.Errorf("UnmarshalClientEventBatchJSON(%s) = (%+v, %v), want ReconnectInteraction{id=\"int_ws_1\", next=4, bg=true}", singleConnect, decConnect, err)
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

	// Verify UnmarshalClientEventBatchJSON aliases ("interaction.reconnect", "tool_result", interaction_id, ack_output_seq_num).
	aliasClientBatch, err := UnmarshalClientEventBatchJSON([]byte(`{"ack_output_seq_num":"11","events":[{"event_type":"interaction.reconnect","interaction_id":"int_alias","next_output_seq_num":"2"},{"event_type":"tool_result","call_id":"c_t","name":"view_file","result":"hello"}]}`))
	if err != nil || aliasClientBatch.AckOutputSeqNum != 11 || aliasClientBatch.ReconnectInteraction == nil || aliasClientBatch.ReconnectInteraction.InteractionID != "int_alias" || len(aliasClientBatch.Events) != 1 || aliasClientBatch.Events[0].Payload.FunctionResult.CallID != "c_t" {
		t.Errorf("UnmarshalClientEventBatchJSON(aliases) = (%+v, %v), want ack=11 id=\"int_alias\" call_id=\"c_t\"", aliasClientBatch, err)
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
						RequestID:    "req_1",
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
	decMultiClient, err := UnmarshalClientEventBatchJSON(multiClientRaw)
	if err != nil || decMultiClient.ReconnectInteraction == nil || len(decMultiClient.Events) != 2 || decMultiClient.Disconnect == nil {
		t.Fatalf("UnmarshalClientEventBatchJSON(%s) = (%+v, %v), want ReconnectInteraction, 2 events, and Disconnect", multiClientRaw, decMultiClient, err)
	}
	if decMultiClient.Events[0].Payload.FunctionResult == nil || !decMultiClient.Events[0].Payload.FunctionResult.IsError || decMultiClient.Events[0].Payload.FunctionResult.CallID != "call_1" {
		t.Errorf("UnmarshalClientEventBatchJSON(multiClient).Events[0].Payload = %+v, want FunctionResult{CallID:\"call_1\", IsError:true}", decMultiClient.Events[0].Payload)
	}

	// Test ServerEventBatch JSON marshal/unmarshal (single, multi-event, step/status_update/complete aliases, string error.code).
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
						RequestID: "hk_1",
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
	decSrvMulti, err := UnmarshalServerEventBatchJSON(srvMultiRaw)
	if err != nil || decSrvMulti.AckInputSeqNum != 8 || decSrvMulti.InteractionReconnected == nil || decSrvMulti.InteractionReconnected.NextInputSeqNum != 2 || len(decSrvMulti.Events) != 2 || decSrvMulti.InteractionCompleted == nil || decSrvMulti.InteractionCompleted.Status == nil || decSrvMulti.InteractionCompleted.Status.Message != "done" || decSrvMulti.ReconnectionRequired == nil || decSrvMulti.Yield == nil || decSrvMulti.Yield.NextInputSeqNum != 9 {
		t.Fatalf("UnmarshalServerEventBatchJSON(%s) = (%+v, %v), want ack=8, reconnected=2, 2 events, completed=\"done\", reconnection_required, and yield=9", srvMultiRaw, decSrvMulti, err)
	}
	if rawReconnZero, err := MarshalServerEventBatchJSON(&ServerEventBatch{InteractionReconnected: &InteractionReconnectedEvent{}}); err != nil || string(rawReconnZero) != `{"event_type":"interaction.connected"}` {
		t.Errorf("MarshalServerEventBatchJSON(InteractionReconnected zero seq) = (%s, %v), want {\"event_type\":\"interaction.connected\"}", rawReconnZero, err)
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
	if decSrvMulti.Events[0].Payload.FunctionInvocation == nil || decSrvMulti.Events[0].Payload.FunctionInvocation.Name != "read_file" {
		t.Errorf("UnmarshalServerEventBatchJSON(srvMulti).Events[0].Payload = %+v, want FunctionInvocation{Name:\"read_file\"}", decSrvMulti.Events[0].Payload)
	}

	// Golden literal JSON server events: interaction.complete, step.start, interaction.status_update, string error.code, and bare error object.
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"interaction.complete","status":{"code":0,"message":"ok"}}`)); err != nil || b.InteractionCompleted == nil {
		t.Errorf("UnmarshalServerEventBatchJSON(interaction.complete) = (%+v, %v), want non-nil InteractionCompleted", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"step.start","seq_num":"14","step":{"type":"function_call","id":"call_99","name":"execute_bash","arguments":{"command":"ls"}}}`)); err != nil || len(b.Events) != 1 || b.Events[0].SeqNum != 14 || b.Events[0].Payload.FunctionInvocation == nil || b.Events[0].Payload.FunctionInvocation.CallID != "call_99" || b.Events[0].Payload.FunctionInvocation.Name != "execute_bash" {
		t.Errorf("UnmarshalServerEventBatchJSON(step.start) = (%+v, %v), want seq=14 call_id=\"call_99\" name=\"execute_bash\"", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"step","seq_num":"15","type":"function_call","id":"call_flat_1","name":"execute_bash","arguments":{"command":"pwd"}}`)); err != nil || len(b.Events) != 1 || b.Events[0].SeqNum != 15 || b.Events[0].Payload.FunctionInvocation == nil || b.Events[0].Payload.FunctionInvocation.CallID != "call_flat_1" || b.Events[0].Payload.FunctionInvocation.Name != "execute_bash" {
		t.Errorf("UnmarshalServerEventBatchJSON(flattened step) = (%+v, %v), want seq=15 call_id=\"call_flat_1\" name=\"execute_bash\"", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"step","type":"function_call","call_id":"call_flat_cid","id":"ignored_flat_id","name":"execute_bash"}`)); err != nil || len(b.Events) != 1 || b.Events[0].Payload.FunctionInvocation == nil || b.Events[0].Payload.FunctionInvocation.CallID != "call_flat_cid" {
		t.Errorf("UnmarshalServerEventBatchJSON(flattened step call_id precedence) = (%+v, %v), want call_id=\"call_flat_cid\"", b, err)
	}
	for _, nonFCStep := range []string{
		`{"event_type":"step","step":{"type":"thought"}}`,
		`{"event_type":"step","type":"thought"}`,
		`{"event_type":"step","type":"function_call","step":{"type":"thought"}}`,
	} {
		if b, err := UnmarshalServerEventBatchJSON([]byte(nonFCStep)); err != nil || len(b.Events) != 0 {
			t.Errorf("UnmarshalServerEventBatchJSON(%s) = (%+v, %v), want 0 events", nonFCStep, b, err)
		}
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"interaction.status_update","interaction_id":"int_1","status":"in_progress"}`)); err != nil || b.InteractionCompleted != nil {
		t.Errorf("UnmarshalServerEventBatchJSON(status=in_progress) = (%+v, %v), want InteractionCompleted=nil", b, err)
	}
	for _, tc := range []struct {
		st         string
		wantStatus *RPCStatus
	}{
		{st: "completed", wantStatus: nil},
		{st: "failed", wantStatus: &RPCStatus{Code: 13, Message: "msg"}},
		{st: "cancelled", wantStatus: &RPCStatus{Code: 1, Message: "msg"}},
		{st: "canceled", wantStatus: &RPCStatus{Code: 1, Message: "msg"}},
	} {
		for _, payload := range []string{
			`{"event_type":"interaction.status_update","interaction_status":"` + tc.st + `","message":"msg"}`,
			`{"event_type":"interaction.status_update","interaction_id":"int_1","status":"` + tc.st + `","message":"msg"}`,
		} {
			b, err := UnmarshalServerEventBatchJSON([]byte(payload))
			if err != nil || b.InteractionCompleted == nil {
				t.Errorf("UnmarshalServerEventBatchJSON(%s) = (%+v, %v), want non-nil InteractionCompleted", payload, b, err)
				continue
			}
			if !reflect.DeepEqual(b.InteractionCompleted.Status, tc.wantStatus) {
				t.Errorf("UnmarshalServerEventBatchJSON(%s).Status = %+v, want %+v", payload, b.InteractionCompleted.Status, tc.wantStatus)
			}
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
				Payload:         &EventPayload{SubInteractionID: "ignored_sub"},
			},
			{
				Payload: &EventPayload{
					FunctionInvocation: &FunctionInvocation{CallID: "fi_z", Name: "fn"},
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
	for _, omittedKey := range []string{`"arguments"`, `"seq_num"`, `"timestamp_micros"`, `"next_input_seq_num"`} {
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
					FunctionInvocation: &FunctionInvocation{
						CallID:    "fi_1",
						Name:      "exec",
						Arguments: map[string]any{"x": 1},
					},
				},
			},
			{
				SeqNum:          4,
				TimestampMicros: 1001,
				Payload: &EventPayload{
					CallHookRequest: &CallHookRequest{
						RequestID: "hr_1",
						Name:      "hook",
						HookType:  1,
					},
				},
			},
		},
	})
	if err != nil || strings.Contains(string(rawSrvDirectEvents), "ack_input_seq_num") || !strings.Contains(string(rawSrvDirectEvents), `"timestamp_micros":"999"`) || !strings.Contains(string(rawSrvDirectEvents), `"timestamp_micros":"1001"`) {
		t.Errorf("MarshalServerEventBatchJSON(direct FunctionInvocation + CallHookRequest) = (%s, %v), want timestamp_micros 999 and 1001 without ack_input_seq_num", rawSrvDirectEvents, err)
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
					FunctionInvocation: &FunctionInvocation{
						CallID:    "fi_1",
						Name:      "exec",
						Arguments: map[string]any{"x": float64(1)},
					},
				},
			},
			{
				SeqNum:          4,
				TimestampMicros: 1001,
				Payload: &EventPayload{
					CallHookRequest: &CallHookRequest{
						RequestID: "hr_1",
						Name:      "hook",
						HookType:  1,
					},
				},
			},
		},
	}
	if err != nil || !reflect.DeepEqual(decSrvDirect, wantSrvDirect) {
		t.Errorf("UnmarshalServerEventBatchJSON(%s) = (%+v, %v), want (%+v, nil)", rawSrvDirectEvents, decSrvDirect, err, wantSrvDirect)
	}

	// Test nested Step.FunctionCall variants, wrapper.Events precedence, empty/null server batches, error status/code/redaction variants, and invalid proto int64 fields.
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
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"step","step":{"function_call":{"call_id":"fc_cid","name":"view_file","arguments":{"p":"a"}}}}`)); err != nil || len(b.Events) != 1 || b.Events[0].Payload.FunctionInvocation.CallID != "fc_cid" || b.Events[0].Payload.FunctionInvocation.Name != "view_file" || b.Events[0].Payload.FunctionInvocation.Arguments["p"] != "a" {
		t.Errorf("UnmarshalServerEventBatchJSON(nested function_call call_id) = (%+v, %v), want call_id=\"fc_cid\" name=\"view_file\"", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"step","step":{"function_call":{"id":"fc_id","name":"view_file"}}}`)); err != nil || len(b.Events) != 1 || b.Events[0].Payload.FunctionInvocation.CallID != "fc_id" {
		t.Errorf("UnmarshalServerEventBatchJSON(nested function_call id) = (%+v, %v), want call_id=\"fc_id\"", b, err)
	}
	if b, err := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"step","step":{"call_id":"outer_cid","name":"outer_fn","arguments":{"k":"outer"},"function_call":{"call_id":"inner_cid","name":"inner_fn","arguments":{"k":"inner"}}}}`)); err != nil || len(b.Events) != 1 || b.Events[0].Payload.FunctionInvocation.CallID != "outer_cid" || b.Events[0].Payload.FunctionInvocation.Name != "outer_fn" || b.Events[0].Payload.FunctionInvocation.Arguments["k"] != "outer" {
		t.Errorf("UnmarshalServerEventBatchJSON(outer step precedence over nested function_call) = (%+v, %v), want call_id=\"outer_cid\" name=\"outer_fn\"", b, err)
	}

	// Verify struct-field secret redaction in wsCloseToTransportError and UnmarshalServerEventBatchJSON("error").
	closeRedactErr := wsCloseToTransportError(1008, `denied: {"lease_token":"lt_close_sec","environment_key":"ek_close_sec"}`)
	var closeTE *TransportError
	if !errors.As(closeRedactErr, &closeTE) {
		t.Fatalf("wsCloseToTransportError(1008, secret) = %v, want *TransportError", closeRedactErr)
	}
	leakedCloseSecret := strings.Contains(closeTE.Op, "lt_close_sec") || strings.Contains(closeTE.Message, "ek_close_sec") || strings.Contains(closeTE.Body, "lt_close_sec")
	redactedCloseFields := strings.Contains(closeTE.Message, "[REDACTED]") && strings.Contains(closeTE.Body, "[REDACTED]")
	if leakedCloseSecret || !redactedCloseFields {
		t.Errorf("wsCloseToTransportError(1008, secret) = (op=%q, msg=%q, body=%q), want secrets replaced with [REDACTED]", closeTE.Op, closeTE.Message, closeTE.Body)
	}
	_, srvRedactErr := UnmarshalServerEventBatchJSON([]byte(`{"event_type":"error","lease_token":"lt_body_sec","error":{"code":7,"message":"bad {\"lease_token\":\"lt_srv_sec\",\"environment_key\":\"ek_srv_sec\"}"}}`))
	var srvRedactTE *TransportError
	if !errors.As(srvRedactErr, &srvRedactTE) {
		t.Fatalf("UnmarshalServerEventBatchJSON(error with secrets) = %v, want *TransportError", srvRedactErr)
	}
	leakedSrvSecret := strings.Contains(srvRedactTE.Op, "lt_srv_sec") || strings.Contains(srvRedactTE.Message, "ek_srv_sec") || strings.Contains(srvRedactTE.Body, "lt_body_sec")
	redactedSrvFields := strings.Contains(srvRedactTE.Message, "[REDACTED]") && strings.Contains(srvRedactTE.Body, "[REDACTED]")
	if leakedSrvSecret || !redactedSrvFields {
		t.Errorf("UnmarshalServerEventBatchJSON(error with secrets) = (op=%q, msg=%q, body=%q), want secrets replaced with [REDACTED]", srvRedactTE.Op, srvRedactTE.Message, srvRedactTE.Body)
	}

	serverErrorCases := []struct {
		raw      string
		wantCode int
		wantSub  string
	}{
		{raw: `{"event_type":"error","error":{"code":429,"message":"too many"}}`, wantCode: 8, wantSub: "too many"},
		{raw: `{"event_type":"error","error":{"code":0,"status":"PERMISSION_DENIED","message":"zero code"}}`, wantCode: 7, wantSub: "zero code"},
		{raw: `{"event_type":"error","error":{"code":"INVALID_ARGUMENT","message":"bad arg"}}`, wantCode: 3, wantSub: "bad arg"},
		{raw: `{"event_type":"error","error":{"code":"DEADLINE_EXCEEDED","message":"timeout"}}`, wantCode: 4, wantSub: "timeout"},
		{raw: `{"event_type":"error","error":{"code":"NOT_FOUND","message":"missing"}}`, wantCode: 5, wantSub: "missing"},
		{raw: `{"event_type":"error","error":{"code":"ALREADY_EXISTS","message":"dup"}}`, wantCode: 10, wantSub: "dup"},
		{raw: `{"event_type":"error","error":{"code":"RESOURCE_EXHAUSTED","message":"quota"}}`, wantCode: 8, wantSub: "quota"},
		{raw: `{"event_type":"error","error":{"code":"UNIMPLEMENTED","message":"unimp"}}`, wantCode: 12, wantSub: "unimp"},
		{raw: `{"event_type":"error","error":{"code":"UNAVAILABLE","message":"down"}}`, wantCode: 14, wantSub: "down"},
		{raw: `{"event_type":"error","error":{"code":"UNAUTHENTICATED","message":"unauth"}}`, wantCode: 16, wantSub: "unauth"},
		{raw: `{"event_type":"error","error":{"code":"INTERNAL","message":"internal"}}`, wantCode: 13, wantSub: "internal"},
		{raw: `{"event_type":"error","error":{"code":null,"status":"UNRECOGNIZED_CODE"}}`, wantCode: 13, wantSub: "server stream error"},
		{raw: `{"event_type":"error","message":"outer error message","error":{"code":7}}`, wantCode: 7, wantSub: "outer error message"},
		{raw: `{"event_type":"error","message":"outer status message","status":{"code":14}}`, wantCode: 14, wantSub: "outer status message"},
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
		`{"ack_output_seq_num":"not_int"}`,
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
		`{"event_type":"function_invocation","seq_num":"not_int"}`,
		`{"event_type":"function_invocation","timestamp_micros":"not_int"}`,
		`{"event_type":"interaction.connected","next_input_seq_num":"not_int"}`,
		`{"event_type":"yield","next_input_seq_num":"not_int"}`,
	} {
		if _, err := UnmarshalServerEventBatchJSON([]byte(badSrvJSON)); err == nil {
			t.Errorf("UnmarshalServerEventBatchJSON(%s) = nil, want error", badSrvJSON)
		}
	}
}
