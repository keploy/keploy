package mockdb

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Under the proven switch a test owns the mocks its mapping lists, whatever
// their recorded times say, so a gob file's per-test pool is read as a yaml or
// json file's is: whole, for the agent to serve by mapping. The reader's
// window filter dropped, before the agent ever saw them, a mock whose response
// is stamped before its request (a recorder's bad stamp, not a bad answer) and
// one recorded outside the window it was asked for: the test that owns it was
// judged for a recorded call it did not make, and its call to that dependency
// missed. With the switch off a gob file's pool is still window filtered, as
// it was.
func TestAGobFilesMockAnsweredBeforeItWasAskedIsServedByMappingUnderTheSwitch(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	at := time.Unix(1_700_000_000, 0).UTC()
	mock := func(req, res time.Duration) *models.Mock {
		return &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks", "lifetime": "perTest"},
				HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://dep/x", ProtoMajor: 1, ProtoMinor: 1},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "ok"},
				ReqTimestampMock: at.Add(req),
				ResTimestampMock: at.Add(req + res),
			},
		}
	}
	ys := New(zap.NewNop(), dir, "mocks")
	// Answered before it was asked; answered after; and one recorded after
	// the window the reader is asked for, which its test owns by its mapping
	// all the same.
	for _, m := range []*models.Mock{mock(0, -37*time.Microsecond), mock(0, time.Millisecond), mock(2*time.Hour, time.Millisecond)} {
		if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
			t.Fatalf("InsertMock: %v", err)
		}
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	read := func() []*models.Mock {
		t.Helper()
		got, err := New(zap.NewNop(), dir, "mocks").GetFilteredMocks(context.Background(), "set-0", at.Add(-time.Hour), at.Add(time.Hour), nil, nil)
		if err != nil {
			t.Fatalf("GetFilteredMocks: %v", err)
		}
		return got
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if got := read(); len(got) != 3 {
		t.Errorf("under the switch the per-test pool holds %d mocks; want all 3, the one answered before it was asked and the one outside the window too", len(got))
	}
	t.Setenv("KEPLOY_PROVEN", "")
	if got := read(); len(got) != 1 || got[0].Spec.ResTimestampMock.Before(got[0].Spec.ReqTimestampMock) {
		t.Errorf("with the switch off the per-test pool is %d mocks; want the one answered after it was asked, as before", len(got))
	}
}
