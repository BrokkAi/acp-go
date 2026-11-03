package acp

import (
	"bytes"
	"encoding/json"

	"github.com/BrokkAi/acp-go/internal/serdewire"
)

// encodeFrame encodes a frame exactly as the reference SDK writes one with
// serde_json (built with preserve_order): as serdewire.Encode writes the
// JSON encoding/json produces for it, which is what transport/http writes
// too. That covers nested MarshalJSON output and forwarded json.RawMessage
// values alike. A frame holding JSON the reference SDK cannot hold, such as
// invalid UTF-8 or a lone surrogate escape in a json.RawMessage, fails with
// serdewire.ErrUnrepresentable and is never written.
func encodeFrame(v any) ([]byte, error) {
	if p, ok := v.(packet); ok {
		return appendPacket(p)
	}
	return serdewire.Marshal(v)
}

// appendPacket writes a frame without marshaling the packet, which would
// re-scan its params and result. A packet's params and result hold compact
// JSON: encoding/json's output, or a guard's rewrite, which compactJSON has
// compacted; its id is in serde_json's form (canonicalID, or an integer this
// connection chose). The members are written in the order the reference SDK
// writes them, which is the packet's field order. A request or notification
// always carries its method, even an empty one, as the reference SDK writes
// it; a response has none.
func appendPacket(p packet) ([]byte, error) {
	params, result := p.Params, p.Result
	var err error
	if len(params) > 0 {
		if params, err = encodeMember(params, p.paramsTyped); err != nil {
			return nil, err
		}
	}
	if len(result) > 0 {
		if result, err = encodeMember(result, p.resultTyped); err != nil {
			return nil, err
		}
	}
	var rpcError []byte
	if p.Error != nil {
		// RPCError.MarshalJSON copies its data and extra members as they
		// are, so the object is compacted, as json.Marshal does, before it
		// is re-encoded.
		marshaled, err := p.Error.MarshalJSON()
		if err != nil {
			return nil, err
		}
		if rpcError, err = compactJSON(marshaled); err != nil {
			return nil, err
		}
		if rpcError, err = serdewire.Encode(rpcError); err != nil {
			return nil, err
		}
	}
	response := len(result) > 0 || rpcError != nil
	// The frame terminator the queue appends fits too.
	out := make([]byte, 0, 64+len(p.ID)+2*len(p.Method)+len(params)+len(result)+len(rpcError))
	out = append(out, `{"jsonrpc":`...)
	out = serdewire.AppendString(out, p.Version)
	if len(p.ID) > 0 {
		out = append(out, `,"id":`...)
		out = append(out, p.ID...)
	}
	if !response {
		out = append(out, `,"method":`...)
		out = serdewire.AppendString(out, p.Method)
	}
	if len(params) > 0 {
		out = append(out, `,"params":`...)
		out = append(out, params...)
	}
	if len(result) > 0 {
		out = append(out, `,"result":`...)
		out = append(out, result...)
	}
	if rpcError != nil {
		out = append(out, `,"error":`...)
		out = append(out, rpcError...)
	}
	return append(out, '}'), nil
}

// encodeMember writes a packet's params or result as serde_json writes it:
// the encoding of a Go value as serdewire.EncodeTyped reads it, and JSON
// text, such as forwarded params, as serdewire.Encode does.
func encodeMember(b []byte, typed bool) ([]byte, error) {
	if typed {
		return serdewire.EncodeTyped(b)
	}
	return serdewire.Encode(b)
}

// compactJSON compacts JSON a guard or a MarshalJSON method produced, which
// need not be compact, so the frame writer's precondition holds and no
// newline inside it reaches the wire. It fails for invalid JSON.
func compactJSON(raw []byte) ([]byte, error) {
	var compact bytes.Buffer
	compact.Grow(len(raw))
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
