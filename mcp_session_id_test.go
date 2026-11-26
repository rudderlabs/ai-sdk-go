package rudderstackai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	sessionUUID  = "6f1c2a8e-3b4d-4c5e-9f60-718293a4b5c6"
	sessionEmail = "jane.doe@example.com"
)

// postTool sends one raw tools/call to a go-sdk Streamable HTTP handler with the given
// Mcp-Session-Id header (omitted when empty).
func postTool(t *testing.T, url, header string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_destinations","arguments":{"workspace_id":"w"},"_meta":{"io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"}}}}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	if header != "" {
		req.Header.Set("Mcp-Session-Id", header)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", res.StatusCode)
	}
}

func TestMCPSessionIDStatelessHeader(t *testing.T) {
	for _, tc := range []struct{ name, header, want string }{
		{"email", sessionEmail, ""},
		{"uuid", sessionUUID, sessionUUID},
		{"none", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, Options{})
			ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true}))
			defer ts.Close()
			postTool(t, ts.URL, tc.header)
			postTool(t, ts.URL, tc.header)
			calls := r.named("mcp_tool_called")
			if len(calls) != 2 {
				t.Fatalf("events = %v", r.names())
			}
			for i, c := range calls {
				got, present := c.Properties["mcp_session_id"]
				if tc.want == "" && present || tc.want != "" && got != tc.want {
					t.Errorf("call %d: mcp_session_id = %v (present %v), want %q", i, got, present, tc.want)
				}
			}
			// A rejected header still scopes call positions in-process; it is only never sent.
			for i, c := range calls {
				idx, scoped := c.Properties["call_index"]
				if scoped != (tc.header != "") || scoped && idx != i {
					t.Errorf("call %d: call_index = %v (present %v), want %d scoped=%v", i, idx, scoped, i, tc.header != "")
				}
			}
			seen := r.named("mcp_client_seen")
			if len(seen) == 0 {
				t.Fatalf("no mcp_client_seen: %v", r.names())
			}
			if got, present := seen[0].Properties["mcp_session_id"]; tc.want == "" && present || tc.want != "" && got != tc.want {
				t.Errorf("mcp_client_seen mcp_session_id = %v, want %q", got, tc.want)
			}
		})
	}
}

type headerTransport struct{ header string }

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Mcp-Session-Id", h.header)
	return http.DefaultTransport.RoundTrip(req)
}

func TestMCPSessionIDSSEHeader(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{Transport: "sse"})
	ts := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return s }, nil))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx,
		&mcp.SSEClientTransport{Endpoint: ts.URL, HTTPClient: &http.Client{Transport: headerTransport{sessionEmail}}},
		&mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_destinations", Arguments: map[string]any{"workspace_id": "w"}})
	if err != nil || res.IsError {
		t.Fatalf("result=%v error=%v", res, err)
	}
	for _, event := range []string{"mcp_client_seen", "mcp_tool_called"} {
		if p := r.props(t, event); p["mcp_session_id"] != nil {
			t.Errorf("%s mcp_session_id = %v, want absent", event, p["mcp_session_id"])
		}
	}
}

func TestMCPSessionIDStatefulIssued(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts *mcp.ServerOptions
	}{
		{"generated", nil},
		{"custom", &mcp.ServerOptions{GetSessionID: func() string { return "session-abc" }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mcp.NewServer(&mcp.Implementation{Name: t.Name(), Version: "1"}, tc.opts)
			mcp.AddTool(s, &mcp.Tool{Name: "list_destinations"}, func(context.Context, *mcp.CallToolRequest, listIn) (*mcp.CallToolResult, listOut, error) {
				return nil, listOut{}, nil
			})
			r := wrap(t, s, Options{})
			ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
			defer ts.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cs.Close() }()
			if cs.ID() == "" {
				t.Fatal("no session ID issued")
			}
			for range 2 {
				if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_destinations", Arguments: map[string]any{"workspace_id": "w"}}); err != nil {
					t.Fatal(err)
				}
			}
			calls := r.named("mcp_tool_called")
			if len(calls) != 2 {
				t.Fatalf("events = %v", r.names())
			}
			for i, c := range calls {
				if c.Properties["mcp_session_id"] != cs.ID() {
					t.Errorf("call %d: mcp_session_id = %v, want %q", i, c.Properties["mcp_session_id"], cs.ID())
				}
			}
			if calls[1].Properties["call_index"] != 1 {
				t.Errorf("call_index = %v, want 1 in one session", calls[1].Properties["call_index"])
			}
			for _, e := range r.named("mcp_client_seen") {
				if e.Properties["mcp_session_id"] != cs.ID() {
					t.Errorf("mcp_client_seen mcp_session_id = %v, want %q", e.Properties["mcp_session_id"], cs.ID())
				}
			}
		})
	}
}

// Net/http trims header whitespace on the wire, so a padded header is fed to the middleware directly.
func TestMCPSessionIDPaddedHeaderTrimmed(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{})
	w, _ := instances.get(s)
	_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	})(context.Background(), "tools/call", &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "list_destinations"},
		Extra:  &mcp.RequestExtra{Header: http.Header{"Mcp-Session-Id": []string{" \t" + sessionUUID + " "}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.props(t, "mcp_tool_called")["mcp_session_id"]; got != sessionUUID {
		t.Errorf("mcp_session_id = %v, want trimmed %q", got, sessionUUID)
	}
}

func TestMCPSessionIDSharedFixture(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name     string  `json:"name"`
			Issued   *string `json:"issued"`
			Header   *string `json:"header"`
			Expected *string `json:"expected"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("spec/fixtures/mcp-session-id-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	orEmpty := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got, want := reportedSessionID(orEmpty(tc.Issued), orEmpty(tc.Header)), orEmpty(tc.Expected); got != want {
				t.Fatalf("reportedSessionID = %q, want %q", got, want)
			}
		})
	}
}

// go-sdk reads MCPGODEBUG in init(), so the compat mode runs in a re-exec'd test binary.
func TestMCPSessionIDLegacyStatelessSessions(t *testing.T) {
	if os.Getenv("RUDDER_LEGACY_STATELESS_INNER") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMCPSessionIDLegacyStatelessSessions$", "-test.v")
		cmd.Env = append(os.Environ(), "MCPGODEBUG=allowsessionsinstateless=1", "RUDDER_LEGACY_STATELESS_INNER=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("inner run failed: %v\n%s", err, out)
		} else if !strings.Contains(string(out), "--- PASS: TestMCPSessionIDLegacyStatelessSessions/email") {
			t.Fatalf("inner cases did not run:\n%s", out)
		}
		return
	}
	for _, tc := range []struct{ name, header, want string }{
		{"email", sessionEmail, ""},
		{"uuid", sessionUUID, sessionUUID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, Options{})
			ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true}))
			defer ts.Close()
			postTool(t, ts.URL, tc.header)
			postTool(t, ts.URL, tc.header)
			events := append(r.named("mcp_client_seen"), r.named("mcp_tool_called")...)
			if len(events) < 3 {
				t.Fatalf("events = %v", r.names())
			}
			for i, e := range events {
				got, present := e.Properties["mcp_session_id"]
				if tc.want == "" && present || tc.want != "" && got != tc.want {
					t.Errorf("%s %d: mcp_session_id = %v (present %v), want %q", e.Event, i, got, present, tc.want)
				}
			}
			// The rejected header still scopes call positions in-process.
			for i, c := range r.named("mcp_tool_called") {
				if idx, scoped := c.Properties["call_index"]; !scoped || idx != i {
					t.Errorf("call %d: call_index = %v (present %v), want %d", i, idx, scoped, i)
				}
			}
		})
	}
}

// A header that is not sent as mcp_session_id still scopes pending input_required legs, so one
// client's retry never consumes another client's leg.
func TestMCPSessionIDRejectedHeaderScopesPendingLegs(t *testing.T) {
	server := confirmServer("scoped-confirm", 0)
	r := wrap(t, server, Options{})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connectAs := func(header string, handler func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)) *mcp.ClientSession {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, &mcp.ClientOptions{ElicitationHandler: handler}).Connect(ctx,
			&mcp.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true, HTTPClient: &http.Client{Transport: headerTransport{header}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	answer := func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
	}
	pending, release := make(chan struct{}), make(chan struct{})
	alpha := connectAs("alpha session", func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		close(pending)
		<-release
		return answer(ctx, req)
	})
	beta := connectAs("beta session", answer)
	alphaDone := make(chan error, 1)
	go func() {
		_, err := alpha.CallTool(ctx, &mcp.CallToolParams{Name: "confirm", Arguments: map[string]any{}})
		alphaDone <- err
	}()
	<-pending // alpha's input_required leg is pending
	if _, err := beta.CallTool(ctx, &mcp.CallToolParams{Name: "confirm", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-alphaDone; err != nil {
		t.Fatal(err)
	}
	calls := r.named("mcp_tool_called")
	if len(calls) != 2 {
		t.Fatalf("mcp_tool_called = %d, want 2: %v", len(calls), r.names())
	}
	for i, c := range calls {
		if _, sent := c.Properties["mcp_session_id"]; sent {
			t.Errorf("call %d sent mcp_session_id", i)
		}
		// Beta's retry is its own first call (index 0, no retry_count), not alpha's leg; alpha's keeps position 0.
		if c.Properties["call_index"] != 0 {
			t.Errorf("call %d: call_index = %v, want 0", i, c.Properties["call_index"])
		}
		if _, ok := c.Properties["retry_count"]; ok {
			t.Errorf("call %d: retry_count present", i)
		}
	}
	if n := len(r.named("mcp_elicitation_completed")); n != 2 {
		t.Errorf("mcp_elicitation_completed = %d, want 2", n)
	}
}
