package rudderstackai

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

type round1PanicWriter struct{ calls int }

func (w *round1PanicWriter) Write(p []byte) (int, error) { return testkit.PanicWrite(&w.calls, p) }

func (*round1PanicWriter) Close() error { return nil }

func TestRound1ObservedWriterPanicSiblings(t *testing.T) {
	for _, site := range []string{"io", "stdio"} {
		t.Run(site, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var tr mcp.Transport = &mcp.IOTransport{Reader: io.NopCloser(strings.NewReader("")), Writer: &round1PanicWriter{}}
			if site == "stdio" {
				tr = &mcp.StdioTransport{}
			}
			_, observed := ObserveMCPTransport(ctx, tr)
			writer := observed.(*mcp.IOTransport).Writer
			if site == "stdio" {
				// Stdio wraps os.Stdout in a nopCloser. Inject its application writer at that boundary.
				if _, ok := writer.(*mcpbridge.Writer); !ok {
					t.Fatal("stdio did not install bridge writer")
				}
				writer = mcpbridge.NewWriter(nopCloser{&round1PanicWriter{}}, mcpbridge.NewResponses())
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Error("expected panic")
					}
				}()
				_, _ = writer.Write([]byte("first\n"))
			}()
			done := make(chan struct{})
			go func() { _, _ = writer.Write([]byte("second\n")); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("writer remained locked")
			}
		})
	}
}

type round1FailWriter struct{}

func (round1FailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (round1FailWriter) Close() error              { return nil }

// An observer that sees no write for a result captures it as without an observer: base's outcome
// and size, one serializer run. Only a write that failed captures no content.
func TestRound1MCPRejectedObserverSiblings(t *testing.T) {
	for _, reason := range []string{"closed", "invalid_id", "duplicate_id", "context_before", "context_after", "connection_before", "connection_after", "write_failed"} {
		t.Run(reason, func(t *testing.T) {
			setupAI(t)
			s := workspaceServer("rejected", nil)
			r := wrap(t, s, Options{CaptureContent: true})
			w, _ := instances.get(s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var writer io.WriteCloser = &round1PanicWriter{calls: 1}
			if reason == "write_failed" {
				writer = round1FailWriter{}
			}
			ctx, tr := ObserveMCPTransport(ctx, &mcp.IOTransport{Reader: io.NopCloser(strings.NewReader("")), Writer: writer})
			responses := mcpbridge.ResponsesFrom(ctx)
			defer responses.Close()
			id := any(1)
			switch reason {
			case "closed":
				responses.Close()
			case "invalid_id":
				id = nil
			case "duplicate_id":
				responses.Await(id, func(map[string]any) {})
			case "context_before":
				cancel()
				waitRound1ObserverClosed(t, responses)
			case "connection_before":
				_ = tr.(*mcp.IOTransport).Reader.Close()
			}
			rc := w.begin(mcpbridge.Request{Ctx: ctx, RequestID: id, Responses: responses})
			rc.ToolCall("list_destinations", nil, false, nil, "")
			v := &review84Count{}
			rc.ToolDone(mcpbridge.Outcome{Result: v})
			if reason == "context_after" {
				cancel()
				waitRound1ObserverClosed(t, responses)
			}
			if reason == "connection_after" {
				_ = tr.(*mcp.IOTransport).Writer.Close()
			}
			if reason == "write_failed" {
				_, _ = tr.(*mcp.IOTransport).Writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}` + "\n"))
			}
			r.wait(t, "mcp_tool_called", 1)
			want := 1
			if reason == "write_failed" {
				want = 0
			}
			if v.Calls != want {
				t.Fatalf("serializer calls=%d want %d", v.Calls, want)
			}
			if p := r.props(t, "mcp_tool_called"); p["tool_status"] != "success" || p["result_size_bytes"] != len(`{"content":[]}`) || p["error_type"] != nil {
				t.Fatal(fmt.Sprint(p))
			}
		})
	}
}

func waitRound1ObserverClosed(t *testing.T, r *mcpbridge.Responses) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for r.Await("closed-probe", func(map[string]any) {}) {
		r.Fail("closed-probe")
		if time.Now().After(deadline) {
			t.Fatal("observer did not close")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRound1GoSDKClosedObserverHandlerCompletion(t *testing.T) {
	for _, closure := range []string{"context", "connection"} {
		t.Run(closure, func(t *testing.T) {
			setupAI(t)
			s := workspaceServer("closure-handler", nil)
			r := wrap(t, s, Options{CaptureContent: true})
			w, _ := instances.get(s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed, tr := ObserveMCPTransport(ctx, &mcp.IOTransport{Reader: io.NopCloser(strings.NewReader("")), Writer: &round1PanicWriter{calls: 1}})
			responses := mcpbridge.ResponsesFrom(observed)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			v := &review84Count{}
			handler := w.receiving(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				// Direct middleware invocation bypasses go-sdk's private request ID context key.
				ctx.Value(reqCtxKey{}).(*reqCtx).rawID = 1
				close(entered)
				<-release
				return &mcp.CallToolResult{StructuredContent: v}, nil
			})
			go func() {
				defer close(done)
				_, _ = handler(observed, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "list_destinations"}})
			}()
			<-entered
			if closure == "context" {
				cancel()
				waitRound1ObserverClosed(t, responses)
			} else {
				_ = tr.(*mcp.IOTransport).Reader.Close()
			}
			close(release)
			<-done
			// Closed before the handler returned: captured once, as without an observer.
			if v.Calls != 1 {
				t.Fatalf("serializer calls=%d", v.Calls)
			}
			if len(r.named("mcp_tool_called")) != 1 || r.props(t, "mcp_tool_called")["tool_status"] != "success" {
				t.Fatalf("events=%v", r.named("mcp_tool_called"))
			}
		})
	}
}

func TestRound1MCPObserverAdmissionRace(t *testing.T) {
	setupAI(t)
	s := workspaceServer("admission-race", nil)
	r := wrap(t, s, Options{CaptureContent: true})
	w, _ := instances.get(s)
	for n := range 300 {
		responses := mcpbridge.NewResponses()
		rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: n, Responses: responses})
		rc.ToolCall("list_destinations", nil, false, nil, "")
		start, closed := make(chan struct{}), make(chan struct{})
		go func() { <-start; responses.Close(); close(closed) }()
		v := &review84Count{}
		close(start)
		rc.ToolDone(mcpbridge.Outcome{Result: v})
		<-closed
		// Rejected or released by the close, the result is captured once, as without an observer.
		if v.Calls != 1 {
			t.Fatalf("iteration=%d calls=%d", n, v.Calls)
		}
	}
	if got := len(r.named("mcp_tool_called")); got != 300 {
		t.Fatalf("events=%d", got)
	}
	for _, e := range r.named("mcp_tool_called") {
		if e.Properties["tool_status"] != "success" {
			t.Fatalf("event=%v", e.Properties)
		}
	}
}
