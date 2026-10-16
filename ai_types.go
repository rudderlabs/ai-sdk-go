package rudderstackai

// Integration names sent as context.wrapperLibrary.integration.
const (
	IntegrationManual      = "manual"
	IntegrationOpenAI      = "openai"
	IntegrationAnthropic   = "anthropic"
	IntegrationGoogleGenAI = "google-genai"
	IntegrationBedrock     = "bedrock"
)

// Trigger is who started an interaction: a person (TriggerUser) or the app (TriggerSystem).
type Trigger string

// Triggers.
const (
	TriggerUser   Trigger = "user"
	TriggerSystem Trigger = "system"
)

// ResponseStatus is the final status of an answer, sent as response_status.
type ResponseStatus string

// Response statuses. StatusPaused: the run waits for something outside it (see [PauseReason]).
// StatusIncomplete: it stopped at a cap (max turns, max output tokens, a budget); error_type
// names the cap.
const (
	StatusSuccess    ResponseStatus = "success"
	StatusError      ResponseStatus = "error"
	StatusTimeout    ResponseStatus = "timeout"
	StatusCancelled  ResponseStatus = "cancelled"
	StatusPaused     ResponseStatus = "paused"
	StatusIncomplete ResponseStatus = "incomplete"
)

// PauseReason is what a paused run waits for, sent as pause_reason.
type PauseReason string

// Pause reasons.
const (
	PauseApproval     PauseReason = "approval"
	PauseHumanInput   PauseReason = "human_input"
	PauseDeferredTool PauseReason = "deferred_tool"
	PauseAuthRequired PauseReason = "auth_required"
	PauseOther        PauseReason = "other"
)

// Surface is where the user met the AI, sent as surface. Set by the app: the SDK cannot tell.
type Surface string

// Surfaces.
const (
	SurfaceWeb        Surface = "web"
	SurfaceMobile     Surface = "mobile"
	SurfaceSlack      Surface = "slack"
	SurfaceTeams      Surface = "teams"
	SurfaceEmail      Surface = "email"
	SurfaceChatGPTApp Surface = "chatgpt_app"
	SurfaceClaudeApp  Surface = "claude_app"
	SurfaceIDE        Surface = "ide"
	SurfaceAPI        Surface = "api"
	SurfaceOther      Surface = "other"
)

// ActionType is the kind of ai_user_action.
type ActionType string

// Standard action types.
const (
	ActionFeedbackGiven     ActionType = "feedback_given"
	ActionFeedbackRetracted ActionType = "feedback_retracted"
	ActionCopiedResponse    ActionType = "copied_response"
	ActionShared            ActionType = "shared"
	ActionReported          ActionType = "reported"
	ActionRegenerated       ActionType = "regenerated"
	ActionAccepted          ActionType = "accepted"
	ActionRejected          ActionType = "rejected"
	ActionEdited            ActionType = "edited"
	ActionClickedThrough    ActionType = "clicked_through"
)

var (
	responseStatuses = set(StatusSuccess, StatusError, StatusTimeout, StatusCancelled, StatusPaused, StatusIncomplete)
	pauseReasons     = set(PauseApproval, PauseHumanInput, PauseDeferredTool, PauseAuthRequired, PauseOther)
	surfaces         = set(SurfaceWeb, SurfaceMobile, SurfaceSlack, SurfaceTeams, SurfaceEmail, SurfaceChatGPTApp, SurfaceClaudeApp, SurfaceIDE, SurfaceAPI, SurfaceOther)
	actionTypes      = set(ActionFeedbackGiven, ActionFeedbackRetracted, ActionCopiedResponse, ActionShared, ActionReported, ActionRegenerated, ActionAccepted, ActionRejected, ActionEdited, ActionClickedThrough)
	serviceTiers     = set("batch", "flex", "standard", "fast", "other")
	attachmentKinds  = set("image", "audio", "video", "pdf", "file")
)

func set[T comparable](values ...T) map[T]bool {
	m := make(map[T]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

// Ptr returns a pointer to v, for the optional fields of [LLMCall], [ToolCall] and the options.
func Ptr[T any](v T) *T { return &v }
