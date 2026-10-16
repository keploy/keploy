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
	if got := now.Charged(run, run, prev); !reflect.DeepEqual(got, want) {
		t.Fatalf("no rejection since the previous test: Charged = %+v, want %+v (the PID map's taint only)", got, want)
	}
	now = CaptureLosses{{Kind: CaptureLossPIDMap, Count: 2}, {Kind: CaptureLossTLSAbort, Count: 5, What: "to b:443"}}
	want = CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}, {Kind: CaptureLossTLSAbort, Count: 2, What: "to b:443"}}
	if got := now.Charged(run, run, prev); !reflect.DeepEqual(got, want) {
		t.Fatalf("two rejections since the previous test: Charged = %+v, want %+v", got, want)
	}
	// What keploy cannot capture at all (decision 20) is counted from the
	// run's base, however far back it was made, as SinceWithin counts it.
	unix := CaptureLosses{{Kind: CaptureLossUnixSocket, Count: 1}}
	if got := unix.Charged(unix, nil, unix); !reflect.DeepEqual(got, unix) {
		t.Fatalf("a Unix-socket call before the base: Charged = %+v, want %+v", got, unix)
	}
	if !CaptureLossTLSAbort.PerTest() || CaptureLossPIDMap.PerTest() || CaptureLossAppBytesLost.PerTest() {
		t.Fatal("only a TLS client's rejection is a test's own")
	}
}

// A TLS handshake a client abandoned without a word once keploy's
// certificate was out is the test's it happened in (PerTest), counted from
// the test's mark, never the run's base, and leaves it without a verdict
// (NotJudged): the reason names the destination and the CA-trust remedy.
func TestAnAbandonedHandshakeIsATestsOwnAndLeavesItNotJudged(t *testing.T) {
	k := CaptureLossTLSAbandoned
	if !k.PerTest() || !k.NotJudged() || k.runScoped() || !CaptureLossUnixSocket.runScoped() || CaptureLossTLSAbort.runScoped() {
		t.Fatal("an abandoned handshake is a test's own and not judged; a Unix socket is the run's")
	}
	at := func(n uint64) CaptureLosses { return CaptureLosses{{Kind: k, Count: n, What: "to a:443"}} }
	// Abandoned before the set's mark: not the set's, whatever the run's
	// base (a Unix-socket call before it would be).
	if got := at(2).SinceWithin(at(2), nil); len(got) != 0 {
		t.Fatalf("SinceWithin = %+v: one before the mark is not this set's", got)
	}
	if got := at(3).Charged(at(1), nil, at(2)); !reflect.DeepEqual(got, at(1)) {
		t.Fatalf("Charged = %+v, want the one since the previous test", got)
	}
	judged, notJudged := at(1).Split()
	if len(judged) != 0 || !reflect.DeepEqual(notJudged, at(1)) {
		t.Fatalf("Split = %+v, %+v: not judged, never failed", judged, notJudged)
	}
	txt := at(1).NotJudgedText()
	for _, want := range []string{"not judged: ", "hung up on 1 TLS handshake(s)", "without a word", "NODE_EXTRA_CA_CERTS", "(to a:443)"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("%q lacks %q", txt, want)
		}
	}
	if r := at(1).NotJudgedRemedy(); !strings.Contains(r, "trust keploy's CA") || strings.Contains(r, "TCP address") {
		t.Fatalf("remedy %q: trust keploy's CA, and nothing of a Unix socket", r)
	}
	if r := append(at(1), CaptureLoss{Kind: CaptureLossUnixSocket, Count: 1}).NotJudgedRemedy(); !strings.Contains(r, "trust keploy's CA") || !strings.Contains(r, "TCP address") {
		t.Fatalf("remedy %q: both", r)
	}
	if got := at(1).Plus(CaptureLosses{{Kind: k, Count: 2, What: "to b:443"}, {Kind: CaptureLossPIDMap, Count: 1}}); !reflect.DeepEqual(got, CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}, {Kind: k, Count: 3, What: "to b:443"}}) {
		t.Fatalf("Plus = %+v", got)
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

// A server-first connection keploy passed through (P250) leaves the test it
// carried bytes in not judged, that test only, and says what it was and how
// to reach it by design.
func TestAServerFirstConnectionLeavesItsTestNotJudged(t *testing.T) {
	k := CaptureLossServerFirst
	if !k.NotJudged() || !k.PerTest() || k.runScoped() {
		t.Fatalf("NotJudged %v PerTest %v runScoped %v; want a test's own loss that leaves it not judged", k.NotJudged(), k.PerTest(), k.runScoped())
	}
	before := CaptureLosses{{Kind: k, Count: 2, What: "to 10.0.0.9:25"}}
	now := CaptureLosses{{Kind: k, Count: 5, What: "to 10.0.0.9:25"}}
	charged := now.Charged(nil, nil, before)
	if len(charged) != 1 || charged[0].Count != 3 {
		t.Fatalf("charged %+v; want what passed in this test only", charged)
	}
	_, notJudged := charged.Split()
	text := notJudged.NotJudgedText()
	for _, want := range []string{"server-first protocol", "passed through for real", "sent data on them 3 time(s)", "to 10.0.0.9:25"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q does not say %q", text, want)
		}
	}
	if !strings.Contains(notJudged.NotJudgedRemedy(), "mock.real") {
		t.Fatalf("remedy %q", notJudged.NotJudgedRemedy())
	}
}

// A connection a destination refused at accept, relayed (P250), is a loss of
// its own kind: not judged, the test's own, with its text and remedy.
func TestARefusalAtAcceptIsALossOfItsOwnKind(t *testing.T) {
	k := CaptureLossRefusedAtAccept
	if !k.NotJudged() || !k.PerTest() || k.runScoped() {
		t.Fatalf("NotJudged %v PerTest %v runScoped %v", k.NotJudged(), k.PerTest(), k.runScoped())
	}
	l := CaptureLosses{{Kind: k, Count: 1, What: "to 10.0.0.9:6379"}}
	_, notJudged := l.Split()
	if text := notJudged.NotJudgedText(); !strings.Contains(text, "refused the application's connections at accept") || strings.Contains(text, "server-first") {
		t.Fatalf("text %q", text)
	}
	if remedy := notJudged.NotJudgedRemedy(); !strings.Contains(remedy, "refused") || strings.Contains(remedy, "mock.real") {
		t.Fatalf("remedy %q", remedy)
	}
}
