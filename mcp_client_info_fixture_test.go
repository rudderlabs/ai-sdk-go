package rudderstackai

import (
	"encoding/json"
	"os"
	"testing"
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
