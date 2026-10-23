package testkit

import (
	"bytes"
	"io"
	"net/http"
)

// HTTPClient returns a client for SDK calls to the mock server that sends each request body
// from memory.
//
// The AWS SDK closes a request body as soon as the response headers arrive, and its closed body
// answers the transport's final read with io.EOF. Against a local server that replies before
// that read, net/http takes the EOF for a write error and closes the connection under the
// response still being read ("use of closed network connection"). An in-memory body is
// written before the server can reply.
func HTTPClient() *http.Client {
	return &http.Client{Transport: bufferedBodies{http.DefaultTransport.(*http.Transport).Clone()}}
}

type bufferedBodies struct{ next http.RoundTripper }

func (t bufferedBodies) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return t.next.RoundTrip(req)
	}
	b, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.ContentLength = int64(len(b))
	return t.next.RoundTrip(req)
}
