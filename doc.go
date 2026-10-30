// Package rudderstackai sends RudderStack AI product analytics events from Go
// services. It implements the AI events spec v0.4.0 (spec/ai-events-v0.4.schema.json).
//
// AI interactions: [RunInteraction] (or [StartInteraction] and [Interaction.End]) turns one
// user prompt and its answer into ai_user_prompt_created and one ai_response_received with
// models, tokens, cost, latency and status, however many model calls it took. Model calls
// report into the interaction carried by their context, through an integration module
// (rudderopenai, rudderanthropic, ruddergenai, rudderbedrock) or [Interaction.RecordCall].
// [RunTask] runs background work as a system-triggered interaction; [TrackAction] and
// [TrackScore] send feedback and scores. [Configure] sets the RudderStack client and the
// process-wide options. Prompt, answer and feedback text are sent only with content capture
// on, masked.
//
// MCP servers: [WrapMCPServer] tracks an MCP server built on
// github.com/modelcontextprotocol/go-sdk: which AI clients call it, which tools they call,
// whether calls succeed, and for which customer. Package ruddermcpgo does the same for
// github.com/mark3labs/mcp-go. State is kept per server name, per process. Events never carry
// tool arguments, results, resource contents, prompt text or elicitation answers.
//
// Events go through a github.com/rudderlabs/analytics-go/v4 client and carry
// context.wrapperLibrary = {name: "ai-sdk-go", version, specVersion, integration} beside the
// client's own context.library.
//
// Analytics failures never reach the host application: the SDK recovers from its own panics,
// returns provider errors unchanged, and drops events it cannot send.
package rudderstackai
