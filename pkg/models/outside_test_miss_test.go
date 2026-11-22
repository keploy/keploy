package models

import (
	"strings"
	"testing"
)

// The set's report names each outside-test miss by its shape, a host by its
// shape beside its host tag and an IP address whole, once with its count,
// with keploy's next step; an answered call and a connection that sent
// nothing are no miss.
func TestOutsideTestMissesAreNamedByShapeOncePerCall(t *testing.T) {
	calls := []UnmatchedCall{
		{Protocol: "DNS", ActualSummary: "A db.tenant-a.example.", Command: "A", HostTag: "#3fa2c1d0e9b8", BeforeSet: true},
		{Protocol: "HTTP", ActualSummary: "GET /v1/tenants/acme/config", Destination: "config.tenant-a.example:443", HostTag: "#77aa", PathTag: "[path tag #1f2e]", BeforeSet: true},
		{Protocol: "DNS", ActualSummary: "A db.tenant-a.example.", Command: "A", HostTag: "#3fa2c1d0e9b8", BeforeSet: true},
		{Protocol: "HTTP", ActualSummary: "POST /hook", Destination: "10.0.0.7:8080", Held: true},
		{Protocol: "HTTP", ActualSummary: "GET /late", Destination: "10.0.0.7:8080"},
		{Protocol: "HTTP", ActualSummary: "GET /answered", Destination: "10.0.0.7:8080", Answer: "by_kind", BeforeSet: true},
		{Protocol: "Postgres", MatchPhase: MatchPhaseNoRequest, BeforeSet: true},
	}
	got := OutsideTestMissesOf(calls)
	if len(got) != 4 {
		t.Fatalf("got %d misses, want 4 (the DNS lookup once, the config call, the held call, the late call): %+v", len(got), got)
	}
	dns := got[0]
	if dns.Protocol != "DNS" || dns.Call != "A {}.{}.{}." || dns.Destination != "{}.{}.{}. #3fa2c1d0e9b8" || dns.When != OutsideBeforeFirstTest || dns.Count != 2 || dns.NextStep != outsideNextDNS {
		t.Errorf("the DNS lookup reads %+v", dns)
	}
	cfg := got[1]
	if cfg.Call != "GET /{}/{}/{}/{}" || cfg.PathTag != "[path tag #1f2e]" || cfg.Destination != "{}.{}.{}:443 #77aa" || cfg.When != OutsideBeforeFirstTest || cfg.Count != 1 || cfg.NextStep != outsideNextCall {
		t.Errorf("the config call reads %+v", cfg)
	}
	if got[2].When != OutsideHeld || got[2].Destination != "10.0.0.7:8080" {
		t.Errorf("the held call reads %+v; want it held, its IP whole", got[2])
	}
	if got[3].When != OutsideAfterLastTest {
		t.Errorf("the late call reads %+v; want it after the last test", got[3])
	}
	for _, m := range got {
		for _, value := range []string{"tenant-a", "acme", "config.", "db."} {
			if strings.Contains(m.Call+m.Destination, value) {
				t.Errorf("%+v names %q, a value the call sent", m, value)
			}
		}
	}
	if OutsideTestMissesOf(nil) != nil {
		t.Error("no call is no miss")
	}
}

// A DNS summary of one word is the name, its type the call's command.
func TestAnOutsideDNSMissWithAOneWordSummaryIsItsName(t *testing.T) {
	got := OutsideTestMissesOf([]UnmatchedCall{{Protocol: "dns", ActualSummary: "db.example.", Command: "AAAA", BeforeSet: true}})
	if len(got) != 1 || got[0].Call != "AAAA {}.{}." || got[0].Destination != "{}.{}." {
		t.Fatalf("got %+v", got)
	}
}
