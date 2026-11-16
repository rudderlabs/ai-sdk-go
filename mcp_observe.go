package rudderstackai

import (
	"context"
	"io"
	"net/http"
	"os"
	"reflect"
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
// It works for stateful and stateless handlers. The request's header map identifies it to the
// server's handlers, which the go-sdk passes on as RequestExtra.Header; a middleware between the
// two that copies the headers (Request.Clone) leaves error_message out. Responses pass through
// unchanged. A failed request's events are sent once its response is written, or without
// error_message when the HTTP request ends first.
func ObserveMCPHTTP(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := headerKey(r.Header)
		if key == nil {
			h.ServeHTTP(w, r)
			return
		}
		responses := mcpbridge.NewResponses()
		// Wrapped twice: the outer wrapper observes.
		if _, nested := httpObservers.LoadOrStore(key, responses); nested {
			h.ServeHTTP(w, r)
			return
		}
		defer httpObservers.Delete(key)
		mcpbridge.ServeHTTP(h, w, r, responses)
	})
}

// httpObservers maps the header map of each HTTP request ObserveMCPHTTP is serving to its
// observer. A stateful session runs every request on its own context, never the POST's, so the
// header map is the only per-request value the server's handlers can match on.
var httpObservers sync.Map

func headerKey(h http.Header) unsafe.Pointer {
	if h == nil {
		return nil
	}
	return reflect.ValueOf(h).UnsafePointer()
}

// observedHTTP is the observer of the HTTP request with header map h, or nil.
func observedHTTP(h http.Header) *mcpbridge.Responses {
	key := headerKey(h)
	if key == nil {
		return nil
	}
	r, _ := httpObservers.Load(key)
	responses, _ := r.(*mcpbridge.Responses)
	return responses
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
