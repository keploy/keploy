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
			rule, ok := passThroughEgressDecision(opts, c.host, c.port, "POST", u)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if ok && rule.Mode != c.wantMode {
				t.Fatalf("mode=%q want %q", rule.Mode, c.wantMode)
			}
		})
	}
}

func TestPtRecorder_FirstSuccess(t *testing.T) {
	r := &ptRecorder{}
	key := ptRecordKey("POST", "dd-agent", 8126, "/v0.4/traces", nil, nil)

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
	if !r.shouldRecord(ptRecordKey("POST", "dd-agent", 8126, "/v0.7/config", nil, nil), 200) {
		t.Fatal("distinct endpoint key should record")
	}
}

// New Relic multiplexes every RPC onto ONE path via ?method=. QueryKeys:[method]
// must make each method a distinct recordOne key while ignoring the per-session
// run_id, so repeat calls of the same method still collapse.
func TestPtRecordKey_QueryMux(t *testing.T) {
	qk := []string{"method"}
	p := "/agent_listener/invoke_raw_method"
	q := func(vals url.Values) url.Values { return vals }

	connect := ptRecordKey("POST", "collector.newrelic.com", 443, p, qk,
		q(url.Values{"method": {"connect"}, "run_id": {"111"}}))
	metric := ptRecordKey("POST", "collector.newrelic.com", 443, p, qk,
		q(url.Values{"method": {"metric_data"}, "run_id": {"111"}}))
	metric2 := ptRecordKey("POST", "collector.newrelic.com", 443, p, qk,
		q(url.Values{"method": {"metric_data"}, "run_id": {"999"}})) // different run_id

	if connect == metric {
		t.Fatal("connect and metric_data must be distinct keys under QueryKeys[method]")
	}
	if metric != metric2 {
		t.Fatal("same method with a different run_id must collapse to one key")
	}

	// A recorder keeps connect and metric_data as separate representatives.
	r := &ptRecorder{}
	if !r.shouldRecord(connect, 200) || !r.shouldRecord(metric, 200) {
		t.Fatal("distinct methods should each record once")
	}
	if r.shouldRecord(metric2, 200) {
		t.Fatal("a second metric_data (varying run_id) must be dropped")
	}
}

// mockQueryMatches gates replay selection so a connect request is never served a
// metric_data mock, while non-significant params (run_id) don't affect matching.
func TestMockQueryMatches(t *testing.T) {
	mock := &models.HTTPReq{
		URL:       "http://collector.newrelic.com/agent_listener/invoke_raw_method?method=connect&run_id=111",
		URLParams: map[string]string{"method": "connect", "run_id": "111"},
	}
	qk := []string{"method"}

	same := &url.URL{Path: "/agent_listener/invoke_raw_method", RawQuery: "method=connect&run_id=999"}
	if !mockQueryMatches(mock, same, qk) {
		t.Fatal("same method (differing run_id) should match")
	}
	other := &url.URL{Path: "/agent_listener/invoke_raw_method", RawQuery: "method=metric_data"}
	if mockQueryMatches(mock, other, qk) {
		t.Fatal("different method must not match")
	}
	// Empty queryKeys → query-agnostic (default recordOne behavior).
	if !mockQueryMatches(mock, other, nil) {
		t.Fatal("empty queryKeys should match regardless of query")
	}
}

func TestPtRecorder_2xxFirstDropsRest(t *testing.T) {
	r := &ptRecorder{}
	key := ptRecordKey("GET", "dd-agent", 8126, "/info", nil, nil)
	if !r.shouldRecord(key, 200) {
		t.Fatal("first 2xx should record")
	}
	if r.shouldRecord(key, 503) {
		t.Fatal("nothing records once a 2xx is captured")
	}
}
