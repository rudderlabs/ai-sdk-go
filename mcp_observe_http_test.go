package rudderstackai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var streamableModes = []struct {
	name string
	opts *mcp.StreamableHTTPOptions
}{
	{"stateful sse", nil},
	{"stateful json", &mcp.StreamableHTTPOptions{JSONResponse: true}},
	{"stateless", &mcp.StreamableHTTPOptions{Stateless: true}},
}

// errorServer fails lookup with its text argument: as a protocol error when protocol is set,
// else as an isError result.
func errorServer(name string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Text     string
			Protocol bool
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		if args.Protocol {
			return nil, &jsonrpc.Error{Code: -32602, Message: args.Text}
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: args.Text}}}, nil
	})
	return s
}

// serveStreamable serves s with the go-sdk's StreamableHTTPHandler, behind the token check
// apps typically put in front of it, wrapped by wrapHandler.
func serveStreamable(t *testing.T, s *mcp.Server, opts *mcp.StreamableHTTPOptions, wrapHandler func(http.Handler) http.Handler) string {
	t.Helper()
	verify := func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: "oauth|8812", Expiration: time.Now().Add(time.Hour)}, nil
	}
	h := auth.RequireBearerToken(verify, nil)(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, opts))
	ts := httptest.NewServer(wrapHandler(h))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestObserveMCPHTTPErrorMessage(t *testing.T) {
	long := "reach jane.doe@example.com " + strings.Repeat("x", 300)
	for _, mode := range streamableModes {
		for _, tc := range []struct {
			name     string
			protocol bool
			text     string
		}{
			{"is error result", false, "lookup failed"},
			{"protocol error", true, "bad lookup"},
			{"masked and capped", false, long},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				setupAI(t)
				s := errorServer("observe-http-mcp")
				r := wrap(t, s, Options{ContentCapture: &ContentCapture{ErrorMessage: Ptr(true)}})
				cs := connectHTTP(t, serveStreamable(t, s, mode.opts, ObserveMCPHTTP), "2025-06-18")
				_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": tc.text, "protocol": tc.protocol}})
				source := map[string]any{"result": map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": tc.text}}}}
				if tc.protocol {
					source = map[string]any{"error": map[string]any{"message": tc.text}}
				}
				want := errorMessage(source, MaskText)
				if want == "" || strings.Contains(want, "jane.doe") {
					t.Fatalf("bad expectation %q", want)
				}
				p := r.wait(t, "mcp_tool_called", 1)[0].Properties
				if p["error_message"] != want {
					t.Errorf("error_message=%v, want %q", p["error_message"], want)
				}
			})
		}
	}
}

func TestObserveMCPHTTPRequiredForErrorMessage(t *testing.T) {
	for _, mode := range streamableModes {
		t.Run(mode.name, func(t *testing.T) {
			setupAI(t)
			s := errorServer("unobserved-http-mcp")
			r := wrap(t, s, Options{CaptureContent: true})
			cs := connectHTTP(t, serveStreamable(t, s, mode.opts, func(h http.Handler) http.Handler { return h }), "2025-06-18")
			_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "lookup failed"}})
			p := r.wait(t, "mcp_tool_called", 1)[0].Properties
			if p["tool_status"] != "error" {
				t.Fatalf("props: %v", p)
			}
			if _, ok := p["error_message"]; ok {
				t.Errorf("error_message without ObserveMCPHTTP: %v", p)
			}
		})
	}
}

// A middleware between the wrapper and the handler that deep-copies the headers breaks the
// match: the event goes out at once, without error_message.
func TestObserveMCPHTTPClonedHeaders(t *testing.T) {
	setupAI(t)
	s := errorServer("cloned-http-mcp")
	r := wrap(t, s, Options{CaptureContent: true})
	cloning := func(h http.Handler) http.Handler {
		return ObserveMCPHTTP(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			h.ServeHTTP(w, req.Clone(req.Context()))
		}))
	}
	cs := connectHTTP(t, serveStreamable(t, s, nil, cloning), "2025-06-18")
	start := time.Now()
	_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "lookup failed"}})
	p := r.wait(t, "mcp_tool_called", 1)[0].Properties
	if _, ok := p["error_message"]; ok {
		t.Errorf("error_message through a cloned request: %v", p)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("event held for %v", time.Since(start))
	}
}

// A middleware that hands several requests one header map makes them indistinguishable: neither
// is observed, even the first, whose call found its observer before the second arrived.
func TestObserveMCPHTTPSharedHeaderMap(t *testing.T) {
	setupAI(t)
	s := mcp.NewServer(&mcp.Implementation{Name: "shared-header-mcp", Version: "1"}, nil)
	started := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	release := make(chan struct{})
	for name, ch := range started {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			close(ch)
			<-release
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: name + " failed"}}}, nil
		})
	}
	r := wrap(t, s, Options{CaptureContent: true})
	var mu sync.Mutex
	var shared http.Header
	sharing := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			if shared == nil {
				shared = req.Header
			}
			req.Header = shared
			mu.Unlock()
			h.ServeHTTP(w, req)
		})
	}
	url := serveStreamable(t, s, &mcp.StreamableHTTPOptions{Stateless: true}, func(h http.Handler) http.Handler { return sharing(ObserveMCPHTTP(h)) })
	var wg sync.WaitGroup
	post := func(name string) {
		wg.Go(func() {
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
			req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
			req.Header.Set("Authorization", "Bearer tok_secret")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = res.Body.Close()
		})
	}
	post("a")
	<-started["a"] // a's call already holds its observer
	post("b")
	<-started["b"] // b's request has registered the same map
	close(release)
	wg.Wait()
	for _, e := range r.wait(t, "mcp_tool_called", 2) {
		if msg, ok := e.Properties["error_message"]; ok {
			t.Errorf("%v observed through a shared header map: %q", e.Properties["tool_name"], msg)
		}
	}
}

func TestObserveMCPHTTPNested(t *testing.T) {
	cloning := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(w, req.Clone(req.Context())) })
	}
	for name, nest := range map[string]func(http.Handler) http.Handler{
		"twice":          func(h http.Handler) http.Handler { return ObserveMCPHTTP(ObserveMCPHTTP(h)) },
		"around a clone": func(h http.Handler) http.Handler { return ObserveMCPHTTP(cloning(ObserveMCPHTTP(h))) },
		// Headers A, then a copy B, then A again: the outermost wrapper still observes A.
		"restored after a clone": func(h http.Handler) http.Handler {
			return ObserveMCPHTTP(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				original := req.Header
				ObserveMCPHTTP(http.HandlerFunc(func(w http.ResponseWriter, copied *http.Request) {
					copied.Header = original
					ObserveMCPHTTP(h).ServeHTTP(w, copied)
				})).ServeHTTP(w, req.Clone(req.Context()))
			}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			setupAI(t)
			s := errorServer("nested-http-mcp")
			r := wrap(t, s, Options{CaptureContent: true})
			cs := connectHTTP(t, serveStreamable(t, s, nil, nest), "2025-06-18")
			_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "lookup failed"}})
			if p := r.wait(t, "mcp_tool_called", 1)[0].Properties; p["error_message"] != "lookup failed" {
				t.Errorf("error_message=%v through nested wrappers", p["error_message"])
			}
		})
	}
}

// Stateless clients send no session id and number their requests alike: concurrent requests
// with the same id must each get their own response's text.
func TestObserveMCPHTTPStatelessSameRequestID(t *testing.T) {
	setupAI(t)
	const n = 8
	s := mcp.NewServer(&mcp.Implementation{Name: "same-id-mcp", Version: "1"}, nil)
	var arrived sync.WaitGroup
	arrived.Add(n)
	for i := range n {
		name := "lookup_" + strconv.Itoa(i)
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Every request is in flight before any responds.
			arrived.Done()
			arrived.Wait()
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "failed " + name}}}, nil
		})
	}
	r := wrap(t, s, Options{CaptureContent: true})
	url := serveStreamable(t, s, &mcp.StreamableHTTPOptions{Stateless: true}, ObserveMCPHTTP)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup_` + strconv.Itoa(i) + `","arguments":{}}}`
			req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
			req.Header.Set("Authorization", "Bearer tok_secret")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = res.Body.Close()
		})
	}
	wg.Wait()
	for _, e := range r.wait(t, "mcp_tool_called", n) {
		if want := "failed " + e.Properties["tool_name"].(string); e.Properties["error_message"] != want {
			t.Errorf("error_message=%v, want %q", e.Properties["error_message"], want)
		}
	}
}

// The wrapper finds a request's observer by the identity of its header map, which the go-sdk
// passes on unchanged as RequestExtra.Header. If a go-sdk upgrade copies it instead,
// error_message silently disappears; this test fails first.
func TestObserveMCPHTTPGoSDKPassesHeaderMap(t *testing.T) {
	for _, mode := range streamableModes {
		t.Run(mode.name, func(t *testing.T) {
			setupAI(t)
			s := errorServer("header-identity-mcp")
			wrap(t, s, Options{CaptureContent: true})
			var mu sync.Mutex
			found := map[string]bool{}
			s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					if extra := req.GetExtra(); extra != nil {
						mu.Lock()
						found[method] = observedHTTP(extra.Header, nil, "") != nil
						mu.Unlock()
					}
					return next(ctx, method, req)
				}
			})
			cs := connectHTTP(t, serveStreamable(t, s, mode.opts, ObserveMCPHTTP), "2025-06-18")
			_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "x"}})
			mu.Lock()
			defer mu.Unlock()
			for _, method := range []string{"initialize", "tools/call"} {
				if !found[method] {
					t.Errorf("%s: the go-sdk no longer passes the request's header map as RequestExtra.Header (%v)", method, found)
				}
			}
		})
	}
}

// A middleware that reuses one header map across requests must not let a call that outlived its
// own POST take another session's response text.
func TestObserveMCPHTTPReusedHeaderMapOtherSession(t *testing.T) {
	// The other session either calls a failing tool on its own session, or is a new session whose
	// initialize (a request without a session header) fails.
	for _, newSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "established session", true: "new session"}[newSession], func(t *testing.T) {
			testReusedHeaderMap(t, newSession)
		})
	}
}

func testReusedHeaderMap(t *testing.T, newSession bool) {
	setupAI(t)
	s := mcp.NewServer(&mcp.Implementation{Name: "reused-header-mcp", Version: "1"}, nil)
	aBlocked, release, aReturned, bStarted, bRelease := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.AddTool(&mcp.Tool{Name: "a_fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		defer close(aReturned)
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "A text"}}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "b_fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(bStarted)
		<-bRelease
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "B secret"}}}, nil
	})
	r := wrap(t, s, Options{CaptureContent: true})
	// Added after WrapMCPServer, so it runs before it: A's call waits here until its POST is gone.
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if init, ok := req.GetParams().(*mcp.InitializeParams); ok && init.ClientInfo != nil && init.ClientInfo.Name == "b" {
				close(bStarted)
				<-bRelease
				return nil, &jsonrpc.Error{Code: -32603, Message: "B secret"}
			}
			if call, ok := req.(*mcp.CallToolRequest); ok && call.Params.Name == "a_fail" {
				close(aBlocked)
				<-release
			}
			return next(ctx, method, req)
		}
	})
	var poolMu sync.Mutex
	pool := http.Header{}
	posts := make(chan struct{}, 16)
	pooling := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			poolMu.Lock()
			clear(pool)
			for k, v := range req.Header {
				pool[k] = v
			}
			req.Header = pool
			poolMu.Unlock()
			h.ServeHTTP(w, req)
			posts <- struct{}{}
		})
	}
	url := serveStreamable(t, s, nil, func(h http.Handler) http.Handler { return pooling(ObserveMCPHTTP(h)) })
	post := func(ctx context.Context, session, body string) (string, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		req.Header.Set("Authorization", "Bearer tok_secret")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = res.Body.Close() }()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.Header.Get("Mcp-Session-Id"), nil
	}
	open := func() string {
		session, err := post(context.Background(), "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
		if err != nil || session == "" {
			t.Fatalf("initialize: session=%q err=%v", session, err)
		}
		<-posts
		if _, err := post(context.Background(), session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
			t.Fatal(err)
		}
		<-posts
		return session
	}
	call := func(name string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
	}

	sessionA := open()
	ctxA, cancelA := context.WithCancel(context.Background())
	go func() { _, _ = post(ctxA, sessionA, call("a_fail")) }()
	<-aBlocked
	cancelA() // A's POST ends; its call carries on
	<-posts

	if newSession {
		go func() {
			_, _ = post(context.Background(), "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"b","version":"1"}}}`)
		}()
	} else {
		sessionB := open()
		go func() { _, _ = post(context.Background(), sessionB, call("b_fail")) }()
	}
	<-bStarted // B's POST now owns the reused header map
	close(release)
	<-aReturned
	time.Sleep(100 * time.Millisecond) // A's failed event is held or sent
	close(bRelease)

	events := 2
	if newSession {
		events = 1 // initialize sends no failure event
	}
	for _, e := range r.wait(t, "mcp_tool_called", events) {
		if e.Properties["tool_name"] == "a_fail" && e.Properties["error_message"] == "B secret" {
			t.Errorf("session A's event took session B's error text: %v", e.Properties)
		}
		if e.Properties["tool_name"] == "b_fail" && e.Properties["error_message"] != "B secret" {
			t.Errorf("session B's event lost its own error text: %v", e.Properties)
		}
	}
}

// rewriting replaces text in what the handler writes, as a response-editing middleware does.
type rewriting struct {
	http.ResponseWriter
	from, to string
}

func (w rewriting) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w rewriting) FlushError() error           { return http.NewResponseController(w.ResponseWriter).Flush() }
func (w rewriting) Write(p []byte) (int, error) {
	if _, err := w.ResponseWriter.Write(bytes.ReplaceAll(p, []byte(w.from), []byte(w.to))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Wrapped outermost, with the handler wrapped again inside a middleware that copies the headers
// and edits the response, the event carries the text the client received.
func TestObserveMCPHTTPReadsWhatTheClientReceives(t *testing.T) {
	for _, mode := range streamableModes {
		t.Run(mode.name, func(t *testing.T) {
			setupAI(t)
			s := errorServer("final-text-mcp")
			r := wrap(t, s, Options{CaptureContent: true})
			editing := func(h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					h.ServeHTTP(rewriting{w, "backend-private", "client-public"}, req.Clone(req.Context()))
				})
			}
			url := serveStreamable(t, s, mode.opts, func(h http.Handler) http.Handler { return ObserveMCPHTTP(editing(ObserveMCPHTTP(h))) })
			res, err := connectHTTP(t, url, "2025-06-18").CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "backend-private"}})
			if err != nil {
				t.Fatal(err)
			}
			client := res.Content[0].(*mcp.TextContent).Text
			if p := r.wait(t, "mcp_tool_called", 1)[0].Properties; client != "client-public" || p["error_message"] != client {
				t.Errorf("client got %q, event error_message=%v", client, p["error_message"])
			}
		})
	}
}

// diagnosing adds a named SSE event, which MCP clients ignore, before each response it sees.
type diagnosing struct{ http.ResponseWriter }

func (w diagnosing) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w diagnosing) FlushError() error           { return http.NewResponseController(w.ResponseWriter).Flush() }
func (w diagnosing) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"result"`)) {
		_, _ = w.ResponseWriter.Write([]byte("event: diagnostic\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32603,\"message\":\"internal note\"}}\n\n"))
	}
	return w.ResponseWriter.Write(p)
}

func TestObserveMCPHTTPIgnoresNamedSSEEvents(t *testing.T) {
	setupAI(t)
	s := errorServer("named-event-mcp")
	r := wrap(t, s, Options{CaptureContent: true})
	url := serveStreamable(t, s, nil, func(h http.Handler) http.Handler {
		return ObserveMCPHTTP(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(diagnosing{w}, req) }))
	})
	_, _ = connectHTTP(t, url, "2025-06-18").CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "lookup failed"}})
	if p := r.wait(t, "mcp_tool_called", 1)[0].Properties; p["error_message"] != "lookup failed" {
		t.Errorf("error_message=%v, want the response the client read", p["error_message"])
	}
}

// A JSON response still buffered when the client gives up is never sent: no text from it.
func TestObserveMCPHTTPClientGoneBeforeResponse(t *testing.T) {
	setupAI(t)
	s := failingServer("gone-mcp")
	r := wrap(t, s, Options{CaptureContent: true})
	raw := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	buffered := make(chan struct{})
	ts := httptest.NewServer(ObserveMCPHTTP(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw.ServeHTTP(w, req)
		close(buffered)
		<-req.Context().Done() // still running when the client leaves
	})))
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"text":"late failure"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if res, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
		}
	}()
	<-buffered
	cancel()
	<-done
	if p := r.wait(t, "mcp_tool_called", 1)[0].Properties; p["error_message"] != nil {
		t.Errorf("error_message from a response the client never got: %v", p["error_message"])
	}
}

// Failed resources/read and prompts/get carry error_message too, in every mode.
func TestObserveMCPHTTPResourcesAndPrompts(t *testing.T) {
	for _, mode := range streamableModes {
		t.Run(mode.name, func(t *testing.T) {
			setupAI(t)
			s := errorServer("resources-prompts-mcp")
			s.AddResource(&mcp.Resource{Name: "db", URI: "db://x"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return nil, &jsonrpc.Error{Code: -32603, Message: "resource failed"}
			})
			s.AddPrompt(&mcp.Prompt{Name: "summary"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return nil, &jsonrpc.Error{Code: -32603, Message: "prompt failed"}
			})
			r := wrap(t, s, Options{CaptureContent: true})
			cs := connectHTTP(t, serveStreamable(t, s, mode.opts, ObserveMCPHTTP), "2025-06-18")
			_, _ = cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "db://x"})
			_, _ = cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "summary"})
			for event, want := range map[string]string{"mcp_resource_read": "resource failed", "mcp_prompt_used": "prompt failed"} {
				if p := r.wait(t, event, 1)[0].Properties; p["error_message"] != want {
					t.Errorf("%s error_message=%v, want %q", event, p["error_message"], want)
				}
			}
		})
	}
}

type flushErrOnly struct{ http.ResponseWriter }

func (flushErrOnly) FlushError() error { return http.ErrNotSupported }

type unwrapOnly struct{ http.ResponseWriter }

func (u unwrapOnly) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// Behind writers that flush only through Unwrap, can't flush, or have no flush at all.
func TestObserveMCPHTTPWriters(t *testing.T) {
	for name, writer := range map[string]func(http.ResponseWriter) http.ResponseWriter{
		"unwrap only": func(w http.ResponseWriter) http.ResponseWriter { return unwrapOnly{w} },
		"unsupported": func(w http.ResponseWriter) http.ResponseWriter { return flushErrOnly{w} },
		"no flush":    func(w http.ResponseWriter) http.ResponseWriter { return struct{ http.ResponseWriter }{w} },
	} {
		for _, mode := range streamableModes {
			t.Run(name+"/"+mode.name, func(t *testing.T) {
				setupAI(t)
				s := errorServer("writers-mcp")
				r := wrap(t, s, Options{CaptureContent: true})
				url := serveStreamable(t, s, mode.opts, func(h http.Handler) http.Handler {
					o := ObserveMCPHTTP(h)
					return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { o.ServeHTTP(writer(w), req) })
				})
				res, err := connectHTTP(t, url, "2025-06-18").CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "writer failure"}})
				if err != nil {
					t.Skipf("the go-sdk can't serve this mode behind this writer: %v", err)
				}
				if p := r.wait(t, "mcp_tool_called", 1)[0].Properties; p["error_message"] != res.Content[0].(*mcp.TextContent).Text {
					t.Errorf("error_message=%v", p["error_message"])
				}
			})
		}
	}
}
