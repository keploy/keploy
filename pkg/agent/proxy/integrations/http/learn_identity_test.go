package http

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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
		{name: "an object key the application minted, the answer empty", method: "PUT",
			recTarget: "/bucket/uploads/7f1c2a9e-1111-4c1e-9a7e-0a1b2c3d4e5f.png", liveTarget: "/bucket/uploads/0b9d6f3e-2222-4f6a-8e2b-9c8d7e6f5a4b.png",
			recBody: "x", liveBody: "x", leaf: "url.path.2"},
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
		{name: "a query parameter naming the operation", method: "GET", recTarget: "/?Action=ReceiveMessage&MaxNumberOfMessages=1", liveTarget: "/?Action=PurgeQueue&MaxNumberOfMessages=1",
			answer: `{}`, leaf: "url.query.Action"},
		{name: "a GraphQL operation name", method: "POST", recTarget: "/graphql", liveTarget: "/graphql",
			recBody: `{"operationName":"GetOrder","query":"q"}`, liveBody: `{"operationName":"CancelOrder","query":"q"}`,
			answer: `{}`, leaf: "body.operationName"},
		{name: "a GraphQL operation with no operation name", method: "POST", recTarget: "/graphql", liveTarget: "/graphql",
			recBody: `{"query":"query GetOrder { order(id: 1) { id } }"}`, liveBody: `{"query":"mutation CancelOrder { cancel(id: 1) { id } }"}`,
			answer: `{}`, leaf: "body.query"},
		{name: "a GraphQL operation's type alone", method: "POST", recTarget: "/graphql", liveTarget: "/graphql",
			recBody: `{"query":"query { orders { id } }"}`, liveBody: `{"query":"mutation { orders { id } }"}`,
			answer: `{}`, leaf: "body.query"},
		{name: "a GraphQL operation after a comment holding a brace", method: "POST", recTarget: "/graphql", liveTarget: "/graphql",
			recBody: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"mutation CancelOrder # {\n { cancel { ok } }"}`,
			answer: `{}`, leaf: "body.query"},
		{name: "a GraphQL operation after a description", method: "POST", recTarget: "/graphql", liveTarget: "/graphql",
			recBody: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"\"Cancel it\" mutation CancelOrder { cancel { ok } }"}`,
			answer: `{}`, leaf: "body.query"},
		{name: "a GraphQL operation with text after it", method: "POST", recTarget: "/graphql", liveTarget: "/graphql",
			recBody: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"mutation CancelOrder { cancel { ok } } x"}`,
			answer: `{}`, leaf: "body.query"},
		{name: "a GraphQL operation where the field held text", method: "POST", recTarget: "/graphql", liveTarget: "/graphql",
			recBody: `{"query":"shoes"}`, liveBody: `{"query":"mutation CancelOrder { cancel { ok } }"}`,
			answer: `{}`, leaf: "body.query"},
		{name: "SOAP 1.2's action", method: "POST", recTarget: "/ws", liveTarget: "/ws",
			recBody: `<e/>`, liveBody: `<e/>`,
			recHeader:  map[string]string{"Content-Type": `application/soap+xml; charset=utf-8; action="urn:GetOrder"`},
			liveHeader: map[string]string{"Content-Type": `application/soap+xml; charset=utf-8; action="urn:CancelOrder"`},
			answer:     `{}`, leaf: "header.content-type.action"},
		{name: "a field naming the queue, as an array", method: "POST", recTarget: "/", liveTarget: "/",
			recBody: `{"QueueUrl":["http://q/orders"]}`, liveBody: `{"QueueUrl":["http://q/orders-dlq"]}`,
			answer: `{}`, leaf: "body.QueueUrl[0]"},
		{name: "an operation header the answer echoes", method: "POST", recTarget: "/", liveTarget: "/",
			recBody: `{}`, liveBody: `{}`,
			recHeader: map[string]string{"X-Amz-Target": "AmazonSQS.PurgeQueue"}, liveHeader: map[string]string{"X-Amz-Target": "AmazonSQS.DeleteQueue"},
			answer: `{"target":"AmazonSQS.PurgeQueue"}`, leaf: "header.x-amz-target"},
		{name: "a field naming the operation the answer echoes", method: "POST", recTarget: "/", liveTarget: "/",
			recBody: "Action=PurgeQueue&QueueUrl=http%3A%2F%2Fq%2Forders", liveBody: "Action=DeleteQueue&QueueUrl=http%3A%2F%2Fq%2Forders",
			recHeader: form, liveHeader: form, answer: `{"Action":"PurgeQueue"}`, leaf: "body.Action"},
		{name: "a tunnelled method the answer echoes", method: "POST", recTarget: "/v1/orders/o-1", liveTarget: "/v1/orders/o-1",
			recBody: `{}`, liveBody: `{}`,
			recHeader: map[string]string{"X-HTTP-Method": "PATCH"}, liveHeader: map[string]string{"X-HTTP-Method": "DELETE"},
			answer: `{"method":"PATCH"}`, leaf: "header.x-http-method"},
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
			// The reason names the leaf's class, whether an echo would have
			// explained it, and the keploy.yml bucket that excuses it.
			bucket := map[byte]string{'u': "url noise matching it", 'h': "header noise naming the header", 'b': "requestBody noise naming the field"}[tc.leaf[0]]
			if why := miss.res.Why; !strings.Contains(why, "says which call this is (") || !strings.Contains(why, "declare it in keploy.yml ("+bucket+")") ||
				strings.Contains(why, "or a value the answer carries back") != classResource(identityClass(tc.leaf)) {
				t.Fatalf("reason %q: want the class, the echo only for an id, and %s", why, bucket)
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

// keploy.yml's noise still excuses such a leaf: the user declared it. Each
// row goes from keploy.yml's configuration as the CLI reads it (test's
// globalNoise, its global part or a test set's) through the noise record's
// full round hands the agent (replay.PrepareMockNoiseConfig) and the noise
// the HTTP parser reads from it (requestNoiseFor) to the learn step, in the
// bucket the refusal names: url for a path segment, header for a header,
// requestbody for a body field.
func TestLearnMatchKeployYmlNoiseStillExcusesWhatNamesTheCall(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		noise                 config.Globalnoise
		recTarget, liveTarget string
		recBody, liveBody     string
		recHeader, liveHeader map[string]string
	}{
		{name: "url noise, a path segment", noise: config.Globalnoise{Global: config.GlobalNoise{"url": {"orders(-dlq)?": {}}}},
			recTarget: "/v1/queues/orders/messages", liveTarget: "/v1/queues/orders-dlq/messages", recBody: `{}`, liveBody: `{}`},
		{name: "a test set's url noise, a query value", noise: config.Globalnoise{Testsets: config.TestsetNoise{"test-set-0": {"url": {"Purge|Delete": {}}}}},
			recTarget: "/?Action=PurgeQueue", liveTarget: "/?Action=DeleteQueue"},
		{name: "header noise, an operation header", noise: config.Globalnoise{Global: config.GlobalNoise{"header": {"X-Amz-Target": {}}}},
			recTarget: "/", liveTarget: "/", recBody: `{}`, liveBody: `{}`,
			recHeader: map[string]string{"X-Amz-Target": "S.A"}, liveHeader: map[string]string{"X-Amz-Target": "S.B"}},
		{name: "requestbody noise, a field naming the queue", noise: config.Globalnoise{Global: config.GlobalNoise{"requestbody": {"QueueUrl": {}}}},
			recTarget: "/", liveTarget: "/", recBody: `{"QueueUrl":"http://q/orders"}`, liveBody: `{"QueueUrl":"http://q/orders-dlq"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			opts.NoiseConfig = replay.PrepareMockNoiseConfig(tc.noise.Global, tc.noise.Testsets, "test-set-0")
			rn := requestNoiseFor(h.Logger, opts)
			n := h.newLearnNoise(opts, rn.Body, rn.URL)
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", 0, "POST", tc.recTarget, tc.recBody, tc.recHeader, `{}`)}}
			served, miss, err := h.learnMatch(context.Background(), live("POST", tc.liveTarget, tc.liveBody, tc.liveHeader), db, n, opts)
			if err != nil || miss != nil || served == nil {
				t.Fatalf("served %v miss %+v err %v: keploy.yml's noise excuses the leaf", served, miss, err)
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

// An id the call acts on, in a path segment or a field naming the resource,
// that changed and comes back in the answer is learned as that echo: an
// application that mints its ids sends a new one at every run, and the
// answer carries it back. A name of the operation the answer echoes is
// still refused (TestLearnMatchRefusesAChangeInWhatNamesTheCall).
func TestLearnMatchLearnsAnIdTheAnswerEchoes(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		method                string
		recTarget, liveTarget string
		recBody, liveBody     string
		answer                string
	}{
		{name: "in a path segment", method: "PUT", recTarget: "/v1/objects/k-1", liveTarget: "/v1/objects/k-2",
			recBody: `{"size":1}`, liveBody: `{"size":1}`, answer: `{"key":"k-1"}`},
		{name: "in a field naming the resource", method: "POST", recTarget: "/", liveTarget: "/",
			recBody: `{"TableName":"t-1"}`, liveBody: `{"TableName":"t-2"}`, answer: `{"TableDescription":{"TableName":"t-1"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := clockedTracker()
			tr.Begin()
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", 0, tc.method, tc.recTarget, tc.recBody, nil, tc.answer)}}
			served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live(tc.method, tc.liveTarget, tc.liveBody, nil), db, n, opts)
			if err != nil || miss != nil || served == nil || len(db.consumed) != 1 {
				t.Fatalf("served %v miss %+v err %v: want the id learned as the echo it is", served, miss, err)
			}
			if got := db.consumed[0].Spec; len(got.ReqBodyNoise) != 0 || len(got.Clocks) != 0 {
				t.Fatalf("learned noise %v, clocks %v: want only the echo", got.ReqBodyNoise, got.Clocks)
			}
		})
	}
}

// Every leaf that names the call is asked, whatever the learn step learned
// at the others: a variable explaining one leaf leaves another, changed
// with nothing to explain it, refused.
func TestLearnMatchRefusesAnUnexplainedLeafBesideAnExplainedOne(t *testing.T) {
	for _, tc := range []struct {
		name          string
		recs          []*models.Mock
		first, second *req
		leaf          string
	}{
		{name: "a path segment beside a variable in another",
			recs: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
				learnRecorded("mock-1", time.Second, "GET", "/v1/orders/o-1/items/a", "", nil, `{"state":"new"}`),
			},
			first: live("POST", "/v1/orders", `{"order":"o-9"}`, nil), second: live("GET", "/v1/orders/o-9/items/b", "", nil), leaf: "url.path.4"},
		{name: "a version in the path beside the order id an earlier call sent",
			recs: []*models.Mock{
				learnRecorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111","item":"pen"}`, nil, `{"id":"o-1111"}`),
				learnRecorded("mock-2", time.Second, "GET", "/orders/o-1111/v/100", "", nil, `{"id":"o-1111","state":"new"}`),
			},
			first: live("POST", "/orders", `{"id":"o-9999","item":"pen"}`, nil), second: live("GET", "/orders/o-9999/v/200", "", nil), leaf: "url.path.3"},
		{name: "a path segment beside a variable in a field naming the resource",
			recs: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
				learnRecorded("mock-1", time.Second, "POST", "/v1/send/a", `{"TableName":"o-1"}`, nil, `{"state":"new"}`),
			},
			first: live("POST", "/v1/orders", `{"order":"o-9"}`, nil), second: live("POST", "/v1/send/b", `{"TableName":"o-9"}`, nil), leaf: "url.path.2"},
		{name: "a field naming the resource beside a variable in a path segment",
			recs: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
				learnRecorded("mock-1", time.Second, "POST", "/v1/orders/o-1", `{"TableName":"orders"}`, nil, `{"state":"new"}`),
			},
			first: live("POST", "/v1/orders", `{"order":"o-9"}`, nil), second: live("POST", "/v1/orders/o-9", `{"TableName":"audit"}`, nil), leaf: "body.TableName"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			db := &learnDb{perTest: tc.recs}
			ctx := learn.WithTracker(context.Background(), tr)
			if served, miss, err := h.learnMatch(ctx, tc.first, db, n, opts); err != nil || served == nil || miss != nil {
				t.Fatalf("first call: served %v miss %+v err %v", served, miss, err)
			}
			served, miss, err := h.learnMatch(ctx, tc.second, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, tc.leaf+" changed") {
				t.Fatalf("served %v miss %+v: want request_changed naming %s", served, miss, tc.leaf)
			}
		})
	}
}

// Each name that says which call this is, written out, with its class: an
// operation's name is never explained by an echo, a resource's is.
func TestIdentityClassNamesEachLeaf(t *testing.T) {
	for leaf, want := range map[string]string{
		"url.path.0":                    classPath,
		"url.path.7":                    classPath,
		"header.x-amz-target":           classOpHeader,
		"header.soapaction":             classOpHeader,
		"header.x-http-method-override": classOpHeader,
		"header.x-http-method":          classOpHeader,
		"header.x-method-override":      classOpHeader,
		"header.content-type.action":    classSOAPAction,
		"body.Action":                   classOpField,
		"url.query.Action":              classOpField,
		"body.operationName":            classOpField,
		"body.method":                   classOpField,
		"body[].method":                 classOpField,
		"body.QueueUrl":                 classResourceField,
		"body.QueueUrl[]":               classResourceField,
		"body.QueueName":                classResourceField,
		"url.query.QueueName":           classResourceField,
		"body.TableName":                classResourceField,
		"body.Items[].Put.TableName":    classResourceField,
		"body.TopicArn":                 classResourceField,
		"body.TargetArn":                classResourceField,
		"body.StreamName":               classResourceField,
		"body.StreamARN":                classResourceField,
		"body.DeliveryStreamName":       classResourceField,
		"body.logGroupName":             classResourceField,
		"body.logStreamName":            classResourceField,
		"body.stateMachineArn":          classResourceField,
		"body.EventBusName":             classResourceField,
		"body.Entries[].EventBusName":   classResourceField,
		"body.FunctionName":             classResourceField,
		"body.SecretId":                 classResourceField,
		"body.KeyId":                    classResourceField,
		"header.action":                 "",
		"header.method":                 "",
		"header.queueurl":               "",
		"header.x-trace":                "",
		"header.content-type":           "",
		"body.request_id":               "",
		"body.query":                    "",
		"url.query.at":                  "",
		"body.Message":                  "",
	} {
		if got := identityClass(leaf); got != want {
			t.Errorf("identityClass(%q) = %q, want %q", leaf, got, want)
		}
		hold := identityHold(want)
		if wantHold := map[bool]LearnHold{true: LearnExplainedOrEcho, false: LearnExplainedOnly}[classResource(want)]; want != "" && hold != wantHold || want == "" && hold != LearnAny {
			t.Errorf("identityHold(%q) = %v for %s", want, hold, leaf)
		}
	}
	for _, class := range []string{classPath, classResourceField} {
		if !classResource(class) {
			t.Errorf("%s holds an id: an echo explains it", class)
		}
	}
	for _, class := range []string{classOpHeader, classSOAPAction, classOpField, classGraphQL} {
		if classResource(class) {
			t.Errorf("%s names the operation: an echo never explains it", class)
		}
	}
}

// A GraphQL document's operations, by type and name, and what is no GraphQL
// document.
func TestGraphQLOperations(t *testing.T) {
	for doc, want := range map[string]string{
		"query GetOrder { order(id: 1) { id } }":                                         "query GetOrder",
		"{ order(id: 1) { id } }":                                                        "query",
		"mutation { cancel(id: \"}{\") { id } }":                                         "mutation",
		"# a comment {\n subscription Watch($id: ID = \"x{\") { order(id: $id) { id } }": "subscription Watch",
		"fragment F on Order { id }\nquery Get { order { ...F } }":                       "query Get",
		"query A { a } mutation B { b(s: \"\"\"}\"\"\") }":                               "query A,mutation B",
		// A comment between the name and the selection set, with a brace.
		"mutation CancelOrder # {\n { cancel { ok } }": "mutation CancelOrder",
		// A description before a definition, a string or a block string.
		"\"Cancel it\" mutation CancelOrder { cancel { ok } }":           "mutation CancelOrder",
		"\"\"\"Cancel\nit\"\"\" mutation CancelOrder { cancel { ok } }":  "mutation CancelOrder",
		"\"F\" fragment F on Order { id }\nquery Get { order { ...F } }": "query Get",
		// An escaped quote in a string, and in a block string, read as
		// part of it: the braces after it are still inside the selection.
		"query GetOrder { order(note: \"a\\\"}\") { id } }":             "query GetOrder",
		"query GetOrder { order(note: \"\"\"a\\\"\"\"}\"\"\") { id } }": "query GetOrder",
		// A string in the variables' defaults, holding a parenthesis.
		"query GetOrder($n: String = \"(\") { order(n: $n) { id } }": "query GetOrder",
		// A byte order mark before the document.
		"\ufeffquery GetOrder { order { id } }": "query GetOrder",
	} {
		if got, ok := graphQLOperations(doc); !ok || got != want {
			t.Errorf("graphQLOperations(%q) = %q, %v; want %q", doc, got, ok, want)
		}
	}
	for _, doc := range []string{"", "shoes", "query", "query Get", "select * from t", "fragment F on O { id }", "{ a", "query Get { a",
		"mutation CancelOrder { cancel { ok } } x", "\"Cancel it\" { a }", "\"Cancel it mutation C { a }", "\"\"\"Cancel it mutation C { a }",
		"query Get { a(s: \"x) }", "query Get { a(s: \"\"\"x) }", "query Get($s: String = \"{) { a }", "query Get # { a }"} {
		if got, ok := graphQLOperations(doc); ok {
			t.Errorf("graphQLOperations(%q) = %q: want no GraphQL document", doc, got)
		}
	}
}

// A GraphQL document names the call by its operations only in a field named
// query, of the body (a batch's too) or of the URL query: where both values
// are documents whose operations differ, or only one of them reads as one
// (failing closed). A field that reads as no document on either side is no
// class.
func TestIdentityOfAGraphQLDocument(t *testing.T) {
	learned := func(noise, rec, live string) learn.Learned {
		return learn.Learned{Recorded: learn.Leaf{Noise: noise, Value: rec}, Live: learn.Leaf{Noise: noise, Value: live}}
	}
	for _, tc := range []struct {
		l    learn.Learned
		want string
	}{
		{learned("body.query", "query A { a }", "query B { a }"), classGraphQL},
		{learned("body[].query", "query A { a }", "mutation A { a }"), classGraphQL},
		{learned("url.query.query", "{ a }", "mutation { a }"), classGraphQL},
		{learned("body.Query", "query A { a }", "query B { a }"), classGraphQL},
		{learned("body.query", "query A { a(id: 1) }", "query A { a(id: 2) }"), ""},
		{learned("body.query", "shoes", "query B { a }"), classGraphQL},
		{learned("body.query", "query A { a }", "boots"), classGraphQL},
		{learned("body.query", "query A { a }", "query A { a } x"), classGraphQL},
		{learned("body.query", "shoes", "boots"), ""},
		{learned("body.text", "query A { a }", "query B { a }"), ""},
		{learned("header.query", "query A { a }", "query B { a }"), ""},
	} {
		if got := identityOf(tc.l); got != tc.want {
			t.Errorf("identityOf(%s: %q -> %q) = %q, want %q", tc.l.Recorded.Noise, tc.l.Recorded.Value, tc.l.Live.Value, got, tc.want)
		}
	}
}

// What explains a change at a leaf that names the call, and the caller's
// hold before the leaf's class.
func TestHeldLearnedExplains(t *testing.T) {
	leaf := func(noise, rec, live string) (learn.Leaf, learn.Leaf) {
		return learn.Leaf{Path: noise, Noise: noise, Type: learn.String, Value: rec}, learn.Leaf{Path: noise, Noise: noise, Type: learn.String, Value: live}
	}
	r, l := leaf("url.path.2", "2026-11-03T10:00:00Z", "2026-11-03T11:00:00Z")
	offset := models.ClockOffset{Min: time.Hour, Max: time.Hour, Res: time.Second}
	var n learnNoise
	if why := n.heldLearned([]learn.Learned{{Kind: learn.KindClockOffset, Recorded: r, Live: l, Offset: &offset}}, nil); why != "" {
		t.Fatalf("a read of the clock plus an offset at a path segment: %q, want it explained", why)
	}
	r, l = leaf("body.QueueUrl", "http://q/o-1", "http://q/o-9")
	if why := n.heldLearned([]learn.Learned{{Kind: learn.KindNoise, Recorded: r, Live: l}}, []variables.Binding{{Name: "q", Value: "http://q/o-9", Recorded: "http://q/o-1"}}); why != "" {
		t.Fatalf("a value the test gave keploy at a field naming the queue: %q, want it explained", why)
	}
	// The caller holds the operation header echo-only: its hold, not the
	// header's class, says an echo is learned there.
	r, l = leaf("header.x-amz-target", "S.A", "S.B")
	n.hold = func(leaf string) LearnHold {
		if leaf == "header.x-amz-target" {
			return LearnEchoOnly
		}
		return LearnAny
	}
	if why := n.heldLearned([]learn.Learned{{Kind: learn.KindEcho, Recorded: r, Live: l}}, nil); why != "" {
		t.Fatalf("an echo at a leaf the caller holds echo-only: %q, want it learned", why)
	}
	if why := n.heldLearned([]learn.Learned{{Kind: learn.KindVariable, Recorded: r, Live: l}}, nil); !strings.Contains(why, "only as a value the answer carries back") {
		t.Fatalf("a variable at a leaf the caller holds echo-only: %q, want it refused by the caller's hold", why)
	}
	// A leaf of no class the caller holds explained-only is refused with no
	// class named.
	r, l = leaf("body.note", "a", "b")
	n.hold = func(string) LearnHold { return LearnExplainedOnly }
	if why := n.heldLearned([]learn.Learned{{Kind: learn.KindNoise, Recorded: r, Live: l}}, nil); !strings.Contains(why, "body.note changed, and it says which call this is: ") {
		t.Fatalf("noise at a leaf the caller holds explained-only: %q, want it refused naming no class", why)
	}
}

// A recording an earlier keploy learned noise on, replayed: the noise still
// excuses a change at a leaf holding an id (a path segment, a field naming
// the resource) and at a leaf of no class, and no longer at one naming the
// operation, which is refused with a reason naming the field and saying to
// record the test again. keploy.yml's noise excuses either.
func TestReplayOldLearnedNoiseExcusesAnIdNotAnOperation(t *testing.T) {
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	jsonCT := map[string]string{"Content-Type": "application/json"}
	target := func(op string) map[string]string {
		return map[string]string{"Content-Type": "application/x-amz-json-1.0", "X-Amz-Target": "AmazonSQS." + op}
	}
	type yml struct {
		header, body map[string][]string
		url          []string
	}
	for _, tc := range []struct {
		name               string
		path, livePath     string
		header, liveHeader map[string]string
		body, liveBody     string
		learned            []string
		refused            string // the field refused, by its shape; "" where served
		declared           yml    // keploy.yml noise that excuses it
	}{
		{name: "a path segment", path: "/v1/queues/orders/messages", livePath: "/v1/queues/orders-dlq/messages",
			learned: []string{"url.path.2"}},
		{name: "a field naming the resource", path: "/", livePath: "/", header: jsonCT, liveHeader: jsonCT,
			body: `{"QueueUrl":"http://q/1/orders"}`, liveBody: `{"QueueUrl":"http://q/1/orders-dlq"}`, learned: []string{"body.QueueUrl"}},
		{name: "a field of no class", path: "/", livePath: "/", header: jsonCT, liveHeader: jsonCT,
			body: `{"note":"a"}`, liveBody: `{"note":"b"}`, learned: []string{"body.note"}},
		{name: "a GraphQL document whose operation is the same", path: "/graphql", livePath: "/graphql", header: jsonCT, liveHeader: jsonCT,
			body: `{"query":"query GetOrder { order(id: 1) { id } }"}`, liveBody: `{"query":"query GetOrder { order(id: 2) { id } }"}`, learned: []string{"body.query"}},
		{name: "an operation header", path: "/", livePath: "/", header: target("PurgeQueue"), liveHeader: target("DeleteQueue"),
			body: `{}`, liveBody: `{}`, learned: []string{"header.x-amz-target"}, refused: "header.x-amz-target",
			declared: yml{header: map[string][]string{"X-Amz-Target": {}}}},
		{name: "a query parameter naming the operation", path: "/?Action=SendMessage", livePath: "/?Action=PurgeQueue",
			learned: []string{"url.query.Action"}, refused: "query.Action", declared: yml{url: []string{"^[A-Za-z]+$"}}},
		{name: "a form field naming the operation", path: "/", livePath: "/", header: form, liveHeader: form,
			body: "Action=SendMessage&QueueUrl=q", liveBody: "Action=PurgeQueue&QueueUrl=q", learned: []string{"body.Action"},
			refused: "body.Action", declared: yml{body: map[string][]string{"Action": {}}}},
		{name: "a JSON field naming the operation", path: "/rpc", livePath: "/rpc", header: jsonCT, liveHeader: jsonCT,
			body: `{"method":"orders.get","id":1}`, liveBody: `{"method":"orders.cancel","id":1}`, learned: []string{"body.method"},
			refused: "body.method", declared: yml{body: map[string][]string{"method": {}}}},
		{name: "a GraphQL document's operation", path: "/graphql", livePath: "/graphql", header: jsonCT, liveHeader: jsonCT,
			body: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"mutation CancelOrder { cancel { id } }"}`,
			learned: []string{"body.query"}, refused: "body.query", declared: yml{body: map[string][]string{"query": {}}}},
		{name: "a GraphQL document's operation after a comment holding a brace", path: "/graphql", livePath: "/graphql", header: jsonCT, liveHeader: jsonCT,
			body: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"mutation CancelOrder # {\n { cancel { ok } }"}`,
			learned: []string{"body.query"}, refused: "body.query", declared: yml{body: map[string][]string{"query": {}}}},
		{name: "a GraphQL document's operation after a description", path: "/graphql", livePath: "/graphql", header: jsonCT, liveHeader: jsonCT,
			body: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"\"Cancel it\" mutation CancelOrder { cancel { ok } }"}`,
			learned: []string{"body.query"}, refused: "body.query", declared: yml{body: map[string][]string{"query": {}}}},
		{name: "a GraphQL document's operation with text after it", path: "/graphql", livePath: "/graphql", header: jsonCT, liveHeader: jsonCT,
			body: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"mutation CancelOrder { cancel { ok } } x"}`,
			learned: []string{"body.query"}, refused: "body.query", declared: yml{body: map[string][]string{"query": {}}}},
		{name: "a field naming the operation in a learned container", path: "/", livePath: "/", header: jsonCT, liveHeader: jsonCT,
			body: `{"input":{"Action":"SendMessage","at":1}}`, liveBody: `{"input":{"Action":"PurgeQueue","at":2}}`, learned: []string{"body.input"},
			refused: "body.input.Action", declared: yml{body: map[string][]string{"input.Action": {}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := recorded("old", tc.path, tc.header, tc.body)
			rec.Spec.ReqBodyNoise = map[string][]string{}
			for _, l := range tc.learned {
				rec.Spec.ReqBodyNoise[l] = []string{}
			}
			in := func() *req { return sent(tc.livePath, tc.liveHeader, tc.liveBody) }
			got, diag := strictMatch(t, in(), nil, nil, nil, rec)
			if tc.refused == "" {
				if got == nil {
					t.Fatalf("not served: %v; the noise learned there still excuses it", differs(diag))
				}
				return
			}
			if got != nil {
				t.Fatalf("served: noise learned at %s still excused it", tc.refused)
			}
			whys := reasonsOf(diag.differs)
			if len(whys) != 1 || whys[0].Field != tc.refused || !strings.HasPrefix(whys[0].Why, tc.refused+" changed, and it says which call this is (") ||
				!strings.Contains(whys[0].Why, "no longer excuses") || !strings.Contains(whys[0].Why, "Record the test again") {
				t.Fatalf("reasons %+v; want %s's, saying its learned noise no longer excuses it and to record the test again", whys, tc.refused)
			}
			if got, diag := strictMatch(t, in(), tc.declared.header, tc.declared.body, tc.declared.url, rec); got == nil {
				t.Fatalf("keploy.yml's noise did not excuse %s: %v", tc.refused, differs(diag))
			}
		})
	}
}

// Of a multipart form, a part naming the operation is refused as any other
// field is, its learned noise no longer excusing it.
func TestReplayOldLearnedNoiseAtAMultipartOperation(t *testing.T) {
	parts := func(boundary, action, note string) (string, string) {
		part := func(name, v string) string {
			return "--" + boundary + "\r\nContent-Disposition: form-data; name=\"" + name + "\"\r\n\r\n" + v + "\r\n"
		}
		return part("Action", action) + part("note", note) + "--" + boundary + "--\r\n", "multipart/form-data; boundary=" + boundary
	}
	body, ct := parts("rec-b", "SendMessage", "a")
	rec := recorded("old", "/upload", map[string]string{"Content-Type": ct}, body)
	rec.Spec.ReqBodyNoise = map[string][]string{"body.Action": {}, "body.note": {}}
	note, noteCT := parts("b1", "SendMessage", "b")
	if got, diag := strictMatch(t, sent("/upload", map[string]string{"Content-Type": noteCT}, note), nil, nil, nil, rec); got == nil {
		t.Fatalf("a learned part of no class was not excused: %v", differs(diag))
	}
	op, opCT := parts("b2", "PurgeQueue", "a")
	got, diag := strictMatch(t, sent("/upload", map[string]string{"Content-Type": opCT}, op), nil, nil, nil, rec)
	if whys := reasonsOf(diagDiffers(diag)); got != nil || len(whys) != 1 || whys[0].Field != "body.Action" || !strings.Contains(whys[0].Why, "Record the test again") {
		t.Fatalf("served %v, reasons %+v; want body.Action refused, saying to record the test again", got != nil, whys)
	}
}

// Noise learned at a file part's name never excused other bytes in the
// file, and still does not: only a field part's value is read as noise.
func TestReplayOldLearnedNoiseAtAMultipartFile(t *testing.T) {
	file := func(boundary, data string) (string, map[string]string) {
		return "--" + boundary + "\r\nContent-Disposition: form-data; name=\"doc\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\n" + data + "\r\n--" + boundary + "--\r\n",
			map[string]string{"Content-Type": "multipart/form-data; boundary=" + boundary}
	}
	body, ct := file("rec-f", "one")
	rec := recorded("old", "/upload", ct, body)
	rec.Spec.ReqBodyNoise = map[string][]string{"body.doc": {}}
	same, sameCT := file("f1", "one")
	if got, diag := strictMatch(t, sent("/upload", sameCT, same), nil, nil, nil, rec); got == nil {
		t.Fatalf("the same file was not served: %v", differs(diag))
	}
	other, otherCT := file("f2", "two")
	if got, _ := strictMatch(t, sent("/upload", otherCT, other), nil, nil, nil, rec); got != nil {
		t.Fatal("a file with other bytes was served: the noise learned at its part's name excused them")
	}
}

func diagDiffers(d *matchDiag) []strictField {
	if d == nil {
		return nil
	}
	return d.differs
}

// The refusal is said in the agent's log too: the mock, the field and why.
func TestReplayOldLearnedNoiseAtAnOperationIsLogged(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	rec := recorded("old", "/", map[string]string{"X-Amz-Target": "AmazonSQS.PurgeQueue", "X-Tenant": "a"}, `{}`)
	rec.Spec.ReqBodyNoise = map[string][]string{"header.x-amz-target": {}}
	core, logs := observer.New(zap.WarnLevel)
	h := &HTTP{Logger: zap.New(core)}
	// Another header changed too, with no noise learned: a difference, and
	// no reason of its own to log.
	ok, _, _, err := h.match(context.Background(), sent("/", map[string]string{"X-Amz-Target": "AmazonSQS.DeleteQueue", "X-Tenant": "b"}, `{}`), &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}, models.ProvenFlakyHeaderNoise(), nil, nil, true, false, false, true, true)
	if err != nil || ok {
		t.Fatalf("match ok=%v err=%v; want the operation refused", ok, err)
	}
	said := logs.FilterMessage("noise learned at a field naming the call's operation no longer excuses it").All()
	if len(said) != 1 || said[0].ContextMap()["mock"] != "old" || said[0].ContextMap()["field"] != "header.x-amz-target" ||
		!strings.Contains(said[0].ContextMap()["reason"].(string), "Record the test again") {
		t.Fatalf("the log said %v; want the mock, the field and why", said)
	}
	if n := logs.FilterMessage("a learned read of the application's clock does not hold").Len(); n != 0 {
		t.Fatalf("it was logged as a read of the clock too (%d)", n)
	}
}

// A read of the application's clock learned at a leaf naming the operation
// is no noise: the walk lets it change, and clockDiffs checks it.
func TestNoiseHoldsAtAClockReadNamingTheOperation(t *testing.T) {
	k := strictKnown{clocks: map[string]models.ClockOffset{"body.action": {Res: time.Second}}}
	if ok, why := k.noiseHolds("body.action", "body.action", "1", "2"); !ok || why != "" {
		t.Fatalf("noiseHolds = %v, %q; want a clock read left to clockDiffs", ok, why)
	}
	if ok, why := k.noiseHolds("body.Action", "body.Action", "1", "2"); ok || why == "" {
		t.Fatalf("noiseHolds = %v, %q; want noise at the operation refused", ok, why)
	}
}

// In record's full round too, noise a recording already holds at a leaf
// naming the operation excuses no change there: the learn step reads the
// leaf again, as its hold decides. At a leaf holding an id it still does.
func TestLearnMatchOldLearnedNoiseExcusesAnIdNotAnOperation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		target, liveTarget string
		header, liveHeader map[string]string
		body, liveBody     string
		learned            string
		refused            string // the leaf refused; "" where served
	}{
		{name: "a path segment", target: "/v1/queues/orders/messages", liveTarget: "/v1/queues/orders-dlq/messages",
			body: `{}`, liveBody: `{}`, learned: "url.path.2"},
		{name: "an operation header", target: "/", liveTarget: "/", body: `{}`, liveBody: `{}`,
			header: map[string]string{"X-Amz-Target": "S.PurgeQueue"}, liveHeader: map[string]string{"X-Amz-Target": "S.DeleteQueue"},
			learned: "header.x-amz-target", refused: "header.x-amz-target"},
		{name: "a GraphQL document's operation", target: "/graphql", liveTarget: "/graphql",
			body: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"mutation CancelOrder { cancel { id } }"}`,
			learned: "body.query", refused: "body.query"},
		{name: "a GraphQL document's operation after a comment holding a brace", target: "/graphql", liveTarget: "/graphql",
			body: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"mutation CancelOrder # {\n { cancel { ok } }"}`,
			learned: "body.query", refused: "body.query"},
		{name: "a GraphQL document's operation after a description", target: "/graphql", liveTarget: "/graphql",
			body: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"\"Cancel it\" mutation CancelOrder { cancel { ok } }"}`,
			learned: "body.query", refused: "body.query"},
		{name: "a GraphQL document's operation with text after it", target: "/graphql", liveTarget: "/graphql",
			body: `{"query":"query GetOrder { order { id } }"}`, liveBody: `{"query":"mutation CancelOrder { cancel { ok } } x"}`,
			learned: "body.query", refused: "body.query"},
		{name: "a GraphQL document whose operation is the same", target: "/graphql", liveTarget: "/graphql",
			body: `{"query":"query GetOrder { order(id: 1) { id } }"}`, liveBody: `{"query":"query GetOrder { order(id: 2) { id } }"}`,
			learned: "body.query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			rec := learnRecorded("old", 0, "POST", tc.target, tc.body, tc.header, `{}`)
			rec.Spec.ReqBodyNoise = map[string][]string{tc.learned: {}}
			db := &learnDb{perTest: []*models.Mock{rec}}
			served, miss, err := h.learnMatch(context.Background(), live("POST", tc.liveTarget, tc.liveBody, tc.liveHeader), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.refused == "" {
				if served == nil || miss != nil {
					t.Fatalf("miss %+v: the noise learned there still excuses it", miss)
				}
				return
			}
			if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, tc.refused+" changed, and it says which call this is") {
				t.Fatalf("served %v miss %+v: want request_changed naming %s", served, miss, tc.refused)
			}
		})
	}
}

// A value the application sent in an earlier call explains a change at a
// leaf naming the operation only where a replay compares that earlier leaf:
// one its call leaves uncompared as noise, declared in keploy.yml or learned
// (in that call, or on the mock before), is checked against nothing but the
// later value, so the two could name any operation together. At a path
// segment the same value is still followed: both calls name one id.
func TestLearnMatchAVariableFromNoiseExplainsNoOperation(t *testing.T) {
	for _, src := range []struct {
		name     string
		declared map[string][]string // keploy.yml's request-body noise
		learned  map[string][]string // noise mock-0 already holds
	}{
		{name: "learned as noise in the earlier call"},
		{name: "learned as noise on the mock before", learned: map[string][]string{"body.next": {}}},
		{name: "declared noise in keploy.yml", declared: map[string][]string{"next": {}}},
	} {
		for _, at := range []struct {
			name, rec, now string
			call           func(v string) (string, map[string]string)
			refused        string
		}{
			{name: "an operation header", rec: "AmazonSQS.PurgeQueue", now: "AmazonSQS.DeleteQueue",
				call:    func(v string) (string, map[string]string) { return "/", map[string]string{"X-Amz-Target": v} },
				refused: "header.x-amz-target changed, and it says which call this is (an operation header)"},
			{name: "a path segment", rec: "orders", now: "audit",
				call: func(v string) (string, map[string]string) { return "/v1/queues/" + v + "/messages", nil }},
		} {
			t.Run(src.name+" at "+at.name, func(t *testing.T) {
				h, _, opts := learnOn(t)
				n := h.newLearnNoise(opts, src.declared, nil)
				tr := learn.NewTracker()
				tr.Begin()
				first := learnRecorded("mock-0", 0, "POST", "/v1/jobs", `{"next":"`+at.rec+`"}`, nil, `{}`)
				first.Spec.ReqBodyNoise = src.learned
				target, header := at.call(at.rec)
				db := &learnDb{perTest: []*models.Mock{first, learnRecorded("mock-1", time.Second, "POST", target, `{"m":"x"}`, header, `{}`)}}
				ctx := learn.WithTracker(context.Background(), tr)
				if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/jobs", `{"next":"`+at.now+`"}`, nil), db, n, opts); err != nil || served == nil || miss != nil {
					t.Fatalf("first call: served %v miss %+v err %v", served, miss, err)
				}
				target, header = at.call(at.now)
				served, miss, err := h.learnMatch(ctx, live("POST", target, `{"m":"x"}`, header), db, n, opts)
				if err != nil {
					t.Fatal(err)
				}
				if at.refused == "" {
					if served == nil || miss != nil {
						t.Fatalf("miss %+v: the value the earlier call sent explains the id", miss)
					}
					return
				}
				if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, at.refused) {
					t.Fatalf("served %v miss %+v: want request_changed saying %q", served, miss, at.refused)
				}
			})
		}
	}
}

// explained, a variable's half: one whose source leaf a replay compares
// explains a change anywhere; one whose source leaf it leaves uncompared
// (learn.Place.Uncompared) explains one at a leaf holding an id, never at
// one naming the operation, by the leaf's class or by the caller's hold.
func TestExplainedAVariableFromAnUncomparedLeaf(t *testing.T) {
	leaf := func(noise string) learn.Learned {
		return learn.Learned{Kind: learn.KindVariable,
			Recorded: learn.Leaf{Path: noise, Noise: noise, Type: learn.String, Value: "a"},
			Live:     learn.Leaf{Path: noise, Noise: noise, Type: learn.String, Value: "b"}}
	}
	var n learnNoise
	for _, tc := range []struct {
		leaf       string
		side       learn.Side // the source's; "" for a place in a payload's text
		uncompared bool
		want       bool
	}{
		{leaf: "header.x-amz-target", side: learn.Request, want: true},
		{leaf: "header.x-amz-target", side: learn.Response, want: true},
		{leaf: "header.x-amz-target", side: learn.Request, uncompared: true},
		{leaf: "header.x-amz-target", side: learn.Response, uncompared: true},
		{leaf: "header.x-amz-target"},
		{leaf: "body.Action", side: learn.Request, uncompared: true},
		{leaf: "body.Action"},
		{leaf: "url.path.2", side: learn.Request, uncompared: true, want: true},
		{leaf: "url.path.2", want: true},
		{leaf: "body.QueueUrl", side: learn.Request, uncompared: true, want: true},
	} {
		l := leaf(tc.leaf)
		l.From.Side, l.From.Uncompared = tc.side, tc.uncompared
		if got := explained(l, nil, n.holdOf(l)); got != tc.want {
			t.Errorf("a variable at %s, its source on side %q uncompared %v: explained %v, want %v", tc.leaf, tc.side, tc.uncompared, got, tc.want)
		}
	}
	n.hold = func(string) LearnHold { return LearnExplainedOnly }
	l := leaf("body.note")
	l.From.Side, l.From.Uncompared = learn.Request, true
	if explained(l, nil, n.holdOf(l)) {
		t.Errorf("a variable from an uncompared leaf at a leaf the caller holds explained-only: explained, want refused")
	}
	n.hold = func(string) LearnHold { return LearnExplainedOrEcho }
	if !explained(l, nil, n.holdOf(l)) {
		t.Errorf("a variable from an uncompared leaf at a leaf the caller holds explained-or-echo: refused, want explained")
	}
}

// noiseMarked marks the leaves a replay leaves uncompared: a request leaf
// learned as noise in the call, excused by the mock's comparison
// (keploy.yml's noise, the noise learned onto the mock before), or bound by
// an echo of the call's plan, and an answer's
// leaf an echo writes into; never a compared one, nor another leaf of the
// answer, whatever its name, nor a leaf a variable that is no echo binds.
func TestNoiseMarkedMarksOnlyUncomparedLeaves(t *testing.T) {
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, map[string][]string{"declared": {}}, nil)
	m := learnRecorded("mock-0", 0, "POST", "/v1/jobs", `{}`, nil, `{}`)
	m.Spec.ReqBodyNoise = map[string][]string{"body.before": {}}
	seen := func(side learn.Side, path string) learn.Seen {
		return learn.Seen{Side: side, Leaf: learn.Leaf{Path: path, Noise: path, Type: learn.String, Value: "a"}, Live: "b", Place: path}
	}
	e := learn.Earlier{Seen: []learn.Seen{
		seen(learn.Request, "body.now"), seen(learn.Request, "body.before"), seen(learn.Request, "body.declared"),
		seen(learn.Request, "body.compared"), seen(learn.Response, "body.now"),
		seen(learn.Request, "body.echoed"), seen(learn.Response, "body.echoAnswer"), seen(learn.Request, "body.echoAnswer"),
		seen(learn.Request, "body.bound"), seen(learn.Response, "body.boundAnswer"),
	}}
	plan := models.MockVariables{
		"id":   {Recorded: "a", Bind: "body.echoed", Answer: []string{"body.echoAnswer"}, Echo: true},
		"kept": {Recorded: "a", Bind: "body.bound", Answer: []string{"body.boundAnswer"}},
	}
	got := map[string]bool{}
	for _, s := range noiseMarked(e, n.known(m), map[string][]string{"body.now": {}}, plan).Seen {
		got[string(s.Side)+" "+s.Leaf.Path] = s.Uncompared
	}
	want := map[string]bool{"request body.now": true, "request body.before": true, "request body.declared": true,
		"request body.compared": false, "response body.now": false,
		"request body.echoed": true, "response body.echoAnswer": true, "request body.echoAnswer": false,
		"request body.bound": false, "response body.boundAnswer": false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("marked %v, want %v", got, want)
	}
}

// A value the application sent at a leaf an earlier call's echo binds (the
// echo takes whatever it sends there, and writes it into the answer) is
// compared at a replay with nothing but itself. It still explains a later
// call's id: create-then-fetch replays with a third id. It explains no
// change of operation: the later call is refused, its reason naming the
// operation header and the uncompared source, never served.
func TestLearnMatchAVariableFromAnEchoExplainsNoOperation(t *testing.T) {
	for _, at := range []struct {
		name, rec, now, third string
		call                  func(v string) (string, map[string]string)
		refused               string
	}{
		{name: "an operation header", rec: "AmazonSQS.PurgeQueue", now: "AmazonSQS.DeleteQueue",
			call:    func(v string) (string, map[string]string) { return "/", map[string]string{"X-Amz-Target": v} },
			refused: "header.x-amz-target changed, and it says which call this is (an operation header): keploy learns a change there only as a value the application sent in an earlier call at a place keploy compares, a value the test gave keploy, or a read of the clock, and this one carries a value from mock-0 at body.next, which a replay does not compare"},
		{name: "a path segment", rec: "j-7f1c", now: "j-0b9d", third: "j-55aa",
			call: func(v string) (string, map[string]string) { return "/v1/jobs/" + v, nil }},
	} {
		t.Run(at.name, func(t *testing.T) {
			first := func(v string) *req { return live("POST", "/v1/jobs", `{"next":"`+v+`"}`, nil) }
			then := func(v string) *req {
				target, header := at.call(v)
				return live("POST", target, `{"m":"x"}`, header)
			}
			target, header := at.call(at.rec)
			recs := []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/v1/jobs", `{"next":"`+at.rec+`"}`, nil, `{"next":"`+at.rec+`"}`),
				learnRecorded("mock-1", time.Second, "POST", target, `{"m":"x"}`, header, `{}`),
			}
			if at.refused == "" {
				consumed := fullRound(t, recs, []*req{first(at.now), then(at.now)})
				if v := consumed[0].Spec.Learned()["next"]; !v.Echo {
					t.Fatalf("the first call learned %+v, want body.next as an echo", consumed[0].Spec.Learned())
				}
				inCI(t, learnedOnto(recs, consumed), []*req{first(at.third), then(at.third)})
				return
			}
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: recs, observe: tr.ServedMock}
			if served, miss, err := h.learnMatch(ctx, first(at.now), db, n, opts); err != nil || served == nil {
				t.Fatalf("first call: served %v miss %+v err %v", served, miss, err)
			}
			if v := db.consumed[0].Spec.Learned()["next"]; !v.Echo {
				t.Fatalf("the first call learned %+v, want body.next as an echo", db.consumed[0].Spec.Learned())
			}
			served, miss, err := h.learnMatch(ctx, then(at.now), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, at.refused) {
				t.Fatalf("served %v miss %+v: want request_changed saying %q", served, miss, at.refused)
			}
		})
	}
}

// A value the application sent earlier in another protocol's payload (a
// SQL text) names no leaf keploy compares by itself: at an operation header
// the call is refused for the operation, its reason naming the payload,
// before anything else is read of the value.
func TestLearnMatchAValueFromAPayloadExplainsNoOperation(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	tr.ServedMock(&models.Mock{Name: "pg-0", Kind: models.GENERIC, Spec: models.MockSpec{ReqTimestampMock: learnAt,
		GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8",
			Data: "INSERT INTO jobs(op) VALUES ('AmazonSQS.PurgeQueue')"}}}}}})
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-1", time.Second, "POST", "/", `{"m":"x"}`,
		map[string]string{"X-Amz-Target": "AmazonSQS.PurgeQueue"}, `{}`)}}
	served, miss, err := h.learnMatch(ctx, live("POST", "/", `{"m":"x"}`, map[string]string{"X-Amz-Target": "AmazonSQS.DeleteQueue"}), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	const want = "header.x-amz-target changed, and it says which call this is (an operation header): keploy learns a change there only as a value the application sent in an earlier call at a place keploy compares, a value the test gave keploy, or a read of the clock, and this one carries a value from pg-0's payload, which a replay does not compare"
	if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, want) {
		t.Fatalf("served %v miss %+v: want request_changed saying %q", served, miss, want)
	}
}

// soapAction reads the action parameter of SOAP 1.2's media type only: the
// same parameter on another media type names no operation.
func TestSOAPActionReadsOnlySOAP12sMediaType(t *testing.T) {
	for ct, want := range map[string]string{
		`application/soap+xml; charset=utf-8; action="urn:GetOrder"`: "urn:GetOrder",
		`application/json; action="urn:GetOrder"`:                    "",
		`text/xml; action="urn:GetOrder"`:                            "",
		`application/soap+xml; charset=utf-8`:                        "",
		`application/soap+xml; action`:                               "",
	} {
		if got := soapAction(ct); got != want {
			t.Errorf("soapAction(%q) = %q, want %q", ct, got, want)
		}
	}
}

// SOAP 1.2's action is a leaf only where the Content-Type carries one: an
// action added or dropped is a part of the request that is not on both
// sides, refused as such, never read as an action that changed to empty.
func TestLearnMatchRefusesASOAP12ActionAddedOrDropped(t *testing.T) {
	const soap, withAction = `application/soap+xml; charset=utf-8`, `application/soap+xml; charset=utf-8; action="urn:GetOrder"`
	for _, tc := range []struct{ name, rec, live, why string }{
		{"dropped", withAction, soap, "in more than values: header is missing"},
		{"added", soap, withAction, "in more than values: header is not in the recording"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := clockedTracker()
			tr.Begin()
			rec := learnRecorded("mock-1", 0, "POST", "/ws", `<e/>`, map[string]string{"Content-Type": tc.rec}, `{}`)
			db := &learnDb{perTest: []*models.Mock{rec}}
			served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/ws", `<e/>`, map[string]string{"Content-Type": tc.live}), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if served != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, tc.why) ||
				strings.Contains(miss.res.Why, "says which call this is") {
				t.Fatalf("served %v miss %+v: want request_changed saying %q, not an action changed to empty", served, miss, tc.why)
			}
			if len(db.consumed) != 0 || len(rec.Spec.ReqBodyNoise) != 0 {
				t.Fatalf("the refused call took or learned something: consumed %d, noise %v", len(db.consumed), rec.Spec.ReqBodyNoise)
			}
		})
	}
}
