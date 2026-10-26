// Command aicheck is the migration check for apps that move their own AI events to the RudderStack
// AI SDK (github.com/rudderlabs/ai-sdk-go).
//
//	go install github.com/rudderlabs/ai-sdk-go/cmd/aicheck@latest
//	aicheck events.jsonl --expect-legacy chatTurnCompleted --expect-joins
//	aicheck what-changes before.jsonl after.jsonl
//
// The check validates a JSONL capture (written by ruddertest.Recorder) against the AI events spec
// the SDK emits and the browser-event schema, and checks legacy events, the AI events switch,
// client/server joins and app-caught failures. Exit code 0: pass, 1: a check failed, 2: bad usage or
// an unreadable file. what-changes prints a redacted before/after diff of two captures for a PR
// description. The Node.js (npx @rudderstack/ai check) and Python (python -m rudderstack.ai.check)
// checkers print the same lines.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const usage = `usage: aicheck <events.jsonl> [flags]
       aicheck what-changes <before.jsonl> <after.jsonl>

check flags (repeat the list flags once per value):
  --spec <file>                 AI events spec schema (default: the one this SDK emits)
  --client-schema <file>        browser AI events schema (default: the one shipped with this SDK)
  --capture-content             the plan sets capture_content: true
  --expect-legacy <event>       legacy event that must still be sent
  --absent <event>              legacy event that must not appear (its switch is off)
  --no-ai-events                captured with the AI events switch off: no AI event may appear
  --feature <value>             allowed feature values
  --surface <value>             allowed surface values
  --user-id <id>                userId expected on user-triggered server prompts and answers
  --expect-session              user-triggered server prompts and answers carry context.sessionId
  --expect-joins                the browser was driven with the server's IDs: every client/server gap fails
  --client-surface <surface>    a surface with a browser client: only its turns need client copies
  --client-opt-out <copy>       client_prompt, client_response, client_feedback or server_feedback:
                                a copy the plan opts out of (fails if sent)
  --feedback-endpoint <type>    action_type with a planned endpoint: its browser copy needs a server one
  --failed-interaction <id>     interaction the app failed and caught: its answer must report the failure
  --mcp-server <name>           allowed MCP server_name values
  --mcp-intent <intent>         allowed request_context intents (other is implied)

exit code: 0 pass, 1 a check failed, 2 bad usage or an unreadable file
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run runs the checker with command-line arguments (without the program name) and returns the
// exit code.
func run(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprint(stdout, usage)
			return 0
		}
	}
	code, err := dispatch(args, stdout)
	var usageErr *errUsage
	switch {
	case errors.As(err, &usageErr):
		fmt.Fprintf(stderr, "error: %s\n%s", err, usage)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}
	return code
}

func dispatch(args []string, stdout io.Writer) (int, error) {
	if len(args) > 0 && args[0] == "what-changes" {
		p, err := parseArgs(args[1:], []string{"before", "after"}, nil)
		if err != nil {
			return 0, err
		}
		before, err := load(p.positionals[0])
		if err != nil {
			return 0, err
		}
		after, err := load(p.positionals[1])
		if err != nil {
			return 0, err
		}
		return whatChanges(before, after, stdout), nil
	}
	p, err := parseArgs(args, []string{"events"}, checkFlags)
	if err != nil {
		return 0, err
	}
	events, err := load(p.positionals[0])
	if err != nil {
		return 0, err
	}
	spec, err := readSchema(p.last("spec"), specSchema)
	if err != nil {
		return 0, err
	}
	client, err := readSchema(p.last("client-schema"), clientSchema)
	if err != nil {
		return 0, err
	}
	sch, err := newSchemas(spec, client)
	if err != nil {
		return 0, err
	}
	return check(events, sch, options{
		captureContent:    p.bools["capture-content"],
		expectLegacy:      p.lists["expect-legacy"],
		noAIEvents:        p.bools["no-ai-events"],
		absent:            p.lists["absent"],
		feature:           p.lists["feature"],
		surface:           p.lists["surface"],
		userID:            p.last("user-id"),
		expectSession:     p.bools["expect-session"],
		expectJoins:       p.bools["expect-joins"],
		clientSurface:     p.lists["client-surface"],
		clientOptOut:      p.lists["client-opt-out"],
		feedbackEndpoint:  p.lists["feedback-endpoint"],
		failedInteraction: p.lists["failed-interaction"],
		mcpServer:         p.lists["mcp-server"],
		mcpIntent:         p.lists["mcp-intent"],
	}, stdout)
}
