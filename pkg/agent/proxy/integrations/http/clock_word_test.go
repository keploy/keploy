package http

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// keploy mock's client test names an app by its own clock ("[E2E]
// stop-<UnixNano>", api-server's e2e fixtures), read after the test began
// and before the app's previous call (a session lookup), so the call's own
// window holds no read of it. Through the HTTP adapter the learn step learns
// that one word of the name as a read of the clock since the test began
// (models.ClockOffset.Word): the consumed mock carries it in
// MockSpec.Clocks, the word's place in the text and its begin with it, and
// no noise. A later replay answers the name its harness reads again after
// that replay's test began, and refuses, naming the field, a name whose
// rest changed, one read before its test began, and the recorded name
// itself: only the word is read as the clock, and the rest is still
// compared.
func TestLearnMatchAClientTestsOwnClockWordIsLearnedAndCheckedAsTheWordAlone(t *testing.T) {
	const field = "body.name"
	name := func(prefix string, read time.Time) string {
		return `{"name":"` + prefix + strconv.FormatInt(read.UnixNano(), 10) + `"}`
	}
	identity := func(r time.Time) (time.Time, bool) { return r, true }
	recBegin := learnAt.Add(time.Second)
	recRead := recBegin.Add(200 * time.Millisecond)
	session := learnRecorded("mock-0", 1300*time.Millisecond, "GET", "/v1/session", "", nil, `{"user":"u-1"}`)
	create := learnRecorded("mock-1", 1450*time.Millisecond, "POST", "/v1/apps", name("[E2E] stop-", recRead), nil, `{"id":"a-1"}`)

	// conn is a run of the client test that began at began: its harness
	// read its clock 200 ms on, and the app looked up the session 100 ms
	// after that, then sent the create.
	conn := func(began time.Time, learnEnded bool) context.Context {
		tr := learn.NewTracker()
		tr.SetClientTests(true)
		tr.SetAppClock(identity)
		if !learnEnded {
			tr.RecordedBegin(recBegin)
		}
		tr.BeginAt(began)
		tr.ServedMockAt(session, began.Add(300*time.Millisecond))
		tr.ServedMockAsRecorded(session)
		if learnEnded {
			tr.SetLearnEnded(true)
		}
		return learn.WithTracker(context.Background(), tr)
	}

	h, n, opts := learnOn(t)
	began := learnAt.Add(10 * time.Minute)
	in := live("POST", "/v1/apps", name("[E2E] stop-", began.Add(200*time.Millisecond)), nil)
	in.arrived = began.Add(450 * time.Millisecond)
	db := &learnDb{perTest: []*models.Mock{session, create}}
	got, miss, err := h.learnMatch(conn(began, false), in, db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || miss != nil || len(db.consumed) != 1 || !learnedClockRead(db.consumed[0].Spec, field) {
		why := ""
		if miss != nil {
			why = miss.res.Why
		}
		t.Fatalf("served %v miss %q consumed %+v; want %s learned as a read of the clock", got, why, db.consumed, field)
	}
	o := db.consumed[0].Spec.Clocks[field]
	if want := (models.ClockWord{Before: "[E2E] stop-"}); o.Word != want || o.Since != models.SinceTestBegin {
		t.Fatalf("%s learned as %+v; want the word after %q, read since its test began", field, o, want.Before)
	}

	// The replay 90 s later: its harness reads its clock again after that
	// test began.
	replay := began.Add(90 * time.Second)
	mocks := learnedOnto([]*models.Mock{session, create}, db.consumed)
	for _, c := range []struct {
		name string
		body string
		ok   bool
	}{
		{"the name read after this test began", name("[E2E] stop-", replay.Add(200*time.Millisecond)), true},
		{"its rest changed", name("[E2E] start-", replay.Add(200*time.Millisecond)), false},
		{"read before this test began", name("[E2E] stop-", replay.Add(-time.Second)), false},
		{"the recorded name", name("[E2E] stop-", recRead), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			call := live("POST", "/v1/apps", c.body, nil)
			call.arrived = replay.Add(450 * time.Millisecond)
			ok, _, diag := matchVarIn(t, conn(replay, true), newHTTP(), &varDb{perTest: append([]*models.Mock(nil), mocks...)}, call)
			if c.ok {
				if !ok {
					t.Fatalf("refused (%v); want it answered", differs(diag))
				}
				return
			}
			why := ""
			for _, f := range diag.differs {
				if f.shape == field {
					why = f.why
				}
			}
			if ok || !strings.HasPrefix(why, field+" was learned as ") {
				t.Fatalf("answered %v, differs %v (%q); want it refused, naming %s and why", ok, differs(diag), why, field)
			}
		})
	}
}
