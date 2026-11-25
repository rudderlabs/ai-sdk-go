package rudderstackai

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// client_name, client_title and client_version as sent (spec/fixtures/mcp-client-info-v0.1.json).
func TestMCPClientInfoSharedFixture(t *testing.T) {
	var fixture struct {
		MaxChars       int    `json:"max_chars"`
		VersionPattern string `json:"version_pattern"`
		Cases          []struct {
			Name     string  `json:"name"`
			Field    string  `json:"field"`
			Value    any     `json:"value"`
			Expected *string `json:"expected"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("spec/fixtures/mcp-client-info-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.MaxChars != maxClientChars || fixture.VersionPattern != versionPattern.String() {
		t.Fatalf("fixture max_chars %d, version_pattern %q; SDK %d, %q", fixture.MaxChars, fixture.VersionPattern, maxClientChars, versionPattern)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			check := clientText
			switch tc.Field {
			case "client_name", "client_title":
			case "client_version":
				check = clientVersion
			default:
				t.Fatalf("unknown field %q", tc.Field)
			}
			want := ""
			if tc.Expected != nil {
				want = *tc.Expected
			}
			if got := check(str(tc.Value)); got != want {
				t.Fatalf("%s(%q) = %q, want %q", tc.Field, tc.Value, got, want)
			}
		})
	}
}

// Clients resend clientInfo in _meta on every 2026-07-28 request: a repeat costs no masking.
func TestCleanClientRepeatsAreCheap(t *testing.T) {
	sent := mcpbridge.Client{Name: "agent for jane.doe@example.com", Title: "Claude Code", Version: "2.1.322"}
	first := cleanClient(&sent)
	if allocs := testing.AllocsPerRun(100, func() { cleanClient(&sent) }); allocs > 1 {
		t.Fatalf("a repeated client allocates %v times, want at most 1", allocs)
	}
	first.Name = "changed"
	if again := cleanClient(&sent); again.Name != "agent for [email]" || again.Version != "2.1.322" {
		t.Fatalf("repeat = %+v, want the checked client, unaffected by changes to an earlier result", *again)
	}
}
