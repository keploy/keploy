package http

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// heartbeatCase varies the call heartbeat learns on. given declares the
// cluster's name as a value the test gave; noCreate leaves out the call that
// creates the cluster (mock-62), so only the given holds the name before the
// heartbeat; random names the cluster with a random word in place of the
// run's clock; seconds names it with RUN in seconds, the digits the
// heartbeat's timestamp sends whole; part sends a part of the name in place
// of the heartbeat's timestamp; timeForm turns the time default on
// (keploy.yml's times setting at its default), else times strict.
type heartbeatCase struct {
	given, noCreate, random, seconds, part, timeForm bool
	early, answerOnly, sentStamp, server             bool
}

// heartbeat learns on a Playwright test's cluster heartbeat (enterprise-ui's
// cluster-operational-base-url.spec.ts): the module reads the clock once for
// RUN (Date.now(), in milliseconds) as the test begins, creates a cluster
// named "ingress-<RUN>" (mock-62), then sends the heartbeat (mock-63) with
// that name and a timestamp of its own read, Math.floor(Date.now()/1000),
// 350 ms on: in the same second as RUN, in both runs. So the timestamp's
// moment stands inside the name mock-62 sent, in both runs. Record's full
// round runs ten minutes later on the real clock (a Node test's clock is
// not moved).
func heartbeat(t *testing.T, c heartbeatCase) (*models.Mock, *learnMiss, *learnDb) {
	t.Helper()
	recBegin := learnAt.Add(1500 * time.Millisecond)
	began := learnAt.Add(10*time.Minute + 500*time.Millisecond)
	name := func(run time.Time, live bool) string {
		switch {
		case c.seconds:
			return "ingress-" + strconv.FormatInt(run.Unix(), 10)
		case !c.random:
			return "ingress-" + strconv.FormatInt(run.UnixMilli(), 10)
		case live:
			return "ingress-9f3a7c2e5b1d4a60"
		}
		return "ingress-1e8b4d6f0a2c9e73"
	}
	stamp := func(at time.Time, nm string) string {
		if c.part {
			return `"` + strings.TrimPrefix(nm, "ingress-") + `"`
		}
		return strconv.FormatInt(at.Unix(), 10)
	}
	body := func(nm, ts string) string {
		return `{"cluster_name":"` + nm + `","timestamp":` + ts + `,"status":"active","ingress_url":"http://ingress.test"}`
	}
	runAt, stampAt := -300*time.Millisecond, 400*time.Millisecond
	if c.early {
		runAt, stampAt = runAt-5*time.Minute, stampAt-5*time.Minute
	}
	nameRec, nameF := name(recBegin.Add(runAt), false), name(began.Add(runAt), true)
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.SetClientTests(!c.server)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.RecordedBegin(recBegin)
	tr.BeginAt(began)
	test, _, _, _ := tr.Test()
	// mock-62's answer echoes the name, served with the run's (the
	// given's answer, or the variable mock-62's name binds).
	if !c.noCreate {
		req, seen := `{"name":"`+nameRec+`"}`, []learn.Seen{{Side: learn.Request, Leaf: learn.Leaf{Path: "body.name", Value: nameRec, Type: learn.String}, Live: nameF, Place: "body.name"}}
		switch {
		case c.answerOnly:
			req, seen = `{"region":"local"}`, []learn.Seen{{Side: learn.Request, Leaf: learn.Leaf{Path: "body.region", Value: "local", Type: learn.String}, Live: "local", Place: "body.region"}}
		case c.sentStamp:
			sec := func(at time.Time) string { return strconv.FormatInt(at.Add(stampAt-300*time.Millisecond).Unix(), 10) }
			req = `{"name":"` + nameRec + `","timestamp":` + sec(recBegin) + `}`
			seen = append(seen, learn.Seen{Side: learn.Request, Leaf: learn.Leaf{Path: "body.timestamp", Value: sec(recBegin), Type: learn.Number}, Live: sec(began), Place: "body.timestamp"})
		}
		create := learnRecorded("mock-62", 1600*time.Millisecond, "POST", "/cluster/create", req, nil, `{"id":7,"name":"`+nameRec+`"}`)
		tr.Served(test, learn.Earlier{Mock: create.Name, At: create.Spec.ReqTimestampMock, Own: true, Payload: learn.RawOf(create, true),
			Seen: append(seen, learn.Seen{Side: learn.Response, Leaf: learn.Leaf{Path: "body.name", Value: nameRec, Type: learn.String}, Live: nameF, Place: "body.name"})})
	}
	hb := learnRecorded("mock-63", 1900*time.Millisecond, "POST", "/cluster/status", body(nameRec, stamp(recBegin.Add(stampAt), nameRec)), nil, `{"ok":true}`)
	db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{hb}}}
	db.table.Start(nil)
	if c.given {
		if _, ok := db.table.Give(variables.Binding{Name: "given:cluster-ingress", Value: nameF, Recorded: nameRec, Mock: "test o::TestHeartbeat", Place: "cluster-operational-base-url.spec.ts:380", Given: true}); !ok {
			t.Fatal("the test's given did not bind")
		}
		tr.GiveValue(learn.TestGiven("test o::TestHeartbeat", "given:cluster-ingress", nameRec, nameF))
	}
	in := live("POST", "/cluster/status", body(nameF, stamp(began.Add(stampAt), nameF)), nil)
	in.arrived = began.Add(450 * time.Millisecond)
	var store integrations.MockMemDb = db
	if c.timeForm {
		store = timeFormGivenDb{db}
	}
	served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, store, n, opts)
	return served, miss, db.learnDb
}

// timeFormGivenDb is a givenDb whose store turned the time default on.
type timeFormGivenDb struct{ *givenDb }

func (timeFormGivenDb) TimeForm() bool { return true }

// A client test's own read of the clock that falls in the second a name it
// sent earlier carries (a heartbeat's timestamp beside a cluster named after
// RUN, both read as the test began, the name echoed in the create call's
// answer) is a time: under the time default it is learned as the read of the
// clock its call's window holds in both runs, checked there at every replay,
// whether or not the test gave the name. The coincidence never makes it a
// value bound to the name, nor refuses it as "inside a longer value". The
// name the test gave is still the given's use. With keploy.yml's times strict
// it is refused as before: strict checks a time by its provenance, and keploy
// cannot tell a pick from the name by the clock from a read of the clock. A
// name the test gave in seconds, whose digits the timestamp sends whole, with
// no call that sent it before, never refuses the read either, under both
// time settings: the search reads a given's text alone, never a clock read's
// words.
func TestLearnMatchKeepsAClientTestsClockReadAnEarlierNameHolds(t *testing.T) {
	const echo = "body.timestamp carries a value mock-62 sent earlier in the test at body.name, inside a longer value or at a place keploy cannot bind it"
	read := func(name string, db *learnDb) {
		t.Helper()
		spec := db.consumed[0].Spec
		if o, ok := spec.Clocks["body.timestamp"]; !ok || o.Layout != "epoch-s" || o.Since != "" {
			t.Errorf("%s: clocks %v; want body.timestamp learned as a read of the clock in epoch seconds, in its call's window", name, spec.Clocks)
		}
		if len(spec.ReqBodyNoise) != 0 {
			t.Errorf("%s: noise %v; want none: a read of the clock stays compared", name, spec.ReqBodyNoise)
		}
	}
	for _, given := range []bool{true, false} {
		served, miss, db := heartbeat(t, heartbeatCase{given: given, timeForm: true})
		if served == nil || miss != nil || len(db.consumed) != 1 {
			t.Fatalf("time default, given %v: served %v, miss %+v; want the heartbeat answered", given, served != nil, miss)
		}
		read("given "+strconv.FormatBool(given), db)
		if v, ok := db.consumed[0].Spec.Learned()["given:cluster-ingress"]; given && (!ok || len(v.Use) != 1 || v.Use[0] != "body.cluster_name") {
			t.Errorf("variables %+v; want the name the test gave used at body.cluster_name", db.consumed[0].Spec.Learned())
		}
		served, miss, _ = heartbeat(t, heartbeatCase{given: given})
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, echo) {
			t.Fatalf("times strict, given %v: served %v, miss %+v; want the heartbeat refused on the name's echo: %s", given, served != nil, miss, echo)
		}
	}
	for _, timeForm := range []bool{true, false} {
		served, miss, db := heartbeat(t, heartbeatCase{given: true, noCreate: true, seconds: true, timeForm: timeForm})
		if served == nil || miss != nil || len(db.consumed) != 1 {
			t.Fatalf("a name given in seconds, time default %v: served %v, miss %+v; want the heartbeat answered", timeForm, served != nil, miss)
		}
		read("a name given in seconds", db)
	}
}

// What the T2 time rule does not explain keeps its reading under the time
// default: a timestamp read with the name five minutes before the test began
// (in no window of its call) is refused on the name's echo; one whose moment
// stands only in the create call's answer is refused as a value keploy
// cannot follow from an answer (a pick from what the application was given);
// one the create call sent whole too stays the variable bound there; in
// keploy record's server tests the rule does not apply, and the heartbeat is
// refused as before; and a part of a name the test gave that is no read of
// the clock (a random word) is refused naming the given, which keploy
// follows only whole.
func TestLearnMatchTheTimeRuleLeavesWhatItDoesNotExplain(t *testing.T) {
	for _, c := range []struct {
		name string
		c    heartbeatCase
		why  string
	}{
		{"read before the test began", heartbeatCase{given: true, early: true}, "body.timestamp carries a value mock-62 sent earlier in the test at body.name, inside a longer value or at a place keploy cannot bind it"},
		{"held in an answer alone", heartbeatCase{given: true, answerOnly: true}, "body.timestamp carries a value mock-62 answered earlier in the test at body.name, which keploy cannot follow from an answer"},
		{"server tests", heartbeatCase{given: true, server: true}, "body.timestamp carries a value mock-62 sent earlier in the test at body.name, inside a longer value or at a place keploy cannot bind it"},
		{"a part of a given that is no time", heartbeatCase{given: true, noCreate: true, random: true, part: true}, learn.GivenDerivedWhy("body.timestamp", "given:cluster-ingress", "cluster-operational-base-url.spec.ts:380")},
	} {
		c.c.timeForm = true
		served, miss, _ := heartbeat(t, c.c)
		if served != nil || miss == nil || !strings.Contains(miss.res.Why, c.why) {
			t.Errorf("%s: served %v, miss %+v; want the heartbeat refused: %s", c.name, served != nil, miss, c.why)
		}
	}
	served, miss, db := heartbeat(t, heartbeatCase{given: true, sentStamp: true, timeForm: true})
	if served == nil || miss != nil || len(db.consumed) != 1 {
		t.Fatalf("the create call sent the timestamp whole: served %v, miss %+v; want the heartbeat answered", served != nil, miss)
	}
	spec := db.consumed[0].Spec
	if _, ok := spec.Clocks["body.timestamp"]; ok || spec.LearnedBinds()["mock-62"]["timestamp"].Bind != "body.timestamp" {
		t.Errorf("the create call sent the timestamp whole: clocks %v binds %+v; want the variable bound at mock-62's body.timestamp", spec.Clocks, spec.LearnedBinds())
	}
}
