package rudderstackai

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// What Identify returns and the identity MCP events carry (spec/fixtures/mcp-identity-v0.1.json).
func TestMCPIdentitySharedFixture(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name     string         `json:"name"`
			Identity map[string]any `json:"identity"`
			Expected map[string]any `json:"expected"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("spec/fixtures/mcp-identity-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			text := func(key string) string { s, _ := tc.Identity[key].(string); return s }
			id := Identity{UserID: text("user_id"), AnonymousID: text("anonymous_id"), AccountID: text("account_id"), OAuthClientID: text("oauth_client_id"), AuthMethod: text("auth_method"), Country: text("country")}
			if scopes, ok := tc.Identity["token_scopes"].([]any); ok {
				id.TokenScopes = []string{} // a typed list: non-string entries can't be passed
				for _, s := range scopes {
					if s, ok := s.(string); ok {
						id.TokenScopes = append(id.TokenScopes, s)
					}
				}
			}
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, Options{Identify: func(context.Context, *auth.TokenInfo) (Identity, error) { return id, nil }})
			cs := connect(t, s, "2025-06-18", nil)
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
			// auth_method has a default ("oauth" or "none"): checked, like country, where the case sets it.
			var seenLabels, toolLabels []string
			for _, key := range []string{"auth_method", "country"} {
				if _, ok := tc.Expected[key]; ok {
					seenLabels = append(seenLabels, key)
					if key == "auth_method" {
						toolLabels = append(toolLabels, key)
					}
				}
			}
			for event, keys := range map[string][]string{
				"mcp_client_seen": append([]string{"account_id", "oauth_client_id", "token_scopes"}, seenLabels...),
				"mcp_tool_called": append([]string{"account_id", "oauth_client_id"}, toolLabels...),
			} {
				m := r.named(event)
				if len(m) != 1 {
					t.Fatalf("%s: %d events", event, len(m))
				}
				if want, _ := tc.Expected["user_id"].(string); m[0].UserId != want {
					t.Errorf("%s userId = %q, want %q", event, m[0].UserId, want)
				}
				if want, ok := tc.Expected["anonymous_id"].(string); ok && m[0].AnonymousId != want {
					t.Errorf("%s anonymousId = %q, want %q", event, m[0].AnonymousId, want)
				}
				for _, key := range keys {
					got, _ := json.Marshal(m[0].Properties[key])
					want, _ := json.Marshal(tc.Expected[key])
					if !reflect.DeepEqual(got, want) {
						t.Errorf("%s %s = %s, want %s", event, key, got, want)
					}
				}
			}
		})
	}
}

// A subscription change whose ID or event name is blank after trimming sends nothing, for every
// action (as in the Node.js and Python SDKs).
func TestSubscriptionIDsCheckedAfterTrimming(t *testing.T) {
	server := workspaceServer(t.Name(), nil)
	r := wrap(t, server, Options{MCPEvents: true})
	for _, action := range []string{SubscriptionCreated, SubscriptionRefreshed, SubscriptionUnsubscribed, SubscriptionRevoked, SubscriptionExpired, SubscriptionCallbackFailed} {
		for _, ids := range [][2]string{{"\u0085", "comment.created"}, {" 　", "comment.created"}, {"sub-1", " \u0085"}} {
			TrackMCPEventSubscription(context.Background(), server, SubscriptionChange{Action: action, SubscriptionID: ids[0], EventName: ids[1], TTL: 10 * time.Millisecond})
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(r.named("mcp_event_subscription_changed")); n != 0 {
		t.Fatalf("%d events for blank ids", n)
	}
}
