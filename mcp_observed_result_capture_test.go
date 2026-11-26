package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A stdio-style client sends tools/call and closes its input before the handler returns: the
// observer is closed, so the result is captured as without one and keeps base's outcome.
func TestRound2GoSDKObservedClientEOF(t *testing.T) {
	setupAI(t)
	release := make(chan struct{})
	s := mcp.NewServer(&mcp.Implementation{Name: "eof", Version: "1"}, nil)
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}
	s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		<-release
		return result, nil
	})
	r := wrap(t, s, Options{})
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	ctx, st := ObserveMCPTransport(context.Background(), &mcp.IOTransport{Reader: serverIn, Writer: serverOut})
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	go func() { _, _ = io.Copy(io.Discard, clientIn) }()
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`,
	} {
		if _, err := clientOut.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	_ = clientOut.Close()
	time.Sleep(50 * time.Millisecond)
	close(release)
	p := r.wait(t, "mcp_tool_called", 1)[0].Properties
	_ = ss.Wait()
	_ = serverOut.Close()
	wire, _ := json.Marshal(result)
	if p["tool_status"] != "success" || p["error_type"] != nil || p["result_size_bytes"] != len(wire) {
		t.Fatalf("handler succeeded; got %v", p)
	}
}

// result_size_bytes is base's size: the bytes encoding/json writes, HTML escapes included, with
// and without an observer, for text and for structured content with an app MarshalJSON.
func TestRound2MCPResultSizeMatchesBase(t *testing.T) {
	results := map[string]func() *mcp.CallToolResult{
		"text": func() *mcp.CallToolResult {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "see https://x.test/?a=1&b=2 <ok>"}}}
		},
		"structured": func() *mcp.CallToolResult {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "{}"}}, StructuredContent: map[string]any{"s": "<x>", "app": round2EscapedJSON{URL: "https://x.test/?a=1&b=2"}}}
		},
	}
	for name, result := range results {
		for _, observe := range []bool{false, true} {
			for _, rule := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/observe=%v/rule=%v", name, observe, rule), func(t *testing.T) {
					setupAI(t)
					s := mcp.NewServer(&mcp.Implementation{Name: "sizes", Version: "1"}, nil)
					s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						return result(), nil
					})
					opts := Options{CaptureContent: true}
					if rule {
						opts.EventRules = EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up"}}}
					}
					r := wrap(t, s, opts)
					cs := connectMaybeObserved(t, s, observe)
					if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup"}); err != nil {
						t.Fatal(err)
					}
					p := r.wait(t, "mcp_tool_called", 1)[0].Properties
					wire, _ := json.Marshal(result())
					if p["result_size_bytes"] != len(wire) {
						t.Fatalf("result_size_bytes=%v, encoding/json gives %d", p["result_size_bytes"], len(wire))
					}
				})
			}
		}
	}
}

// An observed result over 1 MiB keeps no body: the rule still gets base's decoded result.
func TestRound2MCPOversizedRuleInput(t *testing.T) {
	setupAI(t)
	text := strings.Repeat("a<", 600_000)
	s := mcp.NewServer(&mcp.Implementation{Name: "oversized", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	})
	var seen any
	r := wrap(t, s, Options{EventRules: EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", Properties: func(_ map[string]any, v any) map[string]any {
		seen = v
		return nil
	}}}}})
	cs := connectMaybeObserved(t, s, true)
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup"}); err != nil {
		t.Fatal(err)
	}
	p := r.wait(t, "mcp_tool_called", 1)[0].Properties
	r.wait(t, "looked_up", 1)
	wire, _ := json.Marshal(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}})
	if seen != text || p["tool_status"] != "success" || p["result_size_bytes"] != len(wire) {
		t.Fatalf("rule input %.40v; event %v", seen, p)
	}
}

// Buffer-shaped rule properties remain a container when spans are enabled.
func TestRound2MCPRuleBufferShapedProperties(t *testing.T) {
	for _, observe := range []bool{false, true} {
		t.Run(fmt.Sprintf("observe=%t", observe), func(t *testing.T) {
			setupAI(t, WithEmitSpans(true))
			s := mcp.NewServer(&mcp.Implementation{Name: "buffer", Version: "1"}, nil)
			s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
			r := wrap(t, s, Options{EventRules: EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", Properties: func(map[string]any, any) map[string]any {
				return map[string]any{"type": "Buffer", "data": []int{1, 2}}
			}}}}})
			cs := connectMaybeObserved(t, s, observe)
			if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup"}); err != nil {
				t.Fatal(err)
			}
			p := r.wait(t, "looked_up", 1)[0].Properties
			if p["type"] != "Buffer" || mustJSON(p["data"]) != "[1,2]" || p["tool_status"] != "success" {
				t.Fatalf("declared event %v", p)
			}
		})
	}
}

type round2TextKey int

func (round2TextKey) MarshalText() ([]byte, error) { return []byte("public"), nil }

type round2Child struct{ calls *int }

func (c *round2Child) MarshalJSON() ([]byte, error) {
	*c.calls++
	return []byte(`{"v":1}`), nil
}

// Observed results reuse plain-key values without confusing keys named by MarshalText.
func TestRound2MCPSeedMapKeys(t *testing.T) {
	for _, key := range []string{"text", "string"} {
		t.Run(key, func(t *testing.T) {
			setupAI(t)
			calls := 0
			child := &round2Child{calls: &calls}
			var structured any = map[round2TextKey]*round2Child{1: child}
			if key == "string" {
				structured = map[string]*round2Child{"public": child}
			}
			s := mcp.NewServer(&mcp.Implementation{Name: "keys", Version: "1"}, nil)
			s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: structured}, nil
			})
			r := wrap(t, s, Options{CaptureContent: true, EventRules: EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", Properties: func(map[string]any, any) map[string]any {
				return map[string]any{"child": child}
			}}}}})
			cs := connectMaybeObserved(t, s, true)
			if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup"}); err != nil {
				t.Fatal(err)
			}
			p := r.wait(t, "looked_up", 1)[0].Properties
			want := 1 // the library's write
			if key == "text" {
				want = 2 // not seeded, so the rule property runs it once more
			}
			if mustJSON(p["child"]) != `{"v":1}` || calls != want {
				t.Fatalf("child=%v calls=%d want %d", p["child"], calls, want)
			}
		})
	}
}
