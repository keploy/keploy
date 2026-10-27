package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// spilled is m as the agent's disk store keeps a per-test mock whose answer is big (proxy.EligibleForResponseSpill):
// its response apart from it, loaded only when it is served.
func spilled(m *models.Mock) *models.Mock {
	resp := m.Spec.HTTPResp
	m.Spec.HTTPResp = nil
	m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) { return resp, nil, nil })
	return m
}

// tr/page.spec.ts:14 (atg71 round F): the test makes an app "tr-empty-<Date.now>", the app list (MyApps, the same
// request every time) answers it whole among the org's other apps, and so does AppById. The list's answer is over
// 8 KB, so the disk store keeps it apart from the mock: the later lists are answered with this run's name all the
// same, and learn it, as AppById's small answer does.
func TestLearnMatchABigListAnswerHoldsTheAppTheTestMade(t *testing.T) {
	const recTr, fTr = "tr-empty-1792264245842", "tr-empty-1792265196426"
	list := `{"operationName":"MyApps","variables":{}}`
	others := strings.Repeat(`{"id":"a0","name":"defaultapp","createdBy":{"email":"user@example.com"}},`, 120)
	before := `{"data":{"apps":[` + strings.TrimSuffix(others, ",") + `]}}`
	after := `{"data":{"apps":[` + others + `{"id":"app-1","name":"` + recTr + `"}]}}`
	if len(after) < 8*1024 {
		t.Fatalf("the list answer is %d bytes, not one the disk store keeps apart", len(after))
	}
	recs := []*models.Mock{
		gql("mock-0", 0, list, before),
		gql("mock-1", 1, `{"operationName":"CreateATGApp","variables":{"name":"`+recTr+`"}}`, `{"data":{"createATGApp":"app-1"}}`),
		spilled(gql("mock-2", 2, list, after)),
		spilled(gql("mock-3", 3, list, after)),
		gql("mock-4", 4, `{"operationName":"AppById","variables":{"id":"app-1"}}`, `{"data":{"appById":{"id":"app-1","name":"`+recTr+`"}}}`),
	}
	calls := []*req{
		gqlLive(list),
		gqlLive(`{"operationName":"CreateATGApp","variables":{"name":"` + fTr + `"}}`),
		gqlLive(list),
		gqlLive(list),
		gqlLive(`{"operationName":"AppById","variables":{"id":"app-1"}}`),
	}
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	var per []*models.Mock
	for _, r := range recs {
		cp := *r
		per = append(per, &cp)
	}
	db := &learnDb{perTest: per, observe: tr.ServedMock}
	for i, c := range calls {
		out, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
		if i >= 2 && (!strings.Contains(out.Spec.HTTPResp.Body, fTr) || strings.Contains(out.Spec.HTTPResp.Body, recTr)) {
			t.Errorf("F call %d (%s) was answered with the recorded name, not this run's", i, out.Name)
		}
	}
	for _, m := range db.consumed[2:] {
		if v, ok := m.Spec.Learned()["name"]; !ok || v.Recorded != recTr || len(v.Answer) != 1 {
			t.Errorf("%s learned %+v: the name is not an answer place of it", m.Name, m.Spec.Learned())
		}
	}

	// The replays after F: the variable pass answers the big lists with that run's name.
	const ciTr = "tr-empty-1792269999999"
	mocks := learnedOnto(recs, db.consumed)
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	for i, c := range []*req{
		gqlLive(list),
		gqlLive(`{"operationName":"CreateATGApp","variables":{"name":"` + ciTr + `"}}`),
		gqlLive(list),
		gqlLive(list),
		gqlLive(`{"operationName":"AppById","variables":{"id":"app-1"}}`),
	} {
		ok, stub, diag := matchVar(t, h, ci, c)
		if i < 2 {
			if !ok {
				t.Fatalf("CI call %d: %+v", i, diag)
			}
			continue
		}
		if !ok || !strings.Contains(stub.Spec.HTTPResp.Body, ciTr) || strings.Contains(stub.Spec.HTTPResp.Body, recTr) {
			t.Fatalf("CI call %d: ok %v diag %+v", i, ok, diag)
		}
	}
}
