package rudderstackai

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unsafe"

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

// Sessions and remembered clients keep checked labels: a cut label must not pin the raw value's memory.
func TestClientLabelsDoNotShareTheRawValue(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		check func(string) string
	}{
		{strings.Repeat("a", 4096), clientText},
		{"1.2.3" + strings.Repeat(" ", 4096), clientVersion},
	} {
		got := tc.check(tc.raw)
		start, at := uintptr(unsafe.Pointer(unsafe.StringData(tc.raw))), uintptr(unsafe.Pointer(unsafe.StringData(got)))
		if got == "" || (at >= start && at < start+uintptr(len(tc.raw))) {
			t.Fatalf("%.10q shares the raw value's memory", got)
		}
	}
}

// The memo keeps the raw clientInfo as its key: a substring (a User-Agent version, a callback result) must not pin
// the string it was cut from.
func TestCheckedClientMemoKeyKeepsNoRawMemory(t *testing.T) {
	buf := strings.Repeat("x", 1<<16) + "memo-key-agent"
	name := buf[1<<16:]
	cleanClient(&mcpbridge.Client{Name: name})
	start := uintptr(unsafe.Pointer(unsafe.StringData(buf)))
	checkedClients.Lock()
	defer checkedClients.Unlock()
	for key := range checkedClients.byRaw {
		at := uintptr(unsafe.Pointer(unsafe.StringData(key.Name)))
		if key.Name == "memo-key-agent" && at >= start && at < start+uintptr(len(buf)) {
			t.Fatal("the memo key shares the raw value's memory")
		}
	}
}

func TestCheckedClientMemoSkipsWithheldAndLongValues(t *testing.T) {
	size := func() int {
		checkedClients.Lock()
		defer checkedClients.Unlock()
		return len(checkedClients.byRaw)
	}
	checkedClients.Lock()
	clear(checkedClients.byRaw)
	checkedClients.Unlock()
	cleanClient(&mcpbridge.Client{Name: "memo-agent", Version: "v1.2.3"}) // version withheld
	cleanClient(&mcpbridge.Client{Name: strings.Repeat("a", maxCheckedClientBytes+1)})
	if n := size(); n != 0 {
		t.Fatalf("memo holds %d entries, want none", n)
	}
	cleanClient(&mcpbridge.Client{Name: "memo-agent", Version: "1.2.3"})
	if n := size(); n != 1 {
		t.Fatalf("memo holds %d entries, want 1", n)
	}
}
