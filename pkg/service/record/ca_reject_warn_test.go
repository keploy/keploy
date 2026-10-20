package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func tlsFailed(n uint64, to string) models.CaptureLosses {
	return models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: n, What: to}}
}

// recordOff is a switch-off recording of test cases names against instr: the
// watch as record.go drives it (begun before the app, a check as each test
// case is saved, the final read, the end), with every log line from WARN up.
func recordOff(t *testing.T, instr Instrumentation, names ...string) (bool, *observer.ObservedLogs) {
	t.Helper()
	t.Setenv(provenswitch.Env, "0")
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, n := range names {
		w.afterTestCase(context.Background(), n)
		settle(w)
	}
	w.afterTestCases(context.Background())
	return w.finish("test-set-0", false), logs
}

// settle waits for the switch-off watch's reads asked for so far: a recording
// whose reads keep up with its test cases.
func settle(w *captureWatch) {
	if w.ca != nil {
		w.ca.waitForReads()
	}
}

const caWarn = "TLS handshake(s) with keploy's proxy failed while this recording ran (a client that does not trust keploy's CA rejects keploy's certificate); those calls are not recorded"

// With the proven switch off, a recording whose client rejected keploy's
// certificate (it pins its own CA, say) says so once, at WARN: how many
// handshakes failed, the test cases recorded while they did, how many after
// the last, and where keploy says how to trust its CA. The recording is not
// failed, and nothing else is said: switch-off record ends as it did.
func TestWithTheSwitchOffAFailedHandshakeIsSaidAndNothingFails(t *testing.T) {
	instr := &countingInstr{answers: []models.CaptureLosses{
		nil,                                // capture begins
		tlsFailed(1, "to api.example:443"), // test-1 saved: one failed during it
		tlsFailed(1, "to api.example:443"), // test-2: none more
		tlsFailed(3, "to api.example:443"), // test-3: two more
		tlsFailed(4, "to pay.example:443"), // the final read: one after the last
	}}
	failed, logs := recordOff(t, instr, "test-1", "test-2", "test-3")
	if failed {
		t.Fatal("the recording failed with the switch off; it must end as it did")
	}
	all := logs.All()
	if len(all) != 1 || all[0].Level != zapcore.WarnLevel || all[0].Message != "4 "+caWarn {
		t.Fatalf("logged %v, want one WARN %q", all, "4 "+caWarn)
	}
	f := all[0].ContextMap()
	if got, _ := f["testCases"].([]interface{}); len(got) != 2 || got[0] != "test-1" || got[1] != "test-3" {
		t.Fatalf("testCases %v, want [test-1 test-3]", f["testCases"])
	}
	if f["afterTheLastTestCase"] != uint64(1) || f["last"] != "to pay.example:443" {
		t.Fatalf("after the last %v, last %v; want 1 and to pay.example:443", f["afterTheLastTestCase"], f["last"])
	}
	if step, _ := f["next_step"].(string); !strings.Contains(step, caDocs) {
		t.Fatalf("next_step %q does not point at %s", step, caDocs)
	}
}

// Nothing failed, nothing said; a count that cannot be read is left unsaid,
// and fails nothing; an agent started again (its count from nothing) still
// has its failures counted.
func TestWithTheSwitchOffTheWarnSaysOnlyWhatItSaw(t *testing.T) {
	if failed, logs := recordOff(t, &countingInstr{answers: []models.CaptureLosses{nil, pid(2)}}, "test-1"); failed || logs.Len() != 0 {
		t.Fatalf("no handshake failed: failed %v, logged %v", failed, logs.All())
	}
	unread := &countingInstr{answers: []models.CaptureLosses{nil}, errAt: map[int]error{1: errors.New("agent gone")}}
	if failed, logs := recordOff(t, unread, "test-1"); failed || logs.Len() != 0 || unread.reads != 1 {
		t.Fatalf("an unreadable start: failed %v, logged %v, %d reads (want 1: nothing read after it)", failed, logs.All(), unread.reads)
	}
	readFails := &countingInstr{answers: []models.CaptureLosses{nil, tlsFailed(1, "to a:443")}, errAt: map[int]error{2: errors.New("timeout")}}
	if failed, logs := recordOff(t, readFails, "test-1", "test-2"); failed || logs.Len() != 1 || !strings.HasPrefix(logs.All()[0].Message, "1 ") ||
		fmt.Sprint(logs.All()[0].ContextMap()["testCases"]) != "[test-1 test-2]" {
		t.Fatalf("a check that could not be read: failed %v, logged %v; want the next read to count it, its test case first", failed, logs.All())
	}
	restarted := &countingInstr{answers: []models.CaptureLosses{tlsFailed(5, "to a:443"), tlsFailed(5, "to a:443"), tlsFailed(2, "to a:443")}}
	if failed, logs := recordOff(t, restarted, "test-1", "test-2"); failed || logs.Len() != 1 || logs.All()[0].Message != "2 "+caWarn {
		t.Fatalf("an agent started again: failed %v, logged %v; want its 2 counted", failed, logs.All())
	}
}

// With the switch on the WARN is not said: a failed handshake fails the
// recording there (captureWatch).
func TestWithTheSwitchOnTheCAWarnIsNotSaid(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zapcore.WarnLevel)
	instr := &countingInstr{answers: []models.CaptureLosses{nil, tlsFailed(1, "to a:443")}}
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCases(context.Background())
	if !w.finish("test-set-0", false) {
		t.Fatal("with the switch on a failed handshake fails the recording")
	}
	if n := logs.FilterMessageSnippet(caWarn).Len(); n != 0 {
		t.Fatalf("the switch-off WARN was said %d times with the switch on", n)
	}
}

// httpLossReader reads the count as AgentClient.GetCaptureLosses does: a
// request on ctx, which ends with it.
type httpLossReader struct {
	Instrumentation
	url string
}

func (h *httpLossReader) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	var l models.CaptureLosses
	return l, json.NewDecoder(res.Body).Decode(&l)
}

// A stop ends record's context before the drain saves its last test cases
// and before the final read (utils.Stop, ExecCancel): the reads go on all the
// same, so a handshake that failed in a test case saved by the drain, or
// after the last, is said.
func TestAFailedHandshakeAroundTheStopIsStillSaid(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	var count atomic.Uint64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(tlsFailed(count.Load(), "to up:8443"))
	}))
	defer srv.Close()
	core, logs := observer.New(zapcore.WarnLevel)
	ctx, stop := context.WithCancel(context.Background())
	w := watchCaptureLosses(ctx, zap.New(core), &httpLossReader{url: srv.URL})
	w.afterTestCase(ctx, "test-1")
	settle(w)
	count.Store(1)
	stop()                         // the stop: Start's context ends
	w.afterTestCase(ctx, "test-2") // the drain saves test-2
	settle(w)
	count.Store(3)
	w.afterTestCases(ctx) // the teardown's final read
	w.finish("test-set-0", false)
	all := logs.All()
	if len(all) != 1 || all[0].Message != "3 "+caWarn {
		t.Fatalf("logged %v, want one WARN %q", all, "3 "+caWarn)
	}
	f := all[0].ContextMap()
	if got, _ := f["testCases"].([]interface{}); len(got) != 1 || got[0] != "test-2" || f["afterTheLastTestCase"] != uint64(2) {
		t.Fatalf("testCases %v, after the last %v; want [test-2] and 2", f["testCases"], f["afterTheLastTestCase"])
	}
}

// orderedReader answers test-1's read (the second) only once released, and
// every later read (the final one) at once, with a higher count.
type orderedReader struct {
	Instrumentation
	n       atomic.Int32
	perTest chan struct{} // closed as test-1's read is asked
	release chan struct{} // test-1's read answers once it closes
}

func (o *orderedReader) GetCaptureLosses(context.Context) (models.CaptureLosses, error) {
	switch o.n.Add(1) {
	case 1:
		return nil, nil
	case 2:
		close(o.perTest)
		<-o.release
		return tlsFailed(1, "to a:443"), nil
	default:
		return tlsFailed(2, "to a:443"), nil
	}
}

// A test case's read and the final read asked while it is out are made one
// after the other, in order: the final one is not asked until the other is
// counted, so an earlier, lower answer is never taken for an agent started
// again. Neither ask waits for its read.
func TestReadsAtOnceAreCountedInOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	o := &orderedReader{perTest: make(chan struct{}), release: make(chan struct{})}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), o)
	w.afterTestCase(context.Background(), "test-1")
	<-o.perTest
	w.afterTestCases(context.Background())
	// The final read must wait for test-1's to be counted: it is not asked
	// while test-1's is out (given time it would be, were it not held).
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if o.n.Load() > 2 {
			t.Fatal("the final read was asked while test-1's read was still out")
		}
	}
	close(o.release)
	w.finish("test-set-0", false)
	all := logs.All()
	if len(all) != 1 || all[0].Message != "2 "+caWarn {
		t.Fatalf("logged %v; the agent counted 2", all)
	}
}

// Only failed handshakes are said: one a client hung up on
// (tls_handshake_abandoned) may be a client that gave up, and is not.
func TestAnAbandonedHandshakeIsNotSaid(t *testing.T) {
	abandoned := func(n uint64) models.CaptureLosses {
		return models.CaptureLosses{{Kind: models.CaptureLossTLSAbandoned, Count: n, What: "to a:443"}}
	}
	instr := &countingInstr{answers: []models.CaptureLosses{nil, abandoned(1), abandoned(3)}}
	if failed, logs := recordOff(t, instr, "test-1", "test-2"); failed || logs.Len() != 0 {
		t.Fatalf("abandoned handshakes: failed %v, logged %v; want nothing", failed, logs.All())
	}
}

// A read that fails once counts exist leaves them to the next read: it is no
// count of 0, after which the next read would count them again.
func TestAFailedReadIsNoCountOfZero(t *testing.T) {
	instr := &countingInstr{
		answers: []models.CaptureLosses{tlsFailed(2, "to a:443"), tlsFailed(2, "to a:443"), nil, tlsFailed(2, "to a:443")},
		errAt:   map[int]error{3: errors.New("timeout")},
	}
	if failed, logs := recordOff(t, instr, "test-1", "test-2", "test-3"); failed || logs.Len() != 0 {
		t.Fatalf("nothing failed after capture began: failed %v, logged %v", failed, logs.All())
	}
}

// The WARN says the destination of the last failed handshake, a test case's
// too; with every failure after the last test case it names none, and says to
// record again.
func TestTheWarnsFieldsAreWhatItSaw(t *testing.T) {
	during := &countingInstr{answers: []models.CaptureLosses{nil, tlsFailed(1, "to api.example:443")}}
	_, logs := recordOff(t, during, "test-1")
	if all := logs.All(); len(all) != 1 || all[0].ContextMap()["last"] != "to api.example:443" {
		t.Fatalf("logged %v; want the test case's destination", logs.All())
	}
	if f := logs.All()[0].ContextMap(); f["afterTheLastTestCase"] != nil {
		t.Fatalf("afterTheLastTestCase %v with none after the last", f["afterTheLastTestCase"])
	}
	afterOnly := &countingInstr{answers: []models.CaptureLosses{nil, nil, tlsFailed(1, "to a:443")}}
	_, logs = recordOff(t, afterOnly, "test-1")
	all := logs.All()
	if len(all) != 1 {
		t.Fatalf("logged %v, want one WARN", all)
	}
	f := all[0].ContextMap()
	step, _ := f["next_step"].(string)
	if f["testCases"] != nil || f["afterTheLastTestCase"] != uint64(1) || !strings.HasSuffix(step, "then record again") {
		t.Fatalf("testCases %v, after the last %v, next_step %q; want none, 1, and to record again", f["testCases"], f["afterTheLastTestCase"], step)
	}
}

// In a native or docker stop the final read comes before the drain saves
// its last test cases (record.go): a handshake that failed in a test case the
// drain saves is that test case's, and named, not one after the last.
func TestATestCaseTheDrainSavesAfterTheFinalReadIsNamed(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	instr := &countingInstr{answers: []models.CaptureLosses{
		nil,                      // capture begins
		nil,                      // test-1
		tlsFailed(1, "to a:443"), // the final read
		tlsFailed(2, "to a:443"), // test-2, saved by the drain: one more since
	}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background())
	settle(w)
	w.afterTestCase(context.Background(), "test-2")
	w.finish("test-set-0", false)
	all := logs.All()
	if len(all) != 1 || all[0].Message != "2 "+caWarn {
		t.Fatalf("logged %v, want one WARN %q", all, "2 "+caWarn)
	}
	f := all[0].ContextMap()
	step, _ := f["next_step"].(string)
	if got, _ := f["testCases"].([]interface{}); len(got) != 1 || got[0] != "test-2" || f["afterTheLastTestCase"] != nil || !strings.HasSuffix(step, "re-record these test cases") {
		t.Fatalf("testCases %v, after the last %v, next_step %q; want [test-2], none, and to re-record them", f["testCases"], f["afterTheLastTestCase"], step)
	}
}

// A stop before capture began ends the base read: nothing is watched, and
// nothing said.
func TestAStopBeforeCaptureBeganIsNoWatch(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(tlsFailed(1, "to a:443"))
	}))
	defer srv.Close()
	ctx, stop := context.WithCancel(context.Background())
	stop()
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(ctx, zap.New(core), &httpLossReader{url: srv.URL})
	if w.ca != nil {
		t.Fatal("a base read the stop ended began a watch")
	}
	w.afterTestCase(ctx, "test-1")
	w.afterTestCases(ctx)
	w.finish("test-set-0", false)
	if logs.Len() != 0 {
		t.Fatalf("logged %v", logs.All())
	}
}

// wedgedReader answers its reads from answers, but read number wedge, which
// the agent never answers: it ends only as its context does.
type wedgedReader struct {
	Instrumentation
	answers []models.CaptureLosses
	wedge   int32
	n       atomic.Int32
	asked   chan struct{} // closed as the wedged read is asked
	ended   chan struct{} // closed as the wedged read's context ends
}

func newWedgedReader(wedge int32, answers ...models.CaptureLosses) *wedgedReader {
	return &wedgedReader{answers: answers, wedge: wedge, asked: make(chan struct{}), ended: make(chan struct{})}
}

func (w *wedgedReader) GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	i := w.n.Add(1)
	if i == w.wedge {
		close(w.asked)
		<-ctx.Done()
		close(w.ended)
		return nil, ctx.Err()
	}
	if int(i) > len(w.answers) {
		return w.answers[len(w.answers)-1], nil
	}
	return w.answers[i-1], nil
}

// within fails the test unless ch closes; the bound is only for a wait
// that never ends, which is what it catches.
func within(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// stoppedRun is a run stopped by a signal, as NewCtx's handler marks it, put
// back when the test ends.
func stoppedRun(t *testing.T) {
	t.Helper()
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	utils.MarkInterrupted()
}

// manualStall has the watch measure its reads' bound on a clock the test
// drives.
func manualStall(w *captureWatch) *stall.ManualClock {
	clk := &stall.ManualClock{}
	w.ca.clock = clk
	w.ca.every = time.Millisecond
	return clk
}

// giveUp moves clk past the reads' bound once a read waits on it: a timer
// it fires, the read looks at its stall.
func giveUp(t *testing.T, clk *stall.ManualClock) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second) // bounds only a read that never looks
	for clk.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no read looked at its stall")
		}
		time.Sleep(time.Millisecond)
	}
	clk.Advance(stall.HangBudget)
}

// finishing runs finish on a goroutine, and returns a channel closed once it
// has said what it had to.
func finishing(w *captureWatch) <-chan struct{} {
	done := make(chan struct{})
	go func() { w.finish("test-set-0", false); close(done) }()
	return done
}

// In a native or docker stop the final read is asked before the drain saves
// what the agent still holds (record.go). It holds that drain up for nothing,
// and stands no offer: a press while the drain saves, its read still out, is
// no answer to an offer of the watch's, and the drain saves all the agent
// held. Nor does finish stand one while it waits for the read.
func TestTheCAWatchStandsNoOfferWhileADrainSaves(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	stoppedRun(t)
	const held = 50
	f, frames, stopDrain := newHeldAgentRecorder(t, held, zap.NewNop())
	var got atomic.Int32
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range frames.Outgoing {
			got.Add(1)
		}
	}()
	r := newWedgedReader(2, nil) // the final read: never answered
	core, logs := observer.New(zapcore.InfoLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	clk := manualStall(w)
	stopDrain() // the stop: the drain begins
	asked := make(chan struct{})
	go func() { w.afterTestCases(context.Background()); close(asked) }()
	within(t, asked, "the final read's ask, without waiting for the read")
	within(t, r.asked, "the final read")
	if utils.MarkInterruptedAgain() {
		t.Fatal("the CA watch stood an offer while the drain saved: the press gave the drain up")
	}
	close(f.started)
	within(t, drained, "the drain")
	if n := got.Load(); n != held {
		t.Fatalf("the drain saved %d of the %d mocks the agent held", n, held)
	}
	// The stop then waits for the read before it lets the agent go, the
	// frame drains over: saying so, offering nothing (a record drain may
	// still select on utils.StopNow); the read's own bound ends it.
	waited := make(chan struct{})
	go func() { w.waitForReads(); close(waited) }()
	saidWaiting(t, logs)
	if utils.MarkInterruptedAgain() {
		t.Fatal("the stop's wait for the read stood an offer")
	}
	select {
	case <-utils.StopNow():
		t.Fatal("a press at the stop's wait for the read gave the stop's waits up")
	default:
	}
	giveUp(t, clk)
	within(t, waited, "the stop's wait, the read given up on its bound")
	within(t, finishing(w), "finish")
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("said %d WARNs of a count it never read: %v", n, logs.All())
	}
}

// saidWaiting waits for the watch to say it waits for its reads.
func saidWaiting(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second) // bounds only a wait that never says so
	for logs.FilterMessageSnippet("Still waiting for the keploy agent").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the wait never said it waits for the read: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
}

// finish comes after every drain: there, once it has said it waits for a read
// the agent does not answer, one more Ctrl+C stops it now, and ends the read.
func TestFinishOffersOneMoreCtrlC(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	stoppedRun(t)
	r := newWedgedReader(2, nil)
	core, logs := observer.New(zapcore.InfoLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	manualStall(w)
	w.afterTestCases(context.Background())
	within(t, r.asked, "the final read")
	done := finishing(w)
	deadline := time.Now().Add(30 * time.Second) // bounds only a finish that never offers
	for logs.FilterMessageSnippet("press Ctrl+C again").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("finish never offered: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer finish's offer")
	}
	within(t, done, "finish")
	within(t, r.ended, "the end of the read")
	if utils.MarkInterruptedAgain() {
		t.Fatal("finish's offer still stood after it ended")
	}
}

// A test case the drain saves after the final read, whose own read fails
// (the agent went once its streams closed), is not checked: what the final
// read saw beyond the last counted read is said to be in it or after the
// last test case, and it is named, never all of it after the last.
func TestATestCaseWhoseReadNeverCameIsNamedNotChecked(t *testing.T) {
	instr := &countingInstr{
		answers: []models.CaptureLosses{nil, nil, tlsFailed(1, "to a:443")},
		errAt:   map[int]error{4: errors.New("the agent went")},
	}
	t.Setenv(provenswitch.Env, "0")
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background()) // the native order: the final read before the drain
	settle(w)
	w.afterTestCase(context.Background(), "test-2") // saved by the drain; its read fails
	settle(w)
	w.finish("test-set-0", false)
	all := logs.All()
	if len(all) != 1 || all[0].Message != "1 "+caWarn {
		t.Fatalf("logged %v, want one WARN %q", all, "1 "+caWarn)
	}
	f := all[0].ContextMap()
	step, _ := f["next_step"].(string)
	if f["afterTheLastTestCase"] != nil || f["inTheseOrAfterTheLastTestCase"] != uint64(1) ||
		fmt.Sprint(f["testCasesNotChecked"]) != "[test-2]" || !strings.HasSuffix(step, "re-record these test cases") {
		t.Fatalf("fields %v; want 1 in test-2 or after the last, test-2 not checked, and to re-record it", f)
	}

	// When the final read saw nothing beyond the last counted one, it saw
	// what the unread test case did too: it is not named as not checked.
	covered := &countingInstr{
		answers: []models.CaptureLosses{nil, tlsFailed(1, "to a:443"), tlsFailed(1, "to a:443")},
		errAt:   map[int]error{4: errors.New("the agent went")},
	}
	core, logs = observer.New(zapcore.WarnLevel)
	w = watchCaptureLosses(context.Background(), zap.New(core), covered)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background())
	settle(w)
	w.afterTestCase(context.Background(), "test-2")
	settle(w)
	w.finish("test-set-0", false)
	all = logs.All()
	if len(all) != 1 || all[0].ContextMap()["testCasesNotChecked"] != nil || fmt.Sprint(all[0].ContextMap()["testCases"]) != "[test-1]" {
		t.Fatalf("logged %v; want test-1 named, and nothing said not checked", all)
	}
}

// The base read is bounded as the others are: one the agent never answers is
// given up once keploy had stall.HangBudget to run, and nothing is watched.
func TestTheBaseReadIsGivenUpOnItsStall(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	clk := &stall.ManualClock{}
	caReadClock = clk
	t.Cleanup(func() { caReadClock = nil })
	r := newWedgedReader(1, nil)
	got := make(chan *captureWatch, 1)
	go func() { got <- watchCaptureLosses(context.Background(), zap.NewNop(), r) }()
	within(t, r.asked, "the base read")
	giveUp(t, clk)
	select {
	case w := <-got:
		if w.ca != nil {
			t.Fatal("a base read given up began a watch")
		}
	case <-time.After(30 * time.Second): // bounds only a read never given up
		t.Fatal("the base read was never given up")
	}
}

// Once finish has said what it had to, a late ask (a consumer of a record
// drain that gave up) reads nothing.
func TestAnAskAfterFinishReadsNothing(t *testing.T) {
	instr := &countingInstr{answers: []models.CaptureLosses{nil}}
	t.Setenv(provenswitch.Env, "0")
	w := watchCaptureLosses(context.Background(), zap.NewNop(), instr)
	w.finish("test-set-0", false)
	w.afterTestCase(context.Background(), "test-1")
	w.afterTestCases(context.Background())
	settle(w)
	if instr.reads != 1 {
		t.Fatalf("%d reads; an ask after finish read again", instr.reads)
	}
}

// A read the agent never answers is given up once keploy had the whole
// stall.HangBudget of its own time to run: a stop nobody presses Ctrl+C at
// (CI's pkill -INT) still ends. Wall time that passed while keploy could not
// run is no part of it: a starved keploy waits for a slow agent.
func TestAReadTheAgentNeverAnswersIsGivenUpOnItsStall(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	r := newWedgedReader(2, nil, tlsFailed(1, "to a:443"))
	core, logs := observer.New(zapcore.InfoLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	clk := manualStall(w)
	asked := make(chan struct{})
	go func() { w.afterTestCase(context.Background(), "test-1"); close(asked) }()
	within(t, asked, "test-1's ask, without waiting for its read")
	within(t, r.asked, "test-1's read")
	w.afterTestCases(context.Background())
	done := finishing(w)
	for i := 0; i < 10; i++ {
		clk.Pass() // wall time passes; keploy cannot run
	}
	select {
	case <-done:
		t.Fatal("a read was given up for wall time keploy could not run")
	default:
	}
	giveUp(t, clk)
	within(t, r.ended, "the end of test-1's read")
	within(t, done, "finish")
	// test-1's read was given up: the next read counts it.
	all := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(all) != 1 || all[0].Message != "1 "+caWarn {
		t.Fatalf("logged %v, want one WARN %q", all, "1 "+caWarn)
	}
	if got, _ := all[0].ContextMap()["testCases"].([]interface{}); len(got) != 1 || got[0] != "test-1" {
		t.Fatalf("testCases %v, want [test-1]", all[0].ContextMap()["testCases"])
	}
}

// The interrupt that gave the stop's waits up (utils.StopNow: a drain's offer
// answered) ends a read the agent never answers at once.
func TestAReadEndsWhenTheUserGaveTheStopsWaitsUp(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	stoppedRun(t)
	r := newWedgedReader(2, nil)
	w := watchCaptureLosses(context.Background(), zap.NewNop(), r)
	manualStall(w)
	w.afterTestCases(context.Background())
	within(t, r.asked, "the final read")
	_, withdraw := utils.OfferStopNow() // a drain said another interrupt stops it now
	defer withdraw()
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer the drain's offer")
	}
	within(t, r.ended, "the end of the read")
	within(t, finishing(w), "finish")
}

// Test cases saved while a read is out are counted by the next read, together:
// a read that lags names every test case saved since the last.
func TestAReadThatLagsNamesEveryTestCaseItCounts(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	o := &orderedReader{perTest: make(chan struct{}), release: make(chan struct{})}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), o)
	w.afterTestCase(context.Background(), "test-1")
	<-o.perTest
	w.afterTestCase(context.Background(), "test-2")
	w.afterTestCase(context.Background(), "test-3")
	close(o.release) // test-1's read: 1; the next, for test-2 and test-3: 2
	w.afterTestCases(context.Background())
	w.finish("test-set-0", false)
	all := logs.All()
	if len(all) != 1 || all[0].Message != "2 "+caWarn {
		t.Fatalf("logged %v, want one WARN %q", all, "2 "+caWarn)
	}
	if got := fmt.Sprint(all[0].ContextMap()["testCases"]); got != "[test-1 test-2 test-3]" {
		t.Fatalf("testCases %s, want [test-1 test-2 test-3]", got)
	}
}

// An agent started again before the final read counts from nothing: what the
// final read sees is all after the last test case.
func TestAnAgentStartedAgainBeforeTheFinalRead(t *testing.T) {
	instr := &countingInstr{answers: []models.CaptureLosses{nil, tlsFailed(5, "to a:443"), tlsFailed(2, "to b:443")}}
	_, logs := recordOff(t, instr, "test-1")
	all := logs.All()
	if len(all) != 1 || all[0].Message != "7 "+caWarn || all[0].ContextMap()["afterTheLastTestCase"] != uint64(2) {
		t.Fatalf("logged %v, want 7, 2 of them after the last test case", all)
	}
}

// No goroutine of the watch's outlives its reads.
func TestNoGoroutineOutlivesTheReads(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		recordOff(t, &countingInstr{answers: []models.CaptureLosses{nil, tlsFailed(1, "to a:443")}}, "test-1", "test-2")
	}
	deadline := time.Now().Add(30 * time.Second) // bounds only goroutines that never end
	for runtime.NumGoroutine() > before+5 {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines after 20 recordings, %d before", runtime.NumGoroutine(), before)
		}
		time.Sleep(time.Millisecond)
	}
}

// deafReader answers its first read at once, and its second only once
// released, whatever its context: a read that does not end with it.
type deafReader struct {
	Instrumentation
	n       atomic.Int32
	asked   chan struct{}
	release chan struct{}
}

func (d *deafReader) GetCaptureLosses(context.Context) (models.CaptureLosses, error) {
	if d.n.Add(1) == 1 {
		return nil, nil
	}
	close(d.asked)
	<-d.release
	return tlsFailed(1, "to a:443"), nil
}

// A read that does not end with its context is given up on its stall all the
// same, and its answer, coming after, is not counted.
func TestAnAnswerAfterTheReadWasGivenUpIsNotCounted(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	r := &deafReader{asked: make(chan struct{}), release: make(chan struct{})}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	clk := manualStall(w)
	w.afterTestCases(context.Background())
	within(t, r.asked, "the final read")
	done := finishing(w)
	giveUp(t, clk)
	within(t, done, "finish")
	close(r.release)
	settle(w)
	if logs.Len() != 0 {
		t.Fatalf("counted an answer that came after its read was given up: %v", logs.All())
	}
}

// A press at finish's offer while a test case's read is out names that test
// case as not checked: it is neither dropped nor taken for after the last.
func TestAPressAtFinishNamesTheTestCaseItsReadWasFor(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	stoppedRun(t)
	r := newWedgedReader(4, nil, nil, tlsFailed(1, "to a:443")) // base, test-1, the final read, then test-2's: never answered
	core, logs := observer.New(zapcore.InfoLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	manualStall(w)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background()) // the native order: before the drain
	settle(w)
	w.afterTestCase(context.Background(), "test-2") // saved by the drain
	within(t, r.asked, "test-2's read")
	done := finishing(w)
	deadline := time.Now().Add(30 * time.Second) // bounds only a finish that never offers
	for logs.FilterMessageSnippet("press Ctrl+C again").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("finish never offered: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer finish's offer")
	}
	within(t, done, "finish")
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("logged %v, want one WARN", warns)
	}
	f := warns[0].ContextMap()
	if f["afterTheLastTestCase"] != nil || f["inTheseOrAfterTheLastTestCase"] != uint64(1) || fmt.Sprint(f["testCasesNotChecked"]) != "[test-2]" {
		t.Fatalf("fields %v; want 1 in test-2 or after the last, test-2 not checked", f)
	}
}

// The stop's wait for the reads after the frame drains is ended by no
// interrupt: neither a press nor the copy of the stop's own signal sudo hands
// on, which keploy cannot tell apart, cuts it short. The read's own bound
// ends it.
func TestNoInterruptCutsTheStopsWaitForTheReadsShort(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	stoppedRun(t)
	r := newWedgedReader(2, nil)
	core, logs := observer.New(zapcore.DebugLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	clk := manualStall(w)
	w.afterTestCases(context.Background())
	within(t, r.asked, "the final read")
	waited := make(chan struct{})
	var all atomic.Bool
	go func() { all.Store(w.ca.wait(false)); close(waited) }()
	saidWaiting(t, logs)
	for i := 0; i < 3; i++ {
		if utils.MarkInterruptedAgain() {
			t.Fatal("the stop's wait for the reads stood an offer")
		}
	}
	if n := logs.FilterMessageSnippet("press Ctrl+C").Len(); n != 0 {
		t.Fatalf("the stop's wait said a press stops it: %v", logs.All())
	}
	if n := logs.FilterMessageSnippet("Ctrl+C does not cut this short: each read is given up once keploy has had " + stall.HangBudget.String()).Len(); n != 1 {
		t.Fatalf("the stop's wait did not say Ctrl+C does not cut it short, and each read's bound: %v", logs.All())
	}
	giveUp(t, clk)
	within(t, waited, "the end of the wait")
	if !all.Load() {
		t.Fatal("an interrupt cut the stop's wait for the reads short")
	}
	gaveUpOnItsBound := false
	for _, e := range logs.FilterMessageSnippet("could not read its agent's count").All() {
		if err, _ := e.ContextMap()["error"].(string); err == errCountSilent.Error() {
			gaveUpOnItsBound = true
		}
	}
	if !gaveUpOnItsBound {
		t.Fatalf("the read did not end on its own bound: an interrupt cut the wait short: %v", logs.All())
	}
}

// With the final read failed, test cases whose reads never came are named as
// not checked beside those counted; and when nothing was counted at all, they
// are named all the same: unknown is not none.
func TestUnreadTestCasesWithoutAFinalRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	instr := &countingInstr{
		answers: []models.CaptureLosses{nil, tlsFailed(1, "to a:443")},
		errAt:   map[int]error{3: errors.New("the agent went"), 4: errors.New("the agent went")},
	}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background()) // fails
	settle(w)
	w.afterTestCase(context.Background(), "test-2") // fails
	settle(w)
	w.finish("test-set-0", false)
	all := logs.All()
	if len(all) != 1 || fmt.Sprint(all[0].ContextMap()["testCases"]) != "[test-1]" || fmt.Sprint(all[0].ContextMap()["testCasesNotChecked"]) != "[test-2]" {
		t.Fatalf("logged %v; want test-1 counted and test-2 not checked", all)
	}

	none := &countingInstr{answers: []models.CaptureLosses{nil}, errAt: map[int]error{2: errors.New("the agent went"), 3: errors.New("the agent went")}}
	core, logs = observer.New(zapcore.WarnLevel)
	w = watchCaptureLosses(context.Background(), zap.New(core), none)
	w.afterTestCase(context.Background(), "test-1")
	settle(w)
	w.afterTestCases(context.Background())
	settle(w)
	w.finish("test-set-0", false)
	all = logs.All()
	if len(all) != 1 || !strings.HasPrefix(all[0].Message, "keploy could not check") || fmt.Sprint(all[0].ContextMap()["testCasesNotChecked"]) != "[test-1]" {
		t.Fatalf("nothing was counted and test-1 was not checked; it said %v", all)
	}
}

// A press at finish's offer with nothing counted yet, a test case's read
// out (a copy of the stop's signal taken for a press does the same), says
// that test case was not checked: the WARN is never dropped unsaid.
func TestAPressAtFinishWithNothingCountedNamesWhatWasNotChecked(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	stoppedRun(t)
	r := newWedgedReader(2, nil) // base, then test-1's read: never answered
	core, logs := observer.New(zapcore.InfoLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	manualStall(w)
	w.afterTestCase(context.Background(), "test-1")
	within(t, r.asked, "test-1's read")
	done := finishing(w)
	deadline := time.Now().Add(30 * time.Second) // bounds only a finish that never offers
	for logs.FilterMessageSnippet("press Ctrl+C again").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("finish never offered: %v", logs.All())
		}
		time.Sleep(time.Millisecond)
	}
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer finish's offer")
	}
	within(t, done, "finish")
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 1 || fmt.Sprint(warns[0].ContextMap()["testCasesNotChecked"]) != "[test-1]" {
		t.Fatalf("logged %v; want test-1 said not checked", warns)
	}
}

// A read made since the final one was asked, which saw nothing beyond the
// last counted, covers a test case whose own read never came (its calls came
// before the stop): it is not said to be unchecked.
func TestAReadAfterTheFinalOneCoversAnUnreadTestCase(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	instr := &countingInstr{
		answers: []models.CaptureLosses{nil, nil, nil},
		errAt:   map[int]error{4: errors.New("the agent went")},
	}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	w.afterTestCases(context.Background()) // the native order: the final read, before the drain
	settle(w)
	w.afterTestCase(context.Background(), "test-1") // saved by the drain, read: nothing more
	settle(w)
	w.afterTestCase(context.Background(), "test-2") // saved by the drain; its read fails
	settle(w)
	w.finish("test-set-0", false)
	if logs.Len() != 0 {
		t.Fatalf("a clean recording said %v", logs.All())
	}
}

// takenBeforeFinalReader answers the base read at once; test-1's read (the
// second) once released, with nothing failed; and fails every read after.
type takenBeforeFinalReader struct {
	Instrumentation
	n       atomic.Int32
	asked   chan struct{}
	release chan struct{}
}

func (r *takenBeforeFinalReader) GetCaptureLosses(context.Context) (models.CaptureLosses, error) {
	switch r.n.Add(1) {
	case 1:
		return nil, nil
	case 2:
		close(r.asked)
		<-r.release
		return nil, nil
	default:
		return nil, errors.New("the agent went")
	}
}

// Only a read taken after the final read was asked covers an unread test
// case: one taken before it (here test-1's, still out as the final read is
// asked, and answered after) may hold a count from before the stop, and the
// final read itself failed, so test-2, whose read never came, is said to be
// unchecked.
func TestAReadTakenBeforeTheFinalOneCoversNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	r := &takenBeforeFinalReader{asked: make(chan struct{}), release: make(chan struct{})}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), r)
	w.afterTestCase(context.Background(), "test-1")
	within(t, r.asked, "test-1's read")
	w.afterTestCases(context.Background()) // asked while test-1's read is out
	close(r.release)                       // test-1's read answers after the final read was asked
	settle(w)
	w.afterTestCase(context.Background(), "test-2") // its read fails
	settle(w)
	w.finish("test-set-0", false)
	all := logs.All()
	if len(all) != 1 || fmt.Sprint(all[0].ContextMap()["testCasesNotChecked"]) != "[test-2]" {
		t.Fatalf("logged %v; want test-2 said not checked", all)
	}
}
