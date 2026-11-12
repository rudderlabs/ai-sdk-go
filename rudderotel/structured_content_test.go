package rudderotel

import (
	"errors"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel/attribute"

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
					got := cleanAttributes(attrs, true, all)
					want := []attribute.KeyValue{attribute.Int64("safe", 42)}
					if !withhold {
						want = append([]attribute.KeyValue{{Key: key, Value: build("masked")}}, want...)
					}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("got %v want %v", got, want)
					}
					if len(calls) == 0 || calls[len(calls)-1] != "[email]" {
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
