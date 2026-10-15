package models

import (
	"reflect"
	"strings"
	"testing"
)

func TestCaptureLossesSinceIsWhatGrew(t *testing.T) {
	base := CaptureLosses{{Kind: CaptureLossPIDMap, Count: 4}}
	if got := (CaptureLosses{{Kind: CaptureLossPIDMap, Count: 4}}).Since(base); len(got) != 0 {
		t.Fatalf("nothing grew: %+v", got)
	}
	got := (CaptureLosses{{Kind: CaptureLossPIDMap, Count: 7, What: "x"}}).Since(base)
	if len(got) != 1 || got[0].Count != 3 || got[0].What != "x" {
		t.Fatalf("grew by 3: %+v", got)
	}
	// A kind base never saw is taken whole; so is a count below base's (an
	// agent restarted under the run).
	if got := (CaptureLosses{{Kind: "other", Count: 2}}).Since(base); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("a new kind: %+v", got)
	}
	if got := (CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}}).Since(base); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("a restarted agent's count: %+v", got)
	}
	if got := CaptureLosses(nil).Since(base); len(got) != 0 {
		t.Fatalf("no counts: %+v", got)
	}
}

// A test is charged the agent-life kinds' growth since the run began (they
// taint every later test) and a test's own kinds' growth since the previous
// test ended (only the test they happened in).
func TestChargedTaintsWithTheRunsKindsAndKeepsATestsOwnToIt(t *testing.T) {
	run := CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}}
	prev := CaptureLosses{{Kind: CaptureLossPIDMap, Count: 2}, {Kind: CaptureLossTLSAbort, Count: 3, What: "to a:443"}}
	now := CaptureLosses{{Kind: CaptureLossPIDMap, Count: 2}, {Kind: CaptureLossTLSAbort, Count: 3, What: "to a:443"}}
	want := CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}}
	if got := now.Charged(run, prev); !reflect.DeepEqual(got, want) {
		t.Fatalf("no rejection since the previous test: Charged = %+v, want %+v (the PID map's taint only)", got, want)
	}
	now = CaptureLosses{{Kind: CaptureLossPIDMap, Count: 2}, {Kind: CaptureLossTLSAbort, Count: 5, What: "to b:443"}}
	want = CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}, {Kind: CaptureLossTLSAbort, Count: 2, What: "to b:443"}}
	if got := now.Charged(run, prev); !reflect.DeepEqual(got, want) {
		t.Fatalf("two rejections since the previous test: Charged = %+v, want %+v", got, want)
	}
	if !CaptureLossTLSAbort.PerTest() || CaptureLossPIDMap.PerTest() || CaptureLossAppBytesLost.PerTest() {
		t.Fatal("only a TLS client's rejection is a test's own")
	}
}

func TestCaptureLossesTextSaysWhatWasNotCaptured(t *testing.T) {
	if CaptureLosses(nil).Text() != "" {
		t.Fatal("no losses, no text")
	}
	s := (CaptureLosses{{Kind: CaptureLossPIDMap, Count: 2, What: "target_namespace_pids"}}).Text()
	for _, want := range []string{"capture incomplete: ", "refused 2 insert(s)", "(target_namespace_pids)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("%q lacks %q", s, want)
		}
	}
}

func TestScopeEvidenceWithACaptureLossIsNotEmpty(t *testing.T) {
	if !(&ScopeEvidence{}).Empty() {
		t.Fatal("empty evidence")
	}
	if (&ScopeEvidence{CaptureIncomplete: CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}}}).Empty() {
		t.Fatal("a capture loss is evidence")
	}
}

// A capture loss names what it was by keploy's own words and a destination:
// the destinations keploy gives stay as they are, and a word that carries a
// path, a query or credentials is written in its shape, so no value the app
// sent reaches a report.
func TestCaptureLossesTextShapesWhatItNames(t *testing.T) {
	// What the producers give (the proxy's TLS and relay counters, the
	// agent's PID-map causes), byte for byte.
	pidMap := "target_namespace_pids, " + PIDMapRefusalCause(-7)
	kept := CaptureLosses{
		{Kind: CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.5:443"},
		{Kind: CaptureLossDropped, Count: 2, What: "per_conn_cap on a connection to 10.0.0.9:5432"},
		{Kind: CaptureLossPIDMap, Count: 3, What: pidMap},
	}.Text()
	for _, want := range []string{"(to 10.0.0.5:443)", "(per_conn_cap on a connection to 10.0.0.9:5432)", "(" + pidMap + ")"} {
		if !strings.Contains(kept, want) {
			t.Fatalf("text %q does not keep %q", kept, want)
		}
	}
	shaped := CaptureLosses{{Kind: CaptureLossDropped, Count: 1,
		What: "per_conn_cap on a connection to user:pw@api.internal:8080/users/alice@example.com/orders/12345?token=s3cret"}}.Text()
	if !strings.Contains(shaped, "(per_conn_cap on a connection to {}/users/{}/orders/{}?token={})") {
		t.Fatalf("text %q", shaped)
	}
	for _, v := range []string{"alice", "12345", "s3cret", "pw@"} {
		if strings.Contains(shaped, v) {
			t.Fatalf("text %q holds %q", shaped, v)
		}
	}
}

// Bytes written to a held connection that never reached the proxy are said in
// their own words, with their count, never as an unnamed kind.
func TestCaptureLossesTextSaysAppBytesLost(t *testing.T) {
	s := (CaptureLosses{{Kind: CaptureLossAppBytesLost, Count: 3, What: "from 127.0.0.1:40000"}}).Text()
	for _, want := range []string{"capture incomplete: ", "on 3 connection(s)", "never reached the proxy", "(from 127.0.0.1:40000)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("%q lacks %q", s, want)
		}
	}
	if strings.Contains(s, "of kind") {
		t.Fatalf("%q names the kind without saying what it is", s)
	}
}
