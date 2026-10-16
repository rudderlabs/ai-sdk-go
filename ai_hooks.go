package rudderstackai

import (
	"context"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func init() {
	hook.ForCall = func(ctx context.Context, integration string, client analytics.Client) (any, bool) {
		i, ephemeral := forCall(ctx, integration, client)
		if i == nil {
			return nil, false
		}
		return i, ephemeral
	}
	hook.KeepMessages = func(i any, messages any) { i.(*Interaction).keepMessages(messages) }
	hook.KeepsText = func(i any) bool { return i.(*Interaction).keepsText() }
	hook.FirstToken = func(i any, at time.Time) { i.(*Interaction).firstTokenAt(at) }
	hook.HintOutcome = func(i any, status, errorType string) {
		i.(*Interaction).hintOutcome(ResponseStatus(status), errorType)
	}
	hook.OnBeforeEnd = func(i any, fn func()) { i.(*Interaction).onBeforeEnd(fn) }
	hook.StatusForErrorType = func(t string) string { return string(statusForErrorType(t)) }
	hook.RegisterErrorClassifier = registerErrorClassifier
	hook.Untracked = func(ctx context.Context) bool { return ctx != nil && isUntracked(ctx) }
	hook.Reset = resetForTest
}
