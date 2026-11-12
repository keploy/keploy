package http

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// sdkHeaders are the headers an AWS SDK (aws-sdk-go-v2) sends with an SQS
// JSON-protocol call made on day (YYYYMMDD): SigV4 signed, with the date, the
// signature and the SDK's invocation id new on every call.
func sdkHeaders(day, invocation, signature string) map[string]string {
	return map[string]string{
		"Content-Type":          "application/x-amz-json-1.0",
		"X-Amz-Target":          "AmazonSQS.SendMessage",
		"X-Amz-Date":            day + "T101500Z",
		"Amz-Sdk-Invocation-Id": invocation,
		"Amz-Sdk-Request":       "attempt=1; max=3",
		"User-Agent":            "aws-sdk-go-v2/1.30.3 os/linux lang/go#1.23 api/sqs#1.34.3",
		"Authorization": "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/" + day + "/us-east-1/sqs/aws4_request, " +
			"SignedHeaders=amz-sdk-invocation-id;amz-sdk-request;content-type;host;x-amz-date;x-amz-target, Signature=" + signature,
	}
}

// sqsSend is an SQS SendMessage recording (JSON protocol) as an SDK made it
// at record, its deduplication id declared a variable when vars is not nil.
func sqsSend(name, dedup string, vars models.MockVariables) *models.Mock {
	m := at(varMock(name, "POST", "http://sqs.us-east-1.amazonaws.com/",
		`{"QueueUrl":"https://sqs.us-east-1.amazonaws.com/000000000000/orders.fifo","MessageBody":"order 1","MessageGroupId":"g","MessageDeduplicationId":"`+dedup+`"}`,
		200, `{"MessageId":"m-1"}`, vars), 1)
	m.Spec.HTTPReq.Header = sdkHeaders("20261019", "8f0c2b9e-1111-4c1a-9d3e-000000000001", strings.Repeat("a1", 32))
	return m
}

// sqsCall is the live SendMessage of deduplication id dedup, an SDK's call at
// replay, days later: its date, signature and invocation id its own.
func sqsCall(dedup string) VariableCall {
	u, _ := url.Parse("http://sqs.us-east-1.amazonaws.com/")
	h := http.Header{"Host": {"sqs.us-east-1.amazonaws.com"}}
	for k, v := range sdkHeaders("20261023", "5d7e9a31-2222-4b8f-8c6d-000000000002", strings.Repeat("b2", 32)) {
		h.Set(k, v)
	}
	return VariableCall{Method: "POST", URL: u, Header: h,
		Body: []byte(`{"QueueUrl":"https://sqs.us-east-1.amazonaws.com/000000000000/orders.fifo","MessageBody":"order 1","MessageGroupId":"g","MessageDeduplicationId":"` + dedup + `"}`)}
}

// sdkNoise is the run's request noise with no configuration: under the
// switch, the headers known to change on every call.
func sdkNoise() RequestNoise { return NewRequestNoise(zap.NewNop(), models.OutgoingOptions{}) }

// Another parser's call (the SQS parser's) to a recording that declares a
// variable is answered by the variable pass: the variable binds to the
// call's new value, the recording is consumed by the parser's own consume,
// and a later call with another value where the variable is bound is not
// served by it.
func TestVariableMatch_AnotherParsersCallIsServedByTheVariablePass(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := sqsSend("mock-1", "dedup-rec", models.MockVariables{"dedup": bindV("dedup-rec", "body.MessageDeduplicationId")})
	db := &varDb{perTest: []*models.Mock{m}}
	var consumed []string
	consume := func(x *models.Mock) bool { consumed = append(consumed, x.Name); return db.DeleteFilteredMock(*x) }
	ctx := context.Background()

	res, err := h.VariableMatch(ctx, ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, consume, nil)
	if err != nil || res.Outcome != VariableServed || res.Served == nil || res.Served.Name != "mock-1" {
		t.Fatalf("the call with a new deduplication id was not served by its recording: %+v, %v", res, err)
	}
	if strings.Join(consumed, ",") != "mock-1" {
		t.Fatalf("the parser's consume took %v; want mock-1", consumed)
	}
	if b, ok := db.table.Lookup("dedup"); !ok || b.Value != "dedup-live" {
		t.Fatalf("the variable was not bound to the call's value: %+v %v", b, ok)
	}

	// The variable holds dedup-live in this test: a recording of it does not
	// answer a call that sends another value.
	again := sqsSend("mock-2", "dedup-rec", models.MockVariables{"dedup": useV("dedup-rec", []string{"body.MessageDeduplicationId"})})
	res, err = h.VariableMatch(ctx, ReadVariableScope(db), sqsCall("dedup-other"), sdkNoise(), []*models.Mock{again}, db, consume, nil)
	if err != nil || res.Outcome != VariableExactOnly || res.Problem == "" || len(res.Rest) != 0 {
		t.Fatalf("a call breaking the bound variable was not left to an exact recording, with why: %+v, %v", res, err)
	}
}

// Recordings that declare no variables are the caller's to match: the pass
// leaves them in Rest, and with the switch off it leaves the whole pool.
func TestVariableMatch_LeavesTheRestToTheCaller(t *testing.T) {
	h := newHTTP()
	plain := sqsSend("plain", "d", nil)
	withVar := sqsSend("var", "dedup-rec", models.MockVariables{"dedup": bindV("dedup-rec", "body.MessageDeduplicationId")})
	db := &varDb{perTest: []*models.Mock{plain, withVar}}
	never := func(*models.Mock) bool { t.Fatal("consumed with the switch off"); return false }

	t.Setenv(provenswitch.Env, "")
	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("x"), sdkNoise(), []*models.Mock{plain, withVar}, db, never, nil)
	if err != nil || res.Outcome != VariableNone || len(res.Rest) != 2 {
		t.Fatalf("with the switch off the pass ran: %+v, %v", res, err)
	}

	t.Setenv(provenswitch.Env, "1")
	res, err = h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("x"), sdkNoise(), []*models.Mock{plain}, db, never, nil)
	if err != nil || res.Outcome != VariableNone || len(res.Rest) != 1 || res.Rest[0] != plain {
		t.Fatalf("a pool with no variables was not left whole to the caller: %+v, %v", res, err)
	}
}

// A recording another connection took first is checked again.
func TestVariableMatch_ARecordingTakenFirstIsCheckedAgain(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := sqsSend("mock-1", "dedup-rec", models.MockVariables{"dedup": bindV("dedup-rec", "body.MessageDeduplicationId")})
	db := &varDb{perTest: []*models.Mock{m}}
	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, func(*models.Mock) bool { return false }, nil)
	if err != nil || res.Outcome != VariableRetry {
		t.Fatalf("a recording taken first was not checked again: %+v, %v", res, err)
	}
}

// A recording whose answer cannot carry its variable does not answer the
// call: a miss, with why; checked again instead when a test began since the
// scope was read.
func TestVariableMatch_ARecordingThatCannotCarryItsVariableIsAMiss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	bad := func() *models.Mock {
		return sqsSend("mock-1", "dedup-rec", models.MockVariables{"dedup": bindV("dedup-rec", "body.MessageDeduplicationId", "body.Missing")})
	}
	m := bad()
	db := &varDb{perTest: []*models.Mock{m}}
	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, func(*models.Mock) bool { return true }, nil)
	if err != nil || res.Outcome != VariableMiss || res.Problem == "" || res.Served != nil {
		t.Fatalf("a recording that cannot carry its variable: %+v, %v; want a miss with why", res, err)
	}

	m = bad()
	db = &varDb{perTest: []*models.Mock{m}}
	scope := ReadVariableScope(db)
	db.table.Start(nil) // a test begins after the scope was read
	res, err = h.VariableMatch(context.Background(), scope, sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, func(*models.Mock) bool { return true }, nil)
	if err != nil || res.Outcome != VariableRetry {
		t.Fatalf("with a test begun since the scope was read: %+v, %v; want the call checked again", res, err)
	}
}

// An SDK's call is fitted outside the run's request noise: the date, the
// signature and the invocation id differ from the recording's on every call.
// Compared without it, every header by value, no SDK call fit a recording,
// and a call whose variable took a new value was a miss.
func TestVariableMatch_AnSDKCallIsFittedOutsideTheRequestNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := sqsSend("mock-1", "dedup-rec", models.MockVariables{"dedup": bindV("dedup-rec", "body.MessageDeduplicationId")})
	db := &varDb{perTest: []*models.Mock{m}}
	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), RequestNoise{}, []*models.Mock{m}, db, func(*models.Mock) bool { return true }, nil)
	if err != nil || res.Outcome != VariableNone {
		t.Fatalf("without the run's noise, an SDK call's headers fit the recording: %+v, %v", res, err)
	}
	res, err = h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, func(*models.Mock) bool { return true }, nil)
	if err != nil || res.Outcome != VariableServed {
		t.Fatalf("with the run's noise, the SDK call was not served by its recording: %+v, %v", res, err)
	}
}

// The live destination is read once per request (PinLiveDestination), and
// not at all with the switch off.
func TestPinLiveDestination(t *testing.T) {
	ctx := context.Background()
	t.Setenv(provenswitch.Env, "")
	if PinLiveDestination(ctx, sqsCall("x")) != ctx {
		t.Fatal("the destination was pinned with the switch off")
	}
	t.Setenv(provenswitch.Env, "1")
	pinned := PinLiveDestination(ctx, sqsCall("x"))
	if _, ok := pinned.Value(liveDestKey{}).(destination); !ok {
		t.Fatal("the destination was not pinned under the switch")
	}
}

// Another parser's exact pass holds its recordings to the HTTP parser's
// strict rule: an SDK's call with other AWS credentials, on another day, is
// the recording; one to another region, with another signed-header set, or
// with another value of a header outside the noise is not.
func TestStrictFitsHoldsAnotherParsersRecordingsToTheStrictRule(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	rec := sqsSend("plain", "d", nil)
	fits := func(c VariableCall) bool {
		t.Helper()
		got, err := h.StrictFits(context.Background(), c, sdkNoise(), []*models.Mock{rec})
		if err != nil {
			t.Fatal(err)
		}
		return len(got) == 1
	}
	if !fits(sqsCall("d")) {
		t.Fatal("an SDK's call of the recording, signed with its own date and signature, does not fit")
	}
	other := func(edit func(h http.Header)) VariableCall {
		c := sqsCall("d")
		h := c.Header.Clone()
		edit(h)
		c.Header = h
		return c
	}
	if !fits(other(func(h http.Header) {
		h.Set("Authorization", strings.Replace(h.Get("Authorization"), "AKIAEXAMPLE", "AKIACISECRET", 1))
	})) {
		t.Fatal("a call signed with other credentials does not fit (the access key is not compared)")
	}
	for name, edit := range map[string]func(h http.Header){
		"another region": func(h http.Header) {
			h.Set("Authorization", strings.Replace(h.Get("Authorization"), "us-east-1", "eu-west-1", 1))
		},
		"other signed headers": func(h http.Header) {
			h.Set("Authorization", strings.Replace(h.Get("Authorization"), "content-type;", "", 1))
		},
		"another operation":      func(h http.Header) { h.Set("X-Amz-Target", "AmazonSQS.SendMessageBatch") },
		"another header's value": func(h http.Header) { h.Set("Content-Type", "application/json") },
	} {
		if fits(other(edit)) {
			t.Fatalf("%s: the call fits the recording", name)
		}
	}
}

// The parser's written answer is asked of the answer the variables rendered,
// before any bind or take: one it cannot write (nil) is a miss that binds and
// takes nothing, so the recording still answers a later call; one it writes
// is the answer served.
func TestVariableMatch_AskTheWrittenAnswerBeforeTheTake(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := sqsSend("mock-1", "dedup-rec", models.MockVariables{"dedup": bindV("dedup-rec", "body.MessageDeduplicationId")})
	db := &varDb{perTest: []*models.Mock{m}}
	var consumed []string
	consume := func(x *models.Mock) bool { consumed = append(consumed, x.Name); return db.DeleteFilteredMock(*x) }
	var asked []string
	refuse := func(a *models.Mock) *models.Mock { asked = append(asked, a.Name); return nil }

	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, consume, refuse)
	if err != nil || res.Outcome != VariableMiss || res.Served != nil || !strings.Contains(res.Problem, "cannot write an answer") {
		t.Fatalf("an answer the parser cannot write was not a miss: %+v, %v", res, err)
	}
	if strings.Join(asked, ",") != "mock-1" || len(consumed) != 0 || len(db.perTest) != 1 {
		t.Fatalf("asked %v, took %v, %d left; want mock-1 asked, nothing taken", asked, consumed, len(db.perTest))
	}
	if b, ok := db.table.Lookup("dedup"); ok {
		t.Fatalf("a refused answer bound the variable: %+v", b)
	}

	written := func(a *models.Mock) *models.Mock {
		out := *a
		resp := *a.Spec.HTTPResp
		resp.Body = "written"
		out.Spec.HTTPResp = &resp
		return &out
	}
	res, err = h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, consume, written)
	if err != nil || res.Outcome != VariableServed || res.Served == nil || res.Served.Spec.HTTPResp.Body != "written" || strings.Join(consumed, ",") != "mock-1" {
		t.Fatalf("served %+v (took %v), %v; want the written answer, mock-1 taken", res, consumed, err)
	}
	if m.Spec.HTTPResp.Body == "written" {
		t.Fatal("the recording itself was written")
	}
	if b, ok := db.table.Lookup("dedup"); !ok || b.Value != "dedup-live" {
		t.Fatalf("the variable was not bound to the call's value: %+v %v", b, ok)
	}
}

// answeredVarDb is varDb that tells answered, the set's recordings that
// answered a call before and redacted something
// (integrations.AnsweredRedactingReader), or, hiding, tells nothing (a
// wrapper that does not forward the reader).
type answeredVarDb struct {
	*varDb
	answered []*models.Mock
	hiding   bool
}

func (d answeredVarDb) AnsweredRedacting(models.Kind) ([]*models.Mock, bool) {
	return d.answered, !d.hiding
}

// Another parser's call that breaks a variable at a place a recording of
// the pool redacted gets a why that shows the values there redacted, as an
// HTTP miss report shows them: the value the call sent, and the variable's
// value in the test, which an earlier live call sent. Redacted by the
// recording the why names, by another of the pool, or by another that
// answered a call before (on the set's answered list only); a pool that
// redacted nothing there shows them, unless the store does not tell its
// answered recordings: then the why shows no value of the call. The caller's
// pool is left as it was, past its end too.
func TestVariableMatch_TheWhyShowsNoValueAtARedactedPlace(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		name string
		// redacted: the recordings that declare the variable redacted its
		// place; another: a recording of the pool beside them, which
		// declares none, did; answered: that recording answered a call
		// before and is only on the set's answered list; hiding: the store
		// does not tell its answered recordings.
		redacted, another, answered, hiding bool
	}{
		{name: "redacted", redacted: true},
		{name: "redacted-by-another-recording", another: true},
		{name: "redacted-by-a-recording-answered-before", another: true, answered: true},
		{name: "kept", hiding: false},
		{name: "kept-at-a-store-that-does-not-tell", hiding: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHTTP()
			rec := map[bool]string{true: "REDACTED_abc", false: "dedup-rec"}[c.redacted]
			send := func(name string, vars models.MockVariables) *models.Mock {
				m := sqsSend(name, rec, vars)
				if c.redacted {
					m.Noise = []string{`^REDACTED_abc$`}
				}
				return m
			}
			m := send("mock-1", models.MockVariables{"dedup": bindV(rec, "body.MessageDeduplicationId")})
			db := &varDb{perTest: []*models.Mock{m}}
			consume := func(x *models.Mock) bool { return db.DeleteFilteredMock(*x) }
			ctx := context.Background()
			if res, err := h.VariableMatch(ctx, ReadVariableScope(db), sqsCall(liveBound), sdkNoise(), []*models.Mock{m}, db, consume, nil); err != nil || res.Outcome != VariableServed {
				t.Fatalf("the row is of a variable bound by a served call: %+v, %v", res, err)
			}
			// The caller's pool has room past its end, which the why must
			// not write into.
			pool := append(make([]*models.Mock, 0, 4), send("mock-2", models.MockVariables{"dedup": useV(rec, []string{"body.MessageDeduplicationId"})}))
			store := answeredVarDb{varDb: db, hiding: c.hiding}
			if c.another {
				other := sqsSend("mock-3", "REDACTED_xyz", nil)
				other.Spec.HTTPReq.Body = strings.Replace(other.Spec.HTTPReq.Body, "order 1", "order 3", 1)
				other.Noise = []string{`^REDACTED_xyz$`}
				if c.answered {
					store.answered = []*models.Mock{other}
				} else {
					pool = append(pool, other)
				}
			}
			n := len(pool)
			res, err := h.VariableMatch(ctx, ReadVariableScope(db), sqsCall(liveSecret), sdkNoise(), pool, store, consume, nil)
			if err != nil || res.Outcome != VariableExactOnly || !strings.Contains(res.Problem, "mock-2") {
				t.Fatalf("the row is of a call breaking the bound variable, with why: %+v, %v", res, err)
			}
			for i, m := range pool[n:cap(pool)] {
				if m != nil {
					t.Errorf("the why wrote %q past the caller's pool's end (%d)", m.Name, n+i)
				}
			}
			if c.hiding {
				if strings.Contains(res.Problem, liveSecret) || strings.Contains(res.Problem, liveBound) {
					t.Errorf("a store that does not tell its answered recordings, the why shows a value of the call: %s", res.Problem)
				}
			} else if c.redacted || c.another {
				if strings.Contains(res.Problem, liveSecret) || strings.Contains(res.Problem, liveBound) || !strings.Contains(res.Problem, valueRedacted) {
					t.Errorf("the why shows a value at the place a recording redacted: %s", res.Problem)
				}
			} else if !strings.Contains(res.Problem, liveSecret) || !strings.Contains(res.Problem, liveBound) {
				t.Errorf("the why does not show the values at a place no recording redacted: %s", res.Problem)
			}
		})
	}
}

// Another parser's call that breaks a variable at a path segment gets a why
// that shows the values there redacted when another recording of the call's
// shape (as many segments) redacted that segment, in the pool or only on the
// store's answered list, though the recording the why names did not. With
// no recording that redacted it, the why shows them.
func TestVariableMatch_TheWhyShowsNoValueAtARedactedPathSegment(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	call := func(method, path string) VariableCall {
		u, _ := url.Parse("http://store" + path)
		return VariableCall{Method: method, URL: u, Header: http.Header{"Host": {"store"}, "Content-Type": {"application/json"}}}
	}
	for _, c := range []struct {
		name             string
		inPool, answered bool
	}{
		{name: "redacted-by-another-recording", inPool: true},
		{name: "redacted-by-a-recording-answered-before", answered: true},
		{name: "kept"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHTTP()
			bind := at(varMock("mock-1", "POST", "http://store/orders/ord-rec-1", "", 201, `{}`, models.MockVariables{"orderId": bindV("ord-rec-1", "url.path[1]")}), 1)
			db := &varDb{perTest: []*models.Mock{bind}}
			consume := func(x *models.Mock) bool { return db.DeleteFilteredMock(*x) }
			ctx := context.Background()
			if res, err := h.VariableMatch(ctx, ReadVariableScope(db), call("POST", "/orders/"+liveBound), sdkNoise(), []*models.Mock{bind}, db, consume, nil); err != nil || res.Outcome != VariableServed {
				t.Fatalf("the row is of a variable bound by a served call: %+v, %v", res, err)
			}
			use := at(varMock("mock-2", "GET", "http://store/orders/ord-rec-1/items", "", 200, `{}`, models.MockVariables{"orderId": useV("ord-rec-1", []string{"url.path[1]"})}), 2)
			other := at(varMock("other", "GET", "http://store/orders/REDACTED_abc/notes", "", 200, `{}`, nil), 3)
			other.Noise = []string{`^REDACTED_abc$`}
			pool := []*models.Mock{use}
			store := answeredVarDb{varDb: db}
			if c.inPool {
				pool = append(pool, other)
			}
			if c.answered {
				store.answered = []*models.Mock{other}
			}
			res, err := h.VariableMatch(ctx, ReadVariableScope(db), call("GET", "/orders/"+liveSecret+"/items"), sdkNoise(), pool, store, consume, nil)
			if err != nil || res.Outcome != VariableExactOnly || !strings.Contains(res.Problem, "mock-2") {
				t.Fatalf("the row is of a call breaking the bound variable, with why: %+v, %v", res, err)
			}
			shows := strings.Contains(res.Problem, liveSecret)
			if redacted := c.inPool || c.answered; redacted && (shows || strings.Contains(res.Problem, liveBound)) {
				t.Errorf("the why shows a value at the segment another recording redacted: %s", res.Problem)
			} else if !redacted && !shows {
				t.Errorf("with no recording that redacted the segment, the why hides the value the call sent: %s", res.Problem)
			}
		})
	}
}

// Another parser's miss carries the strict comparison's own reasons as the
// HTTP parser's does (StrictMissOf): a message body an older keploy learned
// as noise whole, a text of more than one word, that changed is refused with
// a reason that names the field and says to record again, against the
// recording it names. A body that only differs, or a recording of another
// route, gives no such reason, and a nil miss explains nothing.
func TestStrictMissOf_CarriesTheStrictComparisonsOwnReason(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	withMessage := func(c VariableCall, msg string) VariableCall {
		c.Body = []byte(strings.Replace(string(c.Body), `"MessageBody":"order 1"`, `"MessageBody":"`+msg+`"`, 1))
		return c
	}
	stale := sqsSend("stale", "d", nil)
	stale.Spec.ReqBodyNoise = map[string][]string{"body.MessageBody": {}}
	plain := sqsSend("plain", "d", nil)
	miss := func(c VariableCall, pool ...*models.Mock) *StrictMiss {
		t.Helper()
		if fits, err := h.StrictFits(context.Background(), c, sdkNoise(), pool); err != nil || len(fits) != 0 {
			t.Fatalf("fits %d, err %v; want no recording to fit", len(fits), err)
		}
		return h.StrictMissOf(context.Background(), c, sdkNoise(), pool)
	}
	// noPanic runs f, a nil miss's or a nil report's, failing the row where
	// it panics.
	noPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("%s panicked: %v", name, p)
			}
		}()
		f()
	}

	// far differs in its deduplication id too: stale is the closest, and
	// of two as close the first (strictCompare's closerThan).
	far := sqsSend("far", "x", nil)
	m := miss(withMessage(sqsCall("d"), "evil 2"), far, stale, plain)
	want := []models.FieldReason{{Field: "body.MessageBody", Why: staleWholeWhy("body.MessageBody")}}
	r := &models.MockMismatchReport{FieldReasons: []models.FieldReason{{Field: "body.other", Why: "an earlier report's"}}}
	m.Explain(r)
	if m.Closest() != stale || !reflect.DeepEqual(r.FieldReasons, want) {
		t.Fatalf("closest %v, reasons %+v; want stale, %+v", m.Closest(), r.FieldReasons, want)
	}
	if !strings.Contains(r.FieldReasons[0].Why, "Record the test again") {
		t.Fatalf("the reason %q does not say to record again", r.FieldReasons[0].Why)
	}

	if m := miss(withMessage(sqsCall("d"), "evil 2"), plain); m != nil {
		t.Fatalf("a body that only differs gave %+v; want no reason of the comparison's own", m)
	}
	other := withMessage(sqsCall("d"), "evil 2")
	other.URL = &url.URL{Scheme: "http", Host: "sqs.us-east-1.amazonaws.com", Path: "/000000000000/orders.fifo"}
	if m := miss(other, stale); m != nil {
		t.Fatalf("a recording of another path gave %+v; want no reason", m)
	}

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if m := h.StrictMissOf(ended, withMessage(sqsCall("d"), "evil 2"), sdkNoise(), []*models.Mock{stale}); m != nil {
		t.Fatalf("a call whose context ended gave %+v; want no reason", m)
	}

	var none *StrictMiss
	r = &models.MockMismatchReport{FieldReasons: want}
	noPanic("a nil miss's Explain", func() { none.Explain(r) })
	noPanic("a nil miss's Explain of no report", func() { none.Explain(nil) })
	noPanic("an Explain of no report", func() { m.Explain(nil) })
	noPanic("a nil miss's Closest", func() {
		if none.Closest() != nil {
			t.Fatal("a nil miss named a recording")
		}
	})
	if !reflect.DeepEqual(r.FieldReasons, want) {
		t.Fatal("a nil miss changed the report")
	}
}
