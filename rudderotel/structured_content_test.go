package rudderotel

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestStructuredContentRedaction(t *testing.T) {
	builders := map[string]func(string) attribute.Value{
		"slice": func(text string) attribute.Value {
			return attribute.SliceValue(
				attribute.StringValue("first"), attribute.Int64Value(7), attribute.BoolValue(false),
				attribute.Value{}, attribute.Float64Value(1.5),
				attribute.MapValue(attribute.StringSlice("nested", []string{"first", text})),
			)
		},
		"map": func(text string) attribute.Value {
			return attribute.MapValue(
				attribute.String("first", "first"), attribute.Int64("integer", 7),
				attribute.Bool("bool", false), attribute.Float64("float", 1.5),
				attribute.KeyValue{Key: "nested", Value: attribute.SliceValue(attribute.StringValue(text))},
				attribute.BoolSlice("bools", []bool{true, false}),
				attribute.Int64Slice("ints", []int64{1, 2}),
				attribute.Float64Slice("floats", []float64{1.5, 2.5}),
				attribute.KeyValue{Key: "bytes", Value: attribute.ByteSliceValue([]byte{1, 2})},
			)
		},
		"deep": func(text string) attribute.Value {
			v := attribute.StringValue(text)
			for range 20 {
				v = attribute.MapValue(attribute.KeyValue{Key: "nested", Value: attribute.SliceValue(v)})
			}
			return v
		},
	}
	for name, build := range builders {
		for _, withhold := range []bool{false, true} {
			for _, all := range []bool{false, true} {
				t.Run(name+"/withhold="+strconvBool(withhold)+"/all="+strconvBool(all), func(t *testing.T) {
					hook.Reset()
					t.Cleanup(hook.Reset)
					calls := []string{}
					if err := rudderstackai.Configure(rudderstackai.WithRedactHook(func(s string) (string, error) {
						calls = append(calls, s)
						if withhold && s == "[email]" {
							return "", errors.New("withheld")
						}
						if s == "[email]" {
							return "masked", nil
						}
						return s, nil
					})); err != nil {
						t.Fatal(err)
					}
					key := attribute.Key("input.value")
					if all {
						key = "body"
					}
					attrs := []attribute.KeyValue{
						{Key: key, Value: build("person@example.com")},
						attribute.Int64("safe", 42),
					}
					original := []attribute.KeyValue{
						{Key: key, Value: build("person@example.com")},
						attribute.Int64("safe", 42),
					}
					source := tracetest.SpanStub{Attributes: attrs}
					if all {
						source.Attributes = nil
						source.Events = []sdktrace.Event{{Name: "gen_ai.user.message", Attributes: attrs}}
					}
					track, err := SpanToTrack(source.Snapshot(), Options{CaptureContent: rudderstackai.Ptr(true)})
					if err != nil {
						t.Fatal(err)
					}
					otlp := track.Properties["otlp"].(object)
					span := otlp["resourceSpans"].([]object)[0]["scopeSpans"].([]object)[0]["spans"].([]object)[0]
					got := span["attributes"].([]object)
					if all {
						got = span["events"].([]object)[0]["attributes"].([]object)
					}
					wantValues := []object{{"key": "safe", "value": object{"intValue": "42"}}}
					if !withhold {
						wantValues = append([]object{{"key": string(key), "value": structuredExpected(name)}}, wantValues...)
					}
					gotJSON, _ := json.Marshal(got)
					wantJSON, _ := json.Marshal(wantValues)
					if string(gotJSON) != string(wantJSON) {
						t.Errorf("got %s want %s", gotJSON, wantJSON)
					}

					if !slices.Contains(calls, "[email]") {
						t.Errorf("nested string did not reach hook after built-in masking: %v", calls)
					}
					if !reflect.DeepEqual(attrs, original) {
						t.Fatal("caller attributes mutated")
					}
				})
			}
		}
	}
}

func structuredExpected(name string) object {
	str := func(s string) object { return object{"stringValue": s} }
	arr := func(values ...object) object { return object{"arrayValue": object{"values": values}} }
	kv := func(key string, v object) object { return object{"key": key, "value": v} }
	obj := func(values ...object) object { return object{"kvlistValue": object{"values": values}} }
	switch name {
	case "slice":
		return arr(str("first"), object{"intValue": "7"}, object{"boolValue": false}, object{}, object{"doubleValue": 1.5}, obj(kv("nested", arr(str("first"), str("masked")))))
	case "map":
		return obj(kv("bool", object{"boolValue": false}), kv("bools", arr(object{"boolValue": true}, object{"boolValue": false})), kv("first", str("first")), kv("float", object{"doubleValue": 1.5}), kv("floats", arr(object{"doubleValue": 1.5}, object{"doubleValue": 2.5})), kv("integer", object{"intValue": "7"}), kv("ints", arr(object{"intValue": "1"}, object{"intValue": "2"})), kv("nested", arr(str("masked"))))
	default:
		v := str("masked")
		for range 20 {
			v = obj(kv("nested", arr(v)))
		}
		return v
	}
}
