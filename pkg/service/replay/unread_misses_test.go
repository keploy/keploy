package replay

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// misses answers the harness's per-test GetMockErrors calls in test order:
// reads[i] is what the agent hands back for the (i+1)th test.
func misses(reads ...func() ([]models.UnmatchedCall, error)) func() ([]models.UnmatchedCall, error) {
	var mu sync.Mutex
	n := 0
	return func() ([]models.UnmatchedCall, error) {
		mu.Lock()
		defer mu.Unlock()
		i := n
		n++
		if i < len(reads) && reads[i] != nil {
			return reads[i]()
		}
		return nil, nil
	}
}

func incompleteRead() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /read"}},
		fmt.Errorf("%w (agent: keploy's error drain stopped)", models.ErrUnmatchedCallsUnread)
}

func failedRead() ([]models.UnmatchedCall, error) {
	return nil, errors.New("get mock errors returned status 500")
}

// A test whose misses could not all be read has no verdict under the switch,
// and neither has its set: INTERNAL_ERR, with the tests named. With the switch
// off the run is what it was (the failure is a Debug line, the set PASSED).
func TestRunTestSet_UnreadMissesLeaveTheSetUnjudged(t *testing.T) {
	for _, read := range []struct {
		name string
		fn   func() ([]models.UnmatchedCall, error)
	}{{"incomplete read", incompleteRead}, {"failed read", failedRead}} {
		for _, on := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/switch=%v", read.name, on), func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				h := newPartialRunHarness(t, 3, 0)
				h.instr.mockErrorsFor = misses(nil, read.fn, nil)

				status := h.run(t)

				var unread []string
				for _, r := range h.report.report.Tests {
					if r.FailureInfo.UnmatchedCallsUnread {
						unread = append(unread, r.TestCaseID)
					}
				}
				if !on {
					if status != models.TestSetStatusPassed || len(unread) != 0 {
						t.Fatalf("switch off: status %s, unread %v; want PASSED and none, as before", status, unread)
					}
					return
				}
				if status != models.TestSetStatusInternalErr {
					t.Fatalf("status %s; a set with a test whose misses were not all read must be neither PASSED nor FAILED", status)
				}
				if len(unread) != 1 || unread[0] != "test-2" {
					t.Fatalf("unread %v, want [test-2]", unread)
				}
				if reason := h.report.report.FailureReason; !strings.Contains(reason, "not judged") || !strings.Contains(reason, "test-2") {
					t.Fatalf("failure reason %q does not say the set is not judged, and for which test", reason)
				}
				if !h.replayer.notJudged("test-set-0") {
					t.Fatal("the set is not recorded as not judged: the run would stop after it, and the sets after it would get no verdict")
				}
			})
		}
	}
}

// What an incomplete read did return is still attached to its test.
func TestAttachMockErrors_KeepsWhatAnIncompleteReadReturned(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 1, 0)
	h.instr.mockErrorsFor = misses(incompleteRead)
	result := &models.TestResult{}
	h.replayer.attachMockErrors(t.Context(), "test-set-0", "test-1", result)
	if !result.FailureInfo.UnmatchedCallsUnread {
		t.Fatal("the result is not marked unread")
	}
	if len(result.FailureInfo.UnmatchedCalls) != 1 || result.FailureInfo.UnmatchedCalls[0].ActualSummary != "GET /read" {
		t.Fatalf("unmatched calls %+v; the calls the read did return must be kept", result.FailureInfo.UnmatchedCalls)
	}
}

// A set that is only not judged fails, and the run goes on to the next set so
// every set gets its verdict. Every other INTERNAL_ERR ends the round, as
// before, and so does a not-judged mark on a set that ended otherwise.
func TestAbortAfterTestSet(t *testing.T) {
	for _, tc := range []struct {
		status    models.TestSetStatus
		notJudged bool
		want      bool
	}{
		{models.TestSetStatusInternalErr, true, false},
		{models.TestSetStatusInternalErr, false, true},
		{models.TestSetStatusAppHalted, true, true},
		{models.TestSetStatusFailed, true, false},
	} {
		if got := abortAfterTestSet(tc.status, utils.Native, tc.notJudged); got != tc.want {
			t.Errorf("abortAfterTestSet(%s, notJudged=%v) = %v, want %v", tc.status, tc.notJudged, got, tc.want)
		}
	}
}

// Start decides whether to stop after an INTERNAL_ERR set through
// abortAfterTestSet, told whether that set was only not judged: the rule is
// tested above, and this pins the one call that feeds it the set's mark.
func TestStartGoesOnAfterANotJudgedSet(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "replay.go", nil, 0)
	if err != nil {
		t.Fatalf("parse replay.go: %v", err)
	}
	var start *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "Start" && fn.Recv != nil {
			start = fn
		}
	}
	if start == nil {
		t.Fatal("Replayer.Start not found in replay.go")
	}
	found := false
	ast.Inspect(start, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != "abortAfterTestSet" || len(call.Args) != 3 {
			return true
		}
		if mark, ok := call.Args[2].(*ast.CallExpr); ok && calleeName(mark) == "r.notJudged" && len(mark.Args) == 1 {
			if id, ok := mark.Args[0].(*ast.Ident); ok && id.Name == "testSet" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("Start does not call abortAfterTestSet(…, r.notJudged(testSet)): a set that is only not judged would stop the run, and the sets after it would get no verdict")
	}
}

// A set's not-judged mark is the run's of that set: a mark left by an earlier
// attempt (a flaky check) or an earlier Start does not keep a later run of
// the set, judged this time, from being judged, nor a later INTERNAL_ERR of
// it from ending the round.
func TestRunTestSetClearsTheSetsNotJudgedMark(t *testing.T) {
	h := newPartialRunHarness(t, 1, 0)
	h.replayer.markNotJudged("test-set-0")
	h.replayer.markNotJudged("test-set-9")
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("set %s; want PASSED", status)
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("the set's run left an earlier mark standing")
	}
	if !h.replayer.notJudged("test-set-9") {
		t.Fatal("another set's mark was cleared")
	}
}

// A set's not-judged reason gives what its tests could not be judged for: a
// Unix socket's count as the latest test saw it (it only grows), and each
// test's abandoned TLS handshakes added up (each is only its own test's).
func TestTestsKeployCannotCaptureAddsEachTestsAbandonedHandshakes(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ab := models.CaptureLoss{Kind: models.CaptureLossTLSAbandoned, Count: 1, What: "to 10.0.0.9:443"}
	unix := func(n uint64) models.CaptureLoss {
		return models.CaptureLoss{Kind: models.CaptureLossUnixSocket, Count: n, What: "/run/db.sock"}
	}
	results := []models.TestResult{
		{TestCaseID: "test-1", FailureInfo: models.FailureInfo{CannotCapture: models.CaptureLosses{ab}}},
		{TestCaseID: "test-2"},
		{TestCaseID: "test-3", FailureInfo: models.FailureInfo{CannotCapture: models.CaptureLosses{ab, unix(1)}}},
		{TestCaseID: "test-4", FailureInfo: models.FailureInfo{CannotCapture: models.CaptureLosses{unix(2)}}},
	}
	tests, lost := testsKeployCannotCapture(results)
	if fmt.Sprint(tests) != "[test-1 test-3 test-4]" {
		t.Fatalf("tests %v", tests)
	}
	want := models.CaptureLosses{{Kind: models.CaptureLossTLSAbandoned, Count: 2, What: "to 10.0.0.9:443"}, unix(2)}
	if fmt.Sprint(lost) != fmt.Sprint(want) {
		t.Fatalf("what %+v, want %+v", lost, want)
	}
}

// A stream the app sent only keepalives on is not changed by a bound of the
// user's, so its next step never suggests one; the stall bound's does.
func TestTheStalledAnswersNextStepFollowsWhatEndedIt(t *testing.T) {
	keepalives := stalledAnswerRemedy(&pkg.AnswerStallError{Answered: true, Keepalives: true})
	if strings.Contains(keepalives, "give the") || !strings.Contains(keepalives, "--api-timeout does not change this") {
		t.Fatalf("keepalives' next step %q suggests a bound", keepalives)
	}
	if grace := stalledAnswerRemedy(&pkg.AnswerStallError{}); !strings.Contains(grace, "give the tests a bound of your own with --api-timeout") {
		t.Fatalf("the stall bound's next step %q", grace)
	}
}

// The start of a gRPC answer that began and sent no message is judged on its
// metadata: a content-type that differs from the recording is a judged
// difference, compared with the recording's messages and trailers; one that
// matches is no difference (the test fails closed).
func TestAGRPCAnswersStartIsJudgedOnItsMetadata(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	sent := &models.TestCase{Kind: models.GRPC_EXPORT, Name: "test-1"}
	sent.GrpcResp.Headers = models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":status": "200"},
		OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
	}
	sent.GrpcResp.Trailers = models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "0"}}
	run := templates.NewRun(zap.NewNop(), nil, nil)
	same := &models.GrpcResp{Headers: sent.GrpcResp.Headers}
	if _, differs := r.answerStartDiffers(run, sent, same, "s"); differs {
		t.Fatal("a start that matches the recording differs")
	}
	other := &models.GrpcResp{Headers: models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":status": "200"},
		OrdinaryHeaders: map[string]string{"content-type": "application/grpc+json"},
	}}
	judged, differs := r.answerStartDiffers(run, sent, other, "s")
	g, ok := judged.(*models.GrpcResp)
	if !differs || !ok || g.Headers.OrdinaryHeaders["content-type"] != "application/grpc+json" || g.Trailers.OrdinaryHeaders["grpc-status"] != "0" {
		t.Fatalf("%v, %+v; want the start that differs, with the recording's rest", differs, judged)
	}
	var none *models.GrpcResp
	if _, differs := r.answerStartDiffers(run, sent, none, "s"); differs {
		t.Fatal("no answer differs")
	}
}
