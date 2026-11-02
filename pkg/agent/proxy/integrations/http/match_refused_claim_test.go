package http

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A store that hands out a recording and then refuses every claim of it (the
// claim of a session mock looks in the reusable tier, and the staging filed
// it elsewhere) must not keep the match retrying the same recording: each
// pass whose claim fails leaves that recording out of the call's
// candidates, so the call ends in a miss once none is left. Before, the
// match re-read the same pool and failed the same claim forever, at 99% CPU,
// and the call was never answered.
//
// One row per pass that claims: the exact-body pass, the correlation pass,
// the JSON body pass with one candidate, the fuzzy pass, and under the proven
// switch the strict pass. Each waits for the match to return; a hang ends in
// go test's own -timeout, which names the row.
func TestAClaimTheStoreRefusesEndsTheCall(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, live, contentType string
		correlations                      []models.FieldCorrelation
		proven                            bool
	}{
		{name: "exact-body", recorded: `{"a":1}`, live: `{"a":1}`, contentType: "application/json"},
		{name: "correlation", recorded: `{"id":"r-1","a":1}`, live: `{"id":"l-1","a":1}`, contentType: "application/json",
			correlations: []models.FieldCorrelation{{RequestPath: "body.id", ResponsePaths: []string{"body.id"}, RecordedValue: "r-1"}}},
		{name: "json-body", recorded: `{"a":1}`, live: `{"a":2}`, contentType: "application/json"},
		{name: "fuzzy", recorded: "a=1&b=xyz", live: "a=1&b=xyw", contentType: "text/plain"},
		{name: "strict", recorded: `{"a":1}`, live: `{"a":1}`, contentType: "application/json", proven: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				t.Setenv("KEPLOY_PROVEN", "1")
			} else {
				t.Setenv("KEPLOY_PROVEN", "")
			}
			m := &models.Mock{
				Name: "refused",
				Kind: models.Kind(models.HTTP),
				Spec: models.MockSpec{
					HTTPReq: &models.HTTPReq{
						Method: "POST", URL: "http://api/q", ProtoMajor: 1, ProtoMinor: 1,
						Header: map[string]string{"Content-Type": tc.contentType, "Host": "api"},
						Body:   tc.recorded,
					},
					HTTPResp:     &models.HTTPResp{StatusCode: 200, Body: "ok"},
					Correlations: tc.correlations,
				},
				TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, Consume: models.ConsumeCursorSaturate, LifetimeDerived: true},
			}
			db := &mockMemDb{mocks: []*models.Mock{m}}
			h := http.Header{}
			h.Set("Host", "api") // under the proven switch the authority is compared
			h.Set("Content-Type", tc.contentType)
			live := &req{method: "POST", url: &url.URL{Path: "/q"}, header: h, body: []byte(tc.live), raw: []byte("POST /q HTTP/1.1\r\nHost: api\r\n\r\n" + tc.live)}
			hp := newHTTP()
			ok, stub, diag, err := hp.match(context.Background(), live, db, nil, nil, nil, true, false, false, true, true)
			if err != nil || ok || stub != nil {
				t.Fatalf("match: ok %v, served %v, err %v; want a miss", ok, stub != nil, err)
			}
			if db.updateUnFilteredMockOld != m {
				t.Fatalf("the pass under test never claimed the recording (claimed %v): the row does not reach its claim", db.updateUnFilteredMockOld)
			}
			// The miss names the refused recording and the refusal, never
			// "no mocks": the store holds the recording.
			if diag == nil || diag.refused != m || diag.phase != models.MatchPhaseExhausted || diag.candidates != 1 {
				t.Fatalf("diag %+v; want the refused recording, phase %s, 1 candidate", diag, models.MatchPhaseExhausted)
			}
			r := hp.buildHTTPMismatchReport(&http.Request{Method: "POST", URL: &url.URL{Path: "/q"}, Host: "api", Header: h}, []byte(tc.live), db, nil, nil, nil, diag)
			if r.MatchPhase != models.MatchPhaseExhausted || r.ClosestMock != "refused" || !strings.Contains(r.Diff, "refused to hand it over") || !strings.Contains(r.NextSteps, "another call took it first") {
				t.Fatalf("report phase %q, closest %q, diff %q, next %q; want the refusal named", r.MatchPhase, r.ClosestMock, r.Diff, r.NextSteps)
			}
		})
	}
}

// refusingVarDb is varDb whose store refuses every claim of a recording.
type refusingVarDb struct {
	*varDb
	claimed []*models.Mock
}

func (d *refusingVarDb) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	d.claimed = append(d.claimed, old)
	return false
}

// Under the proven switch a recording with variables is served by the
// variable pass alone: a store that hands it out and refuses its claim ends
// the call in a miss, as for the passes above. Before, the pass asked for
// the call to be checked again, found the same recording and failed the same
// claim forever.
func TestAVariableClaimTheStoreRefusesEndsTheCall(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	create := createAndReadBack("ord-rec-1")[0]
	create.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimeSession}
	db := &refusingVarDb{varDb: &varDb{session: []*models.Mock{create}}}
	ok, stub, _, err := newHTTP().match(context.Background(), liveReq("POST", "/orders", `{"id":"l1","item":"pen"}`), db, nil, nil, nil, true, false, false, true, true)
	if err != nil || ok || stub != nil {
		t.Fatalf("match: ok %v, served %v, err %v; want a miss", ok, stub != nil, err)
	}
	if len(db.claimed) != 1 || db.claimed[0] != create {
		t.Fatalf("claims %d; want the one claim of the recording, refused once", len(db.claimed))
	}
}

// refusesOne is mockMemDb whose store refuses the claims of one recording
// only, as when another call took that copy first, and keeps the claims in
// order.
type refusesOne struct {
	*mockMemDb
	refused *models.Mock
	claimed []*models.Mock
}

func (d *refusesOne) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	d.claimed = append(d.claimed, old)
	return old != d.refused
}

// A call whose claim of one recording failed is answered by the next one it
// matches: the failed claim leaves that recording out of the call's later
// passes only, never ends the call while another recording answers it. The
// exact-body pass, and the strict pass under the proven switch.
func TestAClaimRefusedIsAnsweredByTheNextRecording(t *testing.T) {
	for _, proven := range []bool{false, true} {
		name := "exact-body"
		if proven {
			name = "strict"
		}
		t.Run(name, func(t *testing.T) {
			if proven {
				t.Setenv("KEPLOY_PROVEN", "1")
			} else {
				t.Setenv("KEPLOY_PROVEN", "")
			}
			rec := func(name string) *models.Mock {
				return &models.Mock{
					Name: name,
					Kind: models.Kind(models.HTTP),
					Spec: models.MockSpec{
						HTTPReq: &models.HTTPReq{
							Method: "POST", URL: "http://api/q", ProtoMajor: 1, ProtoMinor: 1,
							Header: map[string]string{"Content-Type": "application/json", "Host": "api"},
							Body:   `{"a":1}`,
						},
						HTTPResp: &models.HTTPResp{StatusCode: 200, Body: name},
					},
					TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
				}
			}
			first, second := rec("first"), rec("second")
			db := &refusesOne{mockMemDb: &mockMemDb{mocks: []*models.Mock{first, second}}, refused: first}
			h := http.Header{}
			h.Set("Host", "api") // under the proven switch the authority is compared
			h.Set("Content-Type", "application/json")
			live := &req{method: "POST", url: &url.URL{Path: "/q"}, header: h, body: []byte(`{"a":1}`), raw: []byte("POST /q HTTP/1.1\r\nHost: api\r\n\r\n{\"a\":1}")}
			ok, stub, _, err := newHTTP().match(context.Background(), live, db, nil, nil, nil, true, false, false, false, true)
			if err != nil || !ok || stub == nil || stub.Name != "second" {
				t.Fatalf("match: ok %v, served %v, err %v; want the second recording", ok, stub, err)
			}
		})
	}
}

// refusesEveryClaimOnce is mockMemDb whose store refuses every claim and
// keeps the claims in order. A second claim of one recording cancels the
// call's context: a match that claims again a recording it was refused ends
// there, with the context's error, instead of spinning until go test's
// -timeout.
type refusesEveryClaimOnce struct {
	*mockMemDb
	claimed []*models.Mock
	cancel  context.CancelFunc
}

func (d *refusesEveryClaimOnce) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	for _, c := range d.claimed {
		if c == old {
			d.cancel()
		}
	}
	d.claimed = append(d.claimed, old)
	return false
}

// Every recording a call's claim failed on stays out of its later passes,
// not only the latest: two recordings the store hands out and refuses are
// each claimed once, and the call ends in a miss naming the second. A call
// that left out only its latest refusal would claim the first again, and the
// two would take turns forever. The exact-body pass, and the strict pass
// under the proven switch.
func TestEveryRecordingTheStoreRefusedStaysLeftOut(t *testing.T) {
	for _, proven := range []bool{false, true} {
		name := "exact-body"
		if proven {
			name = "strict"
		}
		t.Run(name, func(t *testing.T) {
			if proven {
				t.Setenv("KEPLOY_PROVEN", "1")
			} else {
				t.Setenv("KEPLOY_PROVEN", "")
			}
			rec := func(name string) *models.Mock {
				return &models.Mock{
					Name: name,
					Kind: models.Kind(models.HTTP),
					Spec: models.MockSpec{
						HTTPReq: &models.HTTPReq{
							Method: "POST", URL: "http://api/q", ProtoMajor: 1, ProtoMinor: 1,
							Header: map[string]string{"Content-Type": "application/json", "Host": "api"},
							Body:   `{"a":1}`,
						},
						HTTPResp: &models.HTTPResp{StatusCode: 200, Body: name},
					},
					TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
				}
			}
			first, second := rec("first"), rec("second")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			db := &refusesEveryClaimOnce{mockMemDb: &mockMemDb{mocks: []*models.Mock{first, second}}, cancel: cancel}
			h := http.Header{}
			h.Set("Host", "api") // under the proven switch the authority is compared
			h.Set("Content-Type", "application/json")
			live := &req{method: "POST", url: &url.URL{Path: "/q"}, header: h, body: []byte(`{"a":1}`), raw: []byte("POST /q HTTP/1.1\r\nHost: api\r\n\r\n{\"a\":1}")}
			ok, stub, diag, err := newHTTP().match(ctx, live, db, nil, nil, nil, true, false, false, false, true)
			if err != nil || ok || stub != nil {
				t.Fatalf("match: ok %v, served %v, err %v, claims %v; want a miss, each recording claimed once", ok, stub != nil, err, mockNamesOf(db.claimed))
			}
			if len(db.claimed) != 2 || db.claimed[0] == db.claimed[1] {
				t.Fatalf("claims %v; want each of the two recordings claimed once", mockNamesOf(db.claimed))
			}
			if diag == nil || diag.refused != db.claimed[1] || diag.phase != models.MatchPhaseExhausted || diag.candidates != 2 {
				t.Fatalf("diag %+v; want the last refused recording, phase %s, 2 candidates", diag, models.MatchPhaseExhausted)
			}
		})
	}
}

// refusingBackground is varDb as a store whose consume door refuses its call
// every recording (integrations.RefusesAnswer: the background's recordings,
// the call being one of the open test's own).
type refusingBackground struct {
	*varDb
}

func (d *refusingBackground) HideBackgroundWhen(func() bool) integrations.MockMemDb { return d }
func (d *refusingBackground) HidesBackground() bool                                 { return true }
func (d *refusingBackground) RefusesAnswer(*models.Mock) bool                       { return true }

// A recording with variables the store refuses the call before its consume
// is left out of the call's later passes too: the call ends in a miss. Before,
// the pass asked for the call to be checked again with the recording still in
// it, and a store that went on offering it spun the call.
func TestAVariableRecordingTheStoreRefusesTheCallEndsIt(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	create := createAndReadBack("ord-rec-1")[0]
	create.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimeSession}
	db := &refusingBackground{varDb: &varDb{session: []*models.Mock{create}}}
	ok, stub, _, err := newHTTP().match(context.Background(), liveReq("POST", "/orders", `{"id":"l1","item":"pen"}`), db, nil, nil, nil, true, false, false, true, true)
	if err != nil || ok || stub != nil {
		t.Fatalf("match: ok %v, served %v, err %v; want a miss", ok, stub != nil, err)
	}
}

// refusesNamed is varDb whose store hands out the recording named refused
// and refuses every claim of it, in either tier, counting them; the others
// it serves.
type refusesNamed struct {
	*varDb
	refused string
	claims  int
}

func (d *refusesNamed) DeleteFilteredMock(m models.Mock) bool {
	return m.Name != d.refused && d.varDb.DeleteFilteredMock(m)
}

// UpdateUnFilteredMock is the door every claim the store refuses ends at
// (a per-test recording's claim falls back to it): each refused claim is
// counted once there.
func (d *refusesNamed) UpdateUnFilteredMock(old, nw *models.Mock) bool {
	if old.Name == d.refused {
		d.claims++
		return false
	}
	return d.varDb.UpdateUnFilteredMock(old, nw)
}

// Under the proven switch a call a recording with variables fits, but whose
// variables do not hold, is left to an exact recording of the call alone. A
// claim of that recording the store refuses leaves it out of the call's
// later passes: the next copy answers the call, and with none the call
// misses after the one refusal. Each waits for the match to return; a hang
// ends in go test's own -timeout, which names the row.
func TestAnExactRecordingRefusedAfterAVariableDidNotHoldIsLeftOut(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name   string
		copies int
		want   string
	}{
		{name: "only-copy", copies: 1},
		{name: "next-copy", copies: 2, want: "exact-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHTTP()
			mocks := createAndReadBack("4f2a9c71")
			for i := 1; i <= tc.copies; i++ {
				mocks = append(mocks, at(varMock(fmt.Sprintf("exact-%d", i), "GET", "http://store/orders/77d01b3e", "", 200, `{"id":"77d01b3e"}`, nil), 2+i))
			}
			db := &refusesNamed{varDb: &varDb{perTest: mocks}, refused: "exact-1"}
			if ok, stub, _, err := h.match(context.Background(), liveReq("POST", "/orders", `{"id":"9b0e66aa","item":"pen"}`), db, nil, nil, nil, true, false, false, true, true); err != nil || !ok || stub.Name != "mock-1" {
				t.Fatalf("the create: ok %v, served %v, err %v; want mock-1", ok, stub, err)
			}
			ok, stub, diag, err := h.match(context.Background(), liveReq("GET", "/orders/77d01b3e", ""), db, nil, nil, nil, true, false, false, true, true)
			if err != nil {
				t.Fatal(err)
			}
			if db.claims != 1 {
				t.Fatalf("the store refused %d claims of exact-1; want the one", db.claims)
			}
			if tc.want == "" {
				wantProblem(t, ok, diag, "this call sent")
				return
			}
			if !ok || stub.Name != tc.want {
				t.Fatalf("served %v (ok %v); want %s", stub, ok, tc.want)
			}
		})
	}
}

// refusesNamedGen is the strict index's store (ownGenDb) refusing every
// claim of the recording named refused, in either tier, counting them as
// refusesNamed does, while it goes on handing it out (never reported spent).
type refusesNamedGen struct {
	*ownGenDb
	refused string
	claims  int
}

func (d *refusesNamedGen) DeleteFilteredMock(m models.Mock) bool {
	return m.Name != d.refused && d.ownGenDb.DeleteFilteredMock(m)
}

func (d *refusesNamedGen) UpdateUnFilteredMock(old, nw *models.Mock) bool {
	if old.Name == d.refused {
		d.claims++
		return false
	}
	return d.ownGenDb.UpdateUnFilteredMock(old, nw)
}

// refusesNamedGenVar is the strict index's store of recordings with
// variables (genVarDb) refusing every claim of the recording named refused,
// in either tier, counting them as refusesNamed does.
type refusesNamedGenVar struct {
	*genVarDb
	refused string
	claims  int
}

func (d *refusesNamedGenVar) DeleteFilteredMock(m models.Mock) bool {
	return m.Name != d.refused && d.genVarDb.DeleteFilteredMock(m)
}

func (d *refusesNamedGenVar) UpdateUnFilteredMock(old, nw *models.Mock) bool {
	if old.Name == d.refused {
		d.claims++
		return false
	}
	return d.genVarDb.UpdateUnFilteredMock(old, nw)
}

// A recording the strict index lists whose claim the store refuses is
// claimed once through the index and once from the store, which the call
// reads from then on and which leaves it out after its refusal: the call
// misses after two refusals, never going back to the index. The strict pass
// and the variable pass through the index, each with the store still
// handing the recording out. Each waits for the match to return; a hang
// ends in go test's own -timeout, which names the row.
func TestARecordingRefusedThroughTheIndexIsClaimedOnceMoreFromTheStore(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	verifyOff(t)
	t.Run("strict", func(t *testing.T) {
		h := newHTTP()
		db := &refusesNamedGen{ownGenDb: newOwnGenDb(strictItems("mock", 10), nil), refused: "mock-5"}
		strictMatchOf(t, h, strictCall(0), db, db, nil)
		strictMatchOf(t, h, strictCall(1), db, db, nil) // the generation's second call builds its index
		if s := strictMatchOf(t, h, strictCall(5), db, db, nil); s != nil {
			t.Fatalf("served %s, whose every claim the store refuses", s.Name)
		}
		if db.claims != 2 {
			t.Fatalf("the store refused %d claims of mock-5; want 2, through the index and from the store", db.claims)
		}
	})
	t.Run("variables", func(t *testing.T) {
		h := newHTTP()
		db := &refusesNamedGenVar{genVarDb: &genVarDb{varDb: &varDb{perTest: append(strictItems("mock", 10), orderBind())}, gen: 1}, refused: "bind"}
		strictMatchOf(t, h, strictCall(0), db, db, nil)
		strictMatchOf(t, h, strictCall(1), db, db, nil) // the generation's second call builds its index
		if s := strictMatchOf(t, h, liveReq("POST", "http://"+strictIxHost+"/orders", `{"id":"ord-live-9"}`), db, db, nil); s != nil {
			t.Fatalf("served %s, whose every claim the store refuses", s.Name)
		}
		if db.claims != 2 {
			t.Fatalf("the store refused %d claims of bind; want 2, through the index and from the store", db.claims)
		}
	})
}

// refilesOnce hands out its recording and, at the first claim, files a new
// copy of it (same name, new pointer) in its place and refuses the claim of
// the old one, as when another call's serve filed the session mock again
// first. The copy's claim goes through.
type refilesOnce struct {
	*mockMemDb
	refiled bool
}

func (d *refilesOnce) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	if !d.refiled {
		d.refiled = true
		cp := *old
		for i, m := range d.mocks {
			if m == old {
				d.mocks[i] = &cp
			}
		}
		return false
	}
	return true
}

// refilesOnceVar is refilesOnce over varDb's session region, which the
// variable pass reads.
type refilesOnceVar struct {
	*varDb
	refiled bool
}

func (d *refilesOnceVar) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	if !d.refiled {
		d.refiled = true
		cp := *old
		for i, m := range d.session {
			if m == old {
				d.session[i] = &cp
			}
		}
		return false
	}
	return true
}

// Two calls race on one session mock: the winner's serve files a new copy
// under the same name, and the loser's claim of the old copy fails. The
// failed claim leaves out that copy, by pointer, never the recording by
// name, so the new copy answers the loser, as before a refused claim was
// left out. One row per pass that claims; the variable pass under the
// proven switch.
func TestARefiledCopyOfARefusedRecordingAnswersTheCall(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, live, contentType string
		correlations                      []models.FieldCorrelation
		proven                            bool
	}{
		{name: "exact-body", recorded: `{"a":1}`, live: `{"a":1}`, contentType: "application/json"},
		{name: "correlation", recorded: `{"id":"r-1","a":1}`, live: `{"id":"l-1","a":1}`, contentType: "application/json",
			correlations: []models.FieldCorrelation{{RequestPath: "body.id", ResponsePaths: []string{"body.id"}, RecordedValue: "r-1"}}},
		{name: "json-body", recorded: `{"a":1}`, live: `{"a":2}`, contentType: "application/json"},
		{name: "fuzzy", recorded: "a=1&b=xyz", live: "a=1&b=xyw", contentType: "text/plain"},
		{name: "strict", recorded: `{"a":1}`, live: `{"a":1}`, contentType: "application/json", proven: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				t.Setenv("KEPLOY_PROVEN", "1")
			} else {
				t.Setenv("KEPLOY_PROVEN", "")
			}
			m := &models.Mock{
				Name: "rec",
				Kind: models.Kind(models.HTTP),
				Spec: models.MockSpec{
					HTTPReq: &models.HTTPReq{
						Method: "POST", URL: "http://api/q", ProtoMajor: 1, ProtoMinor: 1,
						Header: map[string]string{"Content-Type": tc.contentType, "Host": "api"},
						Body:   tc.recorded,
					},
					HTTPResp:     &models.HTTPResp{StatusCode: 200, Body: "ok"},
					Correlations: tc.correlations,
				},
				TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession},
			}
			db := &refilesOnce{mockMemDb: &mockMemDb{mocks: []*models.Mock{m}}}
			h := http.Header{}
			h.Set("Host", "api")
			h.Set("Content-Type", tc.contentType)
			live := &req{method: "POST", url: &url.URL{Path: "/q"}, header: h, body: []byte(tc.live), raw: []byte("POST /q HTTP/1.1\r\nHost: api\r\n\r\n" + tc.live)}
			ok, stub, _, err := newHTTP().match(context.Background(), live, db, nil, nil, nil, true, false, false, true, true)
			if err != nil || !ok || stub == nil || stub.Name != "rec" || !db.refiled {
				t.Fatalf("match: ok %v, served %v, err %v, refiled %v; want the new copy of rec", ok, stub, err, db.refiled)
			}
		})
	}
	t.Run("variable", func(t *testing.T) {
		t.Setenv("KEPLOY_PROVEN", "1")
		create := createAndReadBack("ord-rec-1")[0]
		create.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimeSession}
		db := &refilesOnceVar{varDb: &varDb{session: []*models.Mock{create}}}
		ok, stub, _, err := newHTTP().match(context.Background(), liveReq("POST", "/orders", `{"id":"l1","item":"pen"}`), db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub == nil || stub.Name != create.Name || !db.refiled {
			t.Fatalf("match: ok %v, served %v, err %v, refiled %v; want the new copy of %s", ok, stub, err, db.refiled, create.Name)
		}
	})
}

// refusesOneVar is varDb whose store refuses the claims of one recording
// only, and keeps the claims in order.
type refusesOneVar struct {
	*varDb
	refused *models.Mock
	claimed []*models.Mock
}

func (d *refusesOneVar) UpdateUnFilteredMock(old, nw *models.Mock) bool {
	d.claimed = append(d.claimed, old)
	return old != d.refused && d.varDb.UpdateUnFilteredMock(old, nw)
}

// A pass whose pick is a stateful cursor's, not its first fit, leaves out the
// recording it picked and the store refused, never its first fit: a call of
// two recordings of one request (ConsumeCursorSaturate) whose first has
// answered picks the second; the store refuses it, and the next pass answers
// with the first, the one recording left. Leaving out the first fit instead
// would pick the refused second again and claim it a second time. One row per
// pass whose pick is a cursor's: the exact-body pass, the strict pass and the
// variable pass under the proven switch. The store refuses the second every
// time, so the call ends either way; the claims say which was left out.
func TestARefusedCursorPickIsLeftOutNotTheFirstFit(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	rec := func(name string, at time.Time) *models.Mock {
		return &models.Mock{
			Name: name,
			Kind: models.Kind(models.HTTP),
			Spec: models.MockSpec{
				HTTPReq: &models.HTTPReq{
					Method: "POST", URL: "http://api/q", ProtoMajor: 1, ProtoMinor: 1,
					Header: map[string]string{"Content-Type": "application/json", "Host": "api"},
					Body:   `{"a":1}`,
				},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: name},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
			},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, Consume: models.ConsumeCursorSaturate},
		}
	}
	echo := func(name, key string, seq int) *models.Mock {
		return reusable(at(varMock(name, "POST", "http://pay/charge", `{"key":"`+key+`"}`, 200, `{"echo":"`+key+`"}`,
			models.MockVariables{"key": {Recorded: key, Bind: "body.key", Answer: []string{"body.echo"}, Echo: true}}), seq))
	}
	for _, tc := range []struct {
		name   string
		proven bool
		vars   bool
	}{
		{name: "exact-body"},
		{name: "strict", proven: true},
		{name: "variable", proven: true, vars: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				t.Setenv("KEPLOY_PROVEN", "1")
			} else {
				t.Setenv("KEPLOY_PROVEN", "")
			}
			var (
				first, second *models.Mock
				db            integrations.MockMemDb
				claimed       func() []*models.Mock
				live          *req
			)
			if tc.vars {
				first, second = echo("first", "k-1", 1), echo("second", "k-2", 2)
				vdb := &refusesOneVar{varDb: &varDb{session: []*models.Mock{first, second}}, refused: second}
				// The first recording has answered: the cursor is on the second.
				vdb.cursors = map[string]int{servedKey(first): 1}
				db, claimed, live = vdb, func() []*models.Mock { return vdb.claimed }, liveReq("POST", "/charge", `{"key":"x-1"}`)
			} else {
				first, second = rec("first", t0), rec("second", t0.Add(time.Second))
				mdb := &refusesOne{mockMemDb: &mockMemDb{mocks: []*models.Mock{first, second}}, refused: second}
				// The first recording has answered: the cursor is on the
				// second, for the exact-body pass's group and the strict
				// pass's served marks alike.
				_, key, _ := cursorGroup(first, []*models.Mock{first, second}, func(*models.Mock) bool { return false })
				mdb.cursors = map[string]int{servedKey(first): 1, key: 1}
				h := http.Header{}
				h.Set("Host", "api")
				h.Set("Content-Type", "application/json")
				db, claimed = mdb, func() []*models.Mock { return mdb.claimed }
				live = &req{method: "POST", url: &url.URL{Path: "/q"}, header: h, body: []byte(`{"a":1}`), raw: []byte("POST /q HTTP/1.1\r\nHost: api\r\n\r\n{\"a\":1}")}
			}
			ok, stub, _, err := newHTTP().match(context.Background(), live, db, nil, nil, nil, true, false, false, true, true)
			if got := mockNamesOf(claimed()); strings.Join(got, " ") != "second first" {
				t.Fatalf("claims %v; want the cursor's pick (second), refused, then the first: the refused pick is never claimed again", got)
			}
			if err != nil || !ok || stub == nil || stub.Name != "first" {
				t.Fatalf("match: ok %v, served %v, err %v; want the first recording", ok, stub, err)
			}
		})
	}
}
