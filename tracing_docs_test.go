package rudderstackai

import (
	"os"
	"strings"
	"testing"
)

func TestTracingDocsEventBudgetAndPendingSpanGuarantee(t *testing.T) {
	b, err := os.ReadFile("docs/tracing.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, phrase := range []string{"event the SDK builds", "analytics client may add", "Flush", "every still-pending wrapper span", "blocked"} {
		if !strings.Contains(text, phrase) {
			t.Errorf("tracing docs missing %q", phrase)
		}
	}
	for _, phrase := range []string{"measures the whole Go analytics message", "remains pending."} {
		if strings.Contains(text, phrase) {
			t.Errorf("tracing docs retain incorrect claim %q", phrase)
		}
	}
}
