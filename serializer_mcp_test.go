package rudderstackai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestSerializerMCPPostWrite(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			setupAI(t)
			s := workspaceServer("serializer-write", nil)
			rec := wrap(t, s, Options{CaptureContent: true})
			w, _ := instances.get(s)
			responses := mcpbridge.NewResponses()
			out := io.Writer(io.Discard)
			if fail {
				out = fixtureFailWriter{}
			}
			writer := mcpbridge.NewWriter(out, responses)
			rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1, Responses: responses})
			rc.ToolCall("list_destinations", nil, false, nil, "")
			v := &fixtureSerializer{Output: map[string]any{"isError": false, "structuredContent": map[string]any{"public": "visible"}}}
			rc.ToolDone(mcpbridge.Outcome{Result: v})
			if v.Calls != 0 {
				t.Fatalf("before write: %d serializer calls", v.Calls)
			}
			written := v
			if fail {
				// The library encodes before the write fails; that run is not the SDK's.
				twin := *v
				written = &twin
			}
			wire, err := json.Marshal(map[string]any{"id": 1, "result": written})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(append(wire, '\n')); (err != nil) != fail {
				t.Fatal(err)
			}
			want := 1
			if fail {
				want = 0
			}
			if v.Calls != want {
				t.Fatalf("after write: %d calls, want %d", v.Calls, want)
			}
			if len(rec.named("mcp_tool_called")) != 1 {
				t.Fatal("missing event")
			}
			// A failed write keeps base's outcome and size.
			if p := rec.props(t, "mcp_tool_called"); p["tool_status"] != "success" || p["result_size_bytes"] != len(`{"isError":false,"structuredContent":{"public":"visible"}}`) {
				t.Fatalf("outcome=%v", p)
			}
		})
	}
}
