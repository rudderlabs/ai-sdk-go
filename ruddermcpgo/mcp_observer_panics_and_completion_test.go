package ruddermcpgo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

type round1PanicWriter struct{ calls int }

func (w *round1PanicWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		panic("writer")
	}
	return len(p), nil
}
func TestRound1ObserveStdioWriterPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, out := ObserveStdio(ctx, strings.NewReader(""), &round1PanicWriter{})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic")
			}
		}()
		_, _ = out.Write([]byte("first\n"))
	}()
	done := make(chan struct{})
	go func() { _, _ = out.Write([]byte("second\n")); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("writer remained locked")
	}
}

type round1Count struct{ calls int }

func (v *round1Count) MarshalJSON() ([]byte, error) { v.calls++; return []byte("{}"), nil }

func TestRound1ClosedObserverHandlerCompletion(t *testing.T) {
	for _, transport := range []string{"stdio", "http"} {
		for _, closure := range []string{"context", "connection"} {
			t.Run(transport+"/"+closure, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				s := server.NewMCPServer("closure", "1", server.WithToolCapabilities(true))
				entered, release := make(chan struct{}), make(chan struct{})
				v := &round1Count{}
				s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					close(entered)
					<-release
					return &mcp.CallToolResult{StructuredContent: v}, nil
				})
				r := wrap(t, s, Options{CaptureContent: true})
				run := func(ctx context.Context, out io.Writer) {
					responses := mcpbridge.ResponsesFrom(ctx)
					done := make(chan struct{})
					go func() {
						defer close(done)
						_ = s.HandleMessage(ctx, []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"lookup\",\"arguments\":{}}}"))
					}()
					<-entered
					if closure == "context" {
						cancel()
						waitRound1Closed(t, responses)
					} else {
						if closer, ok := out.(io.Closer); ok {
							_ = closer.Close()
						} else {
							responses.Close()
						}
					}
					close(release)
					<-done
				}
				if transport == "stdio" {
					observed, _, out := ObserveStdio(ctx, strings.NewReader(""), io.Discard)
					run(observed, out)
				} else {
					ObserveHTTP(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) { run(req.Context(), out) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx))
				}
				// Closed before the handler returned: captured once, as without an observer.
				if v.calls != 1 {
					t.Fatalf("serializer calls=%d", v.calls)
				}
				if len(r.named("mcp_tool_called")) != 1 || r.named("mcp_tool_called")[0].Properties["tool_status"] != "success" {
					t.Fatalf("events=%v", r.named("mcp_tool_called"))
				}
			})
		}
	}
}

func waitRound1Closed(t *testing.T, r *mcpbridge.Responses) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for r.Await("probe", func(map[string]any) {}) {
		r.Fail("probe")
		if time.Now().After(deadline) {
			t.Fatal("observer did not close")
		}
		time.Sleep(time.Millisecond)
	}
}
