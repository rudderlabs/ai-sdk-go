package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// BedrockReply is one exact reply on a Bedrock Runtime route (`bedrock.converse`,
// `bedrock.converse_stream`), shaped like a fixture's Bedrock call step.
type BedrockReply struct {
	// Response is the Converse output body, returned as is.
	Response json.RawMessage `json:"response,omitempty"`
	// Stream holds ConverseStream events, each an object with one key, e.g.
	// {"contentBlockDelta": {...}}: the key is the :event-type, its value the payload.
	Stream []json.RawMessage `json:"stream,omitempty"`
	// RequestID is sent as the x-amzn-RequestId header.
	RequestID string `json:"request_id,omitempty"`
	// Error fails the call: its status, X-Amzn-ErrorType and a {"message"} body.
	Error *BedrockError `json:"error,omitempty"`
	// StreamError ends the stream with one exception message after the Stream events.
	StreamError *BedrockError `json:"stream_error,omitempty"`
}

// BedrockError is an AWS error: Code is the exception name (ThrottlingException, ...).
type BedrockError struct {
	Code    string `json:"code"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message"`
}

// ScriptBedrock queues replies for `bedrock.converse` (POST /model/{modelId}/converse) or
// `bedrock.converse_stream` (POST /model/{modelId}/converse-stream), served in order.
func (l *LLM) ScriptBedrock(route string, replies ...BedrockReply) {
	for _, r := range replies {
		l.enqueue(route, r)
	}
}

func (rep BedrockReply) serve(_ *LLM, w http.ResponseWriter, _ *http.Request) {
	headers := map[string]string{}
	if rep.RequestID != "" {
		headers["X-Amzn-Requestid"] = rep.RequestID
	}
	if rep.Error != nil {
		status := rep.Error.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		headers["X-Amzn-Errortype"] = rep.Error.Code
		writeHead(w, status, "application/json", headers)
		b, _ := json.Marshal(map[string]string{"message": rep.Error.Message})
		_, _ = w.Write(b)
		return
	}
	if rep.Stream == nil && rep.StreamError == nil {
		writeHead(w, http.StatusOK, "application/json", headers)
		if len(rep.Response) == 0 {
			_, _ = w.Write([]byte("{}"))
			return
		}
		_, _ = w.Write(rep.Response)
		return
	}
	events := make([]esMessage, 0, len(rep.Stream)+1)
	for i, raw := range rep.Stream {
		var one map[string]json.RawMessage
		if err := json.Unmarshal(raw, &one); err != nil || len(one) != 1 {
			writeError(w, http.StatusInternalServerError,
				fmt.Sprintf("testkit: bedrock stream event %d must be an object with one key: %s", i, raw))
			return
		}
		for name, payload := range one {
			events = append(events, eventMessage(name, payload))
		}
	}
	if e := rep.StreamError; e != nil {
		b, _ := json.Marshal(map[string]string{"message": e.Message})
		events = append(events, exceptionMessage(e.Code, b))
	}
	writeEventStream(w, http.StatusOK, headers, events)
}

// eventMessage is an event-stream event message with a JSON payload.
func eventMessage(eventType string, payload []byte) esMessage {
	return esMessage{payload: compact(payload), headers: [][2]string{
		{":message-type", "event"}, {":event-type", eventType}, {":content-type", "application/json"},
	}}
}

// exceptionMessage is a modeled exception, as AWS sends it mid-stream.
func exceptionMessage(code string, payload []byte) esMessage {
	return esMessage{payload: payload, headers: [][2]string{
		{":message-type", "exception"}, {":exception-type", code}, {":content-type", "application/json"},
	}}
}

// writeEventStream sends an application/vnd.amazon.eventstream body, flushing after each message.
func writeEventStream(w http.ResponseWriter, status int, headers map[string]string, events []esMessage) {
	writeHead(w, status, "application/vnd.amazon.eventstream", headers)
	rc := http.NewResponseController(w)
	for _, m := range events {
		if m.encode(w) != nil {
			return
		}
		_ = rc.Flush()
	}
}

func compact(raw []byte) []byte {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}
