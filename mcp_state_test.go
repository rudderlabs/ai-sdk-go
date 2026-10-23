package rudderstackai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

var fresh = struct {
	sync.Mutex
	done map[*testing.T]bool
}{done: map[*testing.T]bool{}}

// freshState forgets every server name's state once per test: state is process-wide.
func freshState(t *testing.T) {
	fresh.Lock()
	defer fresh.Unlock()
	if fresh.done[t] {
		return
	}
	fresh.done[t] = true
	servers.Lock()
	defer servers.Unlock()
	servers.m.clear()
}

func TestStateIsPerServerName(t *testing.T) {
	first, second := workspaceServer("same-name", nil), workspaceServer("same-name", nil)
	r1, r2 := wrap(t, first, Options{}), wrap(t, second, Options{})
	meta := mcp.Meta{metaInteractionID: "int_shared"}
	for _, server := range []*mcp.Server{first, second} {
		cs := connect(t, server, "2025-06-18", nil)
		callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, meta)
	}
	if got := len(r1.named("mcp_client_seen")) + len(r2.named("mcp_client_seen")); got != 1 {
		t.Errorf("mcp_client_seen = %d, want 1 for the name", got)
	}
	p1, p2 := r1.props(t, "mcp_tool_called"), r2.props(t, "mcp_tool_called")
	if p1["call_index"] != 0 || p2["call_index"] != 1 || p2["retry_count"] != 1 {
		t.Errorf("counters not shared by name: %v / %v", p1, p2)
	}
}

func TestDifferentNamesAreSeparate(t *testing.T) {
	first, second := workspaceServer("name-a", nil), workspaceServer("name-b", nil)
	r1, r2 := wrap(t, first, Options{}), wrap(t, second, Options{})
	meta := mcp.Meta{metaInteractionID: "int_same"}
	for _, server := range []*mcp.Server{first, second} {
		cs := connect(t, server, "2025-06-18", nil)
		callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, meta)
	}
	for i, r := range []*recorder{r1, r2} {
		if len(r.named("mcp_client_seen")) != 1 {
			t.Errorf("server %d: mcp_client_seen = %d, want 1", i, len(r.named("mcp_client_seen")))
		}
		if p := r.props(t, "mcp_tool_called"); p["call_index"] != 0 || p["retry_count"] != 0 {
			t.Errorf("server %d: counters shared across names: %v", i, p)
		}
	}
}

// A server built for every request, as stateless deployments do, keeps one name's state.
func TestPerRequestServersKeepCounting(t *testing.T) {
	freshState(t)
	r := newRecorder(t)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		s := workspaceServer("per-request-mcp", nil)
		if err := WrapMCPServer(s, Options{Analytics: r}); err != nil {
			t.Error(err)
		}
		return s
	}, &mcp.StreamableHTTPOptions{Stateless: true})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	cs, err := mcp.NewClient(testClientInfo, nil).Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for range 3 {
		callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, mcp.Meta{metaInteractionID: "int_per_request"})
	}
	calls := r.named("mcp_tool_called")
	if len(calls) != 3 {
		t.Fatalf("calls = %v", r.names())
	}
	for i, c := range calls {
		if c.Properties["call_index"] != i || c.Properties["retry_count"] != i {
			t.Errorf("call %d = %v", i, c.Properties)
		}
	}
	if got := len(r.named("mcp_client_seen")); got != 1 {
		t.Errorf("mcp_client_seen = %d, want 1", got)
	}
}

func TestRewrapKeepsState(t *testing.T) {
	server := workspaceServer("rewrap-mcp", nil)
	wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	meta := mcp.Meta{metaInteractionID: "int_rewrap"}
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, meta)
	second := wrap(t, server, Options{})
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, meta)
	if p := second.props(t, "mcp_tool_called"); p["call_index"] != 1 || p["retry_count"] != 1 {
		t.Errorf("rewrap lost state: %v", p)
	}
}

// Wrapped servers are not kept alive by the integration: building one per request does not
// grow memory.
func TestWrappedServersAreReleased(t *testing.T) {
	freshState(t)
	r := newRecorder(t)
	const n = 200
	for i := range n {
		if err := WrapMCPServer(workspaceServer("released-"+strconv.Itoa(i%3), nil), Options{Analytics: r}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for instances.size() >= n/2 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if got := instances.size(); got >= n/2 {
		t.Errorf("%d of %d wrapped servers still held", got, n)
	}
}

func TestStateIsBounded(t *testing.T) {
	freshState(t)
	for i := range maxServers + 50 {
		stateFor("bounded-" + strconv.Itoa(i))
	}
	servers.Lock()
	got := servers.m.size()
	servers.Unlock()
	if got != maxServers {
		t.Errorf("server names kept = %d, want %d", got, maxServers)
	}

	r := newRecorder(t)
	w, err := mcpbridge.Wrap("bounded-calls", "1", Options{Analytics: r})
	if err != nil {
		t.Fatal(err)
	}
	tools := make([]mcpbridge.Tool, 0, defaultLimits.MaxMapEntries+10)
	for i := range defaultLimits.MaxMapEntries + 10 {
		tools = append(tools, mcpbridge.Tool{Name: "tool_" + strconv.Itoa(i), Schema: map[string]any{"type": "object"}})
	}
	w.Begin(mcpbridge.Request{Ctx: context.Background(), Connection: "c1"}).ListTools(tools, true)
	for i := range defaultLimits.MaxMapEntries + 10 {
		args := map[string]json.RawMessage{"n": json.RawMessage(strconv.Itoa(i))}
		w.Begin(mcpbridge.Request{Ctx: context.Background(), Connection: "c" + strconv.Itoa(i%2)}).ToolCall("tool", args, false, nil, "")
	}
	st := stateFor("bounded-calls")
	st.Lock()
	defer st.Unlock()
	for name, size := range map[string]int{"retries": st.retries.size(), "toolSchemas": st.toolSchemas.size(), "scopes": st.scopes.size()} {
		if size > defaultLimits.MaxMapEntries {
			t.Errorf("%s = %d entries, more than %d", name, size, defaultLimits.MaxMapEntries)
		}
	}
	if st.retries.size() != defaultLimits.MaxMapEntries {
		t.Errorf("retries = %d, want the cap %d", st.retries.size(), defaultLimits.MaxMapEntries)
	}
}
