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
	"fmt"
	"io"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5

	// MaxGRPCFrameBytes bounds uncompressed gRPC and WebSocket message frames at 32 MiB.
	MaxGRPCFrameBytes = 32 << 20

	// MaxProtoNestingDepth bounds recursive Struct/Value/ListValue decoding.
	MaxProtoNestingDepth = 64
)

// GRPCWriteFrame writes msg as an uncompressed 5-byte length-prefixed gRPC frame to w.
func GRPCWriteFrame(w io.Writer, msg []byte) error {
	if len(msg) > MaxGRPCFrameBytes {
		return fmt.Errorf("outbound gRPC frame size %d exceeds limit %d", len(msg), MaxGRPCFrameBytes)
	}
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(msg)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(msg) > 0 {
		if _, err := w.Write(msg); err != nil {
			return err
		}
	}
	return nil
}

// GRPCReadFrame reads an uncompressed 5-byte length-prefixed gRPC frame from r.
func GRPCReadFrame(r io.Reader) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != 0 {
		return nil, fmt.Errorf("unsupported compressed gRPC frame flag %d", hdr[0])
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > MaxGRPCFrameBytes {
		return nil, fmt.Errorf("inbound gRPC frame size %d exceeds limit %d", n, MaxGRPCFrameBytes)
	}
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// DecodeGRPCMessage percent-decodes a grpc-message header or trailer value.
func DecodeGRPCMessage(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if unescaped, err := url.PathUnescape(trimmed); err == nil {
		return unescaped
	}
	return trimmed
}

func appendTag(dst []byte, fieldNum int, wireType int) []byte {
	return binary.AppendUvarint(dst, uint64(fieldNum<<3|wireType))
}

func appendVarintField(dst []byte, fieldNum int, v uint64) []byte {
	dst = appendTag(dst, fieldNum, wireVarint)
	return binary.AppendUvarint(dst, v)
}

func appendBoolField(dst []byte, fieldNum int, v bool) []byte {
	if !v {
		return dst
	}
	return appendVarintField(dst, fieldNum, 1)
}

func appendStringField(dst []byte, fieldNum int, s string) []byte {
	if s == "" {
		return dst
	}
	dst = appendTag(dst, fieldNum, wireBytes)
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func appendBytesField(dst []byte, fieldNum int, b []byte) []byte {
	dst = appendTag(dst, fieldNum, wireBytes)
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// MarshalClientEventBatch encodes b into protobuf wire format matching genai.ClientEventBatch.
// All wire field numbers in this file must stay synchronized with the canonical
// genai.ClientEventBatch and genai.ServerEventBatch protobuf definitions (verified by
// differential protobuf round-trip tests).
func MarshalClientEventBatch(b *ClientEventBatch) []byte {
	if b == nil {
		return nil
	}
	var out []byte
	for _, ev := range b.Events {
		if ev != nil {
			out = appendBytesField(out, 1, marshalClientEvent(ev))
		}
	}
	if b.AckOutputSeqNum != 0 {
		out = appendVarintField(out, 2, uint64(b.AckOutputSeqNum))
	}
	if r := b.ReconnectInteraction; r != nil {
		var sub []byte
		sub = appendStringField(sub, 1, r.InteractionID)
		if r.NextOutputSeqNum != 0 {
			sub = appendVarintField(sub, 2, uint64(r.NextOutputSeqNum))
		}
		sub = appendBoolField(sub, 3, r.Background)
		sub = appendStringField(sub, 5, r.ClientID)
		out = appendBytesField(out, 8, sub)
	}
	if b.Disconnect != nil {
		out = appendBytesField(out, 9, nil)
	}
	return out
}

// MarshalClientEvent encodes a single ClientEvent into protobuf wire format.
func MarshalClientEvent(e *ClientEvent) []byte {
	if e == nil {
		return nil
	}
	return marshalClientEvent(e)
}

func marshalClientEvent(e *ClientEvent) []byte {
	var out []byte
	if p := e.Payload; p != nil {
		out = appendBytesField(out, 3, marshalPayload(p))
	}
	if e.SeqNum != 0 {
		out = appendVarintField(out, 10, uint64(e.SeqNum))
	}
	if e.TimestampMicros != 0 {
		out = appendVarintField(out, 11, uint64(e.TimestampMicros))
	}
	return out
}

func marshalPayload(p *EventPayload) []byte {
	var sub []byte
	sub = appendStringField(sub, 6, p.SubInteractionID)
	if st := p.Step; st != nil {
		var stepBytes []byte
		if tc := st.ToolCall; tc != nil {
			var tcb []byte
			tcb = appendStringField(tcb, 1, tc.ID)
			if fc := tc.FunctionCall; fc != nil {
				var fcb []byte
				fcb = appendStringField(fcb, 1, fc.Name)
				fcb = appendBytesField(fcb, 2, EncodeProtoStruct(fc.Arguments))
				tcb = appendBytesField(tcb, 3, fcb)
			}
			stepBytes = appendBytesField(stepBytes, 4, tcb)
		}
		if tr := st.ToolResult; tr != nil {
			var tb []byte
			tb = appendStringField(tb, 1, tr.CallID)
			if fnRes := tr.FunctionResult; fnRes != nil {
				var frb []byte
				frb = appendStringField(frb, 2, fnRes.Name)
				frb = appendBoolField(frb, 3, fnRes.IsError)
				frb = appendBytesField(frb, 7, EncodeProtoValue(fnRes.Result))
				tb = appendBytesField(tb, 3, frb)
			}
			stepBytes = appendBytesField(stepBytes, 5, tb)
		}
		if len(stepBytes) > 0 {
			sub = appendBytesField(sub, 11, stepBytes)
		}
	}
	if fi := p.FunctionInvocation; fi != nil {
		var fib []byte
		fib = appendStringField(fib, 1, fi.CallID)
		fib = appendStringField(fib, 2, fi.Name)
		fib = appendBytesField(fib, 3, EncodeProtoStruct(fi.Arguments))
		sub = appendBytesField(sub, 14, fib)
	}
	if req := p.CallHookRequest; req != nil {
		if len(req.RawWire) > 0 {
			sub = appendBytesField(sub, 15, req.RawWire)
		} else {
			var hrb []byte
			hrb = appendStringField(hrb, 1, req.RequestID)
			hrb = appendStringField(hrb, 2, req.Name)
			if req.HookType != 0 {
				hrb = appendVarintField(hrb, 7, uint64(req.HookType))
			}
			sub = appendBytesField(sub, 15, hrb)
		}
	}
	if resp := p.CallHookResponse; resp != nil {
		if len(resp.RawWire) > 0 {
			sub = appendBytesField(sub, 16, resp.RawWire)
		} else {
			var hresp []byte
			hresp = appendStringField(hresp, 1, resp.RequestID)
			if resp.EmptyResult {
				hresp = appendBytesField(hresp, 4, nil)
			}
			hresp = appendStringField(hresp, 5, resp.ErrorMessage)
			sub = appendBytesField(sub, 16, hresp)
		}
	}
	if fr := p.FunctionResult; fr != nil {
		var frb []byte
		frb = appendStringField(frb, 1, fr.CallID)
		frb = appendStringField(frb, 2, fr.Name)
		frb = appendBoolField(frb, 3, fr.IsError)
		frb = appendBytesField(frb, 4, EncodeProtoValue(fr.Result))
		sub = appendBytesField(sub, 18, frb)
	}
	return sub
}

// MarshalServerEventBatch encodes b into protobuf wire format matching genai.ServerEventBatch.
func MarshalServerEventBatch(b *ServerEventBatch) []byte {
	if b == nil {
		return nil
	}
	var out []byte
	for _, ev := range b.Events {
		if ev != nil {
			var evBytes []byte
			if ev.Payload != nil {
				evBytes = appendBytesField(evBytes, 3, marshalPayload(ev.Payload))
			}
			if ev.SeqNum != 0 {
				evBytes = appendVarintField(evBytes, 10, uint64(ev.SeqNum))
			}
			if ev.TimestampMicros != 0 {
				evBytes = appendVarintField(evBytes, 11, uint64(ev.TimestampMicros))
			}
			out = appendBytesField(out, 1, evBytes)
		}
	}
	if b.AckInputSeqNum != 0 {
		out = appendVarintField(out, 2, uint64(b.AckInputSeqNum))
	}
	if b.InteractionCompleted != nil {
		var comp []byte
		if st := b.InteractionCompleted.Status; st != nil {
			var stb []byte
			if st.Code != 0 {
				stb = appendVarintField(stb, 1, uint64(st.Code))
			}
			stb = appendStringField(stb, 2, st.Message)
			comp = appendBytesField(comp, 1, stb)
		}
		out = appendBytesField(out, 6, comp)
	}
	if b.InteractionReconnected != nil {
		var rec []byte
		if b.InteractionReconnected.NextInputSeqNum != 0 {
			rec = appendVarintField(rec, 1, uint64(b.InteractionReconnected.NextInputSeqNum))
		}
		out = appendBytesField(out, 8, rec)
	}
	if b.ReconnectionRequired != nil {
		out = appendBytesField(out, 9, nil)
	}
	if b.Yield != nil {
		var yb []byte
		if b.Yield.NextInputSeqNum != 0 {
			yb = appendVarintField(yb, 1, uint64(b.Yield.NextInputSeqNum))
		}
		out = appendBytesField(out, 10, yb)
	}
	return out
}

// EncodeProtoValue encodes a Go value into protobuf wire format matching genai.Value.
func EncodeProtoValue(v any) []byte {
	var out []byte
	switch val := v.(type) {
	case nil:
		return appendVarintField(out, 1, 0)
	case bool:
		out = appendTag(out, 4, wireVarint)
		if val {
			return binary.AppendUvarint(out, 1)
		}
		return binary.AppendUvarint(out, 0)
	case int:
		return encodeNumberValue(float64(val))
	case int32:
		return encodeNumberValue(float64(val))
	case int64:
		return encodeNumberValue(float64(val))
	case uint:
		return encodeNumberValue(float64(val))
	case uint32:
		return encodeNumberValue(float64(val))
	case uint64:
		return encodeNumberValue(float64(val))
	case float32:
		return encodeNumberValue(float64(val))
	case float64:
		return encodeNumberValue(val)
	case string:
		out = appendTag(out, 3, wireBytes)
		out = binary.AppendUvarint(out, uint64(len(val)))
		return append(out, val...)
	case []any:
		var listBuf []byte
		for _, elem := range val {
			listBuf = appendBytesField(listBuf, 1, EncodeProtoValue(elem))
		}
		return appendBytesField(out, 6, listBuf)
	case []string:
		var listBuf []byte
		for _, elem := range val {
			listBuf = appendBytesField(listBuf, 1, EncodeProtoValue(elem))
		}
		return appendBytesField(out, 6, listBuf)
	case map[string]any:
		return appendBytesField(out, 5, EncodeProtoStruct(val))
	default:
		s := fmt.Sprint(val)
		out = appendTag(out, 3, wireBytes)
		out = binary.AppendUvarint(out, uint64(len(s)))
		return append(out, s...)
	}
}

func encodeNumberValue(f float64) []byte {
	out := appendTag(nil, 2, wireFixed64)
	return binary.LittleEndian.AppendUint64(out, math.Float64bits(f))
}

// EncodeProtoStruct encodes a Go map into deterministic protobuf wire format matching genai.Struct.
func EncodeProtoStruct(m map[string]any) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []byte
	for _, k := range keys {
		var entry []byte
		entry = appendTag(entry, 1, wireBytes)
		entry = binary.AppendUvarint(entry, uint64(len(k)))
		entry = append(entry, k...)
		entry = appendBytesField(entry, 2, EncodeProtoValue(m[k]))
		out = appendBytesField(out, 1, entry)
	}
	return out
}

func consumeVarint(b []byte) (uint64, int, error) {
	v, n := binary.Uvarint(b)
	if n == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	if n < 0 {
		return 0, 0, fmt.Errorf("protobuf varint overflows uint64")
	}
	return v, n, nil
}

// ConsumeField decodes a single protobuf wire field from b, returning its field number, wire type, value, and byte length.
func ConsumeField(b []byte) (fieldNum int, wireType int, valVarint uint64, valBytes []byte, consumed int, err error) {
	tag, n, err := consumeVarint(b)
	if err != nil {
		return 0, 0, 0, nil, 0, err
	}
	rawFieldNum := tag >> 3
	if rawFieldNum == 0 || rawFieldNum > (1<<29)-1 {
		return 0, 0, 0, nil, 0, fmt.Errorf("invalid protobuf field number %d", rawFieldNum)
	}
	fieldNum = int(rawFieldNum)
	wireType = int(tag & 0x7)
	rest := b[n:]
	switch wireType {
	case wireVarint:
		v, m, err := consumeVarint(rest)
		if err != nil {
			return 0, 0, 0, nil, 0, err
		}
		return fieldNum, wireType, v, nil, n + m, nil
	case wireFixed64:
		if len(rest) < 8 {
			return 0, 0, 0, nil, 0, io.ErrUnexpectedEOF
		}
		return fieldNum, wireType, binary.LittleEndian.Uint64(rest[:8]), nil, n + 8, nil
	case wireBytes:
		l, m, err := consumeVarint(rest)
		if err != nil {
			return 0, 0, 0, nil, 0, err
		}
		after := rest[m:]
		if l > uint64(len(after)) {
			return 0, 0, 0, nil, 0, io.ErrUnexpectedEOF
		}
		return fieldNum, wireType, 0, after[:int(l)], n + m + int(l), nil
	case wireFixed32:
		if len(rest) < 4 {
			return 0, 0, 0, nil, 0, io.ErrUnexpectedEOF
		}
		return fieldNum, wireType, uint64(binary.LittleEndian.Uint32(rest[:4])), nil, n + 4, nil
	default:
		return 0, 0, 0, nil, 0, fmt.Errorf("unsupported protobuf wire type %d", wireType)
	}
}

// UnmarshalServerEventBatch decodes protobuf wire bytes into a ServerEventBatch.
// All wire field numbers decoded below must stay synchronized with the canonical
// genai.ServerEventBatch protobuf definition.
func UnmarshalServerEventBatch(data []byte) (*ServerEventBatch, error) {
	out := &ServerEventBatch{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			ev, err := unmarshalServerEvent(vb)
			if err != nil {
				return nil, err
			}
			out.Events = append(out.Events, ev)
		case num == 2 && wt == wireVarint:
			out.AckInputSeqNum = int64(vv)
		case num == 6 && wt == wireBytes:
			comp, err := unmarshalInteractionCompleted(vb)
			if err != nil {
				return nil, err
			}
			out.InteractionCompleted = comp
		case num == 8 && wt == wireBytes:
			rec, err := unmarshalInteractionReconnected(vb)
			if err != nil {
				return nil, err
			}
			out.InteractionReconnected = rec
		case num == 9 && wt == wireBytes:
			out.ReconnectionRequired = &ReconnectionRequiredEvent{}
		case num == 10 && wt == wireBytes:
			y, err := unmarshalYield(vb)
			if err != nil {
				return nil, err
			}
			out.Yield = y
		}
	}
	return out, nil
}

// UnmarshalClientEventBatch decodes protobuf wire bytes into a ClientEventBatch.
func UnmarshalClientEventBatch(data []byte) (*ClientEventBatch, error) {
	out := &ClientEventBatch{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			ev, err := unmarshalClientEvent(vb)
			if err != nil {
				return nil, err
			}
			out.Events = append(out.Events, ev)
		case num == 2 && wt == wireVarint:
			out.AckOutputSeqNum = int64(vv)
		case num == 8 && wt == wireBytes:
			rec, err := unmarshalReconnectInteraction(vb)
			if err != nil {
				return nil, err
			}
			out.ReconnectInteraction = rec
		case num == 9 && wt == wireBytes:
			out.Disconnect = &DisconnectEvent{}
		}
	}
	return out, nil
}

func unmarshalReconnectInteraction(data []byte) (*ReconnectInteractionEvent, error) {
	out := &ReconnectInteractionEvent{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			out.InteractionID = string(vb)
		case num == 2 && wt == wireVarint:
			out.NextOutputSeqNum = int64(vv)
		case num == 3 && wt == wireVarint:
			out.Background = vv != 0
		case (num == 5 || (num == 4 && out.ClientID == "")) && wt == wireBytes:
			out.ClientID = string(vb)
		}
	}
	return out, nil
}

func unmarshalClientEvent(data []byte) (*ClientEvent, error) {
	out := &ClientEvent{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 3 && wt == wireBytes:
			p, err := unmarshalPayload(vb)
			if err != nil {
				return nil, err
			}
			out.Payload = p
		case num == 10 && wt == wireVarint:
			out.SeqNum = int64(vv)
		case num == 11 && wt == wireVarint:
			out.TimestampMicros = int64(vv)
		}
	}
	return out, nil
}

func unmarshalInteractionCompleted(data []byte) (*InteractionCompletedEvent, error) {
	out := &InteractionCompletedEvent{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		if num == 1 && wt == wireBytes {
			st, err := unmarshalRPCStatus(vb)
			if err != nil {
				return nil, err
			}
			out.Status = st
		}
	}
	return out, nil
}

func unmarshalRPCStatus(data []byte) (*RPCStatus, error) {
	out := &RPCStatus{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireVarint:
			out.Code = int32(vv)
		case num == 2 && wt == wireBytes:
			out.Message = string(vb)
		}
	}
	return out, nil
}

func unmarshalInteractionReconnected(data []byte) (*InteractionReconnectedEvent, error) {
	out := &InteractionReconnectedEvent{}
	var legacySeq int64
	for len(data) > 0 {
		num, wt, vv, _, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireVarint:
			out.NextInputSeqNum = int64(vv)
		case num == 2 && wt == wireVarint:
			legacySeq = int64(vv)
		}
	}
	if out.NextInputSeqNum == 0 && legacySeq != 0 {
		out.NextInputSeqNum = legacySeq
	}
	return out, nil
}

func unmarshalYield(data []byte) (*YieldEvent, error) {
	out := &YieldEvent{}
	for len(data) > 0 {
		num, wt, vv, _, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		if num == 1 && wt == wireVarint {
			out.NextInputSeqNum = int64(vv)
		}
	}
	return out, nil
}

func unmarshalServerEvent(data []byte) (*ServerEvent, error) {
	out := &ServerEvent{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 3 && wt == wireBytes:
			p, err := unmarshalPayload(vb)
			if err != nil {
				return nil, err
			}
			out.Payload = p
		case num == 10 && wt == wireVarint:
			out.SeqNum = int64(vv)
		case num == 11 && wt == wireVarint:
			out.TimestampMicros = int64(vv)
		}
	}
	return out, nil
}

func unmarshalPayload(data []byte) (*EventPayload, error) {
	out := &EventPayload{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 6 && wt == wireBytes:
			out.SubInteractionID = string(vb)
		case num == 11 && wt == wireBytes:
			st, err := unmarshalStep(vb)
			if err != nil {
				return nil, err
			}
			out.Step = st
		case num == 14 && wt == wireBytes:
			fi, err := unmarshalFunctionInvocation(vb)
			if err != nil {
				return nil, err
			}
			out.FunctionInvocation = fi
		case num == 15 && wt == wireBytes:
			hr, err := unmarshalCallHookRequest(vb)
			if err != nil {
				return nil, err
			}
			out.CallHookRequest = hr
		case num == 16 && wt == wireBytes:
			resp, err := unmarshalCallHookResponse(vb)
			if err != nil {
				return nil, err
			}
			out.CallHookResponse = resp
		case num == 18 && wt == wireBytes:
			fr, err := unmarshalFunctionResultEvent(vb)
			if err != nil {
				return nil, err
			}
			out.FunctionResult = fr
		}
	}
	return out, nil
}

func unmarshalStep(data []byte) (*Step, error) {
	out := &Step{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 4 && wt == wireBytes:
			tc, err := unmarshalToolCallStep(vb)
			if err != nil {
				return nil, err
			}
			out.ToolCall = tc
		case num == 5 && wt == wireBytes:
			tr, err := unmarshalToolResultStep(vb)
			if err != nil {
				return nil, err
			}
			out.ToolResult = tr
		}
	}
	return out, nil
}

func unmarshalToolCallStep(data []byte) (*ToolCallStep, error) {
	out := &ToolCallStep{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			out.ID = string(vb)
		case num == 3 && wt == wireBytes:
			fc, err := unmarshalFunctionCallStep(vb)
			if err != nil {
				return nil, err
			}
			out.FunctionCall = fc
		}
	}
	return out, nil
}

func unmarshalFunctionCallStep(data []byte) (*FunctionCallStep, error) {
	out := &FunctionCallStep{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			out.Name = string(vb)
		case num == 2 && wt == wireBytes:
			m, err := DecodeProtoStruct(vb, 0)
			if err != nil {
				return nil, err
			}
			out.Arguments = m
		}
	}
	return out, nil
}

func unmarshalToolResultStep(data []byte) (*ToolResultStep, error) {
	out := &ToolResultStep{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			out.CallID = string(vb)
		case num == 3 && wt == wireBytes:
			fr, err := unmarshalFunctionResultStep(vb)
			if err != nil {
				return nil, err
			}
			out.FunctionResult = fr
		}
	}
	return out, nil
}

func unmarshalFunctionResultStep(data []byte) (*FunctionResultStep, error) {
	out := &FunctionResultStep{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 2 && wt == wireBytes:
			out.Name = string(vb)
		case num == 3 && wt == wireVarint:
			out.IsError = vv != 0
		case num == 7 && wt == wireBytes:
			v, err := DecodeProtoValue(vb, 0)
			if err != nil {
				return nil, err
			}
			out.Result = v
		}
	}
	return out, nil
}

func unmarshalFunctionInvocation(data []byte) (*FunctionInvocation, error) {
	out := &FunctionInvocation{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			out.CallID = string(vb)
		case num == 2 && wt == wireBytes:
			out.Name = string(vb)
		case num == 3 && wt == wireBytes:
			m, err := DecodeProtoStruct(vb, 0)
			if err != nil {
				return nil, err
			}
			out.Arguments = m
		}
	}
	return out, nil
}

func unmarshalFunctionResultEvent(data []byte) (*FunctionResultEvent, error) {
	out := &FunctionResultEvent{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			out.CallID = string(vb)
		case num == 2 && wt == wireBytes:
			out.Name = string(vb)
		case num == 3 && wt == wireVarint:
			out.IsError = vv != 0
		case num == 4 && wt == wireBytes:
			v, err := DecodeProtoValue(vb, 0)
			if err != nil {
				return nil, err
			}
			out.Result = v
		}
	}
	return out, nil
}

func unmarshalCallHookRequest(data []byte) (*CallHookRequest, error) {
	out := &CallHookRequest{
		RawWire: append([]byte(nil), data...),
	}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			out.RequestID = string(vb)
		case num == 2 && wt == wireBytes:
			out.Name = string(vb)
		case num == 7 && wt == wireVarint:
			out.HookType = int32(vv)
		}
	}
	return out, nil
}

func unmarshalCallHookResponse(data []byte) (*CallHookResponse, error) {
	out := &CallHookResponse{
		RawWire: append([]byte(nil), data...),
	}
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			out.RequestID = string(vb)
		case num == 4 && wt == wireBytes:
			out.EmptyResult = true
		case num == 5 && wt == wireBytes:
			out.ErrorMessage = string(vb)
		}
	}
	return out, nil
}

// DecodeProtoStruct decodes protobuf wire bytes matching genai.Struct into a Go map.
func DecodeProtoStruct(data []byte, depth int) (map[string]any, error) {
	if depth > MaxProtoNestingDepth {
		return nil, fmt.Errorf("protobuf struct nesting exceeds maximum depth %d", MaxProtoNestingDepth)
	}
	out := make(map[string]any)
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		if num != 1 || wt != wireBytes {
			continue
		}
		var entryKey string
		var entryVal any
		entryData := vb
		for len(entryData) > 0 {
			enum, ewt, _, evb, en, err := ConsumeField(entryData)
			if err != nil {
				return nil, err
			}
			entryData = entryData[en:]
			switch {
			case enum == 1 && ewt == wireBytes:
				entryKey = string(evb)
			case enum == 2 && ewt == wireBytes:
				v, err := DecodeProtoValue(evb, depth+1)
				if err != nil {
					return nil, err
				}
				entryVal = v
			}
		}
		out[entryKey] = entryVal
	}
	return out, nil
}

// DecodeProtoValue decodes protobuf wire bytes matching genai.Value into a native Go value.
func DecodeProtoValue(data []byte, depth int) (any, error) {
	if depth > MaxProtoNestingDepth {
		return nil, fmt.Errorf("protobuf value nesting exceeds maximum depth %d", MaxProtoNestingDepth)
	}
	var res any
	for len(data) > 0 {
		num, wt, vv, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireVarint:
			res = nil
		case num == 2 && wt == wireFixed64:
			res = math.Float64frombits(vv)
		case num == 3 && wt == wireBytes:
			res = string(vb)
		case num == 4 && wt == wireVarint:
			res = vv != 0
		case num == 5 && wt == wireBytes:
			m, err := DecodeProtoStruct(vb, depth+1)
			if err != nil {
				return nil, err
			}
			res = m
		case num == 6 && wt == wireBytes:
			items, err := decodeProtoList(vb, depth+1)
			if err != nil {
				return nil, err
			}
			res = items
		case num == 7 && wt == wireBytes:
			txt, err := decodeContentText(vb)
			if err != nil {
				return nil, err
			}
			res = txt
		}
	}
	return res, nil
}

func decodeContentText(data []byte) (any, error) {
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		if num == 10 && wt == wireBytes {
			inner := vb
			var textVal string
			for len(inner) > 0 {
				inum, iwt, _, ivb, in, ierr := ConsumeField(inner)
				if ierr != nil {
					return nil, ierr
				}
				inner = inner[in:]
				if inum == 3 && iwt == wireBytes {
					textVal = string(ivb)
				}
			}
			return textVal, nil
		}
	}
	return nil, nil
}

func decodeProtoList(data []byte, depth int) ([]any, error) {
	if depth > MaxProtoNestingDepth {
		return nil, fmt.Errorf("protobuf list nesting exceeds maximum depth %d", MaxProtoNestingDepth)
	}
	var out []any
	for len(data) > 0 {
		num, wt, _, vb, n, err := ConsumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		if num == 1 && wt == wireBytes {
			v, err := DecodeProtoValue(vb, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	return out, nil
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
