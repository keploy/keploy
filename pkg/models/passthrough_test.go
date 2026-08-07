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
