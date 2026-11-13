// Package spanmodel maps normalized spans without importing OpenTelemetry.
package spanmodel

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

type Context struct{ TraceID, SpanID, TraceState string }
type Event struct {
	Name                  string
	Time                  time.Time
	Attributes            map[string]any
	AttributeKeys         []string
	DroppedAttributeCount int
}
type Link struct {
	SpanContext           Context
	Attributes            map[string]any
	AttributeKeys         []string
	DroppedAttributeCount int
}
type Scope struct {
	Name, Version, SchemaURL string
	Attributes               map[string]any
	AttributeKeys            []string
}
type Span struct {
	Name                                           string
	Kind                                           int
	Context, Parent                                Context
	Start, End                                     time.Time
	StatusCode                                     int
	StatusMessage                                  string
	Attributes, ResourceAttributes                 map[string]any
	AttributeKeys, ResourceAttributeKeys           []string
	ResourceSchemaURL                              string
	Scope                                          Scope
	Events                                         []Event
	Links                                          []Link
	DroppedAttributes, DroppedEvents, DroppedLinks int
}

func kindName(k int) string {
	return []string{"unspecified", "internal", "server", "client", "producer", "consumer"}[min(max(k, 0), 5)]
}
func firstString(a map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := a[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case int:
			return strconv.Itoa(v)
		case int64:
			return strconv.FormatInt(v, 10)
		case float64:
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				return strconv.FormatFloat(v, 'f', -1, 64)
			}
		}
	}
	return ""
}
func spanOrResource(s Span, keys ...string) string {
	for _, k := range keys {
		if v := firstString(s.Attributes, k); v != "" {
			return v
		}
		if v := firstString(s.ResourceAttributes, k); v != "" {
			return v
		}
	}
	return ""
}
func genAISummary(a map[string]any) map[string]any {
	g := map[string]any{}
	for k, keys := range map[string][]string{"provider_name": {"gen_ai.provider.name", "gen_ai.system"}, "operation_name": {"gen_ai.operation.name"}, "request_model": {"gen_ai.request.model"}, "response_model": {"gen_ai.response.model"}, "conversation_id": {"gen_ai.conversation.id"}} {
		if v := firstString(a, keys...); v != "" {
			g[k] = v
		}
	}
	for k, keys := range map[string][]string{"input_tokens": {"gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens"}, "output_tokens": {"gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens"}} {
		for _, key := range keys {
			v := a[key]
			var f float64
			switch n := v.(type) {
			case int:
				f = float64(n)
			case int64:
				f = float64(n)
			case uint64:
				f = float64(n)
			case float64:
				f = n
			default:
				continue
			}
			if f >= 0 && f < float64(^uint(0)>>1) && math.Trunc(f) == f {
				g[k] = int(f)
				break
			}
		}
	}
	switch v := a["gen_ai.response.finish_reasons"].(type) {
	case []string:
		if len(v) > 0 {
			g["finish_reasons"] = v
		}
	case []any:
		if len(v) > 0 {
			g["finish_reasons"] = v
		}
	case string:
		if v != "" {
			g["finish_reasons"] = []string{v}
		}
	}
	return g
}
func cleanAttributes(a map[string]any, capture, all, masked bool) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		if stripped(k) {
			continue
		}
		content := all || contentAttribute(k)
		if content && !capture {
			continue
		}
		if hook.Tracing.NormalizeValue != nil {
			var keep bool
			v, keep = hook.Tracing.NormalizeValue(v)
			if !keep {
				continue
			}
		}
		if content && (!masked || k == "exception.message") {
			var ok bool
			v, ok = mask(v)
			if !ok {
				continue
			}
		}
		out[k] = v
	}
	return out
}
func stripped(k string) bool {
	for _, s := range strippedKeys {
		if k == s {
			return true
		}
	}
	return hasPrefix(k, strippedPrefixes)
}
func mask(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		return hook.Tracing.Redact(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			m, ok := mask(e)
			if !ok {
				return nil, false
			}
			out[i] = m
		}
		return out, true
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			m, ok := mask(e)
			if !ok {
				return nil, false
			}
			out[k] = m
		}
		return out, true
	}
	return v, true
}
func keyValues(a map[string]any, order ...[]string) []object {
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(order) > 0 && len(order[0]) > 0 {
		ordered := []string{}
		seen := map[string]bool{}
		for _, k := range order[0] {
			if _, ok := a[k]; ok {
				ordered = append(ordered, k)
				seen[k] = true
			}
		}
		for _, k := range keys {
			if !seen[k] {
				ordered = append(ordered, k)
			}
		}
		keys = ordered
	}
	out := make([]object, 0, len(a))
	for _, k := range keys {
		out = append(out, object{"key": k, "value": anyValue(a[k])})
	}
	return out
}
func anyValue(v any) object {
	switch x := v.(type) {
	case string:
		return object{"stringValue": x}
	case bool:
		return object{"boolValue": x}
	case int:
		return object{"intValue": strconv.Itoa(x)}
	case int64:
		return object{"intValue": strconv.FormatInt(x, 10)}
	case uint64:
		return object{"intValue": strconv.FormatUint(x, 10)}
	case json.Number:
		if n, e := x.Int64(); e == nil {
			return anyValue(n)
		}
		f, _ := x.Float64()
		return anyValue(f)
	case float64:
		var f any = x
		if math.IsNaN(x) {
			f = "NaN"
		} else if math.IsInf(x, 1) {
			f = "Infinity"
		} else if math.IsInf(x, -1) {
			f = "-Infinity"
		}
		return object{"doubleValue": f}
	case []any:
		out := make([]object, len(x))
		for i, e := range x {
			out[i] = anyValue(e)
		}
		return object{"arrayValue": object{"values": out}}
	case map[string]any:
		return object{"kvlistValue": object{"values": keyValues(x)}}
	case []string:
		out := make([]object, len(x))
		for i, e := range x {
			out[i] = anyValue(e)
		}
		return object{"arrayValue": object{"values": out}}
	}
	return object{}
}
