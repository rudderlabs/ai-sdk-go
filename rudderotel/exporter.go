package rudderotel

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	analytics "github.com/rudderlabs/analytics-go/v4"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// Exporter sends each finished AI span as one RudderStack track event.
// It implements sdktrace.SpanExporter; it never closes the app's client.
type Exporter struct {
	options Options
	client  analytics.Client
	closed  atomic.Bool
}

var _ sdktrace.SpanExporter = (*Exporter)(nil)

// NewExporter uses options.Analytics, falling back to Configure(WithAnalytics(...)).
func NewExporter(options Options) (*Exporter, error) {
	client := options.Analytics
	if client == nil {
		client = hook.Tracing.Analytics()
	}
	if client == nil {
		return nil, errors.New("rudderotel: tracing needs a RudderStack client")
	}
	if options.MaxEventBytes < 0 {
		return nil, errors.New("rudderotel: MaxEventBytes must be positive")
	}
	hook.Tracing.RememberClient(client)
	return &Exporter{options: options, client: client}, nil
}

func safe(where string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			hook.Recovered(r, where)
		}
	}()
	fn()
}

// ExportSpans applies the filter and continues a batch after an individual enqueue fails.
func (e *Exporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if e.closed.Load() {
		return errors.New("rudderotel: exporter is shut down")
	}
	var failures []error
	for _, s := range spans {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		safe("tracing.export", func() {
			keep := IsAISpan(s)
			if e.options.Filter != nil {
				keep = e.options.Filter(s)
			} else if e.options.ExportAllSpans {
				keep = true
			}
			if !keep {
				return
			}
			m, err := SpanToTrack(s, e.options)
			if err != nil {
				hook.Report(rudderstackai.KindInternal, "tracing.map", err)
				return
			}
			if m == nil {
				return
			}
			// A transport panic is an export failure, like an Enqueue error.
			err = enqueue(e.client, *m)
			if err != nil {
				failures = append(failures, err)
			}
		})
	}
	return errors.Join(failures...)
}

func enqueue(c analytics.Client, m analytics.Track) (err error) {
	defer func() {
		if r := recover(); r != nil {
			hook.Recovered(r, "tracing.enqueue")
			err = fmt.Errorf("rudderotel: client enqueue panicked (%T)", r)
		}
	}()
	return c.Enqueue(m)
}

// ForceFlush waits for background classifications and flushes the configured RudderStack client.
func (e *Exporter) ForceFlush(ctx context.Context) error {
	ctx, cancel := flushContext(ctx)
	defer cancel()
	if !hook.Tracing.FlushClient(ctx, e.client) {
		return errors.New("rudderotel: RudderStack flush failed")
	}
	return nil
}

// Shutdown flushes and rejects new exports without closing the RudderStack client.
func (e *Exporter) Shutdown(ctx context.Context) error {
	if e.closed.Swap(true) {
		return nil
	}
	return e.ForceFlush(ctx)
}
