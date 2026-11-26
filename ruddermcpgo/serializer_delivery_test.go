package ruddermcpgo

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/serializerfixture"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// delayedWriter delays each write, like a slow transport.
type delayedWriter struct {
	io.Writer
	delay *atomic.Int64
}

func (w delayedWriter) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(w.delay.Load()))
	return w.Writer.Write(p)
}

// slowWire is app output whose JSON encoding takes a while, like a large structured result.
type slowWire struct{ delay time.Duration }

func (v slowWire) MarshalJSON() ([]byte, error) { return testkit.SlowJSON(v.delay) }

// rewritingWriter is an app response filter between the server and the observed writer: it
// rewrites each JSON-RPC response line before the observer sees it.
type rewritingWriter struct {
	io.Writer
	rewrite func(map[string]any)
}

func (w rewritingWriter) Write(p []byte) (int, error) {
	var message map[string]any
	if json.Unmarshal(p, &message) != nil {
		return w.Writer.Write(p)
	}
	w.rewrite(message)
	b, err := json.Marshal(message)
	if err != nil {
		return 0, err
	}
	if _, err = w.Writer.Write(append(b, '\n')); err != nil {
		return 0, err
	}
	return len(p), nil
}

// serveObservedStdio serves s over observed stdio and connects a real go-sdk client. A filter,
// when set, rewrites the server's responses before the observer sees them.
func serveObservedStdio(t *testing.T, s *server.MCPServer, delay *atomic.Int64, filter func(map[string]any)) *sdk.ClientSession {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	ctx, in, out := ObserveStdio(ctx, serverIn, delayedWriter{serverOut, delay})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if filter != nil {
			out = rewritingWriter{out, filter}
		}
		_ = server.NewStdioServer(s).Listen(ctx, in, out)
	}()
	cs, err := sdk.NewClient(testClient, nil).Connect(context.Background(),
		&sdk.IOTransport{Reader: clientIn, Writer: clientOut}, &sdk.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		_ = serverIn.Close()
		_ = clientIn.Close()
		<-done
	})
	return cs
}

var fixtureMS = testkit.FixtureMilliseconds

// propMS reads an integer millisecond property, whichever numeric type the event carries.
var propMS = testkit.PropertyMilliseconds

// waitEvents waits for n events named event; held events are sent after the response is written.
func waitEvents(t *testing.T, r *recorder, event string, n int) []analytics.Track {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := r.named(event); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d %s, got %v", n, event, r.names())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runDelivered rewrites the response in a filter after the server hooks ran and before the write
// is observed, and checks that capture follows the message the client received.
func runDelivered(t *testing.T, c serializerfixture.Case) {
	original, replacement := c.Setup["original"].(string), c.Setup["replacement"].(string)
	isError := c.Setup["outcome"] == "error"
	s := server.NewMCPServer("delivered", "1", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if isError {
			return mcp.NewToolResultError(original), nil
		}
		return &mcp.CallToolResult{StructuredContent: original}, nil
	})
	rule := ai.ToolEventRule{Event: "Lookup", Properties: func(_ map[string]any, result any) map[string]any { return map[string]any{"output": result} }}
	r := wrap(t, s, Options{CaptureContent: c.Config.CaptureContent, EventRules: ai.EventRules{Tools: map[string]ai.ToolEventRule{"lookup": rule}}})
	cs := serveObservedStdio(t, s, &atomic.Int64{}, func(message map[string]any) {
		result, _ := message["result"].(map[string]any)
		if result == nil {
			return
		}
		if isError {
			result["content"] = []any{map[string]any{"type": "text", "text": replacement}}
		} else {
			result["structuredContent"] = replacement
		}
	})
	res := callTool(t, cs, "lookup", map[string]any{}, nil)
	delivered := res.StructuredContent
	if isError {
		delivered = res.Content[0].(*sdk.TextContent).Text
	}
	events := waitEvents(t, r, "mcp_tool_called", 1)
	var captured any
	if isError {
		captured = events[0].Properties["error_message"]
	} else {
		// The declared event follows mcp_tool_called after the write; wait for it too.
		captured = waitEvents(t, r, "Lookup", 1)[0].Properties["output"]
	}
	want := c.Expect["decoded"].(map[string]any)
	for name, got := range map[string]any{"delivered": delivered, "captured": captured} {
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want[name])
		if string(a) != string(b) {
			t.Errorf("%s=%s want=%s", name, a, b)
		}
	}
	all, _ := json.Marshal(r.all())
	for _, word := range c.Expect["never_emitted"].([]any) {
		if strings.Contains(string(all), word.(string)) {
			t.Errorf("emitted %v", word)
		}
	}
	if n := len(r.named("mcp_tool_called")); n != 1 {
		t.Errorf("mcp_tool_called=%d", n)
	}
}

// runHandlerDuration runs two real calls with real delays. Handler completion fixes duration and
// the event time; the write and wire serialization that follow belong to the gap.
func runHandlerDuration(t *testing.T, c serializerfixture.Case) {
	clock := c.Setup["clock"].(map[string]any)
	handler, write, wire, wait := fixtureMS(t, clock, "handler_ms"), fixtureMS(t, clock, "write_ms"), fixtureMS(t, clock, "serializer_ms"), fixtureMS(t, clock, "following_wait_ms")
	var calls atomic.Int32
	s := server.NewMCPServer("duration", "1", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		time.Sleep(handler)
		first := calls.Add(1) == 1
		if c.Setup["outcome"] == "error" {
			return mcp.NewToolResultError("failed"), nil
		}
		if first && wire > 0 {
			return &mcp.CallToolResult{StructuredContent: slowWire{wire}}, nil
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"public": "visible"}}, nil
	})
	r := wrap(t, s, Options{CaptureContent: c.Config.CaptureContent, EventRules: ai.EventRules{Tools: map[string]ai.ToolEventRule{"lookup": {Event: "looked_up"}}}})
	var delay atomic.Int64
	cs := serveObservedStdio(t, s, &delay, nil)
	// The initialize exchange above has run; delay only the first call's response.
	delay.Store(int64(write))
	started := time.Now()
	callTool(t, cs, "lookup", map[string]any{}, nil)
	delay.Store(0)
	time.Sleep(wait)
	callTool(t, cs, "lookup", map[string]any{}, nil)
	events := waitEvents(t, r, "mcp_tool_called", 2)
	declared := waitEvents(t, r, "looked_up", 2)
	want := c.Expect
	// Real clocks: allow scheduling slack, far below the write delay the case must exclude.
	const slack = 100 * time.Millisecond
	within := func(name string, got, expected time.Duration) {
		t.Helper()
		if got < expected || got > expected+slack {
			t.Errorf("%s=%s want %s (+%s)", name, got, expected, slack)
		}
	}
	within("duration", propMS(t, events[0].Properties["duration_ms"]), time.Duration(want["duration_ms"].(float64))*time.Millisecond)
	within("timestamp", events[0].OriginalTimestamp.Sub(started), time.Duration(want["timestamp_ms"].(float64)-clock["start_ms"].(float64))*time.Millisecond)
	within("declared timestamp", declared[0].OriginalTimestamp.Sub(started), time.Duration(want["declared_timestamp_ms"].(float64)-clock["start_ms"].(float64))*time.Millisecond)
	within("ms_since_previous_call", propMS(t, events[1].Properties["ms_since_previous_call"]), time.Duration(want["ms_since_previous_call"].(float64))*time.Millisecond)
	for name, n := range want["event_counts"].(map[string]any) {
		if got := len(r.named(name)); got != int(n.(float64)) {
			t.Errorf("%s=%d want %v", name, got, n)
		}
	}
}
