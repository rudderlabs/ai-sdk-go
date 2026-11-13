package rudderbedrock

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

func TestBedrockCancellationFinishesWithBlockedDecoder(t *testing.T) {
	rec := setup(t)
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, i := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "alice"})
	defer i.End(rudderstackai.EndOptions{})
	ctx, span := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	sc := &streamCall{ctx: ctx, call: &kit.Call{Span: span, Interaction: i, Provider: provider, Integration: integration, Model: model, StartedAt: time.Now()}}
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close(); _ = reader.Close() }()
	resp := &smithyhttp.Response{Response: &http.Response{Header: http.Header{}, Body: reader}}
	sc.start(resp)
	readDone := make(chan struct{})
	go func() { _, _ = resp.Body.Read(make([]byte, 1)); close(readDone) }()
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		rec.mu.Lock()
		n := 0
		for _, event := range rec.msgs {
			if event.Event == "ai_otel_span" {
				n++
			}
		}
		rec.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation watcher did not finish before Flush")
		}
		time.Sleep(time.Millisecond)
	}
	spans := rec.named(t, "ai_otel_span")
	if len(spans) != 1 || spans[0]["status_code"] != "error" {
		t.Fatalf("blocked decoder left pending span: %v", spans)
	}
	_ = writer.Close()
	<-readDone
	_ = resp.Body.Close()
	if len(rec.named(t, "ai_otel_span")) != 1 {
		t.Fatal("finished twice")
	}
}

func TestBedrockExtractionAndMissingStreamFinishSpan(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			rec := setup(t)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			var params any = &bedrockruntime.ConverseInput{ModelId: aws.String(model)}
			var result any = &bedrockruntime.ConverseOutput{Output: &types.ConverseOutputMemberMessage{Value: types.Message{Content: []types.ContentBlock{(*types.ContentBlockMemberText)(nil)}}}}
			if stream {
				params = &bedrockruntime.ConverseStreamInput{ModelId: aws.String(model)}
				result = &bedrockruntime.ConverseStreamOutput{}
			}
			tr := &tracker{cfg: &config{}}
			out, _, err := tr.HandleInitialize(context.Background(), middleware.InitializeInput{Parameters: params}, middleware.InitializeHandlerFunc(func(context.Context, middleware.InitializeInput) (middleware.InitializeOutput, middleware.Metadata, error) {
				return middleware.InitializeOutput{Result: result}, middleware.Metadata{}, nil
			}))
			if err != nil || out.Result != result {
				t.Fatal("provider result changed")
			}
			spans := rec.named(t, "ai_otel_span")
			if len(spans) != 1 || spans[0]["status_code"] != "error" {
				t.Fatalf("failed extraction left pending span: %v", spans)
			}
		})
	}
}

func TestBedrockLateUnreadStreamFinishesOnFlush(t *testing.T) {
	rec := setup(t)
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
	ctx, span := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	i.End(rudderstackai.EndOptions{})
	sc := &streamCall{ctx: ctx, call: &kit.Call{Span: span, Interaction: i, Provider: provider, Integration: integration, Model: model, StartedAt: time.Now()}}
	resp := &smithyhttp.Response{Response: &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}}
	sc.start(resp)
	rudderstackai.Flush(context.Background())
	spans := rec.named(t, "ai_otel_span")
	if len(spans) != 1 || spans[0]["status_code"] != "error" {
		t.Fatalf("late unread stream pending: %v", spans)
	}
	_ = resp.Body.Close()
	if len(rec.named(t, "ai_otel_span")) != 1 {
		t.Fatal("finished twice")
	}
}

func TestBedrockObserverSetupFailureFinishesSpan(t *testing.T) {
	rec := setup(t)
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
	defer i.End(rudderstackai.EndOptions{})
	ctx, span := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	sc := &streamCall{ctx: ctx, call: &kit.Call{Span: span, Interaction: i, Provider: provider, Integration: integration, Model: model, StartedAt: time.Now()}}
	original := hook.KeepsText
	hook.KeepsText = func(any) bool { panic("observer setup") }
	defer func() { hook.KeepsText = original }()
	body := io.NopCloser(strings.NewReader("provider bytes"))
	resp := &smithyhttp.Response{Response: &http.Response{Header: http.Header{}, Body: body}}
	safe(func() { sc.start(resp) })
	rec.mu.Lock()
	n := 0
	for _, event := range rec.msgs {
		if event.Event == "ai_otel_span" {
			n++
			if event.Properties["status_code"] != "error" {
				t.Error("setup span succeeded")
			}
		}
	}
	rec.mu.Unlock()
	if n != 1 {
		t.Fatalf("observer setup left %d finished spans", n)
	}
	if resp.Body != body {
		t.Fatal("setup failure changed body")
	}
}
