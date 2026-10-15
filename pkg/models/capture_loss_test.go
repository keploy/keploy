package models

import (
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

// A call keploy cannot capture at all (decision 20: a Unix socket) leaves a
// test without a verdict; every other loss fails it. And it is counted from
// the run's base, not the test set's mark: the connection outlives both.
func TestNotJudgedLossesAreSplitAndCountedFromTheRun(t *testing.T) {
	unixAt := func(n uint64) CaptureLoss {
		return CaptureLoss{Kind: CaptureLossUnixSocket, Count: n, What: "/tmp/db.sock"}
	}
	tls := CaptureLoss{Kind: CaptureLossTLSAbort, Count: 2}
	judged, notJudged := CaptureLosses{tls, unixAt(1)}.Split()
	if len(judged) != 1 || judged[0] != tls || len(notJudged) != 1 || notJudged[0] != unixAt(1) {
		t.Fatalf("Split = %+v, %+v", judged, notJudged)
	}
	if !CaptureLossUnixSocket.NotJudged() || CaptureLossTLSAbort.NotJudged() || CaptureLossPIDMap.NotJudged() {
		t.Fatal("only a Unix socket is not judged")
	}
	text := CaptureLosses{unixAt(3)}.NotJudgedText()
	for _, w := range []string{"not judged:", "Unix socket keploy cannot capture", "/tmp/db.sock", "cannot prove this test's calls were all captured"} {
		if !strings.Contains(text, w) {
			t.Errorf("%q does not say %q", text, w)
		}
	}
	if strings.Contains(text, "capture incomplete") {
		t.Errorf("%q calls it a capture loss", text)
	}
	// The set's mark is after the connect; the run's base before it.
	now := CaptureLosses{tls, unixAt(1)}
	mark := CaptureLosses{tls, unixAt(1)}
	got := now.SinceWithin(mark, nil)
	if len(got) != 1 || got[0].Kind != CaptureLossUnixSocket {
		t.Fatalf("SinceWithin = %+v, want the Unix socket only, from the run's base", got)
	}
	if got := now.SinceWithin(mark, CaptureLosses{unixAt(1)}); len(got) != 0 {
		t.Fatalf("SinceWithin with the connect before the run's base = %+v, want nothing", got)
	}
}

// Unix sockets keploy cannot see (a report that could not load, or stopped)
// are a standing loss that leaves every test from then on NOT JUDGED, as a
// call over one does: no mark takes it away, and it is no failure.
func TestUnixSocketsKeployCannotSeeAreAStandingNotJudgedLoss(t *testing.T) {
	unseen := CaptureLoss{Kind: CaptureLossUnixUnseen, Count: 1, What: "no trampoline"}
	if !unseen.Kind.NotJudged() {
		t.Fatal("unix_unseen fails tests; want it to leave them not judged")
	}
	now := CaptureLosses{unseen}
	if got := now.Since(now); len(got) != 1 || got[0] != unseen {
		t.Fatalf("Since(itself) = %+v, want unix_unseen kept: a state, no mark takes it away", got)
	}
	if got := now.SinceWithin(now, now); len(got) != 1 || got[0].Kind != CaptureLossUnixUnseen {
		t.Fatalf("SinceWithin = %+v, want unix_unseen", got)
	}
	judged, notJudged := now.Split()
	if len(judged) != 0 || len(notJudged) != 1 {
		t.Fatalf("Split = %+v, %+v; want it not judged", judged, notJudged)
	}
	if text := notJudged.NotJudgedText(); !strings.Contains(text, "cannot see the application's calls over Unix sockets") || !strings.Contains(text, "no trampoline") {
		t.Fatalf("NotJudgedText = %q", text)
	}
}
