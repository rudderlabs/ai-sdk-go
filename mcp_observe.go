package rudderstackai

import (
	"context"
	"io"
	"net/http"
	"os"
	"reflect"
	"slices"
	"sync"
	"unsafe"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// ObserveMCPTransport returns the context and transport to connect a server wrapped with
// [WrapMCPServer] with, so that failed requests carry error_message (with CaptureContent on)
// taken from the final JSON-RPC response written to the client, matched by request id:
//
//	ctx, t := rudderstackai.ObserveMCPTransport(ctx, &mcp.StdioTransport{})
//	err := server.Run(ctx, t)
//
// It observes *mcp.StdioTransport and *mcp.IOTransport at their reader and writer, so the
// go-sdk's own connection and protocol handling are unchanged; any other transport (and an
// IOTransport without a reader or writer) is returned as is, and error_message stays out.
// Without it, error_message is left out: middleware added after WrapMCPServer can still replace
// a result or error. A failed request's events are sent after its response is written, or
// without error_message when the connection or ctx ends or no response follows within 10 seconds.
func ObserveMCPTransport(ctx context.Context, t mcp.Transport) (context.Context, mcp.Transport) {
	var observed *mcp.IOTransport
	switch x := t.(type) {
	case *mcp.StdioTransport:
		if x != nil {
			// What StdioTransport.Connect uses.
			observed = &mcp.IOTransport{Reader: os.Stdin, Writer: nopCloser{os.Stdout}, MaxLineLength: x.MaxLineLength}
		}
	case *mcp.IOTransport:
		if x != nil && x.Reader != nil && x.Writer != nil {
			cp := *x
			observed = &cp
		}
	}
	if observed == nil {
		return ctx, t
	}
	responses := mcpbridge.NewResponses()
	responses.CloseWhenDone(ctx) // the connection closing also closes it
	observed.Reader = mcpbridge.NewReader(observed.Reader, responses)
	observed.Writer = mcpbridge.NewWriter(observed.Writer, responses)
	// The go-sdk derives every request's context from the one passed to Server.Connect.
	return mcpbridge.WithResponses(ctx, responses), observed
}

// ObserveMCPHTTP wraps the go-sdk StreamableHTTPHandler serving a server wrapped with
// [WrapMCPServer], so that failed requests carry error_message (with CaptureContent on) taken
// from the final JSON-RPC response written to the client, as JSON or as an SSE frame, matched by
// request id within the HTTP request that carried it:
//
//	http.Handle("/mcp", rudderstackai.ObserveMCPHTTP(mcp.NewStreamableHTTPHandler(getServer, opts)))
//
// It works for stateful and stateless handlers. Wrap the outermost handler, so it reads the
// bytes the client receives after any middleware. The request's header map identifies it to
// the server's handlers, which the go-sdk passes on as RequestExtra.Header: if a middleware
// inside it copies the headers (Request.Clone), wrap the handler again inside that middleware;
// the inner wrapper defers to the outer one. Without that, error_message is left out, as it is
// for requests in flight together that share one header map, and when a middleware between two
// wrappers replaces the request context. A middleware that hands one header map to several
// requests (which also races in net/http) is not supported, in front of the wrapper or inside
// it. Responses pass through unchanged. A failed request's events are sent once its response is
// written, or without error_message when the HTTP request ends first or the client leaves
// before the response is sent.
func ObserveMCPHTTP(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := headerKey(r.Header)
		outer, _ := r.Context().Value(observingHTTP{}).(*observingHTTPRequest)
		if key == nil || (outer != nil && slices.Contains(outer.keys, key)) {
			h.ServeHTTP(w, r)
			return
		}
		session := r.Header.Get("Mcp-Session-Id")
		if outer != nil {
			// Behind a middleware that copied the headers: the outer wrapper reads the response,
			// so the copy only needs to point at its observer.
			defer observeHTTP(key, session, outer.responses)()
			next := &observingHTTPRequest{responses: outer.responses, keys: append(slices.Clip(outer.keys), key)}
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), observingHTTP{}, next)))
			return
		}
		responses := mcpbridge.NewResponses()
		defer observeHTTP(key, session, responses)()
		next := &observingHTTPRequest{responses: responses, keys: []unsafe.Pointer{key}}
		mcpbridge.ServeHTTP(h, w, r.WithContext(context.WithValue(r.Context(), observingHTTP{}, next)), responses)
	})
}

type observingHTTP struct{}

// observingHTTPRequest is the outermost wrapper's observer and every header map registered for it.
type observingHTTPRequest struct {
	responses *mcpbridge.Responses
	keys      []unsafe.Pointer
}

// httpObservers maps the header map of each HTTP request ObserveMCPHTTP is serving to its
// observer. A stateful session runs every request on its own context, never the POST's, so the
// header map is the only per-request value the server's handlers can match on.
var (
	httpObserversMu sync.Mutex
	httpObservers   = map[unsafe.Pointer]*httpObserver{}
	// httpSessions is the observer each session first matched. A request without a session
	// header (initialize, stateless) only matches sessions it opened, never one seen before, so a
	// call that outlived its own request can't take another session's response.
	httpSessions weakMap[mcp.ServerSession, *mcpbridge.Responses]
)

type httpObserver struct {
	// responses is nil once two requests in flight share the header map: their requests can't
	// be told apart, so neither is observed.
	responses *mcpbridge.Responses
	// session is the request's Mcp-Session-Id: a call from another session never matches, even
	// when a middleware reuses the header map after that call's own request ended.
	session  string
	inFlight int
}

// observeHTTP registers responses for the HTTP request with header map key and session, and
// returns its release.
func observeHTTP(key unsafe.Pointer, session string, responses *mcpbridge.Responses) (release func()) {
	httpObserversMu.Lock()
	defer httpObserversMu.Unlock()
	o := httpObservers[key]
	if o == nil {
		o = &httpObserver{responses: responses, session: session}
		httpObservers[key] = o
	} else if o.responses != nil {
		// Another request already holds this map: its calls may have found the observer, so
		// release them too.
		o.responses.Close()
		o.responses = nil
	}
	o.inFlight++
	return func() {
		httpObserversMu.Lock()
		defer httpObserversMu.Unlock()
		if o.inFlight--; o.inFlight == 0 {
			delete(httpObservers, key)
		}
	}
}

func headerKey(h http.Header) unsafe.Pointer {
	if h == nil {
		return nil
	}
	return reflect.ValueOf(h).UnsafePointer()
}

// observedHTTP is the observer of the HTTP request with header map h, for a call in session ss
// (sessionID its ID), or nil.
func observedHTTP(h http.Header, ss *mcp.ServerSession, sessionID string) *mcpbridge.Responses {
	key := headerKey(h)
	if key == nil {
		return nil
	}
	httpObserversMu.Lock()
	o := httpObservers[key]
	var responses *mcpbridge.Responses
	var session string
	if o != nil {
		responses, session = o.responses, o.session
	}
	httpObserversMu.Unlock()
	if responses == nil || (session != "" && sessionID != "" && session != sessionID) {
		return nil
	}
	if ss == nil {
		return responses
	}
	matched := false
	httpSessions.update(ss, func(first *mcpbridge.Responses, seen bool) *mcpbridge.Responses {
		switch {
		case !seen:
			matched = true
			return responses
		case session != "" || first == responses:
			// A request with the session's own header, or one the session opened.
			matched = true
		}
		return first
	})
	if !matched {
		return nil
	}
	return responses
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
