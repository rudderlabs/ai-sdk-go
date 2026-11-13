package rudderotel

import (
	"math"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func attributes(attrs []attribute.KeyValue) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func asString(v attribute.Value) string {
	switch v.Type() {
	case attribute.STRING:
		return v.AsString()
	case attribute.INT64:
		return strconv.FormatInt(v.AsInt64(), 10)
	case attribute.FLOAT64:
		f := v.AsFloat64()
		if !math.IsNaN(f) && !math.IsInf(f, 0) {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	return ""
}

func firstString(a map[string]attribute.Value, keys ...string) string {
	for _, k := range keys {
		if v := asString(a[k]); v != "" {
			return v
		}
	}
	return ""
}

func count(a map[string]attribute.Value, keys ...string) (int, bool) {
	for _, k := range keys {
		v := a[k]
		if v.Type() == attribute.INT64 && v.AsInt64() >= 0 && uint64(v.AsInt64()) <= uint64(^uint(0)>>1) {
			return int(v.AsInt64()), true
		}
		if v.Type() == attribute.FLOAT64 {
			f := v.AsFloat64()
			if f >= 0 && f < float64(^uint(0)>>1) && math.Trunc(f) == f {
				return int(f), true
			}
		}
	}
	return 0, false
}

func stringArray(v attribute.Value) []string {
	if v.Type() == attribute.STRINGSLICE && len(v.AsStringSlice()) > 0 {
		return v.AsStringSlice()
	}
	if v.Type() == attribute.STRING && v.AsString() != "" {
		return []string{v.AsString()}
	}
	return nil
}

// IsAISpan is the default export filter: GenAI, OpenInference, or legacy ai.operationId.
func IsAISpan(s sdktrace.ReadOnlySpan) bool {
	for _, kv := range s.Attributes() {
		k := string(kv.Key)
		if strings.HasPrefix(k, "gen_ai.") || k == "openinference.span.kind" || k == "ai.operationId" {
			return true
		}
	}
	return false
}
