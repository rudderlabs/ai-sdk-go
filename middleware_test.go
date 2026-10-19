package rudderstackai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareCookiesReachIdentity(t *testing.T) {
	r := setupAI(t)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = RunInteraction(req.Context(), InteractionOptions{}, func(context.Context, *Interaction) error { return nil })
	}))
	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("Cookie", `rl_anonymous_id=%22anon-mw%22; rl_user_id=%22user-mw%22`)
	h.ServeHTTP(httptest.NewRecorder(), req)
	m := r.named("ai_response_received")
	if len(m) != 1 {
		t.Fatalf("got %d ai_response_received", len(m))
	}
	if m[0].AnonymousId != "anon-mw" || m[0].UserId != "user-mw" {
		t.Fatalf("identity = %q / %q", m[0].UserId, m[0].AnonymousId)
	}
	if _, ok := m[0].Properties["client_disconnected"]; ok {
		t.Fatal("client_disconnected set on a completed request")
	}
}

func TestMiddlewareMarksClientDisconnected(t *testing.T) {
	r := setupAI(t)
	ctx, cancel := context.WithCancel(context.Background())
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, i := StartInteraction(req.Context(), InteractionOptions{UserID: "u1"})
		cancel() // the client goes away while the answer is being made
		i.End(EndOptions{})
	}))
	req := httptest.NewRequest(http.MethodPost, "/chat", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := r.response(t)["client_disconnected"]; got != true {
		t.Fatalf("client_disconnected = %v", got)
	}
}

func TestMiddlewareCancelAfterHandlerIsNotADisconnect(t *testing.T) {
	r := setupAI(t)
	ctx, cancel := context.WithCancel(context.Background())
	var open *Interaction
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, open = StartInteraction(req.Context(), InteractionOptions{UserID: "u1"})
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/chat", nil).WithContext(ctx))
	cancel() // net/http cancels the request context once the handler returns
	open.End(EndOptions{})
	if _, ok := r.response(t)["client_disconnected"]; ok {
		t.Fatal("client_disconnected set after the handler returned")
	}
}
