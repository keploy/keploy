package http

import (
	"net/url"
	"testing"
)

func TestIsOTLPTracesExport(t *testing.T) {
	mk := func(p string) *url.URL { return &url.URL{Path: p} }
	cases := []struct {
		name   string
		method string
		u      *url.URL
		want   bool
	}{
		{"otlp post traces", "POST", mk("/v1/traces"), true},
		{"get traces (not export)", "GET", mk("/v1/traces"), false},
		{"post other path", "POST", mk("/v1/metrics"), false},
		{"post api endpoint", "POST", mk("/cluster/login"), false},
		{"nil url", "POST", nil, false},
		{"trailing segment not matched", "POST", mk("/v1/traces/extra"), false},
	}
	for _, c := range cases {
		if got := isOTLPTracesExport(c.method, c.u); got != c.want {
			t.Errorf("%s: isOTLPTracesExport(%q,%v)=%v want %v", c.name, c.method, c.u, got, c.want)
		}
	}
}
