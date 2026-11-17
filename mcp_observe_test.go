package rudderstackai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// connectMaybeObserved connects a client over pipes, the server side an IOTransport wrapped
// with ObserveMCPTransport when observe is set.
func connectMaybeObserved(t *testing.T, server *mcp.Server, observe bool) *mcp.ClientSession {
	t.Helper()
	c1, c2 := net.Pipe()
	return connectOver(t, server, &mcp.IOTransport{Reader: c2, Writer: c2}, &mcp.IOTransport{Reader: c1, Writer: c1}, observe)
}

func connectOver(t *testing.T, server *mcp.Server, st, ct mcp.Transport, observe bool) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	if observe {
		ctx, st = ObserveMCPTransport(ctx, st)
	}
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(testClientInfo, nil).Connect(context.Background(), ct, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	return cs
}

func failingServer(name string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Text string }
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return nil, &jsonrpc.Error{Code: -32602, Message: args.Text}
	})
	return s
}

func TestObserveMCPTransportConcurrentIDs(t *testing.T) {
	setupAI(t)
	s := failingServer("concurrent-errors-mcp")
	r := wrap(t, s, Options{CaptureContent: true})
	cs := connectMaybeObserved(t, s, true)
	var wg sync.WaitGroup
	for _, text := range []string{"first failure", "second failure", "third failure", "fourth failure"} {
		wg.Go(func() {
			if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": text}}); err == nil {
				t.Error("expected error")
			}
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, e := range r.wait(t, "mcp_tool_called", 4) {
		seen[e.Properties["error_message"].(string)] = true
	}
	if len(seen) != 4 {
		t.Errorf("messages not matched by id: %v", seen)
	}
}

type reusedResponseWriter struct {
	net.Conn
	started chan struct{}
	release chan struct{}
}

func (w reusedResponseWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "first failure") {
		close(w.started)
		<-w.release
	}
	return w.Conn.Write(p)
}

func TestObserveMCPTransportReusedResponseSnapshot(t *testing.T) {
	for _, source := range []string{"rpc", "result"} {
		t.Run(source, func(t *testing.T) {
			setupAI(t)
			text := &mcp.TextContent{Text: "first failure"}
			result := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{text}, StructuredContent: map[string]any{"errorType": "cancelled"}}
			rpc := &jsonrpc.Error{Code: -32602, Message: "first failure"}
			handled := make(chan string, 2)
			s := mcp.NewServer(&mcp.Implementation{Name: "reuse", Version: "1"}, nil)
			s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				handled <- ctx.Value(reqCtxKey{}).(*reqCtx).requestID
				if source == "rpc" {
					return nil, rpc
				}
				return result, nil
			})
			r := wrap(t, s, Options{CaptureContent: true, ClassifyToolError: func(_ string, got *mcp.CallToolResult) string {
				return got.StructuredContent.(map[string]any)["errorType"].(string)
			}})
			c1, c2 := net.Pipe()
			w := reusedResponseWriter{Conn: c2, started: make(chan struct{}), release: make(chan struct{})}
			cs := connectOver(t, s, &mcp.IOTransport{Reader: c2, Writer: w}, &mcp.IOTransport{Reader: c1, Writer: c1}, true)
			release := sync.OnceFunc(func() { close(w.release) })
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup"})
			}()
			select {
			case <-w.started:
			case <-ctx.Done():
				t.Fatal("first write never started")
			}
			if source == "rpc" && len(r.named("mcp_tool_called")) != 0 {
				t.Fatal("event before successful write")
			}
			// The first response is serialized; reuse its source while that write is held.
			firstID := <-handled
			text.Text, rpc.Message = "second failure", "second failure"
			result.StructuredContent.(map[string]any)["errorType"] = "api_error"
			second := make(chan struct{})
			go func() {
				defer close(second)
				_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup"})
			}()
			var secondID string
			select {
			case secondID = <-handled:
			case <-ctx.Done():
				t.Fatal("second request never handled")
			}
			release()
			<-done
			<-second
			events := r.wait(t, "mcp_tool_called", 2)
			seen := map[any]any{}
			for _, e := range events {
				seen[e.Properties["request_id"]] = e.Properties["error_message"]
				if source == "result" {
					expected := "api_error"
					if e.Properties["request_id"] == firstID {
						expected = "cancelled"
					}
					if e.Properties["error_type"] != expected {
						t.Fatalf("classification: %v", e.Properties)
					}
				}
			}
			var firstMessage any = "first failure"
			if source == "result" {
				firstMessage = nil
			}
			if len(seen) != 2 || seen[firstID] != firstMessage || seen[secondID] != "second failure" {
				t.Fatalf("request messages: %v", seen)
			}
		})
	}
}

// The function field cannot be serialized without MarshalJSON, like Node's toJSON case.
type snapshotErrorData struct{ Unserializable func() }

func (snapshotErrorData) MarshalJSON() ([]byte, error) {
	return []byte(`{"detail":"custom data"}`), nil
}

type snapshotResponseWriter struct {
	reusedResponseWriter
	fail bool
}

func (w snapshotResponseWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "first failure") && w.fail {
		close(w.started)
		<-w.release
		return 0, errors.New("write failed")
	}
	return w.reusedResponseWriter.Write(p)
}

func TestObserveMCPTransportCustomMarshalerSnapshot(t *testing.T) {
	for _, source := range []string{"rpc", "result"} {
		for _, fail := range []bool{false, true} {
			t.Run(source+map[bool]string{false: "/written", true: "/write-failed"}[fail], func(t *testing.T) {
				setupAI(t)
				// go-sdk requires RawMessage for error data, so applications marshal it first.
				data, err := json.Marshal(snapshotErrorData{Unserializable: func() {}})
				if err != nil {
					t.Fatal(err)
				}
				s := mcp.NewServer(&mcp.Implementation{Name: "custom-data", Version: "1"}, nil)
				s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					if source == "rpc" {
						return nil, &jsonrpc.Error{Code: -32602, Message: "first failure", Data: data}
					}
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
						&mcp.ImageContent{Data: []byte("image"), MIMEType: "image/png"},
						&mcp.TextContent{Text: "first failure"},
					}}, nil
				})
				r := wrap(t, s, Options{CaptureContent: true})
				c1, c2 := net.Pipe()
				w := snapshotResponseWriter{reusedResponseWriter: reusedResponseWriter{Conn: c2, started: make(chan struct{}), release: make(chan struct{})}, fail: fail}
				cs := connectOver(t, s, &mcp.IOTransport{Reader: c2, Writer: w}, &mcp.IOTransport{Reader: c1, Writer: c1}, true)
				release := sync.OnceFunc(func() { close(w.release) })
				defer release()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup"})
					if err == nil && (result == nil || !result.IsError) {
						err = errors.New("expected isError result")
					}
					done <- err
				}()
				select {
				case <-w.started:
				case <-ctx.Done():
					t.Fatal("response write never started")
				}
				if len(r.named("mcp_tool_called")) != 0 {
					t.Fatal("event before write completed")
				}
				release()
				callErr := <-done
				if fail || source == "rpc" {
					if callErr == nil {
						t.Fatal("expected call error")
					}
				} else if callErr != nil {
					t.Fatal(callErr)
				}
				if !fail && source == "rpc" {
					var rpc *jsonrpc.Error
					if !errors.As(callErr, &rpc) || string(rpc.Data) != string(data) {
						t.Fatalf("custom data missing from wire error: %v", callErr)
					}
				}
				r.wait(t, "mcp_tool_called", 1)
				_ = cs.Close() // Closing observation must not emit the held event again.
				events := r.named("mcp_tool_called")
				if len(events) != 1 {
					t.Fatalf("got %d tool events, want exactly one", len(events))
				}
				p := events[0].Properties
				if p["tool_status"] != "error" {
					t.Fatalf("status: %v", p)
				}
				message, present := p["error_message"]
				if fail && present || !fail && (!present || message != "first failure") {
					t.Fatalf("error_message after write (failed=%v): %v", fail, p)
				}
			})
		}
	}
}

func TestObserveMCPTransportDeclaredEventFollows(t *testing.T) {
	setupAI(t)
	s := failingServer("declared-errors-mcp")
	r := wrap(t, s, Options{CaptureContent: true, EventRules: EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "lookup_failed"}}}})
	cs := connectMaybeObserved(t, s, true)
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "boom"}}); err == nil {
		t.Fatal("expected error")
	}
	r.wait(t, "lookup_failed", 1)
	names := r.names()
	if len(names) < 2 || names[len(names)-2] != "mcp_tool_called" || names[len(names)-1] != "lookup_failed" {
		t.Errorf("order: %v", names)
	}
	if r.props(t, "mcp_tool_called")["error_message"] != "boom" {
		t.Errorf("props: %v", r.props(t, "mcp_tool_called"))
	}
}

// failingWriter fails every write of an error response.
type failingWriter struct{ net.Conn }

func (w failingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `"error"`) {
		return 0, errors.New("write failed")
	}
	return w.Conn.Write(p)
}

func TestObserveMCPTransportFailedWriteSendsWithoutMessage(t *testing.T) {
	setupAI(t)
	s := failingServer("failed-write-mcp")
	r := wrap(t, s, Options{CaptureContent: true})
	c1, c2 := net.Pipe()
	cs := connectOver(t, s, &mcp.IOTransport{Reader: c2, Writer: failingWriter{c2}}, &mcp.IOTransport{Reader: c1, Writer: c1}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "never sent"}}); err == nil {
		t.Fatal("expected the call to fail")
	}
	p := r.wait(t, "mcp_tool_called", 1)[0].Properties
	if p["tool_status"] != "error" {
		t.Errorf("status: %v", p)
	}
	if _, ok := p["error_message"]; ok {
		t.Errorf("error_message without a delivered response: %v", p)
	}
}

// Observing keeps go-sdk's own connection, so a JSON-RPC batch is still refused after the
// session negotiates 2025-06-18.
func TestObserveMCPTransportKeepsBatchRefusal(t *testing.T) {
	for _, observe := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "observed"}[observe], func(t *testing.T) {
			setupAI(t)
			s := failingServer("batch-mcp")
			wrap(t, s, Options{CaptureContent: true})
			client, server := net.Pipe()
			ctx := context.Background()
			var st mcp.Transport = &mcp.IOTransport{Reader: server, Writer: server}
			if observe {
				ctx, st = ObserveMCPTransport(ctx, st)
			}
			ss, err := s.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ss.Close(); _ = client.Close() })
			lines := bufio.NewScanner(client)
			send := func(msg string) {
				t.Helper()
				_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if _, err := io.WriteString(client, msg+"\n"); err != nil {
					t.Fatal(err)
				}
			}
			send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			if !lines.Scan() || !strings.Contains(lines.Text(), `"protocolVersion":"2025-06-18"`) {
				t.Fatalf("initialize: %q %v", lines.Text(), lines.Err())
			}
			send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			send(`[{"jsonrpc":"2.0","id":2,"method":"ping"},{"jsonrpc":"2.0","id":3,"method":"ping"}]`)
			_ = client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			if lines.Scan() {
				t.Fatalf("batch answered: %q", lines.Text())
			}
		})
	}
}

func TestObserveMCPTransportCloseFlushes(t *testing.T) {
	setupAI(t)
	s := workspaceServer("close-errors-mcp", nil)
	r := wrap(t, s, Options{CaptureContent: true})
	w, _ := instances.get(s)
	responses := mcpbridge.NewResponses()
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: int64(3), Responses: responses})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{ErrorType: "api_error"})
	if len(r.named("mcp_tool_called")) != 0 {
		t.Fatal("sent before the response")
	}
	responses.Close()
	if _, ok := r.props(t, "mcp_tool_called")["error_message"]; ok {
		t.Error("error_message without a response")
	}
}

// The response a held event receives yields the fixture's expected text.
func TestObserveMCPTransportFixtureParity(t *testing.T) {
	data, err := os.ReadFile("spec/fixtures/mcp-error-message-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name          string
			Result, Error map[string]any
			Expected      *string
			Write         string
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			setupAI(t)
			s := workspaceServer("fixture-errors-mcp", nil)
			r := wrap(t, s, Options{CaptureContent: true})
			w, _ := instances.get(s)
			responses := mcpbridge.NewResponses()
			rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: "fixture", Responses: responses})
			rc.ToolCall("list_destinations", nil, false, nil, "")
			rc.ToolDone(mcpbridge.Outcome{ErrorType: "api_error"})
			msg := map[string]any{"jsonrpc": "2.0", "id": "fixture"}
			if tc.Error != nil {
				msg["error"] = tc.Error
			} else {
				msg["result"] = tc.Result
			}
			b, _ := json.Marshal(msg)
			out := io.Discard
			if tc.Write == "fails" {
				out = fixtureFailingWriter{}
			}
			_, _ = mcpbridge.NewWriter(out, responses).Write(append(b, '\n'))
			want := ""
			if tc.Expected != nil {
				want = *tc.Expected
			}
			if got, _ := r.props(t, "mcp_tool_called")["error_message"].(string); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestObserveMCPTransportUnsupportedUnchanged(t *testing.T) {
	ctx := context.Background()
	_, inMemory := mcp.NewInMemoryTransports()
	for _, tr := range []mcp.Transport{nil, inMemory, &mcp.StreamableServerTransport{}, (*mcp.IOTransport)(nil), &mcp.IOTransport{}} {
		gotCtx, got := ObserveMCPTransport(ctx, tr)
		if got != tr || gotCtx != ctx {
			t.Errorf("%T changed", tr)
		}
	}
	stdio := &mcp.StdioTransport{MaxLineLength: 42}
	_, got := ObserveMCPTransport(ctx, stdio)
	if io, ok := got.(*mcp.IOTransport); !ok || io.MaxLineLength != 42 {
		t.Errorf("stdio: %#v", got)
	}
}

// A flush (response, close or timeout) racing a later event of the same request still sends
// the held event first.
func TestHeldEventsKeepOrderDuringFlush(t *testing.T) {
	setupAI(t)
	s := workspaceServer("order-mcp", nil)
	wrap(t, s, Options{CaptureContent: true})
	w, _ := instances.get(s)
	responses := mcpbridge.NewResponses()
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: int64(9), Responses: responses})
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}
	started, release := make(chan struct{}), make(chan struct{})
	rc.send("mcp_tool_called", map[string]any{"tool_status": "error", "error_type": "api_error"}, func() {
		close(started)
		<-release
		record("mcp_tool_called")
	})
	go responses.Close()
	<-started
	done := make(chan struct{})
	go func() {
		defer close(done)
		rc.send("lookup_failed", map[string]any{"tool_status": "error"}, func() { record("lookup_failed") })
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "mcp_tool_called" {
		t.Errorf("order: %v", order)
	}
}

func TestObserveMCPStdioFailedWriteSendsWithoutMessage(t *testing.T) {
	setupAI(t)
	s := workspaceServer("stdio-write-mcp", nil)
	r := wrap(t, s, Options{CaptureContent: true})
	w, _ := instances.get(s)
	closed, err := os.CreateTemp(t.TempDir(), "closed-output")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	saved := os.Stdout
	os.Stdout = closed
	ctx, transport := ObserveMCPTransport(context.Background(), &mcp.StdioTransport{})
	os.Stdout = saved
	responses := mcpbridge.ResponsesFrom(ctx)
	rc := w.begin(mcpbridge.Request{Ctx: ctx, RequestID: int64(1), Responses: responses})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{ErrorType: "api_error"})
	if len(r.named("mcp_tool_called")) != 0 {
		t.Fatal("sent before write")
	}
	_, err = transport.(*mcp.IOTransport).Writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"never written"}}` + "\n"))
	if err == nil {
		t.Fatal("expected closed-file write error")
	}
	if _, ok := r.props(t, "mcp_tool_called")["error_message"]; ok {
		t.Fatal("text from failed stdio write")
	}
}

func TestHeldFieldSnapshot(t *testing.T) {
	fields := []string{"error.code", "error.message", "error.data", "isError", "resultType", "content.text", "content.type", "content.size", "structuredContent", "nextCursor", "structured.nextCursor", "structured.next_cursor", "meta.status", "structured.status", "structured.statusCode", "classifier.input", "classifier.output", "invalid_arguments", "declared.when", "declared.properties", "declared.output", "declared.content", "request.identity", "inputRequests", "inputRequests.key", "inputRequests.method", "requestState"}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			setupAI(t)
			output := map[string]any{"nested": map[string]any{"value": "first"}}
			seen := []any{}
			s := mcp.NewServer(&mcp.Implementation{Name: "field-snapshot", Version: "1"}, nil)
			r := wrap(t, s, Options{CaptureContent: true, EventRules: EventRules{Tools: map[string]ToolEventRule{"lookup": {
				Event: "lookup_failed",
				When: func(args map[string]any, _ any) bool {
					return field != "declared.when" || args["value"].(map[string]any)["valid"] == false
				},
				Properties: func(args map[string]any, value any) map[string]any {
					seen = append(seen, args["value"].(map[string]any)["valid"])
					return output
				},
			}}}})
			w, _ := instances.get(s)
			responses := mcpbridge.NewResponses()
			schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "object", "const": map[string]any{"valid": true}}}}
			request := mcpbridge.Request{Ctx: context.Background(), RequestID: 1, Responses: responses, RegisteredTools: []mcpbridge.Tool{{Name: "lookup", Schema: schema}}}
			rc := w.begin(request)
			args := map[string]json.RawMessage{"value": json.RawMessage(`{"valid":false}`)}
			rc.ToolCall("lookup", args, false, nil, "")
			block := map[string]any{"type": "text", "text": "first failure"}
			sc := map[string]any{"label": "rate_limit"}
			result := map[string]any{"isError": true, "resultType": "complete", "content": []any{block}, "structuredContent": sc}
			if field == "nextCursor" {
				result["nextCursor"] = "first"
			}
			if field == "structured.nextCursor" {
				sc["nextCursor"] = "first"
			}
			if field == "structured.next_cursor" {
				sc["next_cursor"] = "first"
			}
			if field == "meta.status" {
				result["_meta"] = map[string]any{"com.rudderstack/http_status": 429}
			}
			if field == "structured.status" {
				sc["status"] = 429
			}
			if field == "structured.statusCode" {
				sc["statusCode"] = 429
			}
			if field == "declared.content" {
				delete(result, "structuredContent")
				block["text"] = `{"label":"rate_limit","message":"first failure"}`
			}
			b, _ := json.Marshal(result)
			outcome := mcpbridge.Outcome{Result: result}
			if strings.HasPrefix(field, "classifier.") {
				outcome.ClassifiedError = "rate_limit"
			}
			rpc := map[string]any{"code": -32602, "message": "first failure", "data": map[string]any{"unused": "first"}}
			response := map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}
			if strings.HasPrefix(field, "error.") {
				outcome = mcpbridge.Outcome{ErrorType: "invalid_request"}
				delete(response, "result")
				response["error"] = rpc
			}
			inputField := strings.HasPrefix(field, "inputRequests") || field == "requestState"
			if inputField {
				result["resultType"] = "input_required"
				result["inputRequests"] = map[string]any{"first": map[string]any{"method": "elicitation/create"}}
				result["requestState"] = "first-state"
				outcome = mcpbridge.Outcome{InputRequired: true, InputKeys: []string{"first"}, RequestState: "first-state"}
			}
			rc.ToolDone(outcome)
			wire, _ := json.Marshal(response)
			writer := reusedResponseWriter{started: make(chan struct{}), release: make(chan struct{})}
			// Use a pipe only for the blocking writer, with its receiver already draining.
			c1, c2 := net.Pipe()
			writer.Conn = c2
			defer c1.Close()
			defer c2.Close()
			go func() { _, _ = io.Copy(io.Discard, c1) }()
			done := make(chan error, 1)
			go func() { _, err := mcpbridge.NewWriter(writer, responses).Write(append(wire, '\n')); done <- err }()
			select {
			case <-writer.started:
			case <-time.After(5 * time.Second):
				t.Fatal("write never started")
			}
			if len(r.named("mcp_tool_called")) != 0 {
				t.Fatal("event before write settled")
			}
			switch field {
			case "inputRequests":
				result["inputRequests"] = map[string]any{}
			case "inputRequests.key":
				outcome.InputKeys[0] = "second"
			case "inputRequests.method":
				result["inputRequests"].(map[string]any)["first"].(map[string]any)["method"] = "changed"
			case "requestState":
				outcome.RequestState = "second-state"
			case "error.code":
				rpc["code"] = -32603
			case "error.message":
				rpc["message"] = "second failure"
			case "error.data":
				rpc["data"].(map[string]any)["unused"] = "second"
			case "isError":
				result["isError"] = false
			case "resultType":
				result["resultType"] = "input_required"
				result["inputRequests"] = map[string]any{"changed": map[string]any{"method": "elicitation/create"}}
			case "content.text":
				block["text"] = "second failure"
			case "content.type":
				block["type"] = "image"
			case "content.size":
				result["content"] = append(result["content"].([]any), map[string]any{"type": "image", "data": strings.Repeat("x", 200)})
			case "structuredContent":
				result["structuredContent"] = map[string]any{"label": "auth_error"}
			case "nextCursor":
				delete(result, "nextCursor")
			case "structured.nextCursor":
				delete(sc, "nextCursor")
			case "structured.next_cursor":
				delete(sc, "next_cursor")
			case "meta.status":
				result["_meta"].(map[string]any)["com.rudderstack/http_status"] = 401
			case "structured.status":
				sc["status"] = 401
			case "structured.statusCode":
				sc["statusCode"] = 401
			case "classifier.input":
				sc["label"] = "auth_error"
			case "classifier.output":
				outcome.ClassifiedError = "auth_error"
			case "invalid_arguments", "declared.when", "declared.properties":
				copy(args["value"], []byte(`{"valid":true }`))
			case "declared.content":
				block["text"] = `{"label":"auth_error","message":"second failure"}`
			case "declared.output":
				output["nested"].(map[string]any)["value"] = "second"
			case "request.identity":
				request.RequestID = "second"
				request.RegisteredTools[0].Name = "changed"
			}
			close(writer.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if inputField {
				pending, ok := rc.st.inputRequired.get(rc.inputKey("first", "first-state"))
				if !ok || pending.requestID != "1" || len(r.named("mcp_tool_called")) != 0 {
					t.Fatalf("input snapshot: %+v", pending)
				}
				return
			}
			p := r.props(t, "mcp_tool_called")
			wantKind := "invalid_request"
			if strings.Contains(field, "status") || strings.HasPrefix(field, "classifier.") {
				wantKind = "rate_limit"
			}
			wantMessage := "first failure"
			if field == "declared.content" {
				wantMessage = `{"label":"rate_limit","message":"first failure"}`
			}
			if p["request_id"] != "1" || p["tool_name"] != "lookup" || p["tool_status"] != "error" || p["error_type"] != wantKind || p["error_message"] != wantMessage {
				t.Fatalf("completion: %v", p)
			}
			if !strings.HasPrefix(field, "error.") {
				if p["is_error_result"] != true || p["result_size_bytes"] != len(b) || p["result_tokens_estimate"] != len(b)/4 || p["result_paginated"] != (strings.Contains(field, "Cursor") || field == "structured.next_cursor") {
					t.Fatalf("stats: %v", p)
				}
				wantTypes := `["text","structured"]`
				if field == "declared.content" {
					wantTypes = `["text"]`
				}
				if mustJSON(p["result_content_types"]) != wantTypes || mustJSON(p["invalid_arguments"]) != `["value"]` {
					t.Fatalf("types/arguments: %v", p)
				}
			}
			if mustJSON(seen) != `[false]` || mustJSON(r.props(t, "lookup_failed")["nested"]) != `{"value":"first"}` {
				t.Fatalf("declared: seen=%v properties=%v", seen, r.props(t, "lookup_failed"))
			}
		})
	}
}

func TestObserveMCPTransportNonToolIsError(t *testing.T) {
	for _, method := range []string{"resources/read", "prompts/get"} {
		for _, held := range []bool{false, true} {
			t.Run(method+map[bool]string{false: "/immediate", true: "/held"}[held], func(t *testing.T) {
				setupAI(t)
				s := workspaceServer("non-tool-is-error", nil)
				r := wrap(t, s, Options{CaptureContent: true})
				w, _ := instances.get(s)
				responses := mcpbridge.NewResponses()
				defer responses.Close()
				rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1, Responses: responses})
				result := map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "delivered failure"}}}
				body, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				event := "mcp_resource_read"
				if method == "prompts/get" {
					event = "mcp_prompt_used"
				}
				if held {
					rc.Finished(method, "lookup", 0, "api_error")
				}
				rc.Finished(method, "lookup", len(body), "")
				if held && len(r.named(event)) != 0 {
					t.Fatal("event before response write")
				}
				message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := mcpbridge.NewWriter(io.Discard, responses).Write(append(message, '\n')); err != nil {
					t.Fatal(err)
				}
				count := 1
				if held {
					count = 2
				}
				events := r.wait(t, event, count)
				for _, e := range events {
					if _, ok := e.Properties["error_message"]; ok {
						t.Fatalf("non-tool error_message: %v", e.Properties)
					}
				}
				p := events[len(events)-1].Properties
				if p["tool_status"] != "success" || p["result_size_bytes"] != len(body) {
					t.Fatalf("success properties: %v", p)
				}
			})
		}
	}
}

type serializerSequence struct{ calls *atomic.Int32 }

func (s serializerSequence) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, `{"sequence":%d}`, s.calls.Add(1)), nil
}

func TestObserveMCPTransportPreservesSerializerOrder(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, outcome := range []string{"rpc", "isError", "success"} {
			t.Run(fmt.Sprintf("capture=%v/%s", capture, outcome), func(t *testing.T) {
				setupAI(t)
				var calls atomic.Int32
				s := mcp.NewServer(&mcp.Implementation{Name: "serializer-order", Version: "1"}, nil)
				s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					if outcome == "rpc" {
						data, err := json.Marshal(serializerSequence{&calls})
						if err != nil {
							return nil, err
						}
						return nil, &jsonrpc.Error{Code: -32603, Message: "first failure", Data: data}
					}
					return &mcp.CallToolResult{IsError: outcome == "isError", Content: []mcp.Content{&mcp.TextContent{Text: "first failure"}}, StructuredContent: serializerSequence{&calls}}, nil
				})
				wrap(t, s, Options{CaptureContent: capture})
				cs := connectMaybeObserved(t, s, true)
				result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup"})
				if outcome == "rpc" {
					var rpc *jsonrpc.Error
					if !errors.As(err, &rpc) || string(rpc.Data) != `{"sequence":1}` {
						t.Fatalf("RPC response: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					// Main marshals once for result stats, then the transport marshals.
					if result.StructuredContent.(map[string]any)["sequence"] != float64(2) {
						t.Fatalf("client result: %#v", result.StructuredContent)
					}
				}
				want := int32(2)
				if outcome == "rpc" {
					want = 1
				}
				if calls.Load() != want {
					t.Fatalf("serializer calls=%d, want %d", calls.Load(), want)
				}
			})
		}
	}
}
