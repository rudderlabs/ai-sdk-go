package rudderstackai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Identity from the RudderStack JavaScript SDK (v3) cookies: rl_anonymous_id, rl_user_id and the
// rl_session id. Only these cookies are ever read; the request and the app's own cookies are not
// kept.

var rudderstackCookieNames = []string{"rl_anonymous_id", "rl_user_id", "rl_session"}

const encryptedPrefix = "RS_ENC_v3_"

type cookieIdentity struct {
	anonymousID string
	userID      string
	sessionID   int64
}

// parseCookieHeader splits a Cookie header into raw (still URL-encoded) values; a repeated name
// keeps the last value.
func parseCookieHeader(header string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(header, ";") {
		at := strings.Index(pair, "=")
		if at < 0 {
			continue
		}
		if name := strings.TrimSpace(pair[:at]); name != "" {
			out[name] = strings.TrimSpace(pair[at+1:])
		}
	}
	return out
}

// rudderstackCookies keeps only the RudderStack cookies of a header and a map (the map wins).
func rudderstackCookies(header string, values map[string]string) map[string]string {
	raw := parseCookieHeader(header)
	for k, v := range values {
		raw[k] = v
	}
	out := map[string]string{}
	for _, name := range rudderstackCookieNames {
		if v, ok := raw[name]; ok {
			out[name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// requestCookies returns a request's Cookie headers joined, or "".
func requestCookies(r *http.Request) string {
	if r == nil {
		return ""
	}
	return strings.Join(r.Header.Values("Cookie"), "; ")
}

// decodeCookieValue decodes one cookie value as the JavaScript SDK wrote it: URL-decoded, then
// RS_ENC_v3_ + base64 JSON, or plain JSON. ok is false when unreadable.
func decodeCookieValue(raw string) (any, bool) {
	value, err := url.PathUnescape(raw)
	if err != nil || value == "" || !utf8.ValidString(value) {
		return nil, false
	}
	if strings.HasPrefix(value, encryptedPrefix) {
		b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(value[len(encryptedPrefix):], "="))
		if err != nil || !utf8.Valid(b) {
			return nil, false
		}
		value = string(b)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(value)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, false
	}
	return v, v != nil
}

// identityFromCookies reads rl_anonymous_id, rl_user_id and the rl_session id; unreadable cookies
// are skipped.
func identityFromCookies(cookies map[string]string) cookieIdentity {
	var id cookieIdentity
	read := func(name string) any {
		raw, ok := cookies[name]
		if !ok {
			return nil
		}
		v, ok := decodeCookieValue(raw)
		if !ok {
			debugf("unreadable cookie (ignored)", "name", name)
		}
		return v
	}
	if s, ok := read("rl_anonymous_id").(string); ok {
		id.anonymousID = s
	}
	if s, ok := read("rl_user_id").(string); ok {
		id.userID = s
	}
	if session, ok := read("rl_session").(map[string]any); ok && !sessionEnded(session) {
		if n, ok := session["id"].(json.Number); ok {
			if f, err := n.Float64(); err == nil && f > 0 && f == math.Trunc(f) && f <= 1<<53-1 {
				id.sessionID = int64(f)
			}
		}
	}
	return id
}

// sessionEnded: an rl_session past its expiry or its enabled cut-off.
func sessionEnded(session map[string]any) bool {
	now := float64(time.Now().UnixMilli())
	past := func(v any) bool {
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f < now
	}
	if past(session["expiresAt"]) {
		return true
	}
	cut, _ := session["cutOff"].(map[string]any)
	return cut != nil && cut["enabled"] == true && past(cut["expiresAt"])
}

// identityInputs are explicit identity plus cookie inputs.
type identityInputs struct {
	userID, anonymousID string
	cookies             map[string]string
	fromCookies         *bool
}

type resolvedIdentity struct {
	userID, anonymousID string
	sessionID           int64
}

// resolveIdentity: explicit options > explicit context values > cookies (the options' own, else
// the context's). The cookie userId is only a fallback for a visitor nobody identified.
func resolveIdentity(in identityInputs, cv *contextValues, c config) resolvedIdentity {
	cookies := in.cookies
	enabled := c.identityFromCookies
	if cv != nil {
		if cookies == nil {
			cookies = cv.cookies
		}
		if cv.identityFromCookies != nil {
			enabled = *cv.identityFromCookies
		}
	}
	if in.fromCookies != nil {
		enabled = *in.fromCookies
	}
	var cookie cookieIdentity
	if cookies != nil && enabled {
		safely(func() { cookie = identityFromCookies(cookies) })
	}
	userID, anonymousID := in.userID, in.anonymousID
	if cv != nil {
		if userID == "" {
			userID = cv.userID
		}
		if anonymousID == "" {
			anonymousID = cv.anonymousID
		}
	}
	out := resolvedIdentity{userID: userID, anonymousID: anonymousID, sessionID: cookie.sessionID}
	if out.userID == "" && anonymousID == "" {
		out.userID = cookie.userID
	}
	if out.anonymousID == "" {
		out.anonymousID = cookie.anonymousID
	}
	return out
}

// withSessionID adds sessionId to the message context unless the app set one.
func withSessionID(ctx map[string]any, sessionID int64) map[string]any {
	if sessionID == 0 {
		return ctx
	}
	if _, set := ctx["sessionId"]; set {
		return ctx
	}
	out := make(map[string]any, len(ctx)+1)
	for k, v := range ctx {
		out[k] = v
	}
	out["sessionId"] = sessionID
	return out
}
