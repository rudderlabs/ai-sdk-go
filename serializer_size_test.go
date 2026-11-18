package rudderstackai

import (
	"testing"
)

func TestCaptureSizeParity(t *testing.T) {
	for _, spans := range []bool{false, true} {
		setupAI(t, WithEmitSpans(spans))
		for _, value := range []any{map[string]any{"data": []byte("bytes"), "number": float32(.1)}, struct {
			Values []int `json:"values,omitempty"`
			Number int   `json:"number,string"`
		}{Values: []int{}, Number: 123}, map[string]any{"unsupported": func() {}}, []any{float32(.1), "<html>"}} {
			want := -1
			if b, err := compactJSON(value); err == nil {
				want = len(b)
			}
			if got := captureValue(value).size; got != want {
				t.Errorf("spans=%v value=%T size=%d want=%d", spans, value, got, want)
			}
		}
	}
}

type captureTextKey struct{ Calls *int }

func (k captureTextKey) MarshalText() ([]byte, error) { *k.Calls++; return []byte("key"), nil }
func TestCaptureMapKeySerializerOnce(t *testing.T) {
	setupAI(t, WithEmitSpans(false))
	calls := 0
	report := captureValue(map[captureTextKey]int{{Calls: &calls}: 1})
	if calls != 1 {
		t.Fatalf("calls=%d want=1", calls)
	}
	if report.size != len(`{"key":1}`) {
		t.Fatalf("size=%d", report.size)
	}
}
