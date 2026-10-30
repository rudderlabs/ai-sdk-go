package rudderstackai

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// Evaluator kinds: who or what gave a score.
const (
	EvaluatorModel = "model"
	EvaluatorCode  = "code"
	EvaluatorHuman = "human"
)

const explanationChars = 280

// Score is one score of an interaction, sent as ai_interaction_scored. It needs ScoreValue or
// ScoreLabel.
type Score struct {
	// EvaluationName is what was scored, for example "groundedness".
	EvaluationName string
	ScoreValue     *float64
	// ScoreLabel is a label, for example "pass".
	ScoreLabel string
	// Evaluator is EvaluatorModel (an LLM judge), EvaluatorCode or EvaluatorHuman.
	Evaluator string
	// Sampled: the interaction was picked by sampling rather than scored every time.
	Sampled *bool
	// Explanation is why the score was given. Sent only with content capture on, masked and
	// capped at 280 characters.
	Explanation string
}

// EvaluationTurn is what an evaluator sees of one successful interaction.
type EvaluationTurn struct {
	InteractionID  string
	ConversationID string
	Prompt         string
	Answer         string
	ToolNames      []string
	History        []TurnMessage
}

// EvaluateFunc scores one successful interaction after its answer is out. Its context is never
// tracked, so its own model calls send nothing; it times out after 5 seconds.
type EvaluateFunc func(ctx context.Context, turn EvaluationTurn) ([]Score, error)

func scoreProperties(s Score, capture bool) (map[string]any, error) {
	if s.EvaluationName == "" {
		return nil, errors.New("EvaluationName is required")
	}
	switch s.Evaluator {
	case EvaluatorModel, EvaluatorCode, EvaluatorHuman:
	default:
		return nil, errors.New("evaluator must be model, code or human")
	}
	hasValue := s.ScoreValue != nil && !math.IsNaN(*s.ScoreValue) && !math.IsInf(*s.ScoreValue, 0)
	if !hasValue && s.ScoreLabel == "" {
		return nil, errors.New("ScoreValue or ScoreLabel is required")
	}
	p := map[string]any{"evaluation_name": s.EvaluationName}
	if hasValue {
		p["score_value"] = *s.ScoreValue
	}
	put(p, "score_label", s.ScoreLabel)
	p["evaluator"] = s.Evaluator
	if s.Sampled != nil {
		p["sampled"] = *s.Sampled
	}
	if capture && s.Explanation != "" {
		if masked, ok := redactText(s.Explanation); ok {
			text := []rune(strings.TrimSpace(masked))
			if len(text) > explanationChars {
				text = text[:explanationChars]
			}
			put(p, "explanation", strings.TrimSpace(string(text)))
		}
	}
	return p, nil
}

func scheduleEvaluation(i *Interaction, snap turnSnapshot, evaluate EvaluateFunc) chan struct{} {
	t := buildTurn(i, snap, nil, DefaultOutcomes)
	turn := EvaluationTurn{InteractionID: t.InteractionID, ConversationID: t.ConversationID, Prompt: t.Prompt, Answer: t.Answer, ToolNames: t.ToolNames, History: t.History}
	return enqueue(func() {
		scores, err := withTimeout(evaluateTimeout, func(ctx context.Context) ([]Score, error) { return evaluate(ctx, turn) })
		if err != nil {
			report(KindUserCallback, "evaluator", err)
			return
		}
		for _, s := range scores {
			p, err := scoreProperties(s, i.captureContent)
			if err != nil {
				warnf("ai_interaction_scored dropped: %v", err)
				continue
			}
			i.sendBackground("ai_interaction_scored", p)
		}
	})
}

// TrackScoreOptions are a score plus the interaction it is for, for [TrackScore].
type TrackScoreOptions struct {
	Score
	// InteractionID of the answer scored. Required.
	InteractionID string
	// ConversationID of the answer. Required (falls back to the context).
	ConversationID string
	UserID         string
	AnonymousID    string
	Feature        string
	Surface        Surface
	// Trigger is who started the scored answer.
	Trigger    Trigger
	AccountID  string
	OnBehalfOf string
	// AgentVersion names the app release or configuration; Variant names its experiment arm.
	AgentVersion string
	Variant      string
	// Analytics is the client for this event. Falls back to the context, then the global client.
	Analytics analytics.Client
	// Context is extra message context for this event.
	Context map[string]any
}

// TrackScore sends ai_interaction_scored for a score from elsewhere: a human review, an offline
// judge. A score that cannot be made conformant is dropped with a warning.
func TrackScore(ctx context.Context, o TrackScoreOptions) {
	safely(func() {
		cv := valuesFrom(ctx)
		if cv == nil {
			cv = &contextValues{}
		}
		conversation := first(o.ConversationID, cv.conversationID)
		if o.InteractionID == "" || conversation == "" {
			warnf("ai_interaction_scored dropped: InteractionID and ConversationID are required")
			return
		}
		capture := cfg().captureContent
		if i := FromContext(ctx); i != nil {
			capture = i.captureContent
		}
		score, err := scoreProperties(o.Score, capture)
		if err != nil {
			warnf("ai_interaction_scored dropped: %v", err)
			return
		}
		p := map[string]any{"conversation_id": conversation, "interaction_id": o.InteractionID}
		put(p, "feature", first(o.Feature, cv.feature))
		put(p, "surface", string(first2(surfaceOf(o.Surface), surfaceOf(cv.surface))))
		put(p, "account_id", first(o.AccountID, cv.accountID))
		put(p, "on_behalf_of", first(o.OnBehalfOf, cv.onBehalfOf))
		put(p, "agent_version", first(o.AgentVersion, cv.agentVersion))
		put(p, "variant", first(o.Variant, cv.variant))
		if o.Trigger == TriggerUser || o.Trigger == TriggerSystem {
			p["trigger"] = string(o.Trigger)
		}
		for k, v := range score {
			p[k] = v
		}
		sendStandalone(cv, "ai_interaction_scored", o.UserID, o.AnonymousID, o.Analytics, o.Context, p)
	})
}

// sendStandalone sends an event outside an interaction, with identity from the options, the
// context and its cookies.
func sendStandalone(cv *contextValues, event, userID, anonymousID string, client analytics.Client, msgContext, props map[string]any) {
	c := cfg()
	in := identityInputs{userID: userID, anonymousID: anonymousID}
	id := resolveIdentity(in, cv, c)
	if client == nil {
		client = cv.analytics
	}
	if client == nil {
		client = c.analytics
	}
	if msgContext == nil {
		msgContext = cv.context
	}
	emitAI(client, event, id.userID, id.anonymousID, props, withSessionID(msgContext, id.sessionID), IntegrationManual, time.Now())
}
