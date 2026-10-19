package rudderstackai_test

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

type audienceIn struct {
	Name string `json:"name"`
}

type audienceOut struct {
	ID string `json:"id"`
}

func ExampleWrapMCPServer() {
	client, err := analytics.NewWithConfig("WRITE_KEY", analytics.Config{DataPlaneUrl: "https://example.dataplane.rudderstack.com"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close() // flushes queued events

	server := mcp.NewServer(&mcp.Implementation{Name: "workspace-mcp", Version: "1.4.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "create_audience"}, func(context.Context, *mcp.CallToolRequest, audienceIn) (*mcp.CallToolResult, audienceOut, error) {
		return nil, audienceOut{ID: "aud_1"}, nil
	})

	err = rudderstackai.WrapMCPServer(server, rudderstackai.Options{
		Analytics: client,
		Identify: func(_ context.Context, tok *auth.TokenInfo) (rudderstackai.Identity, error) {
			if tok == nil {
				return rudderstackai.Identity{}, nil
			}
			workspace, _ := tok.Extra["workspace_id"].(string)
			return rudderstackai.Identity{AccountID: workspace}, nil
		},
		EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
			"create_audience": {Event: "audience_created"},
		}},
		RequestContext: rudderstackai.RequestContextOptions{Intents: []string{"debug_delivery", "build_audience"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	// Serve as usual; every request reaches the same wrapped server.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	_ = handler
}

func ExampleWrapMCPEventSender() {
	client, err := analytics.NewWithConfig("WRITE_KEY", analytics.Config{DataPlaneUrl: "https://example.dataplane.rudderstack.com"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "workspace-mcp", Version: "1.4.0"}, nil)
	if err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{Analytics: client, MCPEvents: true}); err != nil {
		log.Fatal(err)
	}

	// Your webhook sender, wrapped: one mcp_event_delivered per finished delivery.
	deliver := rudderstackai.WrapMCPEventSender(server, func(ctx context.Context, d rudderstackai.EventDelivery) (rudderstackai.DeliveryOutcome, error) {
		// POST d.Payload to the subscriber's callback, with your own retries.
		return rudderstackai.DeliveryOutcome{HTTPStatus: http.StatusOK, AttemptCount: 1}, nil
	})
	_, _ = deliver(context.Background(), rudderstackai.EventDelivery{
		SubscriptionID: "sub_1", EventName: "comment.created", EventID: "evt_1",
		Payload: map[string]any{"comment_id": "c_1"}, OccurredAt: time.Now(),
	})
}
