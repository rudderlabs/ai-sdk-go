//go:build load

package rudderstackai

// The in-process half of the load test (internal/conformance/load_test.go is the end-to-end
// half): 6,000 MCP calls from interactions, checking every MCP map's cap, eviction of
// per-interaction entries, and the open-interaction count against the SDK's own state.
// Only this package can read that state.
// Node.js and Python make the same checks in their single load test.
//
//	go test -tags load -run TestLoadMCPMaps .

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"
)

type discard struct{}

func (discard) Enqueue(analytics.Message) error { return nil }
func (discard) Close() error                    { return nil }

func TestLoadMCPMaps(t *testing.T) {
	const maxMapEntries, workers, callsPerWorker, name = 2_000, 50, 120, "load-mcp-maps"
	if err := Configure(WithAnalytics(discard{}), WithLimits(Limits{MaxMapEntries: maxMapEntries}),
		WithOnError(func(error, ErrorInfo) {})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Configure(WithLimits(Limits{}), WithOnError(nil)) })
	type args struct {
		Order string `json:"order"`
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := range workers {
		server := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1.0.0"}, nil)
		mcp.AddTool(server, &mcp.Tool{Name: "lookup_order"}, func(_ context.Context, _ *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Order + ": shipped"}}}, nil, nil
		})
		if err := WrapMCPServer(server, Options{Analytics: discard{}}); err != nil {
			t.Fatal(err)
		}
		ct, st := mcp.NewInMemoryTransports()
		ss, err := server.Connect(ctx, st, nil)
		if err != nil {
			t.Fatal(err)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "load-client", Version: "1.0.0"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
		wg.Go(func() {
			for n := range callsPerWorker {
				var id string
				_ = RunInteraction(ctx, InteractionOptions{UserID: fmt.Sprintf("user-%d-%d", w, n)}, func(_ context.Context, in *Interaction) error {
					id = in.ID()
					in.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1-mini", InputTokens: 12, OutputTokens: 5})
					return nil
				})
				if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
					Name: "lookup_order", Arguments: map[string]any{"order": "A-1"},
					Meta: mcp.Meta{"com.rudderstack/interaction_id": id},
				}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()

	s := stateFor(name)
	s.Lock()
	sizes := map[string]int{
		"resources": s.resources.size(), "templates": s.templates.size(), "clientSeen": s.clientSeen.size(),
		"clients": s.clients.size(), "scopes": s.scopes.size(), "retries": s.retries.size(),
		"inputRequired": s.inputRequired.size(), "expiries": s.expiries.size(),
	}
	s.Unlock()
	servers.Lock()
	sizes["servers"] = servers.m.size()
	servers.Unlock()
	t.Logf("MCP map sizes (cap %d) after %d calls: %v", maxMapEntries, workers*callsPerWorker, sizes)
	// Templates and server names have their own fixed caps; every other map has MaxMapEntries.
	caps := map[string]int{"templates": maxTemplates, "servers": maxServers}
	for m, n := range sizes {
		limit, ok := caps[m]
		if !ok {
			limit = maxMapEntries
		}
		if n > limit {
			t.Errorf("%s holds %d entries, over its cap of %d", m, n, limit)
		}
	}
	// Every call has its own interaction, so the per-interaction maps must have filled and evicted.
	for _, m := range []string{"scopes", "retries"} {
		if sizes[m] != maxMapEntries {
			t.Errorf("%s holds %d entries after %d calls: the run did not exercise eviction", m, sizes[m], workers*callsPerWorker)
		}
	}
	open.mu.Lock()
	openN, paused := open.list.Len(), len(open.paused)
	open.mu.Unlock()
	if openN != 0 || paused != 0 {
		t.Errorf("%d interactions open and %d paused at the end", openN, paused)
	}
}
