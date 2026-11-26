package rudderstackai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
)

// mcpResultView is what a tools/call event with no declared rule reads of a result: its size and
// the few top-level fields behind status, pending input, content types and pagination, as
// encoding/json shows them. The result is neither encoded into a tree nor copied: a plain value's
// size comes from the memoised sizer and its fields from reflection, and a serializer's own bytes
// are scanned once. A nil map means the value cannot be encoded.
func mcpResultView(v any) (result map[string]any, size int) {
	defer enterCapture()()
	defer func() {
		if r := recover(); r != nil {
			result, size = nil, 0
		}
	}()
	encoded := func() (map[string]any, int) {
		raw, err := compactJSON(v)
		if err != nil {
			return nil, 0
		}
		return scanResult(raw), len(raw)
	}
	if _, serializer := customTarget(reflect.ValueOf(v)); serializer {
		return encoded() // the serializer runs once; its bytes are the result
	}
	n, err := exactSize(v)
	switch {
	case err == nil:
		return plainResult(v), n
	case errors.Is(err, errSerializer):
		return encoded() // a serializer nested in it: encoded once, as before
	}
	return nil, 0
}

// plainResult reads the top-level fields from a value with no serializer at its root.
func plainResult(v any) map[string]any {
	out := map[string]any{}
	if x, ok := topLevelField(v, "isError"); ok {
		if b, ok := jsonBool(x); ok {
			out["isError"] = b
		}
	}
	for _, key := range []string{"requestState", "nextCursor"} {
		if x, ok := topLevelField(v, key); ok {
			if s, ok := textOf(x); ok {
				out[key] = s
			}
		}
	}
	if x, ok := topLevelField(v, "inputRequests"); ok {
		if rv := reflect.ValueOf(x); rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String {
			requests := map[string]any{}
			for it := rv.MapRange(); it.Next(); {
				entry := map[string]any{}
				if method, ok := topLevelField(it.Value().Interface(), "method"); ok {
					entry["method"] = method
				}
				requests[it.Key().String()] = entry
			}
			out["inputRequests"] = requests
		}
	}
	if meta, ok := topLevelField(v, "_meta"); ok {
		if status, ok := topLevelField(meta, "com.rudderstack/http_status"); ok {
			if f, ok := numberOf(status); ok {
				out["_meta"] = map[string]any{"com.rudderstack/http_status": f}
			}
		}
	}
	if sc, ok := topLevelField(v, "structuredContent"); ok && !isNilValue(sc) {
		picked := map[string]any{}
		for _, key := range []string{"status", "statusCode"} {
			if x, ok := topLevelField(sc, key); ok {
				if f, ok := numberOf(x); ok {
					picked[key] = f
				}
			}
		}
		for _, key := range []string{"nextCursor", "next_cursor"} {
			if x, ok := topLevelField(sc, key); ok {
				if s, ok := textOf(x); ok {
					picked[key] = s
				}
			}
		}
		out["structuredContent"] = picked
	}
	if c, ok := topLevelField(v, "content"); ok {
		if rv := reflect.ValueOf(c); rv.Kind() == reflect.Slice {
			blocks := []any{}
			seen := map[string]bool{}
			for i := 0; i < rv.Len(); i++ {
				name, ok := blockType(rv.Index(i).Interface())
				if ok && !seen[name] {
					seen[name] = true
					blocks = append(blocks, map[string]any{"type": name})
				}
			}
			out["content"] = blocks
		}
	}
	return out
}

// blockType is a content block's type: from its fields, or, for a block with its own serializer,
// from the bytes of that one block.
func blockType(block any) (string, bool) {
	if _, serializer := customTarget(reflect.ValueOf(block)); serializer {
		raw, err := compactJSON(block)
		if err != nil {
			return "", false
		}
		scanned := scanResult(raw)
		name, ok := scanned["type"].(string)
		return name, ok
	}
	t, ok := topLevelField(block, "type")
	if !ok {
		return "", false
	}
	return textOf(t)
}

func textOf(v any) (string, bool) {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return "", false
		}
		rv = rv.Elem()
	}
	if rv.IsValid() && rv.Kind() == reflect.String {
		return rv.String(), true
	}
	return "", false
}

func isNilValue(v any) bool {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return true
		}
		rv = rv.Elem()
	}
	return !rv.IsValid()
}

// scanResult reads the same top-level fields from a JSON document in one streaming pass: only
// those fields are decoded, the rest is skipped.
func scanResult(raw []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	out := map[string]any{}
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return out
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return out
		}
		name, _ := key.(string)
		switch name {
		case "isError", "requestState", "nextCursor":
			var x any
			if decodeFailed(dec.Decode(&x)) {
				return out
			}
			out[name] = x
		case "inputRequests":
			var requests map[string]struct {
				Method any `json:"method"`
			}
			if decodeFailed(dec.Decode(&requests)) {
				return out
			}
			if requests != nil {
				picked := map[string]any{}
				for k, r := range requests {
					picked[k] = map[string]any{"method": r.Method}
				}
				out[name] = picked
			}
		case "_meta":
			var meta struct {
				Status any `json:"com.rudderstack/http_status"`
			}
			if decodeFailed(dec.Decode(&meta)) {
				return out
			}
			if meta.Status != nil {
				out[name] = map[string]any{"com.rudderstack/http_status": meta.Status}
			}
		case "structuredContent":
			var sc *struct {
				Status     any `json:"status"`
				StatusCode any `json:"statusCode"`
				NextCursor any `json:"nextCursor"`
				NextSnake  any `json:"next_cursor"`
			}
			if err := dec.Decode(&sc); err != nil {
				if decodeFailed(err) {
					return out
				}
				// Not an object (the value was read): present, with nothing to read from it.
				out[name] = []any{}
				continue
			}
			if sc != nil {
				picked := map[string]any{}
				for k, x := range map[string]any{"status": sc.Status, "statusCode": sc.StatusCode, "nextCursor": sc.NextCursor, "next_cursor": sc.NextSnake} {
					if x != nil {
						picked[k] = x
					}
				}
				out[name] = picked
			}
		case "content":
			var blocks []struct {
				Type any `json:"type"`
			}
			if decodeFailed(dec.Decode(&blocks)) {
				return out
			}
			list, seen := []any{}, map[string]bool{}
			for _, b := range blocks {
				if s, ok := b.Type.(string); ok && !seen[s] {
					seen[s] = true
					list = append(list, map[string]any{"type": s})
				}
			}
			out[name] = list
		default:
			if skipValue(dec) != nil {
				return out
			}
		}
	}
	return out
}

// decodeFailed is whether a Decode left the stream unusable. A value of another type than the
// field's is still read in full, so scanning goes on, as a decode into generic values would.
func decodeFailed(err error) bool {
	var typeErr *json.UnmarshalTypeError
	return err != nil && !errors.As(err, &typeErr)
}

// skipValue reads one JSON value without keeping it.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("unexpected end")
			}
			return err
		}
		if d, ok := t.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// topLevelField is the value of a map entry or struct field encoding/json would write as key.
func topLevelField(source any, key string) (any, bool) {
	rv := reflect.ValueOf(source)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil, false
	}
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		if v := rv.MapIndex(reflect.ValueOf(key).Convert(rv.Type().Key())); v.IsValid() && v.CanInterface() {
			return v.Interface(), true
		}
	case reflect.Struct:
		for _, f := range cachedJSONFields(rv.Type()) {
			if f.name != key {
				continue
			}
			if v, ok := jsonFieldValue(rv, f); ok && v.CanInterface() {
				return v.Interface(), true
			}
		}
	}
	return nil, false
}

// numberOf is a JSON number as a float64, from any Go number type.
func numberOf(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch {
	case !rv.IsValid():
	case rv.CanFloat():
		return rv.Float(), true
	case rv.CanInt():
		return float64(rv.Int()), true
	case rv.CanUint():
		return float64(rv.Uint()), true
	}
	return 0, false
}

// jsonBool is a bool as encoding/json sees it: through pointers, and for a named bool type.
func jsonBool(v any) (bool, bool) {
	rv := reflect.ValueOf(v)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return false, false
		}
		rv = rv.Elem()
	}
	if rv.IsValid() && rv.Kind() == reflect.Bool {
		return rv.Bool(), true
	}
	return false, false
}
