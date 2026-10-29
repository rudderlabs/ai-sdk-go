package rudderstackai

import (
	"context"
	"strings"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func init() {
	hook.ForCall = func(ctx context.Context, integration string, client analytics.Client) (interaction any, ephemeral bool) {
		safely(func() {
			if i, e := forCall(ctx, integration, client); i != nil {
				interaction, ephemeral = i, e
			}
		})
		return interaction, ephemeral
	}
	hook.RecordToolResult = func(i any, id string, result any, hasResult bool) {
		i.(*Interaction).recordToolResult(id, result.(ToolResult), hasResult)
	}
	hook.KeepMessages = func(i any, messages any) { i.(*Interaction).keepMessages(messages) }
	hook.KeepsText = func(i any) bool { return i.(*Interaction).keepsText() }
	hook.FirstToken = func(i any, at time.Time) { i.(*Interaction).firstTokenAt(at) }
	hook.HintOutcome = func(i any, status, errorType string) {
		i.(*Interaction).hintOutcome(ResponseStatus(status), errorType)
	}
	hook.OnBeforeEnd = func(i any, fn func()) { i.(*Interaction).onBeforeEnd(fn) }
	hook.StatusForErrorType = func(t string) string { return string(statusForErrorType(t)) }
	hook.NoteCallError = func(i any, err error, errorType string) { i.(*Interaction).noteCallError(err, errorType) }
	hook.RegisterErrorClassifier = registerErrorClassifier
	hook.Untracked = func(ctx context.Context) bool { return ctx != nil && isUntracked(ctx) }
	hook.Recovered = func(r any, where string) {
		if !strings.Contains(where, ".") {
			where = callerOf(where)
		}
		report(KindInternal, where, recovered(r))
	}
	hook.Report = report
	hook.MaxMapEntries = func() int { return cfg().limits.MaxMapEntries }
	hook.Reset = resetForTest
}
