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
	"encoding/json"
	"strconv"
	"strings"
)

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
// RawWire preserves the complete serialized protobuf bytes when received from a local hook server.
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

func functionResultJSONMap(callID, name string, isError bool, result any) map[string]any {
	ev := map[string]any{
		"event_type": "function_result",
		"call_id":    callID,
		"name":       name,
		"is_error":   isError,
	}
	if result != nil {
		ev["result"] = result
	}
	return ev
}

func functionInvocationJSONMap(callID, name string, args map[string]any) map[string]any {
	ev := map[string]any{
		"event_type": "function_invocation",
		"call_id":    callID,
		"name":       name,
	}
	if args != nil {
		ev["arguments"] = args
	}
	return ev
}

// MarshalClientEventBatchJSON encodes a ClientEventBatch into the ExternalClientEventBatch JSON wire format
// accepted by /{api_version}/interactions:streamForEnvironmentRuntime.
func MarshalClientEventBatchJSON(b *ClientEventBatch) ([]byte, error) {
	if b == nil {
		return []byte("{}"), nil
	}
	var events []map[string]any
	if b.ReconnectInteraction != nil {
		ev := map[string]any{
			"event_type": "interaction.connect",
			"id":         b.ReconnectInteraction.InteractionID,
		}
		if b.ReconnectInteraction.NextOutputSeqNum > 0 {
			ev["next_output_seq_num"] = b.ReconnectInteraction.NextOutputSeqNum
		}
		if b.ReconnectInteraction.Background {
			ev["background"] = true
		}
		events = append(events, ev)
	}
	for _, ce := range b.Events {
		if ce == nil || ce.Payload == nil {
			continue
		}
		p := ce.Payload
		switch {
		case p.FunctionResult != nil:
			events = append(events, functionResultJSONMap(p.FunctionResult.CallID, p.FunctionResult.Name, p.FunctionResult.IsError, p.FunctionResult.Result))
		case p.Step != nil && p.Step.ToolResult != nil && p.Step.ToolResult.FunctionResult != nil:
			tr := p.Step.ToolResult
			fr := tr.FunctionResult
			events = append(events, functionResultJSONMap(tr.CallID, fr.Name, fr.IsError, fr.Result))
		case p.CallHookResponse != nil:
			// Preserved for internal test parity; BYOS runtime workers only emit function_result events.
			events = append(events, map[string]any{
				"event_type":    "call_hook_response",
				"request_id":    p.CallHookResponse.RequestID,
				"empty_result":  p.CallHookResponse.EmptyResult,
				"error_message": p.CallHookResponse.ErrorMessage,
			})
		}
	}
	if b.Disconnect != nil {
		events = append(events, map[string]any{
			"event_type": "interaction.disconnect",
		})
	}
	switch len(events) {
	case 0:
		// Intentional: AckOutputSeqNum is omitted on the external WebSocket JSON
		// wire format, and the server treats an empty "{}" object frame as an
		// in-band heartbeat that renews the interaction lease without closing the
		// socket.
		return []byte("{}"), nil
	case 1:
		return json.Marshal(events[0])
	default:
		return json.Marshal(map[string]any{"events": events})
	}
}

type rawJSONClientEvent struct {
	EventType        string          `json:"event_type"`
	ID               string          `json:"id"`
	InteractionID    string          `json:"interaction_id"`
	NextOutputSeqNum json.RawMessage `json:"next_output_seq_num"`
	Background       bool            `json:"background"`
	ClientID         string          `json:"client_id"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	IsError          bool            `json:"is_error"`
	Result           any             `json:"result"`
	RequestID        string          `json:"request_id"`
	EmptyResult      bool            `json:"empty_result"`
	ErrorMessage     string          `json:"error_message"`
	AckOutputSeqNum  json.RawMessage `json:"ack_output_seq_num"`
}

// UnmarshalClientEventBatchJSON decodes an ExternalClientEventBatch JSON payload into a ClientEventBatch.
func UnmarshalClientEventBatchJSON(data []byte) (*ClientEventBatch, error) {
	trimmed := bytes.TrimSpace(data)
	out := &ClientEventBatch{}
	if len(trimmed) == 0 {
		return out, nil
	}
	var wrapper struct {
		Events          []rawJSONClientEvent `json:"events"`
		AckOutputSeqNum json.RawMessage      `json:"ack_output_seq_num"`
	}
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, err
	}
	if len(wrapper.AckOutputSeqNum) > 0 {
		ack, err := ParseProtoInt64(wrapper.AckOutputSeqNum)
		if err != nil {
			return nil, err
		}
		out.AckOutputSeqNum = ack
	}
	rawEvents := wrapper.Events
	if len(rawEvents) == 0 {
		var single rawJSONClientEvent
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return nil, err
		}
		rawEvents = []rawJSONClientEvent{single}
	}
	for _, ev := range rawEvents {
		switch ev.EventType {
		case "interaction.connect", "interaction.reconnect":
			nextOut, err := ParseProtoInt64(ev.NextOutputSeqNum)
			if err != nil {
				return nil, err
			}
			id := ev.ID
			if id == "" {
				id = ev.InteractionID
			}
			out.ReconnectInteraction = &ReconnectInteractionEvent{
				InteractionID:    id,
				NextOutputSeqNum: nextOut,
				Background:       ev.Background,
				ClientID:         ev.ClientID,
			}
		case "function_result", "tool_result":
			out.Events = append(out.Events, &ClientEvent{
				Payload: &EventPayload{
					FunctionResult: &FunctionResultEvent{
						CallID:  ev.CallID,
						Name:    ev.Name,
						IsError: ev.IsError,
						Result:  ev.Result,
					},
				},
			})
		case "call_hook_response":
			out.Events = append(out.Events, &ClientEvent{
				Payload: &EventPayload{
					CallHookResponse: &CallHookResponse{
						RequestID:    ev.RequestID,
						EmptyResult:  ev.EmptyResult,
						ErrorMessage: ev.ErrorMessage,
					},
				},
			})
		case "interaction.disconnect":
			out.Disconnect = &DisconnectEvent{}
		}
	}
	return out, nil
}

// MarshalServerEventBatchJSON encodes a ServerEventBatch into ExternalServerEvent JSON format.
func MarshalServerEventBatchJSON(b *ServerEventBatch) ([]byte, error) {
	if b == nil {
		return []byte("{}"), nil
	}
	var events []map[string]any
	if b.InteractionReconnected != nil {
		ev := map[string]any{
			"event_type": "interaction.connected",
		}
		if b.InteractionReconnected.NextInputSeqNum != 0 {
			ev["next_input_seq_num"] = strconv.FormatInt(b.InteractionReconnected.NextInputSeqNum, 10)
		}
		events = append(events, ev)
	}
	for _, se := range b.Events {
		if se == nil || se.Payload == nil {
			continue
		}
		p := se.Payload
		var ev map[string]any
		switch {
		case p.FunctionInvocation != nil:
			ev = functionInvocationJSONMap(p.FunctionInvocation.CallID, p.FunctionInvocation.Name, p.FunctionInvocation.Arguments)
		case p.Step != nil && p.Step.ToolCall != nil && p.Step.ToolCall.FunctionCall != nil:
			tc := p.Step.ToolCall
			fc := tc.FunctionCall
			ev = functionInvocationJSONMap(tc.ID, fc.Name, fc.Arguments)
		case p.CallHookRequest != nil:
			ev = map[string]any{
				"event_type": "call_hook_request",
				"request_id": p.CallHookRequest.RequestID,
				"name":       p.CallHookRequest.Name,
				"hook_type":  p.CallHookRequest.HookType,
			}
		default:
			continue
		}
		if se.SeqNum != 0 {
			ev["seq_num"] = strconv.FormatInt(se.SeqNum, 10)
		}
		if se.TimestampMicros != 0 {
			ev["timestamp_micros"] = strconv.FormatInt(se.TimestampMicros, 10)
		}
		events = append(events, ev)
	}
	if b.InteractionCompleted != nil {
		ev := map[string]any{
			"event_type": "interaction.completed",
		}
		if b.InteractionCompleted.Status != nil {
			ev["status"] = map[string]any{
				"code":    b.InteractionCompleted.Status.Code,
				"message": b.InteractionCompleted.Status.Message,
			}
		}
		events = append(events, ev)
	}
	if b.ReconnectionRequired != nil {
		events = append(events, map[string]any{
			"event_type": "reconnection_required",
		})
	}
	if b.Yield != nil {
		ev := map[string]any{
			"event_type": "yield",
		}
		if b.Yield.NextInputSeqNum != 0 {
			ev["next_input_seq_num"] = strconv.FormatInt(b.Yield.NextInputSeqNum, 10)
		}
		events = append(events, ev)
	}
	switch len(events) {
	case 0:
		if b.AckInputSeqNum != 0 {
			return json.Marshal(map[string]any{
				"ack_input_seq_num": strconv.FormatInt(b.AckInputSeqNum, 10),
			})
		}
		return []byte("{}"), nil
	case 1:
		if b.AckInputSeqNum != 0 {
			events[0]["ack_input_seq_num"] = strconv.FormatInt(b.AckInputSeqNum, 10)
		}
		return json.Marshal(events[0])
	default:
		wrapper := map[string]any{"events": events}
		if b.AckInputSeqNum != 0 {
			wrapper["ack_input_seq_num"] = strconv.FormatInt(b.AckInputSeqNum, 10)
		}
		return json.Marshal(wrapper)
	}
}

type rawJSONServerEvent struct {
	EventType         string          `json:"event_type"`
	Type              string          `json:"type"`
	ID                string          `json:"id"`
	NextInputSeqNum   json.RawMessage `json:"next_input_seq_num"`
	AckInputSeqNum    json.RawMessage `json:"ack_input_seq_num"`
	SeqNum            json.RawMessage `json:"seq_num"`
	TimestampMicros   json.RawMessage `json:"timestamp_micros"`
	CallID            string          `json:"call_id"`
	Name              string          `json:"name"`
	Arguments         map[string]any  `json:"arguments"`
	RequestID         string          `json:"request_id"`
	HookType          int32           `json:"hook_type"`
	InteractionStatus string          `json:"interaction_status"`
	Step              *struct {
		Type         string         `json:"type"`
		ID           string         `json:"id"`
		CallID       string         `json:"call_id"`
		Name         string         `json:"name"`
		Arguments    map[string]any `json:"arguments"`
		FunctionCall *struct {
			ID        string         `json:"id"`
			CallID    string         `json:"call_id"`
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"function_call"`
	} `json:"step"`
	Status json.RawMessage `json:"status"`
	Error  *struct {
		Code    json.RawMessage `json:"code"`
		Status  string          `json:"status"`
		Message string          `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

func parseServerStatus(raw json.RawMessage) (*RPCStatus, string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return nil, s
		}
	case '{':
		var obj struct {
			Code    int32  `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(trimmed, &obj); err == nil {
			return &RPCStatus{Code: obj.Code, Message: obj.Message}, ""
		}
	}
	return nil, ""
}

func parseServerErrorCode(raw json.RawMessage, statusStr string) int {
	for _, candidate := range []string{strings.Trim(strings.TrimSpace(string(raw)), `"`), strings.TrimSpace(statusStr)} {
		if n, err := strconv.Atoi(candidate); err == nil && n > 0 {
			if n >= 100 {
				return HTTPStatusToGRPCCode(n)
			}
			return n
		}
		switch strings.ToUpper(candidate) {
		case "INVALID_ARGUMENT", "FAILED_PRECONDITION", "OUT_OF_RANGE", "INVALID_REQUEST_ERROR":
			return 3
		case "DEADLINE_EXCEEDED", "TIMEOUT":
			return 4
		case "NOT_FOUND", "NOT_FOUND_ERROR":
			return 5
		case "ALREADY_EXISTS", "ABORTED":
			return 10
		case "PERMISSION_DENIED", "FORBIDDEN", "AUTHORIZATION_ERROR":
			return 7
		case "RESOURCE_EXHAUSTED", "RATE_LIMIT_ERROR", "QUOTA_EXCEEDED":
			return 8
		case "UNIMPLEMENTED":
			return 12
		case "UNAVAILABLE", "OVERLOADED_ERROR":
			return 14
		case "UNAUTHENTICATED", "AUTHENTICATION_ERROR":
			return 16
		case "INTERNAL", "API_ERROR", "UNKNOWN":
			return 13
		}
	}
	return 13
}

// UnmarshalServerEventBatchJSON decodes an ExternalServerEvent JSON payload into a ServerEventBatch.
func UnmarshalServerEventBatchJSON(data []byte) (*ServerEventBatch, error) {
	trimmed := bytes.TrimSpace(data)
	out := &ServerEventBatch{}
	if len(trimmed) == 0 {
		return out, nil
	}
	var wrapper struct {
		Events         []rawJSONServerEvent `json:"events"`
		AckInputSeqNum json.RawMessage      `json:"ack_input_seq_num"`
	}
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, err
	}
	if len(wrapper.AckInputSeqNum) > 0 {
		ack, err := ParseProtoInt64(wrapper.AckInputSeqNum)
		if err != nil {
			return nil, err
		}
		out.AckInputSeqNum = ack
	}
	rawEvents := wrapper.Events
	if len(rawEvents) == 0 {
		var single rawJSONServerEvent
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return nil, err
		}
		if single.EventType == "" && single.Error != nil {
			single.EventType = "error"
		}
		rawEvents = []rawJSONServerEvent{single}
	}
	for _, ev := range rawEvents {
		seqNum, err := ParseProtoInt64(ev.SeqNum)
		if err != nil {
			return nil, err
		}
		tsMicros, err := ParseProtoInt64(ev.TimestampMicros)
		if err != nil {
			return nil, err
		}
		switch ev.EventType {
		case "interaction.connected", "interaction.created", "interaction.reconnected", "interaction.reconnect":
			nextIn, err := ParseProtoInt64(ev.NextInputSeqNum)
			if err != nil {
				return nil, err
			}
			out.InteractionReconnected = &InteractionReconnectedEvent{
				NextInputSeqNum: nextIn,
			}
		case "function_invocation":
			out.Events = append(out.Events, &ServerEvent{
				SeqNum:          seqNum,
				TimestampMicros: tsMicros,
				Payload: &EventPayload{
					FunctionInvocation: &FunctionInvocation{
						CallID:    ev.CallID,
						Name:      ev.Name,
						Arguments: ev.Arguments,
					},
				},
			})
		case "step", "step.start":
			switch {
			case ev.Step != nil && (ev.Step.Type == "function_call" || ev.Step.FunctionCall != nil):
				callID := ev.Step.CallID
				if callID == "" {
					callID = ev.Step.ID
				}
				name := ev.Step.Name
				args := ev.Step.Arguments
				if fc := ev.Step.FunctionCall; fc != nil {
					if callID == "" {
						if fc.CallID != "" {
							callID = fc.CallID
						} else {
							callID = fc.ID
						}
					}
					if name == "" {
						name = fc.Name
					}
					if args == nil {
						args = fc.Arguments
					}
				}
				out.Events = append(out.Events, &ServerEvent{
					SeqNum:          seqNum,
					TimestampMicros: tsMicros,
					Payload: &EventPayload{
						FunctionInvocation: &FunctionInvocation{
							CallID:    callID,
							Name:      name,
							Arguments: args,
						},
					},
				})
			case ev.Step == nil && ev.Type == "function_call":
				callID := ev.CallID
				if callID == "" {
					callID = ev.ID
				}
				out.Events = append(out.Events, &ServerEvent{
					SeqNum:          seqNum,
					TimestampMicros: tsMicros,
					Payload: &EventPayload{
						FunctionInvocation: &FunctionInvocation{
							CallID:    callID,
							Name:      ev.Name,
							Arguments: ev.Arguments,
						},
					},
				})
			}
		case "call_hook_request":
			out.Events = append(out.Events, &ServerEvent{
				SeqNum:          seqNum,
				TimestampMicros: tsMicros,
				Payload: &EventPayload{
					CallHookRequest: &CallHookRequest{
						RequestID: ev.RequestID,
						Name:      ev.Name,
						HookType:  ev.HookType,
					},
				},
			})
		case "interaction.completed", "interaction.complete":
			rpcStatus, _ := parseServerStatus(ev.Status)
			out.InteractionCompleted = &InteractionCompletedEvent{
				Status: rpcStatus,
			}
		case "interaction.status_update":
			_, statusStr := parseServerStatus(ev.Status)
			if statusStr == "" {
				statusStr = ev.InteractionStatus
			}
			switch strings.ToLower(strings.TrimSpace(statusStr)) {
			case "completed":
				out.InteractionCompleted = &InteractionCompletedEvent{}
			case "failed":
				out.InteractionCompleted = &InteractionCompletedEvent{
					Status: &RPCStatus{Code: 13, Message: ev.Message},
				}
			case "cancelled", "canceled":
				out.InteractionCompleted = &InteractionCompletedEvent{
					Status: &RPCStatus{Code: 1, Message: ev.Message},
				}
			}
		case "reconnection_required":
			out.ReconnectionRequired = &ReconnectionRequiredEvent{}
		case "yield":
			nextIn, err := ParseProtoInt64(ev.NextInputSeqNum)
			if err != nil {
				return nil, err
			}
			out.Yield = &YieldEvent{
				NextInputSeqNum: nextIn,
			}
		case "error":
			msg := ev.Message
			code := 13
			rpcStatus, statusStr := parseServerStatus(ev.Status)
			if ev.Error != nil {
				if ev.Error.Message != "" {
					msg = ev.Error.Message
				}
				code = parseServerErrorCode(ev.Error.Code, ev.Error.Status)
			} else if rpcStatus != nil {
				if rpcStatus.Message != "" {
					msg = rpcStatus.Message
				}
				if rpcStatus.Code != 0 {
					code = int(rpcStatus.Code)
				}
			} else {
				code = parseServerErrorCode(nil, statusStr)
			}
			msg = RedactSecretsInString(msg)
			if msg == "" {
				msg = "server stream error"
			}
			httpStatus := GRPCCodeToHTTPStatus(code)
			return nil, &TransportError{
				Op:         msg,
				StatusCode: httpStatus,
				GRPCCode:   code,
				Message:    msg,
				Body:       RedactSecretsInString(string(trimmed)),
			}
		}
	}
	return out, nil
}
