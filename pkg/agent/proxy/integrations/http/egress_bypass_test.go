package http

import (
	"net/url"
	"testing"
)

func TestIsTelemetryEgress(t *testing.T) {
	mk := func(p string) *url.URL { return &url.URL{Path: p} }
	cases := []struct {
		name   string
		method string
		u      *url.URL
		want   bool
	}{
		{"otlp traces", "POST", mk("/v1/traces"), true},
		{"pyroscope ingest", "POST", mk("/ingest"), true},
		{"get traces (not export)", "GET", mk("/v1/traces"), false},
		{"get ingest", "GET", mk("/ingest"), false},
		{"otlp metrics (not bypassed)", "POST", mk("/v1/metrics"), false},
		{"api endpoint", "POST", mk("/cluster/login"), false},
		{"nil url", "POST", nil, false},
		{"traces trailing segment", "POST", mk("/v1/traces/extra"), false},
		{"ingest trailing segment", "POST", mk("/ingest/extra"), false},
	}
	for _, c := range cases {
		if got := isTelemetryEgress(c.method, c.u); got != c.want {
			t.Errorf("%s: isTelemetryEgress(%q,%v)=%v want %v", c.name, c.method, c.u, got, c.want)
		}
	}
}
