package proxy

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
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
	missed, unconsumed := corsRun(t, set, tests)
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
	})
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
// read whose recordings answered differently, a write by its method, and a
// repeat the mapping does not name.
func TestWhatIsNotARepeatedReadKeepsItsExactCount(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	repeatRun(t, []repeatCase{
		{name: "once", method: "GET", path: "/api/once", read: "once", answers: []string{`{}`}, recorded: 1, sends: 2, missed: []int{1}},
		{name: "two-answers-more", method: "GET", path: "/api/count", read: "count", answers: []string{`{"n":1}`, `{"n":2}`}, recorded: 2, sends: 3, missed: []int{2}},
		{name: "two-answers-fewer", method: "GET", path: "/api/count2", read: "count2", answers: []string{`{"n":1}`, `{"n":2}`}, recorded: 2, sends: 1, unmade: []int{1}},
		{name: "put", method: "PUT", path: "/api/doc", body: `{"x":1}`, read: "doc", answers: []string{`{"ok":true}`}, recorded: 2, sends: 3, missed: []int{2}},
		{name: "delete-fewer", method: "DELETE", path: "/api/doc/1", read: "del", answers: []string{`{}`}, recorded: 2, sends: 1, unmade: []int{1}},
		{name: "unnamed-more", method: "POST", path: "/api/method/create", body: `{"y":1}`, answers: []string{`{"name":"x"}`}, recorded: 2, sends: 3, missed: []int{2}},
		{name: "unnamed-fewer", method: "GET", path: "/api/unnamed", answers: []string{`{}`}, recorded: 2, sends: 1, unmade: []int{1}},
		{name: "patch", method: "PATCH", path: "/api/doc/2", body: `{"z":1}`, read: "patch", answers: []string{`{}`}, recorded: 2, sends: 3, missed: []int{2}},
		{name: "status", method: "GET", path: "/api/status", read: "st", answers: []string{`{}`}, recorded: 2, sends: 3, missed: []int{2},
			edit: func(i int, mk *models.Mock) {
				if i == 1 {
					mk.Spec.HTTPResp.StatusCode = 503
				}
			}},
		{name: "binary", method: "GET", path: "/api/bin", read: "bin", answers: []string{``}, recorded: 2, sends: 3, missed: []int{2},
			edit: func(i int, mk *models.Mock) { mk.Spec.HTTPResp.Binary = []string{"AAE=", "AAI="}[i] }},
		{name: "skipped", method: "GET", path: "/api/big", read: "big", answers: []string{``}, recorded: 2, sends: 3, missed: []int{2},
			edit: func(_ int, mk *models.Mock) { mk.Spec.HTTPResp.BodySkipped = true }},
		{name: "streamed", method: "GET", path: "/api/events", read: "sse", answers: []string{``}, recorded: 2, sends: 3, missed: []int{2},
			edit: func(_ int, mk *models.Mock) { mk.Spec.HTTPResp.StreamBody = []models.HTTPStreamChunk{{}} }},
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
// whatever read it names: the test's own recording of the call, answered,
// with a shared one of the same read answered too, is not answered a third
// time.
func TestASharedRecordingIsNoPartOfATestsRepeatedRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	own := readMock("own-0", t0, "GET", "/api/shared", "", `{}`)
	shared := readMock("shared-0", t0.Add(time.Second), "GET", "/api/shared", "", `{}`)
	own.TestModeInfo.RepeatRead, shared.TestModeInfo.RepeatRead = "r", "r"
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, []*models.Mock{own, shared}, models.BaseTime, time.Now().Add(time.Hour))
	mm.ServeExactPool("k", nil)
	mm.ServeExactTestConcurrent("t", []*models.Mock{own}, []*models.Mock{shared})
	var missed []int
	for i := 0; i < 3; i++ {
		if r := sendForMiss(t, mm, models.OutgoingOptions{}, nil, readCall("GET", "/api/shared", "")); r != nil {
			missed = append(missed, i)
		}
	}
	if !reflect.DeepEqual(missed, []int{2}) {
		t.Fatalf("missed %v; want the third call (2): the shared recording is no part of the test's read", missed)
	}
	// and a copy that is not the view's own answers nothing again
	if _, ok := mm.AnswerRepeatRead([]*models.Mock{own.ShallowCopy()}); ok {
		t.Fatal("a copy that is not the open view's recording was answered again")
	}
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
