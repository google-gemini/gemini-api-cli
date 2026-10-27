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

// ClientEventBatch represents a batch of outbound client events and stream control signals.
type ClientEventBatch struct {
	Events               []*ClientEvent
	AckOutputSeqNum      int64
	ReconnectInteraction *ReconnectInteractionEvent
	Disconnect           *DisconnectEvent
}

// ReconnectInteractionEvent attaches or resumes an interaction stream as a worker client.
type ReconnectInteractionEvent struct {
	InteractionID    string
	NextOutputSeqNum int64
	Background       bool
	ClientID         string
}

// DisconnectEvent requests a graceful disconnection without terminating the interaction.
type DisconnectEvent struct{}

// ClientEvent represents a sequenced outbound event from the worker to the server.
type ClientEvent struct {
	Payload         *EventPayload
	SeqNum          int64
	TimestampMicros int64
}

// EventPayload carries the typed event union shared by ClientEvent and ServerEvent.
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

// Step represents a step payload containing a tool call or tool result.
type Step struct {
	ToolCall   *ToolCallStep
	ToolResult *ToolResultStep
}

// ToolCallStep represents an inbound tool call step.
type ToolCallStep struct {
	ID           string
	FunctionCall *FunctionCallStep
}

// FunctionCallStep represents a function name and structured argument map.
type FunctionCallStep struct {
	Name      string
	Arguments map[string]any
}

// ToolResultStep represents an outbound tool result step.
type ToolResultStep struct {
	CallID         string
	FunctionResult *FunctionResultStep
}

// FunctionResultStep represents a function execution result inside a ToolResultStep.
type FunctionResultStep struct {
	Name    string
	IsError bool
	Result  any
}

// FunctionInvocation represents a top-level server-to-client function call payload.
type FunctionInvocation struct {
	CallID    string
	Name      string
	Arguments map[string]any
}

// FunctionResultEvent represents a top-level client-to-server function result payload.
type FunctionResultEvent struct {
	CallID  string
	Name    string
	IsError bool
	Result  any
}

// CallHookRequest represents a server-to-client hook execution request.
// RawWire preserves the complete serialized protobuf bytes so callers with full hook descriptors
// can unmarshal nested hook-type payloads without data loss.
type CallHookRequest struct {
	RequestID string
	Name      string
	HookType  int32
	RawWire   []byte
}

// CallHookResponse represents a client-to-server hook execution response.
// When RawWire is non-empty, MarshalClientEventBatch emits RawWire verbatim as the CallHookResponse message.
type CallHookResponse struct {
	RequestID    string
	EmptyResult  bool
	ErrorMessage string
	RawWire      []byte
}

// ServerEventBatch represents a batch of inbound server events and stream lifecycle signals.
type ServerEventBatch struct {
	Events                 []*ServerEvent
	AckInputSeqNum         int64
	InteractionCompleted   *InteractionCompletedEvent
	InteractionReconnected *InteractionReconnectedEvent
	ReconnectionRequired   *ReconnectionRequiredEvent
	Yield                  *YieldEvent
}

// ServerEvent represents a sequenced inbound event from the server.
type ServerEvent struct {
	Payload         *EventPayload
	SeqNum          int64
	TimestampMicros int64
}

// RPCStatus represents a status code and message attached to terminal events.
type RPCStatus struct {
	Code    int32
	Message string
}

// InteractionCompletedEvent signals that the interaction has reached a terminal state.
type InteractionCompletedEvent struct {
	Status *RPCStatus
}

// InteractionReconnectedEvent confirms attachment to a live interaction stream.
type InteractionReconnectedEvent struct {
	NextInputSeqNum int64
}

// ReconnectionRequiredEvent instructs the worker to open a new stream and send ReconnectInteraction.
type ReconnectionRequiredEvent struct{}

// YieldEvent signals that the server actor is currently idle and eligible for stream disconnection.
type YieldEvent struct {
	NextInputSeqNum int64
}
