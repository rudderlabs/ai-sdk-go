// Command tracing records existing OpenTelemetry spans and their product events to a JSONL
// file. It makes no model call and needs no credentials. Replace the recorder with your
// analytics-go client to send the same events to RudderStack.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/rudderotel"
	"github.com/rudderlabs/ai-sdk-go/ruddertest"
)

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("tracing", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "verify the recording without credentials")
	output := fs.String("output", "tracing-events.jsonl", "append events to this JSONL file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *check {
		dir, err := os.MkdirTemp("", "tracing-example-")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		*output = filepath.Join(dir, "events.jsonl")
	}
	if err := record(*output); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *check {
		if err := verify(*output); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stderr, "check passed: one span, one prompt, one response")
	} else {
		fmt.Fprintln(stderr, "recorded events in", *output)
	}
	return 0
}

func record(path string) error {
	client, err := ruddertest.NewRecorder(path)
	if err != nil {
		return err
	}
	processor, err := rudderotel.NewSpanProcessor(rudderotel.Options{Analytics: client, ProductEvents: true, LinkTraceID: true})
	if err != nil {
		return errors.Join(err, client.Close())
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
	ctx, interaction := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{
		UserID: "user_42", ConversationID: "conv_1", Feature: "helpdesk", PromptText: "Where is my order?", Classify: rudderstackai.Ptr(false),
	})
	// Your instrumentation creates this span around its actual model call. Wrappers are optional.
	_, span := provider.Tracer("helpdesk").Start(ctx, "chat gpt-4.1-mini", trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", "openai"),
		attribute.String("gen_ai.request.model", "gpt-4.1-mini"),
	))
	span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", 120), attribute.Int("gen_ai.usage.output_tokens", 25),
		attribute.String("gen_ai.response.id", "response_1"), attribute.StringSlice("gen_ai.response.finish_reasons", []string{"stop"}))
	span.SetStatus(codes.Ok, "")
	span.End()
	interaction.End(rudderstackai.EndOptions{})
	return errors.Join(provider.Shutdown(context.Background()), client.Close())
}

func verify(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	counts := map[string]int{}
	traceID, interactionID := "", ""
	var response map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event struct {
			Event      string
			Properties map[string]any
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		counts[event.Event]++
		switch event.Event {
		case "ai_otel_span":
			traceID, _ = event.Properties["trace_id"].(string)
			interactionID, _ = event.Properties["interaction_id"].(string)
			if event.Properties["otlp"] == nil {
				return errors.New("missing OTLP payload")
			}
		case "ai_response_received":
			response = event.Properties
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if response == nil || traceID == "" || interactionID == "" || response["trace_id"] != traceID || response["interaction_id"] != interactionID || response["llm_call_count"] != float64(1) || response["total_tokens"] != float64(145) {
		return errors.New("response did not join the span or count its tokens")
	}
	if len(counts) != 3 || counts["ai_otel_span"] != 1 || counts["ai_user_prompt_created"] != 1 || counts["ai_response_received"] != 1 {
		return fmt.Errorf("unexpected events: %v", counts)
	}
	return nil
}
