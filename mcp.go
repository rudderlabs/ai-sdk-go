package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// instances are the wrapped go-sdk servers, without keeping them alive.
var instances weakMap[mcp.Server, *wrapped]

// sessionInfo is what the integration keeps per go-sdk session: the protocol version it
// negotiated at initialize and, for stdio and in-memory connections, the ID that scopes its
// call counters.
type sessionInfo struct {
	version    string
	connection string
}

var (
	sessions    weakMap[mcp.ServerSession, sessionInfo]
	connections atomic.Uint64
)

// WrapMCPServer tracks an MCP server built on github.com/modelcontextprotocol/go-sdk. Call it
// once at startup, before serving. It adds a receiving and a sending middleware; requests and
// responses pass through unchanged except for the request_context argument (added to each
// tool's input schema and removed before the tool runs) and, when enabled, the
// request_capability tool. Wrapping the same server again replaces its options.
//
// Counters, client windows and learned schemas belong to the server name (Options.ServerName,
// else the name the server was created with), shared by every server of that name in the
// process: a server built per request keeps counting.
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
	if w, ok := instances.get(server); ok {
		n.resolveName(w.opts.Load().reflectedName, w.opts.Load().version)
		w.opts.Store(n)
		return nil
	}
	n.resolveName(serverImpl(server))
	w := &wrapped{}
	w.opts.Store(n)
	instances.update(server, func(*wrapped, bool) *wrapped { return w })
	server.AddReceivingMiddleware(w.receiving)
	server.AddSendingMiddleware(w.sending)
	return nil
}

// serverImpl reads the name and version the server was created with; the SDK has no getter.
func serverImpl(s *mcp.Server) (name, version string) {
	safely(func() {
		impl := reflect.ValueOf(s).Elem().FieldByName("impl")
		if !impl.IsValid() || impl.Kind() != reflect.Pointer || impl.IsNil() {
			return
		}
		name = impl.Elem().FieldByName("Name").String()
		version = impl.Elem().FieldByName("Version").String()
	})
	return name, version
}

// elicitFlagKey marks a context in which an elicitation/create request reached the client.
type elicitFlagKey struct{}

// handling is the state of one request between the two halves of the middleware.
type handling struct {
	rc      *reqCtx
	method  string
	name    string
	req     mcp.Request // replaces the request, when request_context was stripped
	reply   mcp.Result  // answers the request without calling the server
	init    *mcp.InitializeParams
	session *mcp.ServerSession
}

func (w *wrapped) receiving(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		var h *handling
		safely(func() { h = w.before(ctx, method, req) })
		if h == nil {
			return next(ctx, method, req)
		}
		if h.reply != nil {
			safely(func() { h.rc.Succeeded(method, nil) })
			return h.reply, nil
		}
		if h.req != nil {
			req = h.req
		}
		ctx = context.WithValue(ctx, reqCtxKey{}, h.rc)
		safely(h.rc.Started)
		res, err := next(ctx, method, req)
		// Read now: the transport may cancel the context after the call returned.
		ctxErr := ctx.Err()
		safely(func() {
			if replaced := w.after(h, res, err, ctxErr); replaced != nil {
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
	ss, _ := req.GetSession().(*mcp.ServerSession)
	extra := req.GetExtra()
	params := req.GetParams()
	r := mcpbridge.Request{Ctx: ctx, Meta: metaOf(params), RequestID: requestIDOf(ctx)}
	if extra != nil {
		r.Token = extra.TokenInfo
		r.HTTP = extra.Header != nil
	}
	r.SessionID = sessionIDOf(ss, extra)
	var info sessionInfo
	if ss != nil {
		info, _ = sessions.get(ss)
		if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil {
			r.Client = &mcpbridge.Client{Name: ip.ClientInfo.Name, Title: ip.ClientInfo.Title, Version: ip.ClientInfo.Version}
		}
		if r.SessionID == "" && !r.HTTP {
			info = sessions.update(ss, func(i sessionInfo, _ bool) sessionInfo {
				if i.connection == "" {
					i.connection = strconv.FormatUint(connections.Add(1), 10)
				}
				return i
			})
			r.Connection = info.connection
		}
	}
	r.ProtocolVersion = info.version
	if r.ProtocolVersion == "" && r.HTTP {
		r.ProtocolVersion = extra.Header.Get("Mcp-Protocol-Version")
	}
	rc := w.begin(r)
	h := &handling{rc: rc, method: method, session: ss}
	if method == "initialize" {
		h.init, _ = params.(*mcp.InitializeParams)
		return h
	}
	switch x := req.(type) {
	case *mcp.CallToolRequest:
		w.toolCall(h, x)
	case *mcp.ReadResourceRequest:
		h.name = rc.ResourceName(x.Params.URI)
	case *mcp.GetPromptRequest:
		h.name = x.Params.Name
	}
	return h
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

func (w *wrapped) toolCall(h *handling, r *mcp.CallToolRequest) {
	p := r.Params
	var raw map[string]json.RawMessage
	if len(p.Arguments) > 0 && json.Unmarshal(p.Arguments, &raw) != nil {
		raw = nil
	}
	var answers []mcpbridge.Answer
	for k, v := range p.InputResponses {
		er, _ := v.(*mcp.ElicitResult)
		a := mcpbridge.Answer{Key: k}
		if er != nil {
			a.Action = er.Action
		}
		answers = append(answers, a)
	}
	_, progress := p.Meta["progressToken"]
	strip, capability := h.rc.ToolCall(p.Name, raw, progress, answers, p.RequestState)
	if capability {
		reply := h.rc.Capability(raw)
		h.reply = &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reply}}}
		return
	}
	if strip {
		delete(raw, requestContext)
		if stripped, err := json.Marshal(raw); err == nil {
			cp := *p
			cp.Arguments = stripped
			h.req = &mcp.CallToolRequest{Session: r.Session, Params: &cp, Extra: r.Extra}
		}
	}
}

func rememberNegotiated(ss *mcp.ServerSession, version string) {
	if ss == nil || version == "" {
		return
	}
	sessions.update(ss, func(i sessionInfo, _ bool) sessionInfo {
		i.version = version
		return i
	})
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
				id = strconv.FormatInt(raw, 10)
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

// failure is the error_type of a failed request, or "" when it succeeded. A request is
// cancelled only when it failed (an error, or an error result) while its context was cancelled:
// a call that succeeded stays a success.
func failure(err, ctxErr error, errorResult bool) (kind string, cancelled bool) {
	if err == nil && !errorResult {
		return "", false
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctxErr, context.Canceled) {
		return "cancelled", true
	}
	if err == nil {
		return "", false
	}
	return errorType(err), false
}

func (w *wrapped) after(h *handling, res mcp.Result, err, ctxErr error) mcp.Result {
	rc := h.rc
	var discover []string
	if r, ok := res.(*mcp.DiscoverResult); ok && r != nil {
		discover = r.SupportedVersions
	}
	if err == nil {
		rc.Succeeded(h.method, discover)
	}
	r, _ := res.(*mcp.CallToolResult)
	errType, cancelled := failure(err, ctxErr, r != nil && r.IsError)
	switch h.method {
	case "initialize":
		if r, ok := res.(*mcp.InitializeResult); ok && err == nil && r != nil {
			rememberNegotiated(h.session, r.ProtocolVersion)
			var client *mcpbridge.Client
			var caps any
			if h.init != nil {
				if ci := h.init.ClientInfo; ci != nil {
					client = &mcpbridge.Client{Name: ci.Name, Title: ci.Title, Version: ci.Version}
				}
				caps = h.init.Capabilities
			}
			rc.Initialized(client, caps, r.ProtocolVersion)
		}
	case "tools/list":
		if r, ok := res.(*mcp.ListToolsResult); ok && err == nil && r != nil {
			return toolsList(rc, w, r)
		}
	case "resources/list":
		if r, ok := res.(*mcp.ListResourcesResult); ok && err == nil && r != nil {
			byURI := map[string]string{}
			for _, x := range r.Resources {
				if x != nil {
					byURI[x.URI] = x.Name
				}
			}
			w.RememberResources(byURI)
		}
	case "resources/templates/list":
		if r, ok := res.(*mcp.ListResourceTemplatesResult); ok && err == nil && r != nil {
			var templates []string
			for _, x := range r.ResourceTemplates {
				if x != nil {
					templates = append(templates, x.URITemplate)
				}
			}
			w.RememberTemplates(templates)
		}
	case "tools/call":
		o := mcpbridge.Outcome{ErrorType: errType, Cancelled: cancelled}
		if r, ok := res.(*mcp.CallToolResult); ok && r != nil && err == nil {
			o.Result = r
			if r.InputRequests != nil {
				o.InputRequired, o.RequestState = true, r.RequestState
				for key, ir := range r.InputRequests {
					if _, ok := ir.(*mcp.ElicitParams); ok {
						o.InputKeys = append(o.InputKeys, key)
					}
				}
			}
		}
		rc.ToolDone(o)
	case "resources/read", "prompts/get":
		size := 0
		if err == nil {
			size = byteSize(res)
		}
		rc.Finished(h.method, h.name, size, errType)
	}
	return nil
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
			e := mcpbridge.Elicitation{Duration: time.Since(start), HasDuration: true, Action: "cancel"}
			r, _ := res.(*mcp.ElicitResult)
			switch {
			case err != nil:
				e.ErrorType = errorType(err)
			case r != nil && validAction(r.Action):
				e.Action = r.Action
			default:
				e.ErrorType = "api_error"
			}
			var call mcpbridge.Call
			if rc := fromContext(ctx); rc != nil {
				call = rc
			}
			w.Elicited(call, e)
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
			if rc := fromContext(ctx); rc != nil {
				rc.w.Elicited(rc, mcpbridge.Elicitation{Action: "cancel", ErrorType: "invalid_request"})
			}
		})
	}
	return res, err
}

// toolsList adds request_context and request_capability to a copy of a tools/list page.
func toolsList(rc *reqCtx, w *wrapped, r *mcp.ListToolsResult) mcp.Result {
	listed := make([]mcpbridge.Tool, 0, len(r.Tools))
	index := make([]int, 0, len(r.Tools))
	for i, t := range r.Tools {
		if t == nil {
			continue
		}
		schema, _ := toJSONValue(t.InputSchema).(map[string]any)
		listed = append(listed, mcpbridge.Tool{Name: t.Name, Schema: schema})
		index = append(index, i)
	}
	schemas, addCapability := rc.ListTools(listed, r.NextCursor == "")
	changed := false
	tools := slices.Clone(r.Tools)
	for j, schema := range schemas {
		if schema == nil {
			continue
		}
		cp := *tools[index[j]]
		cp.InputSchema = schema
		tools[index[j]] = &cp
		changed = true
	}
	if def := w.CapabilityTool(); addCapability && def != nil {
		tools = append(tools, &mcp.Tool{
			Name: def.Name, Description: def.Description, InputSchema: def.Schema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		})
		changed = true
	}
	if !changed {
		return nil
	}
	cp := *r
	cp.Tools = tools
	return &cp
}
