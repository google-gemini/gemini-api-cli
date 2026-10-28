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
	"encoding/json"
	"fmt"
	"sort"

	structpb "google3/google/protobuf/struct_go_proto"
	statuspb "google3/google/rpc/status_go_proto"
	agentspb "google3/learning/genai/api/interactions/proto/agents_go_proto"
	contentpb "google3/learning/genai/api/interactions/proto/content_go_proto"
	eventspb "google3/learning/genai/api/interactions/proto/events_go_proto"
	hookspb "google3/learning/genai/api/interactions/proto/hooks_go_proto"
	stepspb "google3/learning/genai/api/interactions/proto/steps_go_proto"
	"google3/third_party/golang/protobuf/v2/proto/proto"
)

// ClientEventBatch represents a batch of client-to-server events on the bidirectional stream.
type ClientEventBatch struct {
	Events               []*ClientEvent
	AckOutputSeqNum      int64
	ReconnectInteraction *ReconnectInteractionEvent
	Disconnect           *DisconnectEvent
}

// ReconnectInteractionEvent represents the initial stream handshake sent by an environment worker.
type ReconnectInteractionEvent struct {
	InteractionID    string
	NextOutputSeqNum int64
	Background       bool
}

// DisconnectEvent signals that the worker is cleanly disconnecting from the interaction stream.
type DisconnectEvent struct{}

// ClientEvent represents a single numbered client event sent to the server.
type ClientEvent struct {
	SeqNum          int64
	TimestampMicros int64
	Payload         *EventPayload
}

// ServerEventBatch represents a batch of server-to-client events received on the stream.
type ServerEventBatch struct {
	Events                 []*ServerEvent
	AckInputSeqNum         int64
	InteractionReconnected *InteractionReconnectedEvent
	InteractionCompleted   *InteractionCompletedEvent
	ReconnectionRequired   *ReconnectionRequiredEvent
	Yield                  *YieldEvent
}

// InteractionReconnectedEvent confirms the stream handshake and reports the next expected client seq_num.
type InteractionReconnectedEvent struct {
	NextInputSeqNum int64
}

// ReconnectionRequiredEvent instructs the worker to close the stream and reconnect.
type ReconnectionRequiredEvent struct{}

// YieldEvent indicates the server is yielding the stream after observing up to NextInputSeqNum.
type YieldEvent struct {
	NextInputSeqNum int64
}

// InteractionCompletedEvent indicates that the interaction has reached a terminal state.
type InteractionCompletedEvent struct {
	Status *RPCStatus
}

// RPCStatus represents a google.rpc.Status code and message.
type RPCStatus struct {
	Code    int32
	Message string
}

// ServerEvent represents a single numbered server event delivered on the stream.
type ServerEvent struct {
	SeqNum          int64
	TimestampMicros int64
	Payload         *EventPayload
}

// EventPayload carries the body of a ClientEvent or ServerEvent.
type EventPayload struct {
	SubInteractionID   string
	Step               *Step
	FunctionInvocation *FunctionInvocation
	FunctionResult     *FunctionResultEvent
	CallHookRequest    *CallHookRequest
	CallHookResponse   *CallHookResponse
}

// Clone returns a deep copy of EventPayload suitable for caching across reconnects.
func (p *EventPayload) Clone() *EventPayload {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Step != nil {
		st := *p.Step
		if p.Step.ToolCall != nil {
			tc := *p.Step.ToolCall
			if p.Step.ToolCall.FunctionCall != nil {
				fc := *p.Step.ToolCall.FunctionCall
				fc.Arguments = cloneMap(p.Step.ToolCall.FunctionCall.Arguments)
				tc.FunctionCall = &fc
			}
			st.ToolCall = &tc
		}
		if p.Step.ToolResult != nil {
			tr := *p.Step.ToolResult
			if p.Step.ToolResult.FunctionResult != nil {
				fr := *p.Step.ToolResult.FunctionResult
				fr.Result = cloneAny(p.Step.ToolResult.FunctionResult.Result)
				tr.FunctionResult = &fr
			}
			st.ToolResult = &tr
		}
		cp.Step = &st
	}
	if p.FunctionInvocation != nil {
		fi := *p.FunctionInvocation
		fi.Arguments = cloneMap(p.FunctionInvocation.Arguments)
		cp.FunctionInvocation = &fi
	}
	if p.FunctionResult != nil {
		fr := *p.FunctionResult
		fr.Result = cloneAny(p.FunctionResult.Result)
		cp.FunctionResult = &fr
	}
	if p.CallHookRequest != nil {
		hr := *p.CallHookRequest
		if len(p.CallHookRequest.RawWire) > 0 {
			hr.RawWire = append([]byte(nil), p.CallHookRequest.RawWire...)
		}
		cp.CallHookRequest = &hr
	}
	if p.CallHookResponse != nil {
		resp := *p.CallHookResponse
		if len(p.CallHookResponse.RawWire) > 0 {
			resp.RawWire = append([]byte(nil), p.CallHookResponse.RawWire...)
		}
		cp.CallHookResponse = &resp
	}
	return &cp
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return cloneMap(val)
	case []any:
		out := make([]any, len(val))
		for i, elem := range val {
			out[i] = cloneAny(elem)
		}
		return out
	case []string:
		return append([]string(nil), val...)
	case []byte:
		return append([]byte(nil), val...)
	default:
		return val
	}
}

// Step represents an interaction step containing a tool call or tool result.
type Step struct {
	ToolCall   *ToolCallStep
	ToolResult *ToolResultStep
}

// ToolCallStep represents a tool call step with an identifier and function call.
type ToolCallStep struct {
	ID           string
	FunctionCall *FunctionCallStep
}

// FunctionCallStep represents the function name and arguments of a tool call step.
type FunctionCallStep struct {
	Name      string
	Arguments map[string]any
}

// ToolResultStep represents a tool result step matching a prior tool call ID.
type ToolResultStep struct {
	CallID         string
	FunctionResult *FunctionResultStep
}

// FunctionResultStep represents the function result payload inside a ToolResultStep.
type FunctionResultStep struct {
	Name    string
	IsError bool
	Result  any
}

// FunctionInvocation represents a direct function invocation payload from the server.
type FunctionInvocation struct {
	CallID    string
	Name      string
	Arguments map[string]any
}

// FunctionResultEvent represents a direct function result payload sent by the worker.
type FunctionResultEvent struct {
	CallID  string
	Name    string
	IsError bool
	Result  any
}

// CallHookRequest represents a lifecycle hook execution request from the server.
type CallHookRequest struct {
	RequestID string
	Name      string
	HookType  int32
	// RawWire holds the raw protobuf bytes of the CallHookRequest message so
	// specialized hook handlers can unmarshal the full typed hook arguments.
	RawWire []byte
}

// CallHookResponse represents a lifecycle hook execution response sent by the worker.
type CallHookResponse struct {
	RequestID    string
	EmptyResult  bool
	ErrorMessage string
	// RawWire, when non-empty, holds the full serialized CallHookResponse message.
	RawWire []byte
}

// HasNonHandshakeContent reports whether a ServerEventBatch carries events or control signals
// beyond the initial InteractionReconnected handshake.
func HasNonHandshakeContent(b *ServerEventBatch) bool {
	if b == nil {
		return false
	}
	return len(b.Events) > 0 ||
		b.AckInputSeqNum > 0 ||
		b.InteractionCompleted != nil ||
		b.ReconnectionRequired != nil ||
		b.Yield != nil
}

// MarshalClientEventBatch serializes a ClientEventBatch into protobuf wire format.
func MarshalClientEventBatch(b *ClientEventBatch) []byte {
	if b == nil {
		return nil
	}
	builder := eventspb.ClientEventBatch_builder{
		AckOutputSeqNum: b.AckOutputSeqNum,
	}
	if len(b.Events) > 0 {
		events := make([]*eventspb.ClientEvent, 0, len(b.Events))
		for _, ev := range b.Events {
			if pbEv := toProtoClientEvent(ev); pbEv != nil {
				events = append(events, pbEv)
			}
		}
		builder.Events = events
	}
	if b.ReconnectInteraction != nil {
		builder.ReconnectInteraction = eventspb.ReconnectInteractionEvent_builder{
			InteractionId:    b.ReconnectInteraction.InteractionID,
			NextOutputSeqNum: b.ReconnectInteraction.NextOutputSeqNum,
			Background:       b.ReconnectInteraction.Background,
		}.Build()
	}
	if b.Disconnect != nil {
		builder.Disconnect = &eventspb.DisconnectEvent{}
	}
	out, _ := proto.Marshal(builder.Build())
	return out
}

// MarshalClientEvent serializes a single ClientEvent into protobuf wire format.
func MarshalClientEvent(e *ClientEvent) []byte {
	pbEv := toProtoClientEvent(e)
	if pbEv == nil {
		return nil
	}
	out, _ := proto.Marshal(pbEv)
	return out
}

// MarshalServerEventBatch serializes a ServerEventBatch into protobuf wire format.
func MarshalServerEventBatch(b *ServerEventBatch) []byte {
	if b == nil {
		return nil
	}
	builder := eventspb.ServerEventBatch_builder{
		AckInputSeqNum: b.AckInputSeqNum,
	}
	if len(b.Events) > 0 {
		events := make([]*eventspb.ServerEvent, 0, len(b.Events))
		for _, ev := range b.Events {
			if ev == nil {
				continue
			}
			evBuilder := eventspb.ServerEvent_builder{
				SeqNum:          ev.SeqNum,
				TimestampMicros: ev.TimestampMicros,
			}
			if ev.Payload != nil {
				evBuilder.Payload = toProtoPayload(ev.Payload)
			}
			events = append(events, evBuilder.Build())
		}
		builder.Events = events
	}
	if b.InteractionReconnected != nil {
		builder.InteractionReconnected = eventspb.InteractionReconnectedEvent_builder{
			NextInputSeqNum: b.InteractionReconnected.NextInputSeqNum,
		}.Build()
	}
	if b.InteractionCompleted != nil {
		icBuilder := eventspb.InteractionCompletedEvent_builder{}
		if b.InteractionCompleted.Status != nil {
			icBuilder.Status = &statuspb.Status{
				Code:    b.InteractionCompleted.Status.Code,
				Message: b.InteractionCompleted.Status.Message,
			}
		}
		builder.InteractionCompleted = icBuilder.Build()
	}
	if b.ReconnectionRequired != nil {
		builder.ReconnectionRequired = &eventspb.ReconnectionRequiredEvent{}
	}
	if b.Yield != nil {
		builder.Yield = eventspb.Yield_builder{
			NextInputSeqNum: b.Yield.NextInputSeqNum,
		}.Build()
	}
	out, _ := proto.Marshal(builder.Build())
	return out
}

// UnmarshalServerEventBatch parses a protobuf-encoded ServerEventBatch.
func UnmarshalServerEventBatch(data []byte) (*ServerEventBatch, error) {
	pbBatch := &eventspb.ServerEventBatch{}
	if err := proto.Unmarshal(data, pbBatch); err != nil {
		return nil, err
	}
	out := &ServerEventBatch{
		AckInputSeqNum: pbBatch.GetAckInputSeqNum(),
	}
	for _, ev := range pbBatch.GetEvents() {
		outEv := &ServerEvent{
			SeqNum:          ev.GetSeqNum(),
			TimestampMicros: ev.GetTimestampMicros(),
		}
		if ev.HasPayload() {
			outEv.Payload = fromProtoPayload(ev.GetPayload())
		}
		out.Events = append(out.Events, outEv)
	}
	if pbBatch.HasInteractionReconnected() {
		out.InteractionReconnected = &InteractionReconnectedEvent{
			NextInputSeqNum: pbBatch.GetInteractionReconnected().GetNextInputSeqNum(),
		}
	}
	if pbBatch.HasInteractionCompleted() {
		ic := &InteractionCompletedEvent{}
		if st := pbBatch.GetInteractionCompleted().GetStatus(); st != nil {
			ic.Status = &RPCStatus{
				Code:    st.GetCode(),
				Message: st.GetMessage(),
			}
		}
		out.InteractionCompleted = ic
	}
	if pbBatch.HasReconnectionRequired() {
		out.ReconnectionRequired = &ReconnectionRequiredEvent{}
	}
	if pbBatch.HasYield() {
		out.Yield = &YieldEvent{
			NextInputSeqNum: pbBatch.GetYield().GetNextInputSeqNum(),
		}
	}
	return out, nil
}

// UnmarshalClientEventBatch parses a protobuf-encoded ClientEventBatch.
func UnmarshalClientEventBatch(data []byte) (*ClientEventBatch, error) {
	pbBatch := &eventspb.ClientEventBatch{}
	if err := proto.Unmarshal(data, pbBatch); err != nil {
		return nil, err
	}
	out := &ClientEventBatch{
		AckOutputSeqNum: pbBatch.GetAckOutputSeqNum(),
	}
	for _, ev := range pbBatch.GetEvents() {
		outEv := &ClientEvent{
			SeqNum:          ev.GetSeqNum(),
			TimestampMicros: ev.GetTimestampMicros(),
		}
		if ev.HasPayload() {
			outEv.Payload = fromProtoPayload(ev.GetPayload())
		}
		out.Events = append(out.Events, outEv)
	}
	if pbBatch.HasReconnectInteraction() {
		ri := pbBatch.GetReconnectInteraction()
		out.ReconnectInteraction = &ReconnectInteractionEvent{
			InteractionID:    ri.GetInteractionId(),
			NextOutputSeqNum: ri.GetNextOutputSeqNum(),
			Background:       ri.GetBackground(),
		}
	}
	if pbBatch.HasDisconnect() {
		out.Disconnect = &DisconnectEvent{}
	}
	return out, nil
}

func toProtoClientEvent(e *ClientEvent) *eventspb.ClientEvent {
	if e == nil {
		return nil
	}
	b := eventspb.ClientEvent_builder{
		SeqNum:          e.SeqNum,
		TimestampMicros: e.TimestampMicros,
	}
	if e.Payload != nil {
		b.Payload = toProtoPayload(e.Payload)
	}
	return b.Build()
}

func toProtoPayload(p *EventPayload) *eventspb.Payload {
	b := eventspb.Payload_builder{
		SubInteractionId: p.SubInteractionID,
	}
	if p.Step != nil {
		b.Step = toProtoStep(p.Step)
	}
	if p.FunctionInvocation != nil {
		fiBuilder := eventspb.FunctionInvocation_builder{
			CallId: p.FunctionInvocation.CallID,
			Name:   p.FunctionInvocation.Name,
		}
		if len(p.FunctionInvocation.Arguments) > 0 {
			fiBuilder.Arguments = mapToProtoStruct(p.FunctionInvocation.Arguments)
		}
		b.FunctionInvocation = fiBuilder.Build()
	}
	if p.FunctionResult != nil {
		frBuilder := eventspb.FunctionResultEvent_builder{
			CallId:  p.FunctionResult.CallID,
			Name:    p.FunctionResult.Name,
			IsError: p.FunctionResult.IsError,
		}
		if p.FunctionResult.Result != nil {
			frBuilder.Result = anyToProtoValue(p.FunctionResult.Result)
		}
		b.FunctionResult = frBuilder.Build()
	}
	if p.CallHookRequest != nil {
		b.CallHookRequest = toProtoCallHookRequest(p.CallHookRequest)
	}
	if p.CallHookResponse != nil {
		b.CallHookResponse = toProtoCallHookResponse(p.CallHookResponse)
	}
	return b.Build()
}

func toProtoStep(s *Step) *stepspb.Step {
	b := stepspb.Step_builder{}
	if s.ToolCall != nil {
		tcBuilder := stepspb.ToolCallStep_builder{}
		if s.ToolCall.ID != "" {
			tcBuilder.Id = new(s.ToolCall.ID)
		}
		if s.ToolCall.FunctionCall != nil {
			fcBuilder := stepspb.FunctionCallStep_builder{}
			if s.ToolCall.FunctionCall.Name != "" {
				fcBuilder.Name = new(s.ToolCall.FunctionCall.Name)
			}
			if len(s.ToolCall.FunctionCall.Arguments) > 0 {
				fcBuilder.Arguments = mapToProtoStruct(s.ToolCall.FunctionCall.Arguments)
			}
			tcBuilder.FunctionCall = fcBuilder.Build()
		}
		b.ToolCall = tcBuilder.Build()
	}
	if s.ToolResult != nil {
		trBuilder := stepspb.ToolResultStep_builder{}
		if s.ToolResult.CallID != "" {
			trBuilder.CallId = new(s.ToolResult.CallID)
		}
		if s.ToolResult.FunctionResult != nil {
			frBuilder := stepspb.FunctionResultStep_builder{}
			if s.ToolResult.FunctionResult.Name != "" {
				frBuilder.Name = new(s.ToolResult.FunctionResult.Name)
			}
			if s.ToolResult.FunctionResult.IsError {
				frBuilder.IsError = new(true)
			}
			if s.ToolResult.FunctionResult.Result != nil {
				frBuilder.Result = anyToProtoValue(s.ToolResult.FunctionResult.Result)
			}
			trBuilder.FunctionResult = frBuilder.Build()
		}
		b.ToolResult = trBuilder.Build()
	}
	return b.Build()
}

func toProtoCallHookRequest(req *CallHookRequest) *hookspb.CallHookRequest {
	if len(req.RawWire) > 0 {
		out := &hookspb.CallHookRequest{}
		if err := proto.Unmarshal(req.RawWire, out); err == nil {
			return out
		}
	}
	return hookspb.CallHookRequest_builder{
		RequestId: req.RequestID,
		Name:      req.Name,
		Type:      agentspb.AntigravityAgentConfig_LifecycleHook(req.HookType),
	}.Build()
}

func toProtoCallHookResponse(resp *CallHookResponse) *hookspb.CallHookResponse {
	if len(resp.RawWire) > 0 {
		out := &hookspb.CallHookResponse{}
		if err := proto.Unmarshal(resp.RawWire, out); err == nil {
			return out
		}
	}
	b := hookspb.CallHookResponse_builder{
		RequestId: resp.RequestID,
	}
	if resp.ErrorMessage != "" {
		b.ErrorMessage = new(resp.ErrorMessage)
	} else if resp.EmptyResult {
		b.EmptyResult = &hookspb.EmptyResult{}
	}
	return b.Build()
}

func fromProtoPayload(p *eventspb.Payload) *EventPayload {
	out := &EventPayload{
		SubInteractionID: p.GetSubInteractionId(),
	}
	if p.HasStep() {
		out.Step = fromProtoStep(p.GetStep())
	}
	if p.HasFunctionInvocation() {
		fi := p.GetFunctionInvocation()
		out.FunctionInvocation = &FunctionInvocation{
			CallID:    fi.GetCallId(),
			Name:      fi.GetName(),
			Arguments: protoStructToMap(fi.GetArguments()),
		}
	}
	if p.HasFunctionResult() {
		fr := p.GetFunctionResult()
		var res any
		if fr.HasResult() {
			res = protoValueToAny(fr.GetResult())
		}
		out.FunctionResult = &FunctionResultEvent{
			CallID:  fr.GetCallId(),
			Name:    fr.GetName(),
			IsError: fr.GetIsError(),
			Result:  res,
		}
	}
	if p.HasCallHookRequest() {
		out.CallHookRequest = fromProtoCallHookRequest(p.GetCallHookRequest())
	}
	if p.HasCallHookResponse() {
		out.CallHookResponse = fromProtoCallHookResponse(p.GetCallHookResponse())
	}
	return out
}

func fromProtoStep(s *stepspb.Step) *Step {
	out := &Step{}
	if s.HasToolCall() {
		tc := s.GetToolCall()
		outTC := &ToolCallStep{
			ID: tc.GetId(),
		}
		if tc.HasFunctionCall() {
			fc := tc.GetFunctionCall()
			outTC.FunctionCall = &FunctionCallStep{
				Name:      fc.GetName(),
				Arguments: protoStructToMap(fc.GetArguments()),
			}
		}
		out.ToolCall = outTC
	}
	if s.HasToolResult() {
		tr := s.GetToolResult()
		outTR := &ToolResultStep{
			CallID: tr.GetCallId(),
		}
		if tr.HasFunctionResult() {
			fr := tr.GetFunctionResult()
			var res any
			if fr.HasResult() {
				res = protoValueToAny(fr.GetResult())
			}
			outTR.FunctionResult = &FunctionResultStep{
				Name:    fr.GetName(),
				IsError: fr.GetIsError(),
				Result:  res,
			}
		}
		out.ToolResult = outTR
	}
	return out
}

func fromProtoCallHookRequest(req *hookspb.CallHookRequest) *CallHookRequest {
	raw, _ := proto.Marshal(req)
	if len(raw) == 0 {
		raw = nil
	}
	return &CallHookRequest{
		RequestID: req.GetRequestId(),
		Name:      req.GetName(),
		HookType:  int32(req.GetType()),
		RawWire:   raw,
	}
}

func fromProtoCallHookResponse(resp *hookspb.CallHookResponse) *CallHookResponse {
	raw, _ := proto.Marshal(resp)
	if len(raw) == 0 {
		raw = nil
	}
	return &CallHookResponse{
		RequestID:    resp.GetRequestId(),
		EmptyResult:  resp.HasEmptyResult(),
		ErrorMessage: resp.GetErrorMessage(),
		RawWire:      raw,
	}
}

func mapToProtoStruct(m map[string]any) *contentpb.Struct {
	if len(m) == 0 {
		return &contentpb.Struct{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fields := make([]*contentpb.Field, 0, len(keys))
	for _, k := range keys {
		fields = append(fields, contentpb.Field_builder{
			Name:  new(k),
			Value: anyToProtoValue(m[k]),
		}.Build())
	}
	return contentpb.Struct_builder{
		Fields: fields,
	}.Build()
}

func anyToProtoValue(v any) *contentpb.Value {
	switch val := v.(type) {
	case nil:
		return contentpb.Value_builder{
			NullValue: structpb.NullValue_NULL_VALUE.Enum(),
		}.Build()
	case string:
		return contentpb.Value_builder{
			StringValue: new(val),
		}.Build()
	case bool:
		return contentpb.Value_builder{
			BoolValue: new(val),
		}.Build()
	case int:
		return contentpb.Value_builder{
			NumberValue: new(float64(val)),
		}.Build()
	case int32:
		return contentpb.Value_builder{
			NumberValue: new(float64(val)),
		}.Build()
	case int64:
		return contentpb.Value_builder{
			NumberValue: new(float64(val)),
		}.Build()
	case uint:
		return contentpb.Value_builder{
			NumberValue: new(float64(val)),
		}.Build()
	case uint32:
		return contentpb.Value_builder{
			NumberValue: new(float64(val)),
		}.Build()
	case uint64:
		return contentpb.Value_builder{
			NumberValue: new(float64(val)),
		}.Build()
	case float32:
		return contentpb.Value_builder{
			NumberValue: new(float64(val)),
		}.Build()
	case float64:
		return contentpb.Value_builder{
			NumberValue: new(val),
		}.Build()
	case json.Number:
		if f, err := val.Float64(); err == nil {
			return contentpb.Value_builder{
				NumberValue: new(f),
			}.Build()
		}
		return contentpb.Value_builder{
			StringValue: new(val.String()),
		}.Build()
	case map[string]any:
		return contentpb.Value_builder{
			StructValue: mapToProtoStruct(val),
		}.Build()
	case map[string]string:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make([]*contentpb.Field, 0, len(keys))
		for _, k := range keys {
			fields = append(fields, contentpb.Field_builder{
				Name:  new(k),
				Value: contentpb.Value_builder{StringValue: new(val[k])}.Build(),
			}.Build())
		}
		return contentpb.Value_builder{
			StructValue: contentpb.Struct_builder{Fields: fields}.Build(),
		}.Build()
	case []any:
		items := make([]*contentpb.Value, 0, len(val))
		for _, item := range val {
			items = append(items, anyToProtoValue(item))
		}
		return contentpb.Value_builder{
			ListValue: contentpb.ListValue_builder{Values: items}.Build(),
		}.Build()
	case []string:
		items := make([]*contentpb.Value, 0, len(val))
		for _, item := range val {
			items = append(items, contentpb.Value_builder{
				StringValue: new(item),
			}.Build())
		}
		return contentpb.Value_builder{
			ListValue: contentpb.ListValue_builder{Values: items}.Build(),
		}.Build()
	default:
		return contentpb.Value_builder{
			StringValue: new(fmt.Sprint(val)),
		}.Build()
	}
}

func protoStructToMap(s *contentpb.Struct) map[string]any {
	out := make(map[string]any)
	if s == nil {
		return out
	}
	for _, f := range s.GetFields() {
		out[f.GetName()] = protoValueToAny(f.GetValue())
	}
	return out
}

func protoValueToAny(v *contentpb.Value) any {
	if v == nil {
		return nil
	}
	switch {
	case v.HasNullValue():
		return nil
	case v.HasNumberValue():
		return v.GetNumberValue()
	case v.HasStringValue():
		return v.GetStringValue()
	case v.HasBoolValue():
		return v.GetBoolValue()
	case v.HasStructValue():
		return protoStructToMap(v.GetStructValue())
	case v.HasListValue():
		vals := v.GetListValue().GetValues()
		items := make([]any, 0, len(vals))
		for _, item := range vals {
			items = append(items, protoValueToAny(item))
		}
		return items
	case v.HasContentValue():
		if txt := v.GetContentValue().GetText(); txt != nil {
			return txt.GetText()
		}
		return ""
	default:
		return nil
	}
}
