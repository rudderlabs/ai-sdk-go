package ruddermcpgo

import (
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestSessionsEvictOldest(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	if err := rudderstackai.Configure(rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 2})); err != nil {
		t.Fatal(err)
	}
	var s sessions
	for _, k := range []string{"a", "b", "c"} {
		s.store(k, sessionInfo{version: k})
	}
	if _, ok := s.load("a"); ok {
		t.Fatal("the oldest session was kept past the cap")
	}
	if info, ok := s.load("c"); !ok || info.version != "c" || len(s.items) != 2 {
		t.Fatalf("sessions = %d", len(s.items))
	}
}
