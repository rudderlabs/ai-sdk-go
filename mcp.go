package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	capabilityTool  = "request_capability"
	requestContext  = "request_context"
	maxEntries      = 10_000
	protocol2026    = "2026-07-28"
	legacyProtocol  = "2025-11-25"
	capabilityReply = "Recorded for the server owner. No tool here can do this yet; tell the user."
)

var sentiments = []string{"negative", "neutral", "positive"}

// owned are the properties the integration sets; a declared tool rule cannot override them.
var owned = map[string]bool{
	"mcp_session_id": true, "server_name": true, "account_id": true, "client_name": true,
	"client_version": true, "protocol_version": true, "interaction_id": true, "trace_id": true,
	"tool_name": true, "tool_status": true,
}

// wrapped is one tracked server.
type wrapped struct {
	opts    atomic.Pointer[options]
	name    string
	version string
}

var registry sync.Map // *mcp.Server -> *wrapped

// serverMemory is shared by every server with the same name, so per-request servers agree.
type serverMemory struct {
	toolSchemas          map[string]map[string]any
	nativeContext        map[string]bool
	nativeCapabilityTool bool
	resources            map[string]string
	templates            []resourceTemplate
}

type resourceTemplate struct {
	template string
	matches  func(string) bool
}

type scopeState struct {
	calls  int
	last   time.Time
	hashes map[string]int
}

type pendingInput struct {
	start     time.Time
	requestID string
	toolName  string
}

var memory = struct {
	sync.Mutex
	servers       map[string]*serverMemory
	clientSeen    *boundedMap[time.Time]
	scopes        *boundedMap[*scopeState]
	inputRequired *boundedMap[pendingInput]
	expiries      map[string]*time.Timer
}{
	servers:       map[string]*serverMemory{},
	clientSeen:    newBoundedMap[time.Time](maxEntries),
	scopes:        newBoundedMap[*scopeState](maxEntries),
	inputRequired: newBoundedMap[pendingInput](maxEntries),
	expiries:      map[string]*time.Timer{},
}

// serverMem returns the memory of a server name; the caller holds memory.Mutex.
func serverMem(name string) *serverMemory {
	m := memory.servers[name]
	if m == nil {
		m = &serverMemory{
			toolSchemas:   map[string]map[string]any{},
			nativeContext: map[string]bool{},
			resources:     map[string]string{},
		}
		memory.servers[name] = m
	}
	return m
}

// resetMemory forgets the in-memory windows, counters, caches and expiry timers.
func resetMemory() {
	memory.Lock()
	defer memory.Unlock()
	memory.servers = map[string]*serverMemory{}
	memory.clientSeen.clear()
	memory.scopes.clear()
	memory.inputRequired.clear()
	for _, t := range memory.expiries {
		t.Stop()
	}
	memory.expiries = map[string]*time.Timer{}
}

// WrapMCPServer tracks an MCP server built on github.com/modelcontextprotocol/go-sdk. Call it
// once at startup, before serving. It adds a receiving and a sending middleware; requests and
// responses pass through unchanged except for the request_context argument (added to each
// tool's input schema and removed before the tool runs) and, when enabled, the
// request_capability tool. Wrapping the same server again replaces its options.
//
// It returns an error for invalid options, such as a missing client or a declared tool event
// named with a reserved prefix.
func WrapMCPServer(server *mcp.Server, opts Options) error {
	if server == nil {
		return errors.New("rudderstackai: server is nil")
	}
	n, err := normalizeOptions(opts)
	if err != nil {
		return err
	}
	if existing, ok := registry.Load(server); ok {
		existing.(*wrapped).opts.Store(n)
		return nil
	}
	w := &wrapped{}
	w.name, w.version = serverImpl(server)
	w.opts.Store(n)
	registry.Store(server, w)
	server.AddReceivingMiddleware(w.receiving)
	server.AddSendingMiddleware(w.sending)
	return nil
}

// serverImpl reads the name and version the server was created with; the SDK has no getter.
func serverImpl(s *mcp.Server) (name, version string) {
	name = "unknown"
	safely(func() {
		impl := reflect.ValueOf(s).Elem().FieldByName("impl")
		if !impl.IsValid() || impl.Kind() != reflect.Pointer || impl.IsNil() {
			return
		}
		if n := impl.Elem().FieldByName("Name").String(); n != "" {
			name = n
		}
		version = impl.Elem().FieldByName("Version").String()
	})
	return name, version
}

// reqCtx is everything known about one request, for the events it causes.
type reqCtx struct {
	w         *wrapped
	opts      *options
	base      map[string]any
	who       who
	identity  Identity
	requestID string
	// scope is "session:<id>", else "interaction:<caller's id>"; empty when the server minted it.
	scope    string
	toolName string
}

type reqCtxKey struct{}

// elicitFlagKey marks a context in which an elicitation/create request reached the client.
type elicitFlagKey struct{}

// handling is the state of one request between the two halves of the middleware.
type handling struct {
	rc       *reqCtx
	method   string
	name     string
	req      mcp.Request // replaces the request, when request_context was stripped
	reply    mcp.Result  // answers the request without calling the server
	args     map[string]any
	props    map[string]any
	start    time.Time
	init     *mcp.InitializeParams
	answered bool
	session  *mcp.ServerSession
	reqExtra *mcp.RequestExtra
}

func (w *wrapped) receiving(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		var h *handling
		safely(func() { h = w.before(ctx, method, req) })
		if h == nil {
			return next(ctx, method, req)
		}
		if h.reply != nil {
			return h.reply, nil
		}
		if h.req != nil {
			req = h.req
		}
		ctx = context.WithValue(ctx, reqCtxKey{}, h.rc)
		h.start = time.Now()
		res, err := next(ctx, method, req)
		safely(func() {
			if replaced := w.after(ctx, h, res, err); replaced != nil {
				res = replaced
			}
		})
		return res, err
	}
}

func metaOf(p mcp.Params) (meta map[string]any) {
	safely(func() {
		if p != nil {
			meta = p.GetMeta()
		}
	})
	return meta
}

func (w *wrapped) before(ctx context.Context, method string, req mcp.Request) *handling {
	if strings.HasPrefix(method, "notifications/") {
		return nil
	}
	opts := w.opts.Load()
	ss, _ := req.GetSession().(*mcp.ServerSession)
	extra := req.GetExtra()
	params := req.GetParams()
	meta := metaOf(params)
	id := opts.identity(ctx, tokenOf(extra))
	rc := w.context(ctx, opts, ss, extra, meta, id)
	h := &handling{rc: rc, method: method, props: map[string]any{}, session: ss, reqExtra: extra}
	if method == "initialize" {
		h.init, _ = params.(*mcp.InitializeParams)
		return h
	}
	if _, ok := meta[metaProtocolVersion]; ok {
		w.newProtocolClient(rc, extra, meta)
	} else if _, ok := meta[metaClientInfo]; ok {
		w.newProtocolClient(rc, extra, meta)
	}
	switch r := req.(type) {
	case *mcp.CallToolRequest:
		w.toolCall(h, r)
	case *mcp.ReadResourceRequest:
		h.name = w.resourceName(r.Params.URI)
	case *mcp.GetPromptRequest:
		h.name = r.Params.Name
		if h.name == "" {
			h.name = "unknown"
		}
	}
	return h
}

func (w *wrapped) newProtocolClient(rc *reqCtx, extra *mcp.RequestExtra, meta map[string]any) {
	client := implementation(meta[metaClientInfo])
	w.clientSeen(rc, extra, client, capabilityNames(meta[metaClientCapabilities]))
}

func implementation(v any) *mcp.Implementation {
	var impl mcp.Implementation
	b, err := compactJSON(v)
	if err != nil || v == nil || json.Unmarshal(b, &impl) != nil {
		return nil
	}
	return &impl
}

func (w *wrapped) context(ctx context.Context, opts *options, ss *mcp.ServerSession, extra *mcp.RequestExtra, meta map[string]any, id Identity) *reqCtx {
	client := implementation(meta[metaClientInfo])
	var ip *mcp.InitializeParams
	if ss != nil {
		ip = ss.InitializeParams()
	}
	if client == nil && ip != nil {
		client = ip.ClientInfo
	}
	sessionID := sessionIDOf(ss, extra)
	interactionID, traceID := joinIDs(meta)
	base := map[string]any{}
	put(base, "mcp_session_id", sessionID)
	base["server_name"] = w.name
	put(base, "account_id", id.AccountID)
	if client != nil {
		put(base, "client_name", client.Name)
		put(base, "client_version", client.Version)
	}
	put(base, "protocol_version", protocolVersion(ss, extra, meta))
	base["interaction_id"] = interactionID
	if interactionID == "" {
		base["interaction_id"] = mintID()
	}
	put(base, "trace_id", traceID)
	rc := &reqCtx{w: w, opts: opts, base: base, who: who{id.UserID, id.AnonymousID}, identity: id, requestID: requestIDOf(ctx)}
	switch {
	case sessionID != "":
		rc.scope = "session:" + sessionID
	case interactionID != "":
		rc.scope = "interaction:" + interactionID
	}
	return rc
}

func tokenOf(extra *mcp.RequestExtra) (token *auth.TokenInfo) {
	if extra != nil {
		token = extra.TokenInfo
	}
	return token
}

func sessionIDOf(ss *mcp.ServerSession, extra *mcp.RequestExtra) string {
	if ss != nil {
		if id := ss.ID(); id != "" {
			return id
		}
	}
	if extra != nil && extra.Header != nil {
		return extra.Header.Get("Mcp-Session-Id")
	}
	return ""
}

// negotiated holds the protocol version each session negotiated at initialize, without
// keeping the session alive.
var negotiated = struct {
	sync.Mutex
	m map[weak.Pointer[mcp.ServerSession]]string
}{m: map[weak.Pointer[mcp.ServerSession]]string{}}

func rememberNegotiated(ss *mcp.ServerSession, version string) {
	if ss == nil || version == "" {
		return
	}
	wp := weak.Make(ss)
	negotiated.Lock()
	defer negotiated.Unlock()
	if _, ok := negotiated.m[wp]; !ok {
		runtime.AddCleanup(ss, func(wp weak.Pointer[mcp.ServerSession]) {
			negotiated.Lock()
			defer negotiated.Unlock()
			delete(negotiated.m, wp)
		}, wp)
	}
	negotiated.m[wp] = version
}

// protocolVersion is the request's _meta version (2026-07-28), else the session's negotiated
// one, else the Mcp-Protocol-Version header.
func protocolVersion(ss *mcp.ServerSession, extra *mcp.RequestExtra, meta map[string]any) string {
	if v := str(meta[metaProtocolVersion]); v != "" {
		return v
	}
	if ss != nil {
		negotiated.Lock()
		v := negotiated.m[weak.Make(ss)]
		negotiated.Unlock()
		if v != "" {
			return v
		}
	}
	if extra != nil && extra.Header != nil {
		return extra.Header.Get("Mcp-Protocol-Version")
	}
	return ""
}

// idKeyType is the go-sdk's unexported context key type for the JSON-RPC request ID.
var idKeyType atomic.Pointer[reflect.Type]

// requestIDOf reads the JSON-RPC request ID the go-sdk stores in the request context. Its key
// type is unexported, so it is found once by walking the context chain.
func requestIDOf(ctx context.Context) (id string) {
	id = "unknown"
	safely(func() {
		t := idKeyType.Load()
		if t == nil {
			found := findIDKeyType(ctx)
			if found == nil {
				return
			}
			idKeyType.Store(&found)
			t = &found
		}
		if v, ok := ctx.Value(reflect.Zero(*t).Interface()).(jsonrpc.ID); ok && v.IsValid() {
			switch raw := v.Raw().(type) {
			case string:
				id = raw
			case int64:
				id = formatInt(raw)
			}
		}
	})
	return id
}

func findIDKeyType(ctx context.Context) reflect.Type {
	v := reflect.ValueOf(ctx)
	for range 1000 {
		for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
			if v.IsNil() {
				return nil
			}
			v = v.Elem()
		}
		if !v.IsValid() || v.Kind() != reflect.Struct {
			return nil
		}
		if k := v.FieldByName("key"); k.IsValid() && k.Kind() == reflect.Interface && !k.IsNil() {
			t := k.Elem().Type()
			if t.Name() == "idContextKey" && t.PkgPath() == "github.com/modelcontextprotocol/go-sdk/mcp" {
				return t
			}
		}
		v = v.FieldByName("Context")
	}
	return nil
}

func formatInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func transportOf(opts *options, extra *mcp.RequestExtra) string {
	if extra != nil && extra.Header != nil {
		return TransportStreamableHTTP
	}
	return opts.Transport
}

func (w *wrapped) clientSeen(rc *reqCtx, extra *mcp.RequestExtra, client *mcp.Implementation, caps []string) {
	var clientName, clientTitle, clientVersion string
	if client != nil {
		clientName, clientTitle, clientVersion = client.Name, client.Title, client.Version
	}
	user := rc.who.userID
	if user == "" {
		user = rc.who.anonymousID
	}
	key := strings.Join([]string{w.name, clientName, rc.identity.OAuthClientID, user}, "\x00")
	now := time.Now()
	memory.Lock()
	until, seen := memory.clientSeen.get(key)
	if seen && until.After(now) {
		memory.Unlock()
		return
	}
	memory.clientSeen.set(key, now.Add(rc.opts.ClientSeenWindow))
	memory.Unlock()
	p := map[string]any{}
	put(p, "mcp_session_id", str(rc.base["mcp_session_id"]))
	p["server_name"] = w.name
	put(p, "server_version", w.version)
	put(p, "client_name", clientName)
	put(p, "client_title", clientTitle)
	put(p, "client_version", clientVersion)
	if caps != nil {
		p["client_capabilities"] = caps
	}
	put(p, "protocol_version", str(rc.base["protocol_version"]))
	put(p, "transport", transportOf(rc.opts, extra))
	put(p, "auth_method", rc.identity.AuthMethod)
	put(p, "oauth_client_id", rc.identity.OAuthClientID)
	if rc.identity.TokenScopes != nil {
		p["token_scopes"] = rc.identity.TokenScopes
	}
	put(p, "country", rc.identity.Country)
	put(p, "account_id", rc.identity.AccountID)
	emit(rc.opts.Analytics, "mcp_client_seen", rc.who, p)
}

func (w *wrapped) toolCall(h *handling, r *mcp.CallToolRequest) {
	rc, opts, p := h.rc, h.rc.opts, r.Params
	h.name = p.Name
	if h.name == "" {
		h.name = "unknown"
	}
	rc.toolName = h.name
	memory.Lock()
	sm := serverMem(w.name)
	nativeCap, nativeContext := sm.nativeCapabilityTool, sm.nativeContext[h.name]
	memory.Unlock()
	if h.name == capabilityTool && opts.capabilities != nil && !nativeCap {
		h.reply = w.capabilityRequested(rc, p.Arguments)
		return
	}
	var raw map[string]json.RawMessage
	if len(p.Arguments) > 0 && json.Unmarshal(p.Arguments, &raw) == nil && raw != nil {
		if reported, ok := raw[requestContext]; ok && !opts.RequestContext.Disabled && !nativeContext {
			delete(raw, requestContext)
			if stripped, err := json.Marshal(raw); err == nil {
				cp := *p
				cp.Arguments = stripped
				h.req = &mcp.CallToolRequest{Session: r.Session, Params: &cp, Extra: r.Extra}
				var v any
				_ = json.Unmarshal(reported, &v)
				for k, val := range w.reportedContext(opts, v) {
					h.props[k] = val
				}
			}
		}
		h.args = map[string]any{}
		keys := make([]string, 0, len(raw))
		for k, val := range raw {
			var x any
			_ = json.Unmarshal(val, &x)
			h.args[k] = x
			keys = append(keys, k)
		}
		sort.Strings(keys)
		h.props["argument_keys"] = keys
	}
	_, progress := p.Meta["progressToken"]
	h.props["progress_requested"] = progress
	h.answered = len(p.InputResponses) > 0
	if h.answered {
		w.inputResponses(rc, p, h.name)
	}
	if rc.scope == "" {
		return
	}
	start := time.Now()
	key := w.name + "\x00" + rc.scope
	memory.Lock()
	defer memory.Unlock()
	s, ok := memory.scopes.get(key)
	if !ok {
		s = &scopeState{hashes: map[string]int{}}
	}
	h.props["call_index"] = s.calls
	s.calls++
	if !s.last.IsZero() {
		h.props["ms_since_previous_call"] = max(0, start.Sub(s.last).Milliseconds())
	}
	s.last = start
	if !h.answered {
		hash := argumentsHash(h.name, h.args)
		h.props["retry_count"] = s.hashes[hash]
		s.hashes[hash]++
	}
	memory.scopes.set(key, s)
}

func (w *wrapped) reportedContext(opts *options, v any) map[string]any {
	out := map[string]any{}
	m, ok := v.(map[string]any)
	if !ok {
		return out
	}
	if intent := str(m["intent"]); opts.intents != nil && intent != "" {
		if slices.Contains(opts.intents, intent) {
			out["intent"] = intent
		} else {
			out["intent"] = "other"
		}
	}
	if s := str(m["user_sentiment"]); opts.userSentiment && slices.Contains(sentiments, s) {
		out["user_sentiment"] = s
	}
	if reason, ok := m["call_reason"].(string); ok && opts.CaptureContent {
		if masked, ok := maskText(strings.TrimSpace(reason)); ok && masked != "" {
			out["call_reason"] = capText(masked, maxTextLength)
		}
	}
	return out
}

func (w *wrapped) capabilityRequested(rc *reqCtx, arguments json.RawMessage) mcp.Result {
	var args map[string]any
	_ = json.Unmarshal(arguments, &args)
	capability := "other"
	if c := str(args["capability"]); c != "" && slices.Contains(rc.opts.capabilities, c) {
		capability = c
	}
	p := map[string]any{}
	for k, v := range rc.base {
		p[k] = v
	}
	p["capability"] = capability
	if detail, ok := args["detail"].(string); ok && capability == "other" && rc.opts.CaptureContent {
		// Withheld, not sent, when it still looks sensitive after masking.
		if masked, ok := maskText(strings.TrimSpace(detail)); ok && masked != "" && !hasSensitive(masked) {
			p["capability_detail"] = capText(masked, maxTextLength)
		}
	}
	emit(rc.opts.Analytics, "mcp_capability_requested", rc.who, p)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: capabilityReply}}}
}

func (w *wrapped) resourceName(uri string) string {
	memory.Lock()
	defer memory.Unlock()
	sm := serverMem(w.name)
	for _, t := range sm.templates {
		if t.matches(uri) {
			return t.template
		}
	}
	if name := sm.resources[uri]; name != "" {
		return name
	}
	return "unknown"
}

func (w *wrapped) after(ctx context.Context, h *handling, res mcp.Result, err error) mcp.Result {
	switch h.method {
	case "initialize":
		if r, ok := res.(*mcp.InitializeResult); ok && err == nil && r != nil {
			rememberNegotiated(h.session, r.ProtocolVersion)
			put(h.rc.base, "protocol_version", r.ProtocolVersion)
			var client *mcp.Implementation
			var caps []string
			if h.init != nil {
				client = h.init.ClientInfo
				caps = capabilityNames(h.init.Capabilities)
			}
			w.clientSeen(h.rc, h.reqExtra, client, caps)
		}
	case "tools/list":
		if r, ok := res.(*mcp.ListToolsResult); ok && err == nil && r != nil {
			if replaced := w.toolsList(h.rc, r); replaced != nil {
				return replaced
			}
		}
	case "resources/list", "resources/templates/list":
		if err == nil {
			w.rememberResources(res)
		}
	case "tools/call":
		w.toolCallDone(ctx, h, res, err)
	case "resources/read", "prompts/get":
		event, nameKey := "mcp_resource_read", "resource_name"
		if h.method == "prompts/get" {
			event, nameKey = "mcp_prompt_used", "prompt_name"
		}
		extra := map[string]any{}
		status := "success"
		switch {
		case wasCanceled(ctx, err):
			status = "error"
			extra["error_type"] = "cancelled"
		case err != nil:
			status = "error"
			extra["error_type"] = errorType(err)
		default:
			extra["result_size_bytes"] = byteSize(res)
		}
		w.finish(h, event, nameKey, status, extra)
	}
	return nil
}

func wasCanceled(ctx context.Context, err error) bool {
	return errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled)
}

func (w *wrapped) toolCallDone(ctx context.Context, h *handling, res mcp.Result, err error) {
	r, _ := res.(*mcp.CallToolResult)
	extra := map[string]any{}
	status := "success"
	var result map[string]any
	switch {
	case wasCanceled(ctx, err):
		status = "error"
		extra["error_type"] = "cancelled"
		extra["time_to_cancel_ms"] = time.Since(h.start).Milliseconds()
	case err != nil:
		status = "error"
		extra["error_type"] = errorType(err)
	case r == nil:
		status = "error"
		extra["error_type"] = "api_error"
	case r.InputRequests != nil:
		// A 2026-07-28 input_required leg: the call goes on in the client's retry.
		w.inputRequired(h, r)
		return
	default:
		b, _ := compactJSON(r)
		result, _ = toJSONValue(r).(map[string]any)
		for k, v := range resultStats(result, len(b)) {
			extra[k] = v
		}
		extra["is_error_result"] = r.IsError
		if r.IsError {
			status = "error"
			memory.Lock()
			schema := serverMem(w.name).toolSchemas[h.name]
			memory.Unlock()
			invalid := invalidArguments(schema, h.args)
			if len(invalid) > 0 {
				extra["invalid_arguments"] = invalid
				extra["error_type"] = "invalid_request"
			} else {
				extra["error_type"] = "api_error"
			}
		}
	}
	w.finish(h, "mcp_tool_called", "tool_name", status, extra)
	w.declaredEvent(h, result, status)
}

func (w *wrapped) finish(h *handling, event, nameKey, status string, extra map[string]any) {
	end := time.Now()
	p := map[string]any{}
	for k, v := range h.rc.base {
		p[k] = v
	}
	p["request_id"] = h.rc.requestID
	p[nameKey] = h.name
	p["tool_status"] = status
	if _, cancel := extra["time_to_cancel_ms"]; !cancel {
		p["duration_ms"] = end.Sub(h.start).Milliseconds()
	}
	if h.method == "tools/call" {
		for k, v := range h.props {
			p[k] = v
		}
		if h.rc.scope != "" {
			memory.Lock()
			if s, ok := memory.scopes.get(w.name + "\x00" + h.rc.scope); ok {
				s.last = end
			}
			memory.Unlock()
		}
	}
	for k, v := range extra {
		p[k] = v
	}
	emit(h.rc.opts.Analytics, event, h.rc.who, p)
}

// toolResultValue is what rules see of a result: structuredContent, else a single text block
// parsed as JSON (or as is), else the content blocks.
func toolResultValue(result map[string]any) any {
	if result == nil {
		return nil
	}
	if sc, ok := result["structuredContent"]; ok && sc != nil {
		return sc
	}
	content, _ := result["content"].([]any)
	if len(content) == 1 {
		if block, ok := content[0].(map[string]any); ok && block["type"] == "text" {
			if text, ok := block["text"].(string); ok {
				var v any
				if json.Unmarshal([]byte(text), &v) == nil {
					return v
				}
				return text
			}
		}
	}
	return result["content"]
}

func (w *wrapped) declaredEvent(h *handling, result map[string]any, status string) {
	rule, ok := h.rc.opts.rules[h.name]
	if !ok {
		return
	}
	value := toolResultValue(result)
	if rule.When != nil {
		send := false
		safely(func() { send = rule.When(h.args, value) })
		if !send {
			return
		}
	}
	p := map[string]any{}
	if rule.Properties != nil {
		safely(func() {
			for k, v := range rule.Properties(h.args, value) {
				if !owned[k] {
					p[k] = v
				}
			}
		})
	}
	for k, v := range h.rc.base {
		p[k] = v
	}
	p["tool_name"] = h.name
	p["tool_status"] = status
	emit(h.rc.opts.Analytics, rule.Event, h.rc.who, p)
}

func (w *wrapped) inputKey(rc *reqCtx, toolName, key, requestState string) string {
	if requestState != "" {
		return strings.Join([]string{w.name, "state", requestState, key}, "\x00")
	}
	whom := str(rc.base["mcp_session_id"])
	if whom == "" {
		user := rc.who.userID
		if user == "" {
			user = rc.who.anonymousID
		}
		whom = str(rc.base["client_name"]) + "\x00" + user
	}
	return strings.Join([]string{w.name, whom, toolName, key}, "\x00")
}

func (w *wrapped) inputRequired(h *handling, r *mcp.CallToolResult) {
	memory.Lock()
	defer memory.Unlock()
	for key, ir := range r.InputRequests {
		if _, ok := ir.(*mcp.ElicitParams); !ok {
			continue
		}
		memory.inputRequired.set(w.inputKey(h.rc, h.name, key, r.RequestState), pendingInput{
			start: time.Now(), requestID: h.rc.requestID, toolName: h.name,
		})
	}
}

// inputResponses records the elicitations a 2026-07-28 retry answers.
func (w *wrapped) inputResponses(rc *reqCtx, p *mcp.CallToolParamsRaw, toolName string) {
	keys := make([]string, 0, len(p.InputResponses))
	for k := range p.InputResponses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		er, ok := p.InputResponses[key].(*mcp.ElicitResult)
		if !ok || !validAction(er.Action) {
			continue
		}
		k := w.inputKey(rc, toolName, key, p.RequestState)
		memory.Lock()
		pending, found := memory.inputRequired.get(k)
		memory.inputRequired.remove(k)
		memory.Unlock()
		e := elicitation{action: er.Action, toolName: toolName}
		if found {
			e.duration = time.Since(pending.start)
			e.hasDuration = true
			e.requestID = pending.requestID
		}
		w.elicitationCompleted(rc, e)
	}
}

func validAction(a string) bool { return a == "accept" || a == "decline" || a == "cancel" }

type elicitation struct {
	action      string
	errorType   string
	duration    time.Duration
	hasDuration bool
	toolName    string
	requestID   string
}

func (w *wrapped) elicitationCompleted(rc *reqCtx, e elicitation) {
	p := map[string]any{}
	var whom who
	var opts *options
	if rc != nil {
		for k, v := range rc.base {
			p[k] = v
		}
		whom, opts = rc.who, rc.opts
	} else {
		p["server_name"] = w.name
		opts = w.opts.Load()
	}
	p["elicitation_action"] = e.action
	put(p, "tool_name", e.toolName)
	put(p, "request_id", e.requestID)
	if e.hasDuration {
		p["duration_ms"] = e.duration.Milliseconds()
	}
	put(p, "error_type", e.errorType)
	emit(opts.Analytics, "mcp_elicitation_completed", whom, p)
}

func (w *wrapped) sending(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "elicitation/create" {
			return next(ctx, method, req)
		}
		if flag, ok := ctx.Value(elicitFlagKey{}).(*atomic.Bool); ok {
			flag.Store(true)
		}
		start := time.Now()
		res, err := next(ctx, method, req)
		safely(func() {
			rc, _ := ctx.Value(reqCtxKey{}).(*reqCtx)
			if rc != nil && rc.w != w {
				rc = nil
			}
			e := elicitation{duration: time.Since(start), hasDuration: true, action: "cancel"}
			if rc != nil {
				e.toolName, e.requestID = rc.toolName, rc.requestID
			}
			r, _ := res.(*mcp.ElicitResult)
			switch {
			case err != nil:
				e.errorType = errorType(err)
			case r != nil && validAction(r.Action):
				e.action = r.Action
			default:
				e.errorType = "api_error"
			}
			w.elicitationCompleted(rc, e)
		})
		return res, err
	}
}

// Elicit calls session.Elicit and records an elicitation the session rejects before sending
// it, such as for a client without elicitation support (mcp_elicitation_completed with
// elicitation_action "cancel" and error_type "invalid_request"). Elicitations that reach the
// client are recorded by the wrapped server whether or not they go through Elicit.
func Elicit(ctx context.Context, session *mcp.ServerSession, params *mcp.ElicitParams) (*mcp.ElicitResult, error) {
	sent := &atomic.Bool{}
	res, err := session.Elicit(context.WithValue(ctx, elicitFlagKey{}, sent), params)
	if err != nil && !sent.Load() {
		safely(func() {
			if rc, ok := ctx.Value(reqCtxKey{}).(*reqCtx); ok && rc != nil {
				rc.w.elicitationCompleted(rc, elicitation{
					action: "cancel", errorType: "invalid_request", toolName: rc.toolName, requestID: rc.requestID,
				})
			}
		})
	}
	return res, err
}

func requestContextSchema(opts *options) map[string]any {
	props := map[string]any{}
	if opts.intents != nil {
		props["intent"] = map[string]any{
			"type": "string", "enum": opts.intents,
			"description": "What the user is trying to do, from this list; 'other' when none fits.",
		}
	}
	if opts.userSentiment {
		props["user_sentiment"] = map[string]any{
			"type": "string", "enum": sentiments,
			"description": "The user's mood in their latest message.",
		}
	}
	if opts.CaptureContent {
		props["call_reason"] = map[string]any{
			"type": "string", "maxLength": maxTextLength,
			"description": "One short sentence on why you are making this call. No personal data.",
		}
	}
	if len(props) == 0 {
		return nil
	}
	return map[string]any{
		"type":                 "object",
		"description":          "Optional context for the server's analytics; removed before the tool runs. Leave out what you are unsure of.",
		"properties":           props,
		"additionalProperties": false,
	}
}

func capabilityToolDef(opts *options) *mcp.Tool {
	props := map[string]any{
		"capability": map[string]any{
			"type": "string", "enum": opts.capabilities,
			"description": "The missing capability; 'other' when none fits.",
		},
	}
	if opts.CaptureContent {
		props["detail"] = map[string]any{
			"type": "string", "maxLength": maxTextLength,
			"description": "For 'other' only: a short description of the need. No personal data.",
		}
	}
	return &mcp.Tool{
		Name:        capabilityTool,
		Description: "Call this when no available tool can do what the user needs. It records the missing capability for the server's owner and performs no action.",
		InputSchema: map[string]any{
			"type": "object", "properties": props, "required": []string{"capability"}, "additionalProperties": false,
		},
	}
}

// toolsList records the tools and adds request_context and request_capability to a copy of a
// tools/list page.
func (w *wrapped) toolsList(rc *reqCtx, r *mcp.ListToolsResult) mcp.Result {
	opts := rc.opts
	var contextSchema map[string]any
	if !opts.RequestContext.Disabled {
		contextSchema = requestContextSchema(opts)
	}
	memory.Lock()
	defer memory.Unlock()
	sm := serverMem(w.name)
	changed := false
	tools := make([]*mcp.Tool, 0, len(r.Tools)+1)
	for _, t := range r.Tools {
		if t == nil {
			tools = append(tools, t)
			continue
		}
		schema, _ := toJSONValue(t.InputSchema).(map[string]any)
		if schema == nil {
			schema = map[string]any{}
		}
		sm.toolSchemas[t.Name] = schema
		if t.Name == capabilityTool {
			sm.nativeCapabilityTool = true
		}
		props, _ := schema["properties"].(map[string]any)
		if _, native := props[requestContext]; native {
			sm.nativeContext[t.Name] = true
			tools = append(tools, t)
			continue
		}
		delete(sm.nativeContext, t.Name)
		if contextSchema == nil {
			tools = append(tools, t)
			continue
		}
		changed = true
		newProps := map[string]any{}
		for k, v := range props {
			newProps[k] = v
		}
		newProps[requestContext] = contextSchema
		newSchema := map[string]any{}
		for k, v := range schema {
			newSchema[k] = v
		}
		newSchema["type"] = "object"
		newSchema["properties"] = newProps
		cp := *t
		cp.InputSchema = newSchema
		tools = append(tools, &cp)
	}
	if opts.capabilities != nil && !sm.nativeCapabilityTool && r.NextCursor == "" {
		tools = append(tools, capabilityToolDef(opts))
		changed = true
	}
	if !changed {
		return nil
	}
	cp := *r
	cp.Tools = tools
	return &cp
}

func (w *wrapped) rememberResources(res mcp.Result) {
	memory.Lock()
	defer memory.Unlock()
	sm := serverMem(w.name)
	switch r := res.(type) {
	case *mcp.ListResourcesResult:
		for _, x := range r.Resources {
			if x != nil && x.URI != "" && x.Name != "" {
				sm.resources[x.URI] = x.Name
			}
		}
	case *mcp.ListResourceTemplatesResult:
		for _, x := range r.ResourceTemplates {
			if x == nil || x.URITemplate == "" || slices.ContainsFunc(sm.templates, func(t resourceTemplate) bool { return t.template == x.URITemplate }) {
				continue
			}
			sm.templates = append(sm.templates, resourceTemplate{template: x.URITemplate, matches: templateMatcher(x.URITemplate).MatchString})
		}
	}
}
