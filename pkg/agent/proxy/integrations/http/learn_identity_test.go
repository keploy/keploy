package http

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A change in a leaf that says which call this is (a path segment, an
// operation header, a body field naming the destination or the operation)
// is the request's: the learn step refuses it (request_changed, naming the
// leaf) and learns nothing, where no earlier call's value and no read of the
// clock explains it. Learned as noise, a call to another queue or another
// operation was answered with this one's recording and never compared again.
func TestLearnMatchRefusesAChangeInWhatNamesTheCall(t *testing.T) {
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for _, tc := range []struct {
		name                  string
		method                string
		recTarget, liveTarget string
		recBody, liveBody     string
		recHeader, liveHeader map[string]string
		answer                string
		leaf                  string
	}{
		{name: "a path segment", method: "POST", recTarget: "/v1/queues/orders/messages", liveTarget: "/v1/queues/orders-dlq/messages",
			recBody: `{"id":"x"}`, liveBody: `{"id":"x"}`, answer: `{}`, leaf: "url.path.2"},
		{name: "an id in the path no earlier call gave", method: "GET", recTarget: "/v1/users/41", liveTarget: "/v1/users/42",
			answer: `{"name":"a"}`, leaf: "url.path.2"},
		{name: "an id in the path the answer echoes", method: "PUT", recTarget: "/v1/objects/k-1", liveTarget: "/v1/objects/k-2",
			recBody: `{"size":1}`, liveBody: `{"size":1}`, answer: `{"key":"k-1"}`, leaf: "url.path.2"},
		{name: "a JSON field naming the queue", method: "POST", recTarget: "/", liveTarget: "/",
			recBody: `{"QueueUrl":"http://q/orders","id":"x"}`, liveBody: `{"QueueUrl":"http://q/orders-dlq","id":"x"}`,
			recHeader: map[string]string{"X-Amz-Target": "AmazonSQS.SendMessage"}, liveHeader: map[string]string{"X-Amz-Target": "AmazonSQS.SendMessage"},
			answer: `{}`, leaf: "body.QueueUrl"},
		{name: "a JSON field naming the table, nested", method: "POST", recTarget: "/", liveTarget: "/",
			recBody: `{"TransactItems":[{"Put":{"TableName":"orders","Item":{}}}]}`, liveBody: `{"TransactItems":[{"Put":{"TableName":"audit","Item":{}}}]}`,
			answer: `{}`, leaf: "body.TransactItems[0].Put.TableName"},
		{name: "an operation header", method: "POST", recTarget: "/", liveTarget: "/",
			recBody: `{"QueueUrl":"http://q/orders"}`, liveBody: `{"QueueUrl":"http://q/orders"}`,
			recHeader: map[string]string{"X-Amz-Target": "AmazonSQS.PurgeQueue"}, liveHeader: map[string]string{"X-Amz-Target": "AmazonSQS.DeleteQueue"},
			answer: `{}`, leaf: "header.x-amz-target"},
		{name: "a SOAP action", method: "POST", recTarget: "/ws", liveTarget: "/ws",
			recBody: `{}`, liveBody: `{}`,
			recHeader: map[string]string{"SOAPAction": "urn:GetOrder"}, liveHeader: map[string]string{"SOAPAction": "urn:CancelOrder"},
			answer: `{}`, leaf: "header.soapaction"},
		{name: "a tunnelled method", method: "POST", recTarget: "/v1/orders/o-1", liveTarget: "/v1/orders/o-1",
			recBody: `{}`, liveBody: `{}`,
			recHeader: map[string]string{"X-HTTP-Method-Override": "PATCH"}, liveHeader: map[string]string{"X-HTTP-Method-Override": "DELETE"},
			answer: `{}`, leaf: "header.x-http-method-override"},
		{name: "a form field naming the operation", method: "POST", recTarget: "/", liveTarget: "/",
			recBody: "Action=SendMessage&QueueUrl=http%3A%2F%2Fq%2Forders&MessageBody=m", liveBody: "Action=PurgeQueue&QueueUrl=http%3A%2F%2Fq%2Forders&MessageBody=m",
			recHeader: form, liveHeader: form, answer: `{}`, leaf: "body.Action"},
		{name: "a form field naming the topic", method: "POST", recTarget: "/", liveTarget: "/",
			recBody: "Action=Publish&TopicArn=arn%3Aaws%3Asns%3Aus-east-1%3A1%3Aorders&Message=m", liveBody: "Action=Publish&TopicArn=arn%3Aaws%3Asns%3Aus-east-1%3A1%3Aaudit&Message=m",
			recHeader: form, liveHeader: form, answer: `{}`, leaf: "body.TopicArn"},
		{name: "a GraphQL operation name", method: "POST", recTarget: "/graphql", liveTarget: "/graphql",
			recBody: `{"operationName":"GetOrder","query":"q"}`, liveBody: `{"operationName":"CancelOrder","query":"q"}`,
			answer: `{}`, leaf: "body.operationName"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := clockedTracker()
			tr.Begin()
			rec := learnRecorded("mock-1", 0, tc.method, tc.recTarget, tc.recBody, tc.recHeader, tc.answer)
			db := &learnDb{perTest: []*models.Mock{rec}}
			served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live(tc.method, tc.liveTarget, tc.liveBody, tc.liveHeader), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, tc.leaf+" changed") ||
				miss.closest == nil || miss.closest.Name != "mock-1" {
				t.Fatalf("served %v miss %+v: want request_changed naming %s", served, miss, tc.leaf)
			}
			if len(db.consumed) != 0 || len(db.perTest) != 1 || len(rec.Spec.ReqBodyNoise) != 0 || len(rec.Spec.Variables) != 0 {
				t.Fatalf("the refused call took or learned something: consumed %d, noise %v, variables %v", len(db.consumed), rec.Spec.ReqBodyNoise, rec.Spec.Variables)
			}
		})
	}
}

// Beside such a change, what does not name the call is still learned as it
// was: the leaves that name it are the only ones held.
func TestLearnMatchStillLearnsWhatDoesNotNameTheCall(t *testing.T) {
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/queues/orders/messages?at=100", `{"QueueUrl":"http://q/orders","request_id":"r-1"}`,
		map[string]string{"X-Amz-Target": "AmazonSQS.SendMessage", "X-Trace": "t1"}, `{}`)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/queues/orders/messages?at=200", `{"QueueUrl":"http://q/orders","request_id":"r-2"}`,
		map[string]string{"X-Amz-Target": "AmazonSQS.SendMessage", "X-Trace": "t2"}), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	got := db.consumed[0].Spec.ReqBodyNoise
	for _, want := range []string{"url.query.at", "header.x-trace", "body.request_id"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("learned %v, want %s among it", got, want)
		}
	}
}

// A read of the application's clock in a path segment still explains it: it
// is learned as the read it is, compared at every replay, never noise.
func TestLearnMatchAClockReadInAPathSegmentIsLearned(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	receive := learnRecorded("mock-0", 0, "POST", "/queue/receive", `{"MaxNumberOfMessages":10}`, nil, `{"Messages":[{"MessageId":"135dcb85","Body":"m-1"}]}`)
	receive.Spec.HTTPResp.Header = map[string]string{"Date": learnAt.Format(http.TimeFormat)}
	event := learnRecorded("mock-1", 4*time.Millisecond, "PUT", "/v1/events/m-1/"+iso(learnAt.Add(3*time.Millisecond)), `{}`, nil, `{}`)
	event.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{event}}, staged: []*models.Mock{receive}, pool: map[time.Time]bool{receive.Spec.ReqTimestampMock: true}}
	tr := learn.NewTracker()
	offset := learn.FullRoundLead + 2*time.Second - 40*time.Second
	tr.SetAppClock(func(real time.Time) (time.Time, bool) { return real.Add(offset), true })
	at := learnAt.Add(40 * time.Second)
	tr.ServedMockAt(receive, at)
	in := live("PUT", "/v1/events/m-1/"+iso(at.Add(offset+time.Millisecond)), `{}`, nil)
	in.arrived = at.Add(2 * time.Millisecond)
	got, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), in, db, n, opts)
	if err != nil || got == nil || miss != nil || len(db.pooled) != 1 {
		t.Fatalf("served %v miss %+v err %v pooled %d; want served, url.path.3 learned as a clock read", got, miss, err, len(db.pooled))
	}
	if !learnedClockRead(db.pooled[0].Spec, "url.path.3") {
		t.Fatalf("clock reads %+v, noise %v: want url.path.3 learned as a read of the clock", db.pooled[0].Spec.Clocks, db.pooled[0].Spec.ReqBodyNoise)
	}
}

// keploy.yml's noise still excuses such a leaf: the user declared it.
func TestLearnMatchDeclaredNoiseStillExcusesWhatNamesTheCall(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		header, url           func(n *learnNoise)
		recTarget, liveTarget string
		recBody, liveBody     string
		recHeader, liveHeader map[string]string
	}{
		{name: "a header", header: func(n *learnNoise) { n.headers = map[string]bool{"x-amz-target": true} }, recTarget: "/", liveTarget: "/",
			recBody: `{}`, liveBody: `{}`, recHeader: map[string]string{"X-Amz-Target": "S.A"}, liveHeader: map[string]string{"X-Amz-Target": "S.B"}},
		{name: "a path pattern", url: func(n *learnNoise) { n.url = []*regexp.Regexp{regexp.MustCompile(`orders(-dlq)?`)} },
			recTarget: "/v1/queues/orders/messages", liveTarget: "/v1/queues/orders-dlq/messages", recBody: `{}`, liveBody: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			for _, f := range []func(*learnNoise){tc.header, tc.url} {
				if f != nil {
					f(&n)
				}
			}
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", 0, "POST", tc.recTarget, tc.recBody, tc.recHeader, `{}`)}}
			served, miss, err := h.learnMatch(context.Background(), live("POST", tc.liveTarget, tc.liveBody, tc.liveHeader), db, n, opts)
			if err != nil || miss != nil || served == nil {
				t.Fatalf("served %v miss %+v err %v: declared noise excuses the leaf", served, miss, err)
			}
		})
	}
}

// Another parser's call (LearnMatch) is held as the HTTP parser's own is,
// where its own hold leaves a leaf to the learn step (LearnAny).
func TestLearnMatchHoldsWhatNamesAnotherParsersCall(t *testing.T) {
	h, _, opts := learnOn(t)
	tr := clockedTracker()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", 0, "POST", "/v1/queues/orders/messages", `{"id":"x"}`, nil, `{}`)}}
	in := live("POST", "/v1/queues/orders-dlq/messages", `{"id":"x"}`, nil)
	call := VariableCall{Method: in.method, URL: in.url, Header: in.header, Body: in.body}
	served, miss, err := h.LearnMatch(learn.WithTracker(context.Background(), tr), call, RequestNoise{}, db, opts, integrations.CallRound{}, nil, func(string) LearnHold { return LearnAny })
	if err != nil || served != nil || miss == nil || miss.m == nil || miss.m.res.Reason != models.MissRequestChanged || !strings.Contains(miss.m.res.Why, "url.path.2 changed") {
		t.Fatalf("served %v miss %+v err %v: want request_changed naming url.path.2", served, miss, err)
	}
}
