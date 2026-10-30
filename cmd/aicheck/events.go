package main

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The check command: validate a capture of AI events, legacy events and their joins. Server events
// without context.wrapperLibrary were not sent by an AI SDK: they count as legacy events. Every
// output line and exit code is shared with the Node.js and Python checkers through
// spec/fixtures/migration-check-v0.1.json.

// specVersion is the AI events spec version the bundled schema describes and the SDKs send.
const specVersion = "0.3.0"

var (
	sdkNames   = []string{"@rudderstack/ai", "ai-sdk-go", "rudderstack-ai"} // context.wrapperLibrary.name: Node.js, Go, Python
	specEvents = set("ai_user_prompt_created", "ai_response_received", "ai_user_action",
		"ai_interaction_classified", "ai_interaction_scored", "ai_llm_response_received")
	clientCopies    = set("ai_user_prompt_created", "ai_response_received", "ai_user_action")
	failureStatuses = []string{"cancelled", "error", "incomplete", "timeout"} // what an app-caught failure may report
	clientOnly      = set("ai_assistant_opened", "ai_assistant_closed", "ai_conversation_started",
		"ai_conversation_switched", "ai_conversation_renamed", "ai_conversation_deleted", "ai_conversation_pinned",
		"ai_conversation_shared", "ai_agent_selected", "ai_suggestion_shown", "ai_feedback_requested", "ai_limit_reached")
	// feedback about a whole conversation or feature: its interaction_id is minted for it, no answer has it
	scopedFeedback = []string{"conversation", "feature"}
	mcpEvents      = set("mcp_client_seen", "mcp_tool_called", "mcp_elicitation_completed", "mcp_capability_requested",
		"mcp_resource_read", "mcp_prompt_used", "mcp_event_subscription_changed", "mcp_event_delivered")
	// a standalone score carries a feature only when given; ai_otel_span (tracing) carries none by default
	optionalFeature      = set("ai_interaction_scored", "ai_otel_span")
	requestContextFields = set("intent", "user_sentiment", "call_reason") // what the SDK reads from request_context
	knownEvents          = func() map[string]bool {                       // ai_otel_span: rudderstack.ai.tracing
		m := set("ai_otel_span")
		for _, s := range []map[string]bool{specEvents, mcpEvents, clientOnly} {
			for k := range s {
				m[k] = true
			}
		}
		return m
	}()
	answerBound = []string{"ai_user_action", "ai_interaction_scored", "ai_interaction_classified", "ai_llm_response_received"}
	tracing     = []string{"tracing", "tracing/vercel-ai-sdk"}          // integrations of span events, which carry no specVersion
	turnEvents  = set("ai_user_prompt_created", "ai_response_received") // the surface of a turn comes from these
	// intent_summary is not here: the classifier's own intent_summary option sends it, whatever the capture switch
	content   = set("prompt_text", "response_text", "explanation", "call_reason", "capability_detail")
	rawMCP    = set("arguments", "args", "params", "result", "results", "content", "structuredContent", "uri", "request")
	runFields = []string{"task_name", "trigger_source", "trigger_event_id", "conversation_id", "account_id",
		"on_behalf_of", "parent_interaction_id", "llm_call_count", "total_tokens"}
	mcpCallFields = []string{"server_name", "client_name", "protocol_version", "tool_name", "tool_status",
		"error_type", "intent", "user_sentiment"}
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// options are the check flags.
type options struct {
	captureContent    bool
	expectLegacy      []string
	noAIEvents        bool
	absent            []string
	feature           []string
	surface           []string
	userID            string
	mcpUserID         string
	mcpAnonymous      bool
	expectSession     bool
	expectJoins       bool
	clientSurface     []string
	clientOptOut      []string
	feedbackEndpoint  []string
	failedInteraction []string
	mcpServer         []string
	mcpIntent         []string
}

// counter is Python's Counter of strings: insertion-ordered.
type counter struct {
	keys []string
	n    map[string]int
}

func newCounter() *counter { return &counter{n: map[string]int{}} }

func (c *counter) add(k string) {
	if _, ok := c.n[k]; !ok {
		c.keys = append(c.keys, k)
	}
	c.n[k]++
}

// repr is the repr of dict(counter).
func (c *counter) repr() string {
	parts := make([]string, len(c.keys))
	for i, k := range c.keys {
		parts[i] = reprStr(k) + ": " + strconv.Itoa(c.n[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// in reports whether v is a string in list: Python's `v in list` for a list of strings.
func in(v any, list []string) bool {
	s, ok := v.(string)
	return ok && slices.Contains(list, s)
}

// reserved is Python's name.startswith(("ai_", "mcp_")).
func reserved(name string) bool {
	return strings.HasPrefix(name, "ai_") || strings.HasPrefix(name, "mcp_")
}

// present returns the keys of o that are in want, in key order.
func present(o *Object, want map[string]bool) []string {
	var out []string
	for _, k := range o.keys {
		if want[k] {
			out = append(out, k)
		}
	}
	return out
}

// clientTool recognizes joined tools even when their name is also expected legacy traffic.
func clientTool(name string, props, wrapper *Object, expectedLegacy []string) bool {
	toolField := props.Has("tool_name") || props.Has("tool_status")
	joined := props.Has("conversation_id") && props.Has("interaction_id")
	return !reserved(name) && toolField && (joined || (wrapper.Len() == 0 && !slices.Contains(expectedLegacy, name)))
}

type side struct{ event, side string }

func check(messages []*Object, sch *schemas, opts options, out io.Writer) (int, error) {
	mcpIntents := map[string][]string{} // server ("": every one) -> intents
	for _, entry := range opts.mcpIntent {
		server, intent := "", entry
		if i := strings.LastIndex(entry, ":"); i >= 0 {
			server, intent = entry[:i], entry[i+1:]
		}
		mcpIntents[server] = append(mcpIntents[server], intent)
	}
	var problems, notes []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	events := tracks(messages)
	var interactionOrder []string
	byInteraction := map[string]map[side]int{}
	conversations := map[string]map[string]bool{}
	names, legacy, clientsSeen := newCounter(), newCounter(), newCounter()
	answers := map[string]*Object{}
	surfaces := map[string]any{}
	clientSurfaces := map[string]any{}
	features, clientFeatures := map[string]any{}, map[string]any{} // a turn's feature, like its surface
	scopes := map[string]map[string]map[string]bool{}              // interaction -> feature|surface -> values
	declared := map[string]int{}                                   // non-MCP declared tool events per interaction
	actions := map[string]map[string]*counter{}                    // interaction -> side -> action_type counts
	var runs, toolCalls []*Object

	// Client schema names and declared tools count as browser AI traffic.
	isClientName := func(name string) bool { return sch.client.defs.Has(name) && name != "declared_tool_event" }
	browserEvents := 0
	for i, m := range events {
		name, props := m.Get("event").(string), obj(m.Get("properties"))
		user := m.Get("userId")
		if !truthy(user) {
			user = m.Get("user_id")
		}
		anon := m.Get("anonymousId")
		if !truthy(anon) {
			anon = m.Get("anonymous_id")
		}
		context := obj(m.Get("context"))
		w := obj(context.Get("wrapperLibrary"))
		// AI join IDs identify client tools even when their customer name is also legacy.
		isClient := props.Get("source") == "client" &&
			(isClientName(name) || clientTool(name, props, w, opts.expectLegacy))
		if isClient {
			browserEvents++
		}
		where := fmt.Sprintf("#%d %s", i, name)
		// Provenance: only the AI SDKs' wrapperLibrary marks an AI send; another adapter's tag is still legacy,
		// unless the event uses an AI name not listed in --expect-legacy (then it fails the SDK-name check).
		aiName := specEvents[name] || mcpEvents[name] || clientOnly[name]
		foreign := w.Len() > 0 && !in(w.Get("name"), sdkNames) && (!aiName || slices.Contains(opts.expectLegacy, name))
		if (w.Len() == 0 || foreign) && !isClient { // not sent through the AI SDK: legacy or other analytics
			legacy.add(name)
			if clientOnly[name] && !slices.Contains(opts.expectLegacy, name) { // browser-only: a missing source is the bug
				fail("%s: client-only event without source: client (pass --expect-legacy %s if it is a legacy event)", where, name)
			} else if (specEvents[name] || mcpEvents[name]) && !slices.Contains(opts.expectLegacy, name) {
				fail("%s: no context.wrapperLibrary (not sent by the AI SDK; pass --expect-legacy %s if it is a legacy event)", where, name)
			}
			continue
		}
		names.add(name)
		isMCP := mcpEvents[name] || (!isClient && w.Get("integration") == "mcp")
		// a raw tracing span may carry a custom eventName; past the legacy branch, any other event with a
		// customer name was sent by an AI SDK as a declared tool event
		isSpan := name == "ai_otel_span" || (!isClient && in(w.Get("integration"), tracing) && !knownEvents[name])
		isDeclared := !reserved(name) && !isSpan && !(isClient && sch.client.defs.Has(name))
		if !truthy(user) && !truthy(anon) { // the browser SDK sends anonymousId on every event
			fail("%s: no userId or anonymousId", where)
		}
		if !isDeclared && !isSpan && !isClient && !knownEvents[name] {
			fail("%s: not an AI events spec name (misspelled?)", where)
		}
		if clientOnly[name] && !isClient {
			fail("%s: client-only event without source: client", where)
		}
		if isClient { // client names come from the client schema's $defs
			clientDefinition := name
			if !sch.client.defs.Has(name) {
				clientDefinition = "declared_tool_event"
			}
			errs, err := sch.errors(sch.client, clientDefinition, props)
			if err != nil {
				return 0, err
			}
			for _, e := range errs {
				fail("%s (client schema): %s", where, e)
			}
		}
		definition := name
		if isDeclared {
			definition = "declared_tool_event"
			if isMCP {
				definition = "mcp_declared_tool_event"
			}
		}
		if specEvents[name] || mcpEvents[name] || isDeclared { // client copies: spec and client schema
			if props.Get("source") == "client" && !clientCopies[name] && !isClient {
				fail("%s: source: client on an event the browser never sends", where)
			}
			errs, err := sch.errors(sch.spec, definition, props)
			if err != nil {
				return 0, err
			}
			for _, e := range errs {
				fail("%s (spec): %s", where, e)
			}
		}
		// wrapper metadata on every SDK event; tracing spans (ai_otel_span) carry no specVersion
		if !isClient && !in(w.Get("integration"), tracing) && w.Get("specVersion") != specVersion {
			fail("%s: context.wrapperLibrary.specVersion is %s", where, repr(w.Get("specVersion")))
		}
		if !isClient && !in(w.Get("name"), sdkNames) {
			fail("%s: context.wrapperLibrary.name is %s, not one of %s", where, repr(w.Get("name")), reprList(sdkNames))
		}
		// trackAction takes no feature, so server ai_user_action has none; MCP events have none either;
		// optionalFeature events are checked only when they carry one
		featureOptional := (optionalFeature[name] || isSpan) && !props.Has("feature")
		if len(opts.feature) > 0 && !isMCP && !featureOptional && (isClient || name != "ai_user_action") &&
			!in(props.Get("feature"), opts.feature) {
			fail("%s: feature %s not in %s", where, repr(props.Get("feature")), reprList(opts.feature))
		}
		system := name != "ai_feedback_requested" && props.Get("trigger") == "system"
		optionalSurface := name == "ai_user_action" && !isClient && !props.Has("surface") // trackAction
		if len(opts.surface) > 0 && !system && !optionalSurface && (isClient || clientCopies[name]) &&
			!in(props.Get("surface"), opts.surface) {
			fail("%s: surface %s not in %s", where, repr(props.Get("surface")), reprList(opts.surface))
		}
		if !opts.captureContent {
			leaked := present(props, content)
			if obj(props.Get("action_details")).Has("feedback_text") {
				leaked = append(leaked, "feedback_text")
			}
			if len(leaked) > 0 {
				fail("%s: content sent with capture off: %s", where, reprList(sortedSet(leaked)))
			}
		}
		if system && truthy(user) {
			fail("%s: system-triggered event carries userId %s", where, repr(user))
		}
		if !system && !isClient && (name == "ai_user_prompt_created" || name == "ai_response_received") {
			if opts.userID != "" && !truthy(user) { // an anonymousId fallback hides broken user wiring
				fail("%s: no userId, expected %s", where, reprStr(opts.userID))
			} else if opts.userID != "" && user != opts.userID {
				fail("%s: userId %s, expected %s", where, repr(user), reprStr(opts.userID))
			}
			if opts.expectSession && !truthy(context.Get("sessionId")) {
				fail("%s: no context.sessionId (cookies not read?)", where)
			}
		}
		// feedback endpoints pass the real user or none (never a placeholder): only a present one is compared
		if opts.userID != "" && !isClient && name == "ai_user_action" && truthy(user) && user != opts.userID {
			fail("%s: userId %s, expected %s", where, repr(user), reprStr(opts.userID))
		}
		if system && name == "ai_response_received" && !isClient {
			if !truthy(props.Get("task_name")) {
				notes = append(notes, where+": system answer without task_name (a call outside any interaction or task?)")
			}
			runs = append(runs, props)
		}
		if isMCP {
			// declared tool events carry the properties their rule chose; only SDK-owned events are guarded
			if leaked := present(props, rawMCP); mcpEvents[name] && len(leaked) > 0 {
				fail("%s: raw MCP payload keys %s", where, reprList(sortedSet(leaked)))
			}
			if opts.mcpUserID != "" && !truthy(user) {
				fail("%s: no userId, expected %s (identify not wired?)", where, reprStr(opts.mcpUserID))
			} else if opts.mcpUserID != "" && user != opts.mcpUserID {
				fail("%s: userId %s, expected %s", where, repr(user), reprStr(opts.mcpUserID))
			}
			if opts.mcpAnonymous && truthy(user) {
				fail("%s: userId %s on an unauthenticated MCP server (expected none)", where, repr(user))
			} else if opts.mcpAnonymous && anon != "ai_system" {
				fail("%s: anonymousId %s, expected 'ai_system'", where, repr(anon))
			}
			if len(opts.mcpServer) > 0 && !in(props.Get("server_name"), opts.mcpServer) {
				fail("%s: server_name %s not in %s", where, repr(props.Get("server_name")), reprList(opts.mcpServer))
			}
			if keys, ok := props.Get("argument_keys").([]any); ok && slices.ContainsFunc(keys, func(k any) bool { return k == "request_context" }) {
				// A tool declaring its own request_context gets it untouched; the SDK reads nothing from it.
				if len(present(props, requestContextFields)) > 0 {
					fail("%s: request_context reached the tool (argument_keys)", where)
				} else {
					notes = append(notes, where+": request_context in argument_keys (fine if the tool declares its own)")
				}
			}
			server := props.Get("server_name")
			serverName, _ := server.(string)
			allowed := append(append(slices.Clone(mcpIntents[""]), "other"), mcpIntents[serverName]...)
			if intent, ok := props.Lookup("intent"); ok && len(opts.mcpIntent) > 0 && !in(intent, allowed) {
				fail("%s: intent %s not in %s's list", where, repr(intent), repr(server))
			}
			if name == "mcp_client_seen" {
				seen := "(" + strings.Join([]string{repr(props.Get("server_name")), repr(props.Get("client_name")),
					repr(props.Get("oauth_client_id")), repr(user)}, ", ") + ")"
				clientsSeen.add(seen)
			}
			if name == "mcp_tool_called" {
				toolCalls = append(toolCalls, props)
			}
			continue // MCP interaction_id joins the calling answer; it is not an answer of its own
		}
		if iid, ok := key(props.Get("interaction_id")); ok {
			c, seen := byInteraction[iid]
			if !seen {
				c = map[side]int{}
				byInteraction[iid] = c
				interactionOrder = append(interactionOrder, iid)
			}
			s := "server"
			if isClient {
				s = "client"
			}
			// a paused run resumes under the same interaction_id: count only final answers
			paused := name == "ai_response_received" && props.Get("response_status") == "paused"
			scoped := name == "ai_user_action" && in(props.Get("action_type"), []string{"feedback_given", "feedback_retracted", "feedback_dismissed"}) && in(obj(props.Get("action_details")).Get("feedback_scope"), scopedFeedback)
			switch {
			case paused:
				c[side{name + " (paused)", s}]++
			case scoped:
				c[side{name + " (scoped)", s}]++
			default:
				c[side{name, s}]++
			}
			if conversation, ok := key(props.Get("conversation_id")); ok {
				if conversations[iid] == nil {
					conversations[iid] = map[string]bool{}
				}
				conversations[iid][conversation] = true
			}
			for _, k := range []string{"feature", "surface"} { // every copy of a turn lands in the same dashboard segment
				if v, ok := props.Get(k).(string); ok && v != "" {
					if scopes[iid] == nil {
						scopes[iid] = map[string]map[string]bool{"feature": {}, "surface": {}}
					}
					scopes[iid][k][v] = true
				}
			}
			if isDeclared {
				declared[iid]++
			}
			if action, ok := props.Get("action_type").(string); ok && name == "ai_user_action" {
				if actions[iid] == nil {
					actions[iid] = map[string]*counter{"server": newCounter(), "client": newCounter()}
				}
				actions[iid][s].add(action)
			}
			if !isClient && truthy(props.Get("surface")) && turnEvents[name] { // not later feedback
				surfaces[iid] = props.Get("surface")
			}
			if _, seen := clientSurfaces[iid]; isClient && truthy(props.Get("surface")) && !seen {
				clientSurfaces[iid] = props.Get("surface")
			}
			if !isClient && truthy(props.Get("feature")) && turnEvents[name] {
				features[iid] = props.Get("feature")
			}
			if _, seen := clientFeatures[iid]; isClient && truthy(props.Get("feature")) && !seen {
				clientFeatures[iid] = props.Get("feature")
			}
			if name == "ai_response_received" && !isClient && (!paused || answers[iid] == nil) {
				answers[iid] = props
			}
		}
	}

	// Server rows are the counted ones; client copies (source: client) sit alongside them, one per side.
	hasClientCopies := false
	for _, c := range byInteraction {
		if c[side{"ai_response_received", "client"}] > 0 || c[side{"ai_user_prompt_created", "client"}] > 0 {
			hasClientCopies = true
		}
	}
	// feedback types with a server endpoint, per (feature, surface): planned
	// ([[feature:]surface:]action_type), or sent by the server on that feature's turns on that surface;
	// others are browser-only. "" stands for an unknown part, and in a planned scope for every turn.
	turnPart := func(server, client map[string]any, iid string) string {
		v := server[iid]
		if !truthy(v) {
			v = client[iid]
		}
		s, _ := v.(string)
		return s
	}
	type scope struct{ feature, surface string }
	turnScope := func(iid string) scope {
		return scope{turnPart(features, clientFeatures, iid), turnPart(surfaces, clientSurfaces, iid)}
	}
	planned, inferred := map[scope][]string{}, map[scope][]string{}
	for _, endpoint := range opts.feedbackEndpoint {
		parts := strings.Split(endpoint, ":")
		actionType, parts := parts[len(parts)-1], parts[:len(parts)-1]
		var s scope // a part left out matches every turn
		switch n := len(parts); {
		case n >= 2:
			s = scope{parts[n-2], parts[n-1]}
		case n == 1:
			s = scope{"", parts[0]}
		}
		planned[s] = append(planned[s], actionType)
	}
	for iid, sides := range actions {
		s := turnScope(iid) // exact scope, unknown parts included
		inferred[s] = append(inferred[s], sides["server"].keys...)
	}
	serverActions := func(iid string) []string {
		turn := turnScope(iid)
		out := slices.Clone(inferred[turn])
		for s, acts := range planned {
			if (s.feature == "" || s.feature == turn.feature) && (s.surface == "" || s.surface == turn.surface) {
				out = append(out, acts...)
			}
		}
		return sortedSet(out)
	}
	actionCount := func(iid, s, action string) int {
		if actions[iid] == nil {
			return 0
		}
		return actions[iid][s].n[action]
	}
	// --client-surface [feature:]surface: the turns that have a browser client; "" feature: any
	var clientScopes [][2]string
	for _, c := range opts.clientSurface {
		feature, surface := "", c
		if i := strings.LastIndex(c, ":"); i >= 0 {
			feature, surface = c[:i], c[i+1:]
		}
		clientScopes = append(clientScopes, [2]string{feature, surface})
	}
	runIDs := map[string]bool{}
	for _, r := range runs {
		if iid, ok := key(r.Get("interaction_id")); ok {
			runIDs[iid] = true
		}
	}
	gap := func(s string) {
		if opts.expectJoins {
			problems = append(problems, s)
		} else {
			notes = append(notes, s)
		}
	}
	optedOut := func(name string) bool { return slices.Contains(opts.clientOptOut, name) }
	for _, iid := range interactionOrder {
		c := byInteraction[iid]
		// one browser copy per answer the browser rendered: a paused one and the resumed final one
		shown := max(1, c[side{"ai_response_received", "server"}]+c[side{"ai_response_received (paused)", "server"}])
		for _, s := range []string{"server", "client"} {
			limit := 1
			if s == "client" {
				limit = shown
			}
			if n := c[side{"ai_response_received", s}]; n > limit {
				atMost := ""
				if limit > 1 {
					atMost = "at most "
				}
				fail("interaction %s: %d %s ai_response_received (expected %s%d)", iid, n, s, atMost, limit)
			}
			if n := c[side{"ai_user_prompt_created", s}]; n > 1 {
				fail("interaction %s: %d %s ai_user_prompt_created (expected at most 1)", iid, n, s)
			}
		}
		serverPrompt, clientPrompt := c[side{"ai_user_prompt_created", "server"}], c[side{"ai_user_prompt_created", "client"}]
		serverAnswer, clientAnswer := c[side{"ai_response_received", "server"}], c[side{"ai_response_received", "client"}]
		serverPaused, clientPaused := c[side{"ai_response_received (paused)", "server"}], c[side{"ai_response_received (paused)", "client"}]
		if serverPrompt > 0 && serverAnswer+serverPaused == 0 {
			fail("interaction %s: server prompt without a server ai_response_received", iid)
		}
		// exempt only a known server-only surface; an ID with no server event (stale browser ID) stays checked
		surface, known := surfaces[iid]
		serverOnly := len(opts.clientSurface) > 0 && known && !slices.ContainsFunc(clientScopes, func(sc [2]string) bool {
			return surface == sc[1] && (sc[0] == "" || features[iid] == sc[0])
		})
		if (hasClientCopies || opts.expectJoins) && !runIDs[iid] && !serverOnly {
			if clientPrompt > 0 && serverPrompt == 0 {
				gap("interaction " + iid + ": client prompt with no server prompt (request never reached the SDK, or a different ID)")
			}
			if serverPrompt > 0 && clientPrompt == 0 && !optedOut("client_prompt") {
				gap("interaction " + iid + ": server prompt with no client copy (browser copy not sent, or a different ID)")
			}
			if serverAnswer+serverPaused > 0 && clientAnswer == 0 && !optedOut("client_response") {
				gap("interaction " + iid + ": server answer with no client copy (not shown, client left, or client event lost)")
			}
			if clientAnswer > 0 && serverAnswer+serverPaused == 0 { // the browser sends a success copy for an approval pause
				gap("interaction " + iid + ": client answer with no server answer (request never reached the SDK, or a different ID)")
			}
			for _, action := range serverActions(iid) {
				server, browser := actionCount(iid, "server", action), actionCount(iid, "client", action)
				switch {
				case server > 0 && browser == 0 && !optedOut("client_feedback"):
					gap("interaction " + iid + ": server ai_user_action " + action + " with no client copy (browser handler not wired?)")
				case browser > 0 && server == 0 && !optedOut("server_feedback"):
					gap("interaction " + iid + ": client ai_user_action " + action + " with no server copy (endpoint not wired, or a different ID)")
				case server != browser && !optedOut("client_feedback") && !optedOut("server_feedback"):
					gap(fmt.Sprintf("interaction %s: %d server and %d client ai_user_action %s (one copy per side per click: one lost or sent twice)", iid, server, browser, action))
				}
			}
		}
		// An opted-out copy was dropped from the plan, so sending it is a regression, not a gap.
		if optedOut("client_prompt") && clientPrompt > 0 {
			fail("interaction %s: client prompt copy sent, but the plan opts it out", iid)
		}
		if optedOut("client_response") && clientAnswer > 0 {
			fail("interaction %s: client answer copy sent, but the plan opts it out", iid)
		}
		if optedOut("client_feedback") && slices.ContainsFunc(serverActions(iid), func(a string) bool { return actionCount(iid, "client", a) > 0 }) {
			fail("interaction %s: client feedback copy sent, but the plan opts it out", iid)
		}
		if optedOut("server_feedback") && actions[iid] != nil && len(actions[iid]["server"].keys) > 0 {
			fail("interaction %s: server ai_user_action sent, but the plan opts it out (server_feedback)", iid)
		}
		// events bound to an answer: without one, the final answer was lost or sent under another ID
		for _, event := range answerBound {
			if (c[side{event, "server"}] > 0 || c[side{event, "client"}] > 0) && serverAnswer+clientAnswer+serverPaused+clientPaused == 0 {
				gap("interaction " + iid + ": " + event + " for an answer not in this capture")
			}
		}
		if declared[iid] > 0 && serverAnswer+clientAnswer+serverPaused+clientPaused == 0 {
			var names []string
			for k := range c {
				if !reserved(k.event) {
					names = append(names, k.event)
				}
			}
			sort.Strings(names)
			gap("interaction " + iid + ": declared tool event " + reprList(names) + " for an answer not in this capture")
		}
		for _, k := range []string{"feature", "surface"} {
			if len(scopes[iid][k]) > 1 {
				var values []string
				for v := range scopes[iid][k] {
					values = append(values, v)
				}
				fail("interaction %s: several %s values %s", iid, k, reprList(sortedSet(values)))
			}
		}
		if len(conversations[iid]) > 1 {
			var ids []string
			for id := range conversations[iid] {
				ids = append(ids, id)
			}
			fail("interaction %s: several conversation_id values %s", iid, reprList(sortedSet(ids)))
		}
	}

	for _, iid := range opts.failedInteraction {
		answer, ok := answers[iid]
		switch {
		case !ok:
			fail("failed interaction %s: no server ai_response_received", iid)
		case !in(answer.Get("response_status"), failureStatuses):
			fail("failed interaction %s: response_status %s, expected one of %s", iid,
				repr(answer.Get("response_status")), reprList(failureStatuses))
		case !truthy(answer.Get("error_type")):
			fail("failed interaction %s: no error_type (end it with a status and error type)", iid)
		}
	}

	for _, r := range runs {
		parent := r.Get("parent_interaction_id")
		if truthy(parent) {
			if id, ok := key(parent); !ok || byInteraction[id] == nil {
				notes = append(notes, fmt.Sprintf("task %s: parent_interaction_id %s not in this capture", text(r.Get("task_name")), text(parent)))
			}
		}
		fmt.Fprintln(out, "run:", pick(r, runFields))
	}

	for _, k := range clientsSeen.keys {
		if n := clientsSeen.n[k]; n > 1 {
			notes = append(notes, fmt.Sprintf("mcp_client_seen %s: %d times in one window (several processes, or a restart?)", k, n))
		}
	}
	for _, t := range toolCalls {
		joined := ""
		if iid, ok := key(t.Get("interaction_id")); ok && answers[iid] != nil { // the calling answer, not any event sharing the ID
			joined = "joined"
		}
		fmt.Fprintln(out, "mcp call:", pick(t, mcpCallFields), joined)
	}
	if len(toolCalls) > 0 {
		named := 0
		for _, t := range toolCalls {
			if truthy(t.Get("client_name")) {
				named++
			}
		}
		fmt.Fprintf(out, "mcp client_name: on %d of %d tool calls\n", named, len(toolCalls))
	}

	for _, name := range opts.expectLegacy {
		if legacy.n[name] == 0 {
			fail("legacy event %s was not sent (it must keep sending during migration)", name)
		}
	}
	for _, name := range opts.absent { // legacy provenance only: an AI event may share the legacy event's name
		if n := legacy.n[name]; n > 0 {
			fail("legacy %s sent %d times; its switch is off", name, n)
		}
	}
	// browser AI events carry no wrapperLibrary: trackAi must follow the switch too
	sdkEvents := 0
	for _, m := range events {
		if in(obj(obj(m.Get("context")).Get("wrapperLibrary")).Get("name"), sdkNames) {
			sdkEvents++
		}
	}
	if opts.noAIEvents && sdkEvents > 0 {
		fail("%d AI SDK events sent with the AI events switch off", sdkEvents)
	}
	if opts.noAIEvents && browserEvents > 0 {
		fail("%d browser AI events sent with the AI events switch off (trackAi not gated on it)", browserEvents)
	}
	if opts.expectJoins && !opts.noAIEvents && len(answers) == 0 { // browser events alone hide the server side
		fail("no server ai_response_received captured (--expect-joins needs the server side)")
	}
	if !opts.noAIEvents && sdkEvents == 0 && browserEvents == 0 { // a silent recorder must not pass
		fail("no AI events captured (recorder not wired, or the AI events switch is off)")
	}

	sdks := newCounter()
	for _, m := range events {
		w := obj(obj(m.Get("context")).Get("wrapperLibrary"))
		if obj(m.Get("properties")).Get("source") != "client" && w.Len() > 0 {
			sdks.add(repr(w.Get("name")))
		}
	}
	parts := make([]string, len(sdks.keys))
	for i, k := range sdks.keys {
		parts[i] = k + ": " + strconv.Itoa(sdks.n[k])
	}
	fmt.Fprintln(out, "events:", names.repr())
	fmt.Fprintln(out, "legacy or other events (no wrapperLibrary):", legacy.repr())
	fmt.Fprintln(out, "sdks:", "{"+strings.Join(parts, ", ")+"}")
	fmt.Fprintln(out, "interactions:", len(byInteraction))
	for _, n := range notes {
		fmt.Fprintln(out, "note:", n)
	}
	for _, p := range problems {
		fmt.Fprintln(out, "FAIL:", p)
	}
	if len(problems) > 0 {
		fmt.Fprintln(out, "RESULT: FAIL")
		return 1, nil
	}
	fmt.Fprintln(out, "RESULT: PASS")
	return 0, nil
}

// pick is the repr of {k: o.get(k) for k in keys}.
func pick(o *Object, keys []string) string {
	p := newObject()
	for _, k := range keys {
		p.Set(k, o.Get(k))
	}
	return repr(p)
}

// sortedSet is Python's sorted(set(items)).
func sortedSet(items []string) []string {
	out := slices.Clone(items)
	sort.Strings(out)
	return slices.Compact(out)
}
