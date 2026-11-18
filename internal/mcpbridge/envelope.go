package mcpbridge

import (
	"bytes"
	"encoding/json"
)

// Envelope remembers a bounded JSON-RPC envelope independently of the result body.
// It reads top-level members across writes, including an id following a large result.
type Envelope struct {
	resultFields             bool
	errorFields              bool
	child                    *Envelope
	fieldLength              int
	depth                    int
	quoted, escaped, started bool
	field                    []byte
	overflow                 bool
	message                  map[string]any
	// A batch keeps each response's envelope until its closing bracket.
	batch, closed bool
	item          *Envelope
	items         [][]byte
}

func (e *Envelope) Feed(p []byte) {
	if e.batch {
		e.feedBatch(p)
		return
	}
	if e.started && e.depth == 0 {
		return
	}
	for n, b := range p {
		if !e.started {
			if b == '[' && !e.resultFields && !e.errorFields {
				e.batch = true
				e.feedBatch(p[n+1:])
				return
			}
			if b == '{' {
				e.started = true
				e.depth = 1
				e.message = map[string]any{}
			}
			continue
		}
		boundary := !e.quoted && ((b == ',' && e.depth == 1) || (b == '}' && e.depth == 1))
		if e.child != nil && e.child.depth > 0 {
			e.child.Feed([]byte{b})
		}
		if boundary {
			e.member()
			if b == '}' {
				e.depth = 0
				return
			}
			continue
		}
		if !e.resultFields && !e.errorFields && e.child == nil && b == '{' && e.depth == 1 {
			sep := bytes.IndexByte(e.field, ':')
			var key string
			if sep >= 0 && json.Unmarshal(bytes.TrimSpace(e.field[:sep]), &key) == nil && (key == "result" || key == "error") {
				e.child = &Envelope{resultFields: key == "result", errorFields: key == "error"}
				e.child.Feed([]byte{b})
			}
		}
		e.fieldLength++
		if len(e.field) < MaxObserved {
			e.field = append(e.field, b)
		} else {
			e.overflow = true
		}
		if e.quoted {
			if e.escaped {
				e.escaped = false
			} else if b == '\\' {
				e.escaped = true
			} else if b == '"' {
				e.quoted = false
			}
			continue
		}
		switch b {
		case '"':
			e.quoted = true
		case '{', '[':
			e.depth++
		case '}', ']':
			e.depth--
		}
	}
}
func (e *Envelope) member() {
	field := e.field
	e.field = nil
	defer func() { e.overflow = false; e.fieldLength = 0; e.child = nil }()
	sep := bytes.IndexByte(field, ':')
	if sep < 0 {
		return
	}
	var key string
	if json.Unmarshal(bytes.TrimSpace(field[:sep]), &key) != nil {
		return
	}
	// Only protocol fields affect acknowledgement. Extra fields never grow retained state.
	if e.resultFields {
		switch key {
		case "isError", "_meta", "structuredContent", "content", "inputRequests", "requestState", "resultType", "nextCursor":
		default:
			return
		}
	} else if e.errorFields {
		switch key {
		case "code", "message", "data":
		default:
			return
		}
	} else {
		switch key {
		case "id", "method", "jsonrpc", "result", "error":
		default:
			return
		}
	}
	if e.overflow {
		e.message["_content_omitted"] = true
		if key == "result" {
			e.message[key] = map[string]any{}
			if e.child != nil {
				var result map[string]any
				if json.Unmarshal(e.child.JSON(), &result) == nil {
					delete(result, "_content_omitted")
					e.message[key] = result
				}
			}
			e.message["_result_size"] = e.fieldLength - sep - 1
		}
		if key == "error" {
			e.message[key] = map[string]any{"code": -32603}
			if e.child != nil {
				var rpc map[string]any
				if json.Unmarshal(e.child.JSON(), &rpc) == nil {
					delete(rpc, "_content_omitted")
					e.message[key] = rpc
				}
			}
		}
		return
	}
	dec := json.NewDecoder(bytes.NewReader(field[sep+1:]))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) == nil {
		e.message[key] = value
	}
}

func (e *Envelope) feedBatch(p []byte) {
	for n := 0; n < len(p) && !e.closed; n++ {
		if e.item != nil {
			e.item.Feed(p[n : n+1])
			if e.item.depth == 0 {
				e.items = append(e.items, e.item.JSON())
				e.item = nil
			}
			continue
		}
		switch p[n] {
		case '{':
			e.item = &Envelope{}
			e.item.Feed(p[n : n+1])
		case ']':
			e.closed = true
		}
	}
}

// JSON returns an envelope only after the full object, or batch, has been written.
func (e *Envelope) JSON() []byte {
	if e.batch {
		if !e.closed {
			return nil
		}
		return append(append([]byte("["), bytes.Join(e.items, []byte(","))...), ']')
	}
	if !e.started || e.depth != 0 {
		return nil
	}
	b, _ := json.Marshal(e.message)
	return b
}
