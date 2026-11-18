package mcpbridge

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestExecutingLargeWriteAcknowledgement(t *testing.T) {
	for _, message := range []string{
		`{"id":1,"result":{"content":"` + strings.Repeat("x", MaxObserved+32) + `"}}`,
		`{"result":{"content":"` + strings.Repeat("x", MaxObserved+32) + `"},"id":1}`,
	} {
		r := NewResponses()
		defer r.Close()
		var got map[string]any
		r.Await(1, func(m map[string]any) { got = m })
		out := NewWriter(io.Discard, r)
		for len(message) > 0 {
			n := min(113, len(message))
			_, err := out.Write([]byte(message[:n]))
			if err != nil {
				t.Fatal(err)
			}
			message = message[n:]
		}
		if got != nil {
			t.Fatal("ack before complete write")
		}
		_, _ = out.Write([]byte("\n"))
		if got == nil || got["_content_omitted"] != true || r.Waiting() {
			t.Fatalf("successful large write lost: %v", got)
		}
	}
}
func TestEnvelopeSplitAndProtocolFields(t *testing.T) {
	message := []byte(`{"jsonrpc":"2.0","result":{"text":"escaped \"}, text","nested":[{}]},"id":"late"}`)
	for _, n := range []int{1, 3, len(message)} {
		var e Envelope
		for p := message; len(p) > 0; {
			size := min(n, len(p))
			e.Feed(p[:size])
			p = p[size:]
		}
		r := NewResponses()
		defer r.Close()
		called := false
		r.Await("late", func(m map[string]any) { called = true })
		r.DeliverJSON(e.JSON())
		if !called {
			t.Fatalf("split %d: %s", n, e.JSON())
		}
	}
	var e Envelope
	e.Feed([]byte(`{"method":"ping","id":1}`))
	if !bytes.Contains(e.JSON(), []byte(`"method":"ping"`)) {
		t.Fatal("lost method")
	}
}

func TestExecutingLargeErrorMetadata(t *testing.T) {
	for _, suffix := range []bool{false, true} {
		result := `{"isError":true,"content":[{"type":"text","text":"` + strings.Repeat("x", MaxObserved+32) + `"}]}`
		if suffix {
			result = `{"content":[{"type":"text","text":"` + strings.Repeat("x", MaxObserved+32) + `"}],"isError":true}`
		}
		var e Envelope
		e.Feed([]byte(`{"id":1,"result":` + result + `}`))
		r := NewResponses()
		defer r.Close()
		var got map[string]any
		r.Await(1, func(m map[string]any) { got = m })
		r.DeliverJSON(e.JSON())
		if got == nil || got["result"].(map[string]any)["isError"] != true || fmt.Sprint(got["_result_size"]) != fmt.Sprint(len(result)) {
			t.Fatalf("metadata=%v", got)
		}
	}
}

func TestExecutingLargeProtocolError(t *testing.T) {
	var e Envelope
	e.Feed([]byte(`{"error":{"message":"` + strings.Repeat("x", MaxObserved+32) + `","code":-32602},"id":1}`))
	r := NewResponses()
	defer r.Close()
	var got map[string]any
	r.Await(1, func(m map[string]any) { got = m })
	r.DeliverJSON(e.JSON())
	if got == nil || fmt.Sprint(got["error"].(map[string]any)["code"]) != "-32602" {
		t.Fatalf("protocol code=%v", got)
	}
}
