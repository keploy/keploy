package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// timedVarDb is varDb in a replay that turned the time default on or off
// (integrations.TimeFormReader), as keploy.yml's times setting says.
type timedVarDb struct {
	*varDb
	timeForm bool
}

func (d timedVarDb) TimeForm() bool { return d.timeForm }

// A time the application stamped, bound whole at an earlier call and used at
// a later one, that the learn step gave a check by form at its use
// (models.MockVariable.ByForm): two reads of the clock that fell in one
// second when recorded and in two in this replay pass by form under the time
// default, and only there. A use with no such check, an ObjectId, a value of
// another form, and a replay under strict times still need the bound value.
func TestVariables_ATimeWhoseBindDoesNotHoldInAReplayIsCheckedByItsForm(t *testing.T) {
	const (
		recCreated  = "2026-11-11T18:35:46Z"
		liveCreated = "2026-11-12T01:00:00Z"
		recID       = "6732a1b2c3d4e5f601234567"
		liveID      = "6732f0f0c3d4e5f601234999"
	)
	form := learn.TimeForm(learn.Leaf{Type: learn.String, Value: recCreated})
	byForm := map[string]models.ClockOffset{"body.createdAt": {Form: form, Why: "a time, checked by its form alone where the value it was bound to does not hold"}}
	pool := func(useByForm map[string]models.ClockOffset, clocks map[string]models.ClockOffset) []*models.Mock {
		create := varMock("mock-1", "POST", "http://store/apps", `{"createdAt":"`+recCreated+`","id":"`+recID+`"}`, 201, `{}`,
			models.MockVariables{"appCreated": bindV(recCreated, "body.createdAt"), "appId": bindV(recID, "body.id")})
		created := useV(recCreated, []string{"body.createdAt"})
		created.ByForm = useByForm
		appID := useV(recID, []string{"body.appId"})
		if _, ok := useByForm["body.appId"]; ok {
			appID.ByForm = map[string]models.ClockOffset{"body.appId": useByForm["body.appId"]}
			delete(created.ByForm, "body.appId")
		}
		put := varMock("mock-2", "PUT", "http://store/keys/k1", `{"createdAt":"`+recCreated+`","appId":"`+recID+`","updatedAt":"`+recCreated+`"}`, 200, `{}`,
			models.MockVariables{"appCreated": created, "appId": appID})
		put.Spec.Clocks = clocks
		return []*models.Mock{at(create, 1), at(put, 2)}
	}
	putBody := func(created, id, updated string) string {
		return `{"createdAt":"` + created + `","appId":"` + id + `","updatedAt":"` + updated + `"}`
	}
	for _, tc := range []struct {
		what     string
		byForm   map[string]models.ClockOffset
		clocks   map[string]models.ClockOffset
		timeForm bool
		body     string
		served   bool
		miss     string
	}{
		{what: "the next second, checked by form under the time default", byForm: byForm, timeForm: true, body: putBody("2026-11-12T01:00:01Z", liveID, recCreated), served: true},
		{what: "the bound value, under the time default", byForm: byForm, timeForm: true, body: putBody(liveCreated, liveID, recCreated), served: true},
		{what: "the next second under strict times", byForm: byForm, body: putBody("2026-11-12T01:00:01Z", liveID, recCreated), miss: "this call sent"},
		{what: "the next second at a use with no check by form", timeForm: true, body: putBody("2026-11-12T01:00:01Z", liveID, recCreated), miss: "this call sent"},
		{what: "another form at a use checked by form", byForm: byForm, timeForm: true, body: putBody("2026-11-12 01:00:01Z", liveID, recCreated), miss: "this call sent"},
		{what: "another ObjectId at a use with no check by form", byForm: byForm, timeForm: true, body: putBody(liveCreated, "6732f0f0c3d4e5f60123aaaa", recCreated), miss: "this call sent"},
		{what: "another ObjectId at a use a check by form names", byForm: map[string]models.ClockOffset{"body.appId": {Form: form}}, timeForm: true, body: putBody(liveCreated, "6732f0f0c3d4e5f60123aaaa", recCreated), miss: "this call sent"},
		{what: "a time a clock rule checks by form beside the variables, under the time default", clocks: map[string]models.ClockOffset{"body.updatedAt": {Form: form}}, timeForm: true, body: putBody(liveCreated, liveID, "2026-11-12T01:00:07Z"), served: true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			h := newHTTP()
			db := timedVarDb{varDb: &varDb{perTest: pool(tc.byForm, tc.clocks)}, timeForm: tc.timeForm}
			match := func(r *req) (bool, *models.Mock, *matchDiag) {
				ok, stub, diag, err := h.match(t.Context(), r, db, nil, nil, nil, true, false, false, true, true)
				if err != nil {
					t.Fatalf("match: %v", err)
				}
				return ok, stub, diag
			}
			if ok, stub, _ := match(liveReq("POST", "/apps", `{"createdAt":"`+liveCreated+`","id":"`+liveID+`"}`)); !ok || stub.Name != "mock-1" {
				t.Fatalf("the create should be answered by mock-1: ok=%v stub=%v", ok, stub)
			}
			ok, stub, diag := match(liveReq("PUT", "/keys/k1", tc.body))
			if tc.served {
				if !ok || stub.Name != "mock-2" {
					t.Fatalf("the PUT should be answered by mock-2: ok=%v stub=%v diag=%+v", ok, stub, diag)
				}
				return
			}
			wantProblem(t, ok, diag, tc.miss)
		})
	}
}
