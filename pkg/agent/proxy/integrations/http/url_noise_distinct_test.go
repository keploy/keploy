package http

import (
	"maps"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Two whole-path url-noise entries (such as "^/users/[0-9]+$" and
// "^/orders/[^/]+$", or "^/users/[0-9]+$" and "^/v1\\.2/files/[^/]+$") must
// never collapse distinct routes onto one placeholder: each route matches only
// recordings of its own pattern, and a route with no recording is missed in
// both KEPLOY_PROVEN modes.
func TestTwoWholePathEntriesDoNotCollapseRoutes(t *testing.T) {
	entryFor := func(rec, live string) map[string][]string {
		setProven(t, false)
		h := newHTTP()
		db := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
			recordedAs(t, h, "m", 1, getTarget(rec), jsonAnswer(200, `{}`), nil),
		}}
		a := serveReported(t, h, db, getTarget(live)).answers()
		if len(a) != 1 {
			t.Fatalf("%s: %+v", live, a)
		}
		return keployYmlGlobalNoise(t, a[0].NextSteps, "url")["url"]
	}
	noise := map[string][]string{}
	maps.Copy(noise, entryFor("/users/1", "/users/3"))
	maps.Copy(noise, entryFor("/orders/ord_8ab071aa4d5a130a", "/orders/ord_dcce109751713323"))

	db := func(t *testing.T, h *HTTP) *mockMemDb {
		return &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
			recordedAs(t, h, "mock-user-1", 1, getTarget("/users/1"), jsonAnswer(200, `{"user":1}`), nil),
			recordedAs(t, h, "mock-order-a", 2, getTarget("/orders/ord_8ab071aa4d5a130a"), jsonAnswer(200, `{"order":"A"}`), nil),
		}}
	}
	for _, proven := range []bool{false, true} {
		for _, tc := range []struct {
			call, wantBody string
		}{
			{"/orders/ord_ffff109751713323", `{"order":"A"}`},
			{"/users/9", `{"user":1}`},
		} {
			setProven(t, proven)
			h := newHTTP()
			got := serveReportedWith(t, h, db(t, h), models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": noise}}, getTarget(tc.call))
			if got.status != 200 || got.body != tc.wantBody || len(got.reports) != 0 {
				t.Errorf("proven=%v GET %s -> %d %q reports=%d; want 200 %q with no report", proven, tc.call, got.status, got.body, len(got.reports), tc.wantBody)
			}
		}
	}

	// Only /users/1 recorded: GET /orders/<new> must miss in both modes.
	for _, proven := range []bool{false, true} {
		setProven(t, proven)
		h := newHTTP()
		one := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
			recordedAs(t, h, "mock-user-1", 1, getTarget("/users/1"), jsonAnswer(200, `{"user":1}`), nil),
		}}
		got := serveReportedWith(t, h, one, models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": noise}}, getTarget("/orders/ord_ffff109751713323"))
		if got.status == 200 {
			t.Errorf("proven=%v: only /users/1 recorded, GET /orders/<new> answered %d %q reports=%d; want miss", proven, got.status, got.body, len(got.reports))
		}
	}

	// Corpus pair: ^/users/[0-9]+$ and ^/v1\.2/files/[^/]+$.
	corpusNoise := map[string][]string{
		`^/users/[0-9]+$`:      {},
		`^/v1\.2/files/[^/]+$`: {},
	}
	for _, proven := range []bool{false, true} {
		setProven(t, proven)
		h := newHTTP()
		one := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
			recordedAs(t, h, "mock-user-1", 1, getTarget("/users/1"), jsonAnswer(200, `{"user":1}`), nil),
		}}
		got := serveReportedWith(t, h, one, models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": corpusNoise}}, getTarget("/v1.2/files/b"))
		if got.status == 200 {
			t.Errorf("proven=%v: corpus pair answered GET /v1.2/files/b with GET /users/1 (%d %q)", proven, got.status, got.body)
		}
	}
}

// NewRequestNoise sorts url-noise keys deterministically so the per-entry
// placeholders and masking order never vary across runs.
func TestNewRequestNoiseSortsURLNoiseDeterministically(t *testing.T) {
	want := []string{`^/orders/[^/]+$`, `^/users/[0-9]+$`, `^/v1\.2/files/[^/]+$`}
	in := map[string][]string{
		`^/v1\.2/files/[^/]+$`: {},
		`^/users/[0-9]+$`:      {},
		`^/orders/[^/]+$`:      {},
	}
	for i := range 50 {
		got := NewRequestNoise(zap.NewNop(), models.OutgoingOptions{
			DisableAutoHeaderNoise: true,
			NoiseConfig:            map[string]map[string][]string{"url": in},
		})
		if !slices.Equal(got.URL, want) {
			t.Fatalf("iter %d: URL = %q; want sorted %q", i, got.URL, want)
		}
	}
}
