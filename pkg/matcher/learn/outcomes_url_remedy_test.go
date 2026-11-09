package learn_test

import (
	"regexp"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/replay"
)

// A refused change in a url path names a keploy.yml entry, and that entry,
// written as the line says, lets the HTTP matcher serve the call. The test
// sent {"order":"o-1"} (round F: o-9) and a later call asks for
// /v1/orders/o-1 (round F: /v1/orders/o-5): the place held the value and
// round F moved it, so the change is refused. Its line names
// test.globalNoise's url bucket and asks for a regex of the part that
// changes, under global or the test set. The person writes that regex; keploy
// test and keploy mock build the mock matchers' noise from keploy.yml
// (PrepareMockNoiseConfig), the HTTP matcher reads its url bucket
// (NewRequestNoise) and matches the recorded path against the call's
// (MatchURLPath). With the entry the call is served; without it, or under
// another test set, it is not.
func TestOutcomeAURLRefusalsNoiseEntryLetsTheHTTPMatcherServeTheCall(t *testing.T) {
	const (
		field   = "url.path.2"
		mockURL = "http://orders.svc/v1/orders/o-1"
		reqPath = "/v1/orders/o-5"
		// The regex of the part that changes, as the line asks a person to
		// write it: anchored to its path so other ids stay checked.
		changes = `/v1/orders/o-[0-9]+`
	)
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	text := func(path, v string) learn.Leaf {
		return learn.Leaf{Path: path, Noise: path, Type: learn.String, Value: v}
	}
	sent := &models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: at.Add(-2 * time.Second)}}
	dc := learn.NewDriftCache()
	ds := learn.Decide(learn.Input{
		Drifts: dc, Drift: []string{field},
		Rec:  learn.Shape{Leaves: map[string]learn.Leaf{field: text(field, "o-1")}},
		Live: learn.Shape{Leaves: map[string]learn.Leaf{field: text(field, "o-5")}},
		At:   at, Arrived: at,
		Earlier: []learn.Earlier{{Mock: "mock-0", At: at.Add(-2 * time.Second), Own: true,
			Payload: learn.RawOf(sent, true).ServedAs(3),
			Seen:    []learn.Seen{{Side: learn.Request, Leaf: text("body.order", "o-1"), Live: "o-9"}}}},
		Test: "test-2", Mock: "mock-300",
		Caps: learn.Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true},
	})
	var d *learn.Decision
	for i := range ds {
		if ds[i].Refusal != nil {
			d = &ds[i]
		}
	}
	if d == nil || d.Outcome != learn.OutcomeRefused {
		t.Fatalf("FALSE GREEN? o-1 sent, round F moved it to o-9, the call asked for o-5: %+v; want refused", ds)
	}
	r, ok := learn.ParseRefusal(d.Line)
	if !ok {
		t.Fatalf("ParseRefusal(%q) did not read the line", d.Line)
	}
	var noise *learn.Edit
	for i, e := range r.Edits {
		if e.Kind == learn.EditNoiseAtField {
			noise = &r.Edits[i]
		}
	}
	if noise == nil || noise.Field != field {
		t.Fatalf("line %q offers %+v; want a noise edit at %s", d.Line, r.Edits, field)
	}
	// The line names the bucket, the key a person fills in, and both scopes.
	m := regexp.MustCompile(`test\.globalNoise\.global\.(\w+): \{"<regex>": \[\]\}, a regex of the part of ` +
		regexp.QuoteMeta(field) + ` that changes, or the same under test\.globalNoise\.test-sets\.<the test set>`).
		FindStringSubmatch(d.Line)
	if m == nil {
		t.Fatalf("line %q names no keploy.yml url entry for %s", d.Line, field)
	}
	bucket := m[1]

	h := &httpint.HTTP{Logger: zap.NewNop()}
	served := func(global config.GlobalNoise, sets config.TestsetNoise, set string) bool {
		cfg := replay.PrepareMockNoiseConfig(global, sets, set)
		noise := httpint.NewRequestNoise(zap.NewNop(), models.OutgoingOptions{NoiseConfig: cfg})
		return h.MatchURLPath(mockURL, reqPath, noise.URL, false)
	}
	entry := map[string][]string{changes: {}}
	for _, tc := range []struct {
		what   string
		global config.GlobalNoise
		sets   config.TestsetNoise
		set    string
		want   bool
	}{
		{"no entry: the change stays refused", nil, nil, "test-set-0", false},
		{"the entry under global", config.GlobalNoise{bucket: entry}, nil, "test-set-0", true},
		{"the entry under the test set", nil, config.TestsetNoise{"test-set-0": {bucket: entry}}, "test-set-0", true},
		{"the entry under another test set", nil, config.TestsetNoise{"test-set-1": {bucket: entry}}, "test-set-0", false},
	} {
		if got := served(tc.global, tc.sets, tc.set); got != tc.want {
			t.Errorf("%s (line %q): the HTTP matcher served %s for %s: %v; want %v", tc.what, d.Line, reqPath, mockURL, got, tc.want)
		}
	}
}
