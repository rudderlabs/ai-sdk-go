package rudderstackai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"
)

func loadFixture(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(into); err != nil {
		t.Fatal(err)
	}
}

type cookiesFixture struct {
	Values []struct {
		Name    string `json:"name"`
		Raw     string `json:"raw"`
		Decoded any    `json:"decoded"`
	} `json:"values"`
	Identities []struct {
		Name     string `json:"name"`
		Cookies  any    `json:"cookies"`
		Expected struct {
			AnonymousID string      `json:"anonymous_id"`
			UserID      string      `json:"user_id"`
			SessionID   json.Number `json:"session_id"`
		} `json:"expected"`
	} `json:"identities"`
}

func TestCookieValuesMatchSharedFixture(t *testing.T) {
	var f cookiesFixture
	loadFixture(t, "spec/fixtures/cookies-v3.json", &f)
	if len(f.Values) == 0 {
		t.Fatal("no values")
	}
	for _, c := range f.Values {
		t.Run(c.Name, func(t *testing.T) {
			got, ok := decodeCookieValue(c.Raw)
			if c.Decoded == nil {
				if ok {
					t.Fatalf("want unreadable, got %#v", got)
				}
				return
			}
			if !ok {
				t.Fatal("want readable")
			}
			if mustJSON(got) != mustJSON(c.Decoded) {
				t.Fatalf("got %s, want %s", mustJSON(got), mustJSON(c.Decoded))
			}
		})
	}
}

func TestCookieIdentitiesMatchSharedFixture(t *testing.T) {
	var f cookiesFixture
	loadFixture(t, "spec/fixtures/cookies-v3.json", &f)
	if len(f.Identities) == 0 {
		t.Fatal("no identities")
	}
	for _, c := range f.Identities {
		t.Run(c.Name, func(t *testing.T) {
			var cookies map[string]string
			switch v := c.Cookies.(type) {
			case string:
				cookies = rudderstackCookies(v, nil)
			case map[string]any:
				m := map[string]string{}
				for k, x := range v {
					m[k] = x.(string)
				}
				cookies = rudderstackCookies("", m)
			default:
				t.Fatalf("cookies %T", v)
			}
			got := identityFromCookies(cookies)
			var session int64
			if c.Expected.SessionID != "" {
				session, _ = c.Expected.SessionID.Int64()
			}
			want := cookieIdentity{anonymousID: c.Expected.AnonymousID, userID: c.Expected.UserID, sessionID: session}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestCookieIdentityOnEvents(t *testing.T) {
	const (
		anon    = "RS_ENC_v3_IjlmN2UxYzJhLTRiM2QtNGU1Zi04YTZiLTdjOGQ5ZTBmMWEyYiI%3D"
		user    = "RS_ENC_v3_InVzw6lyXzQyIOKckyI%3D"
		session = "RS_ENC_v3_eyJpZCI6MTc2MDM0MjQ5OTk5OSwibWFudWFsVHJhY2siOnRydWV9"
		anonID  = "9f7e1c2a-4b3d-4e5f-8a6b-7c8d9e0f1a2b"
	)
	header := "rl_anonymous_id=" + anon + "; rl_user_id=" + user + "; rl_session=" + session
	req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	req.Header.Add("Cookie", "rl_anonymous_id="+anon)
	req.Header.Add("Cookie", "rl_user_id="+user+"; rl_session="+session)

	cases := []struct {
		name                string
		o                   InteractionOptions
		ctx                 func() context.Context
		user, anon          string
		session             bool
		disableGlobalCookie bool
	}{
		{name: "request", o: InteractionOptions{Request: req}, user: "usér_42 ✓", anon: anonID, session: true},
		{name: "header", o: InteractionOptions{Cookies: header}, user: "usér_42 ✓", anon: anonID, session: true},
		{name: "values win over header", o: InteractionOptions{Cookies: header, CookieValues: map[string]string{"rl_user_id": "RS_ENC_v3_IiI%3D"}}, anon: anonID, session: true},
		{name: "explicit user wins", o: InteractionOptions{UserID: "me", Cookies: header}, user: "me", anon: anonID, session: true},
		// A cookie userId never overrides an explicit anonymous visitor.
		{name: "explicit anonymous", o: InteractionOptions{AnonymousID: "visitor", Cookies: header}, anon: "visitor", session: true},
		{name: "option off", o: InteractionOptions{Cookies: header, IdentityFromCookies: Ptr(false)}},
		{name: "context cookies", ctx: func() context.Context {
			return WithValues(context.Background(), ContextValues{Request: req})
		}, user: "usér_42 ✓", anon: anonID, session: true},
		{name: "global off", o: InteractionOptions{Cookies: header}, disableGlobalCookie: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := setupAI(t, WithIdentityFromCookies(!c.disableGlobalCookie))
			ctx := context.Background()
			if c.ctx != nil {
				ctx = c.ctx()
			}
			_, i := StartInteraction(ctx, c.o)
			i.End(EndOptions{})
			for _, m := range r.all() {
				wantAnon := c.anon
				if c.user == "" && c.anon == "" {
					wantAnon = systemAnonymousID
				}
				if m.UserId != c.user || m.AnonymousId != wantAnon {
					t.Fatalf("%s: user %q anon %q", m.Event, m.UserId, m.AnonymousId)
				}
				_, has := m.Context.Extra["sessionId"]
				if has != c.session {
					t.Fatalf("%s: sessionId present %v", m.Event, has)
				}
			}
		})
	}
}

func TestSessionIDKeepsAppValue(t *testing.T) {
	ctx := map[string]any{"sessionId": 7}
	if got := withSessionID(ctx, 9); got["sessionId"] != 7 {
		t.Fatal(got)
	}
	if got := withSessionID(nil, 0); got != nil {
		t.Fatal(got)
	}
	if requestCookies(nil) != "" {
		t.Fatal("nil request")
	}
}

type providersFixture struct {
	Aliases map[string]string `json:"aliases"`
	Routers []string          `json:"routers"`
	Cases   []struct {
		Input, Expected string
	} `json:"cases"`
	RouterCases []struct {
		Provider, Model, Expected string
	} `json:"router_cases"`
	RequestedModelCases []struct {
		Requested string  `json:"requested"`
		Served    *string `json:"served"`
		Expected  *string `json:"expected"`
	} `json:"requested_model_cases"`
}

func TestProvidersMatchSharedFixture(t *testing.T) {
	var f providersFixture
	loadFixture(t, "spec/fixtures/providers-v0.1.json", &f)
	if !reflect.DeepEqual(f.Aliases, providerAliases) {
		t.Fatalf("aliases differ from the fixture:\n%v\n%v", f.Aliases, providerAliases)
	}
	if !reflect.DeepEqual(f.Routers, routers) {
		t.Fatalf("routers differ: %v vs %v", f.Routers, routers)
	}
	for _, c := range f.Cases {
		if got := normalizeProvider(c.Input); got != c.Expected {
			t.Errorf("normalizeProvider(%q) = %q, want %q", c.Input, got, c.Expected)
		}
	}
	for _, c := range f.RouterCases {
		if got := routedProvider(c.Provider, c.Model); got != c.Expected {
			t.Errorf("routedProvider(%q, %q) = %q, want %q", c.Provider, c.Model, got, c.Expected)
		}
	}
	for _, c := range f.RequestedModelCases {
		served, want := "", ""
		if c.Served != nil {
			served = *c.Served
		}
		if c.Expected != nil {
			want = *c.Expected
		}
		if got := routedModel(c.Requested, served); got != want {
			t.Errorf("routedModel(%q, %q) = %q, want %q", c.Requested, served, got, want)
		}
	}
	if len(f.Cases) == 0 || len(f.RouterCases) == 0 || len(f.RequestedModelCases) == 0 {
		t.Fatal("empty fixture")
	}
	if normalizeProvider("") != "" || routedProvider("", "openai/x") != "" {
		t.Fatal("empty provider")
	}
}

func TestNormalizeServiceTier(t *testing.T) {
	for _, c := range []struct{ provider, in, want string }{
		{"openai", "", ""},
		{"openai", "Batch", "batch"},
		{"openai", "flex", "flex"},
		{"openai", "default", "standard"},
		{"openai", "auto", "standard"},
		{"anthropic", "standard_only", "standard"},
		{"openai", "priority", "fast"},
		{"anthropic", "priority", "other"},
		{"firstParty", "priority", "other"},
		{"openai", "scale", "other"},
	} {
		if got := normalizeServiceTier(c.provider, c.in); got != c.want {
			t.Errorf("normalizeServiceTier(%q, %q) = %q, want %q", c.provider, c.in, got, c.want)
		}
	}
}
