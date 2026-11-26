package rudderstackai

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Per-call content uses OpenTelemetry GenAI shapes for inputs, output text and tool calls.
// It requires both WithLLMCallEvents and content capture.

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
func maskValue(v any) (any, bool) {
	if cfg().emitSpans {
		v = stripBinary(v)
	}
	return maskPlainValue(v)
}

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
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			m, ok := maskPlainValue(x[k])
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
	case "blob", "image-data", "file-data":
		delete(part, "content")
	case "uri":
		uri, _ := part["uri"].(string)
		if len(uri) >= 5 && strings.EqualFold(uri[:5], "data:") {
			delete(part, "uri")
			part["type"] = "blob"
			delete(part, "content")
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

// maxWalkNodes is the values one captured value keeps, counted depth first at each place it is
// reached: a shared value counts every time, so a graph costs what its JSON would.
const maxWalkNodes = 100_000

type visit struct {
	ptr uintptr
	typ reflect.Type
	n   int
}

// walk is one traversal of a value: the containers on the current path (to drop a cycle) and the
// depth reached.
type walk struct {
	path           map[visit]bool
	depth          int
	omitted        bool
	legacy         bool
	resolve        bool
	preserve       bool
	serializers    bool
	usedSerializer bool
	incomplete     bool
	memo           map[visit]any
	nodes          int            // values this walk has counted; the root counts itself
	cuts           int            // times the budget stopped a loop
	cut            map[visit]bool // containers on the path when it did: never memoised
	dumped         map[visit]serializerOutput
	noBudget       bool // the resolve walk of a consumer that reads the whole value, as base did
	share          bool // one pass over a resolved tree: each container is copied once and reused, so no budget
	copies         map[copyKey]any
	fields         bool // the root map's values are separate captured values, each with its own budget
}

// serializerOutput is what an app serializer returned for a value the budget cut, so a later
// captured value of the report reads it whole without running the serializer again.
type copyKey struct {
	visit
	depth int
}

type serializerOutput struct {
	decoded any
	raw     []byte
}

// take counts one more value; false once the budget is spent, so the walk stops and leaves the
// rest out like values past the depth cap.
func (w *walk) take() bool {
	if w.share || w.noBudget {
		return true
	}
	if w.nodes >= maxWalkNodes-1 {
		w.cuts++
		if w.cut == nil {
			w.cut = map[visit]bool{}
		}
		for key := range w.path {
			w.cut[key] = true
		}
		return false
	}
	w.nodes++
	return true
}

// enter marks a pointer, map or slice as on the path; false when it is already there (a cycle) or
// the walk is too deep. Every true must be followed by leave.
func (w *walk) enter(rv reflect.Value) (visit, bool) {
	if w.depth >= maxWalkDepth {
		return visit{}, false
	}
	var key visit
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if rv.IsNil() || (rv.Kind() == reflect.Slice && rv.Len() == 0) {
			w.depth++
			return key, true
		}
		key = visit{rv.Pointer(), rv.Type(), 0}
		if rv.Kind() == reflect.Slice {
			key.n = rv.Len()
		}
		if w.path[key] {
			return key, false
		}
		w.path[key] = true
	}
	w.depth++
	return key, true
}

func (w *walk) leave(key visit) {
	w.depth--
	if key.ptr != 0 {
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
	if !cfg().emitSpans {
		out, _ := (&walk{path: map[visit]bool{}, legacy: true}).strip(v)
		return out
	}
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
		if m := dataURIPrefix.FindStringSubmatch(x); !w.resolve && m != nil {
			return "data:" + m[1], true
		}
		return x, true
	case nil, bool, int, int64, float64, json.Number:
		return v, true
	case []byte:
		if w.resolve {
			return append([]byte(nil), x...), true
		}
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
	textType       = reflect.TypeFor[encoding.TextMarshaler]()
	marshalerType  = reflect.TypeFor[json.Marshaler]()
)

// customTarget follows encoding/json's addressability and JSON-before-Text precedence.
func customTarget(rv reflect.Value) (reflect.Value, bool) {
	t := rv.Type()
	switch {
	case t.Kind() != reflect.Pointer && rv.CanAddr() && reflect.PointerTo(t).Implements(marshalerType):
		return rv.Addr(), true
	case t.Implements(marshalerType):
		return rv, true
	case t.Kind() != reflect.Pointer && rv.CanAddr() && reflect.PointerTo(t).Implements(textType):
		return rv.Addr(), true
	case t.Implements(textType):
		return rv, true
	}
	return reflect.Value{}, false
}

// captureReport owns only detached content and the size measured before byte stripping.
type captureSeed struct {
	original, captured any
	memo               map[visit]any
}

type captureReport struct {
	value any
	size  int
	sizes map[string]int
	memo  map[visit]any
	cut   bool // the budget left values out of value
}

// The recursion guard contains only identities while app code is on the current stack. It
// never caches an output or receiver, and concurrent goroutines never wait on one another.
var captureActive = struct {
	sync.Mutex
	callers map[uint64]map[visit]int
	passes  map[uint64]int
}{callers: map[uint64]map[visit]int{}, passes: map[uint64]int{}}

func captureRunning() bool {
	id := callbackCaller()
	captureActive.Lock()
	defer captureActive.Unlock()
	return captureActive.passes[id] > 0
}

func captureValue(v any, seeds ...captureSeed) captureReport {
	return captureResolved(v, nil, seeds...)
}

// enterCapture marks a capture pass on this goroutine, so an app serializer it runs is refused
// pause and end like any callback.
func enterCapture() (leave func()) {
	id := callbackCaller()
	captureActive.Lock()
	captureActive.passes[id]++
	captureActive.Unlock()
	return func() {
		captureActive.Lock()
		captureActive.passes[id]--
		if captureActive.passes[id] == 0 {
			delete(captureActive.passes, id)
		}
		captureActive.Unlock()
	}
}

// captureEncoded is base's encoding of a report value that stores no content, such as an MCP
// result without a declared rule: one serializer run, for classification and size only.
func captureEncoded(v any) (b []byte, err error) {
	defer enterCapture()()
	defer func() {
		if r := recover(); r != nil {
			b, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return compactJSON(v)
}

func resolveCapture(v any, fields bool, seeds []captureSeed) (any, *walk) {
	return resolveCaptureWith(v, fields, false, seeds)
}

// resolveCaptureWith is resolveCapture; noBudget leaves the walk unbudgeted for a consumer that
// reads the whole value (an MCP result's status). The walk is memoised, so it is linear in the
// distinct containers; only the content copies made from it are budgeted.
func resolveCaptureWith(v any, fields, noBudget bool, seeds []captureSeed) (any, *walk) {
	w := &walk{path: map[visit]bool{}, legacy: !cfg().emitSpans, resolve: true, serializers: true, memo: map[visit]any{}, fields: fields, noBudget: noBudget}
	for _, seed := range seeds {
		w.seed(reflect.ValueOf(seed.original), seed.captured, 0)
		if len(seed.memo) > 0 {
			w.usedSerializer = true
		}
		for key, value := range seed.memo {
			w.memo[key] = value
		}
	}
	resolved, _ := w.strip(v)
	return resolved, w
}

// captureProperties captures event properties or attributes in one pass. The map only holds
// them: each value is stripped on its own, so the map's own shape never drops them all.
func captureProperties(p map[string]any, seeds ...captureSeed) map[string]any {
	return captureProps(p, false, seeds)
}

// captureAttributes is captureProperties with a budget for each attribute.
func captureAttributes(p map[string]any) map[string]any { return captureProps(p, true, nil) }

func captureProps(p map[string]any, fields bool, seeds []captureSeed) map[string]any {
	out := make(map[string]any, len(p))
	if fields {
		return captureEach(p, out, seeds)
	}
	// Everything resolves together, scalars included: the properties are one captured value, so
	// one budget covers all of them, in key order (span attributes are each their own).
	resolved := p
	needsWalk := false
	for _, value := range p {
		switch value.(type) {
		case nil, string, bool, int, int64, float64:
		default:
			needsWalk = true
		}
	}
	if needsWalk {
		defer enterCapture()()
		r, _ := resolveCapture(p, fields, seeds)
		resolved, _ = r.(map[string]any)
	}
	var shared *walk
	if !fields {
		shared = &walk{path: map[visit]bool{}, legacy: !cfg().emitSpans, preserve: true}
	}
	for _, k := range slices.Sorted(maps.Keys(p)) {
		w := shared
		if w == nil {
			w = &walk{path: map[visit]bool{}, legacy: !cfg().emitSpans, preserve: true}
		} else if !w.take() {
			break
		}
		value, ok := resolved[k] // a value the capture left out still used its place in the budget
		if !ok {
			continue
		}
		if v, keep := w.strip(value); keep {
			out[k] = v
		}
	}
	return out
}

// captureContent is the detached snapshot of a report that needs no sizes and seeds no later
// capture, such as call content or span context.
func captureContent(v any) any { return captureContentWith(v, false) }

// captureFields is captureContent for a map whose values are separate captured values, each with
// its own budget.
func captureFields(v any) any { return captureContentWith(v, true) }

func captureContentWith(v any, fields bool) any {
	defer enterCapture()()
	resolved, _ := resolveCapture(v, fields, nil)
	out, _ := (&walk{path: map[visit]bool{}, legacy: !cfg().emitSpans, preserve: true, fields: fields}).strip(resolved)
	return out
}

// consume reads the resolved tree synchronously, for base wire-shaped callback inputs. The
// report stores only the stripped snapshot; neither this tree nor its bytes survive the pass.
func captureResolved(v any, consume func(any), seeds ...captureSeed) captureReport {
	return captureResolvedWith(v, false, consume, seeds)
}

// captureValueFields is captureValue for a map whose values are separate captured values.
func captureValueFields(v any) captureReport { return captureResolvedWith(v, true, nil, nil) }

func captureResolvedWith(v any, fields bool, consume func(any), seeds []captureSeed) captureReport {
	defer enterCapture()()
	// The resolve walk is whole: it is memoised, so linear in distinct containers, and sizes are
	// main's. Only the content copy made from it below is budgeted.
	resolved, w := resolveCaptureWith(v, fields, true, seeds)
	if consume != nil {
		consume(resolved)
	}
	// Each value is sized on its own: a serializer in one captured value doesn't change how
	// another is measured.
	in, z := newInspector(), newSizer()
	sizes := map[string]int{}
	size := fieldSize(v, resolved, fields, in, z)
	if m, ok := v.(map[string]any); ok && fields {
		rm, _ := resolved.(map[string]any)
		for k, value := range m {
			sizes[k] = fieldSize(value, rm[k], false, in, z)
		}
	}
	content := &walk{path: map[visit]bool{}, legacy: !cfg().emitSpans, preserve: true, fields: fields}
	out, _ := content.strip(resolved)
	// One pass copies every memoised container once and reuses it where it appears, so the work
	// is linear in distinct containers however much they share.
	memo := make(map[visit]any, len(w.memo))
	sw := &walk{path: map[visit]bool{}, legacy: !cfg().emitSpans, preserve: true, share: true, copies: map[copyKey]any{}}
	for key, value := range w.memo {
		memo[key], _ = sw.strip(value)
	}
	return captureReport{value: out, size: size, sizes: sizes, memo: memo, cut: content.cuts > 0}
}

// resolvedSize is base's encoded size: serializer output counts as the bytes encoding/json
// wrote for it, HTML escapes included.
func resolvedSize(original, resolved any, z *sizer) int {
	if _, ok := original.(string); ok {
		return captureSize(resolved)
	}
	// What compactJSON gives main for the whole resolved tree, counting a shared value at each
	// place without expanding it, and a serializer's output as the bytes it wrote.
	if n, err := z.size(reflect.ValueOf(resolved)); err == nil {
		return n
	}
	return -1
}

// serialized is a serializer's output inside a resolved tree: its decoded value, and the bytes
// encoding/json wrote for it, which only sizes read. Stripping unwraps it.
type serialized struct {
	value any
	raw   []byte
}

func (s serialized) MarshalJSON() ([]byte, error) { return s.raw, nil }

var serializedType = reflect.TypeFor[serialized]()

// exactSize is len(compactJSON(v)) without expanding shared references.
func exactSize(v any) (int, error) { return newSizer().size(reflect.ValueOf(v)) }

func captureSize(v any) int {
	if text, ok := v.(string); ok {
		return len(text)
	}
	if b, err := compactJSON(v); err == nil {
		return len(b)
	}
	return -1
}

// Seeds pair base-retained rule inputs with their report snapshots. They are inspected only
// during this pass, and are never added to retained capture state.
func (w *walk) seed(rv reflect.Value, captured any, depth int) {
	if !rv.IsValid() || depth >= maxWalkDepth || nonDataValue(rv) {
		return
	}
	if target, custom := customTarget(rv); custom {
		w.usedSerializer = true
		if key, stable := serializerMemoIdentity(rv, target); stable {
			if _, seen := w.memo[key]; !seen {
				w.memo[key] = captured
			}
		}
		return
	}
	if rv.Kind() == reflect.Interface {
		if !rv.IsNil() {
			w.seed(rv.Elem(), captured, depth+1)
		}
		return
	}
	if key, stable := serializerIdentity(rv); stable {
		if _, seen := w.memo[key]; seen {
			return
		}
		w.memo[key] = captured
	}
	switch rv.Kind() {
	case reflect.Pointer:
		if !rv.IsNil() {
			w.seed(rv.Elem(), captured, depth+1)
		}
	case reflect.Map:
		if m, ok := captured.(map[string]any); ok {
			for it := rv.MapRange(); it.Next(); {
				// Only keys named without app code; a missing entry is never seeded as null.
				if name, ok := jsonKeyName(it.Key()); ok {
					if child, present := m[name]; present {
						w.seed(it.Value(), child, depth+1)
					}
				}
			}
		}
	case reflect.Struct:
		if m, ok := captured.(map[string]any); ok {
			for _, f := range cachedJSONFields(rv.Type()) {
				if fv, ok := jsonFieldValue(rv, f); ok {
					w.seed(fv, m[f.name], depth+1)
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if list, ok := captured.([]any); ok {
			for n := 0; n < min(rv.Len(), len(list)); n++ {
				w.seed(rv.Index(n), list[n], depth+1)
			}
		}
	}
}

func serializerIdentity(rv reflect.Value) (visit, bool) {
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if rv.IsNil() {
			return visit{}, false
		}
		key := visit{ptr: rv.Pointer(), typ: rv.Type()}
		if rv.Kind() == reflect.Slice {
			key.n = rv.Len()
		}
		return key, true
	}
	return visit{}, false
}

type staticTextProbe struct{}

func (staticTextProbe) MarshalJSON() ([]byte, error) { return []byte(`"json"`), nil }
func (staticTextProbe) MarshalText() ([]byte, error) { return []byte("text"), nil }

// staticText is whether the running encoding/json encodes a field typed encoding.TextMarshaler
// with MarshalText even when its value has MarshalJSON: Go 1.26 does, Go 1.27's encoder uses the
// value's MarshalJSON.
var staticText = sync.OnceValue(func() bool {
	b, _ := json.Marshal(struct{ F encoding.TextMarshaler }{staticTextProbe{}})
	return string(b) == `{"F":"text"}`
})

// textByStaticType is whether encoding/json encodes target, an interface-typed value whose type
// has no MarshalJSON, with MarshalText although its value may have one.
func textByStaticType(target reflect.Value) bool {
	return target.Kind() == reflect.Interface && !target.Type().Implements(marshalerType) && staticText()
}

// jsonKeyName is encoding/json's name for a map key that needs no MarshalText call.
func jsonKeyName(k reflect.Value) (string, bool) {
	switch {
	case k.Kind() == reflect.String:
		return k.String(), true
	case k.Type().Implements(textType):
		return "", false
	case k.CanInt():
		return strconv.FormatInt(k.Int(), 10), true
	case k.CanUint():
		return strconv.FormatUint(k.Uint(), 10), true
	}
	return "", false
}

// Addressable pointer receivers have identity; promoted by-value methods do not.
func serializerMemoIdentity(rv, target reflect.Value) (visit, bool) {
	if target.Kind() == reflect.Interface {
		// A pointer behind an interface-typed field keeps its identity. A text selection on a
		// value that also has MarshalJSON gets its own key, so the two outputs never mix.
		if target.IsNil() {
			return visit{}, false
		}
		elem := target.Elem()
		key, stable := serializerIdentity(elem)
		if stable && textByStaticType(target) && elem.Type().Implements(marshalerType) {
			key.n = -1
		}
		return key, stable
	}
	key, stable := serializerIdentity(target)
	if rv.Kind() != reflect.Pointer {
		valueReceiver := rv.Type().Implements(marshalerType)
		if !target.Type().Implements(marshalerType) {
			valueReceiver = rv.Type().Implements(textType)
		}
		if valueReceiver && rv.Kind() != reflect.Map && rv.Kind() != reflect.Slice {
			stable = false
		}
	}
	return key, stable
}

// decodeSerializer runs the method encoding/json selects for target and returns its decoded
// output and the bytes encoding/json writes for it. Failure is null, including panic or invalid
// JSON; never walk the receiver's fields.
func decodeSerializer(target reflect.Value) (decoded any, raw []byte) {
	defer func() {
		if recover() != nil {
			decoded, raw = nil, nil
		}
	}()
	var err error
	if textByStaticType(target) {
		var text []byte
		if text, err = target.Interface().(encoding.TextMarshaler).MarshalText(); err == nil {
			raw, err = compactJSON(string(text))
		}
	} else {
		raw, err = compactJSON(target.Interface())
	}
	if err != nil {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&decoded) != nil {
		return nil, nil
	}
	return decoded, raw
}

func (w *walk) custom(rv reflect.Value) (any, bool) {
	target, ok := customTarget(rv)
	if !ok {
		return nil, false
	}
	w.usedSerializer = true
	if target.Kind() == reflect.Pointer && target.IsNil() {
		return nil, true
	}
	key, stable := serializerMemoIdentity(rv, target)
	if stable {
		if value, seen := w.memo[key]; seen {
			return value, true
		}
		if d, seen := w.dumped[key]; seen {
			return w.finishCustom(key, stable, d.decoded, d.raw), true
		}
		w.memo[key] = nil
	}
	id := callbackCaller()
	runningKey := key
	if !stable {
		runningKey = visit{typ: target.Type()}
	}
	captureActive.Lock()
	running := captureActive.callers[id]
	if running == nil {
		running = map[visit]int{}
		captureActive.callers[id] = running
	}
	if running[runningKey] > 0 {
		captureActive.Unlock()
		return nil, true
	}
	running[runningKey]++
	captureActive.Unlock()
	defer func() {
		captureActive.Lock()
		delete(running, runningKey)
		if len(running) == 0 {
			delete(captureActive.callers, id)
		}
		captureActive.Unlock()
	}()
	decoded, raw := decodeSerializer(target)
	return w.finishCustom(key, stable, decoded, raw), true
}

// finishCustom captures a serializer's output. A capture the budget cut is not memoised: another
// captured value of the report reads the output again, whole, within its own budget.
func (w *walk) finishCustom(key visit, stable bool, decoded any, raw []byte) any {
	before := w.cuts
	out, _ := w.strip(decoded)
	if w.resolve && raw != nil {
		out = serialized{value: out, raw: raw}
	}
	if stable {
		if w.cuts != before {
			delete(w.memo, key)
			if w.dumped == nil {
				w.dumped = map[visit]serializerOutput{}
			}
			w.dumped[key] = serializerOutput{decoded, raw}
		} else {
			w.memo[key] = out
		}
	}
	return out
}

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
	if nonDataValue(rv) {
		return nil, false // before any MarshalJSON or MarshalText its type has
	}
	if w.serializers {
		if key, stable := serializerIdentity(rv); stable {
			if value, seen := w.memo[key]; seen {
				return value, true
			}
		}
	}
	t := rv.Type()
	if t == serializedType {
		return w.strip(rv.Interface().(serialized).value)
	}
	if w.serializers {
		if t == reflect.TypeFor[time.Time]() || t == reflect.TypeFor[*time.Time]() {
			if out, custom := w.custom(rv); custom {
				return out, true
			}
		}
	}
	if t == reflect.TypeFor[time.Time]() {
		if w.legacy {
			return jsonSafe(rv.Interface())
		}
		return rv.Interface().(time.Time).UTC().Format("2006-01-02T15:04:05.000Z"), true
	}
	if t == rawMessageType {
		var decoded any
		if json.Unmarshal(rv.Bytes(), &decoded) != nil {
			return nil, false
		}
		return w.strip(decoded)
	}
	if w.serializers {
		if out, custom := w.custom(rv); custom {
			return out, true
		}
	}
	key, ok := w.enter(rv)
	if !ok {
		w.omitted = true
		w.incomplete = true
		return nil, false
	}
	defer w.leave(key)
	if w.share && key.ptr != 0 {
		// A copy depends on how much depth is left, so each (container, depth) is copied once.
		at := copyKey{key, w.depth}
		if c, ok := w.copies[at]; ok {
			return c, true
		}
		defer func() {
			if keep {
				w.copies[at] = out
			}
		}()
	}
	if w.serializers && key.ptr != 0 {
		defer func() {
			if keep && !w.cut[key] {
				w.memo[key] = out
			}
		}()
	}
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
		if w.resolve || w.preserve {
			return rv.Interface(), true
		}
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if w.resolve || w.preserve {
			return rv.Interface(), true
		}
		return rv.Uint(), true
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if (w.resolve || w.preserve) && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return rv.Interface(), true
		}
		return f, !w.legacy || (!math.IsNaN(f) && !math.IsInf(f, 0))
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			if w.resolve {
				b := make([]byte, rv.Len())
				reflect.Copy(reflect.ValueOf(b), rv)
				return b, true
			}
			return nil, false
		}
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil, true
		}
		list := make([]any, 0, min(rv.Len(), maxWalkNodes-w.nodes))
		for n := 0; n < rv.Len(); n++ {
			if !w.take() {
				break
			}
			item := rv.Index(n)
			// Binary items are stripped; other unrepresentable values keep their positions.
			if item.Kind() == reflect.Interface && !item.IsNil() {
				item = item.Elem()
			}
			if !w.resolve && (item.Kind() == reflect.Slice || item.Kind() == reflect.Array) && item.Type().Elem().Kind() == reflect.Uint8 {
				continue
			}
			e, ok := w.typed(rv.Index(n))
			if !ok && !w.omitted && !w.legacy {
				list = append(list, nil)
			} else if ok && !w.isBytes(e) {
				list = append(list, e)
			}
		}
		return list, true
	case reflect.Map:
		if w.legacy && !legacyValidMapKey(t.Key()) {
			return nil, false
		}
		if rv.IsNil() {
			return nil, true
		}
		m := make(map[string]any, rv.Len())
		// Resolve key names before visiting values, as encoding/json does.
		type entry struct {
			name  string
			value reflect.Value
		}
		entries := make([]entry, 0, rv.Len())
		dropped := 0
		for it := rv.MapRange(); it.Next(); {
			name, ok := mapKey(it.Key())
			if w.serializers && legacyValidMapKey(it.Key().Type()) {
				if it.Key().Kind() != reflect.String && it.Key().Type().Implements(textType) {
					w.usedSerializer = true
				}
				name, ok = legacyMapKey(it.Key())
			}
			if !ok {
				if w.legacy {
					return nil, false
				}
				dropped++
				continue
			}
			entries = append(entries, entry{name, it.Value()})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		// An entry whose key is left out still counts; with no place in the order they count first.
		for ; dropped > 0 && (!w.fields || w.depth != 1); dropped-- {
			if !w.take() {
				break
			}
		}
		cutsBefore := w.cuts
		media := false // as stripMap reads the keys of the original map
		for _, e := range entries {
			v := e.value
			for v.Kind() == reflect.Interface && !v.IsNil() {
				v = v.Elem()
			}
			media = media || mediaKeys[e.name] || (e.name == "type" && v.Kind() == reflect.String && (mediaTypes[v.String()] || v.String() == "Buffer"))
		}
		for _, entry := range entries {
			if w.fields && w.depth == 1 {
				w.nodes = 0 // the value is its own root
				clear(w.cut)
			} else if !w.take() {
				break
			}
			if e, ok := w.typed(entry.value); ok && !w.isBytes(e) {
				m[entry.name] = e
			}
		}
		if w.cuts != cutsBefore && media {
			// The marker may be among the values the budget left out, so the payload goes too.
			for k := range payloadKeys {
				delete(m, k)
			}
		}
		if !w.resolve && !w.legacy && isBytes(m) {
			w.omitted = true
			return nil, false
		}
		if !w.resolve && !w.legacy {
			m = inlineFree(m)
		}
		if w.resolve {
			return m, true
		}
		return stripMap(m), true
	case reflect.Struct:
		m := map[string]any{}
		cutsBefore := w.cuts
		w.structFields(rv, m)
		if w.cuts != cutsBefore && structMedia(rv) {
			for k := range payloadKeys {
				delete(m, k)
			}
		}
		if !w.resolve && !w.legacy && isBytes(m) {
			w.omitted = true
			return nil, false
		}
		if !w.resolve && !w.legacy {
			m = inlineFree(m)
		}
		if w.resolve {
			return m, true
		}
		return stripMap(m), true
	}
	// Channels, funcs, complex numbers, unsafe pointers: json cannot encode them either.
	return nil, false
}

// structFields follows encoding/json selection, promotion, conflicts and omission rules.
func (w *walk) structFields(rv reflect.Value, m map[string]any) {
	for _, f := range cachedJSONFields(rv.Type()) {
		fv, ok := jsonFieldValue(rv, f)
		if !ok {
			continue
		}
		if !w.take() {
			break
		}
		e, ok := w.typed(fv)
		if !ok || w.isBytes(e) {
			continue
		}
		// encoding/json ignores ,string on fields with custom JSON/Text encoders.
		_, custom := customTarget(fv)
		if f.quoted && !custom && e != nil {
			if b, err := json.Marshal(e); err == nil {
				e = string(b)
			}
		}
		m[f.name] = e
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
	// Resolve the whole report in one pass, before masking or projection.
	rawCalls := make([]GenAIToolCall, len(outputToolCalls))
	copy(rawCalls, outputToolCalls)
	for n := range rawCalls {
		original := rawCalls[n].Arguments
		rawCalls[n].Arguments = parseToolArguments(original)
		if original != nil && rawCalls[n].Arguments == nil {
			rawCalls[n].Arguments = json.RawMessage("null")
		}
	}
	resolved, _ := captureFields(map[string]any{"messages": inputMessages, "calls": rawCalls}).(map[string]any)
	inputMessages = nil
	if messages, ok := resolved["messages"].([]any); ok {
		for _, value := range messages {
			m, _ := value.(map[string]any)
			role, _ := m["role"].(string)
			message := GenAIMessage{Role: role}
			if parts, ok := m["parts"].([]any); ok {
				for _, value := range parts {
					if p, ok := value.(map[string]any); ok {
						message.Parts = append(message.Parts, GenAIPart(p))
					}
				}
			}
			inputMessages = append(inputMessages, message)
		}
	}
	outputToolCalls = nil
	if calls, ok := resolved["calls"].([]any); ok {
		for _, value := range calls {
			m, _ := value.(map[string]any)
			name, _ := m["name"].(string)
			args, present := m["arguments"]
			if present && args == nil {
				args = json.RawMessage("null")
			}
			outputToolCalls = append(outputToolCalls, GenAIToolCall{Name: name, Arguments: args})
		}
	}
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
			call["arguments"] = stripBinary(t.Arguments)
		}
		calls = append(calls, call)
	}
	if len(calls) > 0 {
		hit := false
		keep := true
		for _, value := range calls {
			call := value.(map[string]any)
			// Tool-call fields follow GenAIToolCall's order: name, then arguments.
			masked, ok := maskValue(capStrings([]any{call["name"], call["arguments"]}, &hit))
			if !ok {
				keep = false
				break
			}
			fields := masked.([]any)
			call["name"] = fields[0]
			if _, present := call["arguments"]; present {
				call["arguments"] = fields[1]
			}
		}
		if keep {
			content.outputToolCalls = calls
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

// Legacy product normalization remains exactly as before emitSpans was introduced.
func (w *walk) isBytes(v any) bool {
	if w.resolve {
		return false
	}
	if isBytes(v) {
		return true
	}
	if !w.legacy {
		return false
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return false
	}
	for k, value := range m {
		if k == "" || !isByte(value) {
			return false
		}
		for _, c := range k {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}
func legacyValidMapKey(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return t.Implements(textType)
}
func legacyMapKey(k reflect.Value) (string, bool) {
	if k.Kind() == reflect.String {
		return k.String(), true
	}
	if k.Type().Implements(textType) {
		if k.Kind() == reflect.Pointer && k.IsNil() {
			return "", false
		}
		value, err := k.Interface().(encoding.TextMarshaler).MarshalText()
		return string(value), err == nil
	}
	switch k.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(k.Uint(), 10), true
	}
	return "", false
}

// structMedia is whether a struct's JSON fields mark it as media, as stripMap reads a map's keys.
func structMedia(rv reflect.Value) bool {
	for _, f := range cachedJSONFields(rv.Type()) {
		fv, ok := jsonFieldValue(rv, f)
		if !ok {
			continue
		}
		for fv.Kind() == reflect.Interface || fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				break
			}
			fv = fv.Elem()
		}
		if mediaKeys[f.name] || (f.name == "type" && fv.Kind() == reflect.String && (mediaTypes[fv.String()] || fv.String() == "Buffer")) {
			return true
		}
	}
	return false
}

// captureEach captures span attributes, each with a budget of its own: values that run no app
// code are stripped as they are, and the rest resolve in one pass.
func captureEach(p, out map[string]any, seeds []captureSeed) map[string]any {
	strip := func(m map[string]any) {
		for k, value := range m {
			if v, keep := (&walk{path: map[visit]bool{}, legacy: !cfg().emitSpans, preserve: true}).strip(value); keep {
				out[k] = v
			}
		}
	}
	var typed map[string]any
	plain := make(map[string]any, len(p))
	for k, value := range p {
		switch value.(type) {
		case nil, string, bool, int, int64, float64:
			plain[k] = value
		default:
			if typed == nil {
				typed = map[string]any{}
			}
			typed[k] = value
		}
	}
	strip(plain)
	if typed != nil {
		defer enterCapture()()
		resolved, _ := resolveCapture(typed, true, seeds)
		m, _ := resolved.(map[string]any)
		strip(m)
	}
	return out
}
