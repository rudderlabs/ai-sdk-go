package rudderstackai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
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

// plainResult reads the top-level fields from a value with no serializer at its root, one field
// at a time, each as encoding/json writes it and decoded as main decoded the result: a small
// field is encoded and decoded whole, and a large one (_meta, structuredContent, content) is
// read for the few keys the event needs.
func plainResult(v any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"isError", "requestState", "nextCursor", "inputRequests"} {
		if d, ok := fieldAsMainDecoded(v, key); ok && d != nil {
			out[key] = d
		}
	}
	if meta, ok := topLevelField(v, "_meta"); ok {
		if picked, _ := pickFields(meta, "com.rudderstack/http_status"); len(picked) > 0 {
			out["_meta"] = picked
		}
	}
	if sc, ok := topLevelField(v, "structuredContent"); ok {
		switch picked, kind := pickFields(sc, "status", "statusCode", "nextCursor", "next_cursor"); kind {
		case jsonObject:
			out["structuredContent"] = picked
		case jsonOther:
			out["structuredContent"] = []any{} // present, with nothing to read from it
		}
	}
	if c, ok := topLevelField(v, "content"); ok {
		out["content"] = contentBlocks(c)
	}
	return out
}

type jsonKind int

const (
	jsonNull jsonKind = iota
	jsonObject
	jsonOther
)

// pickFields is the decoded values of keys in a value, and whether it is an object, null or
// something else as encoding/json writes it. A map or struct is read field by field; a value
// with its own serializer, or a raw document, is encoded once and scanned for the keys.
func pickFields(holder any, keys ...string) (map[string]any, jsonKind) {
	rv := reflect.ValueOf(holder)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if _, own := customTarget(rv); own || rv.Type() == rawMessageType {
			break
		}
		if rv.IsNil() {
			return nil, jsonNull
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil, jsonNull
	}
	if _, own := customTarget(rv); own || rv.Type() == rawMessageType {
		raw, err := compactJSON(rv.Interface())
		if err != nil {
			return nil, jsonNull
		}
		return scanKeys(raw, keys)
	}
	switch rv.Kind() {
	case reflect.Map, reflect.Struct:
		if rv.Kind() == reflect.Map && rv.IsNil() {
			return nil, jsonNull
		}
		picked := map[string]any{}
		for _, key := range keys {
			if d, ok := fieldAsMainDecoded(rv.Interface(), key); ok && d != nil {
				picked[key] = d
			}
		}
		return picked, jsonObject
	case reflect.Slice:
		if rv.IsNil() {
			return nil, jsonNull
		}
	}
	return nil, jsonOther
}

// contentBlocks is the distinct block types of a content value, in order, as main's decode of
// each block's "type" gave them. Each block is read for that key only.
func contentBlocks(content any) []any {
	blocks := []any{}
	rv := reflect.ValueOf(content)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if _, own := customTarget(rv); own || rv.Type() == rawMessageType {
			break
		}
		if rv.IsNil() {
			return blocks
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return blocks
	}
	seen := map[string]bool{}
	add := func(name any) {
		if s, ok := name.(string); ok && !seen[s] {
			seen[s] = true
			blocks = append(blocks, map[string]any{"type": s})
		}
	}
	if _, own := customTarget(rv); own || rv.Type() == rawMessageType {
		raw, err := compactJSON(rv.Interface())
		if err != nil {
			return blocks
		}
		var list []struct {
			Type any `json:"type"`
		}
		_ = json.Unmarshal(raw, &list) // blocks of another shape are skipped, as main skipped them
		for _, b := range list {
			add(b.Type)
		}
		return blocks
	}
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array || rv.Kind() == reflect.Slice && rv.IsNil() {
		return blocks
	}
	for i := 0; i < rv.Len(); i++ {
		picked, kind := pickFields(rv.Index(i).Interface(), "type")
		if kind == jsonObject {
			add(picked["type"])
		}
	}
	return blocks
}

// fieldAsMainDecoded is a struct field's or map entry's value as main decoded it, with what the parent's
// tags do to it: `,string` writes a scalar as a JSON string, omitempty and renames and embedded
// promotion come from the field selection itself.
func fieldAsMainDecoded(source any, key string) (any, bool) {
	rv := reflect.ValueOf(source)
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) && !rv.IsNil() {
		rv = rv.Elem()
	}
	if rv.IsValid() && rv.Kind() == reflect.Struct {
		for _, f := range cachedJSONFields(rv.Type()) {
			if f.name != key {
				continue
			}
			fv, ok := jsonFieldValue(rv, f)
			if !ok || !fv.CanInterface() {
				return nil, false
			}
			if _, own := customTarget(fv); f.quoted && !own && quotable(fv) {
				for fv.Kind() == reflect.Pointer && !fv.IsNil() {
					fv = fv.Elem()
				}
				if fv.Kind() == reflect.Pointer {
					return nil, true // a nil pointer is null, quoted or not
				}
				raw, err := compactJSON(fv.Interface())
				return string(raw), err == nil
			}
			return jsonOf(fv.Interface()), true
		}
		return nil, false
	}
	x, ok := topLevelField(source, key)
	if !ok {
		return nil, false
	}
	return jsonOf(x), true
}

// jsonOf is v as encoding/json writes it, decoded the way main decoded a result.
func jsonOf(v any) any {
	raw, err := compactJSON(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// scanKeys decodes the values of keys in a JSON document and skips the rest.
func scanKeys(raw []byte, keys []string) (map[string]any, jsonKind) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil || t == nil {
		return nil, jsonNull
	}
	if t != json.Delim('{') {
		return nil, jsonOther
	}
	picked := map[string]any{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			break
		}
		name, _ := key.(string)
		if !slices.Contains(keys, name) {
			if skipValue(dec) != nil {
				break
			}
			continue
		}
		var x any
		if decodeFailed(dec.Decode(&x)) {
			break
		}
		if x != nil {
			picked[name] = x
		}
	}
	return picked, jsonObject
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
