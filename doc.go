// Package rudderstackai sends RudderStack AI product analytics events from Go
// services. It implements the AI events spec v0.3.0 (spec/ai-events-v0.3.schema.json).
//
// [WrapMCPServer] tracks an MCP server built on github.com/modelcontextprotocol/go-sdk:
// which AI clients call it, which tools they call, whether calls succeed, and for which
// customer. Package ruddermcpgo does the same for github.com/mark3labs/mcp-go. State is kept per
// server name, per process. Events never carry tool arguments, results, resource contents, prompt text or
// elicitation answers. Events go through a github.com/rudderlabs/analytics-go/v4 client and
// carry context.wrapperLibrary = {name: "ai-sdk-go", version, specVersion, integration: "mcp"}
// beside the client's own context.library.
//
// Analytics failures never reach the host server: the integration recovers from its own
// panics and drops events it cannot send.
package rudderstackai
