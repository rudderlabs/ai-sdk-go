package rudderstackai

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// delayedConn delays each write, like a slow transport; the server's response is the only
// message written while the delay is set.
type delayedConn struct {
	net.Conn
	delay *atomic.Int64
}

func (c delayedConn) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(c.delay.Load()))
	return c.Conn.Write(p)
}

// slowWire is app output whose JSON encoding takes a while, like a large structured result.
type slowWire struct{ delay time.Duration }

func (v slowWire) MarshalJSON() ([]byte, error) { return testkit.SlowJSON(v.delay) }

// propMS reads an integer millisecond property, whichever numeric type the event carries.
var propMS = testkit.PropertyMilliseconds

var fixtureMS = testkit.FixtureMilliseconds

// serveFixtureMCP connects a real go-sdk client to a wrapped server over observed pipes.
// The server writes through w, so a case can delay the wire.
func serveFixtureMCP(t *testing.T, s *mcp.Server, delay *atomic.Int64) *mcp.ClientSession {
	t.Helper()
	c1, c2 := net.Pipe()
	return connectOver(t, s, &mcp.IOTransport{Reader: c2, Writer: delayedConn{Conn: c2, delay: delay}}, &mcp.IOTransport{Reader: c1, Writer: c1}, true)
}

// runSerializerDelivered replaces the handler's result with a middleware before it is written and
// checks that capture follows the message the client received.
func runSerializerDelivered(t *testing.T, c serializerFixture) {
	original, replacement := c.Setup["original"].(string), c.Setup["replacement"].(string)
	isError := c.Setup["outcome"] == "error"
	s := mcp.NewServer(&mcp.Implementation{Name: "delivered-mcp", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if isError {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: original}}}, nil
		}
		return &mcp.CallToolResult{StructuredContent: original}, nil
	})
	rec := wrap(t, s, Options{CaptureContent: c.Config.CaptureContent, EventRules: EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "Lookup", Properties: func(_ map[string]any, result any) map[string]any { return map[string]any{"output": result} }}}}})
	// Added after the wrapper, so it runs outside it and swaps the result the wrapper saw.
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if out, ok := res.(*mcp.CallToolResult); ok && err == nil && method == "tools/call" {
				if isError {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: replacement}}}, nil
				}
				return &mcp.CallToolResult{StructuredContent: replacement, Content: out.Content}, nil
			}
			return res, err
		}
	})
	cs := serveFixtureMCP(t, s, &atomic.Int64{})
	res := callTool(t, cs, "lookup", map[string]any{}, nil)
	delivered := res.StructuredContent
	if isError {
		delivered = res.Content[0].(*mcp.TextContent).Text
	}
	events := rec.wait(t, "mcp_tool_called", 1)
	var captured any
	if isError {
		captured = events[0].Properties["error_message"]
	} else {
		// The declared event follows mcp_tool_called after the write; wait for it too.
		captured = rec.wait(t, "Lookup", 1)[0].Properties["output"]
	}
	want := c.Expect["decoded"].(map[string]any)
	fixtureEqual(t, delivered, want["delivered"])
	fixtureEqual(t, captured, want["captured"])
	fixtureNever(t, c, rec.all())
	for name, n := range c.Expect["event_counts"].(map[string]any) {
		fixtureEqual(t, len(rec.named(name)), n)
	}
}

// runSerializerHandlerDuration runs two real calls with real delays. Handler completion fixes
// duration and the event time; the write and wire serialization that follow belong to the gap.
func runSerializerHandlerDuration(t *testing.T, c serializerFixture) {
	clock := c.Setup["clock"].(map[string]any)
	handler, write, wire, wait := fixtureMS(t, clock, "handler_ms"), fixtureMS(t, clock, "write_ms"), fixtureMS(t, clock, "serializer_ms"), fixtureMS(t, clock, "following_wait_ms")
	var calls atomic.Int32
	s := mcp.NewServer(&mcp.Implementation{Name: "duration-mcp", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		time.Sleep(handler)
		first := calls.Add(1) == 1
		if c.Setup["outcome"] == "error" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "failed"}}}, nil
		}
		if first && wire > 0 {
			return &mcp.CallToolResult{StructuredContent: slowWire{wire}}, nil
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"public": "visible"}}, nil
	})
	rec := wrap(t, s, Options{CaptureContent: c.Config.CaptureContent, EventRules: EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up"}}}})
	var delay atomic.Int64
	cs := serveFixtureMCP(t, s, &delay)
	delay.Store(int64(write))
	started := time.Now()
	callTool(t, cs, "lookup", map[string]any{}, nil)
	delay.Store(0)
	time.Sleep(wait)
	callTool(t, cs, "lookup", map[string]any{}, nil)
	events := rec.wait(t, "mcp_tool_called", 2)
	declared := rec.wait(t, "looked_up", 2)
	want := c.Expect
	// Real clocks: allow scheduling slack, far below the write delay the case must exclude.
	const slack = 100 * time.Millisecond
	within := func(name string, got, expected time.Duration) {
		t.Helper()
		if got < expected || got > expected+slack {
			t.Errorf("%s=%s want %s (+%s)", name, got, expected, slack)
		}
	}
	duration := propMS(t, events[0].Properties["duration_ms"])
	within("duration", duration, time.Duration(want["duration_ms"].(float64))*time.Millisecond)
	within("timestamp", events[0].OriginalTimestamp.Sub(started), time.Duration(want["timestamp_ms"].(float64)-clock["start_ms"].(float64))*time.Millisecond)
	within("declared timestamp", declared[0].OriginalTimestamp.Sub(started), time.Duration(want["declared_timestamp_ms"].(float64)-clock["start_ms"].(float64))*time.Millisecond)
	within("ms_since_previous_call", propMS(t, events[1].Properties["ms_since_previous_call"]), time.Duration(want["ms_since_previous_call"].(float64))*time.Millisecond)
	for name, n := range want["event_counts"].(map[string]any) {
		fixtureEqual(t, len(rec.named(name)), n)
	}
}
