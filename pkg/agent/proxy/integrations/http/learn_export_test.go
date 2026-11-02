package http

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The SQS shapes the rows below drive: a DynamoDB PutItem of a lead, then an
// SQS SendMessage whose message body (a JSON text inside the request's JSON)
// names the lead's id and its creation time. Both run in record's full round
// learn.FullRoundLead past their recording, the lead's id made again.
const (
	exportRecID  = "cd44979c-b5b4-484d-9ab0-e8bd42467be7"
	exportLiveID = "0b1e2f3a-1111-4222-8333-944455556666"
	exportLayout = "2006-01-02T15:04:05.0000000Z"
)

var (
	exportPut  = map[string]string{"X-Amz-Target": "DynamoDB_20120810.PutItem", "Content-Type": "application/x-amz-json-1.0"}
	exportSend = map[string]string{"X-Amz-Target": "AmazonSQS.SendMessage", "Content-Type": "application/x-amz-json-1.0"}
	exportAt   = learnAt.Add(-time.Hour + 17*time.Second + 164946200*time.Nanosecond)
)

func exportPutBody(id string, at time.Time) string {
	return `{"Item":{"Id":{"S":"` + id + `"},"CreatedAt":{"S":"` + at.Format(exportLayout) + `"}},"TableName":"Leads"}`
}

func exportSendBody(id string, at time.Time) string {
	return `{"MessageBody":"{\"type\":\"lead.created\",\"id\":\"` + id + `\",\"at\":\"` + at.Format(exportLayout) + `\"}","QueueUrl":"http://127.0.0.1:9324/000000000000/lead-events"}`
}

func exportCall(r *req) VariableCall {
	return VariableCall{Method: r.method, URL: r.url, Header: r.header, Body: r.body}
}

func exportSendRecording() *models.Mock {
	return learnRecorded("mock-1", time.Second, "POST", "/", exportSendBody(exportRecID, exportAt), exportSend, `{"MD5OfMessageBody":"x","MessageId":"m-1"}`)
}

// A call of another parser that drifted from its recording in record's full
// round is answered by the learn step, with what drifted learned onto the
// recording it took, as the HTTP parser's own call is.
func TestLearnMatchAnswersAnotherParsersDriftedCall(t *testing.T) {
	h, _, opts := learnOn(t)
	tr := clockedTracker()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{exportSendRecording()}}
	ctx := learn.WithTracker(context.Background(), tr)
	call := live("POST", "/", exportSendBody(exportLiveID, exportAt.Add(learn.FullRoundLead)), exportSend)
	served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, nil)
	if err != nil || miss != nil || served == nil || served.Name != "mock-1" {
		t.Fatalf("served %v miss %+v err %v, want mock-1 served", served, miss, err)
	}
	if len(db.consumed) != 1 || !reflect.DeepEqual(db.consumed[0].Spec.ReqBodyNoise, map[string][]string{"body.MessageBody": {}}) {
		t.Fatalf("consumed %+v, want mock-1 taken with what drifted learned onto it", db.consumed)
	}
}

// A changed value the learn step cannot explain is refused, never served: the
// SendMessage's message body holds the id an earlier call of the test sent
// whole, inside a longer value. The miss names the recording and carries the
// learn step's reason and code.
func TestLearnMatchRefusesAnotherParsersCallItCannotExplain(t *testing.T) {
	h, _, opts := learnOn(t)
	tr := clockedTracker()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/", exportPutBody(exportRecID, exportAt), exportPut, `{}`),
		exportSendRecording(),
	}}
	ctx := learn.WithTracker(context.Background(), tr)
	noise := NewRequestNoise(zap.NewNop(), opts)
	liveAt := exportAt.Add(learn.FullRoundLead)
	if served, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/", exportPutBody(exportLiveID, liveAt), exportPut)), noise, db, opts, integrations.CallRound{}, nil); err != nil || miss != nil || served == nil {
		t.Fatalf("the PutItem: served %v miss %+v err %v, want it answered", served, miss, err)
	}
	served, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/", exportSendBody(exportLiveID, liveAt), exportSend)), noise, db, opts, integrations.CallRound{}, nil)
	if err != nil || served != nil || miss == nil {
		t.Fatalf("the SendMessage: served %v miss %+v err %v, want a miss", served, miss, err)
	}
	if c := miss.Closest(); c == nil || c.Name != "mock-1" {
		t.Fatalf("the miss names %v, want mock-1", c)
	}
	report := &models.MockMismatchReport{NextSteps: "the parser's own words"}
	miss.Explain(report)
	if report.Reason != models.MissRequestChanged ||
		!strings.HasPrefix(report.NextSteps, "keploy's learn step: body.MessageBody carries a value mock-0 sent earlier in the test at body.Item.Id.S, inside a longer value") ||
		!strings.HasSuffix(report.NextSteps, " the parser's own words") {
		t.Fatalf("the report says %q (%q), want the learn step's reason before the parser's words", report.NextSteps, report.Reason)
	}
	if len(db.perTest) != 1 || db.perTest[0].Name != "mock-1" {
		t.Fatalf("the refused call took %+v; mock-1 stays in the store", db.perTest)
	}
}

// endedDb is a store whose set's learn step ended (integrations.LearnEnder).
type endedDb struct{ *learnDb }

func (endedDb) LearnEnded() bool { return true }

// Outside record's full round (the switch off, a replay, or a set whose learn
// step ended) the learn step does not run: nothing is answered, nothing is
// missed, and the store is untouched.
func TestLearnMatchRunsOnlyInRecordsFullRound(t *testing.T) {
	for _, c := range []struct {
		name  string
		on    string
		learn bool
		ended bool
	}{
		{"the switch off", "0", true, false},
		{"a replay", "1", false, false},
		{"the learn step ended", "1", true, true},
	} {
		t.Setenv(provenswitch.Env, c.on)
		h := newHTTP()
		opts := models.OutgoingOptions{Learn: c.learn}
		ldb := &learnDb{perTest: []*models.Mock{exportSendRecording()}}
		var db integrations.MockMemDb = ldb
		if c.ended {
			db = endedDb{ldb}
		}
		call := live("POST", "/", exportSendBody(exportLiveID, exportAt), exportSend)
		served, miss, err := h.LearnMatch(context.Background(), exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, nil)
		if err != nil || served != nil || miss != nil || len(ldb.perTest) != 1 || len(ldb.consumed) != 0 {
			t.Fatalf("%s: served %v miss %+v err %v, store %d left %d taken; want nothing run", c.name, served, miss, err, len(ldb.perTest), len(ldb.consumed))
		}
	}
}

// A call whose context ended is the learn step's error, never an answer or a
// miss.
func TestLearnMatchReturnsTheLearnStepsError(t *testing.T) {
	h, _, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{exportSendRecording()}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	call := live("POST", "/", exportSendBody(exportLiveID, exportAt), exportSend)
	served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, nil)
	if err == nil || served != nil || miss != nil {
		t.Fatalf("served %v miss %+v err %v, want the context's error alone", served, miss, err)
	}
}

// A miss with no reason (nil) names no recording and writes nothing.
func TestANilLearnMissSaysNothing(t *testing.T) {
	report := &models.MockMismatchReport{NextSteps: "the parser's own words"}
	for _, m := range []*LearnMiss{nil, {}} {
		m.Explain(report)
		if m.Closest() != nil || report.NextSteps != "the parser's own words" || report.Reason != "" {
			t.Fatalf("%+v: closest %v, report %+v", m, m.Closest(), report)
		}
	}
}

// atRoundDb is roundLearnDb read at the round's instant at
// (integrations.RoundAt), as the agent's MockManager reads its round: the
// store another parser marked its call in, read again by the learn step.
type atRoundDb struct {
	*roundLearnDb
	at integrations.RoundAt
}

func (d *atRoundDb) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	s, ok := d.roundLearnDb.RoundSnapshot(q)
	s.At = d.at
	return s, ok
}

// The learn step decides another parser's call in the round its caller
// marked it in, never in one the replay moved to since: a staging, or a
// test's end or begin, between the caller's mark and the learn step's first
// read is the moved miss, which takes and learns nothing and names no
// recording, so a late call of one test is never answered with the next
// test's recording. A move of the generation alone (a test process's claim
// of its key again) is no move, and a mark of a store that keeps no
// generation fails closed.
func TestLearnMatchDecidesTheCallInTheRoundItsCallerMarked(t *testing.T) {
	marked := integrations.RoundAt{Epoch: 3, Gen: 9, Bounds: 4}
	at := integrations.NewCallRound(marked.Epoch, nil).ReadAt(marked)
	inTest := at.InView(true, false)
	for _, c := range []struct {
		name string
		mark integrations.CallRound
		read integrations.RoundAt
		// plain: the store reads no snapshot of its round (no
		// integrations.RoundReader).
		plain bool
		// moved is what the moved miss says after "marked this request ";
		// "" where the call is served.
		moved string
	}{
		{"read in the round it was marked in", inTest, marked, false, ""},
		{"a claim again moved the generation alone", inTest, integrations.RoundAt{Epoch: 3, Gen: 10, Bounds: 4}, false, ""},
		{"no mark (a store that marks no round)", integrations.CallRound{}, integrations.RoundAt{Epoch: 4, Gen: 12, Bounds: 6}, false, ""},
		{"a test ended or began since the mark", inTest, integrations.RoundAt{Epoch: 3, Gen: 11, Bounds: 6}, false, "in the test then running, and the replay ended or began a test"},
		{"a test began since a mark between tests", at, integrations.RoundAt{Epoch: 3, Gen: 10, Bounds: 5}, false, "in the round between tests, and the replay ended or began a test"},
		{"a test set staged since the mark", inTest, integrations.RoundAt{Epoch: 4, Gen: 9, Bounds: 4}, false, "in the test then running, and the replay " + movedRestaged},
		{"a test set staged again since a mark as one staged", at.InStaging(true), integrations.RoundAt{Epoch: 4, Gen: 9, Bounds: 4}, false, "while the replay staged a test set, and the replay " + movedRestaged},
		{"a mark of a store that keeps no generation", integrations.NewCallRound(marked.Epoch, nil).InView(true, false), marked, false, "in the test then running in a store that keeps no count of its round's moves"},
		{"a store whose reads are no snapshot of the round", inTest, marked, true, "in the test then running in a store that keeps no count of its round's moves"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			tr := clockedTracker()
			tr.Begin()
			ldb := &learnDb{perTest: []*models.Mock{exportSendRecording()}}
			var db integrations.MockMemDb = &atRoundDb{roundLearnDb: &roundLearnDb{s12LearnDb: &s12LearnDb{learnDb: ldb}, test: "test-1"}, at: c.read}
			if c.plain {
				db = ldb
			}
			ctx := learn.WithTracker(context.Background(), tr)
			call := live("POST", "/", exportSendBody(exportLiveID, exportAt.Add(learn.FullRoundLead)), exportSend)
			served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, c.mark, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.moved == "" {
				if miss != nil || served == nil || served.Name != "mock-1" || len(ldb.consumed) != 1 {
					t.Fatalf("served %v miss %+v, %d taken; want mock-1 served", served, miss, len(ldb.consumed))
				}
				return
			}
			report := &models.MockMismatchReport{}
			miss.Explain(report)
			if served != nil || miss == nil || miss.Closest() != nil || len(ldb.consumed) != 0 || len(ldb.perTest) != 1 || ldb.perTest[0].Spec.ReqBodyNoise != nil ||
				report.Reason != "" || !strings.Contains(report.NextSteps, "marked this request "+c.moved) {
				t.Fatalf("served %v miss %+v (closest %v), %d taken, recording's noise %v, report %+v; want the moved miss (%q), nothing taken or learned",
					served, miss, miss.Closest(), len(ldb.consumed), ldb.perTest[0].Spec.ReqBodyNoise, report, c.moved)
			}
		})
	}
}

// installDb is atRoundDb showing test-1's view while the learn tracker has
// begun the next test (integrations.RoundSnapshot.TrackerAhead), until the
// call's wait for the round to move (integrations.RoundWaiter) installs
// test-2's view: a boundary of the round, after the caller's mark.
type installDb struct {
	*atRoundDb
	installed bool
}

func (d *installDb) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	s, ok := d.atRoundDb.RoundSnapshot(q)
	if !d.installed {
		s.Phase, s.Open = integrations.RoundBoundary, false
	}
	return s, ok
}

func (d *installDb) AwaitRoundMove(_ context.Context, at integrations.RoundAt) (bool, error) {
	d.installed, d.test = true, "test-2"
	d.at = integrations.RoundAt{Epoch: at.Epoch, Gen: at.Gen + 1, Bounds: at.Bounds + 1}
	return true, nil
}

// Only the learn step's first read is held to its caller's mark: a call
// marked where the tracker had begun test-2 over test-1's view, whose first
// read found the round as marked, waits for the store to open test-2 and is
// decided there (the learn step's own catch-up), though that install moved
// the round past the mark.
func TestLearnMatchHoldsOnlyItsFirstReadToTheCallersMark(t *testing.T) {
	h, _, opts := learnOn(t)
	tr := clockedTracker()
	tr.Begin()
	marked := integrations.RoundAt{Epoch: 3, Gen: 9, Bounds: 4}
	ldb := &learnDb{perTest: []*models.Mock{exportSendRecording()}}
	db := &installDb{atRoundDb: &atRoundDb{roundLearnDb: &roundLearnDb{s12LearnDb: &s12LearnDb{learnDb: ldb}, test: "test-1"}, at: marked}}
	if s, _ := db.RoundSnapshot(integrations.RoundQuery{Tracker: tr}); !s.TrackerAhead() || s.PoolsCall() {
		t.Fatalf("precondition: the store does not read the tracker ahead of test-1's view: %+v", s.Phase)
	}
	ctx := learn.WithTracker(context.Background(), tr)
	call := live("POST", "/", exportSendBody(exportLiveID, exportAt.Add(learn.FullRoundLead)), exportSend)
	mark := integrations.NewCallRound(marked.Epoch, nil).ReadAt(marked).InView(true, false)
	served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, mark, nil)
	if err != nil || !db.installed || miss != nil || served == nil || served.Name != "mock-1" {
		report := &models.MockMismatchReport{}
		miss.Explain(report)
		t.Fatalf("served %v (installed %v) err %v, miss %q; want mock-1 served in test-2's round", served, db.installed, err, report.NextSteps)
	}
}

// A serve the learn step gives another parser's call (LearnMatch) is told
// with the answer the caller writes (written), which may differ from the one
// the learn step gave it (SQS sets a send's digests over the message sent):
// F's view of the serve, which a later call of the set reads the recording
// with (learn.Order.ServedView), holds the answer the application read, also
// where the call was the recording byte for byte. A later changed value that
// holds a word of it is refused, naming the recording; a fresh one is
// learned. Without written, the answer served is the one written. Before,
// the view held the answer the learn step handed over: a word the
// application read in the answer written was learned as noise (a false
// green).
func TestLearnMatchTellsTheServeWithTheAnswerTheCallerWrites(t *testing.T) {
	const path = "body.ref"
	for _, tc := range []struct {
		name    string
		sent    string // the call's request body in F (recorded {"max":10})
		rewrite bool   // the caller writes count 77 for the recorded 9
		value   string // the later changed value (recorded "7")
		refused bool   // refused, naming mock-2; else learned
	}{
		{"a word of the answer written", `{"max":12}`, true, "77", true},
		{"a fresh word", `{"max":12}`, true, "42", false},
		{"sent as recorded, a word of the answer written", `{"max":10}`, true, "77", true},
		{"sent as recorded, a fresh word", `{"max":10}`, true, "42", false},
		{"no answer of the caller's, a word F gave nowhere", `{"max":12}`, false, "77", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			other := learnRecorded("mock-2", time.Second, "POST", "/v1/receive", `{"max":10}`, nil, `{"count":"9"}`)
			tr := learn.NewTracker()
			ctx := learn.WithTracker(context.Background(), tr)
			tr.Begin()
			var written func(*models.Mock) *models.Mock
			if tc.rewrite {
				written = func(m *models.Mock) *models.Mock {
					c := m.ShallowCopy()
					rs := *m.Spec.HTTPResp
					rs.Body = strings.ReplaceAll(rs.Body, `"9"`, `"77"`)
					c.Spec.HTTPResp = &rs
					return c
				}
			}
			db := &learnDb{perTest: []*models.Mock{other}, observe: tr.ServedMock}
			got, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/v1/receive", tc.sent, nil)), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, written)
			if err != nil || got == nil || got.Name != "mock-2" || got.Spec.HTTPResp.Body != `{"count":"9"}` {
				t.Fatalf("the call: served %v miss %+v err %v; want mock-2 served as the learn step gives it", got, miss, err)
			}
			tr.End()
			tr.Begin()
			order := tr.Placed().Order
			first, last := order.Of(other)
			if first == 0 || last == 0 {
				t.Fatalf("mock-2 is at %d,%d: its serve was not numbered", first, last)
			}
			r := order.ServedView(other, learn.RawOf(other, true).ServedIn(first, last))
			rec := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: "7"}}}
			lv := learn.Shape{Leaves: map[string]learn.Leaf{path: {Path: path, Type: learn.String, Value: tc.value}}}
			v := learn.Classify(learn.NewDriftCache(), []string{path}, lv, rec, learnAt, nil, []learn.Raw{r}, nil)
			if len(v) != 1 {
				t.Fatalf("%d verdicts; want one", len(v))
			}
			switch refused := v[0].Kind == learn.KindUnaccounted; {
			case tc.refused && (!refused || !strings.Contains(v[0].Why, "mock-2")):
				t.Fatalf("FALSE GREEN: %s %s (%q); want refused, naming mock-2: the application read it there", tc.value, v[0].Kind, v[0].Why)
			case !tc.refused && refused:
				t.Fatalf("FALSE RED: %s refused (%q); want learned: the application read no word of it at mock-2", tc.value, v[0].Why)
			}
		})
	}
}
