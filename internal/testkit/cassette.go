package testkit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// Cassette is a provider cassette (spec/fixtures/CASSETTE-FORMAT.md).
type Cassette struct {
	Synthetic    bool          `json:"synthetic"`
	Description  string        `json:"description,omitempty"`
	Sources      []string      `json:"sources,omitempty"`
	RecordedAt   string        `json:"recorded_at,omitempty"`
	Interactions []Interaction `json:"interactions"`
}

// Interaction is one recorded request and its response.
type Interaction struct {
	Request  CassetteRequest  `json:"request"`
	Response CassetteResponse `json:"response"`
}

// CassetteRequest holds only the method and the path (no query, headers or body).
type CassetteRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// CassetteResponse holds a JSON (or string) body, or a streamed body as Events.
type CassetteResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	Events  []StreamEvent     `json:"events,omitempty"`
}

// StreamEvent is one server-sent event, or one AWS event-stream message (Event is its :event-type).
type StreamEvent struct {
	Event   string          `json:"event,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// LoadCassette reads a cassette file.
func LoadCassette(path string) (*Cassette, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Cassette
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("cassette %s: %w", path, err)
	}
	return &c, nil
}

// Replay queues every model call of the cassette on its route, in order, with the recorded status
// and headers. A request takes the next one on its route: the path's ending decides the route
// (`/chat/completions`, `/responses`, `/messages`, `:generateContent`, `:streamGenerateContent`,
// `/model/{modelId}/converse`, `/model/{modelId}/converse-stream`), so API version prefixes, the
// model and the query never matter. Other interactions (`/embeddings`, `/models`, ...) are skipped,
// as in the reference testkits.
func (l *LLM) Replay(c *Cassette) {
	for _, it := range c.Interactions {
		if !strings.EqualFold(it.Request.Method, http.MethodPost) {
			continue
		}
		path, _, _ := strings.Cut(it.Request.Path, "?")
		if route := routeOf(path); route != "" {
			l.enqueue(route, it.Response)
		}
	}
}

func (resp CassetteResponse) serve(_ *LLM, w http.ResponseWriter, _ *http.Request) {
	contentType := strings.ToLower(resp.Headers["content-type"])
	switch {
	case resp.Events != nil && strings.Contains(contentType, "application/vnd.amazon.eventstream"):
		events := make([]eventstream.Message, 0, len(resp.Events))
		for _, e := range resp.Events {
			events = append(events, eventMessage(e.Event, e.Payload))
		}
		writeEventStream(w, resp.Status, resp.Headers, events)
	case resp.Events != nil:
		sse := make([]SSEEvent, 0, len(resp.Events))
		for _, e := range resp.Events {
			sse = append(sse, SSEEvent{Event: e.Event, Data: e.Payload})
		}
		Reply{Status: resp.Status, Headers: resp.Headers, SSE: sse}.serve(nil, w, nil)
	default:
		body := []byte(resp.Body)
		// A string body is a response that was not JSON: sent as recorded.
		var s string
		if json.Unmarshal(resp.Body, &s) == nil {
			body = []byte(s)
		}
		writeHead(w, resp.Status, "application/json", resp.Headers)
		_, _ = w.Write(body)
	}
}
