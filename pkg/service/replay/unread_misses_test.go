package replay

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"
	"time"

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
// off the verdict is what it was (the set PASSED, no test unjudged), but the
// test is marked MissesUnattributed: a miss may be missing from it, or read
// with another test's, and nothing may learn noise from its set. The failure
// was a Debug line, and the miss then masked.
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

				var unread, unattributed []string
				for _, r := range h.report.report.Tests {
					if r.FailureInfo.UnmatchedCallsUnread {
						unread = append(unread, r.TestCaseID)
					}
					if r.FailureInfo.MissesUnattributed {
						unattributed = append(unattributed, r.TestCaseID)
					}
					if len(r.VerdictUnknown()) > 0 && !on {
						t.Errorf("switch off: %s has no verdict; with the switch off a miss fails no test, so misses read short leave its verdict", r.TestCaseID)
					}
				}
				if !on {
					if status != models.TestSetStatusPassed || len(unread) != 0 || fmt.Sprint(unattributed) != "[test-2]" {
						t.Fatalf("switch off: status %s, unread %v, unattributed %v; want PASSED, none unread, and test-2 marked unattributed", status, unread, unattributed)
					}
					return
				}
				if len(unattributed) != 0 {
					t.Fatalf("under the switch the unread test is UnmatchedCallsUnread, not MissesUnattributed: %v", unattributed)
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
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("switch=%v", on), func(t *testing.T) {
			v := ""
			if on {
				v = "1"
			}
			t.Setenv(provenswitch.Env, v)
			h := newPartialRunHarness(t, 1, 0)
			h.instr.mockErrorsFor = misses(incompleteRead)
			result := &models.TestResult{}
			h.replayer.attachMockErrors(t.Context(), "test-set-0", "test-1", result)
			// Under the switch the test is UnmatchedCallsUnread (no verdict);
			// with it off, MissesUnattributed (its verdict stands).
			if result.FailureInfo.UnmatchedCallsUnread != on || result.FailureInfo.MissesUnattributed == on {
				t.Fatalf("unread %v, unattributed %v; want unread under the switch and unattributed with it off",
					result.FailureInfo.UnmatchedCallsUnread, result.FailureInfo.MissesUnattributed)
			}
			if len(result.FailureInfo.UnmatchedCalls) != 1 || result.FailureInfo.UnmatchedCalls[0].ActualSummary != "GET /read" {
				t.Fatalf("unmatched calls %+v; the calls the read did return must be kept", result.FailureInfo.UnmatchedCalls)
			}
		})
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
// metadata against the expected answer as the replay renders it: a
// content-type that differs from the recording (not a +codec of the same
// type, which the matcher takes as the same) is a judged FAILED row, its
// answer what came (the metadata, no message); one that matches, or none, is
// no difference (the test fails closed).
func TestAGRPCAnswersStartIsJudgedOnItsMetadata(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	view := &models.TestCase{Kind: models.GRPC_EXPORT, Name: "test-1"}
	view.GrpcResp.Headers = models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":status": "200"},
		OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
	}
	view.GrpcResp.Body = models.GrpcLengthPrefixedMessage{DecodedData: `1: {"REC"}`}
	view.GrpcResp.Trailers = models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "0"}}
	if _, differs := r.startFailedRow(nil, view, view, &models.GrpcResp{Headers: view.GrpcResp.Headers}, "s", time.Now()); differs {
		t.Fatal("a start that matches the recording differs")
	}
	other := models.GrpcHeaders{
		PseudoHeaders:   map[string]string{":status": "200"},
		OrdinaryHeaders: map[string]string{"content-type": "text/plain"},
	}
	row, differs := r.startFailedRow(nil, view, view, &models.GrpcResp{Headers: other}, "s", time.Now())
	if !differs || row == nil || row.Status != models.TestStatusFailed || row.GrpcRes.Headers.OrdinaryHeaders["content-type"] != "text/plain" || row.GrpcRes.Body.DecodedData != "" || row.FailureInfo.AnswerStalled != "" {
		t.Fatalf("%v, %+v; want a judged FAILED row holding what came", differs, row)
	}
	var none *models.GrpcResp
	if _, differs := r.startFailedRow(nil, view, view, none, "s", time.Now()); differs {
		t.Fatal("no answer differs")
	}
}

// An HTTP start is judged on its status and headers. A header that holds a
// capture is resolved as the main path resolves it, on a throwaway run: the
// app's value is taken where the literal text and uses around it match (a
// cookie by its parts, its Expires date standing when it gives the recorded
// lifetime), and the run itself moves no value. A start that differs is a
// judged FAILED row whose answer is what came.
func TestAnHTTPAnswersStartIsJudgedWithItsTemplatedHeadersTheApps(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	run := templates.NewRun(zap.NewNop(), map[string]interface{}{"sid": "S0rec", "token": "T0rec"}, nil)
	sent := &models.TestCase{Kind: models.HTTP, Name: "test-1", HTTPResp: models.HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type": "application/json",
			"Date":         "Mon, 05 Oct 2026 10:00:00 GMT",
			"Set-Cookie":   "sid={{capture string .sid}}; Expires=Mon, 05 Oct 2026 11:00:00 GMT; Path=/",
		},
		Body: `{"token":"{{capture string .token}}"}`,
	}}
	view := noAnswerView(run, sent)
	answer := func(status int, cookie string) *models.HTTPResp {
		return &models.HTTPResp{StatusCode: status, Header: map[string]string{
			"Content-Type": "application/json",
			"Date":         "Sat, 17 Oct 2026 09:00:00 GMT",
			"Set-Cookie":   cookie,
		}, Body: `{"tok`}
	}
	same := answer(200, "sid=S1live; Expires=Sat, 17 Oct 2026 10:00:00 GMT; Path=/")
	if _, differs := r.startFailedRow(run, sent, view, same, "s", time.Now()); differs {
		t.Fatal("a start whose templated cookie the app filled, its Expires moved by the same lifetime, differs")
	}
	if got := run.Values()["sid"]; got != "S0rec" {
		t.Fatalf("judging the start moved the run's sid to %q", got)
	}
	path := answer(200, "sid=S1live; Expires=Sat, 17 Oct 2026 10:00:00 GMT; Path=/admin")
	if _, differs := r.startFailedRow(run, sent, view, path, "s", time.Now()); !differs {
		t.Fatal("a templated cookie whose Path differs was taken as the app's")
	}
	row, differs := r.startFailedRow(run, sent, view, answer(500, same.Header["Set-Cookie"]), "s", time.Now())
	if !differs || row.Res.StatusCode != 500 || row.Res.Body != `{"tok` || row.Result.StatusCode.Normal {
		t.Fatalf("%v, %+v; want a judged FAILED row on the 500, holding what came", differs, row)
	}
	for _, b := range row.Result.BodyResult {
		if b.Actual != `{"tok` {
			t.Fatalf("the result's body row says %q came; %q did", b.Actual, `{"tok`)
		}
	}
	// In a set with no template, a header is compared as written.
	plain := &models.TestCase{Kind: models.HTTP, Name: "test-2", HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"X-Literal": "{{not a template}}"}}}
	if _, differs := r.startFailedRow(nil, plain, plain, &models.HTTPResp{StatusCode: 200, Header: map[string]string{"X-Literal": "anything"}}, "s", time.Now()); !differs {
		t.Fatal("a header written with braces in a set with no template was taken as a pattern")
	}
}

// A judged start's headers are those the main path would compare: a use of
// a key a cookie of the same answer captures is at the captured value, and a
// use of a key the body captures (a created resource's Location) is the
// app's, its value being in the body that never came. A start that matches
// so fails closed; a header at the old value is a judged difference.
func TestAStartsHeaderThatUsesAValueTheSameAnswerCapturesIsJudgedAsTheMainPathJudgesIt(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	run := templates.NewRun(zap.NewNop(), map[string]interface{}{"id": "REC", "sid": "S1"}, nil)
	sent := &models.TestCase{Kind: models.HTTP, Name: "test-1", HTTPResp: models.HTTPResp{
		StatusCode: 201,
		Header: map[string]string{
			"Content-Type": "application/json",
			"Location":     "/orders/{{string .id}}",
			"Set-Cookie":   "sid={{capture string .sid}}; Path=/",
			"X-Sid":        "{{string .sid}}",
		},
		Body: `{"id":"{{capture string .id}}"}`,
	}}
	view := noAnswerView(run, sent)
	answer := func(sid string) *models.HTTPResp {
		return &models.HTTPResp{StatusCode: 201, Header: map[string]string{
			"Content-Type": "application/json",
			"Location":     "/orders/NEW",
			"Set-Cookie":   "sid=S2; Path=/",
			"X-Sid":        sid,
		}}
	}
	if row, differs := r.startFailedRow(run, sent, view, answer("S2"), "s", time.Now()); differs {
		t.Fatalf("a start the main path would match was judged FAILED: %+v", row.Result.HeadersResult)
	}
	if _, differs := r.startFailedRow(run, sent, view, answer("S1"), "s", time.Now()); !differs {
		t.Fatal("a header at the value its cookie's capture replaced was taken as matching")
	}
	if got := run.Values(); got["id"] != "REC" || got["sid"] != "S1" {
		t.Fatalf("judging the start moved the run's values to %v", got)
	}
}

// drainOrder is prHooks that records, when each test is sent, how many reads
// of the consumed mocks came before it.
type drainOrder struct {
	prHooks
	mu      *sync.Mutex
	reads   *int
	atStart map[string]int
}

func (d drainOrder) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	d.mu.Lock()
	d.atStart[tc.Name] = *d.reads
	d.mu.Unlock()
	return d.prHooks.SimulateRequest(ctx, tc, set)
}

func (d drainOrder) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	d.mu.Lock()
	*d.reads++
	d.mu.Unlock()
	return d.prHooks.GetConsumedMocks(ctx)
}

// Under the proven switch a test that ends early (here, failing closed on an
// answer that never came) reads what it consumed before its row is filed, so
// before the next test runs: its calls are its own, never the next test's
// drain's. Without the switch an early row files as it always did, its calls
// read only by the next test's drain.
func TestUnderTheSwitchAnEarlyRowsConsumptionIsItsOwn(t *testing.T) {
	for _, proven := range []string{"1", ""} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			h := newPartialRunHarness(t, 2, 0)
			var mu sync.Mutex
			reads := 0
			hooks := drainOrder{
				prHooks: prHooks{simErr: map[string]error{"test-1": &pkg.AnswerStallError{Target: "GET /x"}}},
				mu:      &mu, reads: &reads, atStart: map[string]int{},
			}
			h.replayer.hookImpl = hooks
			h.run(t)
			mu.Lock()
			defer mu.Unlock()
			between := hooks.atStart["test-2"] - hooks.atStart["test-1"]
			if want := map[string]int{"1": 1, "": 0}[proven]; between != want {
				t.Fatalf("%d read(s) of the consumed mocks between test-1's send and test-2's; want %d", between, want)
			}
		})
	}
}
