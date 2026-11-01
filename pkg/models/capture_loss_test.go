package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
	if got := now.Charged(SameBases(run), prev); !reflect.DeepEqual(got, want) {
		t.Fatalf("no rejection since the previous test: Charged = %+v, want %+v (the PID map's taint only)", got, want)
	}
	now = CaptureLosses{{Kind: CaptureLossPIDMap, Count: 2}, {Kind: CaptureLossTLSAbort, Count: 5, What: "to b:443"}}
	want = CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}, {Kind: CaptureLossTLSAbort, Count: 2, What: "to b:443"}}
	if got := now.Charged(SameBases(run), prev); !reflect.DeepEqual(got, want) {
		t.Fatalf("two rejections since the previous test: Charged = %+v, want %+v", got, want)
	}
	// What keploy cannot capture at all (decision 20) is counted from the
	// run's base, however far back it was made, as SinceWithin counts it.
	unix := CaptureLosses{{Kind: CaptureLossUnixSocket, Count: 1}}
	if got := unix.Charged(LossBases{Set: unix, App: unix}, unix); !reflect.DeepEqual(got, unix) {
		t.Fatalf("a Unix-socket call before the base: Charged = %+v, want %+v", got, unix)
	}
	if !CaptureLossTLSAbort.PerTest() || CaptureLossPIDMap.PerTest() {
		t.Fatal("a TLS client's rejection is a test's own, a process keploy could not place is not")
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
	if got := at(2).SinceWithin(LossBases{Set: at(2), App: at(2)}); len(got) != 0 {
		t.Fatalf("SinceWithin = %+v: one before the mark is not this set's", got)
	}
	if got := at(3).Charged(LossBases{Set: at(1), App: at(1)}, at(2)); !reflect.DeepEqual(got, at(1)) {
		t.Fatalf("Charged = %+v, want the one since the previous test", got)
	}
	judged, notJudged := at(1).Split()
	if len(judged) != 0 || !reflect.DeepEqual(notJudged, at(1)) {
		t.Fatalf("Split = %+v, %+v: not judged, never failed", judged, notJudged)
	}
	txt := at(1).NotJudgedText()
	for _, want := range []string{"not judged: ", "hung up on 1 TLS handshake(s)", "without a word", "NODE_EXTRA_CA_CERTS", "--cacert", "CURLOPT_CAINFO", "(to a:443)"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("%q lacks %q", txt, want)
		}
	}
	if r := at(1).NotJudgedRemedy(); !strings.Contains(r, "trust keploy's CA") || !strings.Contains(r, "NODE_EXTRA_CA_CERTS") || !strings.Contains(r, "--cacert") || !strings.Contains(r, "CURLOPT_CAINFO") || strings.Contains(r, "TCP address") {
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
// Bytes lost with a connection that ended are that connection's, charged to
// one test: the test the count grew in, never a later one. The same counts,
// with the test's mark past them, charge nothing, and a kind whose later calls
// may go with it still taints.
func TestAppBytesLostChargeOneTest(t *testing.T) {
	if !CaptureLossAppBytesLost.PerTest() {
		t.Fatal("app_bytes_lost is not a test's own")
	}
	lost := CaptureLosses{{Kind: CaptureLossAppBytesLost, Count: 1, What: "from 127.0.0.1:40000"}}
	if got := lost.Charged(LossBases{}, nil); !reflect.DeepEqual(got, lost) {
		t.Fatalf("the test the loss landed in: Charged = %+v, want %+v", got, lost)
	}
	if got := lost.Charged(LossBases{}, lost); len(got) != 0 {
		t.Fatalf("the test after it: Charged = %+v, want nothing", got)
	}
	dropped := CaptureLosses{{Kind: CaptureLossDropped, Count: 1}}
	if got := dropped.Charged(LossBases{}, dropped); !reflect.DeepEqual(got, dropped) {
		t.Fatalf("a kind that taints, the test after it: Charged = %+v, want %+v", got, dropped)
	}
}

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
	got := now.SinceWithin(LossBases{Set: mark, App: mark})
	if len(got) != 1 || got[0].Kind != CaptureLossUnixSocket {
		t.Fatalf("SinceWithin = %+v, want the Unix socket only, from the run's base", got)
	}
	if got := now.SinceWithin(LossBases{Set: mark, App: mark, Run: CaptureLosses{unixAt(1)}, Capture: CaptureLosses{unixAt(1)}}); len(got) != 0 {
		t.Fatalf("SinceWithin with the connect before the run's base = %+v, want nothing", got)
	}
}

// Unix sockets keploy cannot see (a report that loaded, and stopped) are a
// standing loss that leaves every test from then on NOT JUDGED, as a call
// over one does: no mark takes it away, and it is no failure.
func TestUnixSocketsKeployCannotSeeAreAStandingNotJudgedLoss(t *testing.T) {
	unseen := CaptureLoss{Kind: CaptureLossUnixUnseen, Count: 1, What: "keploy's report stopped: ring broken"}
	if !unseen.Kind.NotJudged() {
		t.Fatal("unix_unseen fails tests; want it to leave them not judged")
	}
	now := CaptureLosses{unseen}
	if got := now.Since(now); len(got) != 1 || got[0] != unseen {
		t.Fatalf("Since(itself) = %+v, want unix_unseen kept: a state, no mark takes it away", got)
	}
	if got := now.SinceWithin(SameBases(now)); len(got) != 1 || got[0].Kind != CaptureLossUnixUnseen {
		t.Fatalf("SinceWithin = %+v, want unix_unseen", got)
	}
	judged, notJudged := now.Split()
	if len(judged) != 0 || len(notJudged) != 1 {
		t.Fatalf("Split = %+v, %+v; want it not judged", judged, notJudged)
	}
	if text := notJudged.NotJudgedText(); !strings.Contains(text, "report of the application's calls over Unix sockets stopped") || !strings.Contains(text, "ring broken") {
		t.Fatalf("NotJudgedText = %q", text)
	}
	// Its remedy is keploy's report's: reaching the dependencies over TCP
	// leaves every test not judged all the same.
	if r := notJudged.NotJudgedRemedy(); !strings.Contains(r, "report the reason to keploy") || strings.Contains(r, "TCP") {
		t.Fatalf("remedy %q, want the report's own, never a move to TCP", r)
	}
	if r := append(notJudged, CaptureLoss{Kind: CaptureLossUnixSocket, Count: 1}).NotJudgedRemedy(); !strings.Contains(r, "reach that dependency over a TCP address") || !strings.Contains(r, UnixUnseenRemedy) {
		t.Fatalf("remedy %q, want both: a Unix socket's call seen, and the report's", r)
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
	charged := now.Charged(SameBases(nil), before)
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

// A connection attempt the kernel dropped at keploy's proxy listener while a
// test ran is that test's own loss, and leaves it, and no other, not judged:
// the text says a call may not have reached keploy and what keploy cannot
// tell, the remedy where to look, and a test after it is charged nothing.
func TestAListenerDropLeavesOnlyItsTestNotJudged(t *testing.T) {
	k := CaptureLossListenerDropped
	if !k.PerTest() || !k.NotJudged() || k.runScoped() || k.AppLife() || k.Evidence() {
		t.Fatal("a listener's drop is a test's own and not judged")
	}
	at := func(n uint64) CaptureLosses { return CaptureLosses{{Kind: k, Count: n}} }
	if got := at(3).Charged(SameBases(nil), at(2)); !reflect.DeepEqual(got, at(1)) {
		t.Fatalf("Charged = %+v, want the one since the previous test's end", got)
	}
	if got := at(3).Charged(SameBases(nil), at(3)); len(got) != 0 {
		t.Fatalf("Charged = %+v: a later test is charged nothing", got)
	}
	judged, notJudged := at(1).Split()
	if len(judged) != 0 || !reflect.DeepEqual(notJudged, at(1)) {
		t.Fatalf("Split = %+v, %+v: not judged, never failed", judged, notJudged)
	}
	txt := notJudged.NotJudgedText()
	for _, want := range []string{"not judged: the kernel dropped 1 connection attempt(s) at keploy's proxy listener (its SYN or accept queue full)", "so a call may not have reached keploy", "keploy cannot tell that from one that gave up", "to the test it fell in (in keploy test, to the test case that ended before it, until that test case's misses were read), otherwise to the next test, but to the run when it fell before a test process claimed the run", "one exception: in keploy test, a drop before a test set's first test case is that test case's, where a missed call would be the set's"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("%q lacks %q", txt, want)
		}
	}
	if remedy := notJudged.NotJudgedRemedy(); remedy != ListenerDroppedRemedy {
		t.Fatalf("remedy %q", remedy)
	}
}

// A use of a container whose configuration's read the Docker daemon did not
// answer is the test's own loss, and leaves it not judged: the text names
// the container and says the daemon did not answer, and the remedy is to
// make its inspect answer.
func TestAContainerReadTheDaemonDidNotAnswerLeavesItsTestNotJudged(t *testing.T) {
	k := CaptureLossContainerUnread
	if !k.PerTest() || !k.NotJudged() || k.runScoped() {
		t.Fatal("an unread container is a test's own and not judged")
	}
	at := func(n uint64) CaptureLosses {
		return CaptureLosses{{Kind: k, Count: n, What: "container pg (postgres:16)"}}
	}
	if got := at(3).Charged(LossBases{Set: at(1), App: at(1)}, at(2)); !reflect.DeepEqual(got, at(1)) {
		t.Fatalf("Charged = %+v, want the one since the previous test", got)
	}
	judged, notJudged := at(1).Split()
	if len(judged) != 0 || !reflect.DeepEqual(notJudged, at(1)) {
		t.Fatalf("Split = %+v, %+v: not judged, never failed", judged, notJudged)
	}
	txt := at(1).NotJudgedText()
	for _, want := range []string{"not judged: ", "did not answer keploy's read of a container's configuration (1 time(s)", "(container pg (postgres:16))"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("%q lacks %q", txt, want)
		}
	}
	if r := at(1).NotJudgedRemedy(); !strings.Contains(r, "docker inspect") || strings.Contains(r, "trust keploy's CA") {
		t.Fatalf("remedy %q: make the container's inspect answer, and nothing of TLS", r)
	}
}

// A run of keploy's programs the kernel skipped is evidence only, never a
// verdict (the founder's rule, 2026-10-21): the count is the kernel's
// recursion_misses (5.12+), which 5.10 does not have, so it fails no test
// and leaves none not judged. Split takes it into neither; Evidence takes
// it. A skipped connection report covers every later test (counted from the
// run's base); a skipped call's run is its test's own. Their text says the
// kernel skipped during the recording.
func TestAKernelSkipIsEvidenceNotAVerdict(t *testing.T) {
	k, call := CaptureLossKernelSkip, CaptureLossKernelSkipCall
	for _, kind := range []CaptureLossKind{k, call} {
		if !kind.Evidence() || kind.NotJudged() {
			t.Fatalf("%s: Evidence %v, NotJudged %v; want evidence only, no verdict", kind, kind.Evidence(), kind.NotJudged())
		}
	}
	at := func(k CaptureLossKind, n uint64) CaptureLosses {
		return CaptureLosses{{Kind: k, Count: n, What: "keploy_tcp_recvmsg"}}
	}
	both := at(k, 1).Plus(at(call, 2))
	if judged, notJudged := both.Split(); len(judged) != 0 || len(notJudged) != 0 {
		t.Fatalf("Split = %+v, %+v; want neither: evidence is no verdict", judged, notJudged)
	}
	if got := both.Evidence(); !reflect.DeepEqual(got, both) {
		t.Fatalf("Evidence = %+v, want both", got)
	}
	if k.PerTest() || !k.runScoped() {
		t.Fatal("a skipped connection report covers every later test")
	}
	// Counted from the run's base: a test after the skip still carries it.
	if got := at(k, 3).Charged(LossBases{Set: at(k, 3), App: at(k, 3), Run: at(k, 1), Capture: at(k, 1)}, at(k, 3)); !reflect.DeepEqual(got, at(k, 2)) {
		t.Fatalf("Charged = %+v, want what grew since the run began", got)
	}
	if !call.PerTest() || call.runScoped() {
		t.Fatal("a skipped call's run is its test's own")
	}
	if got := at(call, 3).Charged(LossBases{Set: at(call, 1), App: at(call, 1)}, at(call, 2)); !reflect.DeepEqual(got, at(call, 1)) {
		t.Fatalf("Charged = %+v, want the one since the previous test", got)
	}
	for _, l := range []CaptureLosses{at(k, 1), at(call, 1)} {
		txt := l.EvidenceText()
		for _, want := range []string{"during its recording the kernel skipped 1 capture run(s) of keploy's programs", "(keploy_tcp_recvmsg)"} {
			if !strings.Contains(txt, want) {
				t.Fatalf("%q lacks %q", txt, want)
			}
		}
		if l.NotJudgedText() != "" || l.NotJudgedRemedy() != "" {
			t.Fatalf("not-judged text %q, remedy %q, want none", l.NotJudgedText(), l.NotJudgedRemedy())
		}
	}
}

// A process keploy could not classify lives on: every test from it on is
// not judged (run-scoped), never failed.
func TestAProcessKeployCouldNotClassifyLeavesTheTestsFromItNotJudged(t *testing.T) {
	k := CaptureLossUnclassified
	if k.PerTest() || !k.NotJudged() || !k.runScoped() {
		t.Fatal("an unclassified process taints the run's later tests, not judged")
	}
	l := CaptureLosses{{Kind: k, Count: 1, What: "the latest TGID 77"}}
	if txt := l.NotJudgedText(); !strings.Contains(txt, "could not tell 1 time(s) whether a process was the application's") {
		t.Fatalf("text %q", txt)
	}
	if r := l.NotJudgedRemedy(); !strings.Contains(r, "report it to keploy") || strings.Contains(r, "preempt") {
		t.Fatalf("remedy %q", r)
	}
	if judged, notJudged := l.Split(); len(judged) != 0 || len(notJudged) != 1 {
		t.Fatalf("Split = %v, %v", judged, notJudged)
	}
}

// What a downstream build's capture lost fails the tests it taints (judged,
// never evidence): a piece it saw and could not read, or a connection it
// stopped recording, the test it happened in only (PerTest); an app process
// it could not mark, which lives on, that test and every later one, from the
// running application's start (agent-life, AppLife). The text says which,
// and an untracked process's says what to do.
func TestWhatADownstreamCaptureLostFailsTheTestsItTaints(t *testing.T) {
	for _, tc := range []struct {
		k       CaptureLossKind
		text    string
		perTest bool
		appLife bool
	}{
		{CaptureLossUnread, "could not read 2 piece(s) of what the application's connections carried", true, false},
		{CaptureLossUnrecorded, "stopped recording a connection of the application's mid-stream (a capture hole its parser could not re-align across, or a parser that failed), and 2 piece(s)", true, false},
		{CaptureLossAppSubtreeUntracked, "keploy could not mark 2 process(es) forked in the application's subtree as the application's (its map of the subtree was full, or it could not read the fork's start time), so their calls may have been neither recorded nor mocked while this test ran, or before it; report it to keploy with the agent's log, which names the process", false, true},
	} {
		if tc.k.NotJudged() || tc.k.Evidence() || tc.k.PerTest() != tc.perTest || tc.k.AppLife() != tc.appLife || tc.k.runScoped() {
			t.Fatalf("%s: NotJudged %v, Evidence %v, PerTest %v, AppLife %v; want a judged loss, PerTest %v, AppLife %v",
				tc.k, tc.k.NotJudged(), tc.k.Evidence(), tc.k.PerTest(), tc.k.AppLife(), tc.perTest, tc.appLife)
		}
		at := func(n uint64) CaptureLosses { return CaptureLosses{{Kind: tc.k, Count: n}} }
		// Now 5; the running application started at 1, the set at 2, and
		// the previous test ended at 3: a test's own kind is charged the 2
		// since that test, an application-life kind the 4 since its
		// application started.
		want := at(4)
		if tc.perTest {
			want = at(2)
		}
		if got := at(5).Charged(LossBases{Set: at(2), App: at(1)}, at(3)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: Charged = %+v, want %+v", tc.k, got, want)
		}
		if judged, notJudged := at(2).Split(); len(judged) != 1 || len(notJudged) != 0 {
			t.Fatalf("%s: Split = %v, %v: judged", tc.k, judged, notJudged)
		}
		if txt := at(2).Text(); !strings.Contains(txt, tc.text) {
			t.Fatalf("%s: text %q lacks %q", tc.k, txt, tc.text)
		}
	}
}

// Each kind is charged from the base its loss lasts from: a test's own from
// the previous test's end; a process of the application's keploy could not
// place (AppLife) from the running application's start; one that outlives
// the application (one keploy could not exclude: it predates the recording)
// from the run's start; what keploy cannot capture at all, a process it
// could not classify among it, from that base, however far back; every
// other kind from the set's.
func TestEachKindIsChargedFromTheBaseItsLossLastsFrom(t *testing.T) {
	// Now 9; the hooks loaded at 0 (what keploy cannot capture is counted
	// from there), the run began at 1, the running application started at 3,
	// the set at 5, and the previous test ended at 7.
	for _, tc := range []struct {
		k    CaptureLossKind
		want uint64
	}{
		{CaptureLossTLSAbort, 2},
		{CaptureLossParserStall, 4},
		{CaptureLossPIDMap, 6},
		{CaptureLossAppSubtreeUntracked, 6},
		{CaptureLossPIDMapExcluded, 8},
		{CaptureLossUnclassified, 9},
	} {
		at := func(n uint64) CaptureLosses { return CaptureLosses{{Kind: tc.k, Count: n}} }
		if got := at(9).Charged(LossBases{Set: at(5), App: at(3), Run: at(1), Capture: nil}, at(7)); !reflect.DeepEqual(got, at(tc.want)) {
			t.Errorf("%s: Charged = %+v, want %d", tc.k, got, tc.want)
		}
	}
	for _, k := range []CaptureLossKind{CaptureLossPIDMap, CaptureLossAppSubtreeUntracked} {
		if !k.AppLife() || k.runLife() || k.runScoped() || k.PerTest() {
			t.Errorf("%s: AppLife %v, runLife %v, runScoped %v, PerTest %v; want application-life only", k, k.AppLife(), k.runLife(), k.runScoped(), k.PerTest())
		}
	}
	if k := CaptureLossPIDMapExcluded; !k.runLife() || k.AppLife() || k.runScoped() || k.NotJudged() {
		t.Errorf("%s: want judged and run-life only", k)
	}
}

// A kind this build does not know (a newer agent's) is kept whole when what
// carries it is read and written again, in YAML and in JSON, and it is judged
// and agent-life: an older reader fails every test from it on rather than
// drop the loss or pass them.
func TestAKindThisBuildDoesNotKnowIsKeptAndJudged(t *testing.T) {
	newer := CaptureLossKind("a_kind_from_a_newer_build")
	losses := CaptureLosses{{Kind: newer, Count: 2, What: "the latest: pid 7 (app)"}}
	for _, codec := range []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{
		{"yaml", yaml.Marshal, yaml.Unmarshal},
		{"json", json.Marshal, json.Unmarshal},
	} {
		b, err := codec.marshal(FailureInfo{CaptureIncomplete: losses})
		if err != nil {
			t.Fatalf("%s: write: %v", codec.name, err)
		}
		var back FailureInfo
		if err := codec.unmarshal(b, &back); err != nil {
			t.Fatalf("%s: read: %v", codec.name, err)
		}
		again, err := codec.marshal(back)
		if err != nil {
			t.Fatalf("%s: write again: %v", codec.name, err)
		}
		var twice FailureInfo
		if err := codec.unmarshal(again, &twice); err != nil {
			t.Fatalf("%s: read again: %v", codec.name, err)
		}
		if !reflect.DeepEqual(twice.CaptureIncomplete, losses) {
			t.Fatalf("%s: read and written twice: %+v, want %+v", codec.name, twice.CaptureIncomplete, losses)
		}
	}
	if newer.NotJudged() || newer.Evidence() {
		t.Fatal("an unknown kind is not judged, or is evidence: an older reader would pass a test it taints")
	}
	if judged, _ := losses.Split(); len(judged) != 1 {
		t.Fatalf("Split = %v: an unknown kind must be judged", judged)
	}
	// And it is agent-life: charged from the set's base, so it taints the
	// test it happened in and every later one.
	at := func(n uint64) CaptureLosses { return CaptureLosses{{Kind: newer, Count: n}} }
	if newer.PerTest() {
		t.Fatal("an unknown kind is a test's own: an older reader would charge it to one test only")
	}
	if got := at(5).Charged(LossBases{Set: at(1), App: at(1)}, at(3)); !reflect.DeepEqual(got, at(4)) {
		t.Fatalf("Charged = %+v, want the 4 since the set's base", got)
	}
}

// An agent started again while the application ran on (the replay's own
// charge, never the agent's) leaves the tests it is charged to without a
// verdict, and says why and what to do.
func TestAnAgentStartedAgainLeavesTheTestsWithoutAVerdict(t *testing.T) {
	k := CaptureLossAgentStartedAgain
	if !k.NotJudged() || k.PerTest() || k.Evidence() || k.AppLife() {
		t.Fatalf("%s: NotJudged %v, PerTest %v, Evidence %v, AppLife %v; want not judged only", k, k.NotJudged(), k.PerTest(), k.Evidence(), k.AppLife())
	}
	l := CaptureLosses{{Kind: k, Count: 1, What: "agent process a1, then b2"}}
	judged, notJudged := l.Split()
	if len(judged) != 0 || len(notJudged) != 1 {
		t.Fatalf("Split = %v, %v: not judged", judged, notJudged)
	}
	if text := l.NotJudgedText(); !strings.Contains(text, "keploy's agent was started again while the application ran on") || !strings.Contains(text, "(agent process a1, then b2)") {
		t.Fatalf("NotJudgedText = %q", text)
	}
	if remedy := l.NotJudgedRemedy(); remedy != AgentStartedAgainRemedy {
		t.Fatalf("NotJudgedRemedy = %q, want %q", remedy, AgentStartedAgainRemedy)
	}
}
