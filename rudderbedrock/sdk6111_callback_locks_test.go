package rudderbedrock

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

func TestSDK6111StreamCreditsReenter(t *testing.T) {
	for _, path := range []string{"end", "close", "eof"} {
		for _, threaded := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/same", true: "/goroutine"}[threaded], func(t *testing.T) {
				setup(t)
				_, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
				s := &streamCall{ctx: context.Background(), call: &kit.Call{Interaction: i}, obs: &kit.Observer{Interaction: i, Provider: "aws.bedrock", Model: "anthropic.claude-sonnet-4-6", InputTokens: rudderstackai.Ptr(10), Done: true}}
				progress := make(chan bool, 1)
				err := rudderstackai.Configure(rudderstackai.WithCredits(func(rudderstackai.CreditCall) (*float64, error) {
					if threaded {
						done := make(chan struct{})
						go func() { defer close(done); s.end(kit.Completed, "") }()
						select {
						case <-done:
							progress <- true
						case <-time.After(time.Second):
							progress <- false
						}
					} else {
						s.end(kit.Completed, "")
						progress <- true
					}
					return rudderstackai.Ptr(1.0), nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					switch path {
					case "end":
						s.end(kit.Completed, "")
					case "close":
						_ = (&trackedBody{ReadCloser: io.NopCloser(strings.NewReader("")), s: s}).Close()
					case "eof":
						s.observe(nil, io.EOF)
					}
				}()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("stream callback deadlocked")
				}
				select {
				case ok := <-progress:
					if !ok {
						t.Fatal("callback blocked")
					}
				case <-time.After(time.Second):
					t.Fatal("missing credits callback")
				}
				i.End(rudderstackai.EndOptions{})
			})
		}
	}
}

func TestSDK6111StreamErrorNoticeReenter(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "goroutine"}[threaded], func(t *testing.T) {
			setup(t)
			_, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			s := &streamCall{ctx: context.Background(), call: &kit.Call{Interaction: i}, obs: &kit.Observer{Interaction: i}}
			progress := make(chan bool, 1)
			if err := rudderstackai.Configure(rudderstackai.WithOnError(func(error, rudderstackai.ErrorInfo) {
				if threaded {
					nested := make(chan struct{})
					go func() { defer close(nested); s.end(kit.Completed, "") }()
					select {
					case <-nested:
						progress <- true
					case <-time.After(time.Second):
						progress <- false
					}
				} else {
					s.end(kit.Completed, "")
					progress <- true
				}
			})); err != nil {
				t.Fatal(err)
			}
			s.pending = append(s.pending, (*types.MessageStopEvent)(nil))
			done := make(chan struct{})
			go func() { defer close(done); s.observe(nil, io.EOF) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("stream notice deadlocked")
			}
			select {
			case ok := <-progress:
				if !ok {
					t.Fatal("callback blocked")
				}
			case <-time.After(time.Second):
				t.Fatal("missing error callback")
			}
		})
	}
}
