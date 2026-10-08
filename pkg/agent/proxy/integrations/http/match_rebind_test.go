package http

import (
	"context"
	"crypto/md5" // #nosec G501 -- checking a recomputed ETag
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Recorded vs live app-minted ids for the rebinding tests. Distinct from the
// correlation tests' recUUID/newUUID so the two suites can't mask each other.
const (
	rbRec   = "0f8fad5b-d9cb-469f-a165-70867728950e"
	rbLive  = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	rbOther = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
)

var (
	rbBase = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
)

// rbMock builds an HTTP dependency mock recorded at rbBase+offset. A request
// with a body carries a JSON Content-Type; a bodyless one carries no headers,
// like the live GETs the tests send.
func rbMock(name, method, rawURL, reqBody string, status int, respBody string, offset time.Duration) *models.Mock {
	reqHeader := map[string]string{}
	if reqBody != "" {
		reqHeader["Content-Type"] = "application/json"
	}
	return &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method),
				URL:    rawURL,
				Header: reqHeader,
				Body:   reqBody,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: status,
				Header:     map[string]string{"Content-Type": "application/json"},
				Body:       respBody,
			},
			ReqTimestampMock: rbBase.Add(offset),
		},
	}
}

func getReq(path string) *req {
	return &req{method: "GET", url: &url.URL{Path: path}, header: http.Header{}}
}

// rbOf is the rebind a match of in over db works with: the matcher's own
// exact comparison, no configured noise.
func rbOf(db integrations.MockMemDb, in *req) *rebind {
	h, noise := newHTTP(), mocknoise.New(httpNoiseAdapter{}, false, false)
	return newRebind(db, in, func(in *req, m *models.Mock) bool {
		return h.sameRequest(context.Background(), in, m, nil, nil, noise.KnownNoise(m, nil))
	})
}

// rbFlow is the framework-first create-then-reuse recording: the app mints an
// item id (rbRec at record time) and POSTs it to the inventory dependency; the
// test then reads it back by that id, and lists the items.
func rbFlow() (create, get, list *models.Mock) {
	create = rbMock("create", "POST", "http://inv/items",
		`{"id":"`+rbRec+`","name":"widget"}`, 201, `{"id":"`+rbRec+`","name":"widget"}`, 0)
	get = rbMock("get", "GET", "http://inv/items/"+rbRec,
		"", 200, `{"id":"`+rbRec+`","name":"widget","stock":5}`, time.Second)
	list = rbMock("list", "GET", "http://inv/items",
		"", 200, `[{"id":"`+rbRec+`","name":"widget"}]`, 2*time.Second)
	return create, get, list
}

func rbMatch(t *testing.T, h *HTTP, db integrations.MockMemDb, in *req) *models.Mock {
	t.Helper()
	ok, stub, _, err := h.match(context.Background(), in, db, nil, nil, nil, true, false, false, true, true)
	if err != nil || !ok || stub == nil {
		t.Fatalf("%s %s must match: ok=%v stub=%v err=%v", in.method, in.url.Path, ok, stub, err)
	}
	return stub
}

func assertServes(t *testing.T, stub *models.Mock, want, notWant string) {
	t.Helper()
	body := stub.Spec.HTTPResp.Body
	if !strings.Contains(body, want) {
		t.Fatalf("mock %q must serve %q; got %q", stub.Name, want, body)
	}
	if notWant != "" && strings.Contains(body, notWant) {
		t.Fatalf("mock %q must NOT serve %q; got %q", stub.Name, notWant, body)
	}
}

func createLive(t *testing.T, h *HTTP, db *mockMemDb) *models.Mock {
	t.Helper()
	return rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`))
}

// The failure this feature exists for: the app mints a NEW id on replay, the
// test carries it into the next call, and the dependency mock recorded with
// the OLD id must answer with the live one — as must the create's own echo.
func TestRebind_FrameworkFirstCreateThenReuse(t *testing.T) {
	h := newHTTP()
	create, get, _ := rbFlow()
	db := &mockMemDb{mocks: []*models.Mock{create, get}, updateUnFilteredReturn: true}

	assertServes(t, createLive(t, h, db), rbLive, rbRec)
	if got := db.bound(rbRec); got[rbRec] != rbLive {
		t.Fatalf("the create, first carrier of the id, must bind it: %v", got)
	}
	stub := rbMatch(t, h, db, getReq("/items/"+rbLive))
	if stub.Name != "get" {
		t.Fatalf("GET by the live id must match the recorded GET; got %q", stub.Name)
	}
	assertServes(t, stub, rbLive, rbRec)
}

// An answer names the live id for the request its recording is of: one that
// names no id at all (a list), and one that names the id this run made. A
// request that still names the RECORDED id — in the path, or in a header —
// speaks of the recorded entity, and is answered exactly as recorded.
func TestRebind_AnAnswerNamesTheLiveIDForItsOwnRequest(t *testing.T) {
	h := newHTTP()
	create, get, list := rbFlow()
	byHeader := rbMock("by-header", "GET", "http://inv/order", "", 200, `{"id":"`+rbRec+`"}`, 3*time.Second)
	byHeader.Spec.HTTPReq.Header = map[string]string{"X-Order-Id": rbRec}
	db := &mockMemDb{mocks: []*models.Mock{create, get, list, byHeader}, updateUnFilteredReturn: true}
	createLive(t, h, db)

	assertServes(t, rbMatch(t, h, db, getReq("/items")), rbLive, rbRec)
	in := getReq("/order")
	in.header.Set("X-Order-Id", rbLive)
	assertServes(t, rbMatch(t, h, db, in), rbLive, rbRec)

	stale := rbMatch(t, h, db, getReq("/items/"+rbRec))
	if stale.Name != "get" {
		t.Fatalf("a read by the recorded id must match its recording; got %q", stale.Name)
	}
	assertServes(t, stale, rbRec, rbLive)
	in = getReq("/order")
	in.header.Set("X-Order-Id", rbRec)
	assertServes(t, rbMatch(t, h, db, in), rbRec, rbLive)
}

// With rebinding off nothing is bound and the read-back is served the
// recorded id exactly as before.
func TestRebind_DisabledServesStale(t *testing.T) {
	h := newHTTP()
	create, get, _ := rbFlow()
	db := &mockMemDb{mocks: []*models.Mock{create, get}, updateUnFilteredReturn: true, noRebind: true}

	createLive(t, h, db)
	assertServes(t, rbMatch(t, h, db, getReq("/items/"+rbLive)), rbRec, rbLive)
	if len(db.bound(rbRec)) != 0 {
		t.Fatal("disabled rebinding must bind nothing")
	}
}

// A value is captured only at its first recorded carrier — which the value
// index knows even after that mock was consumed. A read-back matched leniently
// to another entity's recording must not bind that entity's id.
func TestRebind_NoCaptureFromAConsumer(t *testing.T) {
	h := newHTTP()
	create, get, _ := rbFlow()
	db := &mockMemDb{mocks: []*models.Mock{create, get}, updateUnFilteredReturn: true}

	rbMatch(t, h, db, getReq("/items/"+rbLive)) // no create first
	if got := db.bound(rbRec); len(got) != 0 {
		t.Fatalf("a read-back must not capture a binding; got %v", got)
	}
}

// An id made this run stands where a recorded generated UUID stood, and is a
// UUID itself — of any version — that the recording carries nowhere. Anything
// else in that place is not the app making its id anew.
func TestRebind_AnIDMadeThisRunIsAUUIDTheRecordingDoesNotCarry(t *testing.T) {
	const elsewhereID = "5c0d3f2a-7b1e-4a9d-8c6f-0e1d2c3b4a59"
	for name, c := range map[string]struct {
		live      string
		wantBound bool
	}{
		"another random UUID":                    {rbLive, true},
		"a UUID of another generated version":    {"017f22e2-79b0-7cc3-98c4-dc0c0c07398f", true},
		"as long as a UUID, and not one":         {"7c9e6679_7425_40de_944b_e07fc1f90ae7", false},
		"a UUID the recording carries elsewhere": {elsewhereID, false},
		"something that is no id at all":         {"a widget, in blue, size 4, please!!", false},
	} {
		h := newHTTP()
		create := rbMock("create", "POST", "http://inv/items", `{"id":"`+rbRec+`","name":"widget"}`, 201, `{"id":"`+rbRec+`"}`, 0)
		elsewhere := rbMock("elsewhere", "GET", "http://inv/things/"+elsewhereID, "", 200, `{}`, time.Second)
		db := &mockMemDb{mocks: []*models.Mock{create, elsewhere}, updateUnFilteredReturn: true}
		stub := rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+c.live+`","name":"widget"}`))
		if got := db.bound(rbRec)[rbRec] == c.live; got != c.wantBound {
			t.Errorf("%s: bound=%v, want %v", name, got, c.wantBound)
		}
		if want := map[bool]string{true: c.live, false: rbRec}[c.wantBound]; !strings.Contains(stub.Spec.HTTPResp.Body, want) {
			t.Errorf("%s: answered %s, want it to name %s", name, stub.Spec.HTTPResp.Body, want)
		}
	}
}

// The app names an object by an id it minted (PUT /objects/<id>) and reads it
// back: the PUT binds the id; the GET by the live id matches the recorded GET.
func TestRebind_URLPathProducer(t *testing.T) {
	h := newHTTP()
	put := rbMock("put", "PUT", "http://store/objects/"+rbRec, `{"size":3}`, 200, `{"ok":true}`, 0)
	get := rbMock("get", "GET", "http://store/objects/"+rbRec, "", 200, `{"key":"`+rbRec+`","size":3}`, time.Second)
	db := &mockMemDb{mocks: []*models.Mock{put, get}, updateUnFilteredReturn: true}

	rbMatch(t, h, db, jsonReq("PUT", "/objects/"+rbLive, `{"size":3}`))
	if got := db.bound(rbRec); got[rbRec] != rbLive {
		t.Fatalf("the PUT must bind the recorded id to the live one; bound %v", got)
	}
	stub := rbMatch(t, h, db, getReq("/objects/"+rbLive))
	if stub.Name != "get" {
		t.Fatalf("GET by the live id must match the recorded GET; got %q", stub.Name)
	}
	assertServes(t, stub, rbLive, rbRec)
}

// An id the app minted travels as a query value.
func TestRebind_QueryProducer(t *testing.T) {
	h := newHTTP()
	m := rbMock("reserve", "POST", "http://inv/reserve?ref="+rbRec, "", 200, `{"ref":"`+rbRec+`"}`, 0)
	m.Spec.HTTPReq.URLParams = map[string]string{"ref": rbRec} // as the recorder writes it
	db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

	in := getReq("/reserve")
	in.method = "POST"
	in.url.RawQuery = "ref=" + rbLive
	stub := rbMatch(t, h, db, in) // the auto-dynamic pass tolerates a uuid query value
	if got := db.bound(rbRec); got[rbRec] != rbLive {
		t.Fatalf("the query value must bind; bound %v", got)
	}
	assertServes(t, stub, rbLive, rbRec)
}

// Strict enforcement tolerates only configured or learned noise, so v1 keeps
// rebinding out of it entirely.
func TestRebind_StrictModeDoesNotRebind(t *testing.T) {
	h := newHTTP()
	create, _, _ := rbFlow()
	create.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
	db := &mockMemDb{mocks: []*models.Mock{create}, updateUnFilteredReturn: true}

	ok, _, _, err := h.match(context.Background(), jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`), db, nil, nil, nil, true, false, true, true, true)
	if err != nil || !ok {
		t.Fatalf("the noised id must still match under strict: ok=%v err=%v", ok, err)
	}
	if len(db.bound(rbRec)) != 0 {
		t.Fatal("strict mode must not bind")
	}
}

// One recorded id meeting two different live ids in one request is not a binding.
func TestRebind_ContradictoryAlignmentBindsNothing(t *testing.T) {
	h := newHTTP()
	m := rbMock("pair", "POST", "http://inv/pair", `{"a":"`+rbRec+`","b":"`+rbRec+`"}`, 200, `{"ok":true}`, 0)
	db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

	rbMatch(t, h, db, jsonReq("POST", "/pair", `{"a":"`+rbLive+`","b":"`+rbOther+`"}`))
	if len(db.bound(rbRec)) != 0 {
		t.Fatal("a contradictory alignment must not bind")
	}
}

// One recording that echoes a per-request value, asked twice. The first
// request binds the value, once. The second sends another value, and a
// recorded id has one live id: it is not followed, and is answered exactly as
// it is without rebinding — as recorded, or, where the mock was staged with
// its echo correlated (as the agent stages every mock), with its own value by
// the correlation pass.
//
// Rebinding does not answer the second request with its own value itself:
// that takes a second live value for one recorded id, and choosing between
// several is where an answer can name another request's.
func TestRebind_AnEchoThroughAReusedCreatorIsBoundOnce(t *testing.T) {
	for _, correlated := range []bool{false, true} {
		var second [2]string
		for i, on := range []bool{true, false} {
			h := newHTTP()
			m := rbMock("nonce", "POST", "http://auth/nonce", `{"nonce":"`+rbRec+`"}`, 200, `{"nonce":"`+rbRec+`"}`, 0)
			m.TestModeInfo.Lifetime = models.LifetimeSession
			if correlated {
				mocknoise.MaterializeCorrelations(m)
			}
			db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true, noRebind: !on}
			first := rbMatch(t, h, db, jsonReq("POST", "/nonce", `{"nonce":"`+rbLive+`"}`))
			second[i] = rbMatch(t, h, db, jsonReq("POST", "/nonce", `{"nonce":"`+rbOther+`"}`)).Spec.HTTPResp.Body
			if !on {
				continue
			}
			assertServes(t, first, rbLive, rbRec)
			if got := db.bound(rbRec); got[rbRec] != rbLive {
				t.Fatalf("correlated=%v: the first value must stay bound: %v", correlated, got)
			}
			if rec, ok := db.bindings.Recorded(rbOther); ok {
				t.Fatalf("correlated=%v: the second value was bound too, for %s", correlated, rec)
			}
		}
		if second[0] != second[1] {
			t.Fatalf("correlated=%v: the second request must be answered as without rebinding:\n on  %s\n off %s", correlated, second[0], second[1])
		}
		if want := map[bool]string{false: rbRec, true: rbOther}[correlated]; !strings.Contains(second[0], want) || strings.Contains(second[0], rbLive) {
			t.Fatalf("correlated=%v: the second request was answered %s", correlated, second[0])
		}
	}
}

// A rewritten body is re-signed: DynamoDB's x-amz-crc32 (the AWS SDKs verify
// it) is recomputed over the body served. A body served encoded cannot be
// re-signed here, so it is served as recorded.
func TestRebind_RewrittenBodyIsReSigned(t *testing.T) {
	h := newHTTP()
	put := rbMock("put", "POST", "http://ddb/", `{"Item":{"id":{"S":"`+rbRec+`"}}}`, 200, `{}`, 0)
	getBody := `{"Item":{"id":{"S":"` + rbRec + `"}}}`
	get := rbMock("get", "POST", "http://ddb/", `{"Key":{"id":{"S":"`+rbRec+`"}}}`, 200, getBody, time.Second)
	get.Spec.HTTPResp.Header["X-Amz-Crc32"] = strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(getBody))), 10)
	db := &mockMemDb{mocks: []*models.Mock{put, get}, updateUnFilteredReturn: true}

	rbMatch(t, h, db, jsonReq("POST", "/", `{"Item":{"id":{"S":"`+rbLive+`"}}}`))
	stub := rbMatch(t, h, db, jsonReq("POST", "/", `{"Key":{"id":{"S":"`+rbLive+`"}}}`))
	assertServes(t, stub, rbLive, rbRec)
	want := strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(stub.Spec.HTTPResp.Body))), 10)
	if got := stub.Spec.HTTPResp.Header["X-Amz-Crc32"]; got != want {
		t.Fatalf("x-amz-crc32 %s does not sign the served body (want %s)", got, want)
	}

	encoded := rbMock("enc", "GET", "http://ddb/enc/"+rbRec, "", 200, `{"id":"`+rbRec+`"}`, 2*time.Second)
	encoded.Spec.HTTPResp.Header["Content-Encoding"] = "gzip"
	encoded.Spec.HTTPResp.Header["Content-MD5"] = "x"
	db.mocks = append(db.mocks, encoded)
	assertServes(t, rbMatch(t, h, db, getReq("/enc/"+rbLive)), rbRec, rbLive)
}

// A copy stands in for its original. The id here travels in a header, which
// matching checks only by key, so a copy and its original are otherwise the
// same request: kept side by side they would form one stateful group of four
// and serve each recording twice (g1,g1,g2,g2).
func TestRebind_CopyReplacesItsOriginalInAStatefulGroup(t *testing.T) {
	h := newHTTP()
	create, _, _ := rbFlow()
	g1 := rbMock("g1", "GET", "http://inv/item", "", 200, `{"n":1}`, time.Second)
	g2 := rbMock("g2", "GET", "http://inv/item", "", 200, `{"n":2}`, 2*time.Second)
	for _, g := range []*models.Mock{g1, g2} {
		g.Spec.HTTPReq.Header = map[string]string{"X-Item-Id": rbRec}
		g.TestModeInfo.Consume = models.ConsumeCursorSaturate
	}
	db := &mockMemDb{mocks: []*models.Mock{create, g1, g2}, updateUnFilteredReturn: true}

	createLive(t, h, db)
	var got []string
	for i := 0; i < 3; i++ {
		in := getReq("/item")
		in.header.Set("X-Item-Id", rbLive)
		got = append(got, rbMatch(t, h, db, in).Name)
	}
	if strings.Join(got, ",") != "g1,g2,g2" {
		t.Fatalf("served %v, want g1,g2,g2", got)
	}
}

// A capture made in an epoch that has since ended (the replay moved to the
// next test-set while the match ran) is dropped.
func TestRebind_ACaptureFromAnEndedEpochIsDropped(t *testing.T) {
	create, _, _ := rbFlow()
	db := &mockMemDb{mocks: []*models.Mock{create}, updateUnFilteredReturn: true}
	r := rbOf(db, jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`))
	db.epoch++ // the test-set ended
	if !r.fits(create) {
		t.Fatal("the create is the recorded one with this run's id")
	}
	if r.commit(create) {
		t.Fatal("a commit from an ended epoch must be reported as dropped")
	}
	if len(db.bound(rbRec)) != 0 || db.bindings.Claimed(create.Name) {
		t.Fatal("a commit from an ended epoch must bind and claim nothing")
	}
}

// Two creates recorded with identical payloads but their own minted ids:
// each live create binds its own recording, in recorded order, so each
// read-back is answered from its own entity's recording.
func TestRebind_IdenticalCreatesBindInRecordedOrder(t *testing.T) {
	h := newHTTP()
	x1, x2 := rbRec, rbOther
	c1 := rbMock("c1", "POST", "http://inv/items", `{"id":"`+x1+`","name":"widget"}`, 201, `{"id":"`+x1+`"}`, 0)
	c2 := rbMock("c2", "POST", "http://inv/items", `{"id":"`+x2+`","name":"widget"}`, 201, `{"id":"`+x2+`"}`, time.Second)
	g1 := rbMock("g1", "GET", "http://inv/items/"+x1, "", 200, `{"id":"`+x1+`","n":1}`, 2*time.Second)
	g2 := rbMock("g2", "GET", "http://inv/items/"+x2, "", 200, `{"id":"`+x2+`","n":2}`, 3*time.Second)
	for _, m := range []*models.Mock{c1, c2, g1, g2} {
		m.TestModeInfo.Lifetime = models.LifetimeSession // keploy mock record writes them reusable
	}
	db := &mockMemDb{mocks: []*models.Mock{c1, c2, g1, g2}, updateUnFilteredReturn: true}
	y1, y2 := rbLive, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+y1+`","name":"widget"}`))
	rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+y2+`","name":"widget"}`))
	if got := db.bound(x1, x2); got[x1] != y1 || got[x2] != y2 {
		t.Fatalf("each create must bind its own recording: %v", got)
	}
	stub := rbMatch(t, h, db, getReq("/items/"+y2))
	if stub.Name != "g2" {
		t.Fatalf("the second item's read-back must be its own recording; got %q", stub.Name)
	}
	assertServes(t, stub, `"n":2`, "")
}

// An app that asks its dependency for an id nobody made — by path, or in a
// JSON body — is matched leniently to another entity's recording, as it always
// was. It is answered exactly as recorded and the match is reported: rewriting
// the answer to name this run's id would make the wrong question look right.
func TestRebind_AQuestionAboutAnotherEntityIsAnsweredAsRecorded(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	h := newHTTP()
	h.Logger = zap.New(core)
	create, get, _ := rbFlow()
	lookup := rbMock("lookup", "POST", "http://inv/lookup", `{"item":"`+rbRec+`"}`, 200, `{"id":"`+rbRec+`","stock":5}`, 2*time.Second)
	db := &mockMemDb{mocks: []*models.Mock{create, get, lookup}, updateUnFilteredReturn: true}
	createLive(t, h, db) // rbRec is bound to rbLive

	for name, in := range map[string]*req{
		"by path": getReq("/items/" + rbOther), // nobody made rbOther
		"by body": jsonReq("POST", "/lookup", `{"item":"`+rbOther+`"}`),
	} {
		before := logs.Len()
		stub := rbMatch(t, h, db, in)
		body := stub.Spec.HTTPResp.Body
		if !strings.Contains(body, rbRec) || strings.Contains(body, rbLive) || strings.Contains(body, rbOther) {
			t.Fatalf("%s: must be answered as recorded, got %q", name, body)
		}
		warned := logs.All()[before:]
		if len(warned) != 1 || warned[0].ContextMap()["recorded_id"] != rbRec || warned[0].ContextMap()["this_runs_id"] != rbLive {
			t.Fatalf("%s: the match to another entity's recording must be reported once, naming both ids: %v", name, warned)
		}
	}
	// The question about the entity this run made is answered with its id,
	// and one that names the recorded id as recorded; neither is reported.
	before := logs.Len()
	assertServes(t, rbMatch(t, h, db, getReq("/items/"+rbLive)), rbLive, rbRec)
	assertServes(t, rbMatch(t, h, db, jsonReq("POST", "/lookup", `{"item":"`+rbLive+`"}`)), rbLive, rbRec)
	assertServes(t, rbMatch(t, h, db, getReq("/items/"+rbRec)), rbRec, rbLive)
	if logs.Len() != before {
		t.Fatalf("a request for this run's entity, or for the recorded one, is not reported: %v", logs.All()[before:])
	}
}

// S3 and GCS checksums are recomputed over a rewritten body; an algorithm that
// cannot be recomputed here keeps the response as recorded.
func TestReSign(t *testing.T) {
	old, nu := `{"k":"a"}`, `{"k":"b"}`
	h := map[string]string{
		"x-amz-checksum-crc32":  "stale",
		"x-amz-checksum-crc32c": "stale",
		"x-amz-checksum-sha256": "stale",
		"x-amz-checksum-type":   "FULL_OBJECT",
		"x-goog-hash":           "crc32c=stale,md5=stale",
		"Content-Digest":        "sha-256=:stale:",
	}
	if !reSign(h, old, nu) {
		t.Fatal("all of these can be recomputed")
	}
	if h["x-amz-checksum-crc32"] != b64u32(crc32.ChecksumIEEE([]byte(nu))) ||
		h["x-amz-checksum-crc32c"] != b64u32(crc32.Checksum([]byte(nu), castagnoli)) {
		t.Fatalf("crc checksums not recomputed: %v", h)
	}
	if h["x-amz-checksum-type"] != "FULL_OBJECT" {
		t.Fatalf("the checksum type is not a checksum and must be kept: %v", h)
	}
	if !strings.HasPrefix(h["x-goog-hash"], "crc32c=") || strings.Contains(h["x-goog-hash"], "stale") {
		t.Fatalf("x-goog-hash not recomputed: %q", h["x-goog-hash"])
	}
	if !strings.HasPrefix(h["Content-Digest"], "sha-256=:") || strings.Contains(h["Content-Digest"], "stale") {
		t.Fatalf("Content-Digest not recomputed: %q", h["Content-Digest"])
	}
	nvme := map[string]string{"x-amz-checksum-crc64nvme": "stale"}
	if !reSign(nvme, old, "123456789") || nvme["x-amz-checksum-crc64nvme"] != "rosUhgp5mIg=" { // CRC-64/NVME check value 0xae8b14860a799888
		t.Fatalf("crc64nvme not recomputed: %v", nvme)
	}
	if reSign(map[string]string{"x-amz-checksum-crc128": "x"}, old, nu) {
		t.Fatal("a checksum that cannot be recomputed must keep the response as recorded")
	}
	if reSign(map[string]string{"Content-Encoding": "gzip", "Content-MD5": "x"}, old, nu) {
		t.Fatal("an encoded body cannot be re-signed here")
	}
	if !reSign(map[string]string{"Content-Type": "application/json"}, old, nu) {
		t.Fatal("a response with no integrity header needs no re-signing")
	}

	oldMD5, newMD5 := md5.Sum([]byte(old)), md5.Sum([]byte(nu))
	weak := map[string]string{"ETag": `W/"` + hex.EncodeToString(oldMD5[:]) + `"`}
	if !reSign(weak, old, nu) || weak["ETag"] != `W/"`+hex.EncodeToString(newMD5[:])+`"` {
		t.Fatalf("an MD5 ETag is recomputed and stays weak: %v", weak)
	}
	// Whatever order the headers are visited in (a map's order varies).
	for i := 0; i < 64; i++ {
		if reSign(map[string]string{"Content-Encoding": "gzip", "ETag": `"` + hex.EncodeToString(oldMD5[:]) + `"`}, old, nu) {
			t.Fatal("an encoded body whose ETag is its MD5 cannot be re-signed here")
		}
	}
	opaque := map[string]string{"Content-Encoding": "gzip", "ETag": `W/"2d-kJ8sYvA1Gq0y3ZbW8xX9nQpLmTo"`}
	if !reSign(opaque, old, nu) || opaque["ETag"] != `W/"2d-kJ8sYvA1Gq0y3ZbW8xX9nQpLmTo"` {
		t.Fatalf("an opaque ETag is no digest of the body: kept, and no reason to serve as recorded: %v", opaque)
	}
}

// With nothing bound, a match does no rebinding work: the live request is not
// tokenized and no copy is made.
func TestRebind_NothingBoundDoesNothing(t *testing.T) {
	create, get, _ := rbFlow()
	db := &mockMemDb{mocks: []*models.Mock{create, get}}
	r := rbOf(db, getReq("/items/"+rbLive))
	if r == nil || r.active || r.hits != nil {
		t.Fatalf("an empty table must short-circuit: %+v", r)
	}
	pool := []*models.Mock{create, get}
	if got := r.candidates(pool); &got[0] != &pool[0] {
		t.Fatal("no copies with nothing bound")
	}
}

// More live calls than recordings of one shape: once the click recording is
// claimed, the next click is still answered from it, not from an unclaimed
// recording of another shape — in every mode, as with rebinding off.
func TestRebind_ARepeatedCallKeepsItsShape(t *testing.T) {
	for _, on := range []bool{true, false} {
		h := newHTTP()
		click := rbMock("click", "POST", "http://bus/events", `{"type":"click","target":"buy-button","eventId":"`+rbRec+`"}`, 202, `{"ack":"click"}`, 0)
		view := rbMock("view", "POST", "http://bus/events", `{"type":"view","target":"home-page","eventId":"`+rbOther+`"}`, 202, `{"ack":"view"}`, time.Second)
		for _, m := range []*models.Mock{click, view} {
			m.TestModeInfo.Lifetime = models.LifetimeSession
		}
		db := &mockMemDb{mocks: []*models.Mock{click, view}, updateUnFilteredReturn: true, noRebind: !on}
		for _, id := range []string{rbLive, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"} {
			got := rbMatch(t, h, db, jsonReq("POST", "/events", `{"type":"click","target":"buy-button","eventId":"`+id+`"}`))
			if got.Name != "click" {
				t.Fatalf("enabled=%v: a click was answered from %q", on, got.Name)
			}
		}
	}
}

// Among creators the request fits equally but for ids, the one that carries
// one of the request's values verbatim (a SKU, a content hash) wins over one
// that would need that value to differ too.
func TestRebind_TemplateMatchKeepsAVerbatimValue(t *testing.T) {
	h := newHTTP()
	a := rbMock("a", "POST", "http://cart/add", `{"sku":"a1b2c3d4e5f60718","req":"`+rbRec+`"}`, 200, `{"sku":"a"}`, 0)
	b := rbMock("b", "POST", "http://cart/add", `{"sku":"ffeeddccbbaa9988","req":"`+rbOther+`"}`, 200, `{"sku":"b"}`, time.Second)
	db := &mockMemDb{mocks: []*models.Mock{a, b}, updateUnFilteredReturn: true}
	got := rbMatch(t, h, db, jsonReq("POST", "/add", `{"sku":"ffeeddccbbaa9988","req":"`+rbLive+`"}`))
	if got.Name != "b" {
		t.Fatalf("the request for sku ffeeddccbbaa9988 was answered from %q", got.Name)
	}
}

// The first of two identical creates binds the first recording even when its
// fresh id happens to be fewer edits from the second recording's: between two
// ids minted independently that is noise, and the recorded order is what a
// test may rely on.
func TestRebind_FirstCreateBindsTheFirstRecording(t *testing.T) {
	h := newHTTP()
	x1, x2 := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	c1 := rbMock("c1", "POST", "http://inv/items", `{"id":"`+x1+`","name":"widget"}`, 201, `{"id":"`+x1+`","seq":1}`, 0)
	c2 := rbMock("c2", "POST", "http://inv/items", `{"id":"`+x2+`","name":"widget"}`, 201, `{"id":"`+x2+`","seq":2}`, time.Second)
	for _, m := range []*models.Mock{c1, c2} {
		m.TestModeInfo.Lifetime = models.LifetimeSession
	}
	db := &mockMemDb{mocks: []*models.Mock{c1, c2}, updateUnFilteredReturn: true}
	y1 := "22222222-2222-4abc-9def-0123456789ab" // unrelated to both, yet 18 edits from x2 and 30 from x1
	assertServes(t, rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+y1+`","name":"widget"}`)), `"seq":1`, "")
	if got := db.bound(x1, x2); got[x1] != y1 {
		t.Fatalf("the first create must bind the first recording: %v", got)
	}
}

// Identical creates from an instrumented app: each request carries its own
// trace header, which no match can bind. Each create must still bind its own
// recording.
func TestRebind_IdenticalCreatesWithTraceHeaders(t *testing.T) {
	h := newHTTP()
	x1, x2 := rbRec, rbOther
	trace := func(n int) string { return fmt.Sprintf("00-%032x-%016x-01", n+0xabc000, n+0xdef000) }
	c1 := rbMock("c1", "POST", "http://inv/items", `{"id":"`+x1+`","name":"widget"}`, 201, `{"id":"`+x1+`"}`, 0)
	c2 := rbMock("c2", "POST", "http://inv/items", `{"id":"`+x2+`","name":"widget"}`, 201, `{"id":"`+x2+`"}`, time.Second)
	c1.Spec.HTTPReq.Header["Traceparent"] = trace(1)
	c2.Spec.HTTPReq.Header["Traceparent"] = trace(2)
	for _, m := range []*models.Mock{c1, c2} {
		m.TestModeInfo.Lifetime = models.LifetimeSession
	}
	db := &mockMemDb{mocks: []*models.Mock{c1, c2}, updateUnFilteredReturn: true}
	y1, y2 := rbLive, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for i, y := range []string{y1, y2} {
		in := jsonReq("POST", "/items", `{"id":"`+y+`","name":"widget"}`)
		in.header.Set("Traceparent", trace(100+i))
		rbMatch(t, h, db, in)
	}
	if got := db.bound(x1, x2); got[x1] != y1 || got[x2] != y2 {
		t.Fatalf("each create must bind its own recording: %v", got)
	}
}

// A compressed response with an ordinary ETag (a version tag, not a digest of
// the body) is rewritten like any other.
func TestRebind_CompressedResponseWithAnOpaqueETag(t *testing.T) {
	h := newHTTP()
	create, get, _ := rbFlow()
	get.Spec.HTTPResp.Header["Content-Encoding"] = "gzip"
	get.Spec.HTTPResp.Header["Etag"] = `W/"2d-kJ8sYvA1Gq0y3ZbW8xX9nQpLmTo"`
	db := &mockMemDb{mocks: []*models.Mock{create, get}, updateUnFilteredReturn: true}
	createLive(t, h, db)
	assertServes(t, rbMatch(t, h, db, getReq("/items/"+rbLive)), rbLive, rbRec)
}

// The whole response is rewritten, however large: the created item listed
// last in a long list carries the live id too.
func TestRebind_RendersTheWholeResponse(t *testing.T) {
	h := newHTTP()
	create, _, _ := rbFlow()
	var items []string
	for i := 0; i < 400; i++ {
		items = append(items, fmt.Sprintf(`{"id":"%08x-0000-4000-8000-%012x","desc":"%s"}`, i, i, strings.Repeat("d", 200)))
	}
	items = append(items, `{"id":"`+rbRec+`","name":"widget"}`)
	list := rbMock("list", "GET", "http://inv/items", "", 200, "["+strings.Join(items, ",")+"]", 2*time.Second)
	db := &mockMemDb{mocks: []*models.Mock{create, list}, updateUnFilteredReturn: true}
	createLive(t, h, db)
	got := rbMatch(t, h, db, getReq("/items"))
	if len(got.Spec.HTTPResp.Body) < 64<<10 {
		t.Fatal("the list must be larger than 64KB to test anything")
	}
	assertServes(t, got, rbLive, rbRec)
}

// A request that names a recorded entity by its id, and differs from every
// recording elsewhere (a timestamp), fits no creator exactly: the lenient pass
// decides, as it does with rebinding off, and answers from that entity's
// recording.
func TestRebind_ARecordedEntityKeepsItsRecording(t *testing.T) {
	const a, b = "0f8fad5b-d9cb-469f-a165-70867728950e", "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	for _, on := range []bool{true, false} {
		h := newHTTP()
		ma := rbMock("a", "POST", "http://inv/touch", `{"id":"`+a+`","ts":"2026-10-07T12:00:00Z"}`, 200, `{"touched":"a"}`, 0)
		mb := rbMock("b", "POST", "http://inv/touch", `{"id":"`+b+`","ts":"2026-10-07T13:30:45Z"}`, 200, `{"touched":"b"}`, time.Second)
		db := &mockMemDb{mocks: []*models.Mock{ma, mb}, updateUnFilteredReturn: true, noRebind: !on}
		got := rbMatch(t, h, db, jsonReq("POST", "/touch", `{"id":"`+a+`","ts":"2026-10-07T13:30:46Z"}`))
		if got.Name != "a" {
			t.Fatalf("enabled=%v: the request for entity a was answered from %q", on, got.Name)
		}
	}
}

// identicalCreates is two creates recorded with the same payload but for the
// id each minted, under the given URL and with extra recorded-and-replayed
// fields; it returns the names each of two live creates was answered from.
func identicalCreates(t *testing.T, url, extra string, reversedPool bool) (served []string, db *mockMemDb, x1, x2, y1, y2 string) {
	t.Helper()
	h := newHTTP()
	x1, x2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	y1, y2 = rbLive, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	c1 := rbMock("c1", "POST", "http://inv"+url, `{"id":"`+x1+`"`+extra+`}`, 201, `{"seq":1}`, 0)
	c2 := rbMock("c2", "POST", "http://inv"+url, `{"id":"`+x2+`"`+extra+`}`, 201, `{"seq":2}`, time.Second)
	for _, m := range []*models.Mock{c1, c2} {
		m.TestModeInfo.Lifetime = models.LifetimeSession
	}
	pool := []*models.Mock{c1, c2}
	if reversedPool {
		pool = []*models.Mock{c2, c1}
	}
	db = &mockMemDb{mocks: pool, updateUnFilteredReturn: true}
	for _, y := range []string{y1, y2} {
		served = append(served, rbMatch(t, h, db, jsonReq("POST", url, `{"id":"`+y+`"`+extra+`}`)).Name)
	}
	return served, db, x1, x2, y1, y2
}

// A creator is spent once it has answered a live request, whatever became of
// the values it first carries: a random-looking value that is the same on
// record and replay (a tenant id), in the path or in the body, is never bound,
// and must not leave the first recording looking unused for ever.
func TestRebind_IdenticalCreatesWithAConstantID(t *testing.T) {
	const tenant = "5c0d3f2a-7b1e-4a9d-8c6f-0e1d2c3b4a59"
	for name, c := range map[string]struct{ url, extra string }{
		"in the path": {"/tenants/" + tenant + "/items", ""},
		"in the body": {"/items", `,"tenant":"` + tenant + `"`},
	} {
		served, db, x1, x2, y1, y2 := identicalCreates(t, c.url, c.extra, false)
		if strings.Join(served, ",") != "c1,c2" {
			t.Fatalf("%s: served %v, want c1 then c2", name, served)
		}
		if got := db.bound(x1, x2); got[x1] != y1 || got[x2] != y2 {
			t.Fatalf("%s: each create must bind its own recording: %v", name, got)
		}
	}
}

// Creators are claimed in recorded order, whatever order the pool holds them
// in (a match moves the mock it served).
func TestRebind_IdenticalCreatesInAReshuffledPool(t *testing.T) {
	served, db, x1, x2, y1, y2 := identicalCreates(t, "/items", "", true)
	if strings.Join(served, ",") != "c1,c2" {
		t.Fatalf("served %v, want c1 then c2", served)
	}
	if got := db.bound(x1, x2); got[x1] != y1 || got[x2] != y2 {
		t.Fatalf("bindings are cross-wired: %v", got)
	}
}

// Ids in an array line up by position: each create binds both of its own, in
// recorded order, and the second create is answered from the second
// recording. Arrays of one length are walked element by element.
func TestRebind_IdenticalCreatesWithIDsInAnArray(t *testing.T) {
	h := newHTTP()
	ids := func(n int) string {
		return fmt.Sprintf(`["%08x-0000-4000-8000-000000000001","%08x-0000-4000-8000-000000000002"]`, n, n)
	}
	c1 := rbMock("c1", "POST", "http://inv/batch", `{"ids":`+ids(1)+`}`, 201, `{"seq":1}`, 0)
	c2 := rbMock("c2", "POST", "http://inv/batch", `{"ids":`+ids(2)+`}`, 201, `{"seq":2}`, time.Second)
	for _, m := range []*models.Mock{c1, c2} {
		m.TestModeInfo.Lifetime = models.LifetimeSession
	}
	db := &mockMemDb{mocks: []*models.Mock{c1, c2}, updateUnFilteredReturn: true}
	var served []string
	for _, n := range []int{0xa, 0xb} {
		served = append(served, rbMatch(t, h, db, jsonReq("POST", "/batch", `{"ids":`+ids(n)+`}`)).Name)
	}
	if strings.Join(served, ",") != "c1,c2" {
		t.Fatalf("served %v, want c1 then c2", served)
	}
	one := func(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-000000000001", n) }
	if got := db.bound(one(1), one(2)); got[one(1)] != one(0xa) || got[one(2)] != one(0xb) {
		t.Fatalf("each create must bind its own recording's ids: %v", got)
	}
}

// The same holds on the pass that matches a create by its echoed id
// (correlation), which takes the first candidate that fits.
func TestRebind_IdenticalEchoedCreatesInAReshuffledPool(t *testing.T) {
	h := newHTTP()
	x1, x2 := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	c1 := rbMock("c1", "POST", "http://inv/items", `{"id":"`+x1+`","name":"widget"}`, 201, `{"id":"`+x1+`","seq":1}`, 0)
	c2 := rbMock("c2", "POST", "http://inv/items", `{"id":"`+x2+`","name":"widget"}`, 201, `{"id":"`+x2+`","seq":2}`, time.Second)
	for _, m := range []*models.Mock{c1, c2} {
		m.TestModeInfo.Lifetime = models.LifetimeSession
		mocknoise.MaterializeCorrelations(m) // as the agent does when it stages a set
		if len(m.Spec.Correlations) == 0 {
			t.Fatal("the create must echo its id for this test to reach the correlation pass")
		}
	}
	db := &mockMemDb{mocks: []*models.Mock{c2, c1}, updateUnFilteredReturn: true}
	var served []string
	for _, y := range []string{rbLive, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"} {
		got := rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+y+`","name":"widget"}`))
		assertServes(t, got, y, "")
		served = append(served, got.Name)
	}
	if strings.Join(served, ",") != "c1,c2" {
		t.Fatalf("served %v, want c1 then c2", served)
	}
}

// A retried test mints a new id for a recording an earlier attempt already
// bound. A recorded id has one live id for the rest of the test set, so the
// retry is not followed: its create and its read-back are answered exactly as
// they are without rebinding, nothing more is bound, and the first attempt's
// id keeps naming the entity.
//
// Answering each attempt with its own id takes several live ids for one
// recorded id. The replay's own comparison cannot follow that (it is told one
// id per recorded id), and an answer could be rewritten for the wrong attempt.
func TestRebind_ARetryWithANewIDIsAnsweredAsRecorded(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	stage := func(on bool) (*HTTP, *mockMemDb) {
		h := newHTTP()
		post, get, _ := rbFlow()
		for _, m := range []*models.Mock{post, get} {
			m.TestModeInfo.Lifetime = models.LifetimeSession
		}
		return h, &mockMemDb{mocks: []*models.Mock{post, get}, updateUnFilteredReturn: true, noRebind: !on}
	}
	create := func(h *HTTP, db *mockMemDb, y string) *models.Mock {
		return rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+y+`","name":"widget"}`))
	}
	read := func(h *HTTP, db *mockMemDb, y string) *models.Mock { return rbMatch(t, h, db, getReq("/items/"+y)) }

	h, db := stage(true)
	h.Logger = zap.New(core)
	assertServes(t, create(h, db, rbLive), rbLive, rbRec) // first attempt
	assertServes(t, read(h, db, rbLive), rbLive, rbRec)
	retryCreate, retryRead := create(h, db, rbOther), read(h, db, rbOther) // the retry mints its own id

	off, offDB := stage(false)
	for name, c := range map[string][2]*models.Mock{
		"create":    {retryCreate, create(off, offDB, rbOther)},
		"read-back": {retryRead, read(off, offDB, rbOther)},
	} {
		if c[0].Name != c[1].Name || c[0].Spec.HTTPResp.Body != c[1].Spec.HTTPResp.Body {
			t.Fatalf("the retry's %s must be answered as without rebinding:\n on  %s %s\n off %s %s",
				name, c[0].Name, c[0].Spec.HTTPResp.Body, c[1].Name, c[1].Spec.HTTPResp.Body)
		}
	}
	if got := db.bound(rbRec); got[rbRec] != rbLive {
		t.Fatalf("the first attempt's id must stay the entity's: %v", got)
	}
	if rec, ok := db.bindings.Recorded(rbOther); ok {
		t.Fatalf("the retry's id was bound too, for %s", rec)
	}
	assertServes(t, read(h, db, rbLive), rbLive, rbOther) // the first attempt's id still names the entity
	// Each call of the retry was answered from the recording of the entity the
	// first attempt made, and says so — once for each recording.
	if warned := logs.FilterMessageSnippet("names another id").All(); len(warned) != 2 ||
		warned[0].ContextMap()["recorded_id"] != rbRec || warned[0].ContextMap()["this_runs_id"] != rbLive {
		t.Fatalf("the retry's two calls must each be reported, naming the recorded id and the first attempt's: %v", warned)
	}
}

// Two recordings that differ in a long token which is no opaque id — a dated
// key, a pod name, a signed assertion — and a request whose token is one of
// them drifted (a day later, another suffix, a newer timestamp): it is
// answered from the recording it drifted from, exactly as with rebinding off.
func TestRebind_ADriftedTokenKeepsItsRecording(t *testing.T) {
	b64 := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	sig := func(seed int) string {
		const al = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		out := make([]byte, 43)
		for i := range out {
			out[i] = al[(i*7+seed*13+i*i*seed)%len(al)]
		}
		return string(out)
	}
	jwt := func(sub string, iat, seed int) string {
		return `{"assertion":"` + b64(`{"alg":"RS256","typ":"JWT"}`) + "." + b64(fmt.Sprintf(`{"iss":"svc","sub":"%s","iat":%d}`, sub, iat)) + "." + sig(seed) + `"}`
	}
	for name, c := range map[string]struct{ right, wrong, live string }{
		"dated key": {
			`{"key":"daily-sales-eu-2026-10-06","fmt":"pdf"}`,
			`{"key":"daily-sales-us-2026-10-06","fmt":"pdf"}`,
			`{"key":"daily-sales-eu-2026-10-07","fmt":"pdf"}`},
		"signed assertion": {jwt("alice@example.com", 1759750953, 1), jwt("carol@example.com", 1759750953, 2), jwt("alice@example.com", 1759838400, 3)},
		"pod name": {
			`{"pod":"checkout-api-7f9c8d6b5-x2k9p","op":"logs"}`,
			`{"pod":"payments-db-6c4b7a9d8-q7w3e","op":"logs"}`,
			`{"pod":"checkout-api-7f9c8d6b5-m4n8r","op":"logs"}`},
	} {
		for _, withHeaders := range []bool{false, true} {
			for _, on := range []bool{false, true} {
				h := newHTTP()
				wrong := rbMock("wrong", "POST", "http://dep/reports", c.wrong, 200, `{"which":"wrong"}`, 0)
				right := rbMock("right", "POST", "http://dep/reports", c.right, 200, `{"which":"right"}`, time.Second)
				db := &mockMemDb{mocks: []*models.Mock{wrong, right}, updateUnFilteredReturn: true, noRebind: !on}
				in := jsonReq("POST", "/reports", c.live)
				if withHeaders { // as the agent hands it over: raw is the whole request
					const trace = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
					in.header.Set("Traceparent", trace)
					in.raw = []byte("POST /reports HTTP/1.1\r\nHost: dep\r\nContent-Type: application/json\r\nTraceparent: " + trace + "\r\n\r\n" + c.live)
				}
				if got := rbMatch(t, h, db, in).Name; got != "right" {
					t.Errorf("%s (headers=%v, enabled=%v): answered from %q", name, withHeaders, on, got)
				}
			}
		}
	}
}

// A live value the recording itself carries was not minted this run: it names
// a recorded entity. A diverged request that sends it where a creator's id
// sits must not bind it — as a first value or as an alias — or a copy of
// another entity's recording would answer for it.
func TestRebind_ARecordedIDIsNeverBoundAsLive(t *testing.T) {
	const a, b = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	for name, createFirst := range map[string]bool{"creator unclaimed": false, "creator already bound": true} {
		{
			h := newHTTP()
			createB := rbMock("createB", "POST", "http://inv/items", `{"id":"`+b+`","name":"widget"}`, 201, `{"id":"`+b+`"}`, 0)
			getB := rbMock("getB", "GET", "http://inv/items/"+b, "", 200, `{"id":"`+b+`","name":"beta"}`, time.Second)
			getA := rbMock("getA", "GET", "http://inv/items/"+a, "", 200, `{"id":"`+a+`","name":"alpha"}`, 2*time.Second)
			for _, m := range []*models.Mock{createB, getB, getA} {
				m.TestModeInfo.Lifetime = models.LifetimeSession
			}
			db := &mockMemDb{mocks: []*models.Mock{createB, getB, getA}, updateUnFilteredReturn: true}
			if createFirst {
				rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`))
			}
			rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+a+`","name":"widget"}`)) // an upsert of A the recording never had
			if rec, ok := db.bindings.Recorded(a); ok {
				t.Fatalf("%s: recorded id A was bound as a live value for %s", name, rec)
			}
			got := rbMatch(t, h, db, getReq("/items/"+a))
			if got.Name != "getA" || !strings.Contains(got.Spec.HTTPResp.Body, "alpha") {
				t.Fatalf("%s: the read of entity A was answered from %s: %s", name, got.Name, got.Spec.HTTPResp.Body)
			}
		}
	}
}

// rotating is a store that, like the agent's, moves the mock a match served
// to the back of the pool.
type rotating struct{ *mockMemDb }

func (r *rotating) UpdateUnFilteredMock(old, updated *models.Mock) bool {
	for i, m := range r.mocks {
		if m.Name == old.Name {
			r.mocks = append(append(r.mocks[:i:i], r.mocks[i+1:]...), m)
			break
		}
	}
	return r.mockMemDb.UpdateUnFilteredMock(old, updated)
}

// Once every creator has answered a request, the pool's own rotation decides
// again: three recordings of one call, nine live calls, each recording three
// times — not the first one for ever after.
func TestRebind_ClaimedCreatorsKeepThePoolsRotation(t *testing.T) {
	id := func(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", n*7919, n*104729) }
	for _, on := range []bool{false, true} {
		h := newHTTP()
		var pool []*models.Mock
		for i := 1; i <= 3; i++ {
			m := rbMock(fmt.Sprintf("m%d", i), "GET", "http://dep/jobs/"+id(i), "", 200, fmt.Sprintf(`{"n":%d}`, i), time.Duration(i)*time.Second)
			m.TestModeInfo.Lifetime = models.LifetimeSession
			pool = append(pool, m)
		}
		db := &rotating{&mockMemDb{mocks: pool, updateUnFilteredReturn: true, noRebind: !on}}
		var served []string
		for i := 10; i < 19; i++ {
			ok, stub, _, err := h.match(context.Background(), getReq("/jobs/"+id(i)), db, nil, nil, nil, true, false, false, true, true)
			if err != nil || !ok {
				t.Fatalf("no match: ok=%v err=%v", ok, err)
			}
			served = append(served, stub.Name)
		}
		if got := strings.Join(served, ","); got != "m1,m2,m3,m1,m2,m3,m1,m2,m3" {
			t.Fatalf("enabled=%v: served %s", on, got)
		}
	}
}

// A request fits a creator's template only when it is the recorded request
// but for the ids that creator introduced.
func TestRebind_TemplateMatchIsExactButForTheCreatorsIDs(t *testing.T) {
	create, get, _ := rbFlow() // create: {"id": rbRec, "name": "widget"}
	other := rbMock("other", "POST", "http://inv/other", `{"id":"`+rbOther+`"}`, 200, `{}`, -time.Second)
	// A store for each request: what one request tells the replay (an id sent
	// as recorded is not made anew) must not decide the next row.
	store := func() *mockMemDb {
		return &mockMemDb{mocks: []*models.Mock{other, create, get}, updateUnFilteredReturn: true}
	}
	fits := func(body string) bool {
		r := rbOf(store(), jsonReq("POST", "/items", body))
		tm, _ := r.templateMatch([]*models.Mock{create})
		orig, _ := r.original(tm)
		return tm != nil && orig == create
	}
	for body, want := range map[string]bool{
		`{"id":"` + rbLive + `","name":"widget"}`:                   true,  // the id minted this run
		`{"name":"widget","id":"` + rbLive + `"}`:                   true,  // key order is not shape
		`{"id":"` + rbLive + `","name":"gadget"}`:                   false, // another field differs
		`{"id":"` + rbLive + `","name":"widget","extra":1}`:         false, // a field the recording lacks
		`{"id":"` + rbLive + `"}`:                                   false, // a field missing
		`{"id":"7c9e66797425","name":"widget"}`:                     false, // not an id of that kind
		`{"id":"7c9e667974254de944be07fc1f90ae77","name":"widget"}`: false, // another class and length
		`{"id":"` + rbOther + `","name":"widget"}`:                  false, // an id the recording carries: not minted now
		`{"id":"` + rbRec + `","name":"widget"}`:                    false, // the recorded request itself: the exact pass's
		`[{"id":"` + rbLive + `","name":"widget"}]`:                 false,
		`not json`: false,
	} {
		if got := fits(body); got != want {
			t.Errorf("template fit of %s = %v, want %v", body, got, want)
		}
	}
	// A mock that introduced no id has no template.
	in := jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`)
	if m, _ := rbOf(store(), in).templateMatch([]*models.Mock{get}); m != nil {
		t.Fatalf("a mock that is no creator matched as a template: %s", m.Name)
	}
}

// A set is rebound whole or not at all: with a mock of a kind that cannot be
// rebound in it (the id is probably INSERTed there too), nothing is bound and
// every answer is as recorded — exactly as with rebinding off.
func TestRebind_ASetWithAnUnrebindableKindIsLeftAlone(t *testing.T) {
	h := newHTTP()
	create, get, _ := rbFlow()
	sql := &models.Mock{Name: "insert", Kind: models.Postgres, Spec: models.MockSpec{ReqTimestampMock: rbBase.Add(time.Millisecond)}}
	db := &mockMemDb{mocks: []*models.Mock{create, get}, otherKinds: []*models.Mock{sql}, updateUnFilteredReturn: true}
	if rbOf(db, jsonReq("POST", "/items", `{}`)) != nil {
		t.Fatal("a set holding a Postgres mock must not be rebound")
	}
	assertServes(t, createLive(t, h, db), rbRec, rbLive)
	if len(db.bound(rbRec)) != 0 {
		t.Fatal("nothing may be bound in a set that is not rebound")
	}
	assertServes(t, rbMatch(t, h, db, getReq("/items/"+rbRec)), rbRec, rbLive)

	dns := &models.Mock{Name: "lookup", Kind: models.DNS, Spec: models.MockSpec{ReqTimestampMock: rbBase}}
	withDNS := &mockMemDb{mocks: []*models.Mock{create, get}, otherKinds: []*models.Mock{dns}, updateUnFilteredReturn: true}
	if rbOf(withDNS, jsonReq("POST", "/items", `{}`)) == nil {
		t.Fatal("a DNS mock carries no id: the set is still rebound")
	}
}

// A replay that names the recorded values it may bind (keploy test: those in
// its template map) binds no other: a value that merely differs this
// run — a content hash a regression changed — is matched leniently as it
// always was, answered as recorded, and fails the test that checks it.
func TestRebind_OnlyDeclaredValuesAreBound(t *testing.T) {
	for name, c := range map[string]struct {
		only      map[string]bool
		wantBound bool
	}{
		"the id is declared":       {map[string]bool{rbRec: true}, true},
		"another value is":         {map[string]bool{rbOther: true}, false},
		"every minted id is (nil)": {nil, true},
	} {
		h := newHTTP()
		create, get, _ := rbFlow()
		db := &mockMemDb{mocks: []*models.Mock{create, get}, updateUnFilteredReturn: true, only: c.only}
		served := createLive(t, h, db)
		if got := len(db.bound(rbRec)) == 1; got != c.wantBound {
			t.Fatalf("%s: bound=%v, want %v", name, got, c.wantBound)
		}
		if !c.wantBound {
			assertServes(t, served, rbRec, rbLive)
		}
	}
}

// A field marked as request-body noise — learned on the mock, or configured —
// may differ and the request still fits its creator: two creates that stamp
// the time into their payload bind in recorded order, not by which random id
// or timestamp looks more alike.
func TestRebind_TemplateMatchIgnoresNoiseFields(t *testing.T) {
	const x1, x2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	// A noise field at the root and one inside an array, as a real payload has.
	body := func(id, at string) string {
		return `{"id":"` + id + `","name":"widget","createdAt":"` + at + `","lines":[{"sku":"a","stampedAt":"` + at + `"},{"sku":"b","stampedAt":"` + at + `"}]}`
	}
	for name, c := range map[string]struct {
		learned, configured map[string][]string
	}{
		// As the noise engine stores what it learned on a mock (Detect).
		"learned on the mock": {learned: map[string][]string{"body.createdAt": {}, "body.lines[].stampedAt": {}}},
		// As the proxy hands over test.globalNoise.requestBody: lowercased.
		"configured": {configured: map[string][]string{"createdat": {}, "lines[].stampedat": {}}},
	} {
		h := newHTTP()
		c1 := rbMock("c1", "POST", "http://inv/items", body(x1, "2026-10-07T12:00:00Z"), 201, `{"seq":1}`, 0)
		c2 := rbMock("c2", "POST", "http://inv/items", body(x2, "2026-10-07T12:00:01Z"), 201, `{"seq":2}`, time.Second)
		for _, m := range []*models.Mock{c1, c2} {
			m.TestModeInfo.Lifetime = models.LifetimeSession
			m.Spec.ReqBodyNoise = c.learned
		}
		db := &mockMemDb{mocks: []*models.Mock{c2, c1}, updateUnFilteredReturn: true}
		// y1 is 18 edits from x2 and 30 from x1; its timestamp is nearer c2's too.
		y1, y2 := "22222222-2222-4abc-9def-0123456789ab", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		var served []string
		for _, y := range []string{y1, y2} {
			ok, stub, _, err := h.match(context.Background(), jsonReq("POST", "/items", body(y, "2026-11-30T23:59:59Z")), db, nil, c.configured, nil, true, false, false, true, true)
			if err != nil || !ok {
				t.Fatalf("%s: no match: ok=%v err=%v", name, ok, err)
			}
			served = append(served, stub.Name)
		}
		if strings.Join(served, ",") != "c1,c2" {
			t.Fatalf("%s: served %v, want c1 then c2", name, served)
		}
		if got := db.bound(x1, x2); got[x1] != y1 || got[x2] != y2 {
			t.Fatalf("%s: bindings are cross-wired: %v", name, got)
		}
	}
}

// The fit is exact inside arrays too: a request whose list differs from a
// creator's is not that creator's request, whatever its last element is.
func TestRebind_TemplateMatchComparesEveryArrayElement(t *testing.T) {
	const x1, x2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	body := func(id, first string) string { return `{"id":"` + id + `","tags":["` + first + `","shared"]}` }
	c1 := rbMock("c1", "POST", "http://inv/items", body(x1, "red"), 201, `{"seq":1}`, 0)
	c2 := rbMock("c2", "POST", "http://inv/items", body(x2, "blue"), 201, `{"seq":2}`, time.Second)
	db := &mockMemDb{mocks: []*models.Mock{c1, c2}, updateUnFilteredReturn: true}
	in := jsonReq("POST", "/items", body(rbLive, "blue"))
	if m, _ := rbOf(db, in).templateMatch([]*models.Mock{c1, c2}); m == nil || m.Name != "c2" {
		t.Fatalf("fit %v, want c2: the request's list is c2's", m)
	}
	in = jsonReq("POST", "/items", `{"id":"`+rbLive+`","tags":["blue","shared","more"]}`)
	if m, _ := rbOf(db, in).templateMatch([]*models.Mock{c1, c2}); m != nil {
		t.Fatalf("fit %s: a longer list is neither creator's request", m.Name)
	}
}

// Ids are replaced as whole words: one that is part of a longer token is
// another word, and is left as recorded — in an answer, and when deciding
// whether a request names a bound id.
func TestRebind_OnlyWholeWordsAreReplaced(t *testing.T) {
	h := newHTTP()
	create, _, _ := rbFlow()
	refs := rbMock("refs", "GET", "http://inv/refs", "", 200, `{"ref":"order-`+rbRec+`","id":"`+rbRec+`"}`, time.Second)
	db := &mockMemDb{mocks: []*models.Mock{create, refs}, updateUnFilteredReturn: true}
	createLive(t, h, db)
	got := rbMatch(t, h, db, getReq("/refs")).Spec.HTTPResp.Body
	if want := `{"ref":"order-` + rbRec + `","id":"` + rbLive + `"}`; got != want {
		t.Fatalf("served %s, want %s", got, want)
	}
	r := rbOf(db, getReq("/refs/order-"+rbLive))
	if len(r.hits) != 0 {
		t.Fatalf("a longer token is not the bound id: hits %v", r.hits)
	}
}

// A noise path names one field. Two creates that differ in a field whose name
// merely contains it ("provider" holds "id") are different requests, and each
// live create is answered from its own recording.
func TestRebind_TemplateMatchReadsANoisePathAsThatFieldOnly(t *testing.T) {
	const x1, x2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	body := func(id, provider string) string { return `{"id":"` + id + `","provider":"` + provider + `"}` }
	h := newHTTP()
	aws := rbMock("aws", "POST", "http://inv/items", body(x1, "aws"), 201, `{"p":"aws"}`, 0)
	gcp := rbMock("gcp", "POST", "http://inv/items", body(x2, "gcp"), 201, `{"p":"gcp"}`, time.Second)
	for _, m := range []*models.Mock{aws, gcp} {
		m.TestModeInfo.Lifetime = models.LifetimeSession
		m.Spec.ReqBodyNoise = map[string][]string{"body.id": {}} // as an earlier run learned it
	}
	db := &mockMemDb{mocks: []*models.Mock{aws, gcp}, updateUnFilteredReturn: true}
	if stub := rbMatch(t, h, db, jsonReq("POST", "/items", body(rbLive, "gcp"))); stub.Name != "gcp" {
		t.Fatalf("a create for gcp was answered from %q", stub.Name)
	}
	if got := db.bound(x1, x2); got[x2] != rbLive || got[x1] != "" {
		t.Fatalf("the gcp create's id must be bound, and only it: %v", got)
	}
}

// learning is a store that keeps, per mock, the request-body noise matches
// recorded on it: what a detection run leaves in the mocks file.
type learning struct {
	*mockMemDb
	noise map[string]map[string][]string
}

func (l *learning) UpdateUnFilteredMock(old, updated *models.Mock) bool {
	if len(updated.Spec.ReqBodyNoise) > 0 {
		l.noise[updated.Name] = updated.Spec.ReqBodyNoise
	}
	return l.mockMemDb.UpdateUnFilteredMock(old, updated)
}

// detect replays reqs over mocks with noise detection on, and returns who
// answered each and the noise the run learned, per mock.
func detect(t *testing.T, on bool, mocks []*models.Mock, reqs ...*req) (served []string, db *learning) {
	t.Helper()
	h := newHTTP()
	db = &learning{&mockMemDb{mocks: mocks, updateUnFilteredReturn: true, noRebind: !on}, map[string]map[string][]string{}}
	for _, in := range reqs {
		ok, stub, _, err := h.match(context.Background(), in, db, nil, nil, nil, true, true, false, true, true)
		if err != nil || !ok {
			t.Fatalf("rebinding=%v: %s %s must match: ok=%v err=%v", on, in.method, in.url.Path, ok, err)
		}
		served = append(served, stub.Name)
	}
	return served, db
}

// Request-body noise is learned exactly as it is without rebinding, on every
// pass: the recorded body against the body as the app sent it. What a
// detection run writes into the mocks is read by replays that follow no id —
// a strict run, an older keploy, another consumer of the same mocks, the same
// set once it holds a database mock — and they must find the id's field
// marked, or miss the call.
//
// Keeping a followed id out of the learned noise ("an id that is followed is
// not a field that drifts") would be true of the run that followed it, and a
// mock miss for every run that did not.
func TestRebind_NoiseIsLearnedAsWithoutRebinding(t *testing.T) {
	const idem, idem2 = "5c0d3f2a-7b1e-4a9d-8c6f-0e1d2c3b4a59", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	create := func() *models.Mock {
		return rbMock("create", "POST", "http://inv/items", `{"id":"`+rbRec+`","name":"widget"}`, 201, `{"ok":true}`, 0)
	}
	createLive := jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`)
	for name, c := range map[string]struct {
		mocks     func() []*models.Mock
		reqs      []*req
		want      map[string][]string // mock -> the noise paths it must have learned
		wantBound bool
	}{
		"a create that differs in its id alone (matched by its shape)": {
			func() []*models.Mock { return []*models.Mock{create()} },
			[]*req{createLive},
			map[string][]string{"create": {"body.id"}}, true,
		},
		"a read-back by a body field, answered through a copy on the exact pass": {
			func() []*models.Mock {
				return []*models.Mock{create(), rbMock("reserve", "POST", "http://inv/reserve", `{"itemId":"`+rbRec+`","qty":1}`, 200, `{"reserved":1}`, time.Second)}
			},
			[]*req{createLive, jsonReq("POST", "/reserve", `{"itemId":"`+rbLive+`","qty":1}`)},
			map[string][]string{"create": {"body.id"}, "reserve": {"body.itemId"}}, true,
		},
		"a correlated call, answered through a copy on the correlation pass": {
			func() []*models.Mock {
				pay := rbMock("pay", "POST", "http://pay/charge", `{"itemId":"`+rbRec+`","idem":"`+idem+`"}`, 200, `{"idem":"`+idem+`"}`, time.Second)
				mocknoise.MaterializeCorrelations(pay) // as the agent does when it stages a set
				return []*models.Mock{create(), pay}
			},
			[]*req{createLive, jsonReq("POST", "/charge", `{"itemId":"`+rbLive+`","idem":"`+idem2+`"}`)},
			map[string][]string{"create": {"body.id"}, "pay": {"body.idem", "body.itemId"}}, true,
		},
		// Matched on the correlation pass itself, as without rebinding, a call
		// has nothing to learn: the value that differs is the one the answer
		// echoes, and that pass follows it by itself.
		"a correlated call, matched on the correlation pass": {
			func() []*models.Mock {
				pay := rbMock("pay", "POST", "http://pay/charge", `{"amount":5,"idem":"`+idem+`"}`, 200, `{"idem":"`+idem+`"}`, 0)
				mocknoise.MaterializeCorrelations(pay)
				return []*models.Mock{pay}
			},
			[]*req{jsonReq("POST", "/charge", `{"amount":5,"idem":"`+idem2+`"}`)},
			map[string][]string{"pay": nil}, false,
		},
		"a field that holds the id inside a longer string": {
			func() []*models.Mock {
				return []*models.Mock{rbMock("create", "POST", "http://inv/items", `{"id":"`+rbRec+`","self":"/items/`+rbRec+`"}`, 201, `{"ok":true}`, 0)}
			},
			[]*req{jsonReq("POST", "/items", `{"id":"`+rbLive+`","self":"/items/`+rbLive+`"}`)},
			map[string][]string{"create": {"body.id", "body.self"}}, true,
		},
		// Another field drifts too and is not noise yet: this is no recording's
		// request, ids aside, so the lenient pass answers and nothing is bound.
		"a create with a field that drifts (matched leniently)": {
			func() []*models.Mock {
				return []*models.Mock{rbMock("create", "POST", "http://inv/items", `{"id":"`+rbRec+`","at":"2026-10-07T12:00:00Z"}`, 201, `{"ok":true}`, 0)}
			},
			[]*req{jsonReq("POST", "/items", `{"id":"`+rbLive+`","at":"2026-11-30T23:59:59Z"}`)},
			map[string][]string{"create": {"body.at", "body.id"}}, false,
		},
		"the same when the lenient pass has to choose between two recordings": {
			func() []*models.Mock {
				body := func(id, name, at string) string { return `{"id":"` + id + `","name":"` + name + `","at":"` + at + `"}` }
				return []*models.Mock{
					rbMock("create", "POST", "http://inv/items", body(rbRec, "widget", "2026-10-07T12:00:00Z"), 201, `{"n":1}`, 0),
					rbMock("c2", "POST", "http://inv/items", body("22222222-2222-4222-8222-222222222222", "gadget", "2026-10-07T12:00:01Z"), 201, `{"n":2}`, time.Second),
				}
			},
			[]*req{jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget","at":"2026-11-30T23:59:59Z"}`)},
			map[string][]string{"create": {"body.at", "body.id"}}, false,
		},
	} {
		servedOn, on := detect(t, true, c.mocks(), c.reqs...)
		servedOff, off := detect(t, false, c.mocks(), c.reqs...)
		if !reflect.DeepEqual(servedOn, servedOff) {
			t.Errorf("%s: answered by %v with rebinding, %v without", name, servedOn, servedOff)
		}
		if !reflect.DeepEqual(on.noise, off.noise) {
			t.Errorf("%s: a detection run must learn the same noise with rebinding as without:\n with    %v\n without %v", name, on.noise, off.noise)
		}
		for mock, paths := range c.want {
			for _, p := range paths {
				if _, ok := on.noise[mock][p]; !ok {
					t.Errorf("%s: %s must be learned on %s: %v", name, p, mock, on.noise[mock])
				}
			}
			if len(on.noise[mock]) != len(paths) {
				t.Errorf("%s: %s learned %v, want exactly %v", name, mock, on.noise[mock], paths)
			}
		}
		if got := on.bound(rbRec)[rbRec] == rbLive; got != c.wantBound {
			t.Errorf("%s: bound=%v, want %v", name, got, c.wantBound)
		}
	}
}

// A detection run, then a strict run in which the app mints another id: the
// strict run finds the id's field learned as noise and serves every call —
// with rebinding (which stays out of a strict run altogether) and for a
// consumer that never follows an id — and it keeps its guarantee: a field
// that is not noise still fails. The verdicts are those of a detection run
// made without rebinding.
func TestRebind_ADetectionRunThenAStrictRun(t *testing.T) {
	const y2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	mocks := func(learned map[string]map[string][]string) []*models.Mock {
		ms := []*models.Mock{
			rbMock("create", "POST", "http://inv/items", `{"id":"`+rbRec+`","name":"widget"}`, 201, `{"ok":true}`, 0),
			rbMock("reserve", "POST", "http://inv/reserve", `{"itemId":"`+rbRec+`","qty":1}`, 200, `{"reserved":1}`, time.Second),
		}
		for _, m := range ms {
			m.Spec.ReqBodyNoise = learned[m.Name] // what the detection run persisted
		}
		return ms
	}
	flow := func(id, name string) []*req {
		return []*req{
			jsonReq("POST", "/items", `{"id":"`+id+`","name":"`+name+`"}`),
			jsonReq("POST", "/reserve", `{"itemId":"`+id+`","qty":1}`),
		}
	}
	strict := func(detectedWith, strictWith bool, name string) (verdicts []string) {
		_, detection := detect(t, detectedWith, mocks(nil), flow(rbLive, "widget")...)
		h := newHTTP()
		db := &mockMemDb{mocks: mocks(detection.noise), updateUnFilteredReturn: true, noRebind: !strictWith}
		for _, in := range flow(y2, name) {
			ok, stub, diag, err := h.match(context.Background(), in, db, nil, nil, nil, true, false, true, true, true)
			switch {
			case err != nil:
				t.Fatal(err)
			case ok:
				verdicts = append(verdicts, stub.Name+" "+stub.Spec.HTTPResp.Body)
			default:
				verdicts = append(verdicts, "miss at "+diag.phase)
			}
		}
		if len(db.bound(rbRec)) != 0 {
			t.Fatal("a strict run binds nothing")
		}
		return verdicts
	}
	main := strict(false, false, "widget")
	if want := []string{`create {"ok":true}`, `reserve {"reserved":1}`}; !reflect.DeepEqual(main, want) {
		t.Fatalf("without rebinding, detect then strict serves %v, want %v", main, want)
	}
	for _, c := range [][2]bool{{true, true}, {true, false}, {false, true}} {
		if got := strict(c[0], c[1], "widget"); !reflect.DeepEqual(got, main) {
			t.Errorf("detected with rebinding=%v, strict with rebinding=%v: %v, want what a run without any gives: %v", c[0], c[1], got, main)
		}
	}
	// Strict stays strict: a create with another name is no recorded call.
	mainOther := strict(false, false, "gadget")
	if mainOther[0] != "miss at "+models.MatchPhaseStrict {
		t.Fatalf("a field that is not noise must fail a strict run: %v", mainOther)
	}
	if got := strict(true, true, "gadget"); !reflect.DeepEqual(got, mainOther) {
		t.Errorf("with rebinding a strict run gives %v, want %v", got, mainOther)
	}
}

// A request is bound whole or not at all: every id its creator introduced
// lines up — objects by key, arrays by position — or none is bound, so an
// answer never names one operation's entities by ids from two runs.
func TestRebind_ARequestIsBoundWholeOrNotAtAll(t *testing.T) {
	const x1, x2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const y1, y2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "bbbbbbbb-cccc-4ddd-9eee-ffffffffffff"
	recorded := `{"primary":"` + x1 + `","all":["` + x1 + `","` + x2 + `"]}`
	answer := `{"created":["` + x1 + `","` + x2 + `"]}`
	for name, c := range map[string]struct {
		live      string
		wantBound map[string]string
		wantBody  string
	}{
		"both ids made anew": {
			`{"primary":"` + y1 + `","all":["` + y1 + `","` + y2 + `"]}`,
			map[string]string{x1: y1, x2: y2}, `{"created":["` + y1 + `","` + y2 + `"]}`,
		},
		"a list of another length": {
			`{"primary":"` + y1 + `","all":["` + y1 + `"]}`, map[string]string{}, answer,
		},
		"an id as recorded in one place and new in another": {
			`{"primary":"` + x1 + `","all":["` + y1 + `","` + y2 + `"]}`, map[string]string{}, answer,
		},
		"one new id for two recorded ones": {
			`{"primary":"` + y1 + `","all":["` + y1 + `","` + y1 + `"]}`, map[string]string{}, answer,
		},
		"something that is no id in an id's place": {
			`{"primary":"` + y1 + `","all":["` + y1 + `","none"]}`, map[string]string{}, answer,
		},
		"an id made anew in one place and missing from another": {
			`{"primary":"` + y1 + `","all":[{"id":"` + y1 + `"},"` + y2 + `"]}`, map[string]string{}, answer,
		},
	} {
		h := newHTTP()
		create := rbMock("create", "POST", "http://inv/batch", recorded, 201, answer, 0)
		db := &mockMemDb{mocks: []*models.Mock{create}, updateUnFilteredReturn: true}
		stub := rbMatch(t, h, db, jsonReq("POST", "/batch", c.live))
		if got := db.bound(x1, x2); !mapsEqual(got, c.wantBound) {
			t.Errorf("%s: bound %v, want %v", name, got, c.wantBound)
		}
		if stub.Spec.HTTPResp.Body != c.wantBody {
			t.Errorf("%s: answered %s, want %s", name, stub.Spec.HTTPResp.Body, c.wantBody)
		}
	}

	// A batch of two binds both, and each is read back from its own recording.
	h := newHTTP()
	batch := rbMock("batch", "POST", "http://inv/items", `[{"id":"`+x1+`"},{"id":"`+x2+`"}]`, 201, `{"n":2}`, 0)
	g1 := rbMock("g1", "GET", "http://inv/items/"+x1, "", 200, `{"n":1}`, time.Second)
	g2 := rbMock("g2", "GET", "http://inv/items/"+x2, "", 200, `{"n":2}`, 2*time.Second)
	db := &mockMemDb{mocks: []*models.Mock{batch, g1, g2}, updateUnFilteredReturn: true}
	rbMatch(t, h, db, jsonReq("POST", "/items", `[{"id":"`+y1+`"},{"id":"`+y2+`"}]`))
	if got := db.bound(x1, x2); got[x1] != y1 || got[x2] != y2 {
		t.Fatalf("a batch of two must bind both: %v", got)
	}
	if stub := rbMatch(t, h, db, getReq("/items/"+y2)); stub.Name != "g2" {
		t.Fatalf("the second item's read-back was answered from %q", stub.Name)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Only a generated UUID is ever taken for an id the app mints. A model name,
// a queue, a digest, a name-based UUID that differs is the app sending
// something else: the recording answers as recorded and nothing is bound —
// whether the replay follows what the app mints (keploy mock replay) or names
// the value (keploy test, from its templates: being named says the value
// flows from an answer into a later request, not that it changes every run).
func TestRebind_OnlyAGeneratedUUIDIsEverFollowed(t *testing.T) {
	for name, c := range map[string]struct{ rec, live string }{
		"a model name":      {"gpt-4o-mini-2024-07-18", "gpt-4o-mini-2025-01-31"},
		"a queue":           {"orders-processing-queue-1", "orders-processing-queue-2"},
		"a digest":          {"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"},
		"a name-based UUID": {"2ed6657d-e927-568b-95e1-2665a8aea6a2", "886313e1-3b8a-5372-9b90-0c9aee199e5d"},
		"a nanoid":          {"aB3dEfGhIjKlMnOpQrSt9", "Zx9yWvUtSrQpOnMlKjIh4"},
	} {
		for _, named := range []bool{false, true} {
			h := newHTTP()
			call := rbMock("call", "POST", "http://dep/call", `{"v":"`+c.rec+`","q":"x"}`, 200, `{"v":"`+c.rec+`"}`, 0)
			db := &mockMemDb{mocks: []*models.Mock{call}, updateUnFilteredReturn: true}
			if named {
				db.only = map[string]bool{c.rec: true}
			}
			stub := rbMatch(t, h, db, jsonReq("POST", "/call", `{"v":"`+c.live+`","q":"x"}`))
			if got := db.bound(c.rec); len(got) != 0 {
				t.Errorf("%s (named=%v): bound %v", name, named, got)
			}
			if !strings.Contains(stub.Spec.HTTPResp.Body, c.rec) {
				t.Errorf("%s (named=%v): answered %s", name, named, stub.Spec.HTTPResp.Body)
			}
		}
	}
	// A generated UUID is followed, and when ids are named, only if named.
	for name, c := range map[string]struct {
		only      map[string]bool
		wantBound bool
	}{
		"what the app mints": {nil, true},
		"named":              {map[string]bool{rbRec: true}, true},
		"another is named":   {map[string]bool{rbOther: true}, false},
	} {
		h := newHTTP()
		create, _, _ := rbFlow()
		db := &mockMemDb{mocks: []*models.Mock{create}, updateUnFilteredReturn: true, only: c.only}
		createLive(t, h, db)
		if got := db.bound(rbRec)[rbRec] == rbLive; got != c.wantBound {
			t.Errorf("%s: bound=%v, want %v", name, got, c.wantBound)
		}
	}
}

// fits is the one gate: the live request, with this run's ids put back, must
// be the recorded request as the exact passes read one. Each row is one way a
// request is, or is not, a recording's own.
func TestRebind_Fits(t *testing.T) {
	const tenant = "5c0d3f2a-7b1e-4a9d-8c6f-0e1d2c3b4a59"
	create, get, list := rbFlow()
	create.Spec.HTTPReq.Header["Authorization"] = "Bearer t"
	find := rbMock("find", "GET", "http://inv/find?id="+rbRec+"&page=2", "", 200, `{"id":"`+rbRec+`"}`, 3*time.Second)
	find.Spec.HTTPReq.URLParams = map[string]string{"id": rbRec, "page": "2"}
	form := rbMock("form", "POST", "http://inv/move", "from="+rbRec+"&n=1", 200, `{}`, 4*time.Second)
	form.Spec.HTTPReq.Header["Content-Type"] = "application/x-www-form-urlencoded"
	noised := rbMock("noised", "POST", "http://inv/touch", `{"id":"`+rbRec+`","at":"2026-10-07T12:00:00Z"}`, 200, `{}`, 5*time.Second)
	noised.Spec.ReqBodyNoise = map[string][]string{"body.at": {}, "body.id": {}}
	other := rbMock("other", "POST", "http://inv/others", `{"id":"`+rbOther+`","tenant":"`+tenant+`"}`, 201, `{}`, 6*time.Second)
	const third = "33333333-3333-4333-8333-333333333333"
	noisedCreate := rbMock("noised-create", "POST", "http://inv/things", `{"id":"`+third+`","name":"thing"}`, 201, `{}`, 7*time.Second)
	noisedCreate.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
	// A store for each row: what one request tells the replay (an id sent as
	// recorded is not made anew) must not decide the next row.
	bound := map[string]string{}
	store := func() *mockMemDb {
		db := &mockMemDb{mocks: []*models.Mock{create, get, list, find, form, noised, other, noisedCreate}, updateUnFilteredReturn: true}
		if len(bound) > 0 && !db.bindings.ClaimAndBind("", bound) {
			t.Fatal("bind")
		}
		return db
	}

	formReq := func(body string) *req {
		in := jsonReq("POST", "/move", body)
		in.header.Set("Content-Type", "application/x-www-form-urlencoded")
		return in
	}
	withQuery := func(q string) *req {
		in := getReq("/find")
		in.url.RawQuery = q
		return in
	}
	authed := func(in *req) *req {
		in.header.Set("Authorization", "Bearer other")
		return in
	}
	type row struct {
		m    *models.Mock
		in   *req
		want bool
	}
	check := func(when string, rows map[string]row) {
		t.Helper()
		for name, c := range rows {
			if got := rbOf(store(), c.in).fits(c.m); got != c.want {
				t.Errorf("%s, %s: fits %s = %v, want %v", when, name, c.m.Name, got, c.want)
			}
		}
	}
	const y2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	check("nothing bound", map[string]row{
		"the create with the id made this run":       {create, authed(jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`)), true},
		"its keys in another order":                  {create, authed(jsonReq("POST", "/items", `{"name":"widget","id":"`+rbLive+`"}`)), true},
		"the recorded request itself":                {create, authed(jsonReq("POST", "/items", `{"id":"`+rbRec+`","name":"widget"}`)), true},
		"another field differs":                      {create, authed(jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"gadget"}`)), false},
		"another method":                             {create, authed(jsonReq("PUT", "/items", `{"id":"`+rbLive+`","name":"widget"}`)), false},
		"another path":                               {create, authed(jsonReq("POST", "/item", `{"id":"`+rbLive+`","name":"widget"}`)), false},
		"a recorded header the request lacks":        {create, jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`), false},
		"a creator that does not line up":            {create, authed(jsonReq("POST", "/items", `{"id":"not-an-id","name":"widget"}`)), false},
		"a read-back of an id nothing bound":         {get, getReq("/items/" + rbLive), false},
		"a request that names no id":                 {list, getReq("/items"), true},
		"a creator's id beside a constant one":       {other, jsonReq("POST", "/others", `{"id":"`+rbLive+`","tenant":"`+tenant+`"}`), true},
		"the constant one made anew too: both or no": {other, jsonReq("POST", "/others", `{"id":"`+rbLive+`","tenant":"`+y2+`"}`), true},
		// Noise lets a field differ; it does not excuse a creator from lining
		// up. What stands in its id's place must be an id of that kind.
		"a creator whose id is noise, an id anew":    {noisedCreate, jsonReq("POST", "/things", `{"id":"`+rbLive+`","name":"thing"}`), true},
		"a creator whose id is noise, no id there":   {noisedCreate, jsonReq("POST", "/things", `{"id":"n/a","name":"thing"}`), false},
		"a creator whose id is noise, another's id":  {noisedCreate, jsonReq("POST", "/things", `{"id":"`+rbOther+`","name":"thing"}`), false},
		"a creator whose id is noise, the id absent": {noisedCreate, jsonReq("POST", "/things", `{"name":"thing"}`), false},
	})

	bound[rbRec] = rbLive
	check("the id bound", map[string]row{
		"a read-back by this run's id":                {get, getReq("/items/" + rbLive), true},
		"a read-back by the recorded id":              {get, getReq("/items/" + rbRec), false},
		"a read-back by an id nobody made":            {get, getReq("/items/" + rbOther), false},
		"a query value":                               {find, withQuery("id=" + rbLive + "&page=2"), true},
		"a query value, another page":                 {find, withQuery("id=" + rbLive + "&page=3"), false},
		"a query value, a key more":                   {find, withQuery("id=" + rbLive + "&page=2&x=1"), false},
		"a form body":                                 {form, formReq("from=" + rbLive + "&n=1"), true},
		"a form body, another field differs":          {form, formReq("from=" + rbLive + "&n=2"), false},
		"a field that is noise may differ":            {noised, jsonReq("POST", "/touch", `{"id":"`+rbLive+`","at":"2026-11-30T23:59:59Z"}`), true},
		"but an id in a noise field must be this run": {noised, jsonReq("POST", "/touch", `{"id":"`+rbOther+`","at":"2026-11-30T23:59:59Z"}`), false},
		"nor may it be the recorded one":              {noised, jsonReq("POST", "/touch", `{"id":"`+rbRec+`","at":"2026-11-30T23:59:59Z"}`), false},
		"the create again, with the id it was given":  {create, authed(jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`)), true},
		"the create again, with a second id":          {create, authed(jsonReq("POST", "/items", `{"id":"`+y2+`","name":"widget"}`)), false},
		"another creator handed the bound live id":    {other, jsonReq("POST", "/others", `{"id":"`+rbLive+`","tenant":"`+tenant+`"}`), false},
		"a request that names no id":                  {list, getReq("/items"), true},
	})
}

// uuid4 is a random version-4 UUID drawn from rng.
func uuid4(rng *rand.Rand) string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Two creates on one endpoint, each stamping the time into its payload, and
// their read-backs. With the stamp not marked as noise no live create is a
// recorded one, ids aside, so nothing is followed: every call is answered
// exactly as it is without rebinding — whoever the lenient pass picks — and no
// answer names an id of this run. (Binding whatever the lenient pass picks
// goes wrong by chance: one random id is a few edits closer to another, and a
// healthy app then reads back the other entity's data under its own id.) With
// the stamp configured as request-body noise each create is its
// recording's own, and both entities are followed, in either order.
func TestRebind_TwoCreatesWithAFieldThatDrifts(t *testing.T) {
	type call struct{ name, body string }
	run := func(rng *rand.Rand, on bool, noise map[string][]string, names [2]string, appOrder [2]int) (calls []call, db *mockMemDb, rec, live [2]string) {
		h := newHTTP()
		rec, live = [2]string{uuid4(rng), uuid4(rng)}, [2]string{uuid4(rng), uuid4(rng)}
		body := func(id, name, at string) string {
			return `{"id":"` + id + `","name":"` + name + `","createdAt":"` + at + `"}`
		}
		var pool []*models.Mock
		for i := range rec {
			n := strconv.Itoa(i + 1)
			at := time.Duration(i) * time.Second
			pool = append(pool,
				rbMock("create"+n, "POST", "http://inv/items", body(rec[i], names[i], rbBase.Add(at).Format(time.RFC3339)), 201, `{"id":"`+rec[i]+`","n":`+n+`}`, at),
				rbMock("get"+n, "GET", "http://inv/items/"+rec[i], "", 200, `{"id":"`+rec[i]+`","n":`+n+`}`, at+500*time.Millisecond))
		}
		for _, m := range pool {
			m.TestModeInfo.Lifetime = models.LifetimeSession // as keploy mock record writes them: reusable
		}
		db = &mockMemDb{mocks: pool, updateUnFilteredReturn: true, noRebind: !on}
		store := &rotating{db}
		for _, i := range appOrder {
			for _, in := range []*req{
				jsonReq("POST", "/items", body(live[i], names[i], rbBase.Add(time.Duration(rng.Intn(1e6))*time.Second).Format(time.RFC3339))),
				getReq("/items/" + live[i]),
			} {
				ok, stub, _, err := h.match(context.Background(), in, store, nil, noise, nil, true, false, false, true, true)
				if err != nil || !ok {
					t.Fatalf("%s %s must match: ok=%v err=%v", in.method, in.url.Path, ok, err)
				}
				calls = append(calls, call{stub.Name, stub.Spec.HTTPResp.Body})
			}
		}
		return calls, db, rec, live
	}
	for _, names := range [][2]string{{"widget-1", "widget-2"}, {"widget", "widget"}} {
		for _, appOrder := range [][2]int{{0, 1}, {1, 0}} {
			for seed := int64(0); seed < 300; seed++ {
				with, db, rec, live := run(rand.New(rand.NewSource(seed)), true, nil, names, appOrder)
				without, _, _, _ := run(rand.New(rand.NewSource(seed)), false, nil, names, appOrder)
				if !reflect.DeepEqual(with, without) {
					t.Fatalf("names %v, app order %v, seed %d: a create that is no recorded one must replay as without rebinding:\n with    %v\n without %v", names, appOrder, seed, with, without)
				}
				if got := db.bound(rec[0], rec[1]); len(got) != 0 {
					t.Fatalf("names %v, app order %v, seed %d: bound %v on a lenient match", names, appOrder, seed, got)
				}
				for _, c := range with {
					if strings.Contains(c.body, live[0]) || strings.Contains(c.body, live[1]) {
						t.Fatalf("names %v, app order %v, seed %d: %s was answered with an id of this run: %s", names, appOrder, seed, c.name, c.body)
					}
				}
			}
		}
	}
	// With the stamp marked as noise (test.globalNoise, as the proxy hands it
	// over: lowercased), each create is told from the other by its name.
	for _, appOrder := range [][2]int{{0, 1}, {1, 0}} {
		for seed := int64(0); seed < 300; seed++ {
			calls, db, rec, live := run(rand.New(rand.NewSource(seed)), true, map[string][]string{"createdat": {}}, [2]string{"widget-1", "widget-2"}, appOrder)
			if got := db.bound(rec[0], rec[1]); got[rec[0]] != live[0] || got[rec[1]] != live[1] {
				t.Fatalf("app order %v, seed %d: each create must bind its own recording: %v", appOrder, seed, got)
			}
			for k, i := range appOrder {
				n := strconv.Itoa(i + 1)
				want := `{"id":"` + live[i] + `","n":` + n + `}`
				if c := calls[2*k]; c.name != "create"+n || c.body != want {
					t.Fatalf("app order %v, seed %d: create %s was answered by %s: %s", appOrder, seed, n, c.name, c.body)
				}
				if c := calls[2*k+1]; c.name != "get"+n || c.body != want {
					t.Fatalf("app order %v, seed %d: the read-back of entity %s was answered by %s: %s", appOrder, seed, n, c.name, c.body)
				}
			}
		}
	}
}

// An id the app sends in a header on every call (the signed-in user) names
// every request it makes. It must not make a request for ANOTHER entity read
// as the recorded one: the URL and the body are compared, with this run's ids
// put back, and here they name an id nobody made. Such a call was answered
// with this run's id and no warning — the wrong question made to look right.
func TestRebind_AnIDInAHeaderDoesNotNameARequestForAnotherEntity(t *testing.T) {
	const nobody = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	core, logs := observer.New(zap.WarnLevel)
	h := newHTTP()
	h.Logger = zap.New(core)
	signup := rbMock("signup", "POST", "http://users/users", `{"id":"`+rbRec+`","name":"a"}`, 201, `{"id":"`+rbRec+`"}`, 0)
	profile := rbMock("profile", "GET", "http://users/users/"+rbRec+"/profile", "", 200, `{"id":"`+rbRec+`","plan":"pro"}`, time.Second)
	profile.Spec.HTTPReq.Header = map[string]string{"X-User-Id": rbRec}
	act := rbMock("act", "POST", "http://users/act", `{"actor":"`+rbRec+`","target":"`+rbRec+`"}`, 200, `{"target":"`+rbRec+`"}`, 2*time.Second)
	db := &mockMemDb{mocks: []*models.Mock{signup, profile, act}, updateUnFilteredReturn: true}
	rbMatch(t, h, db, jsonReq("POST", "/users", `{"id":"`+rbLive+`","name":"a"}`)) // rbRec is bound to rbLive

	asUser := func(path string) *req {
		in := getReq(path)
		in.header.Set("X-User-Id", rbLive)
		return in
	}
	// The healthy calls are answered with this run's id.
	assertServes(t, rbMatch(t, h, db, asUser("/users/"+rbLive+"/profile")), rbLive, rbRec)
	assertServes(t, rbMatch(t, h, db, jsonReq("POST", "/act", `{"actor":"`+rbLive+`","target":"`+rbLive+`"}`)), rbLive, rbRec)
	for name, in := range map[string]*req{
		"in the path": asUser("/users/" + nobody + "/profile"),
		"in the body": jsonReq("POST", "/act", `{"actor":"`+rbLive+`","target":"`+nobody+`"}`),
		// The recorded id where this run's belongs is no better: the request
		// speaks of the recorded entity.
		"the recorded id in the path": asUser("/users/" + rbRec + "/profile"),
		"the recorded id in the body": jsonReq("POST", "/act", `{"actor":"`+rbLive+`","target":"`+rbRec+`"}`),
	} {
		body := rbMatch(t, h, db, in).Spec.HTTPResp.Body
		if !strings.Contains(body, rbRec) || strings.Contains(body, rbLive) || strings.Contains(body, nobody) {
			t.Errorf("another entity named %s: must be answered as recorded, got %s", name, body)
		}
	}
	// None of them is reported as "names neither": each does name this run's
	// id, in the header or in another field.
	if n := logs.FilterMessageSnippet("names another id").Len(); n != 0 {
		t.Fatalf("%d calls were reported as naming neither id: %v", n, logs.All())
	}
}

// Two bound ids the app sends the wrong way round — a transfer from B to A
// where the recording moves from A to B — are each "named", yet the request is
// not the recorded one with this run's ids in place: it is answered as
// recorded, in a query and in a form body alike. Answered with this run's
// ids, it would say A was debited when the app asked to debit B.
func TestRebind_TwoIDsTheWrongWayRoundAreAnsweredAsRecorded(t *testing.T) {
	const xa, xb = rbRec, rbOther
	const ya, yb = rbLive, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	h := newHTTP()
	open := func(name, id string, at time.Duration) *models.Mock {
		return rbMock(name, "POST", "http://bank/accounts", `{"id":"`+id+`","owner":"`+name+`"}`, 201, `{"id":"`+id+`"}`, at)
	}
	byQuery := rbMock("by-query", "POST", "http://bank/transfer?from="+xa+"&to="+xb, "", 200, `{"debited":"`+xa+`","credited":"`+xb+`"}`, 2*time.Second)
	byQuery.Spec.HTTPReq.URLParams = map[string]string{"from": xa, "to": xb}
	byForm := rbMock("by-form", "POST", "http://bank/transfer-form", "from="+xa+"&to="+xb+"&amount=5", 200, `{"debited":"`+xa+`","credited":"`+xb+`"}`, 3*time.Second)
	byForm.Spec.HTTPReq.Header["Content-Type"] = "application/x-www-form-urlencoded"
	db := &mockMemDb{mocks: []*models.Mock{open("a", xa, 0), open("b", xb, time.Second), byQuery, byForm}, updateUnFilteredReturn: true}
	rbMatch(t, h, db, jsonReq("POST", "/accounts", `{"id":"`+ya+`","owner":"a"}`))
	rbMatch(t, h, db, jsonReq("POST", "/accounts", `{"id":"`+yb+`","owner":"b"}`))
	if got := db.bound(xa, xb); got[xa] != ya || got[xb] != yb {
		t.Fatalf("both accounts must be bound: %v", got)
	}
	query := func(from, to string) *req {
		in := &req{method: "POST", url: &url.URL{Path: "/transfer", RawQuery: "from=" + from + "&to=" + to}, header: http.Header{}}
		return in
	}
	form := func(from, to string) *req {
		in := jsonReq("POST", "/transfer-form", "from="+from+"&to="+to+"&amount=5")
		in.header.Set("Content-Type", "application/x-www-form-urlencoded")
		return in
	}
	for name, c := range map[string]struct {
		in   *req
		want string
	}{
		"query, the recorded way":      {query(ya, yb), `{"debited":"` + ya + `","credited":"` + yb + `"}`},
		"query, the wrong way round":   {query(yb, ya), `{"debited":"` + xa + `","credited":"` + xb + `"}`},
		"form, the recorded way":       {form(ya, yb), `{"debited":"` + ya + `","credited":"` + yb + `"}`},
		"form, the wrong way round":    {form(yb, ya), `{"debited":"` + xa + `","credited":"` + xb + `"}`},
		"query, one id for both sides": {query(ya, ya), `{"debited":"` + xa + `","credited":"` + xb + `"}`},
	} {
		if got := rbMatch(t, h, db, c.in).Spec.HTTPResp.Body; got != c.want {
			t.Errorf("%s: answered %s, want %s", name, got, c.want)
		}
	}
}

// A create that is answered as recorded binds nothing. Here the app makes an
// order for user B where the recording made one for user A: it is not the
// recorded create, so its id is not the recorded order's. Bound all the same,
// the read-back would tell the app that the order it made for B belongs to A,
// under the id it had just minted.
func TestRebind_ACreateAnsweredAsRecordedBindsNothing(t *testing.T) {
	const xa, xb, xo = rbRec, rbOther, "11111111-1111-4111-8111-111111111111"
	const ya, yb, yo = rbLive, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "bbbbbbbb-cccc-4ddd-9eee-ffffffffffff"
	for name, owner := range map[string]string{
		"an owner this run made for another entity": yb,
		"an owner nobody made":                      "cccccccc-dddd-4eee-8fff-000000000000",
		"the recorded owner, as recorded":           xa,
	} {
		h := newHTTP()
		user := func(n, id string, at time.Duration) *models.Mock {
			return rbMock(n, "POST", "http://shop/users", `{"id":"`+id+`","name":"`+n+`"}`, 201, `{"id":"`+id+`"}`, at)
		}
		order := rbMock("order", "POST", "http://shop/orders", `{"id":"`+xo+`","owner":"`+xa+`"}`, 201, `{"id":"`+xo+`","owner":"`+xa+`"}`, 2*time.Second)
		getOrder := rbMock("getOrder", "GET", "http://shop/orders/"+xo, "", 200, `{"id":"`+xo+`","owner":"`+xa+`","ownerName":"a"}`, 3*time.Second)
		db := &mockMemDb{mocks: []*models.Mock{user("a", xa, 0), user("b", xb, time.Second), order, getOrder}, updateUnFilteredReturn: true}
		rbMatch(t, h, db, jsonReq("POST", "/users", `{"id":"`+ya+`","name":"a"}`))
		rbMatch(t, h, db, jsonReq("POST", "/users", `{"id":"`+yb+`","name":"b"}`))

		made := rbMatch(t, h, db, jsonReq("POST", "/orders", `{"id":"`+yo+`","owner":"`+owner+`"}`))
		if want := `{"id":"` + xo + `","owner":"` + xa + `"}`; made.Spec.HTTPResp.Body != want {
			t.Fatalf("%s: the create was answered %s, want it as recorded", name, made.Spec.HTTPResp.Body)
		}
		if got := db.bound(xo); len(got) != 0 {
			t.Fatalf("%s: a create answered as recorded bound its id: %v", name, got)
		}
		if db.bindings.Claimed("order") {
			t.Fatalf("%s: a create answered as recorded claimed its recording", name)
		}
		read := rbMatch(t, h, db, getReq("/orders/"+yo))
		if want := `{"id":"` + xo + `","owner":"` + xa + `","ownerName":"a"}`; read.Spec.HTTPResp.Body != want {
			t.Fatalf("%s: the read-back was answered %s, want it as recorded", name, read.Spec.HTTPResp.Body)
		}
		// The recording is still there for the create it was recorded for.
		assertServes(t, rbMatch(t, h, db, jsonReq("POST", "/orders", `{"id":"`+yo+`","owner":"`+ya+`"}`)), `{"id":"`+yo+`","owner":"`+ya+`"}`, "")
		assertServes(t, rbMatch(t, h, db, getReq("/orders/"+yo)), `{"id":"`+yo+`","owner":"`+ya+`","ownerName":"a"}`, "")
	}
}

// A creator the lenient pass answered from is not claimed: it stays first in
// line for the create it was recorded for. Claimed, it would send that create
// to the next recording, and the entity would be bound to another's data.
func TestRebind_ALenientAnswerDoesNotClaimACreator(t *testing.T) {
	served, db, x1, x2, y1, _ := func() ([]string, *mockMemDb, string, string, string, string) {
		h := newHTTP()
		x1, x2 := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
		y1, stray := rbLive, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		c1 := rbMock("c1", "POST", "http://inv/items", `{"id":"`+x1+`","name":"widget"}`, 201, `{"seq":1}`, 0)
		c2 := rbMock("c2", "POST", "http://inv/items", `{"id":"`+x2+`","name":"widget"}`, 201, `{"seq":2}`, time.Second)
		for _, m := range []*models.Mock{c1, c2} {
			m.TestModeInfo.Lifetime = models.LifetimeSession
		}
		db := &mockMemDb{mocks: []*models.Mock{c1, c2}, updateUnFilteredReturn: true}
		var served []string
		for _, body := range []string{
			`{"id":"` + stray + `","name":"gadget"}`, // no recorded create: answered leniently
			`{"id":"` + y1 + `","name":"widget"}`,    // the first recorded create
		} {
			served = append(served, rbMatch(t, h, db, jsonReq("POST", "/items", body)).Name)
		}
		return served, db, x1, x2, y1, stray
	}()
	if got := db.bound(x1, x2); len(got) != 1 || got[x1] != y1 {
		t.Fatalf("the first recorded create must bind the first recording (served %v): %v", served, got)
	}
}

// A batch create whose elements arrive in another order than recorded (the
// app ranges over a map) is not the recorded request: lined up by position,
// alice's recorded id would be bound to bob's. It is answered as it is
// without rebinding and nothing is bound. Bound by position, each entity
// would get the other's recording, and a read of alice by her new id bob.
func TestRebind_ABatchCreateInAnotherOrderBindsNothing(t *testing.T) {
	const xa, xb = rbRec, rbOther
	const ya, yb = rbLive, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	user := func(id, name string) string { return `{"id":"` + id + `","name":"` + name + `"}` }
	stage := func(on bool) (*HTTP, *mockMemDb) {
		batch := rbMock("batch", "POST", "http://users/batch", `{"users":[`+user(xa, "alice")+`,`+user(xb, "bob")+`]}`, 201, `{"created":["`+xa+`","`+xb+`"]}`, 0)
		ga := rbMock("ga", "GET", "http://users/users/"+xa, "", 200, user(xa, "alice"), time.Second)
		gb := rbMock("gb", "GET", "http://users/users/"+xb, "", 200, user(xb, "bob"), 2*time.Second)
		return newHTTP(), &mockMemDb{mocks: []*models.Mock{batch, ga, gb}, updateUnFilteredReturn: true, noRebind: !on}
	}
	replay := func(on bool, first, second string) (answers []string, db *mockMemDb) {
		h, db := stage(on)
		for _, in := range []*req{
			jsonReq("POST", "/batch", `{"users":[`+first+`,`+second+`]}`),
			getReq("/users/" + ya), getReq("/users/" + yb),
		} {
			stub := rbMatch(t, h, db, in)
			answers = append(answers, stub.Name+" "+stub.Spec.HTTPResp.Body)
		}
		return answers, db
	}
	with, db := replay(true, user(yb, "bob"), user(ya, "alice"))
	without, _ := replay(false, user(yb, "bob"), user(ya, "alice"))
	if !reflect.DeepEqual(with, without) {
		t.Fatalf("a batch in another order must replay as without rebinding:\n with    %v\n without %v", with, without)
	}
	if got := db.bound(xa, xb); len(got) != 0 {
		t.Fatalf("a batch in another order bound %v", got)
	}
	// In the recorded order it is the recorded request: both are followed.
	inOrder, db := replay(true, user(ya, "alice"), user(yb, "bob"))
	want := []string{`batch {"created":["` + ya + `","` + yb + `"]}`, "ga " + user(ya, "alice"), "gb " + user(yb, "bob")}
	if !reflect.DeepEqual(inOrder, want) || len(db.bound(xa, xb)) != 2 {
		t.Fatalf("a batch in the recorded order must be followed: %v, bound %v", inOrder, db.bound(xa, xb))
	}
}

// A create recorded more than once — the same request answered 201 and then
// 409, or 503 and then, on the app's retry, 201 — is a stateful group, and the
// create made with this run's id takes its place in it: the next identical
// request gets the next recording. Were the first request, matched by its
// shape, to leave the group's cursor alone, the second would start the group
// again: 201,201 for a duplicate, and 503,503 for an app that retries once.
//
// The group is that of the recordings as this request would have been sent to
// match them byte for byte — with the id it just made AND the ids bound
// earlier that it carries (here, with owner, the user the item belongs to) —
// because that is the group the next identical request will find.
func TestRebind_ARepeatedCreateAdvancesThroughItsRecordings(t *testing.T) {
	const user, userLive = rbOther, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for name, c := range map[string]struct {
		statuses []int
		want     string
	}{
		"created, then already exists":      {[]int{201, 409}, "201,409,409"},
		"unavailable, then created":         {[]int{503, 201}, "503,201,201"},
		"unavailable twice, then created":   {[]int{503, 503, 201}, "503,503,201"},
		"one recording: nothing to advance": {[]int{201}, "201,201,201"},
	} {
		for _, owner := range []bool{false, true} {
			// correlated stages the recordings as the agent does: with the
			// request→response echoes it finds in them
			// (mocknoise.MaterializeCorrelations). A create whose answer
			// echoes its id is then matched by the correlation pass, and
			// must take its place in the group there too.
			for _, mode := range []struct{ stateful, correlated bool }{{true, false}, {false, false}, {true, true}} {
				stateful, correlated := mode.stateful, mode.correlated
				h := newHTTP()
				item := func(id, owner string) string { return `{"id":"` + id + `","name":"widget"` + owner + `}` }
				ownedBy := func(id string) string {
					if !owner {
						return ""
					}
					return `,"owner":"` + id + `"`
				}
				pool := []*models.Mock{rbMock("signup", "POST", "http://inv/users", `{"id":"`+user+`"}`, 201, `{}`, -time.Second)}
				for i, status := range c.statuses {
					m := rbMock(fmt.Sprintf("r%d", i+1), "POST", "http://inv/items", item(rbRec, ownedBy(user)), status, `{"id":"`+rbRec+`","status":`+strconv.Itoa(status)+`}`, time.Duration(i)*time.Second)
					m.TestModeInfo.Lifetime, m.TestModeInfo.Consume = models.LifetimeSession, models.ConsumeCursorSaturate
					pool = append(pool, m)
				}
				if correlated {
					for _, m := range pool {
						mocknoise.MaterializeCorrelations(m)
					}
					if len(pool[1].Spec.Correlations) == 0 {
						t.Fatalf("%s: precondition: the create's answer echoes its id, so it is correlated", name)
					}
				}
				db := &mockMemDb{mocks: pool, updateUnFilteredReturn: true}
				rbMatch(t, h, db, jsonReq("POST", "/users", `{"id":"`+userLive+`"}`))
				var got []string
				for i := 0; i < 3; i++ {
					ok, stub, _, err := h.match(context.Background(), jsonReq("POST", "/items", item(rbLive, ownedBy(userLive))), db, nil, nil, nil, true, false, false, stateful, true)
					if err != nil || !ok {
						t.Fatalf("%s: call %d must match: ok=%v err=%v", name, i+1, ok, err)
					}
					assertServes(t, stub, rbLive, rbRec) // every one of them names this run's id
					got = append(got, strconv.Itoa(stub.Spec.HTTPResp.StatusCode))
				}
				want := c.want
				if !stateful { // the cursor is off: the first recording, every time
					want = strings.Repeat(strconv.Itoa(c.statuses[0])+",", 3)
					want = want[:len(want)-1]
				}
				if strings.Join(got, ",") != want {
					t.Errorf("%s (owner=%v, stateful=%v): replayed %s, want %s", name, owner, stateful, strings.Join(got, ","), want)
				}
			}
		}
	}
}

// racing is a store on which another request binds the id a match is about to
// bind, between the match's choice and its commit.
type racing struct {
	*mockMemDb
	creator string
	pairs   map[string]string
	raced   bool
	used    []string // the mocks updated in the pool, in order
}

func (r *racing) UpdateUnFilteredMock(old, updated *models.Mock) bool {
	r.used = append(r.used, old.Name)
	return r.mockMemDb.UpdateUnFilteredMock(old, updated)
}

// Bindings is the double's own, but the first commit made through it finds
// another request has just claimed creator with pairs: the race a match on
// another connection wins between this request's alignment and its commit.
func (r *racing) Bindings() *integrations.Bindings {
	if r.mockMemDb.Bindings() == nil {
		return nil
	}
	return integrations.NewBindings(&r.bindings, r.epoch, func(creator string, pairs map[string]string, epoch uint64) bool {
		if !r.raced {
			r.raced = true
			r.bindings.ClaimAndBind(r.creator, r.pairs)
		}
		return epoch == r.epoch && r.bindings.ClaimAndBind(creator, pairs)
	})
}

// Two requests that race to give one recorded id its live id: one wins, and
// the other is answered as recorded — not with the winner's id, which it never
// sent, and not with its own, which stands for nothing.
func TestRebind_ARequestThatLosesTheRaceForAnIDIsAnsweredAsRecorded(t *testing.T) {
	h := newHTTP()
	create, _, _ := rbFlow()
	db := &racing{mockMemDb: &mockMemDb{mocks: []*models.Mock{create}, updateUnFilteredReturn: true}, pairs: map[string]string{rbRec: rbOther}}
	stub := rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`))
	if want := `{"id":"` + rbRec + `","name":"widget"}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("the loser was answered %s, want it as recorded", stub.Spec.HTTPResp.Body)
	}
	if got := db.bound(rbRec); got[rbRec] != rbOther {
		t.Fatalf("the winner's id must stand: %v", got)
	}
	if rec, ok := db.bindings.Recorded(rbLive); ok {
		t.Fatalf("the loser's id was bound too, for %s", rec)
	}
}

// Identical creates on two connections race for the first of two recordings
// the pool shares (session mocks). The one that loses is matched again and
// takes the second recording, with its own id: each create has its
// recording, as when they come one after the other. The first recording is
// not used for the loser at all — no noise saved on it, not counted as used.
func TestRebind_ACreateThatLosesTheRaceForACreatorTakesTheNext(t *testing.T) {
	const x1, x2 = rbRec, rbOther
	const y1, y2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", rbLive
	c1 := rbMock("create-1", "POST", "http://inv/items", `{"id":"`+x1+`","name":"widget"}`, 201, `{"id":"`+x1+`","seq":1}`, 0)
	c2 := rbMock("create-2", "POST", "http://inv/items", `{"id":"`+x2+`","name":"widget"}`, 201, `{"id":"`+x2+`","seq":2}`, time.Second)
	c1.TestModeInfo.Lifetime, c2.TestModeInfo.Lifetime = models.LifetimeSession, models.LifetimeSession
	db := &racing{mockMemDb: &mockMemDb{mocks: []*models.Mock{c2, c1}, updateUnFilteredReturn: true}, creator: "create-1", pairs: map[string]string{x1: y1}}

	stub := rbMatch(t, newHTTP(), db, jsonReq("POST", "/items", `{"id":"`+y2+`","name":"widget"}`))
	if want := `{"id":"` + y2 + `","seq":2}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("the create that lost the first recording was answered %s, want the second, with its own id: %s", stub.Spec.HTTPResp.Body, want)
	}
	if got := db.bound(x1, x2); got[x1] != y1 || got[x2] != y2 {
		t.Fatalf("each create must have its own recording: %v", got)
	}
	if !slices.Equal(db.used, []string{"create-2"}) {
		t.Fatalf("recordings used for the request: %v, want only the one that answered it", db.used)
	}
}

// The same race for a per-test recording: only one request can consume it,
// and the one that did holds it. Its claim, refused because another request
// claimed the creator first, leaves it answering the request as recorded,
// from that recording — not matched again for a recording it no longer
// needs, having taken this one from the pool.
func TestRebind_ACreateThatConsumedARecordingAnotherClaimedIsAnsweredFromIt(t *testing.T) {
	const x1, x2 = rbRec, rbOther
	const y1, y2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", rbLive
	c1 := rbMock("create-1", "POST", "http://inv/items", `{"id":"`+x1+`","name":"widget"}`, 201, `{"id":"`+x1+`","seq":1}`, 0)
	c2 := rbMock("create-2", "POST", "http://inv/items", `{"id":"`+x2+`","name":"widget"}`, 201, `{"id":"`+x2+`","seq":2}`, time.Second)
	db := &racing{mockMemDb: &mockMemDb{mocks: []*models.Mock{c2, c1}, deleteFilteredReturn: true}, creator: "create-1", pairs: map[string]string{x1: y1}}

	stub := rbMatch(t, newHTTP(), db, jsonReq("POST", "/items", `{"id":"`+y2+`","name":"widget"}`))
	if want := `{"id":"` + x1 + `","seq":1}`; stub.Spec.HTTPResp.Body != want {
		t.Fatalf("answered %s, want the recording it consumed, as recorded: %s", stub.Spec.HTTPResp.Body, want)
	}
	if got := db.bound(x1, x2); len(got) != 1 || got[x1] != y1 {
		t.Fatalf("bound %v: want only the other request's pair", got)
	}
}

// The HTTP request tokenizer: carried is everything, read as text; bindable is
// what the aligner's own walk reaches — exactly the strings that lining the
// request up with itself pairs. One walk decides both: a textual guess at "a
// whole JSON string" would call an NDJSON body's id bindable, which the
// aligner's decode can never bind.
func TestHTTPRequestValues(t *testing.T) {
	const id, other = rbRec, rbOther
	for name, c := range map[string]struct {
		url, body string
		header    map[string]string
		carried   []string
		bindable  []string
	}{
		"a path segment":                 {"http://dep/items/" + id, "", nil, []string{id}, []string{id}},
		"a query value":                  {"http://dep/find?ref=" + id, "", nil, []string{id}, []string{id}},
		"a query key that repeats":       {"http://dep/find?ref=" + id + "&ref=" + other, "", nil, []string{id, other}, nil},
		"a JSON object":                  {"http://dep/", `{"id":"` + id + `","n":1}`, nil, []string{id}, []string{id}},
		"a JSON array":                   {"http://dep/", `[{"id":"` + id + `"},"` + other + `"]`, nil, []string{id, other}, []string{id, other}},
		"NDJSON":                         {"http://dep/_bulk", `{"index":{"_id":"` + id + `"}}` + "\n" + `{"title":"hello"}` + "\n", nil, []string{id}, nil},
		"a body that only opens as JSON": {"http://dep/", `{"id":"` + id + `"`, nil, []string{id}, nil},
		"a form body":                    {"http://dep/", "id=" + id + "&name=widget", nil, []string{id}, nil},
		"a header":                       {"http://dep/", "", map[string]string{"X-Request-Id": id}, []string{id}, nil},
		"a header, and a JSON string":    {"http://dep/", `{"id":"` + other + `"}`, map[string]string{"X-Request-Id": id}, []string{id, other}, []string{other}},
		"an id inside a longer string":   {"http://dep/", `{"ref":"order:` + id + `"}`, nil, []string{id}, nil},
		"an object key":                  {"http://dep/", `{"` + id + `":"x"}`, nil, []string{id}, nil},
		"a path id beside NDJSON ids":    {"http://dep/" + id + "/_bulk", `{"index":{"_id":"` + other + `"}}` + "\n" + `{"title":"hello"}` + "\n", nil, []string{id, other}, []string{id}},
		"no id at all":                   {"http://dep/items", `{"name":"widget"}`, nil, nil, nil},
	} {
		m := &models.Mock{Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "POST", URL: c.url, Body: c.body, Header: c.header}}}
		for _, safe := range []models.Method{"GET", "head", "OPTIONS", "TRACE"} {
			lookup := &models.Mock{Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: safe, URL: c.url, Body: c.body, Header: c.header}}}
			if carried, bindable, _ := httpRequestValues(lookup); !slices.Equal(carried, c.carried) || len(bindable) != 0 {
				t.Errorf("%s, by %s: carried %v, bindable %v: a safe request carries what it carries and binds nothing", name, safe, carried, bindable)
			}
		}
		carried, bindable, readable := httpRequestValues(m)
		if !readable {
			t.Errorf("%s: reported unreadable", name)
		}
		sort.Strings(carried)
		sort.Strings(bindable)
		sort.Strings(c.carried)
		sort.Strings(c.bindable)
		if !reflect.DeepEqual(carried, c.carried) || !reflect.DeepEqual(bindable, c.bindable) {
			t.Errorf("%s: carried %q bindable %q, want %q and %q", name, carried, bindable, c.carried, c.bindable)
		}
		var paired []string
		mocknoise.AlignStrings(c.url, c.body, c.url, c.body, func(rec, _ string) {
			if _, random := mocknoise.AppRandomClass(rec); random {
				paired = append(paired, rec)
			}
		}, func(string) {})
		sort.Strings(paired)
		if !reflect.DeepEqual(bindable, paired) {
			t.Errorf("%s: bindable %q, but the request lined up with itself pairs %q", name, bindable, paired)
		}
	}
}

// An id that first reaches a dependency where no match can line it up — in a
// header, in an NDJSON body — has no creator: it is carried there first, so
// the later request that names it in its path is not the first to carry it
// either. A call for an id nobody made is then answered as recorded, and
// nothing is bound. (Were "carried" read off the aligner's walk too, the
// later request would pass for the creator and bind whatever it was sent.)
func TestRebind_AnIDFirstSentWhereItCannotBeLinedUpHasNoCreator(t *testing.T) {
	const nobody = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for name, first := range map[string]*models.Mock{
		"in a header": func() *models.Mock {
			m := rbMock("submit", "POST", "http://dep/submit", `{"job":"resize"}`, 202, `{}`, 0)
			m.Spec.HTTPReq.Header["X-Request-Id"] = rbRec
			return m
		}(),
		"in NDJSON": rbMock("bulk", "POST", "http://dep/_bulk", `{"index":{"_id":"`+rbRec+`"}}`+"\n"+`{"title":"hello"}`+"\n", 200, `{"errors":false}`, 0),
	} {
		h := newHTTP()
		later := rbMock("later", "GET", "http://dep/status/"+rbRec, "", 200, `{"id":"`+rbRec+`","state":"done"}`, time.Second)
		seed := rbMock("seed", "POST", "http://dep/seed", `{"id":"`+rbOther+`"}`, 200, `{}`, 2*time.Second) // so that the set is followed at all
		db := &mockMemDb{mocks: []*models.Mock{first, later, seed}, updateUnFilteredReturn: true}
		if got := db.FirstCarried(first); len(got) != 0 {
			t.Fatalf("%s: the first carrier cannot bind the id, yet first-carries %v", name, got)
		}
		if got := db.FirstCarried(later); len(got) != 0 {
			t.Fatalf("%s: a later carrier passes for the creator of %v", name, got)
		}
		assertServes(t, rbMatch(t, h, db, getReq("/status/"+nobody)), rbRec, nobody)
		if got := db.bound(rbRec); len(got) != 0 {
			t.Fatalf("%s: bound %v", name, got)
		}
	}
}

// A response whose body was recorded in an encoding the recorder does not
// decode (deflate, zstd) cannot be read, so it may be where a dependency
// first handed the app an id. The set is then not followed at all: otherwise
// the request that sends that id back passes for the first to carry it, and a
// call for an id nobody made is answered as if right.
func TestRebind_ASetWithAnUnreadableResponseIsLeftAlone(t *testing.T) {
	const nobody = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	set := func(encoding string) (*HTTP, *mockMemDb) {
		// The dependency mints the order's id; its answer is stored as sent.
		place := rbMock("place", "POST", "http://shop/orders", `{"sku":"a"}`, 201, "\x78\x9c\x01\x02", 0)
		place.Spec.HTTPResp.Header["Content-Encoding"] = encoding
		read := rbMock("pay", "POST", "http://shop/orders/"+rbRec+"/pay", "", 200, `{"id":"`+rbRec+`","status":"paid"}`, time.Second)
		return newHTTP(), &mockMemDb{mocks: []*models.Mock{place, read}, updateUnFilteredReturn: true}
	}
	pay := func(id string) *req { in := getReq("/orders/" + id + "/pay"); in.method = "POST"; return in }
	for _, encoding := range []string{"deflate", "zstd", "gzip, deflate"} {
		h, db := set(encoding)
		if _, why := integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, db.mocks); !strings.Contains(why, `"place"`) {
			t.Fatalf("%s: the reason must name the mock whose response cannot be read: %q", encoding, why)
		}
		if rbOf(db, pay(nobody)) != nil {
			t.Fatalf("%s: a set with a response that cannot be read must not be followed", encoding)
		}
		assertServes(t, rbMatch(t, h, db, pay(nobody)), rbRec, nobody)
		if got := db.bound(rbRec); len(got) != 0 {
			t.Fatalf("%s: bound %v", encoding, got)
		}
	}
	// An encoding the recorder decodes is stored as plain text, and read.
	for _, encoding := range []string{"gzip", "br", "identity", "GZIP"} {
		_, db := set(encoding)
		if ix, why := integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, db.mocks); ix == nil || why != "" {
			t.Fatalf("%s: a decoded response is readable: index %v, why %q", encoding, ix, why)
		}
	}
}

// Creators the live request does not fit keep the pool's own order. Three
// recordings of one call, each naming a job by an id the app minted, under a
// tenant number that differs on every run: no live call is a recorded one
// with new ids in place, so nothing is followed, and each call is answered by
// the pool's rotation exactly as it is without rebinding — the first, second
// and third recording in turn. Held first in recorded order for as long as
// they are unclaimed, the first recording would answer every call.
func TestRebind_CreatorsTheRequestDoesNotFitKeepThePoolsOrder(t *testing.T) {
	id := func(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", n*7919, n*104729) }
	var served [2][]string
	for i, on := range []bool{true, false} {
		h := newHTTP()
		var pool []*models.Mock
		for n := 1; n <= 3; n++ {
			m := rbMock(fmt.Sprintf("m%d", n), "GET", "http://dep/tenants/4711/jobs/"+id(n), "", 200, fmt.Sprintf(`{"n":%d}`, n), time.Duration(n)*time.Second)
			m.TestModeInfo.Lifetime = models.LifetimeSession
			pool = append(pool, m)
		}
		db := &mockMemDb{mocks: pool, updateUnFilteredReturn: true, noRebind: !on}
		for n := 10; n < 19; n++ {
			served[i] = append(served[i], rbMatch(t, h, &rotating{db}, getReq("/tenants/9001/jobs/"+id(n))).Name)
		}
		if on && (db.bindings.Len() != 0 || db.bindings.Claimed("m1")) {
			t.Fatal("a call that is no recorded one binds and claims nothing")
		}
	}
	if got := strings.Join(served[0], ","); got != "m1,m2,m3,m1,m2,m3,m1,m2,m3" || !reflect.DeepEqual(served[0], served[1]) {
		t.Fatalf("served %s with rebinding, %s without", got, strings.Join(served[1], ","))
	}
}

// Two recordings on one endpoint for ONE entity — the call that introduces its
// id, and a later one that carries it — each with a field that drifts and is
// not noise. Neither live call is a recorded one, ids aside, so the lenient
// pass answers both as it does without rebinding: the first from the first
// recording, the second from the second. The creator must not be preferred
// for a call that merely carries an id where it does.
func TestRebind_ACreatorDoesNotAnswerAnotherRecordingsCall(t *testing.T) {
	for name, c := range map[string]struct {
		method, url string
		body        func(id, title, at string) string
	}{
		"the id in the path": {"PUT", "/items/%s", func(_, title, at string) string {
			return `{"title":"` + title + `","updatedAt":"` + at + `"}`
		}},
		"the id in the body": {"POST", "/events%.0s", func(id, title, at string) string {
			return `{"orderId":"` + id + `","type":"` + title + `","at":"` + at + `"}`
		}},
	} {
		var answers [2][]string
		for i, on := range []bool{true, false} {
			h := newHTTP()
			first := rbMock("first", c.method, "http://dep"+fmt.Sprintf(c.url, rbRec), c.body(rbRec, "order-created", "2026-10-07T12:00:00Z"), 200, `{"rev":1}`, 0)
			second := rbMock("second", c.method, "http://dep"+fmt.Sprintf(c.url, rbRec), c.body(rbRec, "order-settled", "2026-10-07T12:00:05Z"), 200, `{"rev":2}`, time.Second)
			for _, m := range []*models.Mock{first, second} {
				m.TestModeInfo.Lifetime = models.LifetimeSession
			}
			db := &mockMemDb{mocks: []*models.Mock{first, second}, updateUnFilteredReturn: true, noRebind: !on}
			for _, title := range []string{"order-created", "order-settled"} {
				stub := rbMatch(t, h, &rotating{db}, jsonReq(c.method, fmt.Sprintf(c.url, rbLive), c.body(rbLive, title, "2026-11-30T23:59:59Z")))
				answers[i] = append(answers[i], stub.Name+" "+stub.Spec.HTTPResp.Body)
			}
			if on && db.bindings.Len() != 0 {
				t.Fatalf("%s: bound on a lenient match", name)
			}
		}
		if want := []string{`first {"rev":1}`, `second {"rev":2}`}; !reflect.DeepEqual(answers[0], want) || !reflect.DeepEqual(answers[1], want) {
			t.Errorf("%s: answered %v with rebinding and %v without, want %v", name, answers[0], answers[1], want)
		}
	}
}

// A read-back that carries a field which changes on every run is followed
// only once that field is request-body noise. Then it is the recorded request,
// this run's id and the noise aside, and is answered from that recording with
// this run's id. Until then it is no recorded request, and is answered exactly
// as without rebinding — by whichever recording the lenient passes choose —
// and without a word about "another entity", since the request does name this
// run's id. This is a limit, kept on purpose: what tells "the same call with a
// new timestamp" from "another call" is the noise the user configured or a
// detection run learned, and nothing else.
func TestRebind_AReadBackWithAFieldThatDriftsNeedsItToBeNoise(t *testing.T) {
	for name, c := range map[string]struct {
		noise map[string][]string
		want  string // "" for what is served without rebinding
	}{
		"not noise":           {nil, ""},
		"configured as noise": {map[string][]string{"at": {}}, `{"id":"` + rbLive + `","stock":5}`},
	} {
		send := func(on bool) (*models.Mock, *observer.ObservedLogs) {
			core, logs := observer.New(zap.DebugLevel)
			h := newHTTP()
			h.Logger = zap.New(core)
			create, _, _ := rbFlow()
			lookup := rbMock("lookup", "POST", "http://inv/lookup", `{"item":"`+rbRec+`","at":"2026-10-07T12:00:00Z"}`, 200, `{"id":"`+rbRec+`","stock":5}`, time.Second)
			elses := rbMock("elses", "POST", "http://inv/lookup", `{"item":"`+rbOther+`","at":"2026-10-07T12:00:09Z"}`, 200, `{"id":"`+rbOther+`","stock":9}`, 2*time.Second)
			db := &mockMemDb{mocks: []*models.Mock{create, elses, lookup}, updateUnFilteredReturn: true, noRebind: !on}
			createLive(t, h, db)
			ok, stub, _, err := h.match(context.Background(), jsonReq("POST", "/lookup", `{"item":"`+rbLive+`","at":"2026-11-30T23:59:59Z"}`), db, nil, c.noise, nil, true, false, false, true, true)
			if err != nil || !ok {
				t.Fatalf("%s: ok=%v err=%v", name, ok, err)
			}
			return stub, logs
		}
		stub, logs := send(true)
		want := c.want
		if want == "" {
			off, _ := send(false)
			want = off.Spec.HTTPResp.Body
		}
		if stub.Spec.HTTPResp.Body != want {
			t.Fatalf("%s: answered %s by %s, want %s", name, stub.Spec.HTTPResp.Body, stub.Name, want)
		}
		if n := logs.FilterMessageSnippet("names another id").Len(); n != 0 {
			t.Fatalf("%s: the request names this run's id, yet was reported as naming neither", name)
		}
		// Why the id was not followed is in the debug log, and only there.
		said := logs.FilterMessageSnippet("answered a dependency call as recorded, without following ids")
		if followed := c.noise != nil; (said.Len() == 0) != followed {
			t.Fatalf("%s: %d debug lines about an id that was not followed", name, said.Len())
		}
		for _, e := range said.All() {
			if e.Level != zap.DebugLevel || e.ContextMap()["mock"] != stub.Name {
				t.Fatalf("%s: %v", name, e)
			}
		}
	}
}

// A creator answers one live request. Two recordings store the same item
// under two tenants; the first tenant is a fixture, the same on every run, and
// the second is made anew. Once the first recording has answered the request
// that named its tenant as recorded, it is spent, though that tenant was never
// bound: the request with the new tenant belongs to the second recording.
// Still in line, the first recording would fit it too — nothing says its
// tenant cannot be this new one — and the fixture tenant would be bound to it.
func TestRebind_ACreatorAnswersOneLiveRequest(t *testing.T) {
	const fixture, second, made = "5c0d3f2a-7b1e-4a9d-8c6f-0e1d2c3b4a59", "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	h := newHTTP()
	store := func(name, tenant string, rev int, at time.Duration) *models.Mock {
		m := rbMock(name, "PUT", "http://inv/tenants/"+tenant+"/items/"+rbRec, `{"size":3}`, 200, fmt.Sprintf(`{"rev":%d}`, rev), at)
		m.TestModeInfo.Lifetime = models.LifetimeSession
		return m
	}
	db := &mockMemDb{mocks: []*models.Mock{store("first", fixture, 1, 0), store("second", second, 2, time.Second)}, updateUnFilteredReturn: true}
	var served []string
	for _, tenant := range []string{fixture, made} {
		served = append(served, rbMatch(t, h, db, jsonReq("PUT", "/tenants/"+tenant+"/items/"+rbLive, `{"size":3}`)).Name)
	}
	if strings.Join(served, ",") != "first,second" {
		t.Fatalf("served %v, want first then second", served)
	}
	if got := db.bound(rbRec, fixture, second); len(got) != 2 || got[rbRec] != rbLive || got[second] != made {
		t.Fatalf("the item and the second tenant must be bound, the fixture tenant never: %v", got)
	}
}

// An id the app still sends as recorded is not one this run makes anew — a
// fixture's id, a tenant, an id the client supplied — and nothing is ever
// bound to it. A call that names another id in its place is a question about
// something else, and is answered as it is without rebinding; it does not
// take the fixture's id for its own, and the answers that follow keep naming
// the fixture as recorded.
func TestRebind_AnIDSentAsRecordedIsNeverBound(t *testing.T) {
	const fixture, stranger = rbRec, rbOther
	stage := func(on bool) (*HTTP, *mockMemDb) {
		user := rbMock("user", "GET", "http://dir/users/"+fixture, "", 200, `{"id":"`+fixture+`","name":"fixture-user"}`, 0)
		orders := rbMock("orders", "GET", "http://dir/users/"+fixture+"/orders", "", 200, `{"owner":"`+fixture+`","orders":[]}`, time.Second)
		whoami := rbMock("whoami", "GET", "http://dir/whoami", "", 200, `{"user":"`+fixture+`"}`, 2*time.Second)
		make1 := rbMock("make", "POST", "http://dir/users", `{"id":"`+fixture+`"}`, 201, `{"made":"`+fixture+`"}`, 3*time.Second)
		return newHTTP(), &mockMemDb{mocks: []*models.Mock{user, orders, whoami, make1}, updateUnFilteredReturn: true, noRebind: !on}
	}
	for name, calls := range map[string][]*req{
		// The fixture is read as recorded, then a stranger is asked for.
		"its own call as recorded, then another id": {getReq("/users/" + fixture), getReq("/users/" + stranger), getReq("/whoami")},
		// The fixture is named as recorded in another call first.
		"named as recorded elsewhere, then another id": {getReq("/users/" + fixture + "/orders"), getReq("/users/" + stranger), getReq("/whoami")},
	} {
		h, db := stage(true)
		off, offDB := stage(false)
		for i, in := range calls {
			got, want := rbMatch(t, h, db, in), rbMatch(t, off, offDB, in)
			if got.Name != want.Name || got.Spec.HTTPResp.Body != want.Spec.HTTPResp.Body {
				t.Errorf("%s: call %d (%s) answered %s %s; without rebinding %s %s", name, i+1, in.url.Path, got.Name, got.Spec.HTTPResp.Body, want.Name, want.Spec.HTTPResp.Body)
			}
		}
		if got := db.bound(fixture); len(got) != 0 {
			t.Errorf("%s: a fixture's id was bound: %v", name, got)
		}
	}
}

// A binding that turns out to be a mistake is disowned. A call for another
// entity reached a recording first and its id was bound to the recording's;
// then the app makes that recorded call again, exactly as recorded. The
// recorded id is a constant after all: answers that name neither id go back
// to it, the call that was bound keeps seeing what it saw, and it is said
// once.
//
// It takes a write: a lookup (a safe method) is never a creator, so a lookup
// of a constant is never bound in the first place (see
// TestRebind_ALookupOfAConstantIsNeverTakenForACreate).
func TestRebind_ABindingIsDisownedWhenItsOwnCallComesBackAsRecorded(t *testing.T) {
	const fixture, stranger = rbRec, rbOther
	core, logs := observer.New(zap.WarnLevel)
	h := newHTTP()
	h.Logger = zap.New(core)
	user := rbMock("user", "POST", "http://dir/users/find", `{"id":"`+fixture+`"}`, 200, `{"id":"`+fixture+`","name":"fixture-user"}`, 0)
	whoami := rbMock("whoami", "GET", "http://dir/whoami", "", 200, `{"user":"`+fixture+`"}`, time.Second)
	db := &mockMemDb{mocks: []*models.Mock{user, whoami}, updateUnFilteredReturn: true}
	find := func(id string) *req { return jsonReq("POST", "/users/find", `{"id":"`+id+`"}`) }

	assertServes(t, rbMatch(t, h, db, find(stranger)), stranger, fixture) // the first call to the recording names another id: bound
	assertServes(t, rbMatch(t, h, db, getReq("/whoami")), stranger, fixture)
	if logs.Len() != 0 {
		t.Fatalf("nothing to say yet: %v", logs.All())
	}

	assertServes(t, rbMatch(t, h, db, find(fixture)), fixture, stranger) // the recorded call, as recorded
	disowned := logs.FilterMessageSnippet("constant after all").All()
	if len(disowned) != 1 {
		t.Fatalf("the mistake must be said once: %v", logs.All())
	}
	assertServes(t, rbMatch(t, h, db, getReq("/whoami")), fixture, stranger) // an answer that names neither: recorded again
	assertServes(t, rbMatch(t, h, db, find(stranger)), stranger, fixture)    // the call that was bound sees what it saw
	rbMatch(t, h, db, find(fixture))
	if n := logs.FilterMessageSnippet("constant after all").Len(); n != 1 {
		t.Fatalf("said %d times", n)
	}
}

// Two creates of one shape: an event, whose id the replay does not follow
// (keploy test names only the ids its templates hold), then an order, whose
// id it does. Each lines up with its own recording. The event's recording is
// a creator too, and the event's request claims it with nothing bound; were
// it no creator, the event's request would be taken for the order's create,
// the order's recorded id bound to the event's id, and the CLI told that pair.
func TestRebind_ACreateOfAnIDNotFollowedKeepsItsOwnRecording(t *testing.T) {
	const evtRec, ordRec = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const evtLive, ordLive = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "bbbbbbbb-cccc-4ddd-9eee-ffffffffffff"
	event := rbMock("event", "POST", "http://sink/docs", `{"id":"`+evtRec+`","type":"doc"}`, 201, `{"id":"`+evtRec+`","what":"event"}`, 0)
	order := rbMock("order", "POST", "http://sink/docs", `{"id":"`+ordRec+`","type":"doc"}`, 201, `{"id":"`+ordRec+`","what":"order"}`, time.Second)
	read := rbMock("read", "GET", "http://sink/docs/"+ordRec, "", 200, `{"id":"`+ordRec+`","what":"order"}`, 2*time.Second)
	h, db := newHTTP(), &mockMemDb{mocks: []*models.Mock{order, read, event}, updateUnFilteredReturn: true, only: map[string]bool{ordRec: true}}

	if got := rbMatch(t, h, db, jsonReq("POST", "/docs", `{"id":"`+evtLive+`","type":"doc"}`)).Spec.HTTPResp.Body; got != `{"id":"`+evtRec+`","what":"event"}` {
		t.Fatalf("the event was answered %s: want its own recording, as recorded (its id is not followed)", got)
	}
	if got := rbMatch(t, h, db, jsonReq("POST", "/docs", `{"id":"`+ordLive+`","type":"doc"}`)).Spec.HTTPResp.Body; got != `{"id":"`+ordLive+`","what":"order"}` {
		t.Fatalf("the order was answered %s: want its own recording, with this run's id", got)
	}
	assertServes(t, rbMatch(t, h, db, getReq("/docs/"+ordLive)), `"what":"order"`, ordRec)
	if got := db.bound(ordRec, evtRec); len(got) != 1 || got[ordRec] != ordLive {
		t.Fatalf("bound %v: want only the order's id, to the order's", got)
	}
	// With something bound, the answer of a create whose id is not followed
	// still names the recorded id, not the one its request made.
	const auditRec, auditLive = "33333333-3333-4333-8333-333333333333", "cccccccc-dddd-4eee-8fff-000000000000"
	db.mocks = append(db.mocks, rbMock("audit", "POST", "http://sink/audit", `{"id":"`+auditRec+`"}`, 201, `{"id":"`+auditRec+`"}`, 3*time.Second))
	if got := rbMatch(t, h, db, jsonReq("POST", "/audit", `{"id":"`+auditLive+`"}`)).Spec.HTTPResp.Body; got != `{"id":"`+auditRec+`"}` {
		t.Fatalf("a create whose id is not followed was answered %s", got)
	}
}

// A request nothing fits — a field that is not noise differs — that carries an
// id bound earlier is answered by the recording the lenient passes choose
// without rebinding, and the same noise is learned: they choose among the
// recordings as they are. A copy carrying this run's id reads as a few edits
// closer to the request, and would turn the choice.
func TestRebind_ARequestNothingFitsIsMatchedAmongTheRecordings(t *testing.T) {
	const x1, x2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const y1, y2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "bbbbbbbb-cccc-4ddd-9eee-ffffffffffff"
	mocks := func() []*models.Mock {
		return []*models.Mock{
			rbMock("c1", "POST", "http://inv/items", `{"id":"`+x1+`","n":1}`, 201, `{}`, 0),
			rbMock("c2", "POST", "http://inv/items", `{"id":"`+x2+`","n":2}`, 201, `{}`, time.Second),
			rbMock("charge1", "POST", "http://pay/charge", `{"order":"`+x1+`","note":"aa aa aa aa"}`, 200, `{"order":"`+x1+`","paid":1}`, 2*time.Second),
			rbMock("charge2", "POST", "http://pay/charge", `{"order":"`+x2+`","note":"zz zz zz zz"}`, 200, `{"order":"`+x2+`","paid":2}`, 3*time.Second),
		}
	}
	reqs := func() []*req {
		return []*req{
			jsonReq("POST", "/items", `{"id":"`+y1+`","n":1}`),
			jsonReq("POST", "/items", `{"id":"`+y2+`","n":2}`),
			jsonReq("POST", "/charge", `{"order":"`+y1+`","note":"zz zz zz zy"}`), // y1's order, a note no recording has
		}
	}
	servedOn, on := detect(t, true, mocks(), reqs()...)
	servedOff, off := detect(t, false, mocks(), reqs()...)
	if servedOn[2] != servedOff[2] {
		t.Fatalf("answered by %s with rebinding, %s without", servedOn[2], servedOff[2])
	}
	if !reflect.DeepEqual(on.noise, off.noise) {
		t.Fatalf("learned %v with rebinding, %v without", on.noise, off.noise)
	}
}

// A create whose answer echoes its id (correlated) sent twice with one id.
// Without rebinding both are matched on the correlation pass, which learns
// nothing; with rebinding the second is matched through a copy, and must
// learn nothing either: what a detection run persists is the same either way.
func TestRebind_ARepeatedCorrelatedCreateLearnsNoNoise(t *testing.T) {
	mocks := func() []*models.Mock {
		c := rbMock("create", "POST", "http://inv/items", `{"id":"`+rbRec+`","name":"widget"}`, 201, `{"id":"`+rbRec+`"}`, 0)
		mocknoise.MaterializeCorrelations(c)
		if len(c.Spec.Correlations) == 0 {
			t.Fatal("the create must be correlated")
		}
		return []*models.Mock{c}
	}
	in := func() *req { return jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`) }
	_, on := detect(t, true, mocks(), in(), in())
	_, off := detect(t, false, mocks(), in(), in())
	if !reflect.DeepEqual(on.noise, off.noise) {
		t.Fatalf("learned %v with rebinding, %v without", on.noise, off.noise)
	}
}

// A create whose answer echoes its id, with a Content-MD5: the correlation
// render rewrites the echo before rebinding looks at the answer, and the
// header is still recomputed for the body that is served.
//
// The correlation render signs what it rewrites itself: with rebinding off,
// and where the replay does not follow the create's id, the echo it serves is
// signed for its body too.
func TestRebind_AnEchoedCreatesIntegrityHeaderSignsWhatIsServed(t *testing.T) {
	respBody := `{"id":"` + rbRec + `","name":"widget"}`
	sum := md5.Sum([]byte(respBody)) // #nosec G401 -- the header under test is an MD5
	for _, c := range []struct {
		name       string
		correlated bool
		db         func(*mockMemDb)
		echoed     bool // the answer names this run's id
	}{
		{"correlated, followed", true, func(*mockMemDb) {}, true},
		{"not correlated, followed", false, func(*mockMemDb) {}, true},
		{"correlated, rebinding off", true, func(db *mockMemDb) { db.noRebind = true }, true},
		{"correlated, its id not followed", true, func(db *mockMemDb) { db.only = map[string]bool{rbOther: true} }, true},
		{"not correlated, rebinding off", false, func(db *mockMemDb) { db.noRebind = true }, false},
	} {
		create := rbMock("create", "POST", "http://inv/items", `{"id":"`+rbRec+`","name":"widget"}`, 201, respBody, 0)
		create.Spec.HTTPResp.Header["Content-MD5"] = base64.StdEncoding.EncodeToString(sum[:])
		create.Spec.HTTPResp.Header["ETag"] = `"` + hex.EncodeToString(sum[:]) + `"`
		if c.correlated {
			mocknoise.MaterializeCorrelations(create)
		}
		other := rbMock("other", "POST", "http://inv/others", `{"id":"`+rbOther+`"}`, 201, `{}`, time.Second)
		db := &mockMemDb{mocks: []*models.Mock{create, other}, updateUnFilteredReturn: true}
		c.db(db)
		stub := rbMatch(t, newHTTP(), db, jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`))
		if c.echoed {
			assertServes(t, stub, rbLive, rbRec)
		} else {
			assertServes(t, stub, rbRec, rbLive)
		}
		served := md5.Sum([]byte(stub.Spec.HTTPResp.Body)) // #nosec G401
		if got, want := stub.Spec.HTTPResp.Header["Content-MD5"], base64.StdEncoding.EncodeToString(served[:]); got != want {
			t.Errorf("%s: Content-MD5 %s does not sign the served body (%s)", c.name, got, want)
		}
		if got, want := stub.Spec.HTTPResp.Header["ETag"], `"`+hex.EncodeToString(served[:])+`"`; got != want {
			t.Errorf("%s: the ETag that is the body's MD5 is %s, want %s", c.name, got, want)
		}
	}

	// A header it cannot recompute (a Digest of an algorithm it does not
	// know): the correlation render serves the echo as it always has, with
	// the header as recorded.
	create := rbMock("create", "POST", "http://inv/items", `{"id":"`+rbRec+`","name":"widget"}`, 201, respBody, 0)
	create.Spec.HTTPResp.Header["Digest"] = "unixsum=12345"
	mocknoise.MaterializeCorrelations(create)
	db := &mockMemDb{mocks: []*models.Mock{create}, updateUnFilteredReturn: true, noRebind: true}
	stub := rbMatch(t, newHTTP(), db, jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`))
	assertServes(t, stub, rbLive, rbRec)
	if got := stub.Spec.HTTPResp.Header["Digest"]; got != "unixsum=12345" {
		t.Fatalf("an unknown digest became %q", got)
	}
}

// A recording that another connection took between this request's match and
// its use is neither claimed nor bound for this request: it is matched again,
// and takes the next recording, with its own id. Bound first and then not
// used, the pair would stand for a recording the app was never answered by,
// and a read-back by this run's id would get the other entity's data.
func TestRebind_ARecordingAnotherConnectionTookIsNeitherClaimedNorBound(t *testing.T) {
	const x1, x2 = rbRec, rbOther
	c1 := rbMock("create-1", "POST", "http://inv/items", `{"id":"`+x1+`","name":"widget"}`, 201, `{"id":"`+x1+`","seq":1}`, 0)
	c2 := rbMock("create-2", "POST", "http://inv/items", `{"id":"`+x2+`","name":"widget"}`, 201, `{"id":"`+x2+`","seq":2}`, time.Second)
	r1 := rbMock("read-1", "GET", "http://inv/items/"+x1, "", 200, `{"id":"`+x1+`","seq":1}`, 2*time.Second)
	r2 := rbMock("read-2", "GET", "http://inv/items/"+x2, "", 200, `{"id":"`+x2+`","seq":2}`, 3*time.Second)
	db := &taking{mockMemDb: &mockMemDb{mocks: []*models.Mock{c1, c2, r1, r2}, updateUnFilteredReturn: true}, take: "create-1"}
	h := newHTTP()
	if got := rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+rbLive+`","name":"widget"}`)).Spec.HTTPResp.Body; got != `{"id":"`+rbLive+`","seq":2}` {
		t.Fatalf("the create was answered %s: want the second recording, with its own id", got)
	}
	if got := db.bound(x1, x2); len(got) != 1 || got[x2] != rbLive {
		t.Fatalf("bound %v: want only the recording that answered", got)
	}
	assertServes(t, rbMatch(t, h, db, getReq("/items/"+rbLive)), `"seq":2`, `"seq":1`)
}

// taking is a store from which another connection takes the mock named take
// at the moment this request tries to use it.
type taking struct {
	*mockMemDb
	take  string
	taken bool
}

func (s *taking) GetSessionMocks() ([]*models.Mock, error) {
	var out []*models.Mock
	for _, m := range s.mocks {
		if !(s.taken && m.Name == s.take) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *taking) UpdateUnFilteredMock(old, updated *models.Mock) bool {
	if old.Name == s.take {
		s.taken = true
		return false
	}
	return s.mockMemDb.UpdateUnFilteredMock(old, updated)
}

// A key the app mints and looks up before it makes anything with it — a HEAD
// that finds no object, then the PUT that makes it — is bound at the write: a
// lookup introduces nothing. The read-back by this run's key is followed.
//
// A lookup that FOUND what it asked for (answered 200) says the key existed
// before: the key is a constant, and a later write with a fresh key in its
// place binds nothing.
func TestRebind_AKeyLookedUpBeforeItIsMadeIsBoundAtTheWrite(t *testing.T) {
	const k = "11111111-1111-4111-8111-111111111111"
	for name, c := range map[string]struct {
		status int
		body   string
		bound  bool
	}{
		"found nothing":                 {404, ``, true},
		"found nothing, and said which": {404, `<Error><Code>NoSuchKey</Code><Key>` + k + `</Key></Error>`, true},
		"found it":                      {200, ``, false},
	} {
		head := rbMock("head", "HEAD", "http://s3/bucket/"+k, "", c.status, c.body, 0)
		put := rbMock("put", "PUT", "http://s3/bucket/"+k, `{"data":"x"}`, 200, `{"key":"`+k+`"}`, time.Second)
		get := rbMock("get", "GET", "http://s3/bucket/"+k, "", 200, `{"key":"`+k+`","data":"x"}`, 2*time.Second)
		other := rbMock("other", "POST", "http://s3/other", `{"id":"`+rbOther+`"}`, 201, `{}`, 3*time.Second) // so the set is followed
		db := &mockMemDb{mocks: []*models.Mock{head, put, get, other}, updateUnFilteredReturn: true}
		h := newHTTP()
		hd := getReq("/bucket/" + rbLive)
		hd.method = "HEAD"
		rbMatch(t, h, db, hd)
		rbMatch(t, h, db, jsonReq("PUT", "/bucket/"+rbLive, `{"data":"x"}`))
		read := rbMatch(t, h, db, getReq("/bucket/"+rbLive))
		if got := db.bound(k); (got[k] == rbLive) != c.bound {
			t.Fatalf("%s: bound %v", name, got)
		}
		if c.bound {
			assertServes(t, read, rbLive, k)
		} else {
			assertServes(t, read, k, rbLive)
		}
	}
}

// What a lookup found, it did not make, and what a dependency handed over in a
// lookup's answer is the dependency's: neither gets a creator at a later
// write.
//   - A 404 that names a request id: the id is the dependency's, first carried
//     by that answer. An app that files a ticket naming another lookup's
//     request id is answered as without rebinding, nothing bound.
//   - A 304 found the user (it is unchanged): a later PUT of a fresh id to the
//     same path is not that user's create, and the user's id stays its own.
func TestRebind_WhatALookupFoundOrWasHandedIsNotBound(t *testing.T) {
	const k1, k2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const r1, r2 = "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	t.Run("a request id in a 404", func(t *testing.T) {
		run := func(on bool) (string, map[string]string) {
			g1 := rbMock("get-1", "GET", "http://inv/items/"+k1, "", 404, `{"requestId":"`+r1+`"}`, 0)
			g2 := rbMock("get-2", "GET", "http://inv/items/"+k2, "", 404, `{"requestId":"`+r2+`"}`, time.Second)
			ticket := rbMock("ticket", "POST", "http://inv/tickets", `{"ref":"`+r1+`"}`, 201, `{"ref":"`+r1+`","state":"open"}`, 2*time.Second)
			h, db := newHTTP(), &mockMemDb{mocks: []*models.Mock{g1, g2, ticket}, updateUnFilteredReturn: true, noRebind: !on}
			rbMatch(t, h, db, getReq("/items/"+k1))
			rbMatch(t, h, db, getReq("/items/"+k2))
			body := rbMatch(t, h, db, jsonReq("POST", "/tickets", `{"ref":"`+r2+`"}`)).Spec.HTTPResp.Body
			return body, db.bound(r1, r2)
		}
		on, bound := run(true)
		if off, _ := run(false); on != off || len(bound) != 0 {
			t.Fatalf("answered %s (bound %v) with rebinding, %s without", on, bound, off)
		}
	})
	t.Run("a user a 304 found", func(t *testing.T) {
		const fixture = k1
		look := rbMock("look", "GET", "http://dir/users/"+fixture, "", 304, ``, 0)
		update := rbMock("update", "PUT", "http://dir/users/"+fixture, `{"name":"fixture-user"}`, 200, `{"id":"`+fixture+`"}`, time.Second)
		whoami := rbMock("whoami", "GET", "http://dir/whoami", "", 200, `{"user":"`+fixture+`"}`, 2*time.Second)
		other := rbMock("other", "POST", "http://dir/others", `{"id":"`+rbOther+`"}`, 201, `{}`, 3*time.Second) // so the set is followed
		h, db := newHTTP(), &mockMemDb{mocks: []*models.Mock{look, update, whoami, other}, updateUnFilteredReturn: true}
		rbMatch(t, h, db, jsonReq("PUT", "/users/"+rbLive, `{"name":"fixture-user"}`))
		assertServes(t, rbMatch(t, h, db, getReq("/whoami")), fixture, rbLive)
		if got := db.bound(fixture); len(got) != 0 {
			t.Fatalf("the user a lookup found was bound: %v", got)
		}
	})
}

// A request that carries no id of this run is matched exactly as without
// rebinding, in a set that is followed: two polls that differ from the
// request only in a field that is noise are told apart by the lenient passes,
// as always — not by which comes first in the pool.
func TestRebind_ARequestWithNoIDOfThisRunIsMatchedAsWithoutRebinding(t *testing.T) {
	run := func(on bool) string {
		create := rbMock("create", "POST", "http://inv/items", `{"id":"`+rbRec+`"}`, 201, `{}`, 0)
		s1 := rbMock("poll-1", "POST", "http://inv/status", `{"q":"s","at":"2026-10-07T12:00:00Z"}`, 200, `{"state":"pending"}`, time.Second)
		s2 := rbMock("poll-2", "POST", "http://inv/status", `{"q":"s","at":"2026-11-30T23:59:59Z"}`, 200, `{"state":"done"}`, 2*time.Second)
		db := &mockMemDb{mocks: []*models.Mock{create, s1, s2}, updateUnFilteredReturn: true, noRebind: !on}
		if on && db.index() == nil {
			t.Fatal("the set must be followed")
		}
		ok, stub, _, err := newHTTP().match(context.Background(), jsonReq("POST", "/status", `{"q":"s","at":"2026-11-30T23:59:58Z"}`), db, nil, map[string][]string{"at": {}}, nil, true, false, false, true, true)
		if err != nil || !ok {
			t.Fatalf("on=%v: ok=%v err=%v", on, ok, err)
		}
		return stub.Name + " " + stub.Spec.HTTPResp.Body
	}
	if on, off := run(true), run(false); on != off {
		t.Fatalf("answered %s with rebinding, %s without", on, off)
	}
}

// An id bound earlier in a URL path does not narrow what the lenient passes
// choose from for a request nothing fits. Without rebinding the exact URL pass
// finds nothing and the dynamic-segment pass takes every recording of that
// path; a copy carrying this run's id would match its own recording's URL on
// the exact pass, and leave the lenient passes only that one — another
// answer, other noise learned, or no answer where there is one.
func TestRebind_AnIDInTheURLDoesNotNarrowTheLenientChoice(t *testing.T) {
	const x1, x2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const y1, y2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "bbbbbbbb-cccc-4ddd-9eee-ffffffffffff"
	creates := func() []*req {
		return []*req{jsonReq("POST", "/items", `{"id":"`+y1+`","n":1}`), jsonReq("POST", "/items", `{"id":"`+y2+`","n":2}`)}
	}
	for name, c := range map[string]struct{ note1, note2, sent string }{
		"another recording is closer":     {`{"text":"aa aa aa aa"}`, `{"text":"zz zz zz zz"}`, `{"text":"zz zz zz zy"}`},
		"its own recording's keys differ": {`{"text":"aa","pinned":true}`, `{"text":"bb"}`, `{"text":"cc"}`},
	} {
		mocks := func() []*models.Mock {
			return []*models.Mock{
				rbMock("c1", "POST", "http://inv/items", `{"id":"`+x1+`","n":1}`, 201, `{}`, 0),
				rbMock("c2", "POST", "http://inv/items", `{"id":"`+x2+`","n":2}`, 201, `{}`, time.Second),
				rbMock("note1", "POST", "http://inv/items/"+x1+"/notes", c.note1, 200, `{"item":"`+x1+`","note":1}`, 2*time.Second),
				rbMock("note2", "POST", "http://inv/items/"+x2+"/notes", c.note2, 200, `{"item":"`+x2+`","note":2}`, 3*time.Second),
			}
		}
		reqs := append(creates(), jsonReq("POST", "/items/"+y1+"/notes", c.sent))
		servedOn, on := detect(t, true, mocks(), reqs...)
		reqs = append(creates(), jsonReq("POST", "/items/"+y1+"/notes", c.sent))
		servedOff, off := detect(t, false, mocks(), reqs...)
		if servedOn[2] != servedOff[2] {
			t.Errorf("%s: answered %s with rebinding, %s without", name, servedOn[2], servedOff[2])
		}
		if !reflect.DeepEqual(on.noise, off.noise) {
			t.Errorf("%s: learned %v with rebinding, %v without", name, on.noise, off.noise)
		}
	}
}

// A test runner that reorders tests can send a lookup by a fresh id before the
// lookup of a constant recorded with the same shape: GET /users/<fresh> (a
// test that expects 404) before GET /users/<fixture>. A lookup makes nothing,
// so it is never taken for a create: the fresh id is not bound to the
// fixture's, and every answer — the lookup's, and what names the fixture
// elsewhere — is what it is without rebinding.
func TestRebind_ALookupOfAConstantIsNeverTakenForACreate(t *testing.T) {
	const fixture, missing, fresh = rbRec, rbOther, rbLive
	run := func(on bool) []string {
		user := rbMock("user", "GET", "http://dir/users/"+fixture, "", 200, `{"id":"`+fixture+`","name":"fixture-user"}`, 0)
		gone := rbMock("gone", "GET", "http://dir/users/"+missing, "", 404, `{"error":"no such user"}`, time.Second)
		whoami := rbMock("whoami", "GET", "http://dir/whoami", "", 200, `{"user":"`+fixture+`"}`, 2*time.Second)
		// A create the replay follows, so the set is followed at all.
		create := rbMock("create", "POST", "http://dir/orders", `{"id":"11111111-1111-4111-8111-111111111111"}`, 201, `{}`, 3*time.Second)
		h, db := newHTTP(), &mockMemDb{mocks: []*models.Mock{user, gone, whoami, create}, updateUnFilteredReturn: true, noRebind: !on}
		var out []string
		for _, path := range []string{"/users/" + fresh, "/whoami", "/users/" + fixture, "/whoami"} {
			out = append(out, rbMatch(t, h, db, getReq(path)).Spec.HTTPResp.Body)
		}
		if got := db.bound(fixture, missing); len(got) != 0 {
			t.Fatalf("a lookup bound %v", got)
		}
		return out
	}
	if on, off := run(true), run(false); !slices.Equal(on, off) {
		t.Fatalf("answered %v, want what it is without rebinding: %v", on, off)
	}
}

// A request that names a bound id both ways — the id this run made in one
// place, the recorded one in another — speaks of the recorded entity (an app
// handed the recorded id by a call that was answered as recorded sends it
// on). It is matched as it is without rebinding: no copy carrying this run's
// id stands in for the recording it names.
func TestRebind_ARequestThatNamesAnIDAsRecordedIsMatchedAsWithoutRebinding(t *testing.T) {
	const x1, x2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const y1, y2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "bbbbbbbb-cccc-4ddd-9eee-ffffffffffff"
	for _, autoURL := range []bool{true, false} {
		stage := func(on bool) (*HTTP, *mockMemDb) {
			c1 := rbMock("c1", "POST", "http://inv/items", `{"id":"`+x1+`","n":1}`, 201, `{}`, 0)
			c2 := rbMock("c2", "POST", "http://inv/items", `{"id":"`+x2+`","n":2}`, 201, `{}`, time.Second)
			g1 := rbMock("get1", "GET", "http://inv/items/"+x1, "", 200, `{"n":1}`, 2*time.Second)
			g2 := rbMock("get2", "GET", "http://inv/items/"+x2, "", 200, `{"n":2}`, 3*time.Second)
			h, db := newHTTP(), &mockMemDb{mocks: []*models.Mock{c1, c2, g1, g2}, updateUnFilteredReturn: true, noRebind: !on}
			rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+y1+`","n":1}`))
			rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+y2+`","n":2}`))
			return h, db
		}
		in := func() *req {
			in := getReq("/items/" + x1)      // the recorded id in the path
			in.header.Set("X-Trace-Item", y1) // this run's id for it in a header
			return in
		}
		h, db := stage(true)
		off, offDB := stage(false)
		ok, got, _, err := h.match(context.Background(), in(), db, nil, nil, nil, autoURL, false, false, true, true)
		okOff, want, _, _ := off.match(context.Background(), in(), offDB, nil, nil, nil, autoURL, false, false, true, true)
		if err != nil || ok != okOff || !ok || got.Name != want.Name || got.Spec.HTTPResp.Body != want.Spec.HTTPResp.Body {
			t.Fatalf("autoURL=%v: answered ok=%v %v; without rebinding ok=%v %v (err %v)", autoURL, ok, got, okOff, want, err)
		}
		if got.Name != "get1" {
			t.Fatalf("autoURL=%v: answered from %s", autoURL, got.Name)
		}
	}
}

// With url-noise that lets the id segment of a path vary, every recording of
// the endpoint matches every id. The recording that carries this run's id is
// still the one that answers a read-back by it.
func TestRebind_URLNoiseOverAnIDDoesNotHideTheRecordingThatCarriesIt(t *testing.T) {
	const x1, x2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const y1, y2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "bbbbbbbb-cccc-4ddd-9eee-ffffffffffff"
	urlNoise := []string{`/items/[0-9a-f-]{36}`}
	h := newHTTP()
	c1 := rbMock("c1", "POST", "http://inv/items", `{"id":"`+x1+`","n":1}`, 201, `{}`, 0)
	c2 := rbMock("c2", "POST", "http://inv/items", `{"id":"`+x2+`","n":2}`, 201, `{}`, time.Second)
	g1 := rbMock("get1", "GET", "http://inv/items/"+x1, "", 200, `{"id":"`+x1+`","n":1}`, 2*time.Second)
	g2 := rbMock("get2", "GET", "http://inv/items/"+x2, "", 200, `{"id":"`+x2+`","n":2}`, 3*time.Second)
	db := &mockMemDb{mocks: []*models.Mock{c1, c2, g1, g2}, updateUnFilteredReturn: true}
	send := func(in *req) *models.Mock {
		ok, stub, _, err := h.match(context.Background(), in, db, nil, nil, urlNoise, true, false, false, true, true)
		if err != nil || !ok {
			t.Fatalf("%s %s: ok=%v err=%v", in.method, in.url.Path, ok, err)
		}
		return stub
	}
	send(jsonReq("POST", "/items", `{"id":"`+y1+`","n":1}`))
	send(jsonReq("POST", "/items", `{"id":"`+y2+`","n":2}`))
	for id, want := range map[string]string{y2: "get2", y1: "get1"} {
		stub := send(getReq("/items/" + id))
		if stub.Name != want || !strings.Contains(stub.Spec.HTTPResp.Body, id) {
			t.Fatalf("read-back of %s answered from %s: %s", id, stub.Name, stub.Spec.HTTPResp.Body)
		}
	}
}

// An app that mints nothing sends every id as recorded, and is answered
// exactly as it is without rebinding — also with the stateful cursor off,
// where a repeated request rotates through its recordings: no creator is
// moved ahead of a later identical recording for a request that brings no new
// id.
func TestRebind_AnAppThatMintsNothingIsAnsweredAsWithoutRebinding(t *testing.T) {
	run := func(on bool) string {
		h := newHTTP()
		var pool []*models.Mock
		for i, status := range []int{201, 409, 409} {
			m := rbMock(fmt.Sprintf("r%d", i+1), "POST", "http://inv/items", `{"id":"`+rbRec+`","name":"widget"}`, status, `{"status":`+strconv.Itoa(status)+`}`, time.Duration(i)*time.Second)
			m.TestModeInfo.Lifetime = models.LifetimeSession
			pool = append(pool, m)
		}
		// The pool hands them over newest first: the recording that first
		// carried the id is not the first in line.
		slices.Reverse(pool)
		db := &rotating{&mockMemDb{mocks: pool, updateUnFilteredReturn: true, noRebind: !on}}
		var served []string
		for i := 0; i < 6; i++ {
			ok, stub, _, err := h.match(context.Background(), jsonReq("POST", "/items", `{"id":"`+rbRec+`","name":"widget"}`), db, nil, nil, nil, true, false, false, false, true)
			if err != nil || !ok {
				t.Fatalf("no match: ok=%v err=%v", ok, err)
			}
			served = append(served, stub.Name)
		}
		return strings.Join(served, ",")
	}
	if on, off := run(true), run(false); on != off {
		t.Fatalf("answered %s with rebinding, %s without", on, off)
	}
}

// What is consumed or updated is the POOLED recording, never the copy that
// carried this run's id through the match: the store finds a mock by what it
// was staged with.
func TestRebind_TheRecordingIsUpdatedNotTheCopyThatMatched(t *testing.T) {
	h := newHTTP()
	create, get, _ := rbFlow()
	get.Spec.HTTPResp.Header = map[string]string{"Location": "/items/" + rbRec}
	db := &mockMemDb{mocks: []*models.Mock{create, get}, updateUnFilteredReturn: true}
	createLive(t, h, db)
	stub := rbMatch(t, h, db, getReq("/items/"+rbLive))
	if db.updateUnFilteredMockOld != get {
		t.Fatalf("the store was asked to update %p (%s), not the pooled recording %p", db.updateUnFilteredMockOld, db.updateUnFilteredMockOld.Spec.HTTPReq.URL, get)
	}
	if strings.Contains(db.updateUnFilteredMockNew.Spec.HTTPReq.URL, rbLive) {
		t.Fatalf("the pooled recording was rewritten with this run's id: %s", db.updateUnFilteredMockNew.Spec.HTTPReq.URL)
	}
	// The answer names this run's id in its headers too.
	if got := stub.Spec.HTTPResp.Header["Location"]; got != "/items/"+rbLive {
		t.Fatalf("Location: %s", got)
	}
	if get.Spec.HTTPResp.Header["Location"] != "/items/"+rbRec {
		t.Fatal("the pooled recording's own answer was changed")
	}
}

// A copy carries this run's id wherever the recording named the recorded one:
// in a header and in the recorded query parameters too, so a request that
// sends it there is the copy's, exactly.
func TestRebind_ACopyCarriesTheIDInHeadersAndQueryToo(t *testing.T) {
	h := newHTTP()
	create, _, _ := rbFlow()
	find := rbMock("find", "GET", "http://inv/find?id="+rbRec, "", 200, `{"id":"`+rbRec+`"}`, time.Second)
	find.Spec.HTTPReq.URLParams = map[string]string{"id": rbRec}
	find.Spec.HTTPReq.Header = map[string]string{"X-Item": rbRec}
	db := &mockMemDb{mocks: []*models.Mock{create, find}, updateUnFilteredReturn: true}
	createLive(t, h, db)
	in := getReq("/find")
	in.url.RawQuery = "id=" + rbLive
	in.header.Set("X-Item", rbLive)
	r := rbOf(db, in)
	copies := r.candidates(db.mocks)
	got := copies[1].Spec.HTTPReq
	if got == find.Spec.HTTPReq || got.URLParams["id"] != rbLive || got.Header["X-Item"] != rbLive || !strings.Contains(got.URL, rbLive) {
		t.Fatalf("the copy of find: url %s params %v header %v", got.URL, got.URLParams, got.Header)
	}
	if find.Spec.HTTPReq.URLParams["id"] != rbRec || find.Spec.HTTPReq.Header["X-Item"] != rbRec {
		t.Fatal("the pooled recording was changed")
	}
	assertServes(t, rbMatch(t, h, db, in), rbLive, rbRec)
}

// A request stored in an encoding the recorder does not decode cannot be
// read, and may be the first to carry an id: the set is not followed.
func TestRebind_ASetWithAnUnreadableRequestIsLeftAlone(t *testing.T) {
	h := newHTTP()
	create, get, _ := rbFlow()
	packed := rbMock("packed", "POST", "http://inv/bulk", "\x78\x9c\x4b\xcb\xcf\x07\x00\x02\x82\x01\x45", 200, `{}`, -time.Second)
	packed.Spec.HTTPReq.Header["Content-Encoding"] = "deflate"
	db := &mockMemDb{mocks: []*models.Mock{packed, create, get}, updateUnFilteredReturn: true}
	if db.Bindings() != nil {
		t.Fatal("a set with a request that cannot be read must not be followed")
	}
	assertServes(t, createLive(t, h, db), rbRec, rbLive)
	if _, _, readable := httpRequestValues(packed); readable {
		t.Fatal("the tokenizer must report the request unreadable")
	}
	packed.Spec.HTTPReq.Header["Content-Encoding"] = "gzip" // one the recorder decodes: stored readable
	if _, _, readable := httpRequestValues(packed); !readable {
		t.Fatal("a decoded encoding is readable")
	}
}

// A constant's recording is not taken for a create. An endpoint has two
// recordings of one shape: the first with an id the app has shown to be a
// constant (it sent it as recorded), the second a create with an id the app
// mints. A live create with a new id is the second one's — the constant's
// recording must not be the one it is lined up with, refused by, and answered
// from.
func TestRebind_AConstantsRecordingIsNotTakenForACreate(t *testing.T) {
	const fixture, minted = rbRec, rbOther
	h := newHTTP()
	seed := rbMock("seed", "POST", "http://inv/items", `{"id":"`+fixture+`","kind":"item"}`, 200, `{"id":"`+fixture+`","seeded":true}`, 0)
	create := rbMock("create", "POST", "http://inv/items", `{"id":"`+minted+`","kind":"item"}`, 201, `{"id":"`+minted+`","seeded":false}`, time.Second)
	touch := rbMock("touch", "GET", "http://inv/items/"+fixture+"/touch", "", 200, `{}`, 2*time.Second)
	db := &mockMemDb{mocks: []*models.Mock{seed, create, touch}, updateUnFilteredReturn: true}

	rbMatch(t, h, db, getReq("/items/"+fixture+"/touch")) // the app names the fixture as recorded
	stub := rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+rbLive+`","kind":"item"}`))
	if stub.Name != "create" || stub.Spec.HTTPResp.Body != `{"id":"`+rbLive+`","seeded":false}` {
		t.Fatalf("the create was answered from %s: %s", stub.Name, stub.Spec.HTTPResp.Body)
	}
	if got := db.bound(fixture, minted); got[minted] != rbLive || got[fixture] != "" {
		t.Fatalf("bound %v, want the minted id alone", got)
	}
}

// Only the call that introduced an id, made again exactly as recorded, says
// its binding was a mistake. The recorded id turning up in another call —
// here in a call to the same endpoint that is not the recorded one — is an
// app passing on what an answer served as recorded handed it, and the binding
// stands.
func TestRebind_ARelayedRecordedIDDoesNotDisownItsBinding(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	h := newHTTP()
	h.Logger = zap.New(core)
	create, _, list := rbFlow() // create: POST /items {"id": rbRec, "name": "widget"}
	db := &mockMemDb{mocks: []*models.Mock{create, list}, updateUnFilteredReturn: true}
	createLive(t, h, db)

	rbMatch(t, h, db, jsonReq("POST", "/items", `{"id":"`+rbRec+`","name":"gadget"}`)) // the recorded id, in another create
	if n := logs.FilterMessageSnippet("constant after all").Len(); n != 0 {
		t.Fatalf("a call that is not the recorded one disowned the binding: %v", logs.All())
	}
	assertServes(t, rbMatch(t, h, db, getReq("/items")), rbLive, rbRec) // the list still names this run's id
}

// A call that misses is reported against the recordings, not against the
// copies a match made of them with this run's ids: the mismatch report shows a
// user what was recorded, and it is read next to mocks that name the recorded
// id.
func TestRebind_AMissIsReportedAgainstTheRecordings(t *testing.T) {
	h := newHTTP()
	create, _, _ := rbFlow()
	update := rbMock("update", "PUT", "http://inv/items/"+rbRec, `{"name":"gadget"}`, 200, `{"id":"`+rbRec+`"}`, time.Second)
	db := &mockMemDb{mocks: []*models.Mock{create, update}, updateUnFilteredReturn: true}
	createLive(t, h, db)

	// The update's URL with this run's id, and a body of another shape: the
	// copy of the recording passes the schema match, and no body match.
	in := jsonReq("PUT", "/items/"+rbLive, `{"price":3,"currency":"EUR"}`)
	ok, _, diag, err := h.match(context.Background(), in, db, nil, nil, nil, true, false, false, true, true)
	if err != nil || ok || diag == nil {
		t.Fatalf("the update with another body must miss: ok=%v diag=%v err=%v", ok, diag, err)
	}
	if len(diag.schemaMatched) != 1 {
		t.Fatalf("the miss must name the one recording whose shape the call has: %d", len(diag.schemaMatched))
	}
	if got := diag.schemaMatched[0]; got != update {
		t.Fatalf("the miss names a copy made for this request (%s), not the recording (%s)", got.Spec.HTTPReq.URL, update.Spec.HTTPReq.URL)
	}
}
