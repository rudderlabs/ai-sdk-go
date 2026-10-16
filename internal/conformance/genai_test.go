package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/genai"

	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

func init() {
	apis["google.generate"] = genaiDriver(genai.BackendGeminiAPI, false)
	apis["google.stream"] = genaiDriver(genai.BackendGeminiAPI, true)
	apis["google.vertex.generate"] = genaiDriver(genai.BackendVertexAI, false)
	apis["google.vertex.stream"] = genaiDriver(genai.BackendVertexAI, true)
}

func genaiDriver(backend genai.Backend, stream bool) apiDriver {
	return func(ctx context.Context, r *run, step obj) error {
		params := obj0(step["params"])
		var opts []ruddergenai.Option
		if p := str(step["provider"]); p != "" {
			opts = append(opts, ruddergenai.WithProvider(p))
		}
		hc := ruddergenai.HTTPClient(nil, opts...)
		if step["wrap_twice"] == true {
			hc.Transport = ruddergenai.Transport(hc.Transport, opts...)
		}
		// Vertex AI in express mode: an API key and no project.
		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey: "sk-google-test", Backend: backend,
			HTTPOptions: genai.HTTPOptions{BaseURL: llm.URL}, HTTPClient: hc,
		})
		if err != nil {
			return err
		}
		var config *genai.GenerateContentConfig
		if c, ok := params["config"]; ok {
			config = &genai.GenerateContentConfig{}
			if err := remarshal(c, config); err != nil {
				return err
			}
		}
		contents, err := contentsOf(params["contents"])
		if err != nil {
			return err
		}
		if ms := num(step["timeout_ms"]); ms > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
			defer cancel()
		}
		if ms := num(step["abort_after_ms"]); ms > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			time.AfterFunc(time.Duration(ms)*time.Millisecond, cancel)
		}
		model := str(params["model"])
		if !stream {
			_, err := client.Models.GenerateContent(ctx, model, contents, config)
			return err
		}
		first := true
		for _, err := range client.Models.GenerateContentStream(ctx, model, contents, config) {
			if err != nil {
				if errors.Is(err, context.Canceled) && step["expect_error"] != true {
					return nil
				}
				return err
			}
			if first {
				first = false
				if step["consume"] == "first_chunk" {
					break
				}
				if during := objs0(step["during"]); len(during) > 0 {
					if err := r.steps(ctx, during); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
}

// contentsOf decodes the JS SDK's ContentListUnion: a string, a part, a content, or a list of
// those (parts without a content become one user turn).
func contentsOf(v any) ([]*genai.Content, error) {
	list, ok := v.([]any)
	if !ok {
		list = []any{v}
	}
	var out []*genai.Content
	var loose []*genai.Part
	for _, x := range list {
		if s, ok := x.(string); ok {
			loose = append(loose, genai.NewPartFromText(s))
			continue
		}
		m := obj0(x)
		if _, isContent := m["parts"]; isContent {
			c := &genai.Content{}
			if err := remarshal(m, c); err != nil {
				return nil, err
			}
			out = append(out, c)
			continue
		}
		p := &genai.Part{}
		if err := remarshal(m, p); err != nil {
			return nil, err
		}
		loose = append(loose, p)
	}
	if len(loose) > 0 {
		out = append(out, genai.NewContentFromParts(loose, genai.RoleUser))
	}
	return out, nil
}

func remarshal(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
