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
	return w, w.finish("test-set-0"), logs
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
	if !w.finish("test-set-0") || w.String() != "tainted=[test-1,test-3] unverified=[]" {
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
	if w.finish("test-set-0") || w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%s; want nothing tainted and the recording kept", w)
	}
	if fmt.Sprint(w.uncapturable) != "[test-2]" {
		t.Fatalf("test cases said %v, want [test-2]", w.uncapturable)
	}
	during := logs.FilterMessageSnippet("abandoned its handshake: such calls are not in the mocks").All()
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
	if w.finish("test-set-0") || w.String() != "tainted=[] unverified=[]" {
		t.Fatalf("%s; want nothing tainted and the recording kept", w)
	}
	if fmt.Sprint(w.uncapturable) != "[test-2]" {
		t.Fatalf("test cases said %v, want [test-2]", w.uncapturable)
	}
	if n := len(logs.FilterMessageSnippet("abandoned its handshake with keploy's proxy").All()); n != 0 {
		t.Fatalf("%d late lines call it an abandoned TLS handshake", n)
	}
	late := logs.FilterMessageSnippet("server-first protocol keploy passes through, after the last test case was recorded").All()
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

// With the switch off, or an agent that cannot count, nothing is read and
// nothing fails.
func TestTheCaptureWatchIsOffWithoutTheSwitchOrACount(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	off := &countingInstr{answers: []models.CaptureLosses{nil, pid(9)}}
	if _, failed, _ := record3(t, off); failed || off.reads != 0 {
		t.Fatalf("switch off: failed %v after %d reads; want no read and no failure", failed, off.reads)
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
	if w.finish("test-set-0") {
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
