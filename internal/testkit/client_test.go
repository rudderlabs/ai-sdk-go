package testkit

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// smithyBody acts like smithy-go's request body after the AWS SDK closes it once the response
// arrives: the transport's final WriteTo read gets io.EOF, which net/http takes for a write error.
type smithyBody struct {
	io.Reader
	closed chan struct{}
}

func (b *smithyBody) WriteTo(io.Writer) (int64, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *smithyBody) Close() error { return nil }

func TestHTTPClientKeepsTheResponseWhenTheRequestBodyFailsLate(t *testing.T) {
	connGone := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "a")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
			close(connGone)
			return
		case <-release:
		}
		_, _ = io.WriteString(w, "b")
	}))
	defer srv.Close()

	body := &smithyBody{Reader: strings.NewReader("hi"), closed: make(chan struct{})}
	req, err := http.NewRequest(http.MethodPost, srv.URL, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 2
	resp, err := HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	close(body.closed)
	select {
	case <-connGone:
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if got, err := io.ReadAll(resp.Body); err != nil || string(got) != "ab" {
		t.Fatalf("body %q, err %v", got, err)
	}
}
