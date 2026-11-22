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

// matchHost covers the apex of a "*.x" glob; normalizeHost must not eat a
// bracket-less IPv6 group as a port.
func TestMatchHostApexAndNormalizeIPv6(t *testing.T) {
	if !matchHost("*.datadoghq.com", "datadoghq.com") {
		t.Fatal("apex host should match *.datadoghq.com")
	}
	if !matchHost("*.datadoghq.com", "intake.datadoghq.com") {
		t.Fatal("subdomain should still match")
	}
	if matchHost("*.datadoghq.com", "evil.com") {
		t.Fatal("unrelated host must not match")
	}
	if got := normalizeHost("::1"); got != "::1" {
		t.Fatalf("normalizeHost(::1) = %q, want ::1 (must not strip a v6 group as a port)", got)
	}
	if got := normalizeHost("api.datadoghq.com:443"); got != "api.datadoghq.com" {
		t.Fatalf("normalizeHost should strip a real :port, got %q", got)
	}
}

// An {id, mode:"off"} override disables a built-in by identity — robust even for
// the PathPrefix OTLP/gRPC rule that a field-reconstructed override would miss.
func TestResolvePassThrough_DisableByID(t *testing.T) {
	grpcPath := "/opentelemetry.proto.collector.trace.v1.TraceService/Export"
	if _, ok := ResolvePassThrough(nil, nil, "", 0, "POST", grpcPath); !ok {
		t.Fatal("OTLP/gRPC should be a built-in passthrough by default")
	}
	off := []PassThroughRule{{ID: "otlp-grpc", Mode: PassThroughOff}}
	if _, ok := ResolvePassThrough(nil, off, "", 0, "POST", grpcPath); ok {
		t.Fatal("id-based off must disable the OTLP/gRPC built-in (field override would miss it)")
	}
	// Disabling one built-in must not affect another.
	if _, ok := ResolvePassThrough(nil, off, "", 0, "POST", "/v1/traces"); !ok {
		t.Fatal("disabling otlp-grpc must not disable otlp-http-traces")
	}
	// Every built-in has a non-empty, unique ID.
	seen := map[string]bool{}
	for _, d := range TelemetryDefaults() {
		if d.Rule.ID == "" {
			t.Fatalf("built-in %q/%q missing ID", d.Provider, d.Label)
		}
		if seen[d.Rule.ID] {
			t.Fatalf("duplicate built-in ID %q", d.Rule.ID)
		}
		seen[d.Rule.ID] = true
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

// Under the proven model a built-in path row matches only with its protocol's
// fingerprint (design v14 §4): any other call on such a path is an ordinary
// dependency. Host rows and the OTLP/gRPC prefix are fingerprints already.
func TestResolveProvenNeedsTheFingerprint(t *testing.T) {
	proto := map[string][]string{"Content-Type": {"application/x-protobuf"}}
	cases := []struct {
		name   string
		host   string
		port   uint32
		path   string
		header map[string][]string
		want   string
	}{
		{"OTLP on its port", "collector", 4318, "/v1/traces", nil, "otlp-http-traces"},
		{"OTLP by its content type", "collector", 8080, "/v1/traces", proto, "otlp-http-traces"},
		{"an app route on an OTLP path", "app", 8080, "/v1/traces", map[string][]string{"Content-Type": {"application/json"}}, ""},
		{"Pyroscope on its port", "pyro", 4040, "/ingest", nil, "pyroscope-ingest"},
		{"an app's /ingest", "app", 8080, "/ingest", nil, ""},
		{"Datadog by its header", "dd", 9000, "/v0.4/traces", map[string][]string{"datadog-meta-lang": {"go"}}, "datadog-v04-traces"},
		{"Datadog on its port", "dd", 8126, "/v0.7/traces", nil, "datadog-v07-traces"},
		{"remote_write by its header", "prom", 9090, "/api/v1/write", map[string][]string{"X-Prometheus-Remote-Write-Version": {"0.1.0"}}, "prometheus-remote-write"},
		{"an app's /api/v1/write", "app", 9090, "/api/v1/write", nil, ""},
		{"a SaaS host", "intake.datadoghq.com", 443, "/anything", nil, "datadog-saas"},
		{"the OTLP/gRPC service", "collector", 4317, "/opentelemetry.proto.collector.trace.v1.TraceService/Export", nil, "otlp-grpc"},
	}
	for _, c := range cases {
		rule, ok := ResolveProven(nil, nil, c.host, c.port, "POST", c.path, c.header)
		if c.want == "" {
			if ok {
				t.Errorf("%s: matched %s, want an ordinary dependency", c.name, rule.ID)
			}
			continue
		}
		if !ok || rule.ID != c.want {
			t.Errorf("%s: got %q (%v), want %s", c.name, rule.ID, ok, c.want)
		}
	}
}

// Under the proven model the user's rules are read as ResolvePassThrough
// reads them (keploy record and keploy test keep theirs until they read
// mock.real; keploy mock's CLI clears them): a user rule beats the built-in
// rows, and an off marker turns a built-in row off, fingerprint or not.
func TestResolveProvenReadsTheUsersRules(t *testing.T) {
	user := []PassThroughRule{{Host: "telemetry.example.com", Mode: PassThroughSkip}}
	rule, ok := ResolveProven(nil, user, "telemetry.example.com", 443, "POST", "/x", nil)
	if !ok || rule.Host != "telemetry.example.com" {
		t.Fatalf("a user's host rule was not read: %+v %v", rule, ok)
	}
	off := []PassThroughRule{{ID: "otlp-http-traces", Mode: PassThroughOff}}
	if rule, ok := ResolveProven(off, nil, "collector", 4318, "POST", "/v1/traces", nil); ok {
		t.Fatalf("a built-in row the user turned off answered: %+v", rule)
	}
	if rule, ok := ResolveProven(nil, nil, "collector", 4318, "POST", "/v1/traces", nil); !ok || rule.ID != "otlp-http-traces" {
		t.Fatalf("the fingerprinted built-in did not answer: %+v %v", rule, ok)
	}
}

// Every fingerprinted built-in path row, one by one (design v14 §4): its
// path alone is an app's route and an ordinary dependency; on its protocol's
// default port, or with its protocol's header (and that header's value, when
// the fingerprint names one), it is the protocol's, and answered.
func TestEveryFingerprintedTelemetryRowNeedsItsFingerprint(t *testing.T) {
	rows := map[string]PassThroughRule{}
	for _, r := range BuiltinTelemetryDefaults() {
		rows[r.ID] = r
	}
	for id, fp := range telemetryFingerprints {
		row, ok := rows[id]
		if !ok {
			t.Fatalf("fingerprint %s names no built-in row", id)
		}
		t.Run(id, func(t *testing.T) {
			port := uint32(18080)
			if fp.port == port {
				t.Fatalf("the test's own port is %s's", id)
			}
			if r, ok := ResolveProven(nil, nil, "app", port, row.Method, row.Path, nil); ok && r.ID == id {
				t.Fatalf("%s answered a call on its path alone", id)
			}
			if fp.port == 0 && fp.header == "" {
				t.Fatalf("%s has no fingerprint", id)
			}
			if fp.port != 0 {
				if r, ok := ResolveProven(nil, nil, "collector", fp.port, row.Method, row.Path, nil); !ok || r.ID != id {
					t.Fatalf("%s did not answer on its port %d: %+v %v", id, fp.port, r, ok)
				}
			}
			if fp.header == "" {
				return
			}
			value := fp.value
			if value == "" {
				value = "1"
			}
			header := map[string][]string{fp.header: {value}}
			if r, ok := ResolveProven(nil, nil, "collector", port, row.Method, row.Path, header); !ok || r.ID != id {
				t.Fatalf("%s did not answer with its header %s: %+v %v", id, fp.header, r, ok)
			}
			if fp.value != "" {
				other := map[string][]string{fp.header: {"application/json"}}
				if r, ok := ResolveProven(nil, nil, "app", port, row.Method, row.Path, other); ok && r.ID == id {
					t.Fatalf("%s answered with its header but another value", id)
				}
			}
		})
	}
	// Every built-in row a path alone would match is fingerprinted: a row
	// with a host, or the OTLP/gRPC service prefix, is one already.
	for _, r := range BuiltinTelemetryDefaults() {
		if r.Host == "" && !r.PathPrefix && r.Port == 0 {
			if _, ok := telemetryFingerprints[r.ID]; !ok {
				t.Errorf("built-in path row %s has no fingerprint", r.ID)
			}
		}
	}
}
