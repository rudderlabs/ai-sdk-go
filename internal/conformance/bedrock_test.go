package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/rudderbedrock"
)

func init() {
	apis["bedrock.converse"] = func(ctx context.Context, r *run, step obj) error { return bedrockCall(ctx, r, step, false) }
	apis["bedrock.converse_stream"] = func(ctx context.Context, r *run, step obj) error { return bedrockCall(ctx, r, step, true) }
}

// bedrockClient is a real Bedrock Runtime client on the mock server, retries off.
func bedrockClient(url string, step obj) *bedrockruntime.Client {
	opts := []func(*bedrockruntime.Options){rudderbedrock.WithTracking()}
	if step["wrap_twice"] == true {
		opts = append(opts, rudderbedrock.WithTracking())
	}
	return bedrockruntime.New(bedrockruntime.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(url),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	}, opts...)
}

func bedrockCall(ctx context.Context, r *run, step obj, stream bool) error {
	raw, _ := json.Marshal(step)
	var reply testkit.BedrockReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return err
	}
	route := "bedrock.converse"
	if stream {
		route = "bedrock.converse_stream"
	}
	r.llm.ScriptBedrock(route, reply)
	in, err := converseInput(obj0(step["params"]))
	if err != nil {
		return err
	}
	if ms, ok := step["timeout_ms"].(float64); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
		defer cancel()
	}
	aborted := false
	if ms, ok := step["abort_after_ms"].(float64); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		t := time.AfterFunc(time.Duration(ms)*time.Millisecond, func() { aborted = true; cancel() })
		defer t.Stop()
	}
	client := bedrockClient(r.llm.URL, step)
	if !stream {
		_, err := client.Converse(ctx, in)
		return err
	}
	out, err := client.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId: in.ModelId, Messages: in.Messages, System: in.System, ToolConfig: in.ToolConfig, InferenceConfig: in.InferenceConfig,
	})
	if err != nil {
		return err
	}
	es := out.GetStream()
	defer es.Close()
	first := true
	for range es.Events() {
		if !first {
			continue
		}
		first = false
		if step["consume"] == "first_chunk" {
			return es.Close()
		}
		if during, ok := step["during"].([]any); ok {
			if err := r.steps(ctx, objs(during)); err != nil {
				return err
			}
		}
	}
	err = es.Err()
	if err != nil && aborted && step["expect_error"] != true && errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return err
}

// converseInput builds the typed request from a fixture's JSON Converse params (the shapes the
// fixture uses: text, toolUse and toolResult blocks, system, toolConfig, inferenceConfig).
func converseInput(p obj) (*bedrockruntime.ConverseInput, error) {
	in := &bedrockruntime.ConverseInput{ModelId: aws.String(str(p["modelId"]))}
	for _, m := range objs0(p["messages"]) {
		msg := types.Message{Role: types.ConversationRole(str(m["role"]))}
		for _, b := range objs0(m["content"]) {
			block, err := contentBlock(b)
			if err != nil {
				return nil, err
			}
			msg.Content = append(msg.Content, block)
		}
		in.Messages = append(in.Messages, msg)
	}
	for _, s := range objs0(p["system"]) {
		in.System = append(in.System, &types.SystemContentBlockMemberText{Value: str(s["text"])})
	}
	if tc := obj0(p["toolConfig"]); tc != nil {
		in.ToolConfig = &types.ToolConfiguration{}
		for _, t := range objs0(tc["tools"]) {
			spec := obj0(t["toolSpec"])
			ts := types.ToolSpecification{Name: aws.String(str(spec["name"]))}
			if d, ok := spec["description"].(string); ok {
				ts.Description = aws.String(d)
			}
			if schema := obj0(spec["inputSchema"]); schema != nil {
				ts.InputSchema = &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema["json"])}
			}
			in.ToolConfig.Tools = append(in.ToolConfig.Tools, &types.ToolMemberToolSpec{Value: ts})
		}
	}
	if ic := obj0(p["inferenceConfig"]); ic != nil {
		in.InferenceConfig = &types.InferenceConfiguration{StopSequences: strs(ic["stopSequences"])}
		if v, ok := ic["maxTokens"].(float64); ok {
			in.InferenceConfig.MaxTokens = aws.Int32(int32(v))
		}
		if v, ok := ic["temperature"].(float64); ok {
			in.InferenceConfig.Temperature = aws.Float32(float32(v))
		}
		if v, ok := ic["topP"].(float64); ok {
			in.InferenceConfig.TopP = aws.Float32(float32(v))
		}
	}
	return in, nil
}

func contentBlock(b obj) (types.ContentBlock, error) {
	switch {
	case b["text"] != nil:
		return &types.ContentBlockMemberText{Value: str(b["text"])}, nil
	case b["toolUse"] != nil:
		u := obj0(b["toolUse"])
		return &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
			ToolUseId: aws.String(str(u["toolUseId"])), Name: aws.String(str(u["name"])), Input: document.NewLazyDocument(u["input"]),
		}}, nil
	case b["toolResult"] != nil:
		res := obj0(b["toolResult"])
		tr := types.ToolResultBlock{ToolUseId: aws.String(str(res["toolUseId"])), Status: types.ToolResultStatus(str(res["status"]))}
		for _, c := range objs0(res["content"]) {
			if c["json"] != nil {
				tr.Content = append(tr.Content, &types.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument(c["json"])})
			} else {
				tr.Content = append(tr.Content, &types.ToolResultContentBlockMemberText{Value: str(c["text"])})
			}
		}
		return &types.ContentBlockMemberToolResult{Value: tr}, nil
	}
	return nil, fmt.Errorf("bedrock: unsupported content block %v", b)
}
