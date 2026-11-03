package http

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/pkg/variables"
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
	} {
		if got, ok := graphQLOperations(doc); !ok || got != want {
			t.Errorf("graphQLOperations(%q) = %q, %v; want %q", doc, got, ok, want)
		}
	}
	for _, doc := range []string{"", "shoes", "query", "query Get", "select * from t", "fragment F on O { id }", "{ a", "query Get { a"} {
		if got, ok := graphQLOperations(doc); ok {
			t.Errorf("graphQLOperations(%q) = %q: want no GraphQL document", doc, got)
		}
	}
}

// A GraphQL document names the call by its operations only in a field named
// query, of the body (a batch's too) or of the URL query, and only where both
// values are documents whose operations differ.
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
		{learned("body.query", "shoes", "query B { a }"), ""},
		{learned("body.query", "query A { a }", "boots"), ""},
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
