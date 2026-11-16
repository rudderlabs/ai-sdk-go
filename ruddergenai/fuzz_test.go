package ruddergenai_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"testing"

	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

// FuzzUsage: whatever a Gemini response carries as usageMetadata, the app gets the Gen AI SDK's
// result unchanged and the SDK sends one conforming answer. The Go twin of the Node.js and Python
// usage property tests; go test -fuzz FuzzUsage explores beyond the seeds.

type static []byte

func (s static) RoundTrip(req *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	return &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(s)),
	}, nil
}

// plausible reports whether every number in the JSON is below 1e15 in magnitude: no provider
// reports token counts beyond that, and sums of larger ones may overflow.
func plausible(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	for {
		tok, err := d.Token()
		if err != nil {
			return err == io.EOF
		}
		if n, ok := tok.(json.Number); ok {
			if f, err := n.Float64(); err != nil || math.Abs(f) >= 1e15 {
				return false
			}
		}
	}
}

func FuzzUsage(f *testing.F) {
	for _, s := range []string{
		`{"promptTokenCount":12,"candidatesTokenCount":5,"totalTokenCount":17}`, `null`, `"x"`, `[]`, `true`,
		`{"promptTokenCount":-1}`, `{"promptTokenCount":1.5}`, `{"thoughtsTokenCount":"x","cachedContentTokenCount":4}`,
		`{"toolUsePromptTokenCount":3,"totalTokenCount":0}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, usage []byte) {
		if !json.Valid(usage) || !plausible(usage) {
			return
		}
		rt := static(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}],` +
			`"modelVersion":"gemini-2.5-flash","responseId":"resp-f","usageMetadata":` + string(usage) + `}`)
		call := func(ctx context.Context, hc *http.Client) string {
			c, err := genai.NewClient(ctx, &genai.ClientConfig{
				APIKey: "fuzz", Backend: genai.BackendGeminiAPI, HTTPOptions: genai.HTTPOptions{BaseURL: "http://fuzz.invalid/"}, HTTPClient: hc,
			})
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.Models.GenerateContent(ctx, "gemini-2.5-flash", genai.Text("hi"), nil)
			if err != nil {
				return "error: " + err.Error()
			}
			r.SDKHTTPResponse = nil
			out, _ := json.Marshal(r)
			return string(out)
		}
		raw := call(context.Background(), &http.Client{Transport: rt})

		hook.Reset()
		rec := &recorder{}
		if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec), rudderstackai.WithLLMCallEvents(true),
			rudderstackai.WithCaptureContent(false), rudderstackai.WithEmitSpans(false)); err != nil {
			t.Fatal(err)
		}
		var got string
		_ = rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", ConversationID: "c"},
			func(ctx context.Context, _ *rudderstackai.Interaction) error {
				got = call(ctx, ruddergenai.HTTPClient(&http.Client{Transport: rt}))
				return nil
			})
		if got != raw {
			t.Fatalf("the app saw %q, without the SDK %q", got, raw)
		}
		answer := rec.response(t) // schema-checks every event; exactly one answer
		for _, k := range []string{"total_input_tokens", "total_output_tokens", "total_tokens", "total_cached_input_tokens", "total_cache_write_tokens"} {
			if v, ok := answer[k]; ok {
				if n, isNum := v.(float64); !isNum || n < 0 || n != math.Trunc(n) {
					t.Errorf("%s = %v", k, v)
				}
			}
		}
	})
}
