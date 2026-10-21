package record

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// countingInstr is an Instrumentation whose agent counts capture losses: each
// read answers the next of answers (the last repeating), or errAt[n] for the
// nth read (from 1).
type countingInstr struct {
	Instrumentation
	answers []models.CaptureLosses
	errAt   map[int]error
	reads   int
}

func (c *countingInstr) GetCaptureLosses(context.Context) (models.CaptureLosses, error) {
	c.reads++
	if err := c.errAt[c.reads]; err != nil {
		return nil, err
	}
	i := c.reads - 1
	if i >= len(c.answers) {
		i = len(c.answers) - 1
	}
	return c.answers[i], nil
}

func pid(n uint64) models.CaptureLosses {
	return models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: n}}
}

func record3(t *testing.T, instr Instrumentation) (*captureWatch, bool, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	return w, w.finish("test-set-0", false), logs
}

// Under the proven switch the test case being recorded at a loss and every
// later one are named, and the recording fails; the one before is not.
func TestARecordingThatLostACaptureNamesTheTestCasesFromTheLossOn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	instr := &countingInstr{answers: []models.CaptureLosses{pid(1), pid(1), pid(4)}}
	w, failed, logs := record3(t, instr)
	if !failed || w.String() != "tainted=[test-2,test-3] unverified=[]" {
		t.Fatalf("failed %v, %s; want test-2 and test-3 tainted and the recording failed", failed, w)
	}
	errs := logs.FilterMessageSnippet("the recording is incomplete").All()
	if len(errs) != 1 {
		t.Fatalf("%d 'recording is incomplete' ERRORs, want 1", len(errs))
	}
	if r := fmt.Sprint(errs[0].ContextMap()["reason"]); r == "" {
		t.Fatal("the ERROR gives no reason")
	}
}

// A TLS client's rejection of keploy's certificate is the test case's it
// happened in, not a taint of every later one: only the test cases recorded
// while one happened are named. It named every test case from the first on.
func TestARecordingNamesOnlyTheTestCasesATLSRejectionHappenedIn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tls := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: n, What: "to 10.0.0.1:443"}}
	}
	// The base, then each test case's check.
	instr := &countingInstr{answers: []models.CaptureLosses{nil, tls(1), tls(1), tls(2)}}
	w, failed, _ := record3(t, instr)
	if !failed || w.String() != "tainted=[test-1,test-3] unverified=[]" {
		t.Fatalf("failed %v, %s; want test-1 and test-3 tainted, not test-2, and the recording failed", failed, w)
	}
}

// The recording's ERROR gives every loss as its reason, not only the last
// tainted test case's own: test-1's failed handshake is still why the
// recording is incomplete when test-3 is named for a PID-map refusal.
func TestARecordingsReasonNamesEveryLossNotOnlyTheLast(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tls := models.CaptureLoss{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}
	both := models.CaptureLosses{pid(1)[0], tls}
	instr := &countingInstr{answers: []models.CaptureLosses{nil, {tls}, {tls}, both}}
	w, failed, logs := record3(t, instr)
	if !failed || w.String() != "tainted=[test-1,test-3] unverified=[]" {
		t.Fatalf("failed %v, %s; want test-1 and test-3 tainted and the recording failed", failed, w)
	}
	errs := logs.FilterMessageSnippet("the recording is incomplete").All()
	if len(errs) != 1 {
		t.Fatalf("%d 'recording is incomplete' ERRORs, want 1", len(errs))
	}
	if r := fmt.Sprint(errs[0].ContextMap()["reason"]); r != both.Text() {
		t.Fatalf("the ERROR's reason %q, want every loss: %q", r, both.Text())
	}
}

// The final read charges no test case with what each test case's check
// already saw: two test cases' own failed handshakes are both charged, so
// nothing is left over for "after the last test case".
func TestARecordingsFinalReadCountsEachTestCasesOwnHandshakes(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tls := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: n, What: "to 10.0.0.1:443"}}
	}
	// The base, each test case's check, then the final read.
	instr := &countingInstr{answers: []models.CaptureLosses{nil, tls(1), tls(1), tls(2), tls(2)}}
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	w.afterTestCases(context.Background())
	if !w.finish("test-set-0", false) || w.String() != "tainted=[test-1,test-3] unverified=[]" {
		t.Fatalf("%s; want test-1 and test-3 tainted and the recording failed", w)
	}
	if n := len(logs.FilterMessageSnippet("after the last test case").All()); n != 0 {
		t.Fatalf("%d 'after the last test case' ERRORs, want none: every handshake was charged to a test case", n)
	}
}

// A TLS handshake a client abandoned without a word is said for the test
// case it happened in, never a failure, and a later test case is not named
// for it; one after the last test case is said too, charged to none.
func TestARecordingSaysAnAbandonedHandshakeAndKeepsIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ab := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossTLSAbandoned, Count: n, What: "to 10.0.0.1:443"}}
	}
	// The base, each test case's check, then the final read.
	instr := &countingInstr{answers: []models.CaptureLosses{nil, nil, ab(1), ab(1), ab(2)}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	w.afterTestCases(context.Background())
	if w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%s; want nothing tainted and the recording kept", w)
	}
	if fmt.Sprint(w.uncapturable) != "[test-2]" {
		t.Fatalf("test cases said %v, want [test-2]", w.uncapturable)
	}
	during := logs.FilterMessageSnippet("a TLS client that abandoned its handshake, a container the Docker daemon did not answer").All()
	if len(during) != 1 || !strings.Contains(fmt.Sprint(during[0].ContextMap()["next_step"]), "trust keploy's CA") {
		t.Fatalf("%d WARNs for the test case, want 1 with the CA-trust remedy: %+v", len(during), during)
	}
	late := logs.FilterMessageSnippet("after the last test case was recorded").All()
	if len(late) != 1 || late[0].Level != zapcore.WarnLevel || !strings.Contains(fmt.Sprint(late[0].ContextMap()["reason"]), "hung up on 1 TLS handshake(s)") ||
		!strings.Contains(late[0].Message, "Go's http.Transport keeps such a spare") || !strings.HasPrefix(fmt.Sprint(late[0].ContextMap()["next_step"]), "if the client does not trust keploy's CA: ") {
		t.Fatalf("%d lines for the late one, want one WARN naming 1 handshake: %+v", len(late), late)
	}
}

// Data the application sends over a server-first protocol keploy passes
// through (P250) is said for the test case it was sent in, never a failure;
// some sent after the last test case is said too, charged to none, as such,
// and not as an abandoned TLS handshake.
func TestARecordingSaysAServerFirstProtocolItPassedThrough(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	sf := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossServerFirst, Count: n, What: "to 10.0.0.9:25"}}
	}
	// The base, each test case's check, then the final read.
	instr := &countingInstr{answers: []models.CaptureLosses{nil, nil, sf(2), sf(2), sf(3)}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	w.afterTestCases(context.Background())
	if w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%s; want nothing tainted and the recording kept", w)
	}
	if fmt.Sprint(w.uncapturable) != "[test-2]" {
		t.Fatalf("test cases said %v, want [test-2]", w.uncapturable)
	}
	if n := len(logs.FilterMessageSnippet("abandoned its handshake with keploy's proxy").All()); n != 0 {
		t.Fatalf("%d late lines call it an abandoned TLS handshake", n)
	}
	late := logs.FilterMessageSnippet("keploy passes through (a server-first protocol, or a destination's refusal), after the last test case was recorded").All()
	if len(late) != 1 || late[0].Level != zapcore.WarnLevel || !strings.Contains(fmt.Sprint(late[0].ContextMap()["reason"]), "sent data on them 1 time(s)") || !strings.Contains(fmt.Sprint(late[0].ContextMap()["next_step"]), "mock.real") {
		t.Fatalf("%d lines for the late one, want one WARN naming 1 send and mock.real: %+v", len(late), late)
	}
}

func TestARecordingWhoseCountCannotBeReadIsNotVerified(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name  string
		instr *countingInstr
		want  string
	}{
		{"at the start", &countingInstr{answers: []models.CaptureLosses{nil}, errAt: map[int]error{1: errors.New("connection refused")}}, "tainted=[] unverified=[test-1,test-2,test-3]"},
		{"at one test case", &countingInstr{answers: []models.CaptureLosses{nil}, errAt: map[int]error{3: errors.New("connection reset")}}, "tainted=[] unverified=[test-2]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, failed, logs := record3(t, tc.instr)
			if !failed || w.String() != tc.want {
				t.Fatalf("failed %v, %s; want %s and the recording failed", failed, w, tc.want)
			}
			if logs.FilterMessageSnippet("the recording is not verified").Len() != 1 {
				t.Fatal("no 'recording is not verified' ERROR")
			}
		})
	}
}

func TestARecordingThatLostNothingPasses(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w, failed, logs := record3(t, &countingInstr{answers: []models.CaptureLosses{pid(3)}})
	if failed || w.String() != "tainted=[] unverified=[]" || logs.Len() != 0 {
		t.Fatalf("failed %v, %s, %d ERRORs; want nothing (refusals before capture began are not the recording's)", failed, w, logs.Len())
	}
}

// With the switch off nothing fails and no loss is said but a failed TLS
// handshake (caRejectWatch: the count is read as capture begins and as each
// test case is saved, for that WARN alone); with an agent that cannot count,
// nothing fails.
func TestTheCaptureWatchIsOffWithoutTheSwitchOrACount(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	off := &countingInstr{answers: []models.CaptureLosses{nil, pid(9)}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), off)
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
		settle(w) // each read made before the next test case is saved
	}
	if failed := w.finish("test-set-0", false); failed || logs.Len() != 0 || off.reads != 4 {
		t.Fatalf("switch off: failed %v, logged %v, after %d reads; want no failure, nothing said of a PID map loss, and the CA WARN's 4 reads", failed, logs.All(), off.reads)
	}
	t.Setenv(provenswitch.Env, "1")
	unsupported := &countingInstr{answers: []models.CaptureLosses{nil}, errAt: map[int]error{1: models.ErrCaptureLossesUnsupported}}
	if _, failed, _ := record3(t, unsupported); failed || unsupported.reads != 1 {
		t.Fatalf("an agent that cannot count: failed %v after %d reads; want one read and no failure", failed, unsupported.reads)
	}
	type plain struct{ Instrumentation }
	if _, failed, _ := record3(t, plain{}); failed {
		t.Fatal("an instrumentation without the count failed the recording")
	}
}

// Decision 20: test cases recorded while or after the application called what
// keploy cannot capture at all (a Unix socket) are named at WARN; the
// recording does not fail for it.
func TestARecordingThatCalledWhatKeployCannotCaptureWarnsAndDoesNotFail(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	db := models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/var/run/postgresql/.s.PGSQL.5432"}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), &countingInstr{answers: []models.CaptureLosses{nil, nil, db}})
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	if w.finish("test-set-0", false) {
		t.Fatal("the recording failed for a call keploy cannot capture")
	}
	warns := logs.FilterMessageSnippet("keploy cannot capture").All()
	if len(warns) != 1 || fmt.Sprint(warns[0].ContextMap()["testCases"]) != "[test-2 test-3]" {
		t.Fatalf("WARN lines %+v, want one naming test-2 and test-3", warns)
	}
}

// fakeAgentWatch is a watch of which agent process answers: replaced says
// another answered after the first.
type fakeAgentWatch struct {
	replaced bool
	stopped  bool
}

func (w *fakeAgentWatch) Replacement() (models.AgentReplacement, bool) {
	if !w.replaced {
		return models.AgentReplacement{First: "a"}, false
	}
	return models.AgentReplacement{First: "a", Next: "b"}, true
}
func (w *fakeAgentWatch) Accept() {}
func (w *fakeAgentWatch) Stop()   { w.stopped = true }

// watchingInstr is a countingInstr whose agent client watches which agent
// process answers it.
type watchingInstr struct {
	*countingInstr
	watch   *fakeAgentWatch
	watched int
}

func (w *watchingInstr) WatchAgentInstance() models.AgentInstanceWatch {
	w.watched++
	return w.watch
}

// P056: an agent started again in its place while the recording ran counts
// from nothing, and what the one before counted it could not capture, or
// captured but had not handed over, went with it: the recording is not
// verified, and fails. One process answering for all of it passes, and the
// watch is begun once and ended.
func TestARecordingWhoseAgentWasStartedAgainIsNotVerified(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, replaced := range []bool{false, true} {
		t.Run(fmt.Sprintf("replaced=%v", replaced), func(t *testing.T) {
			instr := &watchingInstr{countingInstr: &countingInstr{answers: []models.CaptureLosses{nil}}, watch: &fakeAgentWatch{replaced: replaced}}
			_, failed, logs := record3(t, instr)
			if failed != replaced {
				t.Fatalf("failed %v; want %v", failed, replaced)
			}
			if n := logs.FilterMessageSnippet("keploy's agent was started again while it recorded").Len(); (n == 1) != replaced {
				t.Fatalf("%d 'started again' ERROR(s) with replaced=%v", n, replaced)
			}
			if instr.watched != 1 || !instr.watch.stopped {
				t.Fatalf("watched %d time(s), stopped %v; want once, then stopped", instr.watched, instr.watch.stopped)
			}
		})
	}
	// With the switch off nothing is watched.
	t.Setenv(provenswitch.Env, "")
	off := &watchingInstr{countingInstr: &countingInstr{answers: []models.CaptureLosses{nil}}, watch: &fakeAgentWatch{replaced: true}}
	if _, failed, _ := record3(t, off); failed || off.watched != 0 {
		t.Fatalf("switch off: failed %v, watched %d time(s); want neither", failed, off.watched)
	}
}

// A mock stamped after its test case was checked (its hold let the test case
// go before the read of its container's configuration stalled) charges its
// own test case, by the mapping that holds it, whenever that comes: test-1
// is not judged, naming the container, and test-2, checked while the stall
// was counted, is untouched. The count of the kind charges nothing (it came
// after test-1's check), and a marked mock no test case holds is said apart.
// Nothing fails the recording.
func TestARecordingChargesAContainerTheDaemonDidNotAnswerToItsMocksOwnTestCase(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	unread := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: n, What: "container pg (postgres:16)"}}
	}
	// The base, test-1's check, test-2's check (the stall counted by then),
	// and the final read.
	instr := &countingInstr{answers: []models.CaptureLosses{nil, nil, unread(1), unread(2)}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCase(context.Background(), "test-2")
	marked := func(name string) *models.Mock {
		return &models.Mock{Name: name, Spec: models.MockSpec{Metadata: map[string]string{models.ContainerUnreadMetadata: "container pg (postgres:16)"}}}
	}
	w.saved(marked("mock-7"))                                // stamped late, saved after both checks
	w.saved(&models.Mock{Name: "mock-8"})                    // its container named
	w.saved(marked("mock-9"))                                // a startup mock no test case holds
	w.mapped("test-1", []models.MockEntry{{Name: "mock-7"}}) // test-1's delta mapping, binned by its request
	w.mapped("test-2", []models.MockEntry{{Name: "mock-8"}})
	w.mapped("test-1", []models.MockEntry{{Name: "mock-7"}}) // named again: charged once
	w.afterTestCases(context.Background())
	if w.finish("test-set-0", false) || w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%s; want nothing tainted and the recording kept", w)
	}
	charged := logs.FilterMessageSnippet("a container the Docker daemon did not answer keploy's read of").All()
	if len(charged) != 1 || fmt.Sprint(charged[0].ContextMap()["testCases"]) != "[test-1]" ||
		!strings.Contains(fmt.Sprint(charged[0].ContextMap()["reason"]), "(1 time(s)") ||
		!strings.Contains(fmt.Sprint(charged[0].ContextMap()["reason"]), "pg (postgres:16)") ||
		!strings.Contains(fmt.Sprint(charged[0].ContextMap()["next_step"]), "docker inspect") {
		t.Fatalf("%d WARNs for test cases, want one naming test-1 alone, once, with pg and the inspect remedy: %+v", len(charged), charged)
	}
	apart := logs.FilterMessageSnippet("mocks no test case holds").All()
	if len(apart) != 1 || !strings.Contains(fmt.Sprint(apart[0].ContextMap()["reason"]), "pg (postgres:16)") {
		t.Fatalf("%d WARNs for the mock no test case holds, want one naming pg: %+v", len(apart), apart)
	}
	if n := logs.FilterMessageSnippet("after the last test case").Len(); n != 0 {
		t.Fatalf("%d lines charged the count after the last test case: the mocks charge it", n)
	}
}

// A run of a downstream build's call program the kernel skipped after the
// last test case is said as that, charged to no test case, never as an
// abandoned TLS handshake with its CA remedy.
func TestARecordingSaysAKernelSkipAfterItsLastTestCase(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	sk := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossKernelSkipCall, Count: n, What: "keploy_tcp_recvmsg_exit"}}
	}
	instr := &countingInstr{answers: []models.CaptureLosses{nil, nil, nil, nil, sk(1)}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	w.afterTestCases(context.Background())
	if w.finish("test-set-0", false) {
		t.Fatalf("%s; want the recording kept", w)
	}
	late := logs.FilterMessageSnippet("after the last test case was recorded").All()
	if len(late) != 1 || !strings.Contains(late[0].Message, "the kernel skipped runs of keploy's capture programs") || !strings.Contains(late[0].Message, "evidence, not a verdict") ||
		strings.Contains(fmt.Sprint(late[0].ContextMap()["next_step"]), "trust keploy's CA") {
		t.Fatalf("%d lines for the late skip, want one WARN about the kernel's skip, as evidence, without the CA remedy: %+v", len(late), late)
	}
}

// A run of keploy's capture programs the kernel skipped while test cases
// were recorded is evidence only (the kernel's recursion_misses, 5.12+):
// one WARN names the test cases it covers, and the recording is not failed.
// On a kernel without the count (5.10) the same recording ends the same,
// without the WARN.
func TestARecordingSaysAKernelSkipAsEvidence(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	sk := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossKernelSkip, Count: n, What: "keploy_tcp_connect"}}
	}
	run := func(answers []models.CaptureLosses) (bool, *observer.ObservedLogs) {
		core, logs := observer.New(zapcore.WarnLevel)
		w := watchCaptureLosses(context.Background(), zap.New(core), &countingInstr{answers: answers})
		for _, n := range []string{"test-1", "test-2", "test-3"} {
			w.afterTestCase(context.Background(), n)
		}
		w.afterTestCases(context.Background())
		return w.finish("test-set-0", false), logs
	}
	failed, logs := run([]models.CaptureLosses{nil, nil, sk(2), sk(2), sk(2)})
	if failed {
		t.Fatal("a recording failed on evidence")
	}
	said := logs.FilterMessageSnippet("evidence, not a verdict").All()
	if len(said) != 1 || logs.Len() != 1 || fmt.Sprint(said[0].ContextMap()["testCases"]) != "[test-2 test-3]" ||
		!strings.Contains(fmt.Sprint(said[0].ContextMap()["reason"]), "during its recording the kernel skipped 2 capture run(s)") {
		t.Fatalf("logged %+v, want one WARN naming test-2 and test-3 with the skip", logs.All())
	}
	// 5.10: no count, the same recording.
	failed, logs = run([]models.CaptureLosses{nil})
	if failed || logs.Len() != 0 {
		t.Fatalf("without the count: failed %v, logged %+v; want the same recording, said nothing", failed, logs.All())
	}
}

// judgeLater has the capture watch hand what it finds to a later pass, for
// the test's life, and returns what was handed.
func judgeLater(t *testing.T) *[]RecordedLosses {
	t.Helper()
	var got []RecordedLosses
	t.Cleanup(JudgeLossesLater(func(l RecordedLosses) { got = append(got, l) }))
	return &got
}

// Where a later pass judges each recorded test case (enterprise's
// auto-replay), a loss in record fails nothing: the recording is kept, the
// loss is said at WARN, never ERROR, and handed over as evidence on the test
// case whose window it fell in. That pass keeps the test cases that replay.
func TestARecordJudgedLaterKeepsItsLossesAsEvidence(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	got := judgeLater(t)
	// Test case 2 loses a call; test case 3 is recorded after it (a taint,
	// so 3 carries it too).
	instr := &countingInstr{answers: []models.CaptureLosses{nil, nil, pid(1), pid(1), pid(1)}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	w.afterTestCases(context.Background())
	if w.finish("test-set-0", false) {
		t.Fatal("the recording failed on a loss a later pass judges")
	}
	if n := len(logs.FilterLevelExact(zapcore.ErrorLevel).All()); n != 0 {
		t.Fatalf("%d ERROR lines, want none: the loss is evidence", n)
	}
	if len(logs.FilterMessageSnippet("the auto-replay that follows judges each of its test cases").All()) != 1 {
		t.Fatalf("no WARN that auto-replay judges the test cases: %+v", logs.All())
	}
	if len(*got) != 1 {
		t.Fatalf("handed %d accounts, want 1", len(*got))
	}
	l := (*got)[0]
	if l.TestSetID != "test-set-0" || !l.Failing() || fmt.Sprint(l.TestCases()) != "[test-2 test-3]" {
		t.Fatalf("handed %+v, want test-set-0's, failing, naming test-2 and test-3", l)
	}
	if lost := l.ByTestCase["test-2"]; len(lost) != 1 || lost[0].Kind != models.CaptureLossPIDMap {
		t.Fatalf("test-2's evidence %v, want the PID-map refusal", lost)
	}
	if _, ok := l.ByTestCase["test-1"]; ok {
		t.Fatal("test-1, recorded before the loss, carries evidence")
	}
}

// What was lost after the last test case, and connections to a mock.real
// destination neither captured nor relayed, are the set's: handed over, and
// failing the set if the later pass judges none of it.
func TestARecordJudgedLaterHandsOverWhatNoTestCaseHolds(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	got := judgeLater(t)
	instr := &countingInstr{answers: []models.CaptureLosses{nil, nil, pid(2)}}
	w := watchCaptureLosses(context.Background(), zap.NewNop(), instr)
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCases(context.Background())
	if w.finish("test-set-0", true) {
		t.Fatal("the recording failed on what a later pass judges")
	}
	l := (*got)[0]
	if len(l.Uncharged) != 1 || l.Uncharged[0].Count != 2 || !l.RealUncaptured || !l.Failing() || len(l.TestCases()) != 0 {
		t.Fatalf("handed %+v, want the 2 lost after the last test case and the mock.real connections, the set's", l)
	}
}

// Without a later pass (keploy record alone, or an auto-replay turned off),
// record judges its own losses as it always did: the recording fails, and
// mock.real connections neither captured nor relayed fail it too.
func TestARecordNoPassJudgesFailsOnItsLosses(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	undo := JudgeLossesLater(func(RecordedLosses) { t.Fatal("handed over with the pass undone") })
	undo()
	instr := &countingInstr{answers: []models.CaptureLosses{nil, pid(1), pid(1)}}
	w := watchCaptureLosses(context.Background(), zap.NewNop(), instr)
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCases(context.Background())
	if !w.finish("test-set-0", false) {
		t.Fatal("a recording no later pass judges did not fail on its loss")
	}
	w = watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	if !w.finish("test-set-0", true) {
		t.Fatal("mock.real connections neither captured nor relayed did not fail the recording")
	}
}

// With the switch off only the CA WARN runs, as without a later pass,
// judged later or not: nothing fails, nothing is handed over, and the WARN
// is said the same.
func TestARecordWithTheSwitchOffIsAsBefore(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	got := judgeLater(t)
	w := watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{pid(1)}})
	w.afterTestCase(context.Background(), "test-1")
	if w.finish("test-set-0", false) || len(*got) != 0 {
		t.Fatalf("with the switch off: failed or handed over %v", *got)
	}
	said := func() string {
		instr := &countingInstr{answers: []models.CaptureLosses{nil, tlsFailed(1, "to api.example:443"), tlsFailed(2, "to pay.example:443")}}
		failed, logs := recordOff(t, instr, "test-1")
		if failed {
			t.Fatal("the recording failed with the switch off")
		}
		var lines []string
		for _, e := range logs.All() {
			lines = append(lines, e.Level.String()+" "+e.Message+" "+fmt.Sprint(e.ContextMap()))
		}
		return strings.Join(lines, "\n")
	}
	later := said()
	if !strings.Contains(later, caWarn) || len(*got) != 0 {
		t.Fatalf("judged later, switch off: said %q, handed %v; want the CA WARN, nothing handed", later, *got)
	}
	undo := JudgeLossesLater(func(RecordedLosses) {})
	undo()
	if alone := said(); alone != later {
		t.Fatalf("the CA WARN judged later %q, without a later pass %q: want them the same", later, alone)
	}
}

// Where a later pass judges the test cases, what fails nothing either way is
// said exactly as without it, each at WARN with its remedy: a test case
// recorded while a TLS client abandoned its handshake, a handshake abandoned
// after the last test case, data on a connection keploy passes through after
// it, a run of a call program the kernel skipped after it, and a mock whose
// container the Docker daemon did not name that no test case holds.
func TestARecordJudgedLaterSaysWhatFailsNothingAsBefore(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	abandoned := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossTLSAbandoned, Count: n, What: "to db:5432"}}
	}
	late := abandoned(2).Plus(models.CaptureLosses{{Kind: models.CaptureLossServerFirst, Count: 1, What: "to smtp:25"}, {Kind: models.CaptureLossKernelSkipCall, Count: 1, What: "keploy_tcp_recvmsg_exit"}})
	unnamed := &models.Mock{Name: "mock-9", Spec: models.MockSpec{Metadata: map[string]string{models.ContainerUnreadMetadata: "container pg (postgres:16)"}}}
	run := func(judged bool) []string {
		if judged {
			judgeLater(t)
		}
		core, logs := observer.New(zapcore.DebugLevel)
		w := watchCaptureLosses(context.Background(), zap.New(core), &countingInstr{answers: []models.CaptureLosses{nil, abandoned(1), late}})
		w.saved(unnamed)
		w.afterTestCase(context.Background(), "test-1")
		w.afterTestCases(context.Background())
		if w.finish("test-set-0", false) {
			t.Fatalf("judged later %v: a recording that lost nothing judged failed", judged)
		}
		var said []string
		for _, e := range logs.All() {
			said = append(said, e.Level.String()+" "+e.Message+" "+fmt.Sprint(e.ContextMap()))
		}
		return said
	}
	alone := run(false)
	if len(alone) != 5 {
		t.Fatalf("judged alone: %q, want the five WARNs", alone)
	}
	if later := run(true); fmt.Sprint(later) != fmt.Sprint(alone) {
		t.Fatalf("judged later: %q, want what record says alone: %q", later, alone)
	}
}

// A loss that taints every later test case is in each of them, counted from
// the set's start: the account says it once, not once per test case.
func TestARecordedLossThatTaintsLaterTestCasesIsCountedOnce(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	got := judgeLater(t)
	w := watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil, nil, pid(1), pid(1), pid(1)}})
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	w.afterTestCases(context.Background())
	w.finish("test-set-0", false)
	l := (*got)[0]
	if lost := l.Lost(); len(lost) != 1 || lost[0].Count != 1 {
		t.Fatalf("Lost() = %v, want the one PID-map refusal", lost)
	}
	if want := (models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 1}}).Text(); !strings.Contains(l.Text(), want) {
		t.Fatalf("Text() = %q, want it to say %q", l.Text(), want)
	}
}

// Failing is what fails a recording judged on its own: each of a judged
// loss, a test case whose count could not be read, what could not be
// verified, a loss no test case holds and mock.real connections neither
// captured nor relayed; never a not-judged kind alone, or nothing.
func TestWhatFailsARecordingJudgedOnItsOwn(t *testing.T) {
	for _, c := range []struct {
		name string
		l    RecordedLosses
		want bool
	}{
		{"nothing", RecordedLosses{}, false},
		{"a not-judged kind", RecordedLosses{ByTestCase: map[string]models.CaptureLosses{"t": {{Kind: models.CaptureLossTLSAbandoned, Count: 1}}}}, false},
		{"a judged loss", RecordedLosses{ByTestCase: map[string]models.CaptureLosses{"t": pid(1)}}, true},
		{"an unread count", RecordedLosses{Unverified: []string{"t"}}, true},
		{"not verified", RecordedLosses{NotVerified: "the count could not be read after the last test case"}, true},
		{"a loss no test case holds", RecordedLosses{Uncharged: pid(1)}, true},
		{"mock.real connections", RecordedLosses{RealUncaptured: true}, true},
	} {
		if got := c.l.Failing(); got != c.want {
			t.Errorf("%s: Failing() = %v, want %v", c.name, got, c.want)
		}
	}
}

// Handed to a later pass, an agent started again while it recorded is what
// could not be verified, and the watch of it ends as it does without one.
func TestARecordJudgedLaterHandsOverAnAgentStartedAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	got := judgeLater(t)
	instr := &watchingInstr{countingInstr: &countingInstr{answers: []models.CaptureLosses{nil}}, watch: &fakeAgentWatch{replaced: true}}
	core, logs := observer.New(zapcore.ErrorLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	failed, incomplete := w.finishRun("test-set-0", false, "")
	if failed || !incomplete || logs.Len() != 0 {
		t.Fatalf("failed %v, incomplete %v, logged %v at ERROR: want not failed, incomplete, nothing logged", failed, incomplete, logs.All())
	}
	if !instr.watch.stopped {
		t.Fatal("the agent's watch was not stopped")
	}
	if l := (*got)[0]; !strings.Contains(l.NotVerified, "started again") || !l.Failing() {
		t.Fatalf("handed %+v, want it not verified for the restart", l)
	}
}

// An agent that cannot count capture losses leaves nothing to hand over but
// mock.real connections neither captured nor relayed, which the later pass
// takes too: the recording fails on neither.
func TestARecordJudgedLaterWithAnAgentThatCannotCount(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	got := judgeLater(t)
	unsupported := &countingInstr{errAt: map[int]error{1: models.ErrCaptureLossesUnsupported}, answers: []models.CaptureLosses{nil}}
	w := watchCaptureLosses(context.Background(), zap.NewNop(), unsupported)
	if w.finish("test-set-0", false) || len(*got) != 0 {
		t.Fatalf("with nothing to hand over: handed %v", *got)
	}
	if fail, incomplete := w.finishRun("test-set-0", true, ""); fail || !incomplete || len(*got) != 1 || !(*got)[0].RealUncaptured {
		t.Fatalf("with mock.real connections lost: handed %+v, want them handed over, the recording not failed", *got)
	}
}

// A recording whose losses were handed to a later pass is not failed, and is
// incomplete all the same (the session's telemetry says so); one with
// nothing that would fail it is neither.
func TestARecordJudgedLaterIsIncompleteNotFailed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	judgeLater(t)
	w := watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil, pid(1)}})
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCases(context.Background())
	if fail, incomplete := w.finishRun("test-set-0", false, ""); fail || !incomplete {
		t.Fatalf("finishRun = %v, %v; want not failed, incomplete", fail, incomplete)
	}
	w = watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCases(context.Background())
	if fail, incomplete := w.finishRun("test-set-0", false, ""); fail || incomplete {
		t.Fatalf("finishRun = %v, %v with nothing lost; want neither", fail, incomplete)
	}
}

// A failed read of the mock.real connections is handed over as that, not as
// connections lost, and fails the recording judged on its own.
func TestARecordHandsOverAFailedReadOfMockReal(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	unread := "keploy could not read from its agent which mock.real connections it could not capture: agent gone"
	w := watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	if fail, _ := w.finishRun("test-set-0", false, unread); !fail {
		t.Fatal("a recording judged on its own did not fail on an unread mock.real")
	}
	got := judgeLater(t)
	w = watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil}})
	if fail, _ := w.finishRun("test-set-0", false, unread); fail {
		t.Fatal("failed where a later pass judges")
	}
	if l := (*got)[0]; l.RealUncaptured || !strings.Contains(l.RealUnread, "agent gone") || !l.Failing() || strings.Contains(l.Text(), "could neither capture nor relay") {
		t.Fatalf("handed %+v (%q), want the failed read as that", l, l.Text())
	}
}

// A kernel's count of skipped runs is handed to a later pass as evidence on
// the test cases it covers, never as what fails a recording: the pass keeps
// a test case that replays, and names the skip on one that does not.
func TestARecordJudgedLaterHandsOverAKernelSkipAsEvidence(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	got := judgeLater(t)
	sk := models.CaptureLosses{{Kind: models.CaptureLossKernelSkip, Count: 3, What: "keploy_tcp_connect"}}
	w := watchCaptureLosses(context.Background(), zap.NewNop(), &countingInstr{answers: []models.CaptureLosses{nil, nil, sk, sk, sk}})
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		w.afterTestCase(context.Background(), n)
	}
	w.afterTestCases(context.Background())
	if fail, incomplete := w.finishRun("test-set-0", false, ""); fail || incomplete {
		t.Fatalf("finishRun = %v, %v; want neither: evidence is no verdict", fail, incomplete)
	}
	l := (*got)[0]
	if l.Failing() || len(l.FailingTestCases()) != 0 || fmt.Sprint(l.TestCases()) != "[test-2 test-3]" {
		t.Fatalf("handed %+v, want test-2 and test-3 carrying the skip, failing nothing", l)
	}
	for _, tc := range []string{"test-2", "test-3"} {
		judged, notJudged := l.ByTestCase[tc].Split()
		if len(judged) != 0 || len(notJudged) != 0 || len(l.ByTestCase[tc].Evidence()) != 1 {
			t.Fatalf("%s carries %+v: want the skip as evidence, no verdict", tc, l.ByTestCase[tc])
		}
	}
	if txt := l.Text(); !strings.Contains(txt, "during its recording the kernel skipped 3 capture run(s)") || strings.Contains(txt, "not judged") {
		t.Fatalf("Text() = %q, want the skip said, as no verdict", txt)
	}
}
