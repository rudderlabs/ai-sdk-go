// Package dataplane is a mock RudderStack data plane for tests and examples: it accepts
// analytics-go batches and keeps every message.
package dataplane

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Message is one received event.
type Message struct {
	Type        string         `json:"type"`
	Event       string         `json:"event"`
	UserID      string         `json:"userId"`
	AnonymousID string         `json:"anonymousId"`
	Channel     string         `json:"channel"`
	Context     map[string]any `json:"context"`
	Properties  map[string]any `json:"properties"`
}

// Server is a running mock data plane.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	messages []Message
}

// Start starts a mock data plane on a local port.
func Start() *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/batch" {
		http.NotFound(w, r)
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	var batch struct {
		Batch []Message `json:"batch"`
	}
	if err := json.NewDecoder(body).Decode(&batch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.messages = append(s.messages, batch.Batch...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// Messages returns every message received so far.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

// Wait polls until at least n messages arrived or the timeout passes, and returns them.
func (s *Server) Wait(n int, timeout time.Duration) []Message {
	deadline := time.Now().Add(timeout)
	for {
		got := s.Messages()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}
