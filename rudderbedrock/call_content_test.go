package rudderbedrock

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestInputMessages(t *testing.T) {
	system := []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: "Be brief."}}
	messages := []types.Message{
		{Role: types.ConversationRoleUser, Content: []types.ContentBlock{
			&types.ContentBlockMemberText{Value: "Hi"},
			&types.ContentBlockMemberImage{Value: types.ImageBlock{Format: types.ImageFormatPng, Source: &types.ImageSourceMemberBytes{Value: []byte{1}}}},
			&types.ContentBlockMemberDocument{Value: types.DocumentBlock{Source: &types.DocumentSourceMemberS3Location{Value: types.S3Location{Uri: aws.String("s3://b/f.pdf")}}}},
		}},
		{Role: types.ConversationRoleAssistant, Content: []types.ContentBlock{
			&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{ToolUseId: aws.String("t1"), Name: aws.String("weather")}},
		}},
		{Role: types.ConversationRoleUser, Content: []types.ContentBlock{
			&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{ToolUseId: aws.String("t1"), Content: []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: "sunny"}}}},
			&types.ContentBlockMemberText{Value: "thanks"},
		}},
	}
	want := []rudderstackai.GenAIMessage{
		{Role: "system", Parts: []rudderstackai.GenAIPart{{"type": "text", "content": "Be brief."}}},
		{Role: "user", Parts: []rudderstackai.GenAIPart{
			{"type": "text", "content": "Hi"},
			{"type": "blob", "modality": "image", "mime_type": "image/png"},
			{"type": "uri", "modality": "document", "uri": "s3://b/f.pdf"},
		}},
		{Role: "assistant", Parts: []rudderstackai.GenAIPart{{"type": "tool_call", "id": "t1", "name": "weather"}}},
		{Role: "tool", Parts: []rudderstackai.GenAIPart{{"type": "tool_call_response", "id": "t1", "response": "sunny"}}},
		{Role: "user", Parts: []rudderstackai.GenAIPart{{"type": "text", "content": "thanks"}}},
	}
	if got := inputMessages(system, messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestInputMessagesOtherBlock(t *testing.T) {
	got := inputMessages(nil, []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberCachePoint{}}}})
	want := []rudderstackai.GenAIMessage{{Role: "user", Parts: []rudderstackai.GenAIPart{{"type": "cachePoint"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}
