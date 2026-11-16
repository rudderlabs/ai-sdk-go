package ruddermcpgo

import (
	"context"
	"io"
	"net/http"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// ObserveHTTP wraps the handler serving a server wrapped with [WrapServer] (typically
// server.NewStreamableHTTPServer), so that failed requests carry error_message (with
// CaptureContent on) taken from the final JSON-RPC response written to the client, as JSON or as
// an SSE frame, matched by request id:
//
//	http.Handle("/mcp", ruddermcpgo.ObserveHTTP(server.NewStreamableHTTPServer(s)))
//
// Without it, error_message is left out: OnAfterCallTool hooks can still change a result after
// WrapServer reads it. Responses pass through unchanged. A failed request's events are sent once
// its response is written, or without error_message when the HTTP request ends first.
func ObserveHTTP(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses := mcpbridge.NewResponses()
		mcpbridge.ServeHTTP(h, w, r.WithContext(mcpbridge.WithResponses(r.Context(), responses)), responses)
	})
}

// ObserveStdio returns the context, reader and writer to pass to StdioServer.Listen, so that
// failed requests carry error_message as with [ObserveHTTP], read from the newline-delimited
// responses. The reader notices a client reusing a request id before its response is written.
//
//	ctx, in, out := ruddermcpgo.ObserveStdio(ctx, os.Stdin, os.Stdout)
//	err := server.NewStdioServer(s).Listen(ctx, in, out)
//
// Held events are sent without error_message when ctx ends.
func ObserveStdio(ctx context.Context, r io.Reader, w io.Writer) (context.Context, io.Reader, io.Writer) {
	responses := mcpbridge.NewResponses()
	responses.CloseWhenDone(ctx)
	rc, ok := r.(io.ReadCloser)
	if !ok {
		rc = io.NopCloser(r)
	}
	return mcpbridge.WithResponses(ctx, responses), mcpbridge.NewReader(rc, responses), mcpbridge.NewWriter(w, responses)
}
