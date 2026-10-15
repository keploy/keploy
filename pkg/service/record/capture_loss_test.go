package record

import (
	"context"
	"errors"
	"fmt"
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
