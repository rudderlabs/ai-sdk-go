package rudderstackai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Transport values for [Options.Transport], as sent in mcp_client_seen.
const (
	TransportStdio          = "stdio"
	TransportStreamableHTTP = "streamable_http"
	TransportSSE            = "sse"
)

// Identity is who a request is for, as returned by [Options.Identify].
type Identity struct {
	// UserID is the OAuth subject. Defaults to the token's UserID.
	UserID string
	// AnonymousID is used when there is no UserID. Defaults to "ai_system".
	AnonymousID string
	// AccountID is the workspace or tenant, sent as account_id.
	AccountID string
	// OAuthClientID is the registered OAuth app behind the token.
	OAuthClientID string
	// TokenScopes are the granted scopes. Default to the token's scopes.
	TokenScopes []string
	// AuthMethod is, for example, "oauth", "api_key" or "none". Defaults to "oauth" with a
	// token, else "none".
	AuthMethod string
	// Country is a coarse country, if you derive one from the request IP. The IP is never sent.
	Country string
}

// RequestContextOptions configures the request_context argument added to every tool.
type RequestContextOptions struct {
	// Disabled turns request_context off.
	Disabled bool
	// Intents is the closed list the agent picks intent from; "other" is always added. Without
	// a list, intent is not offered. Avoid sensitive categories: the label alone can reveal them.
	Intents []string
	// DisableUserSentiment stops offering user_sentiment (negative, neutral, positive).
	DisableUserSentiment bool
}

// CapabilityRequestOptions configures the injected request_capability tool.
type CapabilityRequestOptions struct {
	// Capabilities are the known gaps the agent picks from, for example "bulk_export";
	// "other" is always added.
	Capabilities []string
}

// ToolEventRule turns calls of one tool into a customer-named track event.
type ToolEventRule struct {
	// Event is the event name, for example "audience_created". Names starting with "ai_" or
	// "mcp_" are reserved.
	Event string
	// Properties returns extra properties from the call's arguments (without request_context)
	// and result (structuredContent, else the single text block parsed as JSON). SDK-owned
	// property names are ignored. If it panics, the event is sent without them.
	Properties func(args map[string]any, result any) map[string]any
	// When sends the event only when it returns true. If it panics, the event is not sent.
	When func(args map[string]any, result any) bool
}

// EventRules declares customer-named events.
type EventRules struct {
	// Tools maps a tool name to its rule.
	Tools map[string]ToolEventRule
}

// Options configures [WrapMCPServer].
type Options struct {
	// Analytics is the RudderStack client. Required.
	Analytics analytics.Client
	// Identify maps a request's verified bearer token (nil without auth) to the user and
	// workspace. Called once per request; if it fails or panics, only the token's own UserID
	// and scopes are used.
	Identify func(ctx context.Context, token *auth.TokenInfo) (Identity, error)
	// EventRules declares tool events. Names starting with "ai_" or "mcp_" are rejected.
	EventRules EventRules
	// RequestContext configures the optional request_context argument, on by default.
	RequestContext RequestContextOptions
	// ClientSeenWindow is the window in which a (client, OAuth client, user) is reported once
	// by mcp_client_seen. Zero means 24 hours.
	ClientSeenWindow time.Duration
	// CaptureContent sends agent-written text: call_reason and capability_detail, masked and
	// capped at 200 characters. Off by default.
	CaptureContent bool
	// CapabilityRequests adds the request_capability tool, recorded as
	// mcp_capability_requested. Nil (the default) leaves it out.
	CapabilityRequests *CapabilityRequestOptions
	// MCPEvents enables the experimental MCP Events helpers [TrackMCPEventSubscription] and
	// [WrapMCPEventSender]. Off by default.
	MCPEvents bool
	// Transport names the transport for mcp_client_seen. Streamable HTTP is detected; stdio
	// and SSE cannot be told apart from inside the server, so set TransportStdio or
	// TransportSSE for those.
	Transport string
}

// options is the validated form of Options.
type options struct {
	Options
	rules         map[string]ToolEventRule
	intents       []string
	userSentiment bool
	capabilities  []string
}

func normalizeOptions(o Options) (*options, error) {
	if o.Analytics == nil {
		return nil, errors.New("rudderstackai: Options.Analytics is required")
	}
	if o.ClientSeenWindow < 0 {
		return nil, errors.New("rudderstackai: Options.ClientSeenWindow must not be negative")
	}
	if o.ClientSeenWindow == 0 {
		o.ClientSeenWindow = 24 * time.Hour
	}
	switch o.Transport {
	case "", TransportStdio, TransportStreamableHTTP, TransportSSE:
	default:
		return nil, fmt.Errorf("rudderstackai: unknown Options.Transport %q", o.Transport)
	}
	rules := map[string]ToolEventRule{}
	for tool, rule := range o.EventRules.Tools {
		if rule.Event == "" {
			return nil, fmt.Errorf("rudderstackai: EventRules.Tools[%q]: event must be a non-empty string", tool)
		}
		if strings.HasPrefix(rule.Event, "ai_") || strings.HasPrefix(rule.Event, "mcp_") {
			return nil, fmt.Errorf("rudderstackai: EventRules.Tools[%q]: event names starting with ai_ or mcp_ are reserved (%s)", tool, rule.Event)
		}
		rules[tool] = rule
	}
	n := &options{Options: o, rules: rules, userSentiment: !o.RequestContext.DisableUserSentiment}
	if len(o.RequestContext.Intents) > 0 {
		n.intents = closedList(o.RequestContext.Intents)
	}
	if o.CapabilityRequests != nil {
		n.capabilities = closedList(o.CapabilityRequests.Capabilities)
	}
	return n, nil
}

// identity calls Identify and fills the defaults.
func (o *options) identity(ctx context.Context, token *auth.TokenInfo) (id Identity) {
	if o.Identify != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					debugf("identify panicked; request is anonymous")
					id = Identity{}
				}
			}()
			var err error
			if id, err = o.Identify(ctx, token); err != nil {
				debugf("identify failed; request is anonymous")
				id = Identity{}
			}
		}()
	}
	if token != nil {
		if id.UserID == "" {
			id.UserID = token.UserID
		}
		if id.TokenScopes == nil {
			id.TokenScopes = token.Scopes
		}
	}
	if id.AuthMethod == "" {
		id.AuthMethod = "none"
		if token != nil {
			id.AuthMethod = "oauth"
		}
	}
	return id
}
