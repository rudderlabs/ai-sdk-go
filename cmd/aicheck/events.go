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
		"ai_conversation_switched", "ai_conversation_renamed", "ai_conversation_deleted")
	mcpEvents = set("mcp_client_seen", "mcp_tool_called", "mcp_elicitation_completed", "mcp_capability_requested",
		"mcp_resource_read", "mcp_prompt_used", "mcp_event_subscription_changed", "mcp_event_delivered")
	content   = set("prompt_text", "response_text", "intent_summary", "explanation", "call_reason", "capability_detail")
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
	expectSession     bool
	expectJoins       bool
	clientSurface     []string
	clientOptOut      []string
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

type side struct{ event, side string }

func check(messages []*Object, sch *schemas, opts options, out io.Writer) (int, error) {
	var problems, notes []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	events := tracks(messages)
	var interactionOrder []string
	byInteraction := map[string]map[side]int{}
	conversations := map[string]map[string]bool{}
	names, legacy, clientsSeen := newCounter(), newCounter(), newCounter()
	answers := map[string]*Object{}
	surfaces := map[string]any{}
	var runs, toolCalls []*Object

	// browser AI event names: only these count as AI traffic when they carry source: client
	isClientName := func(name string) bool { return strings.HasPrefix(name, "ai_") && sch.client.defs.Has(name) }
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
		isClient := props.Get("source") == "client" && isClientName(name) // a legacy event may use source too
		where := fmt.Sprintf("#%d %s", i, name)
		// Provenance: only the AI SDKs' wrapperLibrary marks an AI send; another adapter's tag is still legacy,
		// unless the event uses an AI name (then it falls through and fails the SDK-name check).
		foreign := w.Len() > 0 && !in(w.Get("name"), sdkNames) && !specEvents[name] && !mcpEvents[name] && !clientOnly[name]
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
		isMCP := mcpEvents[name] || w.Get("integration") == "mcp"
		isDeclared := !reserved(name) && props.Has("tool_status")
		if !reserved(name) && !isDeclared {
			continue
		}
		if !truthy(user) && !truthy(anon) && !isClient {
			fail("%s: no userId or anonymousId", where)
		}
		if clientOnly[name] && !isClient {
			fail("%s: client-only event without source: client", where)
		}
		if isClient {
			if sch.client.defs.Has(name) {
				errs, err := sch.errors(sch.client, name, props)
				if err != nil {
					return 0, err
				}
				for _, e := range errs {
					fail("%s (client schema): %s", where, e)
				}
			} else {
				fail("%s: not a client event name", where)
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
			if isClient && !clientCopies[name] {
				fail("%s: source: client on an event the browser never sends", where)
			}
			errs, err := sch.errors(sch.spec, definition, props)
			if err != nil {
				return 0, err
			}
			for _, e := range errs {
				fail("%s (spec): %s", where, e)
			}
			if !isClient && w.Get("specVersion") != specVersion {
				fail("%s: context.wrapperLibrary.specVersion is %s", where, repr(w.Get("specVersion")))
			}
			if !isClient && !in(w.Get("name"), sdkNames) {
				fail("%s: context.wrapperLibrary.name is %s, not one of %s", where, repr(w.Get("name")), reprList(sdkNames))
			}
		}
		// trackAction takes no feature, so server ai_user_action has none; MCP events have none either;
		// a standalone score carries one only when given, so check it only when present
		optionalFeature := name == "ai_interaction_scored" && !props.Has("feature")
		if len(opts.feature) > 0 && !isMCP && !optionalFeature && (isClient || name != "ai_user_action") &&
			!in(props.Get("feature"), opts.feature) {
			fail("%s: feature %s not in %s", where, repr(props.Get("feature")), reprList(opts.feature))
		}
		system := props.Get("trigger") == "system"
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
		if system && name == "ai_response_received" {
			if !truthy(props.Get("task_name")) {
				notes = append(notes, where+": system answer without task_name (a call outside any interaction or task?)")
			}
			runs = append(runs, props)
		}
		if isMCP {
			if leaked := present(props, rawMCP); len(leaked) > 0 {
				fail("%s: raw MCP payload keys %s", where, reprList(sortedSet(leaked)))
			}
			if len(opts.mcpServer) > 0 && !in(props.Get("server_name"), opts.mcpServer) {
				fail("%s: server_name %s not in %s", where, repr(props.Get("server_name")), reprList(opts.mcpServer))
			}
			if keys, ok := props.Get("argument_keys").([]any); ok && slices.ContainsFunc(keys, func(k any) bool { return k == "request_context" }) {
				fail("%s: request_context reached the tool (argument_keys)", where)
			}
			if intent, ok := props.Lookup("intent"); ok && len(opts.mcpIntent) > 0 && !in(intent, append(slices.Clone(opts.mcpIntent), "other")) {
				fail("%s: intent %s not in the server's list", where, repr(intent))
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
			if paused {
				c[side{name + " (paused)", s}]++
			} else {
				c[side{name, s}]++
			}
			if conversation, ok := key(props.Get("conversation_id")); ok {
				if conversations[iid] == nil {
					conversations[iid] = map[string]bool{}
				}
				conversations[iid][conversation] = true
			}
			if !isClient && truthy(props.Get("surface")) && clientCopies[name] {
				surfaces[iid] = props.Get("surface")
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
		for _, s := range []string{"server", "client"} {
			if n := c[side{"ai_response_received", s}]; n > 1 {
				fail("interaction %s: %d %s ai_response_received (expected 1)", iid, n, s)
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
		surface, known := surfaces[iid] // an ID with no server surface stays join-checked
		serverOnly := len(opts.clientSurface) > 0 && known && !in(surface, opts.clientSurface)
		if (hasClientCopies || opts.expectJoins) && !runIDs[iid] && !serverOnly {
			if clientPrompt > 0 && serverPrompt == 0 {
				gap("interaction " + iid + ": client prompt with no server prompt (request never reached the SDK, or a different ID)")
			}
			if serverPrompt > 0 && clientPrompt == 0 && !optedOut("client_prompt") {
				gap("interaction " + iid + ": server prompt with no client copy (browser copy not sent, or a different ID)")
			}
			if serverAnswer > 0 && clientAnswer == 0 && !optedOut("client_response") {
				gap("interaction " + iid + ": server answer with no client copy (not shown, client left, or client event lost)")
			}
			if clientAnswer > 0 && serverAnswer == 0 {
				gap("interaction " + iid + ": client answer with no server answer (request never reached the SDK, or a different ID)")
			}
		}
		// An opted-out copy was dropped from the plan, so sending it is a regression, not a gap.
		if optedOut("client_prompt") && clientPrompt > 0 {
			fail("interaction %s: client prompt copy sent, but the plan opts it out", iid)
		}
		if optedOut("client_response") && clientAnswer > 0 {
			fail("interaction %s: client answer copy sent, but the plan opts it out", iid)
		}
		for _, event := range []string{"ai_user_action", "ai_interaction_scored"} {
			if (c[side{event, "server"}] > 0 || c[side{event, "client"}] > 0) && serverAnswer+clientAnswer+serverPaused+clientPaused == 0 {
				gap("interaction " + iid + ": " + event + " for an answer not in this capture")
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
	sdkEvents, browserEvents := 0, 0
	for _, m := range events {
		if in(obj(obj(m.Get("context")).Get("wrapperLibrary")).Get("name"), sdkNames) {
			sdkEvents++
		}
		name := m.Get("event").(string)
		if obj(m.Get("properties")).Get("source") == "client" && isClientName(name) {
			browserEvents++
		}
	}
	if opts.noAIEvents && sdkEvents > 0 {
		fail("%d AI SDK events sent with the AI events switch off", sdkEvents)
	}
	if opts.noAIEvents && browserEvents > 0 {
		fail("%d browser AI events sent with the AI events switch off (trackAi not gated on it)", browserEvents)
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
