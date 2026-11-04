package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A repeated read (exact_repeat_read.go): a read a test's mapping names, the
// test having recorded it at least twice with one answer, is answered once
// more than recorded and is not charged one recording left unmade. Every
// other call keeps exact serving's count: one more misses, one fewer is
// charged.

// readCall is a request the app sends: method on path of localhost:8083,
// with body when it has one.
func readCall(method, path, body string) string {
	r := method + " " + path + " HTTP/1.1\r\nHost: localhost:8083\r\n"
	if body != "" {
		r += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	return r + "\r\n" + body
}

// readMock is a recording of readCall(method, path, body) at at, answered
// 200 with answer.
func readMock(name string, at time.Time, method, path, body, answer string) *models.Mock {
	header := map[string]string{"Host": "localhost:8083"}
	if body != "" {
		header["Content-Type"] = "application/json"
	}
	return &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"type": models.HTTPClient, "operation": method, models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
		HTTPReq:  &models.HTTPReq{Method: models.Method(method), URL: "http://localhost:8083" + path, Header: header, Body: body},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{
			"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(answer)),
		}, Body: answer},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
	}, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
}

// repeatCase is one test of the rows' set: it recorded call n times, its
// mapping naming the recordings repeated read read ("" names none), and its
// replay sends it sends times. answers are the recordings' answers, one per
// recording; the first's for every recording past their count.
type repeatCase struct {
	name, method, path, body, read string
	answers                        []string
	recorded, sends                int
	// missed are the sends (by index) that must miss; unmade the
	// recordings (by index) its end must charge as never made.
	missed, unmade []int
	// edit, when set, changes recording i before it is staged.
	edit func(i int, mk *models.Mock)
}

// repeatRun records every case into one set, one test each, serves the set
// as a replay does (corsRun) and checks each test's misses and unmade
// recordings.
func repeatRun(t *testing.T, cases []repeatCase) {
	t.Helper()
	repeatRunLogged(t, zap.NewNop(), cases)
}

// repeatRunLogged is repeatRun with the agent logging to logger.
func repeatRunLogged(t *testing.T, logger *zap.Logger, cases []repeatCase) {
	t.Helper()
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	var set []*models.Mock
	var tests []corsTest
	names := map[string][]string{}
	at := t0
	for _, c := range cases {
		var mapping []*models.Mock
		for i := 0; i < c.recorded; i++ {
			answer := c.answers[0]
			if i < len(c.answers) {
				answer = c.answers[i]
			}
			mk := readMock(fmt.Sprintf("%s-%d", c.name, i), at, c.method, c.path, c.body, answer)
			mk.TestModeInfo.RepeatRead = c.read
			if c.edit != nil {
				c.edit(i, mk)
			}
			at = at.Add(time.Second)
			mapping = append(mapping, mk)
			names[c.name] = append(names[c.name], mk.Name)
		}
		set = append(set, mapping...)
		sends := make([]string, c.sends)
		for i := range sends {
			sends[i] = readCall(c.method, c.path, c.body)
		}
		tests = append(tests, corsTest{name: c.name, mapping: mapping, sends: sends})
	}
	missed, unconsumed := corsRunLogged(t, logger, set, tests)
	for _, c := range cases {
		var unmade []string
		for _, i := range c.unmade {
			unmade = append(unmade, names[c.name][i])
		}
		if got := missed[c.name]; !reflect.DeepEqual(got, c.missed) {
			t.Errorf("%s: recorded %d, sent %d: missed %v; want %v", c.name, c.recorded, c.sends, got, c.missed)
		}
		if got := unconsumed[c.name]; !reflect.DeepEqual(got, unmade) {
			t.Errorf("%s: recorded %d, sent %d: never made %v; want %v", c.name, c.recorded, c.sends, got, unmade)
		}
	}
}

const (
	onboarding = `{"message":{"steps":[]}}`
	formScript = `{"message":[{"name":"CRM Deal-Form","script":""}]}`
)

// The Frappe CRM shapes: a read the mapping names, recorded with one answer,
// sent once more than recorded (5 then 6 get_onboarding_status, 3 then 4 and
// 8 then 9 CRM Form Script get_list) or once fewer (an unmade get_list
// repeat): each passes, answered and charged nothing.
func TestARepeatedReadIsAnsweredOnceMoreAndExcusedOnceFewer(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const listBody = `{"doctype":"CRM Form Script","filters":{"dt":"CRM Deal"}}`
	repeatRun(t, []repeatCase{
		{name: "deal", method: "POST", path: "/api/method/get_onboarding_status", read: "onboarding", answers: []string{onboarding}, recorded: 5, sends: 6},
		{name: "convert", method: "POST", path: "/api/method/frappe.client.get_list", body: listBody, read: "form-script", answers: []string{formScript}, recorded: 3, sends: 4},
		{name: "email", method: "POST", path: "/api/method/frappe.client.get_list", body: listBody, read: "form-script", answers: []string{formScript}, recorded: 8, sends: 9},
		{name: "unmade", method: "POST", path: "/api/method/frappe.client.get_list", body: listBody, read: "form-script", answers: []string{formScript}, recorded: 4, sends: 3},
		{name: "get", method: "GET", path: "/api/resource/CRM%20Deal", read: "deals", answers: []string{`{"data":[]}`}, recorded: 2, sends: 3},
		// the proof is the recordings, whatever the method
		{name: "put", method: "PUT", path: "/api/doc", body: `{"x":1}`, read: "doc", answers: []string{`{"ok":true}`}, recorded: 2, sends: 3},
		{name: "delete-fewer", method: "DELETE", path: "/api/doc/1", read: "del", answers: []string{`{}`}, recorded: 2, sends: 1},
		// answers a second apart: their Date, and a cookie's Expires date
		// beside its Max-Age, are written from the time they answered
		{name: "dated", method: "POST", path: "/api/method/get_onboarding_status", read: "dated", answers: []string{onboarding}, recorded: 3, sends: 4,
			edit: func(i int, mk *models.Mock) {
				at := time.Date(2026, 11, 3, 20, 45, 17+i, 0, time.UTC)
				mk.Spec.HTTPResp.Header["Date"] = at.Format(http.TimeFormat)
				mk.Spec.HTTPResp.Header["Set-Cookie"] = "sid=e7b5; Expires=" + at.Add(170*time.Hour).Format(http.TimeFormat) + "; Max-Age=612000; HttpOnly; Path=/; SameSite=Lax"
			}},
	})
}

// A replay says each read it answers once more as it answers it: one line
// naming the test and the read as the replay named it, for a read sent once
// more and for one sent twice more (whose second call more misses, unsaid).
// A read sent as often as recorded, or once fewer, says none.
func TestAReplaySaysEachReadItAnswersOnceMoreAsItAnswersIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.InfoLevel)
	const a, b = "GET /api/a, answered once more: recorded 2 times with one answer", "GET /api/b, answered once more: recorded 2 times with one answer"
	repeatRunLogged(t, zap.New(core), []repeatCase{
		{name: "more", method: "GET", path: "/api/a", read: a, answers: []string{`{"a":1}`}, recorded: 2, sends: 3},
		{name: "exact", method: "GET", path: "/api/a", read: a, answers: []string{`{"a":1}`}, recorded: 2, sends: 2},
		{name: "fewer", method: "GET", path: "/api/b", read: b, answers: []string{`{"b":1}`}, recorded: 3, sends: 2},
		{name: "twice-more", method: "GET", path: "/api/b", read: b, answers: []string{`{"b":1}`}, recorded: 2, sends: 4, missed: []int{3}},
	})
	var said []string
	for _, e := range logs.FilterMessageSnippet("answered a repeated read once more").All() {
		f := e.ContextMap()
		said = append(said, fmt.Sprintf("%v: %v (%v)", f["test"], f["read"], f["mock"]))
	}
	want := []string{"more: " + a + " (more-1)", "twice-more: " + b + " (twice-more-1)"}
	if !reflect.DeepEqual(said, want) {
		t.Fatalf("the replay said %q; want %q", said, want)
	}
}

// Only once: a named read sent two more times than recorded misses on its
// second call more, and one left unmade twice is charged both recordings.
func TestARepeatedReadIsAnsweredOnlyOnceMoreAndExcusedOnlyOnceFewer(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	repeatRun(t, []repeatCase{
		{name: "twice-more", method: "GET", path: "/api/a", read: "a", answers: []string{`{"a":1}`}, recorded: 2, sends: 4, missed: []int{3}},
		{name: "twice-fewer", method: "GET", path: "/api/b", read: "b", answers: []string{`{"b":1}`}, recorded: 3, sends: 1, unmade: []int{1, 2}},
		{name: "none-made", method: "GET", path: "/api/c", read: "c", answers: []string{`{"c":1}`}, recorded: 2, sends: 0, unmade: []int{0, 1}},
	})
}

// What the founder's rule does not allow stays as exact serving decides it,
// even when the mapping names it a repeated read: a read recorded once, a
// read whose recordings answered differently, and a repeat the mapping does
// not name.
func TestWhatIsNotARepeatedReadKeepsItsExactCount(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	repeatRun(t, []repeatCase{
		{name: "once", method: "GET", path: "/api/once", read: "once", answers: []string{`{}`}, recorded: 1, sends: 2, missed: []int{1}},
		{name: "two-answers-more", method: "GET", path: "/api/count", read: "count", answers: []string{`{"n":1}`, `{"n":2}`}, recorded: 2, sends: 3, missed: []int{2}},
		{name: "two-answers-fewer", method: "GET", path: "/api/count2", read: "count2", answers: []string{`{"n":1}`, `{"n":2}`}, recorded: 2, sends: 1, unmade: []int{1}},
		{name: "unnamed-more", method: "POST", path: "/api/method/create", body: `{"y":1}`, answers: []string{`{"name":"x"}`}, recorded: 2, sends: 3, missed: []int{2}},
		{name: "unnamed-fewer", method: "GET", path: "/api/unnamed", answers: []string{`{}`}, recorded: 2, sends: 1, unmade: []int{1}},
		{name: "status", method: "GET", path: "/api/status", read: "st", answers: []string{`{}`}, recorded: 2, sends: 3, missed: []int{2},
			edit: func(i int, mk *models.Mock) {
				if i == 1 {
					mk.Spec.HTTPResp.StatusCode = 503
				}
			}},
		// a header of the answer that differs is another answer
		{name: "etag", method: "GET", path: "/api/etag", read: "etag", answers: []string{`{}`}, recorded: 2, sends: 3, missed: []int{2},
			edit: func(i int, mk *models.Mock) { mk.Spec.HTTPResp.Header["ETag"] = []string{`"a"`, `"b"`}[i] }},
		// a cookie's Expires date with no Max-Age is what a client keeps it by
		{name: "expires", method: "GET", path: "/api/exp", read: "exp", answers: []string{`{}`}, recorded: 2, sends: 1, unmade: []int{1},
			edit: func(i int, mk *models.Mock) {
				mk.Spec.HTTPResp.Header["Set-Cookie"] = "sid=1; Expires=" + time.Date(2026, 11, 10, 0, 0, i, 0, time.UTC).Format(http.TimeFormat) + "; Path=/"
			}},
		{name: "binary", method: "GET", path: "/api/bin", read: "bin", answers: []string{``}, recorded: 2, sends: 3, missed: []int{2},
			edit: func(i int, mk *models.Mock) { mk.Spec.HTTPResp.Binary = []string{"AAE=", "AAI="}[i] }},
		{name: "skipped", method: "GET", path: "/api/big", read: "big", answers: []string{``}, recorded: 2, sends: 3, missed: []int{2},
			edit: func(_ int, mk *models.Mock) { mk.Spec.HTTPResp.BodySkipped = true }},
		{name: "streamed", method: "GET", path: "/api/events", read: "sse", answers: []string{``}, recorded: 2, sends: 3, missed: []int{2},
			edit: func(_ int, mk *models.Mock) { mk.Spec.HTTPResp.StreamBody = []models.HTTPStreamChunk{{}} }},
		// an answer kept apart that cannot be read back is not one answer,
		// whatever the recording still carries
		{name: "unreadable", method: "GET", path: "/api/gone", read: "gone", answers: []string{`{}`}, recorded: 2, sends: 1, unmade: []int{1},
			edit: func(i int, mk *models.Mock) {
				if i == 1 {
					mk.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
						return nil, nil, errors.New("disk mocks: read back")
					})
				}
			}},
		// a recording of another protocol carrying the same request is no
		// HTTP read
		{name: "kind", method: "GET", path: "/api/kind", read: "kind", answers: []string{`{}`}, recorded: 2, sends: 1, unmade: []int{1},
			edit: func(i int, mk *models.Mock) {
				if i == 1 {
					mk.Kind = models.GENERIC
				}
			}},
		// recordings of two calls named one read: neither more nor fewer
		{name: "two-calls", method: "GET", path: "/api/x", read: "xy", answers: []string{`{}`}, recorded: 2, sends: 2, missed: []int{1}, unmade: []int{1},
			edit: func(i int, mk *models.Mock) {
				if i == 1 {
					mk.Spec.HTTPReq.URL = "http://localhost:8083/api/y"
				}
			}},
		// a call more that matches a recording the read does not hold
		{name: "partly-named", method: "GET", path: "/api/p", read: "p", answers: []string{`{}`}, recorded: 3, sends: 4, missed: []int{3},
			edit: func(i int, mk *models.Mock) {
				if i == 0 {
					mk.TestModeInfo.RepeatRead = ""
				}
			}},
		// a call more that matches the recordings of two reads
		{name: "two-reads", method: "GET", path: "/api/q", read: "q1", answers: []string{`{}`}, recorded: 4, sends: 5, missed: []int{4},
			edit: func(i int, mk *models.Mock) {
				if i >= 2 {
					mk.TestModeInfo.RepeatRead = "q2"
				}
			}},
	})
}

// A response the agent's disk store kept apart is read back to compare: a
// named read whose answers were kept apart, one answer, is answered once
// more.
func TestARepeatedReadKeptApartIsReadBackAndAnsweredOnceMore(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	repeatRun(t, []repeatCase{
		{name: "spilled", method: "GET", path: "/api/spilled", read: "s", answers: []string{`{"s":1}`}, recorded: 2, sends: 3,
			edit: func(_ int, mk *models.Mock) {
				resp := *mk.Spec.HTTPResp
				mk.Spec.HTTPResp = nil
				mk.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
					r := resp
					return &r, nil, nil
				})
			}},
	})
}

// A shared recording (keploy record's full round: a test whose request was
// open beside another's) is never one of the test's own repeated reads,
// whatever read it names: it does not make a read the test recorded once a
// repeated one, it does not decide whether the test's own recordings hold
// one answer, and a call more that matches it is not answered again.
func TestASharedRecordingIsNoPartOfATestsRepeatedRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	// run serves the test own (its own recordings) beside shared, all of
	// them recordings of one call the mapping names read "r", sends the call
	// sends times, and returns the sends that missed and what its end
	// charged as never made.
	run := func(own []string, shared string, sends int) (missed []int, left []string) {
		var owned, set []*models.Mock
		for i, answer := range own {
			mk := readMock(fmt.Sprintf("own-%d", i), t0.Add(time.Duration(i)*time.Second), "GET", "/api/shared", "", answer)
			mk.TestModeInfo.RepeatRead = "r"
			owned = append(owned, mk)
		}
		sh := readMock("shared-0", t0.Add(time.Minute), "GET", "/api/shared", "", shared)
		sh.TestModeInfo.RepeatRead = "r"
		set = append(append(set, owned...), sh)
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SetMocksWithWindow(nil, set, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactPool("k", nil)
		mm.ServeExactTestConcurrent("t", owned, []*models.Mock{sh})
		for i := 0; i < sends; i++ {
			if r := sendForMiss(t, mm, models.OutgoingOptions{}, nil, readCall("GET", "/api/shared", "")); r != nil {
				missed = append(missed, i)
			}
		}
		test, _, left, ok := mm.ExactTestOutcome()
		if !ok || test != "t" {
			t.Fatalf("not serving t exactly at its end: %q %v", test, ok)
		}
		return missed, left
	}
	// recorded once by the test, once beside it: the third call misses
	if missed, _ := run([]string{`{}`}, `{}`, 3); !reflect.DeepEqual(missed, []int{2}) {
		t.Errorf("one own, one shared, sent 3: missed %v; want the third call (2)", missed)
	}
	// recorded twice by the test, once beside it: a fourth call is two more
	// than the test's own recordings, and the shared one is not answered
	// again
	if missed, _ := run([]string{`{}`, `{}`}, `{}`, 4); !reflect.DeepEqual(missed, []int{3}) {
		t.Errorf("two own, one shared, sent 4: missed %v; want the fourth call (3)", missed)
	}
	// recorded twice by the test with one answer, once beside it with
	// another: the test's read holds one answer, so its one fewer is excused
	if missed, left := run([]string{`{"v":1}`, `{"v":1}`}, `{"v":2}`, 1); len(missed) != 0 || len(left) != 0 {
		t.Errorf("two own of one answer, one shared of another, sent 1: missed %v, never made %v; want none and none", missed, left)
	}
}

// AnswerRepeatRead answers a read again only from the open view's own
// recordings, only once every recording of the read has answered, with the
// read's last recording, and once.
func TestARepeatedReadIsAnsweredAgainOnlyFromTheViewsOwnAnsweredRecordings(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	r0 := readMock("r-0", t0, "GET", "/api/r", "", `{}`)
	r1 := readMock("r-1", t0.Add(time.Second), "GET", "/api/r", "", `{}`)
	r0.TestModeInfo.RepeatRead, r1.TestModeInfo.RepeatRead = "r", "r"
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, []*models.Mock{r0, r1}, models.BaseTime, time.Now().Add(time.Hour))
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("t", []*models.Mock{r0, r1})
	send := func() {
		t.Helper()
		if r := sendForMiss(t, mm, models.OutgoingOptions{}, nil, readCall("GET", "/api/r", "")); r != nil {
			t.Fatalf("a recorded call missed: %v", r)
		}
	}
	send()
	answered := mm.ConsumedTestMocks()
	if len(answered) != 1 {
		t.Fatalf("answered %d recordings after one call; want 1", len(answered))
	}
	if _, _, ok := mm.AnswerRepeatRead(answered); ok {
		t.Fatal("answered again while one of the read's recordings had not answered")
	}
	send()
	answered = mm.ConsumedTestMocks()
	if _, _, ok := mm.AnswerRepeatRead([]*models.Mock{answered[0].ShallowCopy()}); ok {
		t.Fatal("a copy that is not the open view's recording was answered again")
	}
	again, test, ok := mm.AnswerRepeatRead(answered)
	if !ok || again.Name != "r-1" || test != "t" {
		t.Fatalf("answered again %v in test %q (%v); want the read's last recording r-1 in t", again, test, ok)
	}
	if _, _, ok := mm.AnswerRepeatRead(answered); ok {
		t.Fatal("the read was answered again twice in one test")
	}
	// A test that owns no recording answers none of another test's again.
	mm.ServeExactTest("t2", nil)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a test that owns nothing panicked answering again: %v", r)
			}
		}()
		if _, _, ok := mm.AnswerRepeatRead(answered); ok {
			t.Fatal("a test that owns nothing answered another test's recording again")
		}
	}()
}

// The pinned CORS row's set (TestACORSPreflightRecordedAsTheTestsDataFails
// WhereTheRunsPaceMoves) with the slower test's two preflights named one
// repeated read: that test's third preflight is answered. The rest is as
// that row pins it: the test that recorded one preflight and sent none is
// still charged it, and the test that recorded none still misses its own.
func TestACORSPreflightRecordedTwiceAndNamedARepeatedReadIsAnsweredAThirdTime(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	set, tests := corsSet(false)
	for _, mk := range tests[0].mapping {
		if mk.Spec.HTTPReq.Method == "OPTIONS" {
			mk.TestModeInfo.RepeatRead = "preflight"
		}
	}
	missed, unconsumed := corsRun(t, set, tests)
	if got := missed["more"]; len(got) != 0 {
		t.Errorf("more: missed %v; want none (its third preflight answered again)", got)
	}
	if got := missed["none"]; len(got) != 1 || got[0] != 0 {
		t.Errorf("none: missed %v; want its preflight (0)", got)
	}
	if got := unconsumed["fewer"]; len(got) != 1 || got[0] != "pf-fewer" {
		t.Errorf("fewer: unconsumed %v; want pf-fewer", got)
	}
}
