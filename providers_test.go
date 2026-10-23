package rudderstackai

import "testing"

// Cases beyond spec/fixtures/providers-v0.1.json: a Bedrock ARN, and a Vertex ID as the served model.
func TestRoutedModelCloudAnthropicIDs(t *testing.T) {
	cases := []struct{ requested, served, want string }{
		{"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-haiku-4-5-20251001-v1:0", "claude-haiku-4-5-20251001", ""},
		{"claude-haiku-4-5", "claude-haiku-4-5@20251001", ""},
		{"claude-haiku-4-5", "claude-sonnet-4-5@20250929", "claude-haiku-4-5"},
	}
	for _, c := range cases {
		if got := routedModel(c.requested, c.served); got != c.want {
			t.Errorf("routedModel(%q, %q) = %q, want %q", c.requested, c.served, got, c.want)
		}
	}
}
