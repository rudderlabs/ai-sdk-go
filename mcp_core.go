package rudderstackai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// The MCP engine: library-neutral event building shared by every MCP adapter (WrapMCPServer for
// the go-sdk here, ruddermcpgo for mark3labs/mcp-go through internal/mcpbridge).

const (
	capabilityTool            = "request_capability"
	methodDiscover            = "server/discover"
	requestContext            = "request_context"
	protocol2026              = "2026-07-28"
	legacyProtocol            = "2025-11-25"
	capabilityReply           = "Recorded for the server owner. No tool here can do this yet; tell the user."
	capabilityDesc            = "Call this when no available tool can do what the user needs. It records the missing capability for the server's owner and performs no action."
	capabilityTitle           = "Request a missing capability"
	capabilityReadOnlyHint    = false
	capabilityDestructiveHint = false
	capabilityIdempotentHint  = false
	capabilityOpenWorldHint   = false

	// Limits.MaxMapEntries bounds each per-server-name map; maxServers the server names;
	// maxTemplates the resource templates per name. The least recently set entries go first.
	maxServers   = 1_000
	maxTemplates = 1_000
)

var sentiments = []string{"negative", "neutral", "positive"}

// owned are the properties the integration sets; a declared tool rule cannot override them.
var owned = map[string]bool{
	"sample_rate":    true,
	"mcp_session_id": true, "server_name": true, "server_version": true, "account_id": true, "client_name": true,
	"client_version": true, "protocol_version": true, "interaction_id": true, "trace_id": true,
	"oauth_client_id": true, "auth_method": true,
	"tool_name": true, "tool_status": true,
}

func init() {
	mcpbridge.ErrorType = errorType
	mcpbridge.Wrap = func(name, version string, opts any) (mcpbridge.Server, error) {
		o, _ := opts.(Options)
		n, err := normalizeOptions(o)
		if err != nil {
			return nil, err
		}
		n.resolveName(name, version)
		w := &wrapped{}
		w.opts.Store(n)
		return w, nil
	}
}

// wrapped is one wrapped server instance. Its counters, windows and caches belong to its server
// name, shared with every instance of that name in the process.
type wrapped struct {
	opts    atomic.Pointer[options]
	tools   func() []mcpbridge.Tool
	prompts func() []string
}

func (w *wrapped) name() string { return w.opts.Load().name }

// state is the process-wide state of this server's name.
func (w *wrapped) state() *serverState { return stateFor(w.name()) }

// Rewrap replaces the instance's options and keeps its state.
func (w *wrapped) Rewrap(opts any) error {
	o, _ := opts.(Options)
	n, err := normalizeOptions(o)
	if err != nil {
		return err
	}
	old := w.opts.Load()
	n.resolveName(old.reflectedName, old.version)
	w.opts.Store(n)
	return nil
}

// serverState is what the process remembers about one server name: resources its responses
// taught the integration, and its clients' windows and counters. Every map is bounded.
type serverState struct {
	sync.Mutex
	resources     *boundedMap[string]
	templates     *boundedMap[func(string) bool]
	clientSeen    *boundedMap[time.Time]
	clients       *keyedBoundedMap[rememberedClientKey, rememberedClient]
	scopes        *boundedMap[*scopeState]
	retries       *boundedMap[int]
	inputRequired *boundedMap[pendingInput]
	expiries      *boundedMap[*time.Timer]
}

type scopeState struct {
	calls int
	last  time.Time
}

type rememberedClient struct {
	name, version string
	until         time.Time
	sequence      uint64
}

// The enclosing serverState supplies the server part of this composite key.
type rememberedClientKey struct {
	accountID     string
	oauthClientID string
	userID        string
}

func clientKey(id Identity) rememberedClientKey {
	user := id.stateUserID
	if user == "" {
		user = id.stateAnonymousID
	}
	return rememberedClientKey{accountID: id.stateAccountID, oauthClientID: id.OAuthClientID, userID: user}
}

var requestSequence atomic.Uint64

// pendingInputTTL bounds how long an input_required leg waits for its retry; an older one is
// treated as absent.
const pendingInputTTL = time.Hour

type pendingInput struct {
	start     time.Time
	requestID string
	toolName  string
	// position is the input_required leg's call_index and ms_since_previous_call, which the
	// retry that completes the call takes over: the leg sends no event, so it leaves no gap.
	position map[string]any
}

func newServerState() *serverState {
	maxEntries := cfg().limits.MaxMapEntries
	st := &serverState{
		resources:     newBoundedMap[string](maxEntries),
		templates:     newBoundedMap[func(string) bool](maxTemplates),
		clientSeen:    newBoundedMap[time.Time](maxEntries),
		clients:       newKeyedBoundedMap[rememberedClientKey, rememberedClient](maxEntries),
		scopes:        newBoundedMap[*scopeState](maxEntries),
		retries:       newBoundedMap[int](maxEntries),
		inputRequired: newBoundedMap[pendingInput](maxEntries),
		expiries:      newBoundedMap[*time.Timer](maxEntries),
	}
	st.expiries.evicted = func(t *time.Timer) { t.Stop() }
	return st
}

// servers is the process-wide state by server name.
var servers = struct {
	sync.Mutex
	m *boundedMap[*serverState]
}{m: newBoundedMap[*serverState](maxServers)}

func stateFor(name string) *serverState {
	servers.Lock()
	defer servers.Unlock()
	st, ok := servers.m.get(name)
	if !ok {
		st = newServerState()
	}
	servers.m.set(name, st)
	return st
}

// weakMap maps objects to values without keeping the objects alive.
type weakMap[K, V any] struct {
	mu sync.Mutex
	m  map[weak.Pointer[K]]V
}

func (w *weakMap[K, V]) get(k *K) (V, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	v, ok := w.m[weak.Make(k)]
	return v, ok
}

// update sets k's value to f of its current one.
func (w *weakMap[K, V]) update(k *K, f func(V, bool) V) V {
	wp := weak.Make(k)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[weak.Pointer[K]]V{}
	}
	v, ok := w.m[wp]
	if !ok {
		runtime.AddCleanup(k, func(wp weak.Pointer[K]) {
			w.mu.Lock()
			defer w.mu.Unlock()
			delete(w.m, wp)
		}, wp)
	}
	v = f(v, ok)
	w.m[wp] = v
	return v
}

func (w *weakMap[K, V]) size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.m)
}

// reqCtx is everything known about one request, for the events it causes.
type reqCtx struct {
	w         *wrapped
	st        *serverState
	opts      *options
	base      map[string]any
	who       who
	identity  Identity
	requestID string
	// scopeParts are the raw ["session"|"interaction"|"connection", id]; nil when there is none.
	// scope is their caller-scoped state key.
	scopeParts        []string
	scope             string
	toolName          string
	rawToolName       string
	registeredTools   []mcpbridge.Tool
	registeredPrompts []string
	client            *mcpbridge.Client
	initializeClient  *mcpbridge.Client
	received          time.Time
	sequence          uint64
	http              bool
	meta              map[string]any
	start             time.Time
	tool              *toolState
}

// toolState is one tools/call between its start and its end.
type toolState struct {
	schema     map[string]any
	registered bool
	args       map[string]any
	props      map[string]any
	answered   bool
	skip       bool
}

type reqCtxKey struct{}

func clientFromMeta(v any) *mcpbridge.Client {
	m, ok := toJSONValue(v).(map[string]any)
	if !ok || v == nil {
		return nil
	}
	return &mcpbridge.Client{Name: str(m["name"]), Title: str(m["title"]), Version: str(m["version"])}
}

func knownClient(client *mcpbridge.Client) bool {
	return client != nil && (cleanClientText(client.Name) != "" || cleanClientText(client.Version) != "")
}

// Begin starts a request.
func (w *wrapped) Begin(r mcpbridge.Request) mcpbridge.Call { return w.begin(r) }

func (w *wrapped) begin(r mcpbridge.Request) *reqCtx {
	received := time.Now()
	sequence := requestSequence.Add(1)
	opts := w.opts.Load()
	tok, _ := r.Token.(*auth.TokenInfo)
	id := opts.identity(r.Ctx, tok)
	st := stateFor(opts.name)
	if r.ClientFor != nil {
		var version string
		r.Client, version = r.ClientFor(id.stateAccountID, id.stateUserID, id.stateAnonymousID)
		// Keep the HTTP header fallback when this caller has no negotiated version.
		if version != "" {
			r.ProtocolVersion = version
		}
	}
	client := r.InitializeClient
	if !knownClient(client) {
		client = clientFromMeta(r.Meta[metaClientInfo])
	}
	if !knownClient(client) {
		client = r.Client
	}
	if !knownClient(client) && opts.ClientInfo != nil {
		var name, version string
		if callback("Options.ClientInfo", func() error { name, version = opts.ClientInfo(r.Ctx, tok); return nil }) {
			client = &mcpbridge.Client{Name: name, Version: version}
		}
	}
	// Remembered details enrich request events only; client-seen keeps its existing sources.
	seenClient := client
	if !knownClient(client) {
		if key := clientKey(id); key != (rememberedClientKey{}) {
			st.Lock()
			// Keep expired entries' sequence until eviction so stale initializes cannot revive them.
			if remembered, ok := st.clients.get(key); ok {
				if remembered.until.After(time.Now()) {
					client = &mcpbridge.Client{Name: remembered.name, Version: remembered.version}
				}
			}
			st.Unlock()
		}
	}
	interactionID, traceID := joinIDs(r.Meta)
	base := map[string]any{}
	put(base, "mcp_session_id", capID(r.SessionID))
	base["server_name"] = opts.name
	put(base, "server_version", opts.version)
	put(base, "account_id", id.AccountID)
	put(base, "oauth_client_id", id.OAuthClientID)
	put(base, "auth_method", id.AuthMethod)
	if client != nil {
		put(base, "client_name", cleanClientText(client.Name))
		put(base, "client_version", cleanClientText(client.Version))
	}
	version := str(r.Meta[metaProtocolVersion])
	if version == "" {
		version = r.ProtocolVersion
	}
	put(base, "protocol_version", protocolVersion(version))
	base["interaction_id"] = interactionID
	if interactionID == "" {
		base["interaction_id"] = mintID()
	}
	put(base, "trace_id", traceID)
	rc := &reqCtx{
		w: w, st: st, opts: opts, base: base, who: who{id.UserID, id.AnonymousID}, identity: id,
		client: seenClient, requestID: safeRequestID(r.RequestID), http: r.HTTP, meta: map[string]any{}, start: time.Now(),
		initializeClient: r.InitializeClient, received: received, sequence: sequence, registeredTools: r.RegisteredTools, registeredPrompts: r.RegisteredPrompts,
	}
	// A copy: the SDK may remove the protocol keys from _meta while it handles the request.
	for _, k := range []string{metaProtocolVersion, metaClientInfo, metaClientCapabilities} {
		if v, ok := r.Meta[k]; ok {
			rc.meta[k] = v
		}
	}
	// Raw values: a capped or trimmed key would merge callers that differ only past the cap.
	switch {
	case r.SessionID != "":
		rc.scopeParts = []string{"session", r.SessionID}
	case interactionID != "":
		rc.scopeParts = []string{"interaction", str(r.Meta[metaInteractionID])}
	case r.Connection != "":
		rc.scopeParts = []string{"connection", r.Connection}
	}
	if rc.scopeParts != nil {
		rc.scope = rc.CallerKey(rc.scopeParts...)
	}
	return rc
}

func (rc *reqCtx) CallerKey(parts ...string) string {
	return callerStateKey(rc.identity.stateAccountID, rc.identity.stateUserID, rc.identity.stateAnonymousID, parts...)
}

func (rc *reqCtx) Started()          { rc.start = time.Now() }
func (rc *reqCtx) ToolName() string  { return rc.toolName }
func (rc *reqCtx) RequestID() string { return rc.requestID }

// Succeeded reports a successful request: a 2026-07-28 request that carries the client's
// details reports the client, a server/discover only when the server serves the version it
// asks for (otherwise the client falls back to initialize).
func (rc *reqCtx) Succeeded(method string, discoverVersions []string) {
	if method == "initialize" {
		return
	}
	_, hasVersion := rc.meta[metaProtocolVersion]
	_, hasClient := rc.meta[metaClientInfo]
	if !hasVersion && !hasClient {
		return
	}
	if method == methodDiscover && !slices.Contains(discoverVersions, str(rc.meta[metaProtocolVersion])) {
		return
	}
	rc.clientSeen(rc.client, capabilityNames(rc.meta[metaClientCapabilities]))
}

// Initialized reports a successful initialize.
func (rc *reqCtx) Initialized(client *mcpbridge.Client, capabilities any, version string) {
	if knownClient(rc.initializeClient) {
		client = rc.initializeClient
	}
	put(rc.base, "protocol_version", protocolVersion(version))
	if rc.client == nil {
		rc.client = client
	}
	if knownClient(client) {
		if key := clientKey(rc.identity); key != (rememberedClientKey{}) {
			rc.st.Lock()
			previous, ok := rc.st.clients.get(key)
			if !ok || rc.sequence > previous.sequence {
				rc.st.clients.set(key, rememberedClient{
					name: cleanClientText(client.Name), version: cleanClientText(client.Version), until: rc.received.Add(rc.opts.ClientSeenWindow), sequence: rc.sequence,
				})
			}
			rc.st.Unlock()
		}
	}

	rc.clientSeen(rc.client, capabilityNames(capabilities))
}

func (rc *reqCtx) clientSeen(client *mcpbridge.Client, caps []string) {
	var name, title, version string
	if client != nil {
		name, title, version = cleanClientText(client.Name), cleanClientText(client.Title), cleanClientText(client.Version)
	}
	key := callerStateKey(rc.identity.stateAccountID, rc.identity.stateUserID, rc.identity.stateAnonymousID, name, rc.identity.OAuthClientID)
	now := time.Now()
	rc.st.Lock()
	until, seen := rc.st.clientSeen.get(key)
	if seen && until.After(now) {
		rc.st.Unlock()
		return
	}
	rc.st.clientSeen.set(key, now.Add(rc.opts.ClientSeenWindow))
	rc.st.Unlock()
	p := map[string]any{}
	put(p, "mcp_session_id", str(rc.base["mcp_session_id"]))
	p["server_name"] = rc.opts.name
	put(p, "server_version", rc.opts.version)
	put(p, "client_name", name)
	put(p, "client_title", title)
	put(p, "client_version", version)
	if caps != nil {
		p["client_capabilities"] = caps
	}
	put(p, "protocol_version", str(rc.base["protocol_version"]))
	transport := rc.opts.Transport
	if rc.http {
		transport = TransportStreamableHTTP
	}
	put(p, "transport", transport)
	put(p, "auth_method", rc.identity.AuthMethod)
	put(p, "oauth_client_id", rc.identity.OAuthClientID)
	if rc.identity.TokenScopes != nil {
		p["token_scopes"] = rc.identity.TokenScopes
	}
	put(p, "country", rc.identity.Country)
	put(p, "account_id", rc.identity.AccountID)
	emit(rc.opts.Analytics, "mcp_client_seen", rc.who, p)
}

// ToolCall records the start of a tools/call.
func (rc *reqCtx) ToolCall(name string, args map[string]json.RawMessage, progress bool, answers []mcpbridge.Answer, requestState string) (strip, capability bool) {
	opts := rc.opts
	t := &toolState{props: map[string]any{}}
	rc.tool = t
	rc.rawToolName = name
	rc.toolName = "unknown"
	// Resolved from the server's current registry only, never from lists clients saw.
	nativeCap := false
	for _, tool := range rc.registeredTools {
		if tool.Name == capabilityTool {
			nativeCap = true
		}
		if tool.Name == name && !t.registered {
			t.schema, t.registered = tool.Schema, true
		}
	}
	if t.registered {
		rc.toolName = name
	}
	// A call without a tool name fails at the server; it is not reported.
	t.skip = name == ""
	props, _ := t.schema["properties"].(map[string]any)
	_, nativeContext := props[requestContext]
	if name == capabilityTool && opts.capabilities != nil && !nativeCap {
		return false, true
	}
	if args != nil {
		if reported, ok := args[requestContext]; ok && opts.RequestContext.Enabled && !nativeContext {
			strip = true
			var v any
			_ = json.Unmarshal(reported, &v)
			for k, val := range rc.reportedContext(v) {
				t.props[k] = val
			}
		}
		t.args = map[string]any{}
		keys := make([]string, 0, len(args))
		for k, val := range args {
			if strip && k == requestContext {
				continue
			}
			var x any
			_ = json.Unmarshal(val, &x)
			t.args[k] = x
			keys = append(keys, k)
		}
		t.props["argument_keys"] = registeredArgumentNames(t.schema, keys)
	}
	t.props["progress_requested"] = progress
	t.answered = len(answers) > 0
	var position map[string]any
	if t.answered {
		position = rc.inputResponses(answers, requestState)
	}
	if rc.scope == "" || t.skip {
		return strip, false
	}
	if position != nil {
		for k, v := range position {
			t.props[k] = v
		}
		return strip, false
	}
	start := time.Now()
	rc.st.Lock()
	defer rc.st.Unlock()
	s, ok := rc.st.scopes.get(rc.scope)
	if !ok {
		s = &scopeState{}
	}
	t.props["call_index"] = s.calls
	s.calls++
	if !s.last.IsZero() {
		t.props["ms_since_previous_call"] = max(0, start.Sub(s.last).Milliseconds())
	}
	s.last = start
	if !t.answered {
		key := rc.CallerKey(append(slices.Clone(rc.scopeParts), argumentsHash(rc.rawToolName, t.args))...)
		n, _ := rc.st.retries.get(key)
		t.props["retry_count"] = n
		rc.st.retries.set(key, n+1)
	}
	rc.st.scopes.set(rc.scope, s)
	return strip, false
}

func (rc *reqCtx) reportedContext(v any) map[string]any {
	opts := rc.opts
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
		if masked, ok := opts.mask(strings.TrimSpace(reason)); ok && masked != "" {
			out["call_reason"] = capText(masked, maxTextLength)
		}
	}
	return out
}

// Capability records a request_capability call and returns the reply.
func (rc *reqCtx) Capability(args map[string]json.RawMessage) string {
	capability := "other"
	var c string
	_ = json.Unmarshal(args["capability"], &c)
	if c != "" && slices.Contains(rc.opts.capabilities, c) {
		capability = c
	}
	p := map[string]any{}
	for k, v := range rc.base {
		p[k] = v
	}
	p["capability"] = capability
	var detail string
	if json.Unmarshal(args["detail"], &detail) == nil && capability == "other" && rc.opts.CaptureContent {
		// Withheld, not sent, when it still looks sensitive after masking.
		if masked, ok := rc.opts.mask(strings.TrimSpace(detail)); ok && masked != "" && (rc.opts.DisableRedaction || !hasSensitive(masked)) {
			p["capability_detail"] = capText(masked, maxTextLength)
		}
	}
	emit(rc.opts.Analytics, "mcp_capability_requested", rc.who, p)
	return capabilityReply
}

// ResourceName is the registered name or URI template of a resource URI, never the URI.
func (rc *reqCtx) ResourceName(uri string) string {
	rc.st.Lock()
	defer rc.st.Unlock()
	found := ""
	rc.st.templates.each(func(template string, matches func(string) bool) bool {
		if matches(uri) {
			found = template
			return false
		}
		return true
	})
	if found != "" {
		return found
	}
	if name, _ := rc.st.resources.get(uri); name != "" {
		return name
	}
	return "unknown"
}

// PromptName is the registered prompt name, else "unknown"; a missing name stays empty.
func (rc *reqCtx) PromptName(name string) string {
	switch {
	case name == "":
		return ""
	case slices.Contains(rc.registeredPrompts, name):
		return name
	}
	return "unknown"
}

// RememberResources records resource names by URI from a resources/list.
func (w *wrapped) RememberResources(byURI map[string]string) {
	st := w.state()
	st.Lock()
	defer st.Unlock()
	for uri, name := range byURI {
		if uri != "" && name != "" {
			st.resources.set(uri, capID(name))
		}
	}
}

// RememberTemplates records URI templates from a resources/templates/list.
func (w *wrapped) RememberTemplates(templates []string) {
	st := w.state()
	st.Lock()
	defer st.Unlock()
	for _, t := range templates {
		if _, ok := st.templates.get(t); t == "" || ok {
			continue
		}
		st.templates.set(t, templateMatcher(t).MatchString)
	}
}

// ToolDone records the end of a tools/call.
func (rc *reqCtx) ToolDone(o mcpbridge.Outcome) {
	t := rc.tool
	if t == nil || t.skip {
		return
	}
	extra := map[string]any{}
	status := "success"
	var result map[string]any
	switch {
	case o.Cancelled:
		status = "error"
		extra["error_type"] = "cancelled"
		extra["time_to_cancel_ms"] = time.Since(rc.start).Milliseconds()
	case o.ErrorType != "":
		status = "error"
		extra["error_type"] = o.ErrorType
	case o.InputRequired:
		// A 2026-07-28 input_required leg: the call goes on in the client's retry.
		rc.inputRequired(o.InputKeys, o.RequestState)
		return
	case o.Result == nil:
		status = "error"
		extra["error_type"] = "api_error"
	default:
		b, _ := compactJSON(o.Result)
		_ = json.Unmarshal(b, &result)
		for k, v := range resultStats(result, len(b)) {
			extra[k] = v
		}
		isError, _ := result["isError"].(bool)
		extra["is_error_result"] = isError
		if isError {
			status = "error"
			kind := toolErrorType(o.ClassifiedError, result)
			if invalid := invalidArguments(t.schema, t.args); len(invalid) > 0 {
				extra["invalid_arguments"] = registeredArgumentNames(t.schema, invalid)
				if kind == "" {
					kind = "invalid_request"
				}
			}
			if kind == "" {
				kind = "api_error"
			}
			extra["error_type"] = kind
		}
	}
	rc.finish("mcp_tool_called", "tool_name", rc.toolName, status, extra)
	rc.declaredEvent(result, status)
}

// Finished records a resources/read or prompts/get.
func (rc *reqCtx) Finished(method, name string, size int, errorType string) {
	event, nameKey := "mcp_resource_read", "resource_name"
	if method == "prompts/get" {
		event, nameKey = "mcp_prompt_used", "prompt_name"
	}
	name = capID(name)
	if name == "" {
		return
	}
	extra := map[string]any{}
	status := "success"
	if errorType != "" {
		status = "error"
		extra["error_type"] = errorType
	} else {
		extra["result_size_bytes"] = size
	}
	rc.finish(event, nameKey, name, status, extra)
}

func (rc *reqCtx) finish(event, nameKey, name, status string, extra map[string]any) {
	end := time.Now()
	p := map[string]any{}
	for k, v := range rc.base {
		p[k] = v
	}
	put(p, "request_id", rc.requestID)
	p[nameKey] = name
	p["tool_status"] = status
	if _, cancel := extra["time_to_cancel_ms"]; !cancel {
		p["duration_ms"] = end.Sub(rc.start).Milliseconds()
	}
	if t := rc.tool; t != nil && event == "mcp_tool_called" {
		for k, v := range t.props {
			p[k] = v
		}
		if rc.scope != "" {
			rc.st.Lock()
			if s, ok := rc.st.scopes.get(rc.scope); ok {
				s.last = end
			}
			rc.st.Unlock()
		}
	}
	for k, v := range extra {
		p[k] = v
	}
	emit(rc.opts.Analytics, event, rc.who, p)
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

func (rc *reqCtx) declaredEvent(result map[string]any, status string) {
	if rc.tool == nil || !rc.tool.registered {
		return
	}
	rule, ok := rc.opts.rules[rc.rawToolName]
	if !ok {
		return
	}
	args := rc.tool.args
	value := toolResultValue(result)
	if rule.When != nil {
		send := false
		callback("ToolEventRule.When", func() error { send = rule.When(args, value); return nil })
		if !send {
			return
		}
	}
	p := map[string]any{}
	if rule.Properties != nil {
		var out map[string]any
		if callback("ToolEventRule.Properties", func() error { out = rule.Properties(args, value); return nil }) {
			// Sorted so aliases of one property resolve the same way every time.
			keys := slices.Sorted(maps.Keys(out))
			for _, k := range keys {
				v := out[k]
				if !allowToolEventProperty(k, rc.opts.CaptureContent) {
					continue
				}
				switch canon := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(k)); canon {
				case "requestid":
					// Never trust the callback: the property is the actual request's id.
					if rc.requestID != "" {
						p["request_id"] = rc.requestID
					}
				case "argumentkeys", "invalidarguments":
					var list []string
					if s, ok := v.(string); ok {
						list = []string{s}
					}
					raw, _ := toJSONValue(v).([]any)
					for _, item := range raw {
						if name, ok := item.(string); ok {
							list = append(list, name)
						}
					}
					if canon == "argumentkeys" {
						p["argument_keys"] = registeredArgumentNames(rc.tool.schema, list)
					} else {
						p["invalid_arguments"] = registeredArgumentNames(rc.tool.schema, list)
					}
				case "toolname":
					p["tool_name"] = rc.toolName
				case "clientname", "clienttitle", "clientversion":
					if value := cleanClientText(str(v)); value != "" {
						p["client_"+strings.TrimPrefix(canon, "client")] = value
					}
				default:
					if !owned[propertyName(k)] {
						p[k] = v
					}
				}
			}
		}
	}
	custom := slices.Collect(maps.Keys(p))
	for k, v := range rc.base {
		p[k] = v
	}
	p["tool_name"] = rc.toolName
	p["tool_status"] = status
	rates := cfg().sampleRates
	emitRates(rc.opts.Analytics, rule.Event, rc.who, fitToolEventProperties(rule.Event, p, custom, rates), rates)
}

// inputKey scopes a pending elicitation to its caller and session or interaction, so another
// session's retry never consumes it.
func (rc *reqCtx) inputKey(key, requestState string) string {
	scope := rc.scopeParts
	if scope == nil {
		scope = []string{"client", str(rc.base["client_name"])}
	}
	if requestState != "" {
		return rc.CallerKey(append(slices.Clone(scope), "state", requestState, key)...)
	}
	return rc.CallerKey(append(slices.Clone(scope), rc.rawToolName, key)...)
}

func (rc *reqCtx) inputRequired(keys []string, requestState string) {
	var position map[string]any
	if t := rc.tool; t != nil {
		for _, k := range []string{"call_index", "ms_since_previous_call"} {
			if v, ok := t.props[k]; ok {
				if position == nil {
					position = map[string]any{}
				}
				position[k] = v
			}
		}
	}
	rc.st.Lock()
	defer rc.st.Unlock()
	for _, key := range keys {
		rc.st.inputRequired.set(rc.inputKey(key, requestState), pendingInput{
			start: time.Now(), requestID: rc.requestID, toolName: rc.toolName, position: position,
		})
	}
}

// inputResponses records the elicitations a 2026-07-28 retry answers, and returns the position
// of the input_required leg it continues (nil when none is known).
func (rc *reqCtx) inputResponses(answers []mcpbridge.Answer, requestState string) (position map[string]any) {
	sorted := slices.Clone(answers)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	for _, a := range sorted {
		if !validAction(a.Action) {
			continue
		}
		k := rc.inputKey(a.Key, requestState)
		rc.st.Lock()
		pending, found := rc.st.inputRequired.get(k)
		rc.st.inputRequired.remove(k)
		rc.st.Unlock()
		// An expired leg is absent: its position and request id are never reused.
		found = found && time.Since(pending.start) <= pendingInputTTL
		e := elicitation{action: a.Action, toolName: rc.toolName}
		if found {
			e.duration = time.Since(pending.start)
			e.hasDuration = true
			e.requestID = pending.requestID
			if position == nil {
				position = pending.position
			}
		}
		rc.w.elicitationCompleted(rc, e)
	}
	return position
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

// Elicited records an elicitation that reached the client, or one rejected before sending.
func (w *wrapped) Elicited(call mcpbridge.Call, e mcpbridge.Elicitation) {
	rc, _ := call.(*reqCtx)
	if rc != nil && rc.w != w {
		rc = nil
	}
	el := elicitation{action: e.Action, errorType: e.ErrorType, duration: e.Duration, hasDuration: e.HasDuration}
	if rc != nil {
		el.toolName, el.requestID = rc.toolName, rc.requestID
	}
	w.elicitationCompleted(rc, el)
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
		opts = w.opts.Load()
		p["server_name"] = opts.name
		put(p, "server_version", opts.version)
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

// CapabilityTool is the request_capability tool, nil when CapabilityRequests is off. It is
// an additive write: every call records a capability request event.
func (w *wrapped) CapabilityTool() *mcpbridge.ToolDef {
	opts := w.opts.Load()
	if opts.capabilities == nil {
		return nil
	}
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
	return &mcpbridge.ToolDef{
		Name: capabilityTool, Title: capabilityTitle, Description: capabilityDesc, Reply: capabilityReply,
		Annotations: mcpbridge.ToolAnnotations{
			Title: capabilityTitle, ReadOnlyHint: capabilityReadOnlyHint,
			DestructiveHint: capabilityDestructiveHint, IdempotentHint: capabilityIdempotentHint,
			OpenWorldHint: capabilityOpenWorldHint,
		},
		Schema: map[string]any{
			"type": "object", "properties": props, "required": []string{"capability"}, "additionalProperties": false,
		},
	}
}

// ListTools returns the schemas to send for a tools/list page: a copy with request_context
// added, or nil to keep the tool's own. Native request_capability is read from the current
// registry and the page.
func (rc *reqCtx) ListTools(tools []mcpbridge.Tool, lastPage bool) ([]map[string]any, bool) {
	opts := rc.opts
	var contextSchema map[string]any
	if opts.RequestContext.Enabled {
		contextSchema = requestContextSchema(opts)
	}
	nativeCap := slices.ContainsFunc(tools, func(t mcpbridge.Tool) bool { return t.Name == capabilityTool }) ||
		slices.ContainsFunc(rc.registeredTools, func(t mcpbridge.Tool) bool { return t.Name == capabilityTool })
	out := make([]map[string]any, len(tools))
	for i, t := range tools {
		schema := t.Schema
		if schema == nil {
			schema = map[string]any{}
		}
		props, _ := schema["properties"].(map[string]any)
		if _, native := props[requestContext]; native {
			continue
		}
		if contextSchema == nil {
			continue
		}
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
		out[i] = newSchema
	}
	return out, opts.capabilities != nil && !nativeCap && lastPage
}

// fromContext is the request a context belongs to, for elicitations sent within a handler.
func fromContext(ctx context.Context) *reqCtx {
	rc, _ := ctx.Value(reqCtxKey{}).(*reqCtx)
	return rc
}

// toolErrorType uses only spec categories and numeric HTTP status hints, never error text.
func toolErrorType(classified string, result map[string]any) string {
	switch classified {
	case "rate_limit", "timeout", "cancelled", "denied", "auth_error", "invalid_request", "connection_error", "api_error":
		return classified
	}
	meta, _ := result["_meta"].(map[string]any)
	structured, _ := result["structuredContent"].(map[string]any)
	var status float64
	for _, value := range []any{meta["com.rudderstack/http_status"], structured["status"], structured["statusCode"]} {
		if number, ok := value.(float64); ok {
			status = number
			break
		}
	}
	switch {
	case status == 401:
		return "auth_error"
	case status == 403:
		return "denied"
	case status == 408 || status == 504:
		return "timeout"
	case status == 429:
		return "rate_limit"
	case status >= 400 && status < 500:
		return "invalid_request"
	case status >= 500 && status < 600:
		return "api_error"
	default:
		return ""
	}
}

// Keys encode separate components so caller-supplied separators cannot collide.
// The anonymous id only separates callers that have no user id.
func callerStateKey(account, user, anonymous string, parts ...string) string {
	if user != "" {
		anonymous = ""
	}
	values := append([]string{account, user, anonymous}, parts...)
	b, _ := json.Marshal(values)
	return string(b)
}

func cleanClientText(value string) string { return mcpbridge.CleanClientText(value) }

// maxSafeInteger is the largest integer every JSON implementation reads exactly.
const maxSafeInteger = 9007199254740991

// safeRequestID encodes a JSON-RPC id: nil (no id) is "", integer-valued numbers within
// +-maxSafeInteger are decimal strings (-0 is "0"), every non-empty string is
// hashed, and anything else (empty string, bool, object, fractional or
// out of range number) is "unknown".
func safeRequestID(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		if v == "" {
			return "unknown"
		}
		return hashedRequestID(v)
	case json.Number:
		n, err := strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return "unknown"
		}
		return safeRequestID(n)
	case interface{ Value() any }: // mcp-go's RequestId
		return safeRequestID(v.Value())
	}
	switch v := reflect.ValueOf(value); v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n := v.Int(); n >= -maxSafeInteger && n <= maxSafeInteger {
			return strconv.FormatInt(n, 10)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n := v.Uint(); n <= maxSafeInteger {
			return strconv.FormatUint(n, 10)
		}
	case reflect.Float32, reflect.Float64:
		if n := v.Float(); math.Trunc(n) == n && math.Abs(n) <= maxSafeInteger {
			return strconv.FormatInt(int64(n), 10)
		}
	}
	return "unknown"
}

func hashedRequestID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// registeredArgumentNames lists top-level argument names: sorted by code point, names that are
// not registered input properties replaced by "unknown", deduplicated, at most maxListItems.
func registeredArgumentNames(schema map[string]any, list []string) []string {
	props, _ := schema["properties"].(map[string]any)
	out := make([]string, 0, len(list))
	for _, name := range slices.Sorted(slices.Values(list)) {
		if _, ok := props[name]; !ok {
			name = "unknown"
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out[:min(len(out), maxListItems)]
}

// protocolVersion keeps a YYYY-MM-DD version and maps any other text to "unknown".
func protocolVersion(v string) string {
	switch {
	case v == "":
		return ""
	case protocolVersionRE.MatchString(v):
		return v
	}
	return "unknown"
}

var protocolVersionRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func cleanClient(client *mcpbridge.Client) *mcpbridge.Client { return mcpbridge.CleanClient(client) }
