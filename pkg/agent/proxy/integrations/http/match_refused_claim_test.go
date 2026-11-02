package http

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

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
			ok, stub, _, err := newHTTP().match(context.Background(), live, db, nil, nil, nil, true, false, false, true, true)
			if err != nil || ok || stub != nil {
				t.Fatalf("match: ok %v, served %v, err %v; want a miss", ok, stub != nil, err)
			}
			if db.updateUnFilteredMockOld != m {
				t.Fatalf("the pass under test never claimed the recording (claimed %v): the row does not reach its claim", db.updateUnFilteredMockOld)
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
// only, as when another call took that copy first.
type refusesOne struct {
	*mockMemDb
	refused *models.Mock
}

func (d *refusesOne) UpdateUnFilteredMock(old, _ *models.Mock) bool { return old != d.refused }

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
