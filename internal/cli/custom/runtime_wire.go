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
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/url"
	"sort"
	"strings"
)

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5

	maxGRPCFrameBytes    = 32 << 20 // 32 MiB
	maxProtoNestingDepth = 64
)

// Client -> Server outbound wire messages (`ClientEventBatch`).

type clientEventBatch struct {
	Events               []*clientEvent
	AckOutputSeqNum      int64
	ReconnectInteraction *reconnectInteractionEvent
	Disconnect           *disconnectEvent
}

type reconnectInteractionEvent struct {
	InteractionID    string
	NextOutputSeqNum int64
	Background       bool
	ClientID         string
}

type disconnectEvent struct{}

type clientEvent struct {
	Payload         *wirePayload
	SeqNum          int64
	TimestampMicros int64
}

type wirePayload struct {
	// Bidirectional step envelope (`Payload.step`, field 11).
	Step *wireStep
}

type wireStep struct {
	ToolCall   *toolCallStep
	ToolResult *toolResultStep
}

type toolCallStep struct {
	ID           string
	FunctionCall *functionCallStep
}

type functionCallStep struct {
	Name      string
	Arguments map[string]any
}

type toolResultStep struct {
	CallID         string
	FunctionResult *functionResultStep
}

type functionResultStep struct {
	Name    string
	IsError bool
	Result  any
}

// Server -> Client inbound wire messages (`ServerEventBatch`).

type serverEventBatch struct {
	Events                 []*serverEvent
	AckInputSeqNum         int64
	InteractionCompleted   *interactionCompletedEvent
	InteractionReconnected *interactionReconnectedEvent
	ReconnectionRequired   *reconnectionRequiredEvent
	Yield                  *yieldEvent
}

type serverEvent struct {
	Payload *wirePayload
	SeqNum  int64
}

type interactionCompletedEvent struct{}

type interactionReconnectedEvent struct {
	NextInputSeqNum int64
}

type reconnectionRequiredEvent struct{}

type yieldEvent struct {
	NextInputSeqNum int64
}

// gRPC frame helpers (uncompressed 5-byte length-prefixed frames).

func grpcWriteFrame(w io.Writer, msg []byte) error {
	if len(msg) > maxGRPCFrameBytes {
		return fmt.Errorf("outbound gRPC frame size %d exceeds limit %d", len(msg), maxGRPCFrameBytes)
	}
	var hdr [5]byte // hdr[0] == 0 denotes an uncompressed gRPC frame.
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

func grpcReadFrame(r io.Reader) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != 0 {
		return nil, fmt.Errorf("unsupported compressed gRPC frame flag %d", hdr[0])
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > maxGRPCFrameBytes {
		return nil, fmt.Errorf("inbound gRPC frame size %d exceeds limit %d", n, maxGRPCFrameBytes)
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

func decodeGRPCMessage(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if unescaped, err := url.PathUnescape(trimmed); err == nil {
		return unescaped
	}
	return trimmed
}

// Low-level protobuf binary encoding primitives.

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

// Outbound `ClientEventBatch` encoder.

func (b *clientEventBatch) marshalWire() []byte {
	if b == nil {
		return nil
	}
	var out []byte
	for _, ev := range b.Events {
		if ev != nil {
			out = appendBytesField(out, 1, ev.marshalWire())
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
		sub = appendStringField(sub, 4, r.ClientID)
		out = appendBytesField(out, 8, sub)
	}
	if b.Disconnect != nil {
		out = appendBytesField(out, 9, nil)
	}
	return out
}

func (e *clientEvent) marshalWire() []byte {
	var out []byte
	if p := e.Payload; p != nil {
		var sub []byte
		if st := p.Step; st != nil && st.ToolResult != nil {
			tr := st.ToolResult
			var tb []byte
			tb = appendStringField(tb, 1, tr.CallID)
			if fnRes := tr.FunctionResult; fnRes != nil {
				var frb []byte
				frb = appendStringField(frb, 2, fnRes.Name)
				frb = appendBoolField(frb, 3, fnRes.IsError)
				frb = appendBytesField(frb, 7, encodeProtoValue(fnRes.Result))
				tb = appendBytesField(tb, 3, frb)
			}
			sub = appendBytesField(sub, 11, appendBytesField(nil, 5, tb))
		}
		out = appendBytesField(out, 3, sub)
	}
	if e.SeqNum != 0 {
		out = appendVarintField(out, 10, uint64(e.SeqNum))
	}
	if e.TimestampMicros != 0 {
		out = appendVarintField(out, 11, uint64(e.TimestampMicros))
	}
	return out
}

func encodeProtoValue(v any) []byte {
	var out []byte
	switch val := v.(type) {
	case nil:
		return appendVarintField(out, 1, 0) // NullValue.NULL_VALUE
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
	case float64:
		return encodeNumberValue(val)
	case string:
		out = appendTag(out, 3, wireBytes)
		out = binary.AppendUvarint(out, uint64(len(val)))
		return append(out, val...)
	case []any:
		var listBuf []byte
		for _, elem := range val {
			listBuf = appendBytesField(listBuf, 1, encodeProtoValue(elem))
		}
		return appendBytesField(out, 6, listBuf)
	case []string:
		var listBuf []byte
		for _, elem := range val {
			listBuf = appendBytesField(listBuf, 1, encodeProtoValue(elem))
		}
		return appendBytesField(out, 6, listBuf)
	case map[string]any:
		return appendBytesField(out, 5, encodeProtoStruct(val))
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

func encodeProtoStruct(m map[string]any) []byte {
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
		entry = appendBytesField(entry, 2, encodeProtoValue(m[k]))
		out = appendBytesField(out, 1, entry)
	}
	return out
}

// Low-level protobuf binary decoding primitives.

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

func consumeField(b []byte) (fieldNum int, wireType int, valVarint uint64, valBytes []byte, consumed int, err error) {
	tag, n, err := consumeVarint(b)
	if err != nil {
		return 0, 0, 0, nil, 0, err
	}
	fieldNum = int(tag >> 3)
	wireType = int(tag & 0x7)
	if fieldNum <= 0 {
		return 0, 0, 0, nil, 0, fmt.Errorf("invalid protobuf field number %d", fieldNum)
	}
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

// Inbound `ServerEventBatch` decoder.

func unmarshalServerEventBatch(data []byte) (*serverEventBatch, error) {
	out := &serverEventBatch{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := consumeField(data)
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
			out.InteractionCompleted = &interactionCompletedEvent{}
		case num == 8 && wt == wireBytes:
			rec, err := unmarshalInteractionReconnected(vb)
			if err != nil {
				return nil, err
			}
			out.InteractionReconnected = rec
		case num == 9 && wt == wireBytes:
			out.ReconnectionRequired = &reconnectionRequiredEvent{}
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

func unmarshalInteractionReconnected(data []byte) (*interactionReconnectedEvent, error) {
	out := &interactionReconnectedEvent{}
	for len(data) > 0 {
		num, wt, vv, _, n, err := consumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		if num == 2 && wt == wireVarint {
			out.NextInputSeqNum = int64(vv)
		}
	}
	return out, nil
}

func unmarshalYield(data []byte) (*yieldEvent, error) {
	out := &yieldEvent{}
	for len(data) > 0 {
		num, wt, vv, _, n, err := consumeField(data)
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

func unmarshalServerEvent(data []byte) (*serverEvent, error) {
	out := &serverEvent{}
	for len(data) > 0 {
		num, wt, vv, vb, n, err := consumeField(data)
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
		}
	}
	return out, nil
}

func unmarshalPayload(data []byte) (*wirePayload, error) {
	out := &wirePayload{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := consumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		if num == 11 && wt == wireBytes {
			st, err := unmarshalStep(vb)
			if err != nil {
				return nil, err
			}
			out.Step = st
		}
	}
	return out, nil
}

func unmarshalStep(data []byte) (*wireStep, error) {
	out := &wireStep{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := consumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		if num == 4 && wt == wireBytes {
			tc, err := unmarshalToolCallStep(vb)
			if err != nil {
				return nil, err
			}
			out.ToolCall = tc
		}
	}
	return out, nil
}

func unmarshalToolCallStep(data []byte) (*toolCallStep, error) {
	out := &toolCallStep{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := consumeField(data)
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

func unmarshalFunctionCallStep(data []byte) (*functionCallStep, error) {
	out := &functionCallStep{}
	for len(data) > 0 {
		num, wt, _, vb, n, err := consumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		switch {
		case num == 1 && wt == wireBytes:
			out.Name = string(vb)
		case num == 2 && wt == wireBytes:
			m, err := decodeProtoStruct(vb, 0)
			if err != nil {
				return nil, err
			}
			out.Arguments = m
		}
	}
	return out, nil
}

func decodeProtoStruct(data []byte, depth int) (map[string]any, error) {
	if depth > maxProtoNestingDepth {
		return nil, fmt.Errorf("protobuf struct nesting exceeds maximum depth %d", maxProtoNestingDepth)
	}
	out := make(map[string]any)
	for len(data) > 0 {
		num, wt, _, vb, n, err := consumeField(data)
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
			enum, ewt, _, evb, en, err := consumeField(entryData)
			if err != nil {
				return nil, err
			}
			entryData = entryData[en:]
			switch {
			case enum == 1 && ewt == wireBytes:
				entryKey = string(evb)
			case enum == 2 && ewt == wireBytes:
				v, err := decodeProtoValue(evb, depth+1)
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

func decodeProtoValue(data []byte, depth int) (any, error) {
	if depth > maxProtoNestingDepth {
		return nil, fmt.Errorf("protobuf value nesting exceeds maximum depth %d", maxProtoNestingDepth)
	}
	var res any
	for len(data) > 0 {
		num, wt, vv, vb, n, err := consumeField(data)
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
			m, err := decodeProtoStruct(vb, depth+1)
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
		}
	}
	return res, nil
}

func decodeProtoList(data []byte, depth int) ([]any, error) {
	if depth > maxProtoNestingDepth {
		return nil, fmt.Errorf("protobuf list nesting exceeds maximum depth %d", maxProtoNestingDepth)
	}
	var out []any
	for len(data) > 0 {
		num, wt, _, vb, n, err := consumeField(data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		if num == 1 && wt == wireBytes {
			v, err := decodeProtoValue(vb, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	return out, nil
}
