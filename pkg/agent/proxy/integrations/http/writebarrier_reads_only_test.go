package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func gqlMock(name, body string, ts time.Time) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Body: body, Header: map[string]string{"Content-Type": "application/json"}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{}`},
			ReqTimestampMock: ts,
		},
	}
}

// group-effect:388: a "group" barrier armed by a deleteGroup recorded LATER
// (served early after a worker restart) dropped the test's own SeedGroup copy.
// The barrier is about stale READS; a write must be matched as before.
func TestBarrierNeverFiltersAWrite(t *testing.T) {
	t0 := time.Unix(1000, 0)
	seed := gqlMock("seed-group", gqlCreateGroup, t0)
	read := gqlMock("groups-later", gqlGetAllGroups, t0.Add(10*time.Second))
	db := &mockMemDb{mocks: []*models.Mock{seed, read}, updateUnFilteredReturn: true}
	h := newHTTP()
	h.wb.arm("group", t0.Add(5*time.Second))

	in := liveReq("POST", gqlCreateGroup)
	in.header = map[string][]string{"Content-Type": {"application/json"}}
	ok, got, _, err := h.match(context.Background(), in, db, nil, nil, nil, true, false, false, false, false)
	if err != nil || !ok || got != seed {
		t.Fatalf("ok=%v got=%v err=%v: a write's own copy must not be dropped by the barrier", ok, got, err)
	}
}
