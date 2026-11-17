package spanmodel

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/capture"
)

// When completion grows the mandatory core past the budget, shedding restores the admitted core
// but keeps the final IDs, parent, timing, status, message ID and timestamp.
func TestShedRestoresTheCoreWithFinalIDsAndTimestamp(t *testing.T) {
	started := time.Date(2025, 10, 9, 8, 53, 20, 0, time.UTC)
	kinds := capture.Spans{}
	admitted := Span{
		Name: "chat m", Kind: 3, Context: Context{TraceID: "0102030405060708090a0b0c0d0e0f10", SpanID: "1111111111111111"},
		Start: started, End: started.Add(time.Second), StatusCode: 1, Scope: Scope{Name: "ai-sdk-go"},
		Attributes: map[string]any{"gen_ai.operation.name": "chat", "gen_ai.request.model": "m", "optional.tiny": true},
	}
	options := Options{Capture: &kinds, SDKIdentity: true, UserID: "u", MessageContext: map[string]any{"app": "x"}, LibraryName: "ai-sdk-go", LibraryVersion: "test"}
	core, ok := DeliveryCore(admitted, options)
	if !ok {
		t.Fatal("a small span was not admitted")
	}
	// Completion moves the span under a parent assigned later, ends it later with an error and
	// grows the mandatory context beyond the budget with numbers, which never truncate.
	final := admitted
	final.Context.SpanID = "2222222222222222"
	final.Parent = Context{TraceID: admitted.Context.TraceID, SpanID: "3333333333333333"}
	final.End = started.Add(3 * time.Second)
	final.StatusCode = 2
	completed := options
	completed.MessageContext = map[string]any{"app": "x", "pad": make([]int, 20_000)}
	completed.DeliveryCore = core
	m, err := ToTrack(final, completed)
	if err != nil || m == nil {
		t.Fatalf("an admitted span was dropped: %v", err)
	}
	if b, _ := json.Marshal(m); len(b) > DefaultMaxEventBytes {
		t.Fatalf("delivered %d bytes, over the budget", len(b))
	}
	if want := admitted.Context.TraceID + "-2222222222222222"; m.MessageId != want {
		t.Errorf("messageId %q, want the final %q", m.MessageId, want)
	}
	if !m.OriginalTimestamp.Equal(final.End) {
		t.Errorf("timestamp %v, want the completion %v", m.OriginalTimestamp, final.End)
	}
	if _, kept := m.Context.Extra["pad"]; kept {
		t.Error("the oversized context was not restored to the admitted core")
	}
	p := object(m.Properties)
	if p["span_id"] != "2222222222222222" || p["parent_span_id"] != "3333333333333333" || p["end_time"] != "2025-10-09T08:53:23.000Z" ||
		p["duration_ms"] != 3000.0 || p["status_code"] != "error" || p["truncated"] != true {
		t.Errorf("core properties not final: %v", p)
	}
	_, _, span := otlpSpan(p)
	if span["spanId"] != "2222222222222222" || span["parentSpanId"] != "3333333333333333" ||
		span["endTimeUnixNano"] != strconv.FormatInt(final.End.UnixNano(), 10) || span["status"].(object)["code"] != 2 {
		t.Errorf("OTLP span not final: %v", span)
	}
}

type failingMarshaler struct{}

func (failingMarshaler) MarshalJSON() ([]byte, error) { return nil, errors.New("cannot encode") }

// A core that cannot be encoded is never admitted, however small: an encoding error is not a
// size of zero.
func TestDeliveryCoreRefusesAnUnencodableCore(t *testing.T) {
	kinds := capture.Spans{}
	span := Span{
		Name: "chat m", Kind: 3, Context: Context{TraceID: "0102030405060708090a0b0c0d0e0f10", SpanID: "1111111111111111"},
		Start: time.Unix(1, 0), End: time.Unix(2, 0), Scope: Scope{Name: "ai-sdk-go"},
		Attributes: map[string]any{"gen_ai.operation.name": "chat", "gen_ai.request.model": "m"},
	}
	for name, value := range map[string]any{
		"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1),
		"invalid json.Number in a map":  map[string]any{"n": json.Number("not-a-number")},
		"invalid json.Number in a list": []any{1, json.Number("1e")},
		"failing Marshaler":             failingMarshaler{},
		"channel":                       make(chan int),
	} {
		t.Run(name, func(t *testing.T) {
			options := Options{Capture: &kinds, SDKIdentity: true, UserID: "u", MessageContext: map[string]any{"bad": value}, LibraryName: "ai-sdk-go", LibraryVersion: "test"}
			if core, ok := DeliveryCore(span, options); ok || core != nil {
				t.Errorf("an unencodable core was admitted (core %v)", core != nil)
			}
		})
	}
}
