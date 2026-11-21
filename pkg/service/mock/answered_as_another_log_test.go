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
		Differs:       []string{"url.path.3", "url.query." + token},
		Diff:          "GET /reset/" + token + "/confirm/7 was answered with the recording of GET /reset/" + token + "/confirm/1: no recording has the call's URL, which differs from that recording's only at values that look like ids (url.path.3, url.query." + token + ")",
		NextSteps:     `If the call is a new input, record it (keploy mock record) so it is answered with its own recording. If the app makes a new id there on every run, mark it as url noise in keploy.yml, under test.globalNoise.global.url: {"^/reset/` + token + `/confirm/[0-9]+$": [], "^[0-9]{10,}$": []}; each run's call is then answered with this recording as its own. An entry for a query value masks every query value of its shape, in every call.`,
	}
	field := "body.sessions." + token + ".user"
	body := models.UnmatchedCall{Protocol: "HTTP", Destination: "users:8080", ActualSummary: "POST /search", ClosestMock: "mock-2",
		Answer: models.AnswerClosestBody, Differs: []string{field, "body.data.id"}, Diff: "POST /search was answered with the recording of POST /search whose request body is closest to the call's",
		NextSteps: `If the call is a new input, record it (keploy mock record) so it is answered with its own recording. If the fields it differs at (` + field + `, body.data.id) change on every run, mark them as noise in keploy.yml, under test.globalNoise.global.requestbody: {"sessions.` + token + `.user": [], "data.id": []}; the call is then answered with this recording as its own.`}

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
	var nextSteps []string
	for _, e := range logs.All() {
		switch e.Message {
		case "this replay did not prove the tests run with the dependencies off":
			reason, _ = e.ContextMap()["reason"].(string)
		case "an outgoing call was answered with the recording of another call, not its own: this replay is not counted as proven, and --strict fails it":
			nextSteps = append(nextSteps, e.ContextMap()["next_step"].(string))
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
	// A query parameter is named by its shape: a parameter's name can be a
	// value (url.query.{}).
	if len(differs) != 2 || !slices.Equal(differs[0], []string{"url.path.3", "url.query.{}"}) || !slices.Equal(differs[1], []string{models.FieldShape(field), "body.data.id"}) || models.FieldShape(field) == field {
		t.Errorf("the WARNs' differs fields are %q, want [url.path.3 url.query.{}] and [%s body.data.id]", differs, models.FieldShape(field))
	}
	// The body WARN's next step names each requestbody entry's key as its
	// differs names the field, without the root: data.id, and the token's
	// key with the token {} as in differs.
	if len(nextSteps) == 2 && len(differs) == 2 && len(differs[1]) == 2 {
		entries := `requestbody: {"` + strings.TrimPrefix(differs[1][0], "body.") + `": [], "` + strings.TrimPrefix(differs[1][1], "body.") + `": []}`
		if !strings.Contains(nextSteps[1], entries) {
			t.Errorf("the body WARN's next_step %q does not name its entries as its differs %q does: want %s", nextSteps[1], differs[1], entries)
		}
	} else {
		t.Errorf("the WARNs' next steps %q", nextSteps)
	}

	r, err := ReadReceipt(cfg.Path, "set")
	if err != nil || r == nil {
		t.Fatalf("receipt %+v (%v)", r, err)
	}
	if r.Isolated || !strings.Contains(r.IsolationNote, call.Diff) {
		t.Errorf("the receipt's isolation note %q does not name the call whole", r.IsolationNote)
	}
	if len(r.AnsweredAsAnother) != 2 || r.AnsweredAsAnother[0].NextStep != call.NextSteps || !slices.Equal(r.AnsweredAsAnother[1].Differs, body.Differs) {
		t.Errorf("the receipt's answeredAsAnother %+v does not hold the calls whole", r.AnsweredAsAnother)
	}
	if !strings.Contains(console.String(), call.Diff) || !strings.Contains(console.String(), call.NextSteps) {
		t.Errorf("the console %q does not name the call whole with its next step", console.String())
	}
}

// A replay not proven for a reason of its own (here passthrough), with no call
// answered with another call's recording, logs that reason as the receipt
// holds it: the log form of another call's answers is for those alone.
func TestAReplayNotProvenForAnotherReasonLogsItsOwnNote(t *testing.T) {
	keployDir, _, logs := replayIn(t, replayCase{onMiss: "passthrough"})
	r := mustReceipt(t, keployDir)
	infos := logs.FilterMessage("this replay did not prove the tests run with the dependencies off").All()
	if len(infos) != 1 || r.Isolated || r.IsolationNote == "" || len(r.AnsweredAsAnother) != 0 {
		t.Fatalf("a passthrough replay logged %d reason lines with receipt isolated=%v note %q answeredAsAnother %+v, want one, not isolated, a note and none", len(infos), r.Isolated, r.IsolationNote, r.AnsweredAsAnother)
	}
	if reason, _ := infos[0].ContextMap()["reason"].(string); reason != r.IsolationNote {
		t.Errorf("the reason logged is %q, want the receipt's isolation note %q", reason, r.IsolationNote)
	}
}

// stringsEncoder is a zapcore.ArrayEncoder that keeps the strings it is
// given: how a test reads a zap.Strings field back.
type stringsEncoder struct {
	zapcore.ArrayEncoder
	s []string
}

func (e *stringsEncoder) AppendString(v string) { e.s = append(e.s, v) }
