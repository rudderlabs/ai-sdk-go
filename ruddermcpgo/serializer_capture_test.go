package ruddermcpgo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
	"github.com/rudderlabs/ai-sdk-go/internal/serializerfixture"
)

type fixtureWireSerializer struct {
	calls      int
	serialized any
}

func (v *fixtureWireSerializer) MarshalJSON() ([]byte, error) {
	v.calls++
	raw, _ := json.Marshal(v.serialized)
	return []byte(strings.ReplaceAll(string(raw), "{n}", fmt.Sprint(v.calls))), nil
}
func TestSerializerCapture(t *testing.T) {
	raw, err := os.ReadFile("../spec/fixtures/serializer-capture-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []serializerfixture.Case `json:"cases"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		if !c.AppliesToGo() || c.Setup["adapter"] != "go_mcp_go" {
			continue
		}
		c, err = c.ResolveGo()
		if err != nil {
			t.Fatal(err)
		}
		t.Run(c.ID, func(t *testing.T) {
			switch c.Setup["scenario"] {
			case "mcp_delivered":
				runDelivered(t, c)
				return
			case "mcp_handler_duration":
				runHandlerDuration(t, c)
				return
			}
			if c.ID != "N4-adapter-go-mcp-go" {
				t.Fatalf("unknown applicable MCP adapter fixture %s", c.ID)
			}
			descriptor := c.Setup["value"].(map[string]any)
			v := &fixtureWireSerializer{serialized: descriptor["serialized"]}
			s := server.NewMCPServer("fixture", "1", server.WithToolCapabilities(true))
			s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{StructuredContent: v}, nil
			})
			rule := ai.ToolEventRule{Event: "done", Properties: func(map[string]any, any) map[string]any { return map[string]any{"output": v} }}
			r := wrap(t, s, Options{CaptureContent: c.Config.CaptureContent, EventRules: ai.EventRules{Tools: map[string]ai.ToolEventRule{"lookup": rule}}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, _, out := ObserveStdio(ctx, strings.NewReader(""), io.Discard)
			defer mcpbridge.ResponsesFrom(ctx).Close()
			reply := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`))
			counts := c.Expect["serializer_calls"].(map[string]any)
			if v.calls != int(counts["before_write"].(float64)) {
				t.Fatalf("before write serializers=%d", v.calls)
			}
			wire, err := json.Marshal(reply)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = out.Write(append(wire, '\n')); err != nil {
				t.Fatal(err)
			}
			if v.calls != int(counts["after_write"].(float64)) {
				t.Fatalf("after write serializers=%d", v.calls)
			}
			expected, _ := json.Marshal(c.Expect["decoded"].(map[string]any)["rule_result"])
			got, _ := json.Marshal(r.props(t, "done")["output"])
			if string(got) != string(expected) {
				t.Fatalf("rule=%s want=%s", got, expected)
			}
			if len(r.named("mcp_tool_called")) != 1 {
				t.Fatal("tool event lost")
			}
			for _, name := range []string{"done", "mcp_tool_called"} {
				events, _ := json.Marshal(r.named(name))
				for _, forbidden := range c.Expect["never_emitted"].([]any) {
					if strings.Contains(string(events), forbidden.(string)) {
						t.Fatalf("emitted %v", forbidden)
					}
				}
			}
		})
	}
}
