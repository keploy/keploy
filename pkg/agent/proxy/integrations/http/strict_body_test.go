package http

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// postItem is the recording name of a POST /item whose request body is body
// and whose content type is ct.
func postItem(name, ct, body string, at time.Duration) *models.Mock {
	m := httpMock(name, "POST", "/item")
	m.Spec.HTTPReq.Header = map[string]string{"Host": strictIxHost, "Content-Type": ct}
	m.Spec.HTTPReq.Body = body
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return answered(m, at, 200, `{"ok":true}`)
}

// postCall is the live POST /item of body, of content type ct.
func postCall(ct, body string) *req {
	h := http.Header{}
	h.Set("Host", strictIxHost)
	h.Set("Content-Type", ct)
	return &req{method: "POST", url: &url.URL{Path: "/item"}, header: h, body: []byte(body)}
}

// postBody is the JSON body of the i-th POST of postItems.
func postBody(i int) string { return fmt.Sprintf(`{"id":"b%05d","r":%d}`, i, i) }

// postItems are size per-test POST /item recordings, each of its own body.
func postItems(size int) []*models.Mock {
	mocks := make([]*models.Mock, size)
	for i := range mocks {
		mocks[i] = postItem("post-"+strconv.Itoa(i), "application/json", postBody(i), time.Duration(i)*time.Millisecond)
	}
	return mocks
}

// A strict POST compares only the recordings of its path whose body may be
// its own: at 100 recordings of POST /item and at 2000 alike, a call looks at
// one recording (each call compared every unanswered recording of the path,
// so a replay of n calls cost n²), and the bucket is filed by its bodies
// once. Where the replay may declare a source of a recording, which may let
// a body field change, the call compares the whole bucket, as before.
func TestAStrictPOSTLooksOnlyAtTheRecordingsOfItsBody(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	for _, size := range []int{100, 2000} {
		for _, declares := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/declares=%v", size, declares), func(t *testing.T) {
				h := newHTTP()
				db := newOwnGenDb(postItems(size), nil)
				db.declares = declares
				for i := 0; i < 2; i++ {
					strictMatchOf(t, h, postCall("application/json", postBody(i)), db, db, nil)
				}
				filings := strictBodyFilings.Load()
				var served *models.Mock
				visits := strictVisitsOf(func() {
					served = strictMatchOf(t, h, postCall("application/json", postBody(size/2)), db, db, nil)
				})
				if served == nil || served.Name != fmt.Sprintf("post-%d", size/2) {
					t.Fatalf("served %v; want post-%d", served, size/2)
				}
				want := int64(1)
				if declares {
					want = int64(size - 2) // the two calls before took theirs
				}
				if visits != want {
					t.Fatalf("a POST that is one recording of %d looked at %d; want %d", size, visits, want)
				}
				if got := strictBodyFilings.Load(); got != filings {
					t.Fatalf("a call filed its bucket by its bodies again (%d filings)", got-filings)
				}
			})
		}
	}
}

// A call of a path no recording has files no bucket by its bodies.
func TestAStrictCallOfAnUnrecordedPathFilesNoBodies(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	h := newHTTP()
	db := newOwnGenDb(postItems(4), nil)
	strictMatchOf(t, h, postCall("application/json", postBody(0)), db, db, nil)
	filings := strictBodyFilings.Load()
	miss := postCall("application/json", postBody(1))
	miss.url.Path = "/other"
	if served := strictMatchOf(t, h, miss, db, db, nil); served != nil {
		t.Fatalf("a call of /other served %s", served.Name)
	}
	if got := strictBodyFilings.Load(); got != filings {
		t.Fatalf("a call of an unrecorded path filed %d buckets by their bodies; want none", got-filings)
	}
}

// A call whose body is a recording's in another form, which the strict
// comparison holds the same (members in another order, a number written
// another way, -0 for 0), is served that recording through the index.
func TestAStrictPOSTIsServedTheRecordingOfItsBodyInAnotherForm(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	for _, c := range []struct{ rec, live string }{
		{`{"a":1.50,"b":[1,2],"c":{"x":null,"y":true}}`, `{"c":{"y":true,"x":null},"b":[1, 2],"a":1.5}`},
		{`{"n":-0}`, `{"n":0}`},
		{`{"n":1e2}`, `{"n":100.0}`},
		{`{"n":1e400}`, `{"n":10e399}`},
	} {
		t.Run(c.live, func(t *testing.T) {
			h := newHTTP()
			mocks := append(postItems(8), postItem("form", "application/json", c.rec, 9*time.Millisecond))
			db := newOwnGenDb(mocks, nil)
			if served := strictMatchOf(t, h, postCall("application/json", c.live), db, db, nil); served == nil || served.Name != "form" {
				t.Fatalf("served %v; want form", served)
			}
		})
	}
}

// A recording whose body the strict comparison does not compare plainly is
// compared with every call of its path, whatever its body: one that learned
// noise of its body, one with a value the obfuscator redacted, a form, and,
// where keploy.yml declares noise of request bodies, every recording. Each
// is served a call whose body differs from it only where it may, in a bucket
// of recordings of other bodies.
func TestAStrictPOSTOfARecordingThatComparesItsBodyOtherwiseIsServed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	learned := postItem("it", "application/json", `{"id":"x1","t":1}`, 9*time.Millisecond)
	learned.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
	redacted := postItem("it", "application/json", `{"tok":"REDACTED_abc","t":1}`, 9*time.Millisecond)
	redacted.Noise = []string{`^REDACTED_abc$`}
	form := postItem("it", "application/x-www-form-urlencoded", "a=1&b=2", 9*time.Millisecond)
	declared := postItem("it", "application/json", `{"id":"x1"}`, 9*time.Millisecond)
	for _, c := range []struct {
		name      string
		rec       *models.Mock
		ct, live  string
		bodyNoise map[string][]string
	}{
		{"learned noise", learned, "application/json", `{"id":"zz9","t":1}`, nil},
		{"redacted", redacted, "application/json", `{"tok":"secret","t":1}`, nil},
		{"form", form, "application/x-www-form-urlencoded", "b=2&a=1", nil},
		{"keploy.yml body noise", declared, "application/json", `{"id":"x1","extra":7}`, map[string][]string{"extra": {}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHTTP()
			db := newOwnGenDb(append(postItems(8), c.rec), nil)
			ok, served, _, err := h.matchOf(context.Background(), postCall(c.ct, c.live), db, db, nil, c.bodyNoise, nil, true, false, false, true, true)
			if err != nil || !ok || served.Name != "it" {
				t.Fatalf("served %v (ok %v, err %v); want it", served, ok, err)
			}
		})
	}
}

// Of two recordings a call fits, one whose body compares plainly and one
// whose body learned noise, the one first in the pool is served through the
// index, as a read serves it: the bucket's recordings of the call's print and
// the others are read in the pool's order.
func TestAStrictPOSTFitsBothFiledAndUnfiledRecordingsInThePoolsOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	learned := postItem("learned", "application/json", `{"id":"x1","t":1}`, 0)
	learned.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
	plain := postItem("plain", "application/json", `{"id":"x2","t":1}`, time.Millisecond)
	db := newOwnGenDb([]*models.Mock{learned, plain}, nil)
	if served := strictMatchOf(t, newHTTP(), postCall("application/json", `{"t":1,"id":"x2"}`), db, db, nil); served == nil || served.Name != "learned" {
		t.Fatalf("served %v; want learned, the first the call fits", served)
	}
}

// plainPrint files a recording by its body's print only where the strict
// comparison compares that body plainly.
func TestPlainPrintFilesOnlyABodyThatComparesPlainly(t *testing.T) {
	jsonMock := func() *models.Mock { return postItem("m", "application/json", `{"id":"x1","t":1}`, 0) }
	learned := jsonMock()
	learned.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
	clocked := jsonMock()
	clocked.Spec.Clocks = map[string]models.ClockOffset{"body.t": {}}
	headerClock := jsonMock()
	headerClock.Spec.Clocks = map[string]models.ClockOffset{"header.date": {}}
	redactedValue := jsonMock()
	redactedValue.Spec.HTTPReq.Body = `{"id":"REDACTED_abc"}`
	redactedValue.Noise = []string{`^REDACTED_abc$`}
	otherNoise := jsonMock()
	otherNoise.Noise = []string{`^REDACTED_abc$`}
	redactedBody := postItem("m", "text/plain", "REDACTED_abc", 0)
	redactedBody.Noise = []string{`^REDACTED_abc$`}
	for _, c := range []struct {
		name string
		m    *models.Mock
		want bool
	}{
		{"json", jsonMock(), true},
		{"text", postItem("m", "text/plain", "hello", 0), true},
		{"no body", postItem("m", "", "", 0), true},
		{"header clock", headerClock, true},
		{"noise of other values", otherNoise, true},
		{"learned noise", learned, false},
		{"body clock", clocked, false},
		{"redacted value", redactedValue, false},
		{"redacted body", redactedBody, false},
		{"form", postItem("m", "application/x-www-form-urlencoded; charset=utf-8", "a=1", 0), false},
		{"multipart", postItem("m", "multipart/form-data; boundary=x", "--x--", 0), false},
	} {
		if _, ok := plainPrint(c.m); ok != c.want {
			t.Errorf("%s: plainPrint ok %v; want %v", c.name, ok, c.want)
		}
	}
}

// bodyPrint writes bodies the strict comparison holds the same alike, every
// time, and bodies of other values, other members or another order of
// elements apart.
func TestBodyPrintIsOneForBodiesComparedTheSame(t *testing.T) {
	printOf := func(b string) uint64 {
		t.Helper()
		sum, ok := bodyPrint([]byte(b), nil)
		if !ok {
			t.Fatalf("bodyPrint(%s) not ok", b)
		}
		return sum
	}
	many := `{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8}`
	first := printOf(many)
	for i := 0; i < 32; i++ {
		if printOf(many) != first {
			t.Fatalf("one body printed two ways")
		}
	}
	for _, same := range [][2]string{
		{`{"a":1,"b":"x"}`, `{"b":"x","a":1}`},
		{`{"n":-0.0}`, `{"n":0}`},
		{`{"n":1.50}`, `{"n":1.5}`},
		{`{"n":1e400}`, `{"n":10E399}`},
		{`[true,false,null]`, ` [true, false, null] `},
		{`not json`, `not json`},
	} {
		if printOf(same[0]) != printOf(same[1]) {
			t.Errorf("%s and %s printed apart", same[0], same[1])
		}
	}
	for _, apart := range [][2]string{
		{`{"a":1}`, `{"a":2}`},
		{`{"a":1}`, `{"b":1}`},
		{`{"a":1}`, `{"a":"1"}`},
		{`[1,2]`, `[2,1]`},
		{`[[1],[2]]`, `[[1,2]]`},
		{`{"a":true}`, `{"a":false}`},
		{`{"a":null}`, `{"a":false}`},
		{`{"a":{}}`, `{"a":[]}`},
		{`"ab"`, `"ba"`},
		{`not json`, `not json!`},
		{`1`, `"1"`},
	} {
		if printOf(apart[0]) == printOf(apart[1]) {
			t.Errorf("%s and %s printed alike", apart[0], apart[1])
		}
	}
	if _, ok := bodyPrint([]byte(`{"k":["REDACTED_abc"]}`), func(s string) bool { return s == "REDACTED_abc" }); ok {
		t.Error("a body holding a redacted value printed")
	}
}
