package rudderstackai

import (
	"bytes"
	"encoding/json"
	"maps"
	"math"
	"time"
)

func validSnapshotInteger(v any) bool {
	var n float64
	switch v := v.(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return false
		}
	default:
		return false
	}
	return n >= 0 && n <= 9007199254740991 && math.Trunc(n) == n
}

// The field lists and kinds match Node's validPending; unknown extensions are ignored.
var pendingCallKinds = map[string]string{
	"call_event_pending": "b", "call_event_emitted": "b",
	"provider": "s", "model": "s", "raw_model": "s", "requested_model": "s", "agent_name": "s", "span_id": "s",
	"call_count": "i", "call_index": "i", "report_sequence": "i", "error_count": "i", "input_tokens": "i", "output_tokens": "i",
	"reasoning_tokens": "i", "cached_input_tokens": "i", "cache_write_tokens": "i", "cache_write_1h_tokens": "i",
	"cost_usd": "f", "credits": "f", "credits_explicit": "b", "call_purpose": "s", "fallback_used": "b", "byok": "b",
	"latency_ms": "f", "time_to_first_token_ms": "f", "finish_reason": "s", "error_type": "s", "response_id": "s", "service_tier": "s",
	"server_tool_call_count": "i", "server_tool_cost_usd": "f", "usage_known": "b", "from_span": "b", "matched": "b",
	"started_at": "f", "ended_at": "f", "last": "b", "last_ok": "b", "history_only": "b", "unpriced": "b", "fees_unknown": "b",
}
var pendingToolKinds = map[string]string{
	"tool_name": "s", "raw_name": "s", "tool_call_id": "s", "agent_name": "s", "mcp_server": "s", "parent_tool": "s",
	"call_count": "i", "error": "b", "error_type": "s", "duration_ms": "f", "settled": "b", "fired": "b",
	"structured_candidate": "b", "result_seen": "b", "tool_event_emitted": "b", "overflow": "b",
}

func pendingSnapshotNumber(v any) (float64, bool) {
	switch v := v.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	default:
		return 0, false
	}
}

func validPendingRecords(records []map[string]any, kinds map[string]string, calls bool) bool {
	for _, record := range records {
		if record == nil {
			return false
		}
		p := record
		if calls {
			p = maps.Clone(record)
			if _, ok := p["matched"]; !ok {
				if v, exists := p["twin_matched"]; exists {
					p["matched"] = v
				}
			}
			if _, ok := p["usage_known"]; !ok {
				if v, ok := p["usage_unknown"].(bool); ok {
					p["usage_known"] = !v
				}
			}
			for _, edge := range []string{"started", "ended"} {
				key := edge + "_at"
				if v, exists := p[key]; exists {
					if s, ok := v.(string); ok {
						t, err := time.Parse(time.RFC3339Nano, s)
						if err != nil {
							return false
						}
						p[key] = float64(t.Unix())*1000 + float64(t.Nanosecond())/1e6
					}
				} else if n, ok := pendingSnapshotNumber(p[edge+"_ns"]); ok {
					p[key] = n / 1e6
				}
			}
		}
		for key, kind := range kinds {
			v, exists := p[key]
			if !exists {
				continue
			}
			switch kind {
			case "s":
				if _, ok := v.(string); !ok {
					return false
				}
			case "b":
				if _, ok := v.(bool); !ok {
					return false
				}
			case "i":
				if !validSnapshotInteger(v) {
					return false
				}
			case "f":
				n, ok := pendingSnapshotNumber(v)
				if !ok || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
					return false
				}
			}
		}
		if v, exists := p["span_source"]; exists && restoredSpanSource(v) == nil {
			return false
		}
		if v, exists := p["rule_outcome"]; exists {
			outcome, ok := v.(map[string]any)
			if !ok {
				return false
			}
			if _, ok := outcome["suppressed"].(bool); !ok {
				return false
			}
			if properties, ok := outcome["properties"].(map[string]any); !ok || properties == nil {
				return false
			}
			if v, exists := outcome["error_type"]; exists && v != "api_error" {
				return false
			}
			if v, exists := outcome["result_size_bytes"]; exists && !validSnapshotInteger(v) {
				return false
			}
		}
	}
	return true
}

func validPendingSnapshot(s *PausedInteraction) bool {
	if s.invalidSnapshot {
		return false
	}
	for _, v := range []*int{s.ReportCount, s.SuccessfulCallSequence, s.PendingCompactions, s.CallSequence} {
		if v != nil && !validSnapshotInteger(*v) {
			return false
		}
	}
	return validPendingRecords(s.PendingCalls, pendingCallKinds, true) && validPendingRecords(s.PendingTools, pendingToolKinds, false)
}

// UnmarshalJSON distinguishes an omitted optional counter from an explicit null.
func (s *PausedInteraction) UnmarshalJSON(data []byte) error {
	type snapshot PausedInteraction
	var decoded snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		*s = PausedInteraction{invalidSnapshot: true}
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*s = PausedInteraction(decoded)
	// A missing or null timing field would decode as zero and count the wait from the Unix epoch.
	for _, key := range []string{"paused_at", "total_paused_ms"} {
		if raw := bytes.TrimSpace(fields[key]); len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			s.invalidSnapshot = true
		}
	}
	for _, key := range []string{"report_count", "successful_call_sequence", "pending_compactions", "call_sequence", "pending_calls", "pending_tools"} {
		if bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			s.invalidSnapshot = true
		}
	}
	return nil
}
