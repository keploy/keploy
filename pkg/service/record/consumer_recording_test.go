package record

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A DEGRADED CONSUMER RECORDING MUST FAIL THE RECORDING.
//
// Design §3 R6: "record teardown prints N consumer units observed, M test
// cases persisted, K revoked and FAILS the recording with CONSUMER_UNITS_LOST
// when M < N". Until this existed the only surface was one zap ERROR line in
// the AGENT's log — a different process from `keploy record` in the ordinary
// CLI deployment — so a user who seeded ten messages and got eight test files
// got exit 0, and an agent loop keying on the exit code went straight on to
// replay a suite that was silently short. A unit refused by name and a unit
// that vanished produce the same number of files, so the file count cannot say
// it either.
//
// The whole hookup was deletable with the suite green before this file.

// reportingInstrumentation is an Instrumentation that also implements the
// optional ConsumerRecordingReporter.
type reportingInstrumentation struct {
	Instrumentation
	report models.ConsumerRecordingReport
	err    error
	calls  int
}

func (r *reportingInstrumentation) ConsumerRecordingReport(context.Context) (models.ConsumerRecordingReport, error) {
	r.calls++
	return r.report, r.err
}

// plainInstrumentation implements Instrumentation and nothing else — an
// embedder's own, or an agent client that predates the capability.
type plainInstrumentation struct{ Instrumentation }

func newRecorderWith(inst Instrumentation) (*Recorder, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return &Recorder{logger: zap.New(core), instrumentation: inst}, logs
}

func TestReportConsumerRecording(t *testing.T) {
	restore := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = restore })

	t.Run("a lost unit fails the recording", func(t *testing.T) {
		utils.ErrCode = 0
		inst := &reportingInstrumentation{report: models.ConsumerRecordingReport{
			UnitsObserved:  10,
			UnitsPersisted: 8,
			UnitsLost:      2,
			Problems:       []string{string(models.CategoryConsumerUnitsLost) + ": 2 of 10 consumer units were neither persisted nor refused by name"},
		}}
		r, logs := newRecorderWith(inst)
		r.reportConsumerRecording()

		if utils.ErrCode == 0 {
			t.Fatal("a recording that produced fewer test cases than the user watched being made must exit non-zero; " +
				"an agent loop keys on the exit code and would replay a suite that is silently short")
		}
		if logs.FilterMessage("this consumer recording is not trustworthy and must not be replayed as-is").Len() != 1 {
			t.Fatalf("the reason must be named out loud, got %v", logs.All())
		}
	})

	t.Run("a clean consumer recording exits zero and still reports its counts", func(t *testing.T) {
		utils.ErrCode = 0
		inst := &reportingInstrumentation{report: models.ConsumerRecordingReport{
			UnitsObserved: 10, UnitsPersisted: 10,
		}}
		r, logs := newRecorderWith(inst)
		r.reportConsumerRecording()

		if utils.ErrCode != 0 {
			t.Fatal("a recording that reconciled must not fail")
		}
		if logs.FilterMessage("consumer recording reconciliation").Len() != 1 {
			t.Fatalf("the N/M/K reconciliation design §3 R6 asks for is missing: %v", logs.All())
		}
	})

	t.Run("an HTTP-only recording is silent", func(t *testing.T) {
		// Every recording this repository can make on its own: no OSS parser
		// stamps role metadata, so no consumer unit is ever observed. A line
		// here would be pure noise on a path this contract must not touch.
		utils.ErrCode = 0
		r, logs := newRecorderWith(&reportingInstrumentation{})
		r.reportConsumerRecording()

		if utils.ErrCode != 0 || logs.Len() != 0 {
			t.Fatalf("errCode=%d logs=%v", utils.ErrCode, logs.All())
		}
	})

	t.Run("a fetch failure is not a recording failure", func(t *testing.T) {
		// An agent that is already gone, or one that predates the route. A
		// failed request must never invent a recording failure.
		utils.ErrCode = 0
		inst := &reportingInstrumentation{err: errors.New("connection refused")}
		r, _ := newRecorderWith(inst)
		r.reportConsumerRecording()

		if utils.ErrCode != 0 {
			t.Fatal("a failed fetch is a keploy problem, not evidence that the recording is short")
		}
	})

	t.Run("an instrumentation without the capability is not asked", func(t *testing.T) {
		utils.ErrCode = 0
		r, logs := newRecorderWith(plainInstrumentation{})
		r.reportConsumerRecording()

		if utils.ErrCode != 0 || logs.Len() != 0 {
			t.Fatalf("errCode=%d logs=%v", utils.ErrCode, logs.All())
		}
	})
}

// AND THE TEARDOWN MUST STILL CALL IT. The behaviour above proves the check
// works; it cannot prove Start's teardown runs it, and Start is not reachable
// from a unit test. Deleting the call left every assertion above green.
//
// It must sit AFTER NotifyGracefulShutdown, which is what closes the consumer
// recorder and mints its last unit — asked before that, the agent would answer
// with an unfinished session.
func TestTeardownReportsTheConsumerRecordingAfterTheShutdownNotification(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "record.go", nil, 0)
	if err != nil {
		t.Fatalf("parse record.go: %v", err)
	}

	var order []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "NotifyGracefulShutdown", "reportConsumerRecording":
			order = append(order, sel.Sel.Name)
		}
		return true
	})

	notifyAt, reportAt := -1, -1
	for i, name := range order {
		if name == "NotifyGracefulShutdown" && notifyAt < 0 {
			notifyAt = i
		}
		if name == "reportConsumerRecording" && reportAt < 0 {
			reportAt = i
		}
	}
	if reportAt < 0 {
		t.Fatal("record.go's teardown no longer calls reportConsumerRecording.\n\n" +
			"WHY THIS MATTERS: it is the only thing that turns a degraded consumer recording into a " +
			"non-zero exit. Without it a user who seeded ten messages and got eight test files gets " +
			"exit 0, and an agent loop replays a suite that is silently short (design §3 R6).")
	}
	if notifyAt < 0 || reportAt < notifyAt {
		t.Fatalf("reportConsumerRecording must come after NotifyGracefulShutdown; call order was %v.\n\n"+
			"WHY THIS MATTERS: the shutdown notification is what closes the consumer recorder and mints "+
			"its last unit. Asked before it, the agent answers with an unfinished session and the last "+
			"message of every recording looks lost.", order)
	}
}
