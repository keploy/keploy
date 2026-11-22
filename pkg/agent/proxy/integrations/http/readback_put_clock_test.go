package http

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// kpStamped is the key record with its stamps read of the application's
// clock at at: createdAt and expiresAt (a year on) in epoch seconds,
// updatedAt in RFC 3339.
func kpStamped(by, nano string, at time.Time) string {
	return `{"_id":"` + kpRecKey + `","cid":"keploy.io","createdAt":` + strconv.FormatInt(at.Unix(), 10) + `,"createdBy":"` + by +
		`","expiresAt":` + strconv.FormatInt(at.Add(365*24*time.Hour).Unix(), 10) + `,"keyType":"pat","name":"CI identity: github octo-org/repo-` +
		nano + `","status":"active","updatedAt":"` + at.UTC().Format(time.RFC3339) + `"}`
}

// kpStampRun learns on the key record's PUT and GET with keploy.yml's clock
// rules at the PUT's createdAt and updatedAt (api-server's), under the time
// default: recorded, the record is stamped 120 ms into the test; in this
// run, ten minutes later, at the same point of the test.
func kpStampRun(t *testing.T, proven string) *learnDb {
	t.Helper()
	h, _, opts := learnOn(t)
	t.Setenv(provenswitch.Env, proven)
	n := h.newLearnNoise(opts, nil, nil)
	n.sources = func(mock string) []models.DeclaredSource {
		if mock != "mock-0" {
			return nil
		}
		return []models.DeclaredSource{
			{Rule: "pat-created-at", Kind: models.DeclaredSourceClock, Field: "body.data.createdAt", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}},
			{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: &models.ClockOffset{Min: -time.Second, Res: time.Second}},
		}
	}
	recBegin := learnAt.Add(time.Second)
	began := learnAt.Add(10 * time.Minute)
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.RecordedBegin(recBegin)
	tr.BeginAt(began)
	// The client test names its repository by a UnixNano read 100 ms in.
	recNano := strconv.FormatInt(recBegin.Add(100*time.Millisecond).UnixNano(), 10)
	liveNano := strconv.FormatInt(began.Add(100*time.Millisecond).UnixNano(), 10)
	recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Timestamp: recBegin.Add(100 * time.Millisecond),
		Header: map[string]string{"Content-Type": "application/json"}, Body: kpClient(kpRecBy, recNano)}
	tr.SetCases([]learn.ClientCase{{Name: "post-query-83", Req: recReq}})
	liveReq := *recReq
	liveReq.Body, liveReq.Timestamp = kpClient(kpLiveBy, liveNano), began.Add(100*time.Millisecond)
	if !tr.Incoming(&liveReq, "", liveReq.Timestamp) {
		t.Fatal("the client test's request was not paired with its test case")
	}
	recAt, liveAt := recBegin.Add(120*time.Millisecond), began.Add(120*time.Millisecond)
	answer := func(by, nano string, at time.Time) string { return `{"data":{"data":` + kpStamped(by, nano, at) + `}}` }
	mocks := []*models.Mock{
		learnRecorded("mock-0", 1130*time.Millisecond, "PUT", kpPath(kpRecKey), `{"data":`+kpStamped(kpRecBy, recNano, recAt)+`}`, kpVault, `{"data":{"version":1}}`),
		learnRecorded("mock-1", 1300*time.Millisecond, "GET", kpPath(kpRecKey), "", kpVault, answer(kpRecBy, recNano, recAt)),
	}
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: mocks, observe: tr.ServedMock}
	var store integrations.MockMemDb = timeFormDb{db}
	for i, c := range []*req{
		live("PUT", kpPath(kpRecKey), `{"data":`+kpStamped(kpLiveBy, liveNano, liveAt)+`}`, kpVault),
		live("GET", kpPath(kpRecKey), "", kpVault),
	} {
		c.arrived = began.Add(mocks[i].Spec.ReqTimestampMock.Sub(recBegin))
		served, miss, err := h.learnMatch(ctx, c, store, n, opts)
		if err != nil || miss != nil || served == nil {
			why := ""
			if miss != nil {
				why = miss.res.Why
			}
			t.Fatalf("call %d: served %v miss %s err %v", i, served != nil, why, err)
		}
	}
	return db
}

// The key record's stamps drift with the run: the PUT's learn step learns
// createdAt, updatedAt and expiresAt as reads of the application's clock
// (keploy.yml's rules and the offset since the test began), checked at
// every replay. The GET that reads the record back answers what this run's
// PUT wrote (the account, the name's word) but each stamp as recorded: a
// variable bound at the PUT's stamp too is a place the HTTP replay refuses
// (TestAPlaceLearnedAsOneThingIsServed), so the PUT would be refused in
// every later replay. No variable is bound at a place the PUT checks as a
// clock, and those checks stay.
func TestReadBackLeavesAPutsClockStampsToTheirCheck(t *testing.T) {
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven, func(t *testing.T) {
			db := kpStampRun(t, proven)
			put, get := db.consumed[0], db.consumed[1]
			for _, f := range []string{"body.data.createdAt", "body.data.updatedAt", "body.data.expiresAt"} {
				if _, ok := put.Spec.Clocks[f]; !ok {
					t.Fatalf("the PUT learned no read of the clock at %s: %v", f, put.Spec.Clocks)
				}
			}
			for mock, vars := range get.Spec.LearnedBinds() {
				for name, v := range vars {
					if _, clock := put.Spec.Clocks[v.Bind]; mock == put.Name && clock {
						t.Fatalf("the GET binds %s at the PUT's %s, a place the PUT checks as a read of the clock", name, v.Bind)
					}
				}
			}
			answered := map[string]bool{}
			for _, v := range get.Spec.Learned() {
				for _, a := range v.Answer {
					answered[a] = true
				}
			}
			for _, f := range []string{"body.data.data.createdAt", "body.data.data.updatedAt", "body.data.data.expiresAt"} {
				if answered[f] {
					t.Fatalf("the GET answers %s with a variable, want it as recorded", f)
				}
			}
			for _, f := range []string{"body.data.data.createdBy", "body.data.data.name#6"} {
				if !answered[f] {
					t.Fatalf("the GET answers %v, want %s read back from the PUT", answered, f)
				}
			}
		})
	}
}

// clockWritten reads the write's own call: a read of the clock learned at
// another call's leaf of the same path, at a given value's, at an answer's
// leaf or at another leaf of the call is not the write's; a leaf learned as
// no read of the clock is none.
func TestClockWrittenReadsTheWritesOwnLeaf(t *testing.T) {
	const path = "body.data.createdAt"
	o := &models.ClockOffset{Res: time.Second}
	seen := func(mock string, given bool, side learn.Side, at string, offset *models.ClockOffset) learn.Earlier {
		return learn.Earlier{Mock: mock, Given: given, Seen: []learn.Seen{{Side: side, Leaf: learn.Leaf{Path: at, Value: "1"}, Live: "2", Offset: offset}}}
	}
	from := learn.Place{Mock: "mock-0", Side: learn.Request, Path: path}
	for _, c := range []struct {
		name    string
		earlier []learn.Earlier
		want    bool
	}{
		{"the write's leaf", []learn.Earlier{seen("mock-0", false, learn.Request, path, o)}, true},
		{"another call's leaf", []learn.Earlier{seen("mock-9", false, learn.Request, path, o)}, false},
		{"a given value's", []learn.Earlier{seen("mock-0", true, learn.Request, path, o)}, false},
		{"an answer's leaf", []learn.Earlier{seen("mock-0", false, learn.Response, path, o)}, false},
		{"another leaf of the call", []learn.Earlier{seen("mock-0", false, learn.Request, "body.data.updatedAt", o)}, false},
		{"no read of the clock", []learn.Earlier{seen("mock-0", false, learn.Request, path, nil)}, false},
	} {
		if got := clockWritten(c.earlier, from); got != c.want {
			t.Errorf("%s: clockWritten %v, want %v", c.name, got, c.want)
		}
	}
}
