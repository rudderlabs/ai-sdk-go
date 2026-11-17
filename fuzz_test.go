package rudderstackai

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Fuzz tests for the MCP _meta join, the only request data the server wrapper reads. The seed
// inputs run with every go test; go test -fuzz FuzzJoinIDs (or FuzzTraceparent) explores more.

var traceIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

func FuzzJoinIDs(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"com.rudderstack/interaction_id":"abc","traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}`,
		`{"com.rudderstack/interaction_id":"  spaced  ","com.rudderstack/trace_id":"t-1"}`,
		`{"com.rudderstack/interaction_id":12,"com.rudderstack/trace_id":null,"traceparent":["x"]}`,
		`{"traceparent":"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","com.rudderstack/trace_id":"fallback"}`,
		`{"com.rudderstack/interaction_id":"` + strings.Repeat("é", 300) + `"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var meta map[string]any
		if json.Unmarshal(raw, &meta) != nil {
			return
		}
		interactionID, traceID := joinIDs(meta)
		for _, v := range []string{interactionID, traceID} {
			if utf8.RuneCountInString(v) > maxMetaLength {
				t.Errorf("unbounded ID %q", v)
			}
		}
		// A valid traceparent wins over com.rudderstack/trace_id.
		if tp, ok := meta[metaTraceparent].(string); ok {
			if want := traceIDFromTraceparent(tp); want != "" && traceID != want {
				t.Errorf("trace_id %q, want %q from %q", traceID, want, tp)
			}
		}
	})
}

func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func anyHex(s string, n int) bool { return lowerHex(strings.ToLower(s), n) }

func FuzzTraceparent(f *testing.F) {
	const trace, parent = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	for _, seed := range [][5]string{
		{"00", trace, parent, "01", ""},
		{"01", trace, parent, "01", "-extra"},
		{"00", trace, parent, "01", "-extra"},
		{"ff", trace, parent, "01", ""},
		{"00", strings.ToUpper(trace), parent, "01", ""},
		{"00", strings.Repeat("0", 32), parent, "01", ""},
		{"00", trace, strings.Repeat("0", 16), "01", ""},
		{"00", trace, strings.ToUpper(parent), "01", ""},
		{"00", trace, parent, "0A", ""},
		{"00", trace, parent, "0", "1"},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4])
	}
	f.Fuzz(func(t *testing.T, version, traceID, parentID, flags, rest string) {
		got := traceIDFromTraceparent(version + "-" + traceID + "-" + parentID + "-" + flags + rest)
		if got != "" && (!traceIDShape.MatchString(got) || strings.Trim(got, "0") == "") {
			t.Fatalf("trace ID %q is not 32 lowercase hex digits", got)
		}
		// The W3C rule: lowercase hex, not version ff, neither ID all zeros; version 00 is exactly
		// 55 characters, and only later versions may add fields after the flags.
		// The parser trims the header first, so trailing space after the flags or extra fields is not part of it.
		rest = strings.TrimRightFunc(rest, unicode.IsSpace)
		valid := lowerHex(version, 2) && version != "ff" && lowerHex(traceID, 32) && strings.Trim(traceID, "0") != "" &&
			lowerHex(parentID, 16) && strings.Trim(parentID, "0") != "" && lowerHex(flags, 2) &&
			(rest == "" || (rest[0] == '-' && version != "00"))
		if valid && got != traceID {
			t.Fatalf("valid traceparent read as %q, want %q", got, traceID)
		}
		// With every field hex of its length (in any case), the header splits one way only, so the
		// parser must reject whatever the rule rejects: uppercase, version ff, all-zero IDs.
		if !valid && got != "" && anyHex(version, 2) && anyHex(traceID, 32) && anyHex(parentID, 16) && anyHex(flags, 2) &&
			(rest == "" || rest[0] == '-') {
			t.Fatalf("invalid traceparent (version %q, IDs %q %q) read as %q", version, traceID, parentID, got)
		}
	})
}
