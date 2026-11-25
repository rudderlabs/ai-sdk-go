package rudderstackai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Shared with the Node.js and Python SDKs: the same options, free text and an exception. Go's
// option types are strings, so the exception arrives as its text.
func TestInvalidOptionValueIsLoggedByTypeOnly(t *testing.T) {
	start := func(o InteractionOptions) *Interaction {
		o.UserID = "u"
		_, i := StartInteraction(context.Background(), o)
		return i
	}
	options := map[string]func(bad string){
		"invalid surface":    func(bad string) { start(InteractionOptions{Surface: Surface(bad)}).End(EndOptions{}) },
		"invalid confidence": func(bad string) { start(InteractionOptions{}).SetConfidence(Confidence(bad)) },
		"invalid status":     func(bad string) { start(InteractionOptions{}).End(EndOptions{Status: ResponseStatus(bad)}) },
		"invalid pause reason": func(bad string) {
			start(InteractionOptions{}).End(EndOptions{Status: StatusPaused, PauseReason: PauseReason(bad)})
			start(InteractionOptions{}).Pause(PauseOptions{Reason: PauseReason(bad)})
		},
		"invalid block reason": func(bad string) {
			start(InteractionOptions{}).End(EndOptions{Status: StatusBlocked, BlockReason: BlockReason(bad)})
		},
		// Its own client: a non-standard action_type is sent, outside the spec's conformance check.
		"non-standard action_type": func(bad string) {
			TrackAction(context.Background(), TrackActionOptions{ActionType: ActionType(bad), InteractionID: "i", ConversationID: "c", Analytics: &sink{}})
		},
	}
	for option, run := range options {
		t.Run(option, func(t *testing.T) {
			setupAI(t)
			logs := captureLogs(t)
			t.Setenv("RUDDERSTACK_AI_DEBUG", "1") // repeats log at debug level
			for _, bad := range []string{"my order 8812 never arrived", errors.New("card 4111 1111 1111 1111 declined").Error()} {
				run(bad)
			}
			out := logs.String()
			if !strings.Contains(out, option) || strings.Contains(out, "8812") || strings.Contains(out, "4111") {
				t.Fatalf("logs: %s", out)
			}
		})
	}
}
