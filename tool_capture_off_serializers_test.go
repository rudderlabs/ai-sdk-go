package rudderstackai

import (
	"context"
	"fmt"
	"testing"
)

type review84Count struct{ Calls int }

func (v *review84Count) MarshalJSON() ([]byte, error) {
	v.Calls++
	return []byte(`{"content":[]}`), nil
}

func TestReview84CaptureOffToolSerializers(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			setupAI(t, WithEmitSpans(spans), WithCaptureContent(false))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			args, result := &review84Count{}, &review84Count{}
			i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Args: map[string]any{"value": args}})
			i.RecordToolResult("t", ToolResult{Result: result, Error: Ptr(false)})
			i.End(EndOptions{})
			t.Logf("no tool rules, capture off: argument serializer=%d result serializer=%d", args.Calls, result.Calls)
			if args.Calls != 0 || result.Calls != 0 {
				t.Error("capture-disabled reports introduced app serializer calls absent on base")
			}
		})
	}
}
