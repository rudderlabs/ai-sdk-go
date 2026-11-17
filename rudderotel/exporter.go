package rudderotel

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	analytics "github.com/rudderlabs/analytics-go/v4"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/capture"
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
	options.ContentCapture = capture.Clone(options.ContentCapture) // read at export: not the caller's values
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
			// User callbacks get the app's span; the IDs RudderStack exports under are resolved
			// after the filter, right before mapping.
			app := s
			d, delivered := s.(*deliveredSpan)
			if delivered {
				app = d.ReadOnlySpan
			}
			keep := IsAISpan(app)
			if e.options.Filter != nil {
				keep = filterSpan(e.options.Filter, app)
			} else if e.options.ExportAllSpans {
				keep = true
			}
			if !keep {
				return
			}
			if delivered {
				d.resolve()
			}
			sent, err := e.send(app, s)
			if err != nil {
				failures = append(failures, err)
			} else if sent && delivered {
				sentRaw(d)
			}
		})
	}
	return errors.Join(failures...)
}

// filterSpan runs the app's Filter; a panic rejects the span and is reported as user_callback.
func filterSpan(filter func(sdktrace.ReadOnlySpan) bool, s sdktrace.ReadOnlySpan) (keep bool) {
	defer func() {
		if r := recover(); r != nil {
			keep = false
			hook.Report(rudderstackai.KindUserCallback, "tracing.filter", fmt.Errorf("rudderotel: Filter panicked (%T)", r))
		}
	}()
	return filter(s)
}

// send maps one span, delivered as RudderStack exports it with app's callbacks, and enqueues it,
// reporting whether it was enqueued; a transport panic is an export failure, like an Enqueue
// error.
func (e *Exporter) send(app, delivered sdktrace.ReadOnlySpan) (bool, error) {
	m, err := spanToTrack(app, delivered, e.options)
	if err != nil {
		hook.Report(rudderstackai.KindInternal, "tracing.map", err)
		return false, nil
	}
	if m == nil {
		return false, nil
	}
	if err := enqueue(e.client, *m); err != nil {
		return false, err
	}
	return true, nil
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
