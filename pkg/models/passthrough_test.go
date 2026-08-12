package models

import (
	"encoding/json"
	"testing"
)

// MatchPassThrough is single-tier: host/port/path/method must all match, most
// specific wins. Covers segment-boundary matching and the method gate.
func TestMatchPassThrough_SingleTier(t *testing.T) {
	rules := []PassThroughRule{
		{Port: 8126, Mode: PassThroughRecordOne},
		{Host: "*.datadoghq.com", Mode: PassThroughSkip},
		{Host: "api.datadoghq.com", Port: 443, Mode: PassThroughRecordOne},
	}
	cases := []struct {
		name     string
		host     string
		port     uint32
		method   string
		path     string
		wantOK   bool
		wantMode PassThroughMode
	}{
		{"port only", "", 8126, "POST", "/v0.4/traces", true, PassThroughRecordOne},
		{"host glob", "intake.datadoghq.com", 443, "POST", "/api", true, PassThroughSkip},
		{"host+port most specific wins", "api.datadoghq.com", 443, "POST", "/api", true, PassThroughRecordOne},
		{"no match", "example.com", 9999, "POST", "/x", false, ""},
		{"host with port suffix normalized", "intake.datadoghq.com:443", 0, "GET", "/api", true, PassThroughSkip},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := MatchPassThrough(rules, c.host, c.port, c.method, c.path)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if ok && got.Mode != c.wantMode {
				t.Fatalf("mode=%q want %q", got.Mode, c.wantMode)
			}
		})
	}
}

// Segment-boundary matching (#1): a rule for "/ingest" must match "/ingest" and
// "/ingest/x" but NOT a same-prefixed app route "/ingestData". PathPrefix opts
// into raw-prefix for the gRPC service path.
func TestMatchPassThrough_SegmentBoundaryAndPrefix(t *testing.T) {
	seg := []PassThroughRule{{Path: "/ingest", Mode: PassThroughSkip}}
	if _, ok := MatchPassThrough(seg, "", 0, "POST", "/ingest"); !ok {
		t.Fatal("/ingest should match")
	}
	if _, ok := MatchPassThrough(seg, "", 0, "POST", "/ingest/status"); !ok {
		t.Fatal("/ingest/status should match (segment)")
	}
	if _, ok := MatchPassThrough(seg, "", 0, "POST", "/ingestData"); ok {
		t.Fatal("/ingestData must NOT match (not a segment boundary)")
	}
	pre := []PassThroughRule{{Path: "/opentelemetry.proto.collector.", PathPrefix: true, Mode: PassThroughSkip}}
	if _, ok := MatchPassThrough(pre, "", 0, "POST", "/opentelemetry.proto.collector.trace.v1.TraceService/Export"); !ok {
		t.Fatal("gRPC OTLP :path prefix should match with PathPrefix")
	}
}

// The method gate: a built-in POST rule must not swallow a GET to the same path.
func TestMatchPassThrough_MethodGate(t *testing.T) {
	r := []PassThroughRule{{Path: "/v1/traces", Method: "POST", Mode: PassThroughSkip}}
	if _, ok := MatchPassThrough(r, "", 0, "POST", "/v1/traces"); !ok {
		t.Fatal("POST /v1/traces should match")
	}
	if _, ok := MatchPassThrough(r, "", 0, "GET", "/v1/traces"); ok {
		t.Fatal("GET /v1/traces must NOT match a POST-only rule")
	}
}

// NormalizeMode: empty→recordOne (default), known→self, UNKNOWN→off (fail closed).
func TestNormalizeMode(t *testing.T) {
	if (PassThroughRule{}).NormalizeMode() != DefaultPassThroughMode {
		t.Fatal("empty mode should default to recordOne")
	}
	if (PassThroughRule{Mode: PassThroughSkip}).NormalizeMode() != PassThroughSkip {
		t.Fatal("skip should be preserved")
	}
	if (PassThroughRule{Mode: PassThroughOff}).NormalizeMode() != PassThroughOff {
		t.Fatal("off should be preserved")
	}
	for _, m := range []PassThroughMode{"bogus", "OFF", "Skip", "of"} {
		if (PassThroughRule{Mode: m}).NormalizeMode() != PassThroughOff {
			t.Fatalf("unknown mode %q must fail closed to off, got %q", m, (PassThroughRule{Mode: m}).NormalizeMode())
		}
	}
}

// ResolvePassThrough is the off-aware, two-tier entry point.
func TestResolvePassThrough(t *testing.T) {
	// Zero config: New Relic built-in applies (recordOne + queryKeys), POST only.
	nr, ok := ResolvePassThrough(nil, nil, "collector.newrelic.com", 443, "POST", "/agent_listener/invoke_raw_method")
	if !ok || nr.Mode != PassThroughRecordOne {
		t.Fatalf("NR built-in should resolve to recordOne: ok=%v mode=%q", ok, nr.Mode)
	}
	if len(nr.QueryKeys) != 1 || nr.QueryKeys[0] != "method" {
		t.Fatalf("NR queryKeys = %v, want [method]", nr.QueryKeys)
	}
	// Prometheus remote_write built-in skips (POST).
	if r, ok := ResolvePassThrough(nil, nil, "", 0, "POST", "/api/v1/write"); !ok || r.Mode != PassThroughSkip {
		t.Fatalf("prometheus built-in should skip: ok=%v mode=%q", ok, r.Mode)
	}

	// #1 regression guards: app routes / wrong method are NOT passthrough.
	for _, c := range []struct{ method, path string }{
		{"GET", "/ingestData"}, {"POST", "/ingestData"}, {"POST", "/v1/tracesearch"},
		{"GET", "/v1/traces"}, {"POST", "/api/v1/writeReview"},
	} {
		if _, ok := ResolvePassThrough(nil, nil, "", 0, c.method, c.path); ok {
			t.Fatalf("%s %s must not be passthrough", c.method, c.path)
		}
	}

	// #4 two-tier: a user rule beats a MORE-SPECIFIC built-in host rule.
	// off on the datadog host → record normally, even though *.datadoghq.com skips.
	userOff := []PassThroughRule{{Host: "*.datadoghq.com", Mode: PassThroughOff}}
	if _, ok := ResolvePassThrough(nil, userOff, "trace.agent.datadoghq.com", 443, "POST", "/api/v2/series"); ok {
		t.Fatal("user off rule must beat the *.datadoghq.com built-in (record normally)")
	}
	// A path-only user off beats the built-in for that path too.
	if _, ok := ResolvePassThrough(nil, []PassThroughRule{{Path: "/v1/traces", Mode: PassThroughOff}}, "", 0, "POST", "/v1/traces"); ok {
		t.Fatal("user off on /v1/traces must beat the built-in skip")
	}
	// A user recordOne beats a built-in skip on the same target.
	if r, ok := ResolvePassThrough([]PassThroughRule{{Port: 4318, Mode: PassThroughRecordOne}}, nil, "", 4318, "POST", "/v1/traces"); !ok || r.Mode != PassThroughRecordOne {
		t.Fatalf("user recordOne must beat built-in skip: ok=%v mode=%q", ok, r.Mode)
	}
}

// UnmarshalJSON scalar shorthand is accepted on the JSON agent wire (NOT yaml).
func TestPassThroughRule_UnmarshalJSON_Shorthand(t *testing.T) {
	var list []PassThroughRule
	if err := json.Unmarshal([]byte(`[8126, "4317", {"port":4318,"mode":"skip"}, {"host":"*.sentry.io","mode":"skip"}]`), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 4 || list[0].Port != 8126 || list[1].Port != 4317 {
		t.Fatalf("scalar shorthand not parsed: %+v", list)
	}
	if list[2].Port != 4318 || list[2].Mode != PassThroughSkip || list[3].Host != "*.sentry.io" {
		t.Fatalf("object form not parsed: %+v", list[2:])
	}
}

// TelemetryDefaults is the single source of truth; BuiltinTelemetryDefaults must
// be exactly its rules, and the version is exposed. Every path built-in is POST.
func TestTelemetryDefaults_SingleSource(t *testing.T) {
	defs := TelemetryDefaults()
	rules := BuiltinTelemetryDefaults()
	if len(defs) != len(rules) || len(defs) == 0 {
		t.Fatalf("defs/rules length mismatch: %d vs %d", len(defs), len(rules))
	}
	for i := range defs {
		r := defs[i].Rule
		if r.Port != rules[i].Port || r.Host != rules[i].Host || r.Path != rules[i].Path ||
			r.Mode != rules[i].Mode || r.Method != rules[i].Method || r.PathPrefix != rules[i].PathPrefix ||
			len(r.QueryKeys) != len(rules[i].QueryKeys) {
			t.Fatalf("default %d: BuiltinTelemetryDefaults not derived from TelemetryDefaults", i)
		}
		if defs[i].Provider == "" || defs[i].Label == "" {
			t.Fatalf("default %d missing provider/label", i)
		}
		if r.Path != "" && r.Method != "POST" {
			t.Fatalf("path built-in %q should be POST-gated, got method %q", r.Path, r.Method)
		}
	}
	if TelemetryDefaultsVersion < 1 {
		t.Fatal("TelemetryDefaultsVersion must be >= 1")
	}
}
