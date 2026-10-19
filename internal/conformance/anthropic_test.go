package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/rudderlabs/ai-sdk-go/rudderanthropic"
)

func init() {
	apis["anthropic.messages"] = func(ctx context.Context, r *run, s obj) error { return anthropicCall(ctx, r, s, false) }
	// The Go SDK has no stream helper: NewStreaming accumulated into the final message is the
	// equivalent of messages.stream() iterated, then finalMessage().
	apis["anthropic.messages.stream"] = func(ctx context.Context, r *run, s obj) error { return anthropicCall(ctx, r, s, true) }
}

func anthropicCall(ctx context.Context, r *run, s obj, helper bool) error {
	var tracking []rudderanthropic.Option
	if p := str(s["provider"]); p != "" {
		tracking = append(tracking, rudderanthropic.WithProvider(p))
	}
	opts := []option.RequestOption{
		option.WithBaseURL(llm.URL), option.WithAPIKey("sk-ant-test"), option.WithMaxRetries(0),
		rudderanthropic.WithTracking(tracking...),
	}
	if s["wrap_twice"] == true {
		opts = append(opts, rudderanthropic.WithTracking(tracking...))
	}
	if ms := num(s["timeout_ms"]); ms > 0 {
		opts = append(opts, option.WithRequestTimeout(time.Duration(ms)*time.Millisecond))
	}
	client := anthropic.NewClient(opts...)

	params := obj0(s["params"])
	if helper {
		params["stream"] = true
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	body := option.WithRequestBody("application/json", raw)
	if ms := num(s["abort_after_ms"]); ms > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		time.AfterFunc(time.Duration(ms)*time.Millisecond, cancel)
	}
	if params["stream"] != true {
		_, err := client.Messages.New(ctx, anthropic.MessageNewParams{}, body)
		return err
	}

	st := client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{}, body)
	defer func() { _ = st.Close() }()
	var final anthropic.Message
	first := true
	for st.Next() {
		if helper {
			if err := final.Accumulate(st.Current()); err != nil {
				return err
			}
		}
		if !first {
			continue
		}
		first = false
		if s["consume"] == "first_chunk" {
			return st.Close()
		}
		if during := objs0(s["during"]); len(during) > 0 {
			if err := r.steps(ctx, during); err != nil {
				return err
			}
		}
	}
	err = st.Err()
	if err != nil && s["expect_error"] != true && errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
