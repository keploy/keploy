package http

import (
	"context"
	"net/http"
	"net/url"
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

	res, err := h.VariableMatch(ctx, ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, consume)
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
	res, err = h.VariableMatch(ctx, ReadVariableScope(db), sqsCall("dedup-other"), sdkNoise(), []*models.Mock{again}, db, consume)
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

	t.Setenv(provenswitch.Env, "0")
	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("x"), sdkNoise(), []*models.Mock{plain, withVar}, db, never)
	if err != nil || res.Outcome != VariableNone || len(res.Rest) != 2 {
		t.Fatalf("with the switch off the pass ran: %+v, %v", res, err)
	}

	t.Setenv(provenswitch.Env, "1")
	res, err = h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("x"), sdkNoise(), []*models.Mock{plain}, db, never)
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
	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, func(*models.Mock) bool { return false })
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
	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, func(*models.Mock) bool { return true })
	if err != nil || res.Outcome != VariableMiss || res.Problem == "" || res.Served != nil {
		t.Fatalf("a recording that cannot carry its variable: %+v, %v; want a miss with why", res, err)
	}

	m = bad()
	db = &varDb{perTest: []*models.Mock{m}}
	scope := ReadVariableScope(db)
	db.table.Start(nil) // a test begins after the scope was read
	res, err = h.VariableMatch(context.Background(), scope, sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, func(*models.Mock) bool { return true })
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
	res, err := h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), RequestNoise{}, []*models.Mock{m}, db, func(*models.Mock) bool { return true })
	if err != nil || res.Outcome != VariableNone {
		t.Fatalf("without the run's noise, an SDK call's headers fit the recording: %+v, %v", res, err)
	}
	res, err = h.VariableMatch(context.Background(), ReadVariableScope(db), sqsCall("dedup-live"), sdkNoise(), []*models.Mock{m}, db, func(*models.Mock) bool { return true })
	if err != nil || res.Outcome != VariableServed {
		t.Fatalf("with the run's noise, the SDK call was not served by its recording: %+v, %v", res, err)
	}
}

// The live destination is read once per request (PinLiveDestination), and
// not at all with the switch off.
func TestPinLiveDestination(t *testing.T) {
	ctx := context.Background()
	t.Setenv(provenswitch.Env, "0")
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
