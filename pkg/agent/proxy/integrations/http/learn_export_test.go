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

// exportSendLearned is what the learn step writes onto the send's recording
// when its message drifted: the message's id and stamp, words 6 and 9 of
// {"type":"lead.created","id":"<id>","at":"<stamp>"}, and no other word of
// the message, which a replay still compares.
var exportSendLearned = map[string][]string{"body.MessageBody": {"^keploy:words 6,9$"}}

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
	served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, nil, nil)
	if err != nil || miss != nil || served == nil || served.Name != "mock-1" {
		t.Fatalf("served %v miss %+v err %v, want mock-1 served", served, miss, err)
	}
	if len(db.consumed) != 1 || !reflect.DeepEqual(db.consumed[0].Spec.ReqBodyNoise, exportSendLearned) {
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
	if served, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/", exportPutBody(exportLiveID, liveAt), exportPut)), noise, db, opts, integrations.CallRound{}, nil, nil); err != nil || miss != nil || served == nil {
		t.Fatalf("the PutItem: served %v miss %+v err %v, want it answered", served, miss, err)
	}
	served, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/", exportSendBody(exportLiveID, liveAt), exportSend)), noise, db, opts, integrations.CallRound{}, nil, nil)
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
		served, miss, err := h.LearnMatch(context.Background(), exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, nil, nil)
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
	served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, nil, nil)
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
			served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, c.mark, nil, nil)
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
				report.Reason != "" || !strings.HasPrefix(report.NextSteps, "keploy record's full round marked this request "+c.moved) ||
				!strings.HasSuffix(report.NextSteps, ": keploy learns nothing of a call in a round it was not made in, and pairs it with none of that round's recordings. "+
					"If the application makes this call at a test's start or end, make the test, or the application's readiness, wait for it, then record again.") {
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
	served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, mark, nil, nil)
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
			got, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/v1/receive", tc.sent, nil)), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, written, nil)
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

const exportOtherQueue = "http://127.0.0.1:9324/000000000000/lead-events-dlq"

// exportHold holds an SQS call's identity, as the SQS parser does: its queue
// and its operation never learned, a batch entry's Id only as an echo.
func exportHold(leaf string) LearnHold {
	switch leaf {
	case "body.QueueUrl", "header.x-amz-target":
		return LearnNever
	case "body.Entries[].Id":
		return LearnEchoOnly
	}
	return LearnAny
}

// A leaf the caller holds as the call's identity is never learned: a send of
// the recorded message, or of a drifted one, to another queue misses
// (request_changed), naming the queue's leaf and the recording, and nothing
// is taken or learned; so does a send of another operation. A drift outside
// the held leaves is still learned, and keploy.yml's noise on a held leaf
// still excuses it.
func TestLearnMatchNeverLearnsALeafItsCallerHolds(t *testing.T) {
	toQueue := func(body, queue string) string {
		return strings.Replace(body, "http://127.0.0.1:9324/000000000000/lead-events", queue, 1)
	}
	batchTarget := map[string]string{"X-Amz-Target": "AmazonSQS.SendMessageBatch", "Content-Type": "application/x-amz-json-1.0"}
	for _, c := range []struct {
		name    string
		body    string
		header  map[string]string
		noise   map[string][]string
		differs string // the held leaf the miss names; "" where served
	}{
		{"the recorded message to another queue", toQueue(exportSendBody(exportRecID, exportAt), exportOtherQueue), exportSend, nil, "body.QueueUrl"},
		{"a drifted message to another queue", toQueue(exportSendBody(exportLiveID, exportAt.Add(learn.FullRoundLead)), exportOtherQueue), exportSend, nil, "body.QueueUrl"},
		{"a drifted message as another operation", exportSendBody(exportLiveID, exportAt.Add(learn.FullRoundLead)), batchTarget, nil, "header.x-amz-target"},
		{"a drifted message to the recorded queue", exportSendBody(exportLiveID, exportAt.Add(learn.FullRoundLead)), exportSend, nil, ""},
		{"keploy.yml's noise on the queue", toQueue(exportSendBody(exportLiveID, exportAt.Add(learn.FullRoundLead)), exportOtherQueue), exportSend, map[string][]string{"QueueUrl": {}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			tr := clockedTracker()
			tr.Begin()
			db := &learnDb{perTest: []*models.Mock{exportSendRecording()}}
			ctx := learn.WithTracker(context.Background(), tr)
			noise := NewRequestNoise(zap.NewNop(), opts)
			noise.Body = c.noise
			served, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/", c.body, c.header)), noise, db, opts, integrations.CallRound{}, nil, exportHold)
			if err != nil {
				t.Fatal(err)
			}
			if c.differs == "" {
				if miss != nil || served == nil || served.Name != "mock-1" || len(db.consumed) != 1 ||
					!reflect.DeepEqual(db.consumed[0].Spec.ReqBodyNoise, exportSendLearned) {
					t.Fatalf("served %v miss %+v, took %+v; want mock-1 served with only its message body learned", served, miss, db.consumed)
				}
				return
			}
			if served != nil || miss == nil || len(db.consumed) != 0 || len(db.perTest) != 1 || db.perTest[0].Spec.ReqBodyNoise != nil {
				t.Fatalf("served %v miss %+v, took %+v; want a miss, nothing taken or learned", served, miss, db.consumed)
			}
			report := &models.MockMismatchReport{}
			miss.Explain(report)
			if c := miss.Closest(); c == nil || c.Name != "mock-1" {
				t.Fatalf("the miss names %v, want mock-1", c)
			}
			if report.Reason != models.MissRequestChanged || !strings.HasPrefix(report.NextSteps, "keploy's learn step: it differs from the closest recorded call in more than values: "+c.differs+" differs. ") {
				t.Fatalf("the report says %q (%q), want request_changed naming %s", report.NextSteps, report.Reason, c.differs)
			}
		})
	}
}

// A leaf held LearnEchoOnly is learned only as a value the answer carries
// back: a batch entry whose fresh Id the recorded answer echoes is served,
// the answer carrying the call's Id; one whose answer does not carry it
// back is refused (request_changed), nothing taken or learned.
func TestLearnMatchLearnsAnEchoOnlyLeafOnlyAsAnEcho(t *testing.T) {
	const recEntry, liveEntry = "aaaaaaaa-1111-4222-8333-444455556666", "bbbbbbbb-1111-4222-8333-444455556666"
	batch := func(id, body string) string {
		return `{"Entries":[{"Id":"` + id + `","MessageBody":` + strings.TrimSuffix(strings.TrimPrefix(body, `{"MessageBody":`), `,"QueueUrl":"http://127.0.0.1:9324/000000000000/lead-events"}`) + `}],"QueueUrl":"http://127.0.0.1:9324/000000000000/lead-events"}`
	}
	target := map[string]string{"X-Amz-Target": "AmazonSQS.SendMessageBatch", "Content-Type": "application/x-amz-json-1.0"}
	for _, c := range []struct {
		name, answer string
		echoed       bool
	}{
		{"the answer echoes the entry's Id", `{"Failed":[],"Successful":[{"Id":"` + recEntry + `","MD5OfMessageBody":"x","MessageId":"m-1"}]}`, true},
		{"the answer does not carry it back", `{"Failed":[],"Successful":[{"MD5OfMessageBody":"x","MessageId":"m-1"}]}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			tr := clockedTracker()
			tr.Begin()
			rec := learnRecorded("mock-1", time.Second, "POST", "/", batch(recEntry, exportSendBody(exportRecID, exportAt)), target, c.answer)
			db := &learnDb{perTest: []*models.Mock{rec}}
			ctx := learn.WithTracker(context.Background(), tr)
			call := live("POST", "/", batch(liveEntry, exportSendBody(exportLiveID, exportAt.Add(learn.FullRoundLead))), target)
			served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, nil, exportHold)
			if err != nil {
				t.Fatal(err)
			}
			if c.echoed {
				if miss != nil || served == nil || served.Name != "mock-1" || len(db.consumed) != 1 || !strings.Contains(served.Spec.HTTPResp.Body, `"Id":"`+liveEntry+`"`) {
					t.Fatalf("served %v miss %+v, took %d; want mock-1 served with the call's entry Id echoed", served, miss, len(db.consumed))
				}
				if _, noised := db.consumed[0].Spec.ReqBodyNoise["body.Entries[0].Id"]; noised {
					t.Fatalf("the entry's Id was learned as noise: %v", db.consumed[0].Spec.ReqBodyNoise)
				}
				return
			}
			if served != nil || miss == nil || len(db.consumed) != 0 || len(db.perTest) != 1 || db.perTest[0].Spec.ReqBodyNoise != nil {
				t.Fatalf("served %v miss %+v, took %d; want a miss, nothing taken or learned", served, miss, len(db.consumed))
			}
			report := &models.MockMismatchReport{}
			miss.Explain(report)
			if report.Reason != models.MissRequestChanged || !strings.HasPrefix(report.NextSteps, "keploy's learn step: body.Entries[0].Id changed, and keploy learns it only as a value the answer carries back, which the answer does not") {
				t.Fatalf("the report says %q (%q), want the echo-only refusal", report.NextSteps, report.Reason)
			}
		})
	}
}

// The caller's written answer is asked before the take: where it writes
// none, of a call that is its recording exactly or one that drifted from
// it, the call misses (request_changed), naming the recording, which stays
// in the store with nothing learned onto it.
func TestLearnMatchAsksTheWrittenAnswerBeforeTheTake(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"the recording exactly", exportSendBody(exportRecID, exportAt)},
		{"a drifted message", exportSendBody(exportLiveID, exportAt.Add(learn.FullRoundLead))},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			tr := clockedTracker()
			tr.Begin()
			db := &learnDb{perTest: []*models.Mock{exportSendRecording()}}
			ctx := learn.WithTracker(context.Background(), tr)
			var asked []string
			none := func(a *models.Mock) *models.Mock { asked = append(asked, a.Name); return nil }
			served, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/", c.body, exportSend)), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, none, nil)
			if err != nil || served != nil || miss == nil || len(asked) == 0 || len(db.consumed) != 0 || len(db.perTest) != 1 || db.perTest[0].Spec.ReqBodyNoise != nil {
				t.Fatalf("served %v miss %+v err %v, asked %v, took %d; want a miss, nothing taken or learned", served, miss, err, asked, len(db.consumed))
			}
			report := &models.MockMismatchReport{}
			miss.Explain(report)
			if c := miss.Closest(); c == nil || c.Name != "mock-1" || report.Reason != models.MissRequestChanged ||
				!strings.HasPrefix(report.NextSteps, "keploy's learn step: the call's parser cannot write an answer from mock-1's recorded answer for this request") {
				t.Fatalf("the miss names %v and says %q (%q), want request_changed naming mock-1", c, report.NextSteps, report.Reason)
			}
		})
	}
}

// exportBatch is a JSON SendMessageBatch to lead-events of one entry per
// entries item: its Id, its FIFO message group ("" for none) and the message
// exportSendBody(id, at) sends.
type exportEntry struct {
	id, group, lead string
	at              time.Time
}

func exportBatch(entries ...exportEntry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		msg := strings.TrimSuffix(strings.TrimPrefix(exportSendBody(e.lead, e.at), `{"MessageBody":`), `,"QueueUrl":"http://127.0.0.1:9324/000000000000/lead-events"}`)
		group := ""
		if e.group != "" {
			group = `,"MessageGroupId":"` + e.group + `"`
		}
		parts[i] = `{"Id":"` + e.id + `","MessageBody":` + msg + group + `}`
	}
	return `{"Entries":[` + strings.Join(parts, ",") + `],"QueueUrl":"http://127.0.0.1:9324/000000000000/lead-events"}`
}

// exportBatchHold is exportHold with a batch entry's message group held too:
// a leaf of an array entry, which the caller names in keploy.yml's noise
// dialect (body.Entries[].MessageGroupId), never by its place in the call.
func exportBatchHold(leaf string) LearnHold {
	if leaf == "body.Entries[].MessageGroupId" {
		return LearnNever
	}
	return exportHold(leaf)
}

// The hold the caller gives the learn step is asked of every leaf the
// learn step reads or learns:
//   - an array entry's leaf by its noise name: a FIFO batch entry with a
//     drifted message to another message group misses (request_changed),
//     naming the entry's group, nothing taken;
//   - every leaf it learned, not only the first: a batch whose first entry's
//     message drifted (learned, held LearnAny) and whose second entry's Id is
//     fresh with no answer carrying it back is refused, naming the second
//     Id, nothing taken;
//   - the caller's written answer is asked of the answer as the learn step
//     renders it: a written answer that refuses one naming an entry the call
//     did not send serves a fresh echoed entry Id, the recorded Id replaced.
func TestLearnMatchAsksTheHoldOfEveryLeafAndTheRenderedAnswer(t *testing.T) {
	const recEntry, liveEntry, otherEntry = "aaaaaaaa-1111-4222-8333-444455556666", "bbbbbbbb-1111-4222-8333-444455556666", "cccccccc-1111-4222-8333-444455556666"
	target := map[string]string{"X-Amz-Target": "AmazonSQS.SendMessageBatch", "Content-Type": "application/x-amz-json-1.0"}
	drifted := exportAt.Add(learn.FullRoundLead)
	// sentOnly writes an answer only where every entry it names is one the
	// call sent, as SQS does.
	sentOnly := func(call string) func(*models.Mock) *models.Mock {
		return func(a *models.Mock) *models.Mock {
			if strings.Contains(a.Spec.HTTPResp.Body, `"Id":"`+recEntry+`"`) && !strings.Contains(call, `"Id":"`+recEntry+`"`) {
				return nil
			}
			return a
		}
	}
	for _, c := range []struct {
		name, rec, call, answer string
		hold                    func(string) LearnHold
		written                 bool
		refusal                 string // the start of the miss's next steps; "" where served
	}{
		{"an entry's held message group, by its noise name",
			exportBatch(exportEntry{"e1", "tenant-a", exportRecID, exportAt}), exportBatch(exportEntry{"e1", "tenant-b", exportLiveID, drifted}),
			`{"Failed":[],"Successful":[{"Id":"e1","MD5OfMessageBody":"x","MessageId":"m-1"}]}`, exportBatchHold, false,
			"keploy's learn step: it differs from the closest recorded call in more than values: body.Entries[0].MessageGroupId differs. "},
		{"an echo-only Id learned after another leaf",
			exportBatch(exportEntry{"e0", "", exportRecID, exportAt}, exportEntry{recEntry, "", exportRecID, exportAt}),
			exportBatch(exportEntry{"e0", "", exportLiveID, drifted}, exportEntry{otherEntry, "", exportRecID, exportAt}),
			`{"Failed":[],"Successful":[{"Id":"e0","MD5OfMessageBody":"x","MessageId":"m-1"},{"MD5OfMessageBody":"x","MessageId":"m-2"}]}`, exportHold, false,
			"keploy's learn step: body.Entries[1].Id changed, and keploy learns it only as a value the answer carries back, which the answer does not"},
		{"the written answer asked as rendered",
			exportBatch(exportEntry{recEntry, "", exportRecID, exportAt}), exportBatch(exportEntry{liveEntry, "", exportLiveID, drifted}),
			`{"Failed":[],"Successful":[{"Id":"` + recEntry + `","MD5OfMessageBody":"x","MessageId":"m-1"}]}`, exportHold, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			tr := clockedTracker()
			tr.Begin()
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/", c.rec, target, c.answer)}}
			ctx := learn.WithTracker(context.Background(), tr)
			var written func(*models.Mock) *models.Mock
			if c.written {
				written = sentOnly(c.call)
			}
			served, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/", c.call, target)), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, written, c.hold)
			if err != nil {
				t.Fatal(err)
			}
			if c.refusal == "" {
				if miss != nil || served == nil || served.Name != "mock-1" || len(db.consumed) != 1 || !strings.Contains(served.Spec.HTTPResp.Body, `"Id":"`+liveEntry+`"`) {
					t.Fatalf("served %v miss %+v, took %d; want mock-1 served with the call's entry Id echoed", served, miss, len(db.consumed))
				}
				return
			}
			if served != nil || miss == nil || len(db.consumed) != 0 || len(db.perTest) != 1 || db.perTest[0].Spec.ReqBodyNoise != nil {
				t.Fatalf("served %v miss %+v, took %d; want a miss, nothing taken or learned", served, miss, len(db.consumed))
			}
			report := &models.MockMismatchReport{}
			miss.Explain(report)
			if report.Reason != models.MissRequestChanged || !strings.HasPrefix(report.NextSteps, c.refusal) {
				t.Fatalf("the report says %q (%q), want request_changed: %s", report.NextSteps, report.Reason, c.refusal)
			}
		})
	}
}
