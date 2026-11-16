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
	hook.RecordToolResult = func(i any, id string, result any, hasResult bool, name ...string) {
		i.(*Interaction).recordToolResult(id, result.(ToolResult), hasResult, name...)
	}
	hook.HasStructuredTools = func(i any) bool { return i.(*Interaction).hasStructuredTools() }
	hook.KeepMessages = func(i any, messages any) { i.(*Interaction).keepMessages(messages) }
	hook.KeepsText = func(i any) bool { return i.(*Interaction).keepsText() }
	hook.CapturesCallContent = func(i any) bool { return i.(*Interaction).capturesCallContent() }
	hook.FirstToken = func(i any, at time.Time) { i.(*Interaction).firstTokenAt(at) }
	hook.HintOutcome = func(i any, status, errorType, pauseReason string) {
		i.(*Interaction).hintOutcome(ResponseStatus(status), errorType, PauseReason(pauseReason))
	}
	hook.StartInteraction = func(ctx context.Context, opts any, integration string, client analytics.Client, startedAt time.Time) (context.Context, any) {
		o := opts.(InteractionOptions)
		o.adapterAnalytics, o.startedAt = client, startedAt
		ctx, i := StartInteraction(ctx, o)
		i.adopt(integration, nil)
		return ctx, i
	}
	hook.Adopt = func(i any, integration string, client analytics.Client) { i.(*Interaction).adopt(integration, client) }
	hook.PrimaryModel = func(i any, model, provider string) { i.(*Interaction).setPrimaryModel(model, provider) }
	hook.KeepAnswer = func(i any, text string) { i.(*Interaction).keepAnswer(text) }
	hook.Backdate = func(i any, startedAt time.Time) { i.(*Interaction).backdate(startedAt) }
	hook.WarnOnce = func(key, message string) { warnOnce(key, message, 0) }
	hook.ContextConversation = func(ctx context.Context) string {
		if v := valuesFrom(ctx); v != nil {
			return v.conversationID
		}
		return ""
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
	hook.MaxCallsPerInteraction = func() int { return cfg().limits.MaxCallsPerInteraction }
	hook.Reset = resetForTest
}
