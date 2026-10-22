package rudderbedrock

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

func onErrors(t *testing.T) func() []rudderstackai.ErrorInfo {
	t.Helper()
	var mu sync.Mutex
	var seen []rudderstackai.ErrorInfo
	if err := rudderstackai.Configure(rudderstackai.WithOnError(func(_ error, info rudderstackai.ErrorInfo) {
		mu.Lock()
		seen = append(seen, info)
		mu.Unlock()
	})); err != nil {
		t.Fatal(err)
	}
	return func() []rudderstackai.ErrorInfo {
		mu.Lock()
		defer mu.Unlock()
		return append([]rudderstackai.ErrorInfo(nil), seen...)
	}
}

type failingProtocol struct{ smithyhttp.ClientProtocol }

var errDecode = errors.New("decode failed")

func (failingProtocol) DeserializeEventMessage(*smithy.Schema, *smithy.TypeRegistry, io.Reader) (smithy.Deserializable, error) {
	return nil, errDecode
}

// The AWS SDK decodes stream events on its own goroutine: a panic there would end the process.
func TestObservePanicOnDecodeGoroutineIsContained(t *testing.T) {
	setup(t)
	seen := onErrors(t)
	_, in := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	defer in.End(rudderstackai.EndOptions{})
	// A nil context makes observe panic while it reads the decode error.
	sc := &streamCall{call: &kit.Call{Interaction: in}, obs: kit.NewObserver(&kit.Call{Interaction: in})}
	body := &trackedBody{ReadCloser: io.NopCloser(strings.NewReader("")), s: sc}
	done := make(chan error)
	go func() {
		_, err := (&eventProtocol{ClientProtocol: failingProtocol{}}).DeserializeEventMessage(nil, nil, body)
		done <- err
	}()
	if err := <-done; err != errDecode {
		t.Fatalf("err = %v; want the decoder's own", err)
	}
	if s := seen(); len(s) != 1 || s[0].Kind != rudderstackai.KindInternal {
		t.Fatalf("onError saw %+v", s)
	}
}

func TestHandleInitializePanicRunsTheCallUntracked(t *testing.T) {
	setup(t)
	seen := onErrors(t)
	calls := 0
	next := middleware.InitializeHandlerFunc(func(context.Context, middleware.InitializeInput) (middleware.InitializeOutput, middleware.Metadata, error) {
		calls++
		return middleware.InitializeOutput{Result: "ok"}, middleware.Metadata{}, nil
	})
	err := interaction(t, func(ctx context.Context) error {
		// No config: tracking panics before the call goes out.
		out, _, err := (&tracker{}).HandleInitialize(ctx, middleware.InitializeInput{Parameters: &bedrockruntime.ConverseInput{}}, next)
		if out.Result != "ok" {
			t.Fatalf("result = %v", out.Result)
		}
		return err
	})
	if err != nil || calls != 1 {
		t.Fatalf("err = %v, calls = %d; want the call made once", err, calls)
	}
	if s := seen(); len(s) != 1 || s[0].Where != "rudderbedrock.HandleInitialize" {
		t.Fatalf("onError saw %+v", s)
	}
}

func TestHandleInitializeKeepsTheCallersError(t *testing.T) {
	setup(t)
	want := errors.New("throttled")
	next := middleware.InitializeHandlerFunc(func(context.Context, middleware.InitializeInput) (middleware.InitializeOutput, middleware.Metadata, error) {
		return middleware.InitializeOutput{}, middleware.Metadata{}, want
	})
	err := interaction(t, func(ctx context.Context) error {
		_, _, err := (&tracker{}).HandleInitialize(ctx, middleware.InitializeInput{Parameters: &bedrockruntime.ConverseInput{}}, next)
		return err
	})
	if err != want {
		t.Fatalf("err = %v; want the call's own error", err)
	}
}
