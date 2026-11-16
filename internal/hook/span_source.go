package hook

// SpanSource is portable internal rule evidence, never event properties.
type SpanSource struct {
	TraceID       string   `json:"traceId"`
	SpanID        string   `json:"spanId"`
	ParentSpanID  string   `json:"parentSpanId,omitempty"`
	ResponseID    string   `json:"responseId,omitempty"`
	RequestModel  string   `json:"requestModel,omitempty"`
	ResponseModel string   `json:"responseModel,omitempty"`
	Start         float64  `json:"start"`
	End           float64  `json:"end"`
	Usage         bool     `json:"usage"`
	Output        bool     `json:"output"`
	Error         bool     `json:"error"`
	LLM           bool     `json:"llm"`
	Ancestors     []string `json:"ancestors"`
	// AncestorsComplete is true only when known parent edges reach a root.
	AncestorsComplete bool `json:"ancestorsComplete,omitempty"`
}
