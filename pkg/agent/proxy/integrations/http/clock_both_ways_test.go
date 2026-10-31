package http

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// clientJWT is a token the test's client signs itself (api-server's e2e
// setUp: SignAdminJWT), whose auth_time is the client's own read of the
// clock when it signed, never sent to a dependency before the call.
func clientJWT(authTime int64, jti, sig string) string {
	enc := base64.RawURLEncoding.EncodeToString
	claims := `{"userId":"68d4148edcd121253694486c","cid":"keploy.io","tokenType":"access","auth_time":` + strconv.FormatInt(authTime, 10) +
		`,"sid":"e2e-go-session","exp":` + strconv.FormatInt(authTime+901, 10) + `,"jti":"` + jti + `"}`
	return "Bearer " + enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc([]byte(claims)) + "." + sig
}

// The shape of api-server's end-to-end suite run under keploy mock (record's full round):
// the test client signs its admin login at start-up (auth_time = its clock),
// and test A (TestAppCRUD) stamps its PAT's lastUsedAt with the app's clock
// in the same second, in the recording and in F. A later test B
// (TestGenerateTestSuitesOnClient) sends POST /run with a token whose
// auth_time is the login's, a second after A, outside B's own window. Its
// recorded value stands at A's lastUsedAt, which A's learn step learned as a
// read of the clock: keploy cannot tell a copy of that read from the
// client's own read in the same second, which it never sees. The real run's
// value is the client's (the login it signed), so following A's place would
// bind a same-second coincidence, and the value stands inside a token the
// client signed, whose jti and signature change too, where no variable can
// be bound (without the refusal the call is still refused: "inside a longer
// value or at a place keploy cannot bind it"). This row pins that the call
// is refused, naming A's place, and that nothing binds at A's clock read,
// whose recording a replay still serves.
func TestARunTokenTheClientSignedInTheSecondAnEarlierTestReadTheClockIsNotFollowed(t *testing.T) {
	lead := learn.FullRoundLead
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.NewSet()
	ctx := learn.WithTracker(context.Background(), tr)
	boot := learnRecorded("mock-0", 0, "GET", "/v1/health", "", nil, `{}`)
	tr.ServedMockAt(boot, learnAt.Add(lead))
	tr.ServedMockAsRecorded(boot)
	// The client signs its login at learnAt+1.0s in both runs (F: lead on).
	recSec, liveSec := learnAt.Add(time.Second).Unix(), learnAt.Add(lead+time.Second).Unix()
	// Test A: the PAT PUT's lastUsedAt, a read of the clock in its window.
	tr.RecordedBegin(learnAt.Add(1050 * time.Millisecond))
	tr.BeginAt(learnAt.Add(lead + 1050*time.Millisecond))
	recA := learnRecorded("mock-a", 1100*time.Millisecond, "PUT", "/v1/secret/data/pat/e2e-go-key", `{"data":{"lastUsedAt":`+strconv.FormatInt(recSec, 10)+`}}`, nil, `{}`)
	cpA := *recA
	a := &learnDb{perTest: []*models.Mock{&cpA}}
	in := live("PUT", "/v1/secret/data/pat/e2e-go-key", `{"data":{"lastUsedAt":`+strconv.FormatInt(liveSec, 10)+`}}`, nil)
	in.arrived = learnAt.Add(lead + 1100*time.Millisecond)
	if served, miss, err := h.learnMatch(ctx, in, a, n, opts); err != nil || served == nil || miss != nil || !learnedClockRead(a.consumed[0].Spec, "body.data.lastUsedAt") {
		t.Fatalf("test A's PUT: served %v miss %+v err %v; want lastUsedAt learned as a read of the clock", served, miss, err)
	}
	tr.End()
	// Test B: POST /run a second later, with the client's login.
	tr.RecordedBegin(learnAt.Add(2300 * time.Millisecond))
	tr.BeginAt(learnAt.Add(lead + 2300*time.Millisecond))
	recB := learnRecorded("mock-1", 2400*time.Millisecond, "POST", "/run/f10b14b0-419e-41f3-af79-80b33295a276?timeout=30", `{"timeout":30}`,
		map[string]string{"Authorization": clientJWT(recSec, "a38bdb95-ce1f-48af-8c00-1c00de6ea7bc", "RjcrMJT2dYLmXOZV")}, ``)
	cpB := *recB
	db := &learnDb{perTest: []*models.Mock{&cpB}}
	in = live("POST", "/run/f10b14b0-419e-41f3-af79-80b33295a276?timeout=30", `{"timeout":30}`,
		map[string]string{"Authorization": clientJWT(liveSec, "5e0c1b2a-77d4-4c1e-9a3f-0b6d2e8f4c11", "Qx9vT2bLmN4kZp7W")})
	in.arrived = learnAt.Add(lead + 2400*time.Millisecond)
	served, miss, err := h.learnMatch(ctx, in, db, n, opts)
	const why = "header.authorization changed, and its recorded value was sent in mock-a at body.data.lastUsedAt, an earlier test's call, where keploy learned it as a read of the application's clock, and this call does not hold it as its own read of the clock"
	if err != nil {
		t.Fatal(err)
	}
	if served != nil || miss == nil || !strings.Contains(miss.res.Why, why) {
		t.Fatalf("test B's POST /run: served %v miss %+v; want it refused: %s", served != nil, miss, why)
	}
	t.Logf("test B's POST /run refused: %s", miss.res.Why)
	recs := learnedOntoTaking([]*models.Mock{recA, recB}, append(append([]models.Mock{}, a.consumed...), db.consumed...), map[string]string{"mock-a": "TestA", "mock-1": "TestB"})
	if len(recs[0].Spec.Variables) != 0 {
		t.Fatalf("test A's PUT binds %+v; want nothing bound at its clock read", recs[0].Spec.Variables)
	}
	base := learnAt.Add(time.Hour)
	r := live("PUT", "/v1/secret/data/pat/e2e-go-key", `{"data":{"lastUsedAt":`+strconv.FormatInt(base.Unix(), 10)+`}}`, nil)
	r.arrived = base.Add(100 * time.Millisecond)
	if ok, _, diag := matchVarIn(t, clockConn(true, base), newHTTP(), &varDb{perTest: []*models.Mock{recs[0]}}, r); !ok {
		t.Fatalf("test A's recording is not served at a replay: %+v", diag)
	}
}

// A poll the test sends a second after its own call stamped a read of the
// clock (lastUsedAt, learned as that read), whose recorded value, now-2s cut
// to the second, names the second that read was made in, while record's full
// round, slower, saw the poll name another second than the read's: a copy
// moves with its source, so the read is no source of the poll's value, and
// the poll is the call's own read of the clock, as Mongo's learn judges it.
// HTTP's learn bound it to the earlier call's read as a variable instead,
// which every replay whose two calls the clock moves between fails, and
// which puts a variable and a read of the clock at one place, which HTTP's
// replay refuses. It is learned as a read of the clock its window holds in
// both runs, checked at every replay, and nothing binds at the earlier
// call's read.
func TestAPollInTheSecondAnEarlierCallReadTheClockThatTheFullRoundRefutesIsLearnedAsItsOwnRead(t *testing.T) {
	lead := learn.FullRoundLead
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.NewSet()
	ctx := learn.WithTracker(context.Background(), tr)
	boot := learnRecorded("mock-0", 0, "GET", "/v1/health", "", nil, `{}`)
	tr.ServedMockAt(boot, learnAt.Add(lead))
	tr.ServedMockAsRecorded(boot)
	tr.RecordedBegin(learnAt.Add(1050 * time.Millisecond))
	tr.BeginAt(learnAt.Add(lead + 1050*time.Millisecond))
	stamp := func(at time.Time) string { return `{"data":{"lastUsedAt":` + strconv.FormatInt(at.Unix(), 10) + `}}` }
	recA := learnRecorded("mock-a", 1100*time.Millisecond, "PUT", "/v1/secret/data/pat/e2e-go-key", stamp(learnAt.Add(1100*time.Millisecond)), nil, `{}`)
	cpA := *recA
	a := &learnDb{perTest: []*models.Mock{&cpA}}
	in := live("PUT", "/v1/secret/data/pat/e2e-go-key", stamp(learnAt.Add(lead+1100*time.Millisecond)), nil)
	in.arrived = learnAt.Add(lead + 1100*time.Millisecond)
	if served, miss, err := h.learnMatch(ctx, in, a, n, opts); err != nil || served == nil || miss != nil || !learnedClockRead(a.consumed[0].Spec, "body.data.lastUsedAt") {
		t.Fatalf("the PUT: served %v miss %+v err %v; want lastUsedAt learned as a read of the clock", served, miss, err)
	}
	// The poll: dirtyBefore = now-2s, cut to the second. Recorded at 3.2 s,
	// it names the PUT's second (1 s); in F, at 4.1 s, it names 2 s past
	// F's start, a second after F's PUT.
	poll := func(now time.Time) string {
		return `{"dirtyBefore":` + strconv.FormatInt(now.Add(-2*time.Second).Unix(), 10) + `}`
	}
	recP := learnRecorded("mock-1", 3200*time.Millisecond, "POST", "/v1/endpoints/claim", poll(learnAt.Add(3200*time.Millisecond)), nil, `{"claimed":[]}`)
	cpP := *recP
	db := &learnDb{perTest: []*models.Mock{&cpP}}
	in = live("POST", "/v1/endpoints/claim", poll(learnAt.Add(lead+4100*time.Millisecond)), nil)
	in.arrived = learnAt.Add(lead + 4100*time.Millisecond)
	served, miss, err := h.learnMatch(ctx, in, db, n, opts)
	if err != nil || served == nil || miss != nil {
		t.Fatalf("the poll: served %v miss %+v err %v; want it learned", served != nil, miss, err)
	}
	if !learnedClockRead(db.consumed[0].Spec, "body.dirtyBefore") {
		t.Fatalf("the poll's dirtyBefore learned as %+v, noise %v; want a read of the clock, no noise", db.consumed[0].Spec.Clocks, db.consumed[0].Spec.ReqBodyNoise)
	}
	tr.End()
	recs := learnedOntoTaking([]*models.Mock{recA, recP}, append(append([]models.Mock{}, a.consumed...), db.consumed...), map[string]string{"mock-a": "TestA", "mock-1": "TestA"})
	if len(recs[0].Spec.Variables) != 0 || len(recs[1].Spec.Variables) != 0 {
		t.Fatalf("the PUT binds %+v, the poll %+v; want nothing bound at the PUT's read", recs[0].Spec.Variables, recs[1].Spec.Variables)
	}
}
