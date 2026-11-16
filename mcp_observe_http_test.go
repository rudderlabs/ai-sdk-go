package rudderstackai

import (
	"bytes"
	"context"
	"encoding/json"
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

// A middleware that hands several requests one header map makes them indistinguishable: none
// may get another's text.
func TestObserveMCPHTTPSharedHeaderMap(t *testing.T) {
	setupAI(t)
	const n = 2
	s := mcp.NewServer(&mcp.Implementation{Name: "shared-header-mcp", Version: "1"}, nil)
	var arrived sync.WaitGroup
	arrived.Add(n)
	for i := range n {
		name := "lookup_" + strconv.Itoa(i)
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			arrived.Done()
			arrived.Wait()
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "failed " + name}}}, nil
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
		if msg, ok := e.Properties["error_message"]; ok && msg != "failed "+e.Properties["tool_name"].(string) {
			t.Errorf("%v got another request's error_message %q", e.Properties["tool_name"], msg)
		}
	}
}

func TestObserveMCPHTTPNested(t *testing.T) {
	setupAI(t)
	s := errorServer("nested-http-mcp")
	r := wrap(t, s, Options{CaptureContent: true})
	twice := func(h http.Handler) http.Handler { return ObserveMCPHTTP(ObserveMCPHTTP(h)) }
	cs := connectHTTP(t, serveStreamable(t, s, nil, twice), "2025-06-18")
	_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "lookup failed"}})
	if p := r.wait(t, "mcp_tool_called", 1)[0].Properties; p["error_message"] != "lookup failed" {
		t.Errorf("error_message=%v through nested wrappers", p["error_message"])
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
						found[method] = observedHTTP(extra.Header) != nil
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
