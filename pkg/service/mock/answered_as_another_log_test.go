package mock

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The note names the first answeredAsAnotherMax calls and counts each one
// past them, from the first: one call past them is "and 1 more", in the
// receipt's note and in its log form alike.
func TestTheNoteCountsEachCallPastTheOnesItNames(t *testing.T) {
	calls := make([]models.UnmatchedCall, answeredAsAnotherMax+1)
	for i := range calls {
		calls[i] = models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /x", Answer: models.AnswerClosestBody, ClosestMock: "mock-x"}
	}
	for name, note := range map[string]string{"note": answeredAsAnotherNote(calls), "log note": answeredAsAnotherLogNote(calls)} {
		if !strings.Contains(note, "; and 1 more (the receipt's answeredAsAnother lists each)") || strings.Count(note, "answered with recording mock-x") != answeredAsAnotherMax {
			t.Errorf("the %s %q does not name %d calls and count 1 more", name, note, answeredAsAnotherMax)
		}
	}
	if note := answeredAsAnotherLogNote(calls[:answeredAsAnotherMax]); strings.Contains(note, "more") {
		t.Errorf("the log note %q counts more calls than it was given", note)
	}
}

// A call answered with another call's recording whose path holds a token
// (a reset link's) is named whole in the receipt's isolation note, its
// answeredAsAnother entry and on the console, and by its shape and path tag
// in every log line: the WARN, whose differs field names its segment and a
// body field by its shape, and the INFO line that says why the replay did
// not prove the tests run with the dependencies off. No log line holds the
// token.
func TestACallAnsweredWithAnotherCallsRecordingIsLoggedByItsShape(t *testing.T) {
	const token = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	const tag = "#1f2e3d4c5b6a"
	call := models.UnmatchedCall{
		Protocol:      "HTTP",
		Destination:   "users:8080",
		ActualSummary: "GET /reset/" + token + "/confirm/7",
		PathTag:       tag,
		ClosestMock:   "mock-1",
		Answer:        models.AnswerOtherID,
		Differs:       []string{"url.path.3"},
		Diff:          "GET /reset/" + token + "/confirm/7 was answered with the recording of GET /reset/" + token + "/confirm/1: no recording has the call's path",
		NextSteps:     `If the call is a new input, record it (keploy mock record) so it is answered with its own recording. If the app makes a new id there on every run, mark it as url noise in keploy.yml, under test.globalNoise.global.url: {"^/reset/` + token + `/confirm/[0-9]+$": []}; each run's call is then answered with this recording as its own.`,
	}
	field := "body.sessions." + token + ".user"
	body := models.UnmatchedCall{Protocol: "HTTP", Destination: "users:8080", ActualSummary: "POST /search", ClosestMock: "mock-2",
		Answer: models.AnswerClosestBody, Differs: []string{field}, Diff: "POST /search was answered with the recording of POST /search whose request body is closest to the call's"}

	core, logs := observer.New(zapcore.DebugLevel)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-1"}, {Name: "mock-2"}}
	instr.mockErrors = []models.UnmatchedCall{call, body}
	cfg := instrConfig(instr, utils.Native, "pytest -q")
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
		t.Fatalf("Replay returned keploy's own failure %v", err)
	}

	all := logged(logs)
	if strings.Contains(all, token) {
		t.Errorf("a log line holds the call's path token; got:\n%s", all)
	}
	var reason string
	var differs [][]string
	for _, e := range logs.All() {
		switch e.Message {
		case "this replay did not prove the tests run with the dependencies off":
			reason, _ = e.ContextMap()["reason"].(string)
		case "an outgoing call was answered with the recording of another call, not its own: this replay is not counted as proven, and --strict fails it":
			for _, f := range e.Context {
				if f.Key == "differs" {
					got, _ := f.Interface.(zapcore.ArrayMarshaler)
					enc := &stringsEncoder{}
					if got != nil {
						_ = got.MarshalLogArray(enc)
					}
					differs = append(differs, enc.s)
				}
			}
		}
	}
	if !strings.Contains(reason, "2 call(s) were answered with the recording of another call") || !strings.Contains(reason, tag) || !strings.Contains(reason, "answered with recording mock-1 (other_id)") {
		t.Errorf("the INFO line's reason %q does not name the call by its path tag and its recording", reason)
	}
	if len(differs) != 2 || !slices.Equal(differs[0], []string{"url.path.3"}) || !slices.Equal(differs[1], []string{models.FieldShape(field)}) || models.FieldShape(field) == field {
		t.Errorf("the WARNs' differs fields are %q, want [url.path.3] and [%s]", differs, models.FieldShape(field))
	}

	r, err := ReadReceipt(cfg.Path, "set")
	if err != nil || r == nil {
		t.Fatalf("receipt %+v (%v)", r, err)
	}
	if r.Isolated || !strings.Contains(r.IsolationNote, call.Diff) {
		t.Errorf("the receipt's isolation note %q does not name the call whole", r.IsolationNote)
	}
	if len(r.AnsweredAsAnother) != 2 || r.AnsweredAsAnother[0].NextStep != call.NextSteps || !slices.Equal(r.AnsweredAsAnother[1].Differs, []string{field}) {
		t.Errorf("the receipt's answeredAsAnother %+v does not hold the calls whole", r.AnsweredAsAnother)
	}
	if !strings.Contains(console.String(), call.Diff) || !strings.Contains(console.String(), call.NextSteps) {
		t.Errorf("the console %q does not name the call whole with its next step", console.String())
	}
}

// stringsEncoder is a zapcore.ArrayEncoder that keeps the strings it is
// given: how a test reads a zap.Strings field back.
type stringsEncoder struct {
	zapcore.ArrayEncoder
	s []string
}

func (e *stringsEncoder) AppendString(v string) { e.s = append(e.s, v) }
