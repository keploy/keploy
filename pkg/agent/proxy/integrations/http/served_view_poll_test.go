package http

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A poll the application signs anew on every call (an SQS receive: its
// date, signature and invocation id change each time) that F answers with
// its recording's answer whole, again and again where F's start-up lasts
// longer than record's, is one view of the recording: F gave the application
// one answer. A later call of the set reads the recording with that one
// view, so a changed value that holds no word of it is learned. Before, each
// signed request named a view of its own, the fifth was past the views a set
// keeps, and the later call refused every changed value as one that may
// stand where keploy could not read what it gave the application: the
// consumer's insert after the polls was refused in record's full round, and
// its test never ran there. A word of the first serve's request, which the
// one view holds, is still refused naming the recording; one only a later
// serve's request carried is the application's own, not what F gave it.
func TestASignedPollServedManyTimesIsOneViewOfItsRecording(t *testing.T) {
	const (
		polls      = 64
		cannotRead = "where keploy could not read what it gave the application"
	)
	invocation := func(i int) string { return fmt.Sprintf("inv%04d", i) }
	signed := func(i int) map[string]string {
		return map[string]string{
			"X-Amz-Target":          "AmazonSQS.ReceiveMessage",
			"X-Amz-Date":            fmt.Sprintf("20261103T2343%02dZ", i%60),
			"Amz-Sdk-Invocation-Id": invocation(i),
			"Authorization":         fmt.Sprintf("AWS4-HMAC-SHA256 Credential=dummy/20261103/us-east-1/sqs/aws4_request, Signature=%064x", i+1),
			"Content-Type":          "application/x-amz-json-1.0",
			"X-Amzn-Query-Mode":     "true",
			"Amz-Sdk-Request":       "attempt=1; max=3",
			"Accept-Encoding":       "gzip",
			"User-Agent":            "aws-sdk-go-v2/1.36.3",
			"Host":                  "sqs.us-east-1.localhost.localstack.cloud:4566",
			"Content-Length":        "122",
		}
	}
	const body = `{"MaxNumberOfMessages":10,"QueueUrl":"http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/q"}`
	for _, tc := range []struct {
		name string
		ref  string // the later call's body.ref in F (recorded "7")
		why  string // "" learns; else a part of the refusal
	}{
		{"a value no answer F gave holds", "wvq", ""},
		{"a value the first serve's request carried", invocation(0), "as record's full round gave it"},
		{"a value only a later serve's request carried", invocation(40), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			poll := learnRecorded("mock-2", time.Second, "POST", "/000000000000/q/", body, signed(-1), `{}`)
			call := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/messages", `{"ref":"7"}`, nil, `{}`)
			tr := learn.NewTracker()
			ctx := learn.WithTracker(context.Background(), tr)
			tr.Begin()
			for i := 0; i < polls; i++ {
				reportPairing(ctx, live("POST", "/000000000000/q/", body, signed(i)), poll, integrations.AnsweredCall{}, false)
			}
			tr.End()
			tr.Begin()
			got, miss, err := h.learnMatch(ctx, live("POST", "/v1/messages", `{"ref":"`+tc.ref+`"}`, nil), &learnDb{perTest: []*models.Mock{call}}, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.why == "" {
				if got == nil || miss != nil {
					t.Fatalf("served %v miss %+v; want body.ref learned: F gave the %d polls one answer, which holds no word of it", got, miss, polls)
				}
				return
			}
			if got != nil || miss == nil || !strings.Contains(miss.res.Why, "mock-2") || !strings.Contains(miss.res.Why, tc.why) || strings.Contains(miss.res.Why, cannotRead) {
				t.Fatalf("FALSE GREEN? served %v miss %+v; want body.ref refused, naming mock-2: %q", got, miss, tc.why)
			}
		})
	}
}

// Two answers F gave of one recording name one view only where they are the
// same in every part answerAsRecorded compares; each part that differs names
// another view, and none is the recorded answer's.
func TestAnAnswersKeyNamesEveryPartOfIt(t *testing.T) {
	base := func() *models.HTTPResp {
		return &models.HTTPResp{StatusCode: 200, StatusMessage: "OK", ProtoMajor: 1, ProtoMinor: 1,
			Header: map[string]string{"Content-Type": "application/json", "X-Amzn-Requestid": "r-1", "Date": "Tue, 03 Nov 2026 23:43:00 GMT",
				"Connection": "keep-alive", "Content-Length": "2", "X-Amz-Crc32": "2745614147"},
			Body:       `{}`,
			StreamBody: []models.HTTPStreamChunk{{TS: time.Unix(1, 0), Data: []models.HTTPStreamDataField{{Key: "data", Value: "a"}}}}}
	}
	key := answerKey(base())
	if key == "" || key == recordedAnswerKey {
		t.Fatalf("an answer keyed %q; want one key of its own", key)
	}
	// One answer is one key however often it is keyed: its headers are
	// hashed in name order, never in a map's.
	for i := 0; i < 32; i++ {
		if answerKey(base()) != key {
			t.Fatal("one answer keyed twice gave two keys: one answer, many views")
		}
	}
	for name, change := range map[string]func(r *models.HTTPResp){
		"status":         func(r *models.HTTPResp) { r.StatusCode = 201 },
		"status message": func(r *models.HTTPResp) { r.StatusMessage = "Created" },
		"protocol major": func(r *models.HTTPResp) { r.ProtoMajor = 2 },
		"protocol minor": func(r *models.HTTPResp) { r.ProtoMinor = 0 },
		"a header value": func(r *models.HTTPResp) { r.Header["X-Amzn-Requestid"] = "r-2" },
		"one more header": func(r *models.HTTPResp) {
			r.Header["X-Extra"] = "e"
		},
		"a header's name": func(r *models.HTTPResp) {
			r.Header["X-Amzn-Requestie"] = r.Header["X-Amzn-Requestid"]
			delete(r.Header, "X-Amzn-Requestid")
		},
		"body":                  func(r *models.HTTPResp) { r.Body = `{"Messages":[]}` },
		"binary":                func(r *models.HTTPResp) { r.Binary = "AA==" },
		"body skipped":          func(r *models.HTTPResp) { r.BodySkipped = true },
		"body size":             func(r *models.HTTPResp) { r.BodySize = 2 },
		"no stream body":        func(r *models.HTTPResp) { r.StreamBody = nil },
		"a stream chunk's time": func(r *models.HTTPResp) { r.StreamBody[0].TS = time.Unix(2, 0) },
		"a stream chunk's data": func(r *models.HTTPResp) { r.StreamBody[0].Data[0].Value = "b" },
		"a stream field's name": func(r *models.HTTPResp) { r.StreamBody[0].Data[0].Key = "event" },
	} {
		r := base()
		change(r)
		if answerKey(r) == key {
			t.Errorf("an answer with another %s keyed as the first: two answers, one view", name)
		}
	}
	// A stream chunk's fields are keyed one by one, each key and value with
	// its length: fields whose text runs into the next one's, or a space
	// moved from a key into its value, are other answers.
	chunks := func(fields ...models.HTTPStreamDataField) *models.HTTPResp {
		r := base()
		r.StreamBody = []models.HTTPStreamChunk{{TS: time.Unix(1, 0), Data: fields}}
		return r
	}
	pairs := [][2]*models.HTTPResp{
		{chunks(models.HTTPStreamDataField{Key: "data", Value: `"x} {id 1"`}), chunks(models.HTTPStreamDataField{Key: "data", Value: `"x`}, models.HTTPStreamDataField{Key: "id", Value: `1"`})},
		{chunks(models.HTTPStreamDataField{Key: "data", Value: "x} {id 1"}), chunks(models.HTTPStreamDataField{Key: "data", Value: "x"}, models.HTTPStreamDataField{Key: "id", Value: "1"})},
		{chunks(models.HTTPStreamDataField{Key: "a b", Value: "c"}), chunks(models.HTTPStreamDataField{Key: "a", Value: "b c"})},
		{chunks(models.HTTPStreamDataField{Key: "ab", Value: ""}), chunks(models.HTTPStreamDataField{Key: "a", Value: "b"})},
	}
	// Each chunk's fields are counted: a field moved into the next chunk,
	// whose key reads as a time, is another answer.
	moved := func(a, b models.HTTPStreamChunk) *models.HTTPResp {
		r := base()
		r.StreamBody = []models.HTTPStreamChunk{a, b}
		return r
	}
	pairs = append(pairs, [2]*models.HTTPResp{
		moved(models.HTTPStreamChunk{TS: time.Unix(0, 1), Data: []models.HTTPStreamDataField{{Key: "5", Value: "x"}}}, models.HTTPStreamChunk{TS: time.Unix(0, 7)}),
		moved(models.HTTPStreamChunk{TS: time.Unix(0, 1)}, models.HTTPStreamChunk{TS: time.Unix(0, 5), Data: []models.HTTPStreamDataField{{Key: "x", Value: "7"}}}),
	})
	for _, pair := range pairs {
		if answerKey(pair[0]) == answerKey(pair[1]) {
			t.Errorf("stream chunks %#v and %#v keyed as one answer: two answers, one view", pair[0].StreamBody, pair[1].StreamBody)
		}
	}
	if answerKey(nil) != "" {
		t.Errorf("no answer keyed %q; want none", answerKey(nil))
	}
}

// Two serves of one recording that gave the application two different
// answers are two views of it, each its own: here the caller of the learn
// step (LearnMatch's written: SQS sets a digest over each message sent)
// wrote count 77 into the first serve's answer and 78 into the second's,
// to the same request. A later call of the set that carries a word only the
// second answer held is refused, naming the recording, as one that carries
// a word of the first; a fresh word is learned. Keyed by the recorded
// answer, the two serves were one view, the first one's, and the second
// answer's word was learned as noise (a false green).
func TestTwoAnswersOfOneRecordingAreTwoViewsALaterCallReads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ref     string // the later call's body.ref in F (recorded "7")
		refused bool   // refused, naming pool-2; else learned
	}{
		{"a word only the second answer held", "78", true},
		{"a word of the first answer", "77", true},
		{"a fresh word", "42", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			opts.LearnPool = true
			pool := learnRecorded("pool-2", time.Second, "POST", "/v1/receive", `{"max":10}`, nil, `{"count":"9"}`)
			pool.TestModeInfo.Lifetime = models.LifetimeSession
			call := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/messages", `{"ref":"7"}`, nil, `{}`)
			tr := learn.NewTracker()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}, observe: tr.ServedMock}
			tr.Begin()
			for _, count := range []string{"77", "78"} {
				written := func(m *models.Mock) *models.Mock {
					c := m.ShallowCopy()
					rs := *m.Spec.HTTPResp
					rs.Body = strings.ReplaceAll(rs.Body, `"9"`, `"`+count+`"`)
					c.Spec.HTTPResp = &rs
					return c
				}
				got, miss, err := h.LearnMatch(ctx, exportCall(live("POST", "/v1/receive", `{"max":10}`, nil)), NewRequestNoise(zap.NewNop(), opts), db, opts, integrations.CallRound{}, written, nil)
				if err != nil || got == nil || got.Name != "pool-2" || got.Spec.HTTPResp.Body != `{"count":"9"}` {
					t.Fatalf("the serve the caller writes %s into: served %v miss %+v err %v; want pool-2 as the learn step gives it", count, got, miss, err)
				}
			}
			tr.End()
			tr.Begin()
			got, miss, err := h.learnMatch(ctx, live("POST", "/v1/messages", `{"ref":"`+tc.ref+`"}`, nil), &learnDb{perTest: []*models.Mock{call}}, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.refused {
				if got == nil || miss != nil {
					t.Fatalf("FALSE RED: served %v miss %+v; want body.ref learned: no answer F gave at pool-2 holds %s", got, miss, tc.ref)
				}
				return
			}
			if got != nil || miss == nil || !strings.Contains(miss.res.Why, "pool-2") {
				t.Fatalf("FALSE GREEN: served %v miss %+v; want body.ref %s refused, naming pool-2: an answer F gave there held it", got, miss, tc.ref)
			}
		})
	}
}
