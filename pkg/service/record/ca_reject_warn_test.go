package record

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
	t.Setenv(provenswitch.Env, "")
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), instr)
	for _, n := range names {
		w.afterTestCase(context.Background(), n)
	}
	w.afterTestCases(context.Background())
	return w.finish("test-set-0"), logs
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
	if failed, logs := recordOff(t, readFails, "test-1", "test-2"); failed || logs.Len() != 1 || !strings.HasPrefix(logs.All()[0].Message, "1 ") {
		t.Fatalf("a check that could not be read: failed %v, logged %v; want the next read to count it", failed, logs.All())
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
	if !w.finish("test-set-0") {
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
	t.Setenv(provenswitch.Env, "")
	var count atomic.Uint64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(tlsFailed(count.Load(), "to up:8443"))
	}))
	defer srv.Close()
	core, logs := observer.New(zapcore.WarnLevel)
	ctx, stop := context.WithCancel(context.Background())
	w := watchCaptureLosses(ctx, zap.New(core), &httpLossReader{url: srv.URL})
	w.afterTestCase(ctx, "test-1")
	count.Store(1)
	stop()                         // the stop: Start's context ends
	w.afterTestCase(ctx, "test-2") // the drain saves test-2
	count.Store(3)
	w.afterTestCases(ctx) // the teardown's final read
	w.finish("test-set-0")
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

// The final read (the teardown's goroutine) and a test case's (the
// consumer's) can be asked at once: the later one waits for the earlier to
// be counted, so an earlier, lower answer is never taken for an agent
// started again.
func TestReadsAtOnceAreCountedInOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	o := &orderedReader{perTest: make(chan struct{}), release: make(chan struct{})}
	core, logs := observer.New(zapcore.WarnLevel)
	w := watchCaptureLosses(context.Background(), zap.New(core), o)
	perTest := make(chan struct{})
	go func() { w.afterTestCase(context.Background(), "test-1"); close(perTest) }()
	<-o.perTest
	final := make(chan struct{})
	go func() { w.afterTestCases(context.Background()); close(final) }()
	// The final read must wait for test-1's to be counted: it is not asked
	// while test-1's is out (given time it would be, were it not held).
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if o.n.Load() > 2 {
			t.Fatal("the final read was asked while test-1's read was still out")
		}
	}
	close(o.release)
	<-perTest
	<-final
	w.finish("test-set-0")
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
