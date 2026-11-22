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

// Every per-entry url-noise placeholder must be distinct and contain no digits
// so queryValueTypeClass and urlSegmentTypeClass never classify a placeholder
// as a dynamic token.
func TestURLNoisePlaceholdersAreDistinctAndNeverClassAsIDs(t *testing.T) {
	seen := map[string]int{}
	for i := 0; i <= 1000; i++ {
		p := urlNoisePlaceholderOf(i)
		if i == 0 && p != urlNoisePlaceholder {
			t.Fatalf("index 0 placeholder = %q, want %q", p, urlNoisePlaceholder)
		}
		if prev, dup := seen[p]; dup {
			t.Fatalf("placeholder collision between %d and %d: %q", prev, i, p)
		}
		seen[p] = i
		if cls := queryValueTypeClass(p); cls != "" {
			t.Fatalf("index %d placeholder %q classified as query class %q, want \"\"", i, p, cls)
		}
		if cls := urlSegmentTypeClass(p); cls != "" {
			t.Fatalf("index %d placeholder %q classified as path class %q, want \"\"", i, p, cls)
		}
	}
}

// A url-noise entry that sorts at index >= 1 must behave identically to one at
// index 0 in QueryParamsMatch: its placeholder must never be relaxed by pass 2
// against another entry's placeholder or against an unmasked token.
func TestMultipleURLNoiseEntriesDoNotRelaxDistinctQueryTokensInPass2(t *testing.T) {
	noise := map[string][]string{
		`^/users/[0-9]+$`:    {},
		`^inv_[0-9a-f]{16}$`: {},
		`^ord_[0-9a-f]{16}$`: {},
	}
	opts := models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": noise}}

	for _, proven := range []bool{false, true} {
		setProven(t, proven)
		// Same pattern still answers as its own.
		h := newHTTP()
		dbOwn := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
			recordedAs(t, h, "mock-ord", 1, getTarget("/orders?ref=ord_8ab071aa4d5a130a"), jsonAnswer(200, `{"order":"A"}`), nil),
		}}
		own := serveReportedWith(t, h, dbOwn, opts, getTarget("/orders?ref=ord_dcce109751713323"))
		if own.status != 200 || len(own.reports) != 0 {
			t.Errorf("proven=%v same pattern: got %d reports=%d; want 200 with no report", proven, own.status, len(own.reports))
		}

		// Distinct patterns or masked vs unmasked token must miss (502) in both modes.
		for _, tc := range []struct{ name, rec, live string }{
			{"two distinct entries", "/orders?ref=ord_8ab071aa4d5a130a", "/orders?ref=inv_8ab071aa4d5a130a"},
			{"masked recorded vs unmasked token", "/orders?ref=ord_8ab071aa4d5a130a", "/orders?ref=cus_NffrFeUfNV2Hib12"},
			{"unmasked token vs masked live", "/orders?ref=cus_NffrFeUfNV2Hib12", "/orders?ref=ord_8ab071aa4d5a130a"},
		} {
			h := newHTTP()
			db := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
				recordedAs(t, h, "mock-a", 1, getTarget(tc.rec), jsonAnswer(200, `{"order":"A"}`), nil),
			}}
			got := serveReportedWith(t, h, db, opts, getTarget(tc.live))
			if got.status == 200 {
				t.Errorf("proven=%v %s: got %d %q reports=%+v; want 502 miss", proven, tc.name, got.status, got.body, got.reports)
			}
		}
	}
}
