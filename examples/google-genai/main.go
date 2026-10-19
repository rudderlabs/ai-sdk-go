// Command google-genai answers a weather question with the Google Gen AI SDK on the Gemini API,
// tracked by ruddergenai, inside a RudderStack interaction: generateContent calls a get_weather
// tool, the app runs it, and the answer to the function response streams back.
//
//	GEMINI_API_KEY=... RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/google-genai
//
// With --check it runs end to end against a mock data plane, with the model calls answered from
// spec/fixtures/cassettes/google-genai.json, and exits non-zero when an expected event is missing
// or wrong. It needs no network or credentials. --live runs the same check with the events sent
// to RUDDERSTACK_DATA_PLANE_URL, and is skipped without credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("google-genai", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "run end to end against a mock data plane and exit")
	live := fs.Bool("live", false, "run the check with events sent to RUDDERSTACK_DATA_PLANE_URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *check || *live {
		return harness.Run(stderr, *live, runCheck)
	}
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		key = os.Getenv("GOOGLE_API_KEY")
	}
	if key == "" {
		fmt.Fprintln(stderr, "set GEMINI_API_KEY (or GOOGLE_API_KEY)")
		return 1
	}
	return harness.Once(stderr, func(sink *harness.Sink) error {
		res, err := runSink(sink, &genai.ClientConfig{APIKey: key})
		fmt.Fprintf(stderr, "answer: %s\n", res.Answer)
		return err
	})
}

const (
	model    = "gemini-3.5-flash-lite"
	question = "What's the weather in Paris?"
)

// result is what the app returns to its client: the answer and the IDs feedback refers to.
type result struct {
	Answer, InteractionID, ConversationID string
}

// runSink runs the example with its events going to the sink and a Gemini API client from cfg.
func runSink(sink *harness.Sink, cfg *genai.ClientConfig) (result, error) {
	rs, err := sink.NewClient()
	if err != nil {
		return result{}, err
	}
	ctx := context.Background()
	cfg.Backend = genai.BackendGeminiAPI
	// genai has no middleware: tracking sits in its HTTP client.
	cfg.HTTPClient = ruddergenai.HTTPClient(nil)
	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return result{}, errors.Join(err, rs.Close())
	}
	res, err := weather(ctx, client, rs)
	// Close flushes queued events.
	return res, errors.Join(err, rs.Close())
}

var tools = []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
	Name:        "get_weather",
	Description: "Returns the current weather in a city.",
	Parameters: &genai.Schema{
		Type:       genai.TypeObject,
		Properties: map[string]*genai.Schema{"city": {Type: genai.TypeString}},
		Required:   []string{"city"},
	},
}}}}

// getWeather is the app's tool.
func getWeather(args map[string]any) map[string]any {
	return map[string]any{"city": args["city"], "conditions": "sunny", "temperature_c": 20}
}

// weather prices the model and answers one question in an interaction, running the tool the
// model asks for. ToolBreakdown adds per-tool counts to ai_response_received.
func weather(ctx context.Context, client *genai.Client, rs analytics.Client) (result, error) {
	// Example rates in USD per million tokens: use your own.
	if err := rudderstackai.RegisterPrice(model, rudderstackai.ModelPrice{
		Provider:   "gcp.gemini",
		TokenPrice: rudderstackai.TokenPrice{InputPerMtok: 0.10, OutputPerMtok: 0.40},
	}); err != nil {
		return result{}, err
	}
	var res result
	err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{
		UserID: "user_42", ConversationID: "conv_weather_1", Feature: "travel_assistant", Surface: rudderstackai.SurfaceMobile,
		ToolBreakdown: rudderstackai.Ptr(true), Analytics: rs,
	}, func(ctx context.Context, in *rudderstackai.Interaction) error {
		res.InteractionID, res.ConversationID = in.ID(), in.ConversationID()
		config := &genai.GenerateContentConfig{Tools: tools}
		contents := []*genai.Content{genai.NewContentFromText(question, genai.RoleUser)}
		first, err := client.Models.GenerateContent(ctx, model, contents, config)
		if err != nil {
			return err
		}
		calls := first.FunctionCalls()
		if len(calls) == 0 {
			res.Answer = first.Text()
			return nil
		}
		contents = append(contents, first.Candidates[0].Content)
		var parts []*genai.Part
		for _, c := range calls {
			parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: c.ID, Name: c.Name, Response: getWeather(c.Args)}})
		}
		contents = append(contents, genai.NewContentFromParts(parts, genai.RoleUser))
		for chunk, err := range client.Models.GenerateContentStream(ctx, model, contents, config) {
			if err != nil {
				return err
			}
			res.Answer += chunk.Text()
		}
		return nil
	})
	return res, err
}
