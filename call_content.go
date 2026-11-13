package rudderstackai

import (
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Per-call content of ai_llm_response_received (SDK-6798): input messages, output text and the
// tool calls the model requested, in the OpenTelemetry GenAI shapes. Opt-in: WithLLMCallEvents
// and content capture together.

// GenAIPart is one part of a [GenAIMessage]: {"type": "text", "content": "..."}, a "tool_call"
// (id, name, arguments), a "tool_call_response" (id, response), a "uri" (modality, uri,
// mime_type), a "blob" (modality, mime_type; never the bytes) or any other type name.
type GenAIPart map[string]any

// GenAIMessage is one message in the OpenTelemetry GenAI gen_ai.input.messages shape.
type GenAIMessage struct {
	// Role is system, user, assistant or tool.
	Role  string      `json:"role"`
	Parts []GenAIPart `json:"parts"`
}

// GenAIToolCall is a tool call the model requested. Arguments is any JSON value; a string that
// is valid JSON is parsed. Nil sends no arguments.
type GenAIToolCall struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments,omitempty"`
}

// callContent is masked per-call content, ready for the event. Values are generic JSON.
type callContent struct {
	inputMessages []any
	// inputTruncated: strings were shortened before masking; fitProperties adds more.
	inputTruncated  bool
	outputText      string
	hasOutputText   bool
	outputToolCalls []any
}

// parseToolArguments parses a JSON-encoded string; anything else is returned as is.
func parseToolArguments(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	var out any
	if json.Unmarshal([]byte(s), &out) != nil {
		return v
	}
	return out
}

// cutCodePoints cuts s to at most limit code points.
func cutCodePoints(s string, limit int) string {
	if len(s) <= limit { // a string of max bytes has at most limit code points
		return s
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i]
		}
		n++
	}
	return s
}

// capStrings shortens every string longer than the masking cap, so masking never withholds a
// long message; hit reports a cut.
func capStrings(v any, hit *bool) any {
	switch x := v.(type) {
	case string:
		c := cutCodePoints(x, maxMaskedCodePoints)
		if len(c) != len(x) {
			*hit = true
		}
		return c
	case []any:
		out := make([]any, len(x))
		for n, e := range x {
			out[n] = capStrings(e, hit)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = capStrings(e, hit)
		}
		return out
	}
	return v
}

// maskValue sends every nested string through the redaction path (built-ins, then the hook). A
// withheld string drops the whole value: ok is false.
func maskValue(v any) (any, bool) { return maskPlainValue(stripBinary(v)) }

func maskPlainValue(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		return redactText(x)
	case []any:
		out := make([]any, len(x))
		for n, e := range x {
			m, ok := maskPlainValue(e)
			if !ok {
				return nil, false
			}
			out[n] = m
		}
		return out, true
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			m, ok := maskPlainValue(e)
			if !ok {
				return nil, false
			}
			out[k] = m
		}
		return out, true
	}
	return v, true
}

// inlineFree makes a part carry no inline bytes: a blob loses its content, and a data: URI
// (any case) becomes a blob with the part's mime_type, else the URI's media type.
func inlineFree(part map[string]any) map[string]any {
	switch part["type"] {
	case "blob":
		delete(part, "content")
	case "uri":
		uri, _ := part["uri"].(string)
		if len(uri) >= 5 && strings.EqualFold(uri[:5], "data:") {
			delete(part, "uri")
			part["type"] = "blob"
			if m, _ := part["mime_type"].(string); m == "" {
				delete(part, "mime_type")
				if m := dataURIMime(uri); m != "" {
					part["mime_type"] = m
				}
			}
		}
	}
	return part
}

// dataURIMime is the media type of a data: URI (data:image/png;base64,...), when it names one.
func dataURIMime(uri string) string {
	rest := uri[5:]
	if end := strings.IndexAny(rest, ";,"); end >= 0 {
		return rest[:end]
	}
	return rest
}

var (
	dataURIPrefix = regexp.MustCompile(`(?i)^data:([a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+)?(;[^,]*)?,`)
	payloadKeys   = map[string]bool{"data": true, "bytes": true, "base64": true, "b64_json": true}
	mediaKeys     = map[string]bool{"mime_type": true, "mimeType": true, "media_type": true, "mediaType": true, "source_type": true}
	mediaTypes    = map[string]bool{"image": true, "audio": true, "video": true, "document": true, "file": true, "media": true, "blob": true, "image-data": true, "file-data": true, "input_audio": true, "input_image": true, "input_file": true}
)

func isByte(v any) bool {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	case json.Number:
		f, _ = n.Float64()
	default:
		return false
	}
	return f >= 0 && f <= 255 && f == math.Trunc(f)
}

// isBytes: a native byte slice, a {"0": 1, "1": 2} byte object or a {"type": "Buffer", "data": [...]}.
func isBytes(v any) bool {
	switch x := v.(type) {
	case []byte:
		return true
	case map[string]any:
		if x["type"] == "Buffer" {
			if data, ok := x["data"].([]any); ok {
				for _, b := range data {
					if !isByte(b) {
						return false
					}
				}
				return true
			}
		}
	}
	return false
}

// maxWalkDepth is the nesting a walk follows before it drops the value: a backstop for cycles
// that cycle detection cannot see.
const maxWalkDepth = 64

type visit struct {
	ptr   uintptr
	typ   reflect.Type
	n     int
	depth bool
}

// walk is one traversal of a value: the containers on the current path (to drop a cycle) and the
// depth reached.
type walk struct {
	path    map[visit]bool
	depth   int
	omitted bool
}

// enter marks a pointer, map or slice as on the path; false when it is already there (a cycle) or
// the walk is too deep. Every true must be followed by leave.
func (w *walk) enter(rv reflect.Value) (visit, bool) {
	container := rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array || rv.Kind() == reflect.Struct
	if container && w.depth >= maxWalkDepth {
		return visit{}, false
	}
	var key visit
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if rv.IsNil() || (rv.Kind() == reflect.Slice && rv.Len() == 0) {
			if container {
				w.depth++
				key.depth = true
			}
			return key, true
		}
		key = visit{ptr: rv.Pointer(), typ: rv.Type()}
		if rv.Kind() == reflect.Slice {
			key.n = rv.Len()
		}
		if w.path[key] {
			return key, false
		}
		w.path[key] = true
	}
	if container {
		w.depth++
		key.depth = true
	}
	return key, true
}

func (w *walk) leave(key visit) {
	if key.depth {
		w.depth--
	}
	if key.ptr != 0 {
		key.depth = false
		delete(w.path, key)
	}
}

// stripBinary removes inline binary from any value: byte arrays are deleted (from their object or
// array), a data: URI string becomes data:<media type>, and an object that looks like media (a
// mime_type, mimeType, media_type, mediaType or source_type key, or a media type) loses its data,
// bytes, base64 and b64_json keys. Typed values (structs, map[string][]byte...) are walked to
// plain JSON values with byte slices dropped, so they are never encoded as base64. A cyclic or
// too deeply nested value is dropped (nil).
func stripBinary(v any) any {
	out, _ := normalizeSpanValue(v)
	return out
}

func normalizeSpanValue(v any) (any, bool) {
	out, keep := (&walk{path: map[visit]bool{}}).strip(v)
	return out, keep && !isBytes(out)
}

// strip returns the stripped value; keep is false when the value is bytes or cannot be sent.
func (w *walk) strip(v any) (out any, keep bool) {
	switch x := v.(type) {
	case string:
		if m := dataURIPrefix.FindStringSubmatch(x); m != nil {
			return "data:" + m[1], true
		}
		return x, true
	case nil, bool, int, int64, float64, json.Number:
		return v, true
	case []byte:
		return nil, false
	}
	// Plain containers and typed values alike go through the reflective walk; it never panics.
	return w.typed(reflect.ValueOf(v))
}

// stripMap applies the media-object rules to an already generic map.
func stripMap(x map[string]any) map[string]any {
	media := false
	for k := range x {
		if mediaKeys[k] {
			media = true
		}
	}
	if t, ok := x["type"].(string); ok && mediaTypes[t] {
		media = true
	}
	if media {
		for k := range payloadKeys {
			delete(x, k)
		}
	}
	return x
}

var (
	rawMessageType = reflect.TypeFor[json.RawMessage]()
	jsonNumberType = reflect.TypeFor[json.Number]()
)

// Primitive map keys use Node String() conventions; other keys are dropped.
func primitiveFloatKey(f float64) string {
	if f == 0 {
		return "0"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	format := byte('f')
	if math.Abs(f) >= 1e21 || math.Abs(f) < 1e-6 {
		format = 'e'
	}
	s := strconv.FormatFloat(f, format, -1, 64)
	if mantissa, exponent, ok := strings.Cut(s, "e"); ok {
		sign := ""
		if len(exponent) > 0 && (exponent[0] == '+' || exponent[0] == '-') {
			sign, exponent = exponent[:1], exponent[1:]
		}
		return mantissa + "e" + sign + strings.TrimLeft(exponent, "0")
	}
	return s
}

func mapKey(k reflect.Value) (string, bool) {
	for k.Kind() == reflect.Interface {
		if k.IsNil() {
			return "", false
		}
		k = k.Elem()
	}
	switch k.Kind() {
	case reflect.String:
		return k.String(), true
	case reflect.Bool:
		return strconv.FormatBool(k.Bool()), true
	case reflect.Float32, reflect.Float64:
		return primitiveFloatKey(k.Float()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(k.Uint(), 10), true
	}
	return "", false
}

// typed converts a Go value to plain JSON values. keep is false for a byte slice or array (never
// sent), a cycle, a value nested too deeply and a value json could not encode (channels, funcs,
// complex numbers, maps with unsupported key types). A json.RawMessage that is valid JSON is
// decoded; other containers are inspected without invoking serializers that could encode bytes.
// A panic drops the value.
func (w *walk) typed(rv reflect.Value) (out any, keep bool) {
	defer func() {
		if r := recover(); r != nil {
			out, keep = nil, false
		}
	}()
	if !rv.IsValid() {
		return nil, true
	}
	w.omitted = false
	t := rv.Type()
	if t == reflect.TypeFor[time.Time]() {
		return rv.Interface().(time.Time).UTC().Format("2006-01-02T15:04:05.000Z"), true
	}
	if t == rawMessageType {
		var decoded any
		if json.Unmarshal(rv.Bytes(), &decoded) != nil {
			return nil, false
		}
		return w.strip(decoded)
	}
	key, ok := w.enter(rv)
	if !ok {
		w.omitted = true
		return nil, false
	}
	defer w.leave(key)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil, true
		}
		return w.typed(rv.Elem())
	case reflect.String:
		if t == jsonNumberType {
			return json.Number(rv.String()), true
		}
		str, _ := w.strip(rv.String())
		return str, true
	case reflect.Bool:
		return rv.Bool(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint(), true
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		return f, true
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil, false
		}
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil, true
		}
		list := make([]any, 0, rv.Len())
		for n := 0; n < rv.Len(); n++ {
			item := rv.Index(n)
			// Binary items are stripped; other unrepresentable values keep their positions.
			if item.Kind() == reflect.Interface && !item.IsNil() {
				item = item.Elem()
			}
			if (item.Kind() == reflect.Slice || item.Kind() == reflect.Array) && item.Type().Elem().Kind() == reflect.Uint8 {
				continue
			}
			e, ok := w.typed(rv.Index(n))
			if !ok && !w.omitted {
				list = append(list, nil)
			} else if ok && !isBytes(e) {
				list = append(list, e)
			}
		}
		return list, true
	case reflect.Map:
		if rv.IsNil() {
			return nil, true
		}
		m := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			name, ok := mapKey(it.Key())
			if !ok {
				continue
			}
			if e, ok := w.typed(it.Value()); ok && !isBytes(e) {
				m[name] = e
			}
		}
		if isBytes(m) {
			w.omitted = true
			return nil, false
		}
		return stripMap(m), true
	case reflect.Struct:
		m := map[string]any{}
		w.structFields(rv, m)
		if isBytes(m) {
			w.omitted = true
			return nil, false
		}
		return stripMap(m), true
	}
	// Channels, funcs, complex numbers, unsafe pointers: json cannot encode them either.
	return nil, false
}

// structFields adds a struct's exported fields as encoding/json names them; an untagged embedded
// struct is flattened into the same object.
func (w *walk) structFields(rv reflect.Value, m map[string]any) {
	t := rv.Type()
	for n := 0; n < t.NumField(); n++ {
		f := t.Field(n)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" && opts == "" {
			continue
		}
		fv := rv.Field(n)
		if f.Anonymous && name == "" {
			ev := fv
			if ev.Kind() == reflect.Pointer {
				if ev.IsNil() {
					continue
				}
				ev = ev.Elem()
			}
			if ev.Kind() == reflect.Struct {
				key, ok := w.enter(fv)
				if ok {
					w.structFields(ev, m)
					w.leave(key)
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if strings.Contains(opts, "omitempty") && fv.IsZero() {
			continue
		}
		if e, ok := w.typed(fv); ok && !isBytes(e) {
			m[name] = e
		}
	}
}

// genAIMessagesValue is the messages as generic JSON: parts without a string type are skipped, and
// a value that cannot be encoded drops its part.
func genAIMessagesValue(messages []GenAIMessage) []any {
	var out []any
	for _, m := range messages {
		if m.Role == "" {
			continue
		}
		parts := []any{}
		for _, p := range m.Parts {
			if t, _ := p["type"].(string); t == "" {
				continue
			}
			if c, ok := stripBinary(map[string]any(p)).(map[string]any); ok {
				parts = append(parts, inlineFree(c))
			}
		}
		out = append(out, map[string]any{"role": m.Role, "parts": parts})
	}
	return out
}

// captureCallContent is the call's content as it will be sent: strings past the masking cap
// shortened, then every string masked like prompt_text. A field with a withheld string (hook
// failure) is left out whole. Nil when nothing is left.
func captureCallContent(inputMessages []GenAIMessage, outputText string, outputToolCalls []GenAIToolCall) *callContent {
	content := &callContent{}
	has := false
	if messages := genAIMessagesValue(inputMessages); len(messages) > 0 {
		hit := false
		if masked, ok := maskValue(capStrings(messages, &hit)); ok {
			content.inputMessages = masked.([]any)
			content.inputTruncated = hit
			has = true
		}
	}
	if outputText != "" {
		if text, ok := redactText(cutCodePoints(outputText, maxMaskedCodePoints)); ok {
			content.outputText, content.hasOutputText = text, true
			has = true
		}
	}
	var calls []any
	for _, t := range outputToolCalls {
		if t.Name == "" {
			continue
		}
		call := map[string]any{"name": t.Name}
		if t.Arguments != nil {
			call["arguments"] = stripBinary(parseToolArguments(t.Arguments))
		}
		calls = append(calls, call)
	}
	if len(calls) > 0 {
		hit := false
		if masked, ok := maskValue(capStrings(calls, &hit)); ok {
			content.outputToolCalls = masked.([]any)
			has = true
		}
	}
	if !has {
		return nil
	}
	return content
}

// leaf is a string a trim may shorten, and where it lives.
type leaf struct {
	holder any // map[string]any or []any
	key    any // string or int
	length int
	system bool
}

// contentKeys name the strings that count as content: a part's content, response and arguments,
// at any depth.
var contentKeys = map[string]bool{"content": true, "response": true, "arguments": true}

func collectLeaves(v any, out *[]leaf, system, inContent bool) {
	visit := func(holder, key, e any) {
		k, isKey := key.(string)
		content := inContent || (isKey && contentKeys[k])
		switch x := e.(type) {
		case string:
			if content {
				*out = append(*out, leaf{holder, key, utf8.RuneCountInString(x), system})
			}
		case []any, map[string]any:
			collectLeaves(x, out, system, content)
		}
	}
	switch x := v.(type) {
	case []any:
		for n, e := range x {
			visit(x, n, e)
		}
	case map[string]any:
		// Keys in the order Go encodes them, so document order is the emitted order.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			visit(x, k, x[k])
		}
	}
}

func deepCopyJSON(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for n, e := range x {
			out[n] = deepCopyJSON(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopyJSON(e)
		}
		return out
	}
	return v
}

// jsonBytes is the UTF-8 bytes one code point takes inside a compact JSON string (escapes counted).
func jsonBytes(r rune) int { return byteSize(string(r)) - 2 }

// trimInputMessages returns input messages within over bytes of the budget (the event's compact
// JSON, in UTF-8 bytes). First the oldest messages after the leading system ones go, one at a
// time, each saving its compact JSON bytes + 1; the last message always stays. Then the longest
// content string loses its tail, only as many code points as their encoded bytes need to cover
// over; ties go to the first in message order, and strings of system messages are shortened only
// when no other string has anything left to cut. ok is false when every string is empty and it
// still does not fit.
func trimInputMessages(messages []any, over int) ([]any, bool) {
	kept := append([]any(nil), messages...)
	first := len(kept)
	for n, m := range kept {
		if role, _ := m.(map[string]any)["role"].(string); role != "system" {
			first = n
			break
		}
	}
	for over > 0 && len(kept) > first+1 {
		dropped := kept[first]
		kept = append(kept[:first], kept[first+1:]...)
		over -= byteSize(dropped) + 1
	}
	if over <= 0 {
		return kept, true
	}
	copied := deepCopyJSON(kept).([]any)
	for over > 0 {
		var all []leaf
		for _, m := range copied {
			role, _ := m.(map[string]any)["role"].(string)
			collectLeaves(m, &all, role == "system", false)
		}
		longest := func(system bool) *leaf {
			var best *leaf
			for n := range all {
				if all[n].system == system && all[n].length > 0 && (best == nil || all[n].length > best.length) {
					best = &all[n]
				}
			}
			return best
		}
		target := longest(false)
		if target == nil {
			target = longest(true)
		}
		if target == nil {
			return nil, false
		}
		var text string
		switch h := target.holder.(type) {
		case map[string]any:
			text = h[target.key.(string)].(string)
		case []any:
			text = h[target.key.(int)].(string)
		}
		runes := []rune(text)
		for len(runes) > 0 && over > 0 {
			over -= jsonBytes(runes[len(runes)-1])
			runes = runes[:len(runes)-1]
		}
		switch h := target.holder.(type) {
		case map[string]any:
			h[target.key.(string)] = string(runes)
		case []any:
			h[target.key.(int)] = string(runes)
		}
	}
	return copied, true
}
