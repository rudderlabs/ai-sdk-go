package rudderstackai

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCallerModelSharedFixture(t *testing.T) {
	var fixture struct {
		MetaKeys  []string `json:"meta_keys"`
		Pattern   string   `json:"model_pattern"`
		MaxLength int      `json:"max_length"`
		Cases     []struct {
			Name     string         `json:"name"`
			Meta     any            `json:"meta"`
			Expected map[string]any `json:"expected"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("spec/fixtures/mcp-caller-model-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.MetaKeys, []string{metaAIInvocation, metaCodexTurn}) ||
		fixture.Pattern != callerModelPattern.String() || fixture.MaxLength != maxLabelLength {
		t.Fatal("keys, pattern or bound differ from shared fixture")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			meta, _ := tc.Meta.(map[string]any)
			if got := callerModel(meta); !reflect.DeepEqual(got, tc.Expected) {
				t.Fatalf("callerModel = %v, want %v", got, tc.Expected)
			}
		})
	}
}

func TestMCPCallerModel(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{EventRules: EventRules{Tools: map[string]ToolEventRule{"list_destinations": {Event: "destinations_listed"}}}})
	cs := connect(t, s, "2025-11-25", nil)

	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, mcp.Meta{
		metaCodexTurn: map[string]any{
			"model":      "gpt-5.2-codex",
			"workspaces": map[string]any{"/Users/jane.doe/src/app": map[string]any{"has_changes": true}},
		},
		metaAIInvocation: map[string]any{"userIntent": "refund my order"},
	})
	p := r.props(t, "mcp_tool_called")
	if p["caller_model"] != "gpt-5.2-codex" || p["caller_model_source"] != "client_metadata" {
		t.Fatalf("caller model not sent: %v", p)
	}
	if _, ok := r.props(t, "destinations_listed")["caller_model"]; ok {
		t.Fatal("caller_model on the declared tool event")
	}
	if sent := mustJSON(r.all()); strings.Contains(sent, "jane.doe") || strings.Contains(sent, "refund my order") {
		t.Fatalf("other _meta content sent: %s", sent)
	}

	r.reset()
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	p = r.props(t, "mcp_tool_called")
	if _, ok := p["caller_model"]; ok {
		t.Fatalf("caller_model without a model: %v", p)
	}
	if _, ok := p["caller_model_source"]; ok {
		t.Fatalf("caller_model_source without a model: %v", p)
	}
}

// App callbacks run while the request is handled; one that changes _meta must not change the
// recorded model.
func TestMCPCallerModelReadBeforeCallbacks(t *testing.T) {
	for name, hook := range map[string]func(*Options, func()){
		"Identify": func(o *Options, mutate func()) {
			o.Identify = func(context.Context, *auth.TokenInfo) (Identity, error) { mutate(); return Identity{UserID: "u"}, nil }
		},
		"ClientInfoFallback": func(o *Options, mutate func()) {
			o.ClientInfoFallback = func(context.Context, *auth.TokenInfo) (string, string) { mutate(); return "", "" }
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			var current *mcp.CallToolRequest
			mutate := func() {
				if current != nil {
					current.Params.Meta[metaCodexTurn] = map[string]any{"model": "server-mutated"}
				}
			}
			opts := Options{}
			hook(&opts, mutate)
			r := wrap(t, s, opts)
			// Added after WrapMCPServer, so it runs first: it only hands the request to the callback.
			s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					if call, ok := req.(*mcp.CallToolRequest); ok {
						current = call
					}
					return next(ctx, method, req)
				}
			})
			cs := connect(t, s, "2025-11-25", nil)
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, mcp.Meta{metaCodexTurn: map[string]any{"model": "gpt-5.2-codex"}})
			if p := r.props(t, "mcp_tool_called"); p["caller_model"] != "gpt-5.2-codex" {
				t.Fatalf("caller_model = %v, want the client's", p["caller_model"])
			}
		})
	}
}

// mutatingSchema runs app code while the wrapper lists the server's tools for a request.
type mutatingSchema struct{ mutate func() }

func (m mutatingSchema) MarshalJSON() ([]byte, error) {
	m.mutate()
	return []byte(`{"type":"object"}`), nil
}

func TestMCPCallerModelReadBeforeSchemaMarshalers(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: t.Name(), Version: "1"}, nil)
	var current *mcp.CallToolRequest
	mutate := func() {
		if current != nil && current.Params.Meta != nil {
			current.Params.Meta[metaCodexTurn] = map[string]any{"model": "schema-mutated"}
		}
	}
	s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: mutatingSchema{mutate}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	r := wrap(t, s, Options{})
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if call, ok := req.(*mcp.CallToolRequest); ok {
				current = call
			}
			return next(ctx, method, req)
		}
	})
	cs := connect(t, s, "2025-11-25", nil)
	callTool(t, cs, "lookup", map[string]any{}, mcp.Meta{metaCodexTurn: map[string]any{"model": "gpt-5.2-codex"}})
	if p := r.props(t, "mcp_tool_called"); p["caller_model"] != "gpt-5.2-codex" {
		t.Fatalf("caller_model = %v, want the client's", p["caller_model"])
	}
}
