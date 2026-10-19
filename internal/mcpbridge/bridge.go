// Package mcpbridge connects MCP library adapters outside the root package (ruddermcpgo) to the
// shared MCP engine without widening the public API: the root package sets Wrap in its init.
// Every adapter describes requests with these library-neutral types, so all of them build the
// same events.
package mcpbridge

import (
	"context"
	"encoding/json"
	"time"
)

// Set by package rudderstackai.
var (
	// Wrap registers one server instance with options (a rudderstackai.Options) and returns
	// its handle.
	Wrap func(name, version string, opts any) (Server, error)
	// ErrorType maps an error to an error_type.
	ErrorType func(err error) string
)

// Request is what an adapter knows about one incoming request.
type Request struct {
	Ctx context.Context
	// Meta is the request's _meta.
	Meta map[string]any
	// Token is the verified bearer token (*auth.TokenInfo from the go-sdk), or nil.
	Token any
	// SessionID is the reported mcp_session_id; empty when the transport has none.
	SessionID string
	// Connection scopes the call counters when there is no session ID and no caller
	// interaction_id, for a stdio or in-memory connection. Empty for HTTP.
	Connection string
	// Client is the client the session initialized with, if known.
	Client *Client
	// ProtocolVersion is the negotiated version, when the request's _meta has none.
	ProtocolVersion string
	RequestID       string
	// HTTP reports a streamable HTTP request.
	HTTP bool
}

// Client is a client implementation.
type Client struct {
	Name, Title, Version string
}

// Tool is one tool of a tools/list page, its input schema in generic JSON form.
type Tool struct {
	Name   string
	Schema map[string]any
}

// ToolDef is the request_capability tool the integration answers itself.
type ToolDef struct {
	Name, Description string
	Schema            map[string]any
	Reply             string
}

// Answer is one elicitation answer in a 2026-07-28 retry's inputResponses.
type Answer struct {
	Key, Action string
}

// Outcome is how a tools/call ended.
type Outcome struct {
	// Result is the result as sent, before JSON encoding; nil on error.
	Result any
	// ErrorType is set when the call failed with a protocol error.
	ErrorType string
	// Cancelled reports a call the client cancelled.
	Cancelled bool
	// InputRequired reports a 2026-07-28 input_required result; InputKeys are its elicitation
	// keys.
	InputRequired bool
	InputKeys     []string
	RequestState  string
}

// Elicitation is one finished server-to-client elicitation.
type Elicitation struct {
	Action, ErrorType string
	Duration          time.Duration
	HasDuration       bool
}

// Server is one wrapped server instance.
type Server interface {
	// Rewrap replaces the instance's options.
	Rewrap(opts any) error
	Begin(r Request) Call
	// CapabilityTool is the request_capability tool to offer, nil when off.
	CapabilityTool() *ToolDef
	RememberResources(byURI map[string]string)
	RememberTemplates(templates []string)
	// Elicited records an elicitation outside a tracked call (call is nil) or within one.
	Elicited(call Call, e Elicitation)
	// TrackSubscription and DeliveryDone back the MCP Events helpers; change is a
	// rudderstackai.SubscriptionChange, d a rudderstackai.EventDelivery and outcome a
	// rudderstackai.DeliveryOutcome.
	TrackSubscription(ctx context.Context, change any)
	DeliveryDone(ctx context.Context, d, outcome any, failed bool)
}

// Call is one request in flight.
type Call interface {
	// Started marks the start of the server's own handling.
	Started()
	// ToolCall records the start of a tools/call. strip reports a request_context argument
	// to remove before the handler runs; capability a request_capability call the integration
	// answers itself.
	ToolCall(name string, args map[string]json.RawMessage, progress bool, answers []Answer, requestState string) (strip, capability bool)
	// Capability records a request_capability call and returns its reply.
	Capability(args map[string]json.RawMessage) string
	ToolDone(o Outcome)
	// Finished records a resources/read or prompts/get; errorType is empty on success.
	Finished(method, name string, size int, errorType string)
	ResourceName(uri string) string
	// ListTools records a tools/list page and returns, per tool, the schema to send (nil keeps
	// the tool's own), and whether to append request_capability.
	ListTools(tools []Tool, lastPage bool) ([]map[string]any, bool)
	// Initialized reports a successful initialize.
	Initialized(client *Client, capabilities any, version string)
	// Succeeded reports a successful request; on 2026-07-28 it may send mcp_client_seen.
	Succeeded(method string, discoverVersions []string)
	ToolName() string
	RequestID() string
}
