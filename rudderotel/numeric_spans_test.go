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
