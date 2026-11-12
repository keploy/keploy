package http

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

// learn "lead r-1 created" -> "lead r-2 created" (word 1), write
// the learned recording to disk the way keploy does (mockdb.EncodeMock), read
// it back (mockdb.DecodeMocks) as the next keploy test does, and replay
// "deal r-3 deleted". It must be refused.
func TestAWordsPatternReadBackFromDiskStillRefusesOtherWords(t *testing.T) {
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	body := func(note string) string { return `{"note":"` + note + `","item":"book"}` }
	rec := learnRecorded("mock-0", 0, "POST", "/v1/leads", body("lead r-1 created"), nil, `{"ok":true}`)
	db := &learnDb{perTest: []*models.Mock{rec}}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/leads", body("lead r-2 created"), nil), db, n, opts); err != nil || served == nil {
		t.Fatalf("record's full round refused: %+v %v", miss, err)
	}
	mocks := learnedOnto([]*models.Mock{rec}, db.consumed)
	t.Logf("in memory: %#v", mocks[0].Spec.ReqBodyNoise)

	// In memory: refused (the PR's row).
	if ok, _, _ := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: []*models.Mock{mocks[0]}}, live("POST", "/v1/leads", body("deal r-3 deleted"), nil)); ok {
		t.Fatalf("in memory: deal r-3 deleted answered")
	}

	doc, err := mockdb.EncodeMock(mocks[0], zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	back, err := mockdb.DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop())
	if err != nil || len(back) != 1 {
		t.Fatalf("decode %v %v", back, err)
	}
	back[0].TestModeInfo = mocks[0].TestModeInfo
	t.Logf("read back from disk: %#v", back[0].Spec.ReqBodyNoise)
	if ok, _, _ := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: []*models.Mock{back[0]}}, live("POST", "/v1/leads", body("lead r-3 created"), nil)); !ok {
		t.Fatalf("read back: lead r-3 created refused (control)")
	}
	if ok, _, _ := matchVarIn(t, context.Background(), newHTTP(), &varDb{perTest: []*models.Mock{back[0]}}, live("POST", "/v1/leads", body("deal r-3 deleted"), nil)); ok {
		t.Fatalf("FALSE GREEN: after the write to disk and the read back, deal r-3 deleted is answered (noise read back %#v)", back[0].Spec.ReqBodyNoise)
	}
}
