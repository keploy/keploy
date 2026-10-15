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
