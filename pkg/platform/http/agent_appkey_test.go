package http

import (
	"net/http"
	"testing"
)

type captureRT struct{ seen string }

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.seen = r.Header.Get(appIDHeader)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func doReq(t *testing.T, key string) string {
	t.Helper()
	cap := &captureRT{}
	tr := &appIDTransport{base: cap, keyFn: func() string { return key }}
	req, _ := http.NewRequest(http.MethodGet, "http://localhost/agent/health", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	return cap.seen
}

func TestAppIDTransportStampsWhenKeySet(t *testing.T) {
	if got := doReq(t, "ns/dep/ts-1"); got != "ns/dep/ts-1" {
		t.Fatalf("header = %q, want ns/dep/ts-1", got)
	}
}

func TestAppIDTransportNoHeaderWhenKeyEmpty(t *testing.T) {
	if got := doReq(t, ""); got != "" {
		t.Fatalf("expected no %s header for empty key, got %q", appIDHeader, got)
	}
}
