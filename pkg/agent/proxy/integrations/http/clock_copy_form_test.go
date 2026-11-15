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
)

// api-server's PAT last-use touch: test B's vault PUT stamps lastUsedAt with
// time.Now().Unix() in the second an earlier test's call (test A's app
// insert) stamped createdAt, which A's learn step learned as a read of the
// clock, while B's PUT window does not hold that second as its own read (the
// touch made outside B's window in record's full round). The learn step
// refused it ("this call does not hold it as its own read of the clock"), so
// the touch got no answer and its test failed. Under keploy mock's time
// default (T2) the founder's rule makes a place learned as a read of the
// clock a time in any scope, checked by its type and form alone: the PUT is
// served and lastUsedAt learned as a time of its epoch-seconds form, with
// nothing bound at A's place. keploy.yml's times: strict refuses it as
// before, naming A's call and place, never the value (keploy test, T1, is
// unchanged too: TestACopyOfAnEarlierCallsClockReadIsCheckedByItsFormInKeployMockAlone).
// A replay of the learned PUT passes another time of the same form (an hour
// later, no window holds it) and refuses an epoch in milliseconds, a text
// time, and any value under strict times.
func TestLearnMatchChecksACopyOfAnEarlierTestsClockReadByItsFormUnderTheTimeDefault(t *testing.T) {
	lead := learn.FullRoundLead
	sec := func(d time.Duration) string { return strconv.FormatInt(learnAt.Add(d).Unix(), 10) }
	const why = "body.data.lastUsedAt changed, and its recorded value was sent in mock-a at body.app.createdAt, an earlier test's call, where keploy learned it as a read of the application's clock, and this call does not hold it as its own read of the clock"
	for _, tc := range []struct {
		what  string
		timed bool // the time default: learned as a time checked by its form; refused otherwise
	}{
		{what: "keploy mock, the time default", timed: true},
		{what: "keploy mock, times: strict"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.SetClientTests(true)
			tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
			tr.NewSet()
			ctx := learn.WithTracker(context.Background(), tr)
			boot := learnRecorded("mock-0", 0, "GET", "/v1/health", "", nil, `{}`)
			tr.ServedMockAt(boot, learnAt.Add(lead))
			tr.ServedMockAsRecorded(boot)
			tr.RecordedBegin(learnAt.Add(time.Second))
			tr.BeginAt(learnAt.Add(lead + time.Second))
			store := func(db *learnDb) integrations.MockMemDb {
				if tc.timed {
					return timeFormDb{db}
				}
				return db
			}
			// Test A's insert stamps createdAt with its own clock: learned
			// as a read of the clock in its call's window.
			recA := learnRecorded("mock-a", 1100*time.Millisecond, "POST", "/v1/apps", `{"app":{"createdAt":`+sec(time.Second)+`}}`, nil, `{}`)
			cpA := *recA
			a := &learnDb{perTest: []*models.Mock{&cpA}}
			in := live("POST", "/v1/apps", `{"app":{"createdAt":`+sec(lead+time.Second)+`}}`, nil)
			in.arrived = learnAt.Add(lead + 1100*time.Millisecond)
			if served, miss, err := h.learnMatch(ctx, in, store(a), n, opts); err != nil || served == nil || miss != nil || !learnedClockRead(a.consumed[0].Spec, "body.app.createdAt") {
				t.Fatalf("test A's insert: served %v miss %+v err %v; want createdAt learned as a read of the clock", served != nil, miss, err)
			}
			tr.End()
			// Test B, ten seconds on: its PUT carries A's second, which
			// the PUT's window does not hold.
			at := 10 * time.Second
			tr.RecordedBegin(learnAt.Add(at))
			tr.BeginAt(learnAt.Add(lead + at))
			const put = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/k1"
			recB := learnRecorded("mock-1", at+100*time.Millisecond, "PUT", put, `{"data":{"lastUsedAt":`+sec(time.Second)+`}}`, nil, `{}`)
			cpB := *recB
			b := &learnDb{perTest: []*models.Mock{&cpB}}
			in = live("PUT", put, `{"data":{"lastUsedAt":`+sec(lead+time.Second)+`}}`, nil)
			in.arrived = learnAt.Add(lead + at + 100*time.Millisecond)
			served, miss, err := h.learnMatch(ctx, in, store(b), n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if v := a.consumed[0].Spec.Variables; len(v) != 0 {
				t.Fatalf("test A's insert binds %+v; want nothing bound at its read of the clock", v)
			}
			if !tc.timed {
				if served != nil || miss == nil || !strings.Contains(miss.res.Why, why) || strings.Contains(miss.res.Why, sec(time.Second)) {
					t.Fatalf("served %v miss %+v; want the PUT refused: %s, never the value", served != nil, miss, why)
				}
				return
			}
			if served == nil || miss != nil || len(b.consumed) != 1 {
				t.Fatalf("served %v miss %+v; want the PUT served, lastUsedAt a time checked by its form", served != nil, miss)
			}
			o, ok := b.consumed[0].Spec.Clocks["body.data.lastUsedAt"]
			if !ok || o.Form == "" || o.Res != 0 || len(b.consumed[0].Spec.ReqBodyNoise) != 0 || len(b.consumed[0].Spec.Variables) != 0 {
				t.Fatalf("the PUT learned clocks %+v noise %v variables %+v; want lastUsedAt checked by its form alone, nothing else", b.consumed[0].Spec.Clocks, b.consumed[0].Spec.ReqBodyNoise, b.consumed[0].Spec.Variables)
			}
			if !strings.Contains(o.Why, "checked by its form alone") || !strings.Contains(o.Why, "mock-a at body.app.createdAt") {
				t.Fatalf("the learned line says %q; want it to name the form check and A's place", o.Why)
			}
			// What a replay serves: the learned blocks onto the recordings.
			recs := learnedOntoTaking([]*models.Mock{recA, recB}, append(append([]models.Mock{}, a.consumed...), b.consumed...), map[string]string{"mock-a": "TestA", "mock-1": "TestB"})
			base := learnAt.Add(time.Hour)
			for _, rc := range []struct {
				what      string
				value     string
				timeForm  bool
				wantServe bool
			}{
				{"another time of the same form", strconv.FormatInt(base.Unix(), 10), true, true},
				{"an epoch in milliseconds", strconv.FormatInt(base.UnixMilli(), 10), true, false},
				{"a text time", `"` + base.Format(time.RFC3339) + `"`, true, false},
				{"the same form under strict times", strconv.FormatInt(base.Unix(), 10), false, false},
			} {
				db := timedVarDb{varDb: &varDb{perTest: []*models.Mock{recs[1]}}, timeForm: rc.timeForm}
				r := live("PUT", put, `{"data":{"lastUsedAt":`+rc.value+`}}`, nil)
				r.arrived = base.Add(100 * time.Millisecond)
				ok, stub, diag, err := h.match(clockConn(true, base), r, db, nil, nil, nil, true, false, false, true, true)
				if err != nil {
					t.Fatalf("%s: match: %v", rc.what, err)
				}
				if ok != rc.wantServe || ok && stub.Name != "mock-1" {
					t.Errorf("replay, %s: served %v (%v) diag %+v; want served %v", rc.what, ok, stub, diag, rc.wantServe)
				}
			}
		})
	}
}
