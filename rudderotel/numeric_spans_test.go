package rudderotel

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestExporterNonfiniteFloatSliceWithSpansDisabled(t *testing.T) {
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(false)); err != nil {
		t.Fatal(err)
	}
	source := tracetest.SpanStub{Attributes: []attribute.KeyValue{attribute.Float64Slice("values", []float64{math.NaN(), math.Inf(1), math.Inf(-1)})}}
	track, err := SpanToTrack(source.Snapshot(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(track.Properties["otlp"])
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`"doubleValue":"NaN"`, `"doubleValue":"Infinity"`, `"doubleValue":"-Infinity"`} {
		if !strings.Contains(string(encoded), value) {
			t.Errorf("missing %s: %s", value, encoded)
		}
	}
}

func TestExporterSpansOffNumberTypesMatchMain(t *testing.T) {
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(false)); err != nil {
		t.Fatal(err)
	}
	source := tracetest.SpanStub{Attributes: []attribute.KeyValue{
		attribute.Float64("large_float", 1e20), attribute.Float64("integral_float", 1),
		attribute.Int64("max_int", math.MaxInt64), attribute.Int64("min_int", math.MinInt64),
	}}
	track, err := SpanToTrack(source.Snapshot(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(track.Properties["otlp"])
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					Attributes []struct {
						Key   string
						Value struct {
							IntValue    *int64   `json:"intValue,string"`
							DoubleValue *float64 `json:"doubleValue"`
						}
					}
				}
			}
		}
	}
	if err = json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	attrs := parsed.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes
	if len(attrs) != 4 {
		t.Fatalf("attributes=%d", len(attrs))
	}
	for _, a := range attrs {
		if strings.Contains(a.Key, "float") {
			if a.Value.DoubleValue == nil || a.Value.IntValue != nil {
				t.Fatalf("float changed type: %+v", a)
			}
		} else if a.Value.IntValue == nil || a.Value.DoubleValue != nil {
			t.Fatalf("int changed type: %+v", a)
		}
	}
}
