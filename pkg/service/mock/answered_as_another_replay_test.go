package mock

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The quickstart's shape end to end through keploy mock replay: the agent
// answered GET /users/3, absent from the recording, with the recording of
// GET /users/1 and said so in-band. The run passed on it, so it is no miss
// and keploy's own exit stays the suite's, but the summary counts it apart,
// a WARN names it with its next step, the console names it whole, the
// receipt is not isolated and lists it, and --strict fails the run on it as
// on a miss. A run with only its own answers is untouched.
func TestAReplayAnsweredWithAnotherCallsRecordingSaysSoAndStrictFailsIt(t *testing.T) {
	other := models.UnmatchedCall{
		Protocol:      "http",
		Destination:   "users:8080",
		ActualSummary: "GET /users/3",
		ClosestMock:   "mock-1",
		Answer:        models.AnswerOtherID,
		Differs:       []string{"url.path.1"},
		Diff:          "GET /users/3 was answered with the recording of GET /users/1: url.path.1 differs",
		NextSteps:     "record GET /users/3 (keploy mock record) so it is answered with its own recording",
	}
	for _, tc := range []struct {
		name         string
		read         []models.UnmatchedCall
		strict       bool
		runner       models.AppError
		wantExit     int
		wantFailedBy string
	}{
		{name: "answered with another call's recording", read: []models.UnmatchedCall{other}},
		{name: "answered with another call's recording, --strict", read: []models.UnmatchedCall{other}, strict: true, wantExit: 1, wantFailedBy: FailedByStrict},
		// The suite's own failure keeps its name and its exit.
		{name: "answered with another call's recording, --strict, the tests failed", read: []models.UnmatchedCall{other}, strict: true,
			runner: models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 7}, wantExit: 7, wantFailedBy: FailedByRunner},
		{name: "control: every call its own answer, --strict", strict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			runner := tc.runner
			if runner.AppErrorType == "" {
				runner = models.AppError{AppErrorType: models.ErrAppStopped}
			}
			instr := newInstr(t, agentUpFromSetup, false, runner)
			instr.consumedMocks = []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}
			instr.mockErrors = tc.read
			cfg := instrConfig(instr, utils.Native, "pytest -q")
			cfg.Mock.Strict = tc.strict
			// fail: no call can reach a real service, so the run is proven
			// unless something else says it is not.
			cfg.Mock.OnMiss = string(models.MissFail)
			cfg.Path = t.TempDir()
			if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
				t.Fatal(err)
			}
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			svc := New(zap.New(core), instr, loadedMockDB{n: 2}, nil, nil, nil, cfg)
			var console bytes.Buffer
			svc.(ConsoleSetter).SetConsole(&console)
			if err := svc.Replay(context.Background()); err != nil {
				t.Fatalf("Replay returned keploy's own failure %v: the agent's account was read whole", err)
			}
			if utils.ErrCode != tc.wantExit {
				t.Errorf("exit code %d, want %d", utils.ErrCode, tc.wantExit)
			}
			all := logged(logs)
			var summary map[string]any
			for _, e := range logs.All() {
				if strings.HasPrefix(e.Message, "mock replay summary") {
					summary = e.ContextMap()
				}
			}
			if summary == nil {
				t.Fatalf("no replay summary; got:\n%s", all)
			}
			if summary["missed"] != int64(0) {
				t.Errorf("summary missed = %v, want 0: a call answered with another call's recording was served", summary["missed"])
			}
			if strings.Contains(all, "recorded dependency calls were missed") {
				t.Errorf("a served call was reported as a miss; got:\n%s", all)
			}
			r, err := ReadReceipt(cfg.Path, "set")
			if err != nil || r == nil {
				t.Fatalf("receipt %+v (%v)", r, err)
			}
			if r.FailedBy != tc.wantFailedBy {
				t.Errorf("receipt failedBy %q, want %q", r.FailedBy, tc.wantFailedBy)
			}
			const strictLine = "replay failed under --strict: calls were answered with the recording of another call"
			if got := strings.Contains(all, strictLine); got != (tc.wantFailedBy == FailedByStrict) {
				t.Errorf("logged %q: %v, want %v; got:\n%s", strictLine, got, tc.wantFailedBy == FailedByStrict, all)
			}
			if len(tc.read) == 0 {
				if _, ok := summary["answered_as_another"]; ok {
					t.Errorf("summary carries answered_as_another = %v with no such call", summary["answered_as_another"])
				}
				if !r.Isolated || r.AnsweredAsAnother != nil {
					t.Errorf("receipt isolated %v (%q), answeredAsAnother %+v: want isolated with none", r.Isolated, r.IsolationNote, r.AnsweredAsAnother)
				}
				if strings.Contains(console.String(), "answered with another call's recording") {
					t.Errorf("the console names a call answered with another's recording with none: %q", console.String())
				}
				return
			}
			if summary["answered_as_another"] != int64(1) {
				t.Errorf("summary answered_as_another = %v, want 1", summary["answered_as_another"])
			}
			if !strings.Contains(all, "an outgoing call was answered with the recording of another call") || !strings.Contains(all, other.NextSteps) {
				t.Errorf("no WARN names the call with its next step; got:\n%s", all)
			}
			if !strings.Contains(console.String(), other.Diff) || !strings.Contains(console.String(), "next step: "+other.NextSteps) {
				t.Errorf("the console %q does not name the call whole with its next step", console.String())
			}
			// A failed test command is why first (isolation's order).
			if r.Isolated || (tc.runner.ExitCode == 0 && !strings.Contains(r.IsolationNote, other.Diff)) {
				t.Errorf("receipt isolated %v, note %q: want not isolated, naming the call", r.Isolated, r.IsolationNote)
			}
			want := AnsweredAsAnother{Call: "GET /users/3", Mock: "mock-1", Answer: models.AnswerOtherID, Differs: []string{"url.path.1"}, Answered: other.Diff, NextStep: other.NextSteps}
			if len(r.AnsweredAsAnother) != 1 || r.AnsweredAsAnother[0].Mock != want.Mock || r.AnsweredAsAnother[0].Answer != want.Answer ||
				r.AnsweredAsAnother[0].Call != want.Call || r.AnsweredAsAnother[0].Answered != want.Answered || r.AnsweredAsAnother[0].NextStep != want.NextStep ||
				strings.Join(r.AnsweredAsAnother[0].Differs, ",") != "url.path.1" {
				t.Errorf("receipt answeredAsAnother %+v, want [%+v]", r.AnsweredAsAnother, want)
			}
		})
	}
}

// A read of the calls no mock answered that was incomplete still gives the
// calls it read that were answered with another call's recording: the
// receipt lists them as their WARN lines name them, and the run is not
// counted as proven.
func TestAnIncompleteReadKeepsTheCallsAnsweredWithAnotherCallsRecordingItGave(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-1"}}
	instr.mockErrors = []models.UnmatchedCall{{Protocol: "http", ActualSummary: "GET /users/3", ClosestMock: "mock-1", Answer: models.AnswerOtherID,
		Diff: "GET /users/3 was answered with the recording of GET /users/1"}}
	instr.mockErrorsErr = fmt.Errorf("%w: a miss was dropped", models.ErrUnmatchedCallsUnread)
	cfg := instrConfig(instr, utils.Native, "pytest -q")
	cfg.Mock.OnMiss = string(models.MissFail)
	cfg.Path = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg.Path, "set"), 0o755); err != nil {
		t.Fatal(err)
	}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	_ = New(zap.New(core), instr, loadedMockDB{n: 1}, nil, nil, nil, cfg).Replay(context.Background())
	if !strings.Contains(logged(logs), "an outgoing call was answered with the recording of another call") {
		t.Errorf("no WARN names the call read")
	}
	r, err := ReadReceipt(cfg.Path, "set")
	if err != nil || r == nil {
		t.Fatalf("receipt %+v (%v)", r, err)
	}
	if r.Isolated || len(r.AnsweredAsAnother) != 1 || r.AnsweredAsAnother[0].Call != "GET /users/3" {
		t.Errorf("receipt isolated %v, answeredAsAnother %+v: want not isolated, listing GET /users/3", r.Isolated, r.AnsweredAsAnother)
	}
}
