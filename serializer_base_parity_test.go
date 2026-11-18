package rudderstackai

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSerializerBaseParityGuards(t *testing.T) {
	path := os.Getenv("SDK6948_FIXTURE_FILE")
	if path == "" {
		path = "spec/fixtures/serializer-capture-v0.1.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Guards []struct {
			ID string `json:"id"`
		} `json:"base_parity_guards"`
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	guards := map[string]func(*testing.T){
		"cancelled-twins-credits":    TestSDK6111R1CreditsFollowRetainedStatusAndTwinReplacement,
		"same-source-repeat-flags":   TestSDK6942RepeatFlagsUpdateHeldEvents,
		"dated-twin-amounts":         TestTwinAmountsFollowKeptUsage,
		"legacy-pending-credits":     TestSDK6111R2LegacyFailedPendingCallsKeepExplicitCredits,
		"restored-fractional-timing": TestSDK6111R8RestoredFractionalTimingRoundsHalfUp,
		"app-error-classification":   TestAuditAppErrorPreservesProviderOutcome,
	}
	for _, guard := range fixture.Guards {
		t.Run(guard.ID, func(t *testing.T) {
			if run, ok := guards[guard.ID]; ok {
				run(t)
				return
			}
			switch guard.ID {
			case "claude-cache-write-tokens", "claude-subagent-model":
				if _, err := os.Stat("rudderclaude"); !os.IsNotExist(err) {
					t.Fatal("Go Claude guard needs an adapter driver")
				}
				t.Log("base and head have no Claude adapter")
			default:
				t.Fatal("unknown base parity guard", guard.ID)
			}
		})
	}
}
