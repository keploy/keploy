package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
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
