package rudderotel

import (
	"errors"
	"reflect"
	"sync"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var registration sync.Mutex

// RegisterTracing creates and globally registers a provider. It refuses to replace an existing
// provider. Provider options can supply resources, samplers, and additional processors.
// For an app-owned provider, use NewSpanProcessor and sdktrace.WithSpanProcessor instead.
func RegisterTracing(options Options, providerOptions ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, error) {
	registration.Lock()
	defer registration.Unlock()
	// OTel Go exposes the default delegating provider only through the public interface.
	// Check its concrete type, not a tracer's recording state (noop is an explicit provider too).
	t := reflect.TypeOf(otel.GetTracerProvider())
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().PkgPath() != "go.opentelemetry.io/otel/internal/global" || t.Elem().Name() != "tracerProvider" {
		return nil, errors.New("rudderotel: a global OpenTelemetry tracer provider is already registered; add NewSpanProcessor to it")
	}
	processor, err := NewSpanProcessor(options)
	if err != nil {
		return nil, err
	}
	providerOptions = append(providerOptions, sdktrace.WithSpanProcessor(processor))
	provider := sdktrace.NewTracerProvider(providerOptions...)
	otel.SetTracerProvider(provider)
	return provider, nil
}
