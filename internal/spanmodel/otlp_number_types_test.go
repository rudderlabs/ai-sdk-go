package spanmodel

import (
	"encoding/json"
	"math"
	"testing"
)

func TestOTLPNumbersPreserveTypesAndSignedRange(t *testing.T) {
	for _, tc := range []struct {
		value any
		key   string
	}{
		{float64(1e20), "doubleValue"}, {float64(1), "doubleValue"}, {int64(math.MaxInt64), "intValue"}, {int64(math.MinInt64), "intValue"}, {uint64(math.MaxInt64), "intValue"}, {uint64(math.MaxInt64) + 1, "doubleValue"}, {uint64(math.MaxUint64), "doubleValue"},
	} {
		encoded := anyValue(tc.value)
		if _, ok := encoded[tc.key]; !ok {
			t.Errorf("%v encoded as %v, want %s", tc.value, encoded, tc.key)
			continue
		}
		b, err := json.Marshal(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			IntValue    int64   `json:"intValue,string"`
			DoubleValue float64 `json:"doubleValue"`
		}
		if err = json.Unmarshal(b, &parsed); err != nil {
			t.Fatalf("OTLP signed number JSON roundtrip: %v", err)
		}
	}
}
