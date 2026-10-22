// Package ruddermcpgo tracks MCP servers built on github.com/mark3labs/mcp-go with the
// RudderStack AI events: the same events, options, defaults and properties as
// rudderstackai.WrapMCPServer for the official go-sdk. It works through mcp-go's own extension
// points: server hooks (WithHooks / GetHooks) and a tool handler middleware (Use).
//
// Counters, client windows and learned schemas belong to the server name, shared by every
// server of that name in the process, so a server built per request keeps counting.
package ruddermcpgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/modelcontextprotocol/go-sdk/auth"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

const (
	capabilityTool = "request_capability"
	requestContext = "request_context"
	metaVersion    = "io.modelcontextprotocol/protocolVersion"
	// maxPending bounds the requests in flight that the integration remembers.
	maxPending = 10_000
)

// Options configures [WrapServer]. The fields are those of rudderstackai.Options; see there.
// Identify has no token argument: mcp-go does not verify bearer tokens, so read your own
// authentication from ctx (for example, set with server.WithHTTPContextFunc).
type Options struct {
	// Analytics is the RudderStack client. Required.
	Analytics analytics.Client
	// Identify maps a request to the user and workspace. Called once per request; if it fails
	// or panics, the request is anonymous.
	Identify func(ctx context.Context) (rudderstackai.Identity, error)
	// EventRules declares tool events. Names starting with "ai_" or "mcp_" are rejected.
	EventRules rudderstackai.EventRules
	// RequestContext configures the optional request_context argument, on by default.
	RequestContext rudderstackai.RequestContextOptions
	// ClientSeenWindow is the window in which a (client, OAuth client, user) is reported once
	// by mcp_client_seen, per server name and process. Zero means 24 hours.
	ClientSeenWindow time.Duration
	// CaptureContent sends call_reason and capability_detail, masked and capped at 200
	// characters. Off by default.
	CaptureContent bool
	// CapabilityRequests registers the request_capability tool, recorded as
	// mcp_capability_requested. Nil (the default) leaves it out.
	CapabilityRequests *rudderstackai.CapabilityRequestOptions
	// MCPEvents enables the experimental [TrackMCPEventSubscription] and
	// [WrapMCPEventSender]. Off by default.
	MCPEvents bool
	// Transport names the transport for mcp_client_seen. Streamable HTTP is detected; set
	// rudderstackai.TransportStdio or rudderstackai.TransportSSE for those.
	Transport string
	// ServerName overrides the server's own name, sent as server_name.
	ServerName string
	// Redact runs after the built-in masking of call_reason and capability_detail; if it fails
	// or panics, the field is not sent.
	Redact func(text string) (string, error)
	// DisableRedaction turns redaction off: the built-in masking and Redact.
	DisableRedaction bool
}

func (o Options) root() rudderstackai.Options {
	r := rudderstackai.Options{
		Analytics: o.Analytics, EventRules: o.EventRules, RequestContext: o.RequestContext,
		ClientSeenWindow: o.ClientSeenWindow, CaptureContent: o.CaptureContent,
		CapabilityRequests: o.CapabilityRequests, MCPEvents: o.MCPEvents, Transport: o.Transport,
		ServerName: o.ServerName, Redact: o.Redact, DisableRedaction: o.DisableRedaction,
	}
	if o.Identify != nil {
		identify := o.Identify
		r.Identify = func(ctx context.Context, _ *auth.TokenInfo) (rudderstackai.Identity, error) { return identify(ctx) }
	}
	return r
}

// tracked is one wrapped mcp-go server.
type tracked struct {
	srv mcpbridge.Server
	// capability is the request_capability tool this integration registered, if any.
	mu         sync.Mutex
	capability *mcp.Tool

	pending  sync.Mutex
	byMsg    map[any]*call
	byCtx    map[context.Context]*call
	sessions sync.Map // session ID -> sessionInfo, at most maxPending
	nSession int
}

type sessionInfo struct {
	client  *mcpbridge.Client
	version string
}

// call is one request between its hooks.
type call struct {
	c          mcpbridge.Call
	method     string
	name       string
	caps       any
	capability bool
}

var instances = struct {
	sync.Mutex
	m map[weak.Pointer[server.MCPServer]]*tracked
}{m: map[weak.Pointer[server.MCPServer]]*tracked{}}

func lookup(s *server.MCPServer) *tracked {
	instances.Lock()
	defer instances.Unlock()
	return instances.m[weak.Make(s)]
}

// WrapServer tracks an mcp-go server. Call it once at startup, after adding the server's own
// options and before serving. It adds server hooks and a tool handler middleware; requests and
// responses pass through unchanged except for the request_context argument (added to each
// tool's input schema in tools/list and removed before the tool runs) and, when enabled, the
// request_capability tool. Wrapping the same server again replaces its options.
func WrapServer(s *server.MCPServer, opts Options) error {
	if s == nil {
		return errors.New("ruddermcpgo: server is nil")
	}
	if t := lookup(s); t != nil {
		if err := t.srv.Rewrap(opts.root()); err != nil {
			return err
		}
		t.syncCapability(s)
		return nil
	}
	name, version := serverImpl(s)
	srv, err := mcpbridge.Wrap(name, version, opts.root())
	if err != nil {
		return err
	}
	t := &tracked{srv: srv, byMsg: map[any]*call{}, byCtx: map[context.Context]*call{}}
	wp := weak.Make(s)
	instances.Lock()
	instances.m[wp] = t
	instances.Unlock()
	runtime.AddCleanup(s, func(wp weak.Pointer[server.MCPServer]) {
		instances.Lock()
		defer instances.Unlock()
		delete(instances.m, wp)
	}, wp)
	hooks := s.GetHooks()
	if hooks == nil {
		hooks = &server.Hooks{}
		server.WithHooks(hooks)(s)
	}
	hooks.AddBeforeAny(t.beforeAny)
	hooks.AddOnSuccess(t.onSuccess)
	hooks.AddOnError(t.onError)
	s.Use(t.middleware)
	t.syncCapability(s)
	return nil
}

// serverImpl reads the name and version the server was created with; mcp-go has no getter.
func serverImpl(s *server.MCPServer) (name, version string) {
	safely(func() {
		v := reflect.ValueOf(s).Elem()
		name, version = v.FieldByName("name").String(), v.FieldByName("version").String()
	})
	return name, version
}

func safely(f func()) {
	defer func() {
		if r := recover(); r != nil {
			hook.Recovered(r, "safely")
		}
	}()
	f()
}

// syncCapability registers, updates or removes the integration's request_capability tool. A
// server that has its own keeps it. The registered schema accepts any arguments, so input
// validation never rejects a capability outside the list; tools/list shows the full one.
func (t *tracked) syncCapability(s *server.MCPServer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	def := t.srv.CapabilityTool()
	existing := s.GetTool(capabilityTool)
	ours := t.capability != nil && existing != nil && existing.Tool.Description == t.capability.Description
	if existing != nil && !ours {
		t.capability = nil
		return
	}
	if def == nil {
		if ours {
			s.DeleteTools(capabilityTool)
		}
		t.capability = nil
		return
	}
	readOnly := true
	listed := mcp.Tool{Name: def.Name, Description: def.Description, Annotations: mcp.ToolAnnotation{ReadOnlyHint: &readOnly}}
	listed.RawInputSchema, _ = json.Marshal(def.Schema)
	t.capability = &listed
	reply := def.Reply
	registered := mcp.Tool{Name: def.Name, Description: def.Description, Annotations: listed.Annotations,
		RawInputSchema: json.RawMessage(`{"type":"object"}`)}
	s.AddTool(registered, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(reply), nil
	})
}

func (t *tracked) ownCapability() *mcp.Tool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.capability
}

// envelope is the part of a request the integration reads, decoded from its JSON form so it
// works the same across mcp-go versions.
type envelope struct {
	Params struct {
		Meta           map[string]any             `json:"_meta"`
		Name           string                     `json:"name"`
		URI            string                     `json:"uri"`
		Arguments      json.RawMessage            `json:"arguments"`
		InputResponses map[string]json.RawMessage `json:"inputResponses"`
		RequestState   string                     `json:"requestState"`
		ClientInfo     *mcpbridge.Client          `json:"clientInfo"`
		Capabilities   any                        `json:"capabilities"`
	} `json:"params"`
}

func headerOf(message any) http.Header {
	var h http.Header
	safely(func() {
		v := reflect.ValueOf(message)
		for v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		if f := v.FieldByName("Header"); f.IsValid() {
			h, _ = f.Interface().(http.Header)
		}
	})
	return h
}

func requestID(id any) string {
	switch v := id.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (t *tracked) beforeAny(ctx context.Context, id any, method mcp.MCPMethod, message any) {
	safely(func() { t.before(ctx, id, string(method), message) })
}

func (t *tracked) before(ctx context.Context, id any, method string, message any) {
	if strings.HasPrefix(method, "notifications/") {
		return
	}
	b, err := json.Marshal(message)
	if err != nil {
		return
	}
	var env envelope
	if json.Unmarshal(b, &env) != nil {
		return
	}
	header := headerOf(message)
	r := mcpbridge.Request{Ctx: ctx, Meta: env.Params.Meta, RequestID: requestID(id), HTTP: len(header) > 0}
	sessionID := ""
	if session := server.ClientSessionFromContext(ctx); session != nil {
		sessionID = session.SessionID()
		if ci, ok := session.(server.SessionWithClientInfo); ok {
			if info := ci.GetClientInfo(); info.Name != "" {
				r.Client = &mcpbridge.Client{Name: info.Name, Version: info.Version}
			}
		}
		if sessionID == "stdio" || (sessionID == "" && !r.HTTP) {
			// One stdio connection per session object; its fixed ID is not a session ID.
			r.Connection = fmt.Sprintf("%p", session)
			sessionID = ""
		}
	}
	if sessionID == "" && r.HTTP {
		sessionID = header.Get("Mcp-Session-Id")
	}
	r.SessionID = sessionID
	key := sessionID
	if key == "" {
		key = r.Connection
	}
	if v, ok := t.sessions.Load(key); ok && key != "" {
		info := v.(sessionInfo)
		if r.Client == nil {
			r.Client = info.client
		}
		r.ProtocolVersion = info.version
	}
	if r.ProtocolVersion == "" && r.HTTP {
		r.ProtocolVersion = header.Get("Mcp-Protocol-Version")
	}
	c := &call{c: t.srv.Begin(r), method: method}
	switch method {
	case "tools/call":
		t.toolCall(c, &env, message)
	case "resources/read":
		c.name = c.c.ResourceName(env.Params.URI)
	case "prompts/get":
		c.name = env.Params.Name
	case "initialize":
		c.name, c.caps = key, env.Params.Capabilities
		if env.Params.ClientInfo != nil {
			t.remember(key, sessionInfo{client: env.Params.ClientInfo})
		}
	}
	c.c.Started()
	t.pending.Lock()
	defer t.pending.Unlock()
	if len(t.byMsg) < maxPending {
		t.byMsg[message] = c
		t.byCtx[ctx] = c
	}
}

func (t *tracked) remember(key string, info sessionInfo) {
	if key == "" {
		return
	}
	t.pending.Lock()
	defer t.pending.Unlock()
	if _, ok := t.sessions.Load(key); !ok {
		if t.nSession >= maxPending {
			return
		}
		t.nSession++
	}
	t.sessions.Store(key, info)
}

func (t *tracked) toolCall(c *call, env *envelope, message any) {
	var args map[string]json.RawMessage
	if len(env.Params.Arguments) > 0 && json.Unmarshal(env.Params.Arguments, &args) != nil {
		args = nil
	}
	var answers []mcpbridge.Answer
	for k, raw := range env.Params.InputResponses {
		var r struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(raw, &r)
		answers = append(answers, mcpbridge.Answer{Key: k, Action: r.Action})
	}
	_, progress := env.Params.Meta["progressToken"]
	strip, capability := c.c.ToolCall(env.Params.Name, args, progress, answers, env.Params.RequestState)
	if capability && t.ownCapability() != nil {
		c.capability = true
		c.c.Capability(args)
		return
	}
	req, ok := message.(*mcp.CallToolRequest)
	if !strip || !ok {
		return
	}
	// Removed before input validation and the handler, on the request mcp-go goes on with.
	stripped := map[string]any{}
	for k, raw := range args {
		if k == requestContext {
			continue
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		stripped[k] = v
	}
	req.Params.Arguments = stripped
	safely(func() {
		if f := reflect.ValueOf(&req.Params).Elem().FieldByName("RawArguments"); f.IsValid() && f.CanSet() {
			b, _ := json.Marshal(stripped)
			f.Set(reflect.ValueOf(json.RawMessage(b)))
		}
	})
}

// take forgets a request at its end. mcp-go runs the hooks of one request with one context.
func (t *tracked) take(ctx context.Context, message any) *call {
	t.pending.Lock()
	defer t.pending.Unlock()
	c := t.byMsg[message]
	delete(t.byMsg, message)
	if t.byCtx[ctx] == c {
		delete(t.byCtx, ctx)
	}
	return c
}

// middleware hands the request's call to the handler's context, for RequestElicitation.
func (t *tracked) middleware(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t.pending.Lock()
		c := t.byCtx[ctx]
		t.pending.Unlock()
		if c != nil {
			ctx = context.WithValue(ctx, callKey{}, c.c)
		}
		return next(ctx, req)
	}
}

type callKey struct{}

func (t *tracked) onSuccess(ctx context.Context, _ any, method mcp.MCPMethod, message any, result any) {
	c := t.take(ctx, message)
	if c == nil {
		return
	}
	safely(func() { t.succeeded(ctx, c, result) })
}

func (t *tracked) succeeded(ctx context.Context, c *call, result any) {
	var res map[string]any
	if b, err := json.Marshal(result); err == nil {
		_ = json.Unmarshal(b, &res)
	}
	var discover []string
	if v, ok := res["supportedVersions"].([]any); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				discover = append(discover, s)
			}
		}
	}
	c.c.Succeeded(c.method, discover)
	switch c.method {
	case "initialize":
		version, _ := res["protocolVersion"].(string)
		var client *mcpbridge.Client
		if v, ok := t.sessions.Load(c.name); ok && c.name != "" {
			info := v.(sessionInfo)
			client = info.client
			info.version = version
			t.remember(c.name, info)
		}
		c.c.Initialized(client, c.caps, version)
	case "tools/list":
		if r, ok := result.(*mcp.ListToolsResult); ok && r != nil {
			t.toolsList(c, r)
		}
	case "resources/list":
		byURI := map[string]string{}
		if list, ok := res["resources"].([]any); ok {
			for _, x := range list {
				m, _ := x.(map[string]any)
				uri, _ := m["uri"].(string)
				name, _ := m["name"].(string)
				byURI[uri] = name
			}
		}
		t.srv.RememberResources(byURI)
	case "resources/templates/list":
		var templates []string
		if list, ok := res["resourceTemplates"].([]any); ok {
			for _, x := range list {
				m, _ := x.(map[string]any)
				if u, ok := m["uriTemplate"].(string); ok {
					templates = append(templates, u)
				}
			}
		}
		t.srv.RememberTemplates(templates)
	case "tools/call":
		if c.capability {
			return
		}
		o := mcpbridge.Outcome{Result: result}
		if reqs, ok := res["inputRequests"].(map[string]any); ok {
			o.InputRequired = true
			o.RequestState, _ = res["requestState"].(string)
			for key, v := range reqs {
				if m, _ := v.(map[string]any); m["method"] == "elicitation/create" {
					o.InputKeys = append(o.InputKeys, key)
				}
			}
		} else if res == nil {
			o.Result = nil
		} else if isError, _ := res["isError"].(bool); isError && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			o.Cancelled = true
		}
		c.c.ToolDone(o)
	case "resources/read", "prompts/get":
		b, _ := json.Marshal(result)
		c.c.Finished(c.method, c.name, len(b), "")
	}
}

func (t *tracked) onError(ctx context.Context, _ any, method mcp.MCPMethod, message any, err error) {
	c := t.take(ctx, message)
	if c == nil {
		return
	}
	safely(func() {
		kind := errorType(ctx, err)
		switch c.method {
		case "tools/call":
			if !c.capability {
				c.c.ToolDone(mcpbridge.Outcome{ErrorType: kind, Cancelled: kind == "cancelled"})
			}
		case "resources/read", "prompts/get":
			c.c.Finished(c.method, c.name, 0, kind)
		}
	})
}

// errorType maps an mcp-go request error to an error_type: by its JSON-RPC code, else by the
// handler error it wraps.
func errorType(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled"
	}
	var wire interface{ ToJSONRPCError() mcp.JSONRPCError }
	if errors.As(err, &wire) {
		code := wire.ToJSONRPCError().Error.Code
		// An internal error carries the handler's own error: map that instead.
		if inner := errors.Unwrap(err); code == mcp.INTERNAL_ERROR && inner != nil {
			return rootErrorType(inner)
		}
		return rootErrorType(codeError(code))
	}
	return rootErrorType(err)
}

type codeError int

func (e codeError) Error() string    { return "mcp error " + strconv.Itoa(int(e)) }
func (e codeError) ErrorCode() int64 { return int64(e) }

// rootErrorType is the shared mapping, through the bridge.
func rootErrorType(err error) string { return mcpbridge.ErrorType(err) }

func (t *tracked) toolsList(c *call, r *mcp.ListToolsResult) {
	own := t.ownCapability()
	listed := make([]mcpbridge.Tool, 0, len(r.Tools))
	index := make([]int, 0, len(r.Tools))
	for i, tool := range r.Tools {
		if own != nil && tool.Name == capabilityTool {
			r.Tools[i] = *own
			continue
		}
		var m map[string]any
		if b, err := json.Marshal(tool); err == nil {
			_ = json.Unmarshal(b, &m)
		}
		schema, _ := m["inputSchema"].(map[string]any)
		listed = append(listed, mcpbridge.Tool{Name: tool.Name, Schema: schema})
		index = append(index, i)
	}
	schemas, _ := c.c.ListTools(listed, true)
	tools := slices.Clone(r.Tools)
	for j, schema := range schemas {
		if schema == nil {
			continue
		}
		b, err := json.Marshal(schema)
		if err != nil {
			continue
		}
		cp := tools[index[j]]
		cp.InputSchema = mcp.ToolInputSchema{}
		cp.RawInputSchema = b
		tools[index[j]] = cp
	}
	// mcp-go encodes the result after the hooks return: the copy is what the client gets.
	r.Tools = tools
}

// RequestElicitation calls s.RequestElicitation and records the outcome as
// mcp_elicitation_completed: the client's action, or cancel with an error_type. Call it from a
// tool handler, with the handler's context. Elicitations mcp-go sends itself (answering an
// input_required result for a pre-2026-07-28 client) are not seen.
func RequestElicitation(ctx context.Context, s *server.MCPServer, request mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	start := time.Now()
	res, err := s.RequestElicitation(ctx, request)
	safely(func() {
		t := lookup(s)
		if t == nil {
			return
		}
		c, _ := ctx.Value(callKey{}).(mcpbridge.Call)
		e := mcpbridge.Elicitation{Action: "cancel", Duration: time.Since(start), HasDuration: true}
		switch {
		case errors.Is(err, server.ErrElicitationNotSupported), errors.Is(err, server.ErrNoActiveSession):
			e.ErrorType, e.HasDuration = "invalid_request", false
		case err != nil:
			e.ErrorType = errorType(context.Background(), err)
		case res != nil && validAction(string(res.Action)):
			e.Action = string(res.Action)
		default:
			e.ErrorType = "api_error"
		}
		t.srv.Elicited(c, e)
	})
	return res, err
}

func validAction(a string) bool { return a == "accept" || a == "decline" || a == "cancel" }

// TrackMCPEventSubscription records an MCP Events subscription change as
// mcp_event_subscription_changed; see rudderstackai.TrackMCPEventSubscription. Needs
// Options.MCPEvents. Experimental.
func TrackMCPEventSubscription(ctx context.Context, s *server.MCPServer, change rudderstackai.SubscriptionChange) {
	safely(func() {
		if t := lookup(s); t != nil {
			t.srv.TrackSubscription(ctx, change)
		}
	})
}

// WrapMCPEventSender wraps your webhook sender so each finished delivery is recorded as
// mcp_event_delivered; see rudderstackai.WrapMCPEventSender. Needs Options.MCPEvents.
// Experimental.
func WrapMCPEventSender(s *server.MCPServer, sender rudderstackai.EventSender) rudderstackai.EventSender {
	return func(ctx context.Context, d rudderstackai.EventDelivery) (rudderstackai.DeliveryOutcome, error) {
		outcome, err := sender(ctx, d)
		safely(func() {
			if t := lookup(s); t != nil {
				t.srv.DeliveryDone(ctx, d, outcome, err != nil)
			}
		})
		return outcome, err
	}
}
