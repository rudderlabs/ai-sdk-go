package rudderstackai

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
)

// jsonSafeMap returns a deep copy of m that the RudderStack client encodes without running any
// code of yours: analytics-go encodes on its own goroutine, where a panicking MarshalJSON would
// end the process, and a map shared with your code could change under it. Plain JSON values are
// copied; anything else (a struct, a type with MarshalJSON) is encoded now. A value that fails
// or panics while encoding is dropped and reported.
func jsonSafeMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if c, ok := jsonSafe(v); ok {
			out[k] = c
		} else {
			report(KindUserCallback, "MarshalJSON", errUnencodable)
		}
	}
	return out
}

var errUnencodable = errors.New("rudderstackai: a value could not be encoded as JSON; it was dropped")

func jsonSafe(v any) (any, bool) {
	switch x := v.(type) {
	case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return x, true
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case float32:
		return x, !math.IsNaN(float64(x)) && !math.IsInf(float64(x), 0)
	case []string:
		return slices.Clone(x), true
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			c, ok := jsonSafe(e)
			if !ok {
				return nil, false
			}
			out[k] = c
		}
		return out, true
	case []any:
		out := make([]any, len(x))
		for n, e := range x {
			c, ok := jsonSafe(e)
			if !ok {
				return nil, false
			}
			out[n] = c
		}
		return out, true
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for n, e := range x {
			c, ok := jsonSafe(e)
			if !ok {
				return nil, false
			}
			out[n] = c.(map[string]any)
		}
		return out, true
	}
	return encodeNow(v)
}

// encodeNow encodes v, containing a panic; ok is false when it fails.
func encodeNow(v any) (out any, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = nil, false
		}
	}()
	b, err := compactJSON(v)
	if err != nil || !json.Valid(b) {
		return nil, false
	}
	return json.RawMessage(b), true
}
