package dataplane

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"testing"
	"time"
)

func post(t *testing.T, url string, body []byte, gz bool) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestDataPlane(t *testing.T) {
	s := Start()
	defer s.Close()
	var zipped bytes.Buffer
	w := gzip.NewWriter(&zipped)
	_, _ = w.Write([]byte(`{"batch":[{"type":"track","event":"b"}]}`))
	_ = w.Close()
	cases := []struct {
		path string
		body []byte
		gz   bool
		want int
	}{
		{"/v1/batch", []byte(`{"batch":[{"type":"track","event":"a","properties":{"x":1}}]}`), false, 200},
		{"/v1/batch", zipped.Bytes(), true, 200},
		{"/v1/batch", []byte("not gzip"), true, 400},
		{"/v1/batch", []byte("not json"), false, 400},
		{"/other", nil, false, 404},
	}
	for _, c := range cases {
		if got := post(t, s.URL+c.path, c.body, c.gz); got != c.want {
			t.Errorf("%s: status %d, want %d", c.path, got, c.want)
		}
	}
	got := s.Wait(2, time.Second)
	if len(got) != 2 || got[0].Event != "a" || got[1].Event != "b" || got[0].Properties["x"] != 1.0 {
		t.Errorf("messages = %+v", got)
	}
	if len(s.Wait(3, 20*time.Millisecond)) != 2 {
		t.Error("Wait invented messages")
	}
}
