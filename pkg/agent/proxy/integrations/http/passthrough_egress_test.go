package http

import (
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestPassThroughEgressDecision(t *testing.T) {
	opts := models.OutgoingOptions{
		PassThroughPorts: []models.PassThroughRule{{Port: 8126, Mode: models.PassThroughRecordOne}},
		PassThroughHosts: []models.PassThroughRule{{Host: "*.datadoghq.com", Mode: models.PassThroughSkip}},
	}
	cases := []struct {
		name     string
		host     string
		port     uint32
		path     string
		wantOK   bool
		wantMode models.PassThroughMode
	}{
		{"config port recordOne", "", 8126, "/v0.4/traces", true, models.PassThroughRecordOne},
		{"config host skip", "intake.datadoghq.com", 443, "/api/v2/series", true, models.PassThroughSkip},
		{"builtin OTLP default skip", "otel-collector", 4318, "/v1/traces", true, models.PassThroughSkip},
		{"no match", "example.com", 9999, "/orders", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := &url.URL{Path: c.path, Host: c.host}
			mode, ok := passThroughEgressDecision(opts, c.host, c.port, "POST", u)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if ok && mode != c.wantMode {
				t.Fatalf("mode=%q want %q", mode, c.wantMode)
			}
		})
	}
}

func TestPtRecorder_FirstSuccess(t *testing.T) {
	r := &ptRecorder{}
	key := ptRecordKey("POST", "dd-agent", 8126, "/v0.4/traces")

	// A warmup 503 records once (nothing better yet).
	if !r.shouldRecord(key, 503) {
		t.Fatal("first (error) exchange should record")
	}
	// A second error is dropped (we already have one representative).
	if r.shouldRecord(key, 500) {
		t.Fatal("second error should be dropped")
	}
	// The first 2xx records (upgrades the representative).
	if !r.shouldRecord(key, 200) {
		t.Fatal("first 2xx should record")
	}
	// Everything after a 2xx is dropped.
	if r.shouldRecord(key, 200) {
		t.Fatal("subsequent 2xx should be dropped")
	}
	// A different key is independent.
	if !r.shouldRecord(ptRecordKey("POST", "dd-agent", 8126, "/v0.7/config"), 200) {
		t.Fatal("distinct endpoint key should record")
	}
}

func TestPtRecorder_2xxFirstDropsRest(t *testing.T) {
	r := &ptRecorder{}
	key := ptRecordKey("GET", "dd-agent", 8126, "/info")
	if !r.shouldRecord(key, 200) {
		t.Fatal("first 2xx should record")
	}
	if r.shouldRecord(key, 503) {
		t.Fatal("nothing records once a 2xx is captured")
	}
}
