package rudderstackai

import (
	"bytes"
	"encoding/json"
	"math"
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

func validPendingSnapshot(s *PausedInteraction) bool {
	if s.invalidSnapshot {
		return false
	}
	for _, v := range []*int{s.ReportCount, s.SuccessfulCallSequence, s.PendingCompactions} {
		if v != nil && !validSnapshotInteger(*v) {
			return false
		}
	}
	for _, records := range [][]map[string]any{s.PendingCalls, s.PendingTools} {
		for _, p := range records {
			for _, key := range []string{"call_count", "error_count", "input_tokens", "output_tokens", "reasoning_tokens", "cached_input_tokens", "cache_write_tokens", "cache_write_1h_tokens", "server_tool_call_count"} {
				if v, ok := p[key]; ok && !validSnapshotInteger(v) {
					return false
				}
			}
			for _, key := range []string{"last", "last_ok", "history_only", "fallback_used", "byok", "uncredited", "unpriced", "fees_unknown", "error", "settled", "fired"} {
				if v, ok := p[key]; ok {
					if _, ok := v.(bool); !ok {
						return false
					}
				}
			}
			for _, key := range []string{"provider", "model", "requested_model", "response_id", "agent_name", "call_purpose", "service_tier", "finish_reason", "error_type", "tool_name", "raw_name", "tool_call_id", "mcp_server", "parent_tool"} {
				if v, ok := p[key]; ok {
					if _, ok := v.(string); !ok {
						return false
					}
				}
			}
			for _, key := range []string{"credits", "cost_usd", "server_tool_cost_usd", "latency_ms", "time_to_first_token_ms", "duration_ms"} {
				if v, ok := p[key]; ok {
					switch v.(type) {
					case int, int64, float64, json.Number:
					default:
						return false
					}
					n := float64(0)
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
					}
					if n < 0 || n > 9007199254740991 || math.IsNaN(n) || math.IsInf(n, 0) {
						return false
					}
				}
			}
		}
	}
	for _, p := range s.PendingTools {
		if v, ok := p["overflow"]; ok {
			if _, ok := v.(bool); !ok {
				return false
			}
		}
		if v, ok := p["rule_outcome"]; ok {
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
			if n, ok := outcome["result_size_bytes"]; ok && !validSnapshotInteger(n) {
				return false
			}
		}
	}
	return true
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
	for _, key := range []string{"report_count", "successful_call_sequence", "pending_compactions"} {
		if bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			s.invalidSnapshot = true
		}
	}
	return nil
}
