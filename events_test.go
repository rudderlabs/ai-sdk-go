package rudderstackai

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSubscriptionChangesAndExpiry(t *testing.T) {
	server := workspaceServer("events-mcp", nil)
	r := wrap(t, server, Options{MCPEvents: true, Identify: identifyFromToken})
	ctx := context.Background()
	yes, no := true, false
	tok := &auth.TokenInfo{UserID: "oauth|8812", Extra: map[string]any{"workspace": "workspace_acme"}}
	TrackMCPEventSubscription(ctx, server, SubscriptionChange{
		EventsIdentity: EventsIdentity{Token: tok},
		Action:         SubscriptionCreated, SubscriptionID: "sub_1", EventName: "destination.delivery_failed",
		Arguments: map[string]any{"destination_id": "dst_private"}, DeliveryMode: "webhook", TTL: 30 * time.Millisecond,
		ResumedFromCursor: &no, HistoryTruncated: &yes, SessionID: "s1", ClientName: "claude-ai",
	})
	got := r.wait(t, "mcp_event_subscription_changed", 2)
	created, expired := got[0], got[1]
	p := created.Properties
	if p["subscription_action"] != "created" || p["ttl_ms"] != int64(30) || !slices.Equal(p["argument_keys"].([]string), []string{"destination_id"}) ||
		p["account_id"] != "workspace_acme" || p["server_name"] != "events-mcp" || p["history_truncated"] != true || created.UserId != "oauth|8812" {
		t.Errorf("created = %v", p)
	}
	if e := expired.Properties; e["subscription_action"] != "expired" || e["ttl_ms"] != nil || e["history_truncated"] != nil || e["subscription_id"] != "sub_1" {
		t.Errorf("expired = %v", e)
	}
	if strings.Contains(mustJSON(r.all()), "dst_private") {
		t.Error("filter values leaked")
	}

	// A refresh resets the timer; an unsubscribe stops it.
	r.reset()
	TrackMCPEventSubscription(ctx, server, SubscriptionChange{Action: SubscriptionCreated, SubscriptionID: "sub_2", EventName: "e", TTL: 40 * time.Millisecond})
	TrackMCPEventSubscription(ctx, server, SubscriptionChange{Action: SubscriptionUnsubscribed, SubscriptionID: "sub_2", EventName: "e", EventsIdentity: EventsIdentity{UserID: "u1", AccountID: "acct"}})
	TrackMCPEventSubscription(ctx, server, SubscriptionChange{Action: SubscriptionCallbackFailed, SubscriptionID: "sub_3", EventName: "e", FailureReason: "challenge_failed"})
	TrackMCPEventSubscription(ctx, server, SubscriptionChange{Action: SubscriptionCreated, EventName: "e"})
	time.Sleep(80 * time.Millisecond)
	var actions []any
	for _, m := range r.named("mcp_event_subscription_changed") {
		actions = append(actions, m.Properties["subscription_action"])
	}
	if want := []any{"created", "unsubscribed", "callback_failed"}; !slices.Equal(actions, want) {
		t.Errorf("actions = %v", actions)
	}
	if p := r.named("mcp_event_subscription_changed")[1]; p.UserId != "u1" || p.Properties["account_id"] != "acct" {
		t.Errorf("explicit identity = %v", mustJSON(p))
	}
}

func TestEventSenderDeliveries(t *testing.T) {
	server := workspaceServer("events-mcp", nil)
	r := wrap(t, server, Options{MCPEvents: true})
	outcomes := []DeliveryOutcome{{HTTPStatus: 200, AttemptCount: 2}, {HTTPStatus: 410}, {HTTPStatus: 503, AttemptCount: 5}, {DeliveryStatus: DeliveryAcknowledged}}
	i := 0
	send := WrapMCPEventSender(server, func(_ context.Context, d EventDelivery) (DeliveryOutcome, error) {
		if d.EventID == "evt_fail" {
			return DeliveryOutcome{}, errors.New("network down")
		}
		o := outcomes[i]
		i++
		return o, nil
	})
	ctx := context.Background()
	payloads := []any{`{"comment":"private"}`, []byte("12345"), map[string]any{"a": "<b>"}, nil}
	for _, payload := range payloads {
		out, err := send(ctx, EventDelivery{SubscriptionID: "sub_1", EventName: "comment.created", EventID: "evt_1", Payload: payload, OccurredAt: time.Now().Add(-time.Second), EventsIdentity: EventsIdentity{AccountID: "acct"}})
		if err != nil || out.HTTPStatus != outcomes[i-1].HTTPStatus {
			t.Fatalf("sender result changed: %v %v", out, err)
		}
	}
	if _, err := send(ctx, EventDelivery{SubscriptionID: "sub_1", EventName: "comment.created", EventID: "evt_fail"}); err == nil {
		t.Fatal("the sender's error was swallowed")
	}
	got := r.named("mcp_event_delivered")
	var statuses, sizes []any
	for _, m := range got {
		statuses = append(statuses, m.Properties["delivery_status"])
		sizes = append(sizes, m.Properties["payload_size_bytes"])
	}
	if want := []any{"acknowledged", "rejected", "gave_up", "acknowledged", "gave_up"}; !slices.Equal(statuses, want) {
		t.Errorf("statuses = %v", statuses)
	}
	if want := []any{21, 5, 11, nil, nil}; !slices.Equal(sizes, want) {
		t.Errorf("sizes = %v", sizes)
	}
	first := got[0].Properties
	if first["http_status"] != 200 || first["attempt_count"] != 2 || first["account_id"] != "acct" || first["delivery_latency_ms"].(int64) < 900 {
		t.Errorf("first = %v", first)
	}
	if _, ok := got[2].Properties["delivery_latency_ms"]; ok {
		t.Error("latency on a failed delivery")
	}
	if strings.Contains(mustJSON(r.all()), "private") {
		t.Error("payload leaked")
	}
}

func TestMCPEventsNeedOptIn(t *testing.T) {
	server := workspaceServer("events-mcp", nil)
	r := wrap(t, server, Options{})
	other := mcp.NewServer(&mcp.Implementation{Name: "unwrapped"}, nil)
	TrackMCPEventSubscription(context.Background(), server, SubscriptionChange{Action: SubscriptionCreated, SubscriptionID: "s", EventName: "e"})
	TrackMCPEventSubscription(context.Background(), other, SubscriptionChange{Action: SubscriptionCreated, SubscriptionID: "s", EventName: "e"})
	send := WrapMCPEventSender(server, func(context.Context, EventDelivery) (DeliveryOutcome, error) {
		return DeliveryOutcome{HTTPStatus: 200}, nil
	})
	if _, err := send(context.Background(), EventDelivery{SubscriptionID: "s", EventName: "e", EventID: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 0 {
		t.Errorf("events = %v", r.names())
	}
}

// Without its required IDs an MCP Events record is skipped, not sent with placeholders; without
// OccurredAt the latency is left out.
func TestMCPEventsSkipMissingIDsAndLatency(t *testing.T) {
	server := workspaceServer("events-ids-mcp", nil)
	r := wrap(t, server, Options{MCPEvents: true})
	ctx := context.Background()
	TrackMCPEventSubscription(ctx, server, SubscriptionChange{Action: SubscriptionCreated, EventName: "e"})
	TrackMCPEventSubscription(ctx, server, SubscriptionChange{Action: SubscriptionCreated, SubscriptionID: "s"})
	send := WrapMCPEventSender(server, func(context.Context, EventDelivery) (DeliveryOutcome, error) {
		return DeliveryOutcome{HTTPStatus: 200}, nil
	})
	for _, d := range []EventDelivery{
		{EventName: "e", EventID: "x"}, {SubscriptionID: "s", EventID: "x"}, {SubscriptionID: "s", EventName: "e"},
		{SubscriptionID: "s", EventName: "e", EventID: "x"},
	} {
		if _, err := send(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.names(); len(got) != 1 || got[0] != "mcp_event_delivered" {
		t.Fatalf("events = %v", got)
	}
	if _, ok := r.all()[0].Properties["delivery_latency_ms"]; ok {
		t.Error("delivery_latency_ms without OccurredAt")
	}
}
