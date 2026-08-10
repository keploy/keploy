package models

import (
	"encoding/json"
	"testing"
)

func TestMatchPassThrough_PortHostPrecedence(t *testing.T) {
	rules := []PassThroughRule{
		{Port: 8126, Mode: PassThroughRecordOne},
		{Host: "*.datadoghq.com", Mode: PassThroughSkip},
		{Host: "api.datadoghq.com", Port: 443, Mode: PassThroughRecordOne},
	}
	cases := []struct {
		name     string
		host     string
		port     uint32
		path     string
		wantOK   bool
		wantMode PassThroughMode
	}{
		{"port only", "", 8126, "/v0.4/traces", true, PassThroughRecordOne},
		{"host glob", "intake.datadoghq.com", 443, "/api", true, PassThroughSkip},
		{"host+port most specific wins", "api.datadoghq.com", 443, "/api", true, PassThroughRecordOne},
		{"no match", "example.com", 9999, "/x", false, ""},
		{"host with port suffix normalized", "intake.datadoghq.com:443", 0, "/api", true, PassThroughSkip},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := MatchPassThrough(rules, c.host, c.port, c.path)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if ok && got.Mode != c.wantMode {
				t.Fatalf("mode=%q want %q", got.Mode, c.wantMode)
			}
		})
	}
}

func TestMatchPassThrough_PathPrefix(t *testing.T) {
	rules := []PassThroughRule{{Path: "/opentelemetry.proto.collector.", Mode: PassThroughSkip}}
	if _, ok := MatchPassThrough(rules, "", 0, "/opentelemetry.proto.collector.trace.v1.TraceService/Export"); !ok {
		t.Fatal("expected gRPC OTLP :path prefix to match")
	}
	if _, ok := MatchPassThrough(rules, "", 0, "/pkg.Other/Method"); ok {
		t.Fatal("unrelated path should not match")
	}
}

func TestNormalizeMode(t *testing.T) {
	if (PassThroughRule{}).NormalizeMode() != DefaultPassThroughMode {
		t.Fatal("empty mode should default to recordOne")
	}
	if (PassThroughRule{Mode: "bogus"}).NormalizeMode() != DefaultPassThroughMode {
		t.Fatal("unknown mode should default to recordOne")
	}
	if (PassThroughRule{Mode: PassThroughSkip}).NormalizeMode() != PassThroughSkip {
		t.Fatal("skip should be preserved")
	}
}

func TestPassThroughRule_UnmarshalJSON_Shorthand(t *testing.T) {
	var list []PassThroughRule
	if err := json.Unmarshal([]byte(`[8126, "4317", {"port":4318,"mode":"skip"}, {"host":"*.sentry.io","mode":"skip"}]`), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 4 {
		t.Fatalf("len=%d want 4", len(list))
	}
	if list[0].Port != 8126 || list[1].Port != 4317 {
		t.Fatalf("scalar shorthand not parsed: %+v", list[:2])
	}
	if list[2].Port != 4318 || list[2].Mode != PassThroughSkip {
		t.Fatalf("object form not parsed: %+v", list[2])
	}
	if list[3].Host != "*.sentry.io" || list[3].Mode != PassThroughSkip {
		t.Fatalf("host object not parsed: %+v", list[3])
	}
}

func TestMergePassThroughDefaults(t *testing.T) {
	merged := MergePassThroughDefaults([]PassThroughRule{{Port: 8126, Mode: PassThroughRecordOne}}, nil)
	// User rule for 8126 must win over any default on tie (it's first).
	if got, ok := MatchPassThrough(merged, "", 8126, "/v0.4/traces"); !ok || got.Mode != PassThroughRecordOne {
		t.Fatalf("user port rule should win: ok=%v mode=%q", ok, got.Mode)
	}
	// A built-in default (OTLP path) still applies with no user rule.
	if got, ok := MatchPassThrough(merged, "", 0, "/v1/traces"); !ok || got.Mode != PassThroughSkip {
		t.Fatalf("built-in OTLP default should apply: ok=%v mode=%q", ok, got.Mode)
	}
}

// The New Relic built-in must work with ZERO user config: matched by host+path,
// mode recordOne, carrying QueryKeys:[method] so the record/replay layer keys on
// ?method=. Also verifies Prometheus remote_write skips by default.
func TestBuiltinDefaults_NewRelicAndPrometheus(t *testing.T) {
	merged := MergePassThroughDefaults(nil, nil)

	nr, ok := MatchPassThrough(merged, "collector.newrelic.com", 443, "/agent_listener/invoke_raw_method")
	if !ok {
		t.Fatal("New Relic collector should match a built-in default with no user config")
	}
	if nr.Mode != PassThroughRecordOne {
		t.Fatalf("New Relic default mode = %q, want recordOne", nr.Mode)
	}
	if len(nr.QueryKeys) != 1 || nr.QueryKeys[0] != "method" {
		t.Fatalf("New Relic default QueryKeys = %v, want [method]", nr.QueryKeys)
	}

	// A user rule for the same host still overrides the built-in (skip wins).
	over := MergePassThroughDefaults(nil, []PassThroughRule{{Host: "*.newrelic.com", Mode: PassThroughSkip}})
	if got, ok := MatchPassThrough(over, "collector.newrelic.com", 443, "/agent_listener/invoke_raw_method"); !ok || got.Mode != PassThroughSkip {
		t.Fatalf("user New Relic rule should override built-in: ok=%v mode=%q", ok, got.Mode)
	}

	if got, ok := MatchPassThrough(merged, "", 0, "/api/v1/write"); !ok || got.Mode != PassThroughSkip {
		t.Fatalf("Prometheus remote_write built-in should skip: ok=%v mode=%q", ok, got.Mode)
	}
}

// A user mode:"off" rule must win over a built-in default (by specificity/order)
// so the endpoint can be recorded normally instead of skipped. MatchPassThrough
// still returns it (matched); the http decision layer maps "off" to not-passthrough.
func TestBuiltinDefaults_OffOverride(t *testing.T) {
	// Built-in skips /v1/traces; user reclaims it as an internal route.
	merged := MergePassThroughDefaults(nil, []PassThroughRule{{Path: "/v1/traces", Mode: PassThroughOff}})
	got, ok := MatchPassThrough(merged, "", 0, "/v1/traces")
	if !ok {
		t.Fatal("off rule should match (so it can override the built-in)")
	}
	if got.Mode != PassThroughOff {
		t.Fatalf("off rule should win over built-in skip: mode=%q", got.Mode)
	}
	// NormalizeMode must preserve "off" (not coerce to the recordOne default).
	if (PassThroughRule{Mode: PassThroughOff}).NormalizeMode() != PassThroughOff {
		t.Fatal("NormalizeMode must preserve off")
	}
}

// TelemetryDefaults is the single source of truth; BuiltinTelemetryDefaults must
// be exactly its rules (same order, same fields), and the version is exposed.
func TestTelemetryDefaults_SingleSource(t *testing.T) {
	defs := TelemetryDefaults()
	rules := BuiltinTelemetryDefaults()
	if len(defs) != len(rules) || len(defs) == 0 {
		t.Fatalf("defs/rules length mismatch: %d vs %d", len(defs), len(rules))
	}
	for i := range defs {
		r := defs[i].Rule
		if r.Port != rules[i].Port || r.Host != rules[i].Host || r.Path != rules[i].Path ||
			r.Mode != rules[i].Mode || len(r.QueryKeys) != len(rules[i].QueryKeys) {
			t.Fatalf("default %d: BuiltinTelemetryDefaults not derived from TelemetryDefaults", i)
		}
		if defs[i].Provider == "" || defs[i].Label == "" {
			t.Fatalf("default %d missing provider/label", i)
		}
	}
	if TelemetryDefaultsVersion < 1 {
		t.Fatal("TelemetryDefaultsVersion must be >= 1")
	}
}
