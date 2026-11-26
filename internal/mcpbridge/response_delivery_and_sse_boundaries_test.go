package mcpbridge

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type round2FailWriter struct{}

func (round2FailWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A panicking observer callback never escapes Write, Close, Fail or the timeout goroutine.
func TestRound2WaiterPanicContained(t *testing.T) {
	saved := ResponseTimeout
	ResponseTimeout = 10 * time.Millisecond
	t.Cleanup(func() { ResponseTimeout = saved })
	for _, release := range []string{"write", "failed_write", "close", "fail", "timeout"} {
		t.Run(release, func(t *testing.T) {
			r := NewResponses()
			ran := make(chan struct{})
			if !r.Await(1, func(map[string]any) { close(ran); panic("observer") }) {
				t.Fatal("await rejected")
			}
			var w *Writer
			switch release {
			case "write":
				w = NewWriter(&bytes.Buffer{}, r)
			case "failed_write":
				w = NewWriter(round2FailWriter{}, r)
			}
			switch release {
			case "write", "failed_write":
				n, err := w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
				if (err != nil) != (release == "failed_write") || (err == nil && n == 0) {
					t.Fatalf("n=%d err=%v", n, err)
				}
			case "close":
				r.Close()
			case "fail":
				r.Fail(1)
			}
			select {
			case <-ran:
			case <-time.After(time.Second):
				t.Fatal("observer never ran")
			}
		})
	}
}

// The response in a failed write reaches its waiter marked failed; other waiters see no response.
func TestRound2FailedWriteReportsAttemptedResponse(t *testing.T) {
	r := NewResponses()
	got := map[string]string{}
	for _, id := range []int{1, 2} {
		r.AwaitWrite(id, func(response map[string]any, failed bool) {
			got[fmt.Sprint(id)] = fmt.Sprintf("response=%v size=%v failed=%v", response != nil, response["_result_size"], failed)
		})
	}
	_, err := NewWriter(round2FailWriter{}, r).Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"a":"\u003c"}}` + "\n"))
	if err == nil {
		t.Fatal("write error lost")
	}
	if got["1"] != "response=true size=14 failed=true" || got["2"] != "response=false size=<nil> failed=false" {
		t.Fatalf("waiters=%v", got)
	}
	// Await keeps its contract: a failed write gives no response.
	r = NewResponses()
	var plain map[string]any
	called := false
	r.Await(1, func(response map[string]any) { called, plain = true, response })
	r.FailJSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	if !called || plain != nil {
		t.Fatalf("called=%v response=%v", called, plain)
	}
}

// _result_size is the written length of the result, escapes included.
func TestRound2DeliveredResultSize(t *testing.T) {
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"text":"a\u0026b \u003cok\u003e"}}`,
		`[{"jsonrpc":"2.0","id":1,"result":{"text":"a\u0026b \u003cok\u003e"}}]`,
	} {
		r := NewResponses()
		var size any
		r.Await(1, func(response map[string]any) { size = response["_result_size"] })
		r.DeliverJSON([]byte(line))
		if size != len(`{"text":"a\u0026b \u003cok\u003e"}`) {
			t.Fatalf("%s: size=%v", line, size)
		}
	}
}

// An oversized SSE frame whose "data:" prefix is split across writes.
func TestRound2SplitSSEPrefix(t *testing.T) {
	r := NewResponses()
	var lines []string
	var s Lines
	r.Await(1, func(map[string]any) {})
	big := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"` + strings.Repeat("a", MaxObserved) + `"}]}}`
	for _, chunk := range []string{"da", "ta: " + big, "\n"} {
		s.Feed([]byte(chunk), r, func(line []byte) { lines = append(lines, string(line)) })
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], `data: {`) || !strings.Contains(lines[0], `"_content_omitted":true`) {
		t.Fatalf("lines=%.200q", lines)
	}
}

// Every response of an oversized batch is acknowledged.
func TestRound2OversizedBatchEnvelope(t *testing.T) {
	r := NewResponses()
	seen := map[string]any{}
	for _, id := range []int{1, 2} {
		r.Await(id, func(response map[string]any) { seen[fmt.Sprint(id)] = response["result"] })
	}
	big := strings.Repeat("a", MaxObserved)
	batch := `[{"jsonrpc":"2.0","id":1,"result":{"isError":false,"content":[{"type":"text","text":"` + big + `"}]}},{"jsonrpc":"2.0","id":2,"result":{"isError":true}}]` + "\n"
	if _, err := NewWriter(&bytes.Buffer{}, r).Write([]byte(batch)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen["1"] == nil || seen["2"] == nil {
		t.Fatalf("acknowledged=%v", seen)
	}
}
