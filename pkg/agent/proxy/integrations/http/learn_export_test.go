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
	served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, nil)
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
	if served, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/", exportPutBody(exportLiveID, liveAt), exportPut)), noise, db, opts, nil); err != nil || miss != nil || served == nil {
		t.Fatalf("the PutItem: served %v miss %+v err %v, want it answered", served, miss, err)
	}
	served, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/", exportSendBody(exportLiveID, liveAt), exportSend)), noise, db, opts, nil)
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
		served, miss, err := h.LearnMatch(context.Background(), exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, nil)
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
	served, miss, err := h.LearnMatch(ctx, exportCall(call), NewRequestNoise(zap.NewNop(), opts), db, opts, nil)
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
