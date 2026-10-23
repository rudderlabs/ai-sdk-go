package kit

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// onErrors collects what WithOnError sees for the rest of the test.
func onErrors(t *testing.T) func() []rudderstackai.ErrorInfo {
	t.Helper()
	var mu sync.Mutex
	var seen []rudderstackai.ErrorInfo
	hook.Reset() // the hook is rate-limited per kind
	if err := rudderstackai.Configure(rudderstackai.WithLLMCallEvents(true), rudderstackai.WithOnError(func(_ error, info rudderstackai.ErrorInfo) {
		mu.Lock()
		seen = append(seen, info)
		mu.Unlock()
	})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rudderstackai.Configure(rudderstackai.WithOnError(nil)) })
	return func() []rudderstackai.ErrorInfo {
		mu.Lock()
		defer mu.Unlock()
		return append([]rudderstackai.ErrorInfo(nil), seen...)
	}
}

func TestInterceptPanicBeforeSendIsContained(t *testing.T) {
	seen := onErrors(t)
	h := newHarness(t, jsonHandler(200, `{"object":"chat.completion","model":"gpt-4.1","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	h.spec.IsStream = func(*http.Request, map[string]any) bool { panic("sdk bug") }
	ctx, _ := interaction(h.rec)
	resp, err := h.post(ctx, `{"model":"gpt-4.1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, resp); got == "" {
		t.Fatal("the request did not go through")
	}
	if s := seen(); len(s) != 1 || s[0].Kind != rudderstackai.KindInternal || s[0].Where != "kit.Intercept" {
		t.Fatalf("onError saw %+v", s)
	}
}

func TestInterceptPanicAfterSendKeepsTheResponse(t *testing.T) {
	seen := onErrors(t)
	h := newHarness(t, jsonHandler(200, `{}`))
	ctx, _ := interaction(h.rec)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL, nil)
	// A transport returning neither a response nor an error trips the SDK's reading of it.
	resp, err := h.spec.Intercept(req, func(*http.Request) (*http.Response, error) { return nil, nil })
	if resp != nil || err != nil {
		t.Fatalf("got %v, %v; want the transport's nil, nil", resp, err)
	}
	if s := seen(); len(s) != 1 || s[0].Kind != rudderstackai.KindInternal {
		t.Fatalf("onError saw %+v", s)
	}
}

func TestInterceptTransportPanicPropagates(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{}`))
	ctx, _ := interaction(h.rec)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL, nil)
	defer func() {
		if r := recover(); r != "transport" {
			t.Fatalf("recovered %v; want the transport's own panic", r)
		}
	}()
	_, _ = h.spec.Intercept(req, func(*http.Request) (*http.Response, error) { panic("transport") })
}

func TestInterceptTransportErrorIsReturnedUnchanged(t *testing.T) {
	h := newHarness(t, jsonHandler(200, `{}`))
	h.spec.ErrorType = func(int, map[string]any) string { panic("sdk bug") }
	ctx, _ := interaction(h.rec)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL, nil)
	want := errors.New("provider down")
	if _, err := h.spec.Intercept(req, func(*http.Request) (*http.Response, error) { return nil, want }); err != want {
		t.Fatalf("err = %v; want the transport's error", err)
	}
}
