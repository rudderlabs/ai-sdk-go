package ruddermcpgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// mcp-go's legacy SSE transport behind ObserveHTTP answers on another stream: the POST's
// observer is closed when the handler returns, so the call keeps base's outcome and size.
func TestRound2LegacySSEObservedSuccess(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture=%v", capture), func(t *testing.T) {
			s := server.NewMCPServer("legacy-sse", "1", server.WithToolCapabilities(true))
			s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("found it"), nil
			})
			r := wrap(t, s, Options{CaptureContent: capture})
			ts := httptest.NewServer(ObserveHTTP(server.NewSSEServer(s)))
			t.Cleanup(ts.Close)
			cs, err := sdk.NewClient(testClient, nil).Connect(context.Background(), &sdk.SSEClientTransport{Endpoint: ts.URL + "/sse"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cs.Close() })
			if _, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lookup"}); err != nil {
				t.Fatal(err)
			}
			p := waitProps(t, r, "mcp_tool_called")
			wire, _ := json.Marshal(mcp.NewToolResultText("found it"))
			if p["tool_status"] != "success" || p["error_type"] != nil || p["result_size_bytes"] != len(wire) || p["is_error_result"] != false {
				t.Fatalf("successful call reported as %v", p)
			}
		})
	}
}

// Observed stdio and HTTP: result_size_bytes is the written size, HTML escapes included, and a
// result over 1 MiB still gives the rule base's decoded result.
func TestRound2MCPGoObservedResults(t *testing.T) {
	cases := map[string]func() *mcp.CallToolResult{
		"html":      func() *mcp.CallToolResult { return mcp.NewToolResultText("<b>a&b</b> https://x.test/?a=1&b=2") },
		"oversized": func() *mcp.CallToolResult { return mcp.NewToolResultText(strings.Repeat("a&", 600_000)) },
	}
	for _, tc := range observedConnectors {
		for _, name := range []string{"html", "oversized"} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				s := server.NewMCPServer("sizes", "1", server.WithToolCapabilities(true))
				s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return cases[name](), nil })
				var seen any
				r := wrap(t, s, Options{CaptureContent: true, EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"lookup": {Event: "looked_up", Properties: func(_ map[string]any, v any) map[string]any {
					seen = v
					return nil
				}}}}})
				cs := tc.connect(t, s)
				if _, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lookup"}); err != nil {
					t.Fatal(err)
				}
				p := waitProps(t, r, "mcp_tool_called")
				waitProps(t, r, "looked_up")
				wire, _ := json.Marshal(cases[name]())
				if p["result_size_bytes"] != len(wire) || seen != cases[name]().Content[0].(mcp.TextContent).Text {
					t.Fatalf("result_size_bytes=%v want %d; rule input %.40v", p["result_size_bytes"], len(wire), seen)
				}
			})
		}
	}
}

type round2Count struct{ calls int }

func (v *round2Count) MarshalJSON() ([]byte, error) {
	v.calls++
	return []byte(`{"public":"<v>"}`), nil
}

type round2FailedWriter struct{ header http.Header }

func (w *round2FailedWriter) Header() http.Header       { return w.header }
func (w *round2FailedWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (w *round2FailedWriter) WriteHeader(int)           {}

type round2FailWriter struct{}

func (round2FailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A response whose write fails keeps base's outcome and size, read from the attempted bytes;
// the SDK runs no serializer for it (the one run is the encoding of the response).
func TestRound2FailedWriteKeepsOutcome(t *testing.T) {
	for _, transport := range []string{"stdio", "http-json", "http-sse"} {
		t.Run(transport, func(t *testing.T) {
			s := server.NewMCPServer("failed-write", "1", server.WithToolCapabilities(true))
			v := &round2Count{}
			s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: v}, nil
			})
			r := wrap(t, s, Options{CaptureContent: true})
			var size int
			respond := func(ctx context.Context, out io.Writer, frame func([]byte) []byte) {
				reply := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`))
				wire, err := json.Marshal(reply)
				if err != nil {
					t.Fatal(err)
				}
				var members map[string]json.RawMessage
				_ = json.Unmarshal(wire, &members)
				size = len(members["result"])
				if _, err := out.Write(frame(wire)); err == nil {
					t.Fatal("write did not fail")
				}
			}
			if transport == "stdio" {
				ctx, _, out := ObserveStdio(context.Background(), strings.NewReader(""), round2FailWriter{})
				respond(ctx, out, func(b []byte) []byte { return append(b, '\n') })
			} else {
				ObserveHTTP(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
					if transport == "http-sse" {
						out.Header().Set("Content-Type", "text/event-stream")
						respond(req.Context(), out, func(b []byte) []byte { return []byte("event: message\ndata: " + string(b) + "\n\n") })
						return
					}
					out.Header().Set("Content-Type", "application/json")
					respond(req.Context(), out, func(b []byte) []byte { return b })
				})).ServeHTTP(&round2FailedWriter{header: http.Header{}}, httptest.NewRequest(http.MethodPost, "/mcp", nil))
			}
			p := waitProps(t, r, "mcp_tool_called")
			if v.calls != 1 || p["tool_status"] != "success" || p["result_size_bytes"] != size || p["error_message"] != nil {
				t.Fatalf("calls=%d event=%v size=%d", v.calls, p, size)
			}
		})
	}
}
