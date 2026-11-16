package rudderstackai

import (
	"context"
	"reflect"
	"unsafe"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// ObserveMCPTransport wraps the transport a server wrapped with [WrapMCPServer] connects over,
// so that failed requests carry error_message (with CaptureContent on) taken from the final
// JSON-RPC response written to the client, matched by request id:
//
//	server.Run(ctx, rudderstackai.ObserveMCPTransport(&mcp.StdioTransport{}))
//
// Without it, error_message is left out: middleware added after WrapMCPServer can still replace
// a result or error. Messages pass through unchanged. A failed request's events are sent after
// its response is written, or without error_message when the write fails, the connection
// closes or no response follows within a few seconds.
//
// StreamableHTTPHandler builds its transports internally, so HTTP servers cannot use it yet.
func ObserveMCPTransport(t mcp.Transport) mcp.Transport {
	if t == nil {
		return nil
	}
	return &observedTransport{inner: t}
}

type observedTransport struct{ inner mcp.Transport }

func (t *observedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.inner.Connect(ctx)
	if err != nil || c == nil {
		return c, err
	}
	return &observedConn{Connection: c, responses: mcpbridge.NewResponses()}, nil
}

// SupportsProtocolVersion keeps the wrapped transport's answer; a transport without one
// supports every version.
func (t *observedTransport) SupportsProtocolVersion(version string) bool {
	if s, ok := t.inner.(mcp.ProtocolVersionSupporter); ok {
		return s.SupportsProtocolVersion(version)
	}
	return true
}

type observedConn struct {
	mcp.Connection
	responses *mcpbridge.Responses
}

func (c *observedConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	err := c.Connection.Write(ctx, msg)
	resp, ok := msg.(*jsonrpc.Response)
	if !ok || resp == nil {
		return err
	}
	safely(func() {
		if err != nil {
			c.responses.Fail(resp.ID.Raw())
			return
		}
		if c.responses.Waiting() {
			if data, encErr := jsonrpc.EncodeMessage(resp); encErr == nil {
				c.responses.DeliverJSON(data)
			}
		}
	})
	return err
}

func (c *observedConn) Close() error {
	c.responses.Close()
	return c.Connection.Close()
}

// observedResponses is the observer of ss's connection when its transport was wrapped with
// ObserveMCPTransport. go-sdk v1.8.0 offers no getter for a session's connection, so it is read
// by reflection; a changed layout finds none, and error_message stays out.
func observedResponses(ss *mcp.ServerSession) (r *mcpbridge.Responses) {
	if ss == nil {
		return nil
	}
	safely(func() {
		f := reflect.ValueOf(ss).Elem().FieldByName("mcpConn")
		if !f.IsValid() || f.Type() != reflect.TypeFor[mcp.Connection]() {
			return
		}
		if c, ok := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(*observedConn); ok {
			r = c.responses
		}
	})
	return r
}
