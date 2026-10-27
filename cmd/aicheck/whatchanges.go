package main

import (
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// The what-changes command: a redacted before/after diff of two captures of the same driven traffic.
//
// before: AI events switched off (the app as it was); after: AI events on. Prints the event table
// (counts per capture) and, per changed event, a redacted before/after JSON pair with the differing
// fields and values. Equal occurrences pair first, whatever their order; the rest pair in capture
// order, an unmatched one with the other capture's first; the pair shown is the first that differs. Fields that differ on every run (IDs, timestamps, durations) count by presence and type
// only, inside lists too. IDs, user and account IDs, on_behalf_of, emails, traits and timestamps are
// redacted; check the rest before sharing.

var (
	drop     = set("messageId", "timestamp", "originalTimestamp", "sentAt", "receivedAt", "_metadata") // envelope only
	idKey    = regexp.MustCompile(`(^|_)(id|ID)\z|Id\z|[a-z0-9]ID\z|^on_behalf_of\z|[eE]mail`)
	personal = set("traits") // identify() traits (email, name, ...) ride along in context
	email    = regexp.MustCompile(`[^ \t\n\r\f\v@"]+@[^ \t\n\r\f\v@"]+\.[A-Za-z0-9_]+`)
	vary     = regexp.MustCompile(`^(interaction_id|conversation_id|parent_interaction_id|trace_id|anonymousId|anonymous_id|sessionId)\z` +
		// start_time, end_time (ai_otel_span) and OTLP's startTimeUnixNano vary on every run too
		`|_ms\z|[Ll]atency|[Dd]uration|At\z|_at\z|[Tt]imestamp|(^|_)time\z|Time(UnixNano)?\z`)
)

// typeName is Python's type(value).__name__.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case Int:
		return "int"
	case Float:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	}
	return "dict"
}

// withoutOTLP drops properties.otlp: OTLP attributes hold identities as key/value pairs (user.id) the
// name-based redaction misses.
func withoutOTLP(m *Object) *Object {
	props, ok := m.Get("properties").(*Object)
	if !ok || !props.Has("otlp") {
		return m
	}
	p := newObject()
	for _, k := range props.keys {
		if k != "otlp" {
			p.Set(k, props.vals[k])
		}
	}
	out := newObject()
	for _, k := range m.keys {
		out.Set(k, m.vals[k])
	}
	out.Set("properties", p)
	return out
}

// rowKey gives own rows to browser AI copies, legacy events with AI names, and SDK events with
// customer names.
func rowKey(m *Object) string {
	name := m.Get("event").(string)
	if obj(m.Get("properties")).Get("source") == "client" && (clientCopies[name] || clientOnly[name]) {
		return name + " (browser)"
	}
	sdk := in(obj(obj(m.Get("context")).Get("wrapperLibrary")).Get("name"), sdkNames)
	switch {
	case sdk && !reserved(name): // a declared tool event may share a legacy event's name
		return name + " (AI SDK)"
	case !sdk && reserved(name):
		return name + " (legacy)"
	}
	return name
}

// timeKey is vary's timestamp part: masked at any depth in the printed examples, as IDs are.
var timeKey = regexp.MustCompile(`At\z|_at\z|[Tt]imestamp|(^|_)time\z|Time(UnixNano)?\z`)

func redact(v any, k string) any {
	_, isObj := v.(*Object)
	scalar := v != nil && !isObj && !isList(v)
	if personal[k] || (scalar && (idKey.MatchString(k) || timeKey.MatchString(k))) {
		return "<" + k + ">"
	}
	switch v := v.(type) {
	case *Object:
		out := newObject()
		for _, kk := range v.keys {
			out.Set(kk, redact(v.vals[kk], kk))
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redact(item, k)
		}
		return out
	case string:
		return email.ReplaceAllLiteralString(v, "<email>")
	}
	return v
}

func shown(m *Object) any { // envelope fields are per send; nested ones are payload and stay
	o := newObject()
	for _, k := range m.keys {
		if !drop[k] {
			o.Set(k, m.vals[k])
		}
	}
	return redact(o, "")
}

// norm is the comparable form: run-varying and redacted fields keep only their type, at any depth
// (lists too).
func norm(v any, k string) any {
	if vary.MatchString(k) || idKey.MatchString(k) || personal[k] {
		return typeName(v)
	}
	switch v := v.(type) {
	case *Object:
		out := newObject()
		for _, kk := range v.keys {
			out.Set(kk, norm(v.vals[kk], kk))
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = norm(item, k)
		}
		return out
	}
	return redact(v, k)
}

func fields(m *Object, prefix string, out map[string]string) map[string]string {
	for _, k := range m.keys {
		v := m.vals[k]
		if prefix == "" && drop[k] { // envelope only: a payload's own messageId or timestamp is compared by type
			continue
		}
		path := prefix + strings.ReplaceAll(strings.ReplaceAll(k, `\`, `\\`), ".", `\.`) // a literal dot is not nesting
		if o, ok := v.(*Object); ok && o.Len() > 0 && !personal[k] {
			fields(o, path+".", out)
		} else {
			out[path] = dumps(norm(v, k), false)
		}
	}
	return out
}

func sortedKeys(m map[string]string, keep func(string) bool) []string {
	var out []string
	for k := range m {
		if keep(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func diff(fb, fa map[string]string) []string {
	var out []string
	for _, k := range sortedKeys(fa, func(k string) bool { _, ok := fb[k]; return !ok }) {
		out = append(out, "+ "+k)
	}
	for _, k := range sortedKeys(fb, func(k string) bool { _, ok := fa[k]; return !ok }) {
		out = append(out, "- "+k)
	}
	for _, k := range sortedKeys(fb, func(k string) bool { a, ok := fa[k]; return ok && a != fb[k] }) {
		out = append(out, fmt.Sprintf("~ %s (%s -> %s)", k, fb[k], fa[k]))
	}
	return out
}

type change struct {
	name   string
	diff   []string
	mb, ma *Object
}

// whatChanges prints the event table and the changed events' redacted pairs; it always returns 0.
func whatChanges(before, after []*Object, out io.Writer) int {
	byB, byA := map[string][]*Object{}, map[string][]*Object{}
	for _, m := range tracks(before) {
		m = withoutOTLP(m)
		byB[rowKey(m)] = append(byB[rowKey(m)], m)
	}
	for _, m := range tracks(after) {
		m = withoutOTLP(m)
		byA[rowKey(m)] = append(byA[rowKey(m)], m)
	}
	var rows []string
	for k := range byB {
		rows = append(rows, k)
	}
	for k := range byA {
		if byB[k] == nil {
			rows = append(rows, k)
		}
	}
	sort.Strings(rows)

	fmt.Fprintln(out, "| Event | Status | Before | After |\n|---|---|---|---|")
	var changed []change
	for _, name := range rows {
		nb, na := len(byB[name]), len(byA[name])
		var status string
		switch {
		case nb == 0:
			status = "new"
		case na == 0:
			status = "removed"
		default:
			// Compare every occurrence in capture order; a change in a later turn counts too.
			var d []string
			mb, ma := byB[name][0], byA[name][0]
			// equal occurrences pair first, whatever their order (concurrent jobs finish in any order);
			// the rest pair in capture order, and unmatched ones with the other side's first occurrence
			restB, restA := slices.Clone(byB[name]), []*Object(nil)
			for _, m := range byA[name] {
				fm := fields(m, "", map[string]string{})
				twin := slices.IndexFunc(restB, func(o *Object) bool { return maps.Equal(fields(o, "", map[string]string{}), fm) })
				if twin < 0 {
					restA = append(restA, m)
				} else {
					restB = slices.Delete(restB, twin, twin+1)
				}
			}
			var pairs [][2]*Object
			for i := 0; i < len(restB) && i < len(restA); i++ {
				pairs = append(pairs, [2]*Object{restB[i], restA[i]})
			}
			for _, m := range restA[min(len(restB), len(restA)):] {
				pairs = append(pairs, [2]*Object{byB[name][0], m})
			}
			for _, m := range restB[min(len(restB), len(restA)):] {
				pairs = append(pairs, [2]*Object{m, byA[name][0]})
			}
			for _, p := range pairs {
				lines := diff(fields(p[0], "", map[string]string{}), fields(p[1], "", map[string]string{}))
				if len(lines) > 0 && len(d) == 0 {
					mb, ma = p[0], p[1] // show the first pair that actually differs
				}
				for _, l := range lines {
					if !slices.Contains(d, l) {
						d = append(d, l)
					}
				}
			}
			status = "unchanged"
			if len(d) > 0 || nb != na {
				status = "changed"
				changed = append(changed, change{name, d, mb, ma})
			}
		}
		fmt.Fprintf(out, "| `%s` | %s | %d | %d |\n", name, status, nb, na)
	}

	for _, c := range changed {
		differing := strings.Join(c.diff, ", ")
		if differing == "" {
			differing = "count only"
		}
		fmt.Fprintf(out, "\n#### `%s`\n\nDiffering fields: %s\n\n", c.name, differing)
		fmt.Fprintln(out, "Before:\n```json\n"+dumps(shown(c.mb), true)+"\n```")
		fmt.Fprintln(out, "After:\n```json\n"+dumps(shown(c.ma), true)+"\n```")
	}
	return 0
}
