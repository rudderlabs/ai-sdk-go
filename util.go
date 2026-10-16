package rudderstackai

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

const (
	// maxMetaLength caps every _meta value, ID and name copied into an event.
	maxMetaLength = 256
	// maxLabelLength caps short labels: protocol_version, auth_method, country,
	// delivery_mode, failure_reason.
	maxLabelLength = 64
	// maxTextLength caps agent-written text (call_reason, capability_detail), in code points.
	maxTextLength = 200

	metaInteractionID      = "com.rudderstack/interaction_id"
	metaTraceID            = "com.rudderstack/trace_id"
	metaTraceparent        = "traceparent"
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
)

// capText cuts s to limit code points.
func capText(s string, limit int) string {
	if len(s) <= limit {
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

// str returns v when it is a non-empty string.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// opaque reads a _meta value as an opaque, length-capped string.
func opaque(v any) string { return capID(str(v)) }

// capID trims and caps an ID or name read off the wire or from Identify.
func capID(s string) string { return capText(strings.TrimSpace(s), maxMetaLength) }

// capLabel trims and caps a short label, such as a protocol version or a delivery mode.
func capLabel(s string) string { return capText(strings.TrimSpace(s), maxLabelLength) }

var traceparentRE = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(?:-|$)`)

// traceIDFromTraceparent returns the trace-id of a W3C traceparent, never the all-zero one.
func traceIDFromTraceparent(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	m := traceparentRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || m[1] == "ff" || strings.Trim(m[2], "0") == "" || strings.Trim(m[3], "0") == "" {
		return ""
	}
	return m[2]
}

// joinIDs reads interaction_id and trace_id from a request's _meta; nothing else is read.
func joinIDs(meta map[string]any) (interactionID, traceID string) {
	if meta == nil {
		return "", ""
	}
	interactionID = opaque(meta[metaInteractionID])
	traceID = traceIDFromTraceparent(meta[metaTraceparent])
	if traceID == "" {
		traceID = opaque(meta[metaTraceID])
	}
	return interactionID, traceID
}

func mintID() string { return uuid.NewString() }

// compactJSON is the JSON encoding of v without HTML escaping or a trailing newline.
func compactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// byteSize is the UTF-8 size of v's JSON.
func byteSize(v any) int {
	b, err := compactJSON(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// toJSONValue converts v to its generic JSON form (maps, slices, float64, string, bool, nil).
func toJSONValue(v any) any {
	b, err := compactJSON(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// argumentsHash hashes a tool name and its arguments; encoding/json sorts map keys, so equal
// arguments hash equally.
func argumentsHash(tool string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	b, _ := compactJSON(args)
	sum := sha256.Sum256(append([]byte(tool+"\x00"), b...))
	return hex.EncodeToString(sum[:])
}

// capabilityNames lists the keys of a declared capabilities object, sorted.
func capabilityNames(v any) []string {
	m, ok := toJSONValue(v).(map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var contentTypes = map[string]bool{"text": true, "image": true, "audio": true, "resource_link": true, "resource": true}

// resultStats returns the size, rough tokens, content types and pagination of a tools/call
// result. Never its content.
func resultStats(result map[string]any, size int) map[string]any {
	types := []string{}
	blocks, _ := result["content"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		t := str(block["type"])
		if contentTypes[t] && !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	structured, hasStructured := result["structuredContent"]
	if hasStructured && structured != nil {
		types = append(types, "structured")
	}
	sc, _ := structured.(map[string]any)
	paginated := str(result["nextCursor"]) != "" || str(sc["nextCursor"]) != "" || str(sc["next_cursor"]) != ""
	return map[string]any{
		"result_size_bytes":      size,
		"result_tokens_estimate": size / 4,
		"result_content_types":   types,
		"result_paginated":       paginated,
	}
}

func typeMatches(t any, v any) bool {
	switch tt := t.(type) {
	case []any:
		for _, x := range tt {
			if typeMatches(x, v) {
				return true
			}
		}
		return false
	case string:
		switch tt {
		case "string":
			_, ok := v.(string)
			return ok
		case "number":
			_, ok := v.(float64)
			return ok
		case "integer":
			f, ok := v.(float64)
			return ok && f == math.Trunc(f)
		case "boolean":
			_, ok := v.(bool)
			return ok
		case "array":
			_, ok := v.([]any)
			return ok
		case "object":
			_, ok := v.(map[string]any)
			return ok
		case "null":
			return v == nil
		}
	}
	return true
}

func jsonEqual(a, b any) bool {
	x, _ := compactJSON(a)
	y, _ := compactJSON(b)
	return bytes.Equal(x, y)
}

// invalidArguments names the supplied or required arguments that fail the tool's input schema,
// one level deep (presence, type, enum, const, additionalProperties: false). Never values.
func invalidArguments(schema map[string]any, args map[string]any) []string {
	if schema == nil {
		return nil
	}
	props, _ := schema["properties"].(map[string]any)
	var out []string
	add := func(k string) {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		if k, ok := r.(string); ok {
			if _, present := args[k]; !present {
				add(k)
			}
		}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := args[k]
		prop, ok := props[k].(map[string]any)
		if !ok {
			if ap, isBool := schema["additionalProperties"].(bool); isBool && !ap {
				add(k)
			}
			continue
		}
		if t, has := prop["type"]; has && !typeMatches(t, v) {
			add(k)
			continue
		}
		if enum, has := prop["enum"].([]any); has && !slices.ContainsFunc(enum, func(e any) bool { return jsonEqual(e, v) }) {
			add(k)
			continue
		}
		if c, has := prop["const"]; has && !jsonEqual(c, v) {
			add(k)
		}
	}
	return out
}

var templateExpr = regexp.MustCompile(`\{[^}]*\}`)

// templateMatcher matches an RFC 6570 URI template: every {...} expression matches one or more
// characters.
func templateMatcher(template string) *regexp.Regexp {
	parts := templateExpr.Split(template, -1)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".+?") + "$")
}

// errorTypeForCode maps a JSON-RPC or MCP error code to an error_type.
func errorTypeForCode(code int64) string {
	switch code {
	case -32700, -32600, -32601, -32602,
		-32020, // header mismatch
		-32021, // missing required client capability
		-32022: // unsupported protocol version
		return "invalid_request"
	case -32001:
		return "timeout"
	case -32000:
		return "connection_error"
	case -32042: // a URL elicitation, such as a sign-in, is required first
		return "auth_error"
	default:
		return "api_error"
	}
}

// errorType maps an error returned by a handler or the client to an error_type. An error that
// maps to nothing more specific than api_error follows the errors it wraps.
func errorType(err error) string {
	if err == nil {
		return ""
	}
	var wire *jsonrpc.Error
	var coded interface{ ErrorCode() int64 }
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &wire):
		return errorTypeForCode(wire.Code)
	case errors.As(err, &coded):
		return errorTypeForCode(coded.ErrorCode())
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET), errors.Is(err, net.ErrClosed),
		errors.Is(err, io.ErrUnexpectedEOF):
		return "connection_error"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	seen := 0
	for todo := []error{err}; len(todo) > 0 && seen < 32; seen++ {
		e := todo[0]
		todo = todo[1:]
		if kind := errorTypeOf(e); kind != "api_error" {
			return kind
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			if next := u.Unwrap(); next != nil {
				todo = append(todo, next)
			}
		case interface{ Unwrap() []error }:
			todo = append(todo, u.Unwrap()...)
		}
	}
	return "api_error"
}

// errorTypeOf maps one error by its HTTP status and type name, as the Node.js and Python SDKs do.
func errorTypeOf(err error) string {
	status := 0
	switch s := err.(type) {
	case interface{ StatusCode() int }:
		status = s.StatusCode()
	case interface{ HTTPStatusCode() int }:
		status = s.HTTPStatusCode()
	}
	name := strings.ToLower(reflect.TypeOf(err).String())
	has := func(parts ...string) bool {
		return slices.ContainsFunc(parts, func(p string) bool { return strings.Contains(name, p) })
	}
	switch {
	case status == 429 || has("ratelimit", "throttling"):
		return "rate_limit"
	case status == 408 || status == 504 || has("timeout"):
		return "timeout"
	case has("cancel", "abort"):
		return "cancelled"
	case status == 401 || status == 403 || has("authentication", "permissiondenied", "accessdenied"):
		return "auth_error"
	case status == 400 || status == 422 || has("badrequest", "unprocessable", "validationexception"):
		return "invalid_request"
	case has("connection"):
		return "connection_error"
	default:
		return "api_error"
	}
}

// closedList returns the non-empty, unique values plus "other".
func closedList(values []string) []string {
	out := []string{}
	for _, v := range values {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	if !slices.Contains(out, "other") {
		out = append(out, "other")
	}
	return out
}

// boundedMap forgets its least recently set entries past max. Not safe for concurrent use.
type boundedMap[V any] struct {
	max   int
	order *list.List
	items map[string]*list.Element
}

type boundedEntry[V any] struct {
	key   string
	value V
}

func newBoundedMap[V any](limit int) *boundedMap[V] {
	return &boundedMap[V]{max: limit, order: list.New(), items: map[string]*list.Element{}}
}

func (m *boundedMap[V]) get(k string) (V, bool) {
	if e, ok := m.items[k]; ok {
		return e.Value.(*boundedEntry[V]).value, true
	}
	var zero V
	return zero, false
}

func (m *boundedMap[V]) set(k string, v V) {
	if e, ok := m.items[k]; ok {
		m.order.Remove(e)
	}
	m.items[k] = m.order.PushBack(&boundedEntry[V]{key: k, value: v})
	for m.order.Len() > m.max {
		oldest := m.order.Front()
		m.order.Remove(oldest)
		delete(m.items, oldest.Value.(*boundedEntry[V]).key)
	}
}

func (m *boundedMap[V]) remove(k string) {
	if e, ok := m.items[k]; ok {
		m.order.Remove(e)
		delete(m.items, k)
	}
}

// each calls f for every entry, least recently set first, until f returns false.
func (m *boundedMap[V]) each(f func(k string, v V) bool) {
	for e := m.order.Front(); e != nil; e = e.Next() {
		be := e.Value.(*boundedEntry[V])
		if !f(be.key, be.value) {
			return
		}
	}
}

func (m *boundedMap[V]) size() int { return m.order.Len() }

func (m *boundedMap[V]) clear() {
	m.order.Init()
	m.items = map[string]*list.Element{}
}
