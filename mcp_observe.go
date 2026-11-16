package rudderstackai

import (
	"context"
	"io"
	"os"

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

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
