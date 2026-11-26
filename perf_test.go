//go:build unix

package rudderstackai_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// Per-interaction and per-MCP-call overhead of the core SDK: go test -run '^$' -bench . -benchmem .
// rudderopenai has the wrapped-provider scenarios; make bench runs and reports both. The same
// scenario IDs run in the Node.js and Python SDKs.

// nullAnalytics drops every event, so only the SDK's own work is measured.
type nullAnalytics struct{}

func (nullAnalytics) Enqueue(analytics.Message) error { return nil }
func (nullAnalytics) Close() error                    { return nil }

// measure runs op b.N times and reports, beside ns/op, B/op and allocs/op: the p50 and p99 of one
// op, process CPU per op and GC runs per 10k ops.
var measure = testkit.Measure

type lookupArgs struct {
	Order string `json:"order"`
}

// benchServer is an MCP server with one lookup_order tool, connected to a real client in memory.
func benchServer(b *testing.B, name string, wrap bool) *mcp.ClientSession {
	b.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "lookup_order", Description: "Order status"},
		func(_ context.Context, _ *mcp.CallToolRequest, in lookupArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Order + ": shipped"}}}, nil, nil
		})
	if wrap {
		if err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{Analytics: nullAnalytics{}}); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		b.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "bench-client", Version: "1.0.0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	return cs
}

func BenchmarkOverhead(b *testing.B) {
	// A new interaction ID per MCP call fills the SDK's maps to their cap; the default warning about
	// it would land in the middle of the benchmark output.
	quiet := rudderstackai.WithOnError(func(error, rudderstackai.ErrorInfo) {})
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(nullAnalytics{}), rudderstackai.WithCaptureContent(false),
		rudderstackai.WithLLMCallEvents(false), rudderstackai.WithEmitSpans(false), quiet); err != nil {
		b.Fatal(err)
	}
	user := rudderstackai.InteractionOptions{UserID: "bench-user", ConversationID: "bench-conversation"}

	n := 0
	call := func(cs *mcp.ClientSession) func(context.Context) error {
		return func(ctx context.Context) error {
			n++
			// A new interaction ID per call, as from an agent answering many users; the server keeps only UUIDs.
			_, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name: "lookup_order", Arguments: map[string]any{"order": "A-1"},
				Meta: mcp.Meta{"com.rudderstack/interaction_id": fmt.Sprintf("00000000-0000-4000-8000-%012d", n)},
			})
			return err
		}
	}
	rounds, err := strconv.Atoi(os.Getenv("BENCH_ROUNDS"))
	if err != nil || rounds <= 0 {
		rounds = 5
	}
	for range rounds {
		b.Run("interaction/manual/sdk", func(b *testing.B) {
			measure(b, func(ctx context.Context) error {
				return rudderstackai.RunInteraction(ctx, user, func(_ context.Context, in *rudderstackai.Interaction) error {
					in.RecordCall(rudderstackai.LLMCall{Provider: "openai", Model: "gpt-4.1-mini", InputTokens: 12, OutputTokens: 5})
					return nil
				})
			})
		})
		b.Run("mcp/tools-call/baseline", func(b *testing.B) { measure(b, call(benchServer(b, "bench-plain", false))) })
		b.Run("mcp/tools-call/sdk", func(b *testing.B) { measure(b, call(benchServer(b, "bench-mcp", true))) })
	}
}
