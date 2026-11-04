package mismatch

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

func TestBuilder_RendersFieldDiffsAndDefaults(t *testing.T) {
	report := NewReport(ProtocolHTTP, "POST /orders").
		WithPhase(models.MatchPhaseExhausted, 3).
		WithClosest("mock-7", []models.MockFieldDiff{
			{Path: "body.created_at", Kind: models.DiffKindValueChanged, Expected: "2026-01-01", Actual: "2026-06-12"},
		}).Build()

	if report.Protocol != ProtocolHTTP || report.ActualSummary != "POST /orders" {
		t.Fatalf("identity fields wrong: %+v", report)
	}
	if report.MatchPhase != models.MatchPhaseExhausted || report.CandidateCount != 3 {
		t.Errorf("phase fields wrong: %+v", report)
	}
	if report.ClosestMock != "mock-7" {
		t.Errorf("closest mock wrong: %q", report.ClosestMock)
	}
	if !strings.Contains(report.Diff, "body.created_at") {
		t.Errorf("Diff should render field paths, got %q", report.Diff)
	}
	// Pure value drift → next steps must suggest noise, never a nonexistent command.
	// Pure request-body value drift must point users at the request-body noise
	// bucket (test.globalNoise.global.requestbody, root-relative keys) — not the
	// response "body" bucket the request matcher never consults.
	if !strings.Contains(report.NextSteps, "requestbody") {
		t.Errorf("value-only request-body drift should point at the requestbody noise bucket, got %q", report.NextSteps)
	}
	if strings.Contains(report.NextSteps, "keploy rerecord") {
		t.Errorf("next steps must not reference the nonexistent 'keploy rerecord' command: %q", report.NextSteps)
	}
}

func TestBuilder_NoMocksPhase(t *testing.T) {
	report := NewReport(ProtocolGeneric, "opaque exchange").
		WithPhase(models.MatchPhaseNoMocks, 0).Build()
	if !strings.Contains(report.NextSteps, "keploy record") {
		t.Errorf("no-mocks phase should point at 'keploy record', got %q", report.NextSteps)
	}
}

func TestBuilder_StructuralChangeNextSteps(t *testing.T) {
	report := NewReport(ProtocolHTTP, "GET /a").
		WithPhase(models.MatchPhaseSchema, 5).
		WithClosest("mock-1", []models.MockFieldDiff{
			{Path: "body.new_field", Kind: models.DiffKindMissingInMock, Actual: "1"},
		}).Build()
	if !strings.Contains(report.NextSteps, "keploy record") {
		t.Errorf("structural change should suggest re-record, got %q", report.NextSteps)
	}
}

func TestBuilder_FieldDiffCap(t *testing.T) {
	diffs := make([]models.MockFieldDiff, maxFieldDiffs+10)
	for i := range diffs {
		diffs[i] = models.MockFieldDiff{Path: "body.x", Kind: models.DiffKindValueChanged}
	}
	report := NewReport(ProtocolHTTP, "GET /a").WithClosest("m", diffs).Build()
	if len(report.FieldDiffs) != maxFieldDiffs {
		t.Errorf("expected diff cap %d, got %d", maxFieldDiffs, len(report.FieldDiffs))
	}
}

// A report cut to maxFieldDiffs carries the diffs that can be why its call
// missed first, each in its order. Without the proven switch an HTTP header
// only the call carries is no cause (the matcher lets it pass), so it goes
// after the rest; a body field only the call carries stays a cause. Under
// the switch, and outside HTTP, every diff is a cause and the order is kept.
func TestBuilder_FieldDiffCapKeepsTheCausesFirst(t *testing.T) {
	diffs := []models.MockFieldDiff{{Path: "header.Referer", Kind: models.DiffKindMissingInLive}}
	var carried []string
	for i := 0; i < maxFieldDiffs; i++ {
		path := fmt.Sprintf("header.X-H%02d", i)
		carried = append(carried, path)
		diffs = append(diffs, models.MockFieldDiff{Path: path, Kind: models.DiffKindMissingInMock})
	}
	diffs = append(diffs,
		models.MockFieldDiff{Path: "header.Content-Type", Kind: models.DiffKindValueChanged},
		models.MockFieldDiff{Path: "body.new", Kind: models.DiffKindMissingInMock})
	paths := func(ds []models.MockFieldDiff) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Path)
		}
		return out
	}
	inOrder := paths(diffs[:maxFieldDiffs])
	for _, c := range []struct {
		name, protocol, proven string
		want                   []string
	}{
		{"HTTP without the switch", ProtocolHTTP, "", slices.Concat([]string{"header.Referer", "header.Content-Type", "body.new"}, carried[:maxFieldDiffs-3])},
		{"HTTP under the switch", ProtocolHTTP, "1", inOrder},
		{"outside HTTP", ProtocolGeneric, "", inOrder},
	} {
		t.Setenv(provenswitch.Env, c.proven)
		r := NewReport(c.protocol, "POST /x").WithClosest("m", diffs).Build()
		if got := paths(r.FieldDiffs); !slices.Equal(got, c.want) {
			t.Errorf("%s: the report carries %v; want %v", c.name, got, c.want)
		}
	}
}

// The hint names only fields the report carries, so the miss's sanitizer,
// which shapes the paths of the report's diffs, shapes every name in it: a
// body key that may be a value the app sent (an ID) never reaches the
// sanitized next step, with the switch on or off. Past the diffs the report
// carries, the hint counts the rest and offers no noise.
func TestBuilder_HintNamesOnlyFieldsTheReportCarries(t *testing.T) {
	id := func(i int) string { return fmt.Sprintf("48151623%02d", i) }
	idDrifts := func(n int) []models.MockFieldDiff {
		var out []models.MockFieldDiff
		for i := 0; i < n; i++ {
			if p := "body.ids." + id(i); models.FieldShape(p) == p {
				t.Fatalf("%s keeps its shape; the row needs a key that may be a value", p)
			}
			out = append(out, models.MockFieldDiff{Path: "body.ids." + id(i), Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"})
		}
		return out
	}
	var missing []models.MockFieldDiff
	for i := 0; i < 20; i++ {
		missing = append(missing, models.MockFieldDiff{Path: fmt.Sprintf("header.X-H%02d", i), Kind: models.DiffKindMissingInLive})
	}
	for _, c := range []struct {
		name      string
		diffs     []models.MockFieldDiff
		noise     bool // the hint offers keploy.yml noise
		structure bool // the hint is the structure-change one
	}{
		{name: "20 headers missing and 6 body fields drifted", diffs: slices.Concat(missing, idDrifts(6)), structure: true},
		{name: "30 body fields drifted", diffs: idDrifts(30)},
		{name: "25 body fields drifted", diffs: idDrifts(25), noise: true},
		{name: "a dotted header name with an ID drifted", diffs: []models.MockFieldDiff{{Path: "header.X." + id(0), Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"}}},
	} {
		for _, proven := range []string{"", "1"} {
			t.Setenv(provenswitch.Env, proven)
			r := NewReport(ProtocolHTTP, "POST /x").WithClosest("m", c.diffs).Build()
			if got := strings.Contains(r.NextSteps, "test.globalNoise"); got != c.noise {
				t.Errorf("%s (switch %q): hint %q offers noise: %v; want %v", c.name, proven, r.NextSteps, got, c.noise)
			}
			if got := strings.HasPrefix(r.NextSteps, "Request structure changed"); got != c.structure {
				t.Errorf("%s (switch %q): hint %q is the structure-change one: %v; want %v", c.name, proven, r.NextSteps, got, c.structure)
			}
			next := models.UnmatchedCall{FieldDiffs: r.FieldDiffs, NextSteps: r.NextSteps}.SanitizedNextStep()
			for i := range c.diffs {
				if strings.Contains(next, id(i)) {
					t.Errorf("%s (switch %q): the sanitized next step %q names %s", c.name, proven, next, id(i))
				}
			}
		}
	}
}

func TestJSONBodyDiffs_RespectsIgnoreAndReportsValues(t *testing.T) {
	recorded := `{"id":"abc","ts":"111","nested":{"v":1}}`
	live := `{"id":"abc","ts":"222","nested":{"v":2}}`

	diffs := JSONBodyDiffs(recorded, live, map[string][]string{"ts": {}})
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff (ts ignored), got %d: %+v", len(diffs), diffs)
	}
	d := diffs[0]
	if d.Path != "body.nested.v" || d.Kind != models.DiffKindValueChanged || d.Expected != "1" || d.Actual != "2" {
		t.Errorf("unexpected diff: %+v", d)
	}
}

func TestHeaderKeyDiffs_SkipsNoiseAndKeployHeaders(t *testing.T) {
	recorded := map[string]string{
		"Authorization":  "sig",      // in noise → skipped
		"X-Custom":       "v",        // missing in live → reported
		"Keploy-Test-Id": "test-1",   // keploy header → skipped
		"Content-Type":   "app/json", // present both → not reported
	}
	live := http.Header{
		"Content-Type": {"app/json"},
		"X-New":        {"n"}, // missing in recording → reported
	}
	noise := map[string][]string{"authorization": {}}

	diffs := HeaderKeyDiffs(recorded, live, noise)
	got := map[string]models.MockFieldDiffKind{}
	for _, d := range diffs {
		got[d.Path] = d.Kind
	}
	if got["header.X-Custom"] != models.DiffKindMissingInLive {
		t.Errorf("expected header.X-Custom missing_in_live, got %+v", diffs)
	}
	if got["header.X-New"] != models.DiffKindMissingInMock {
		t.Errorf("expected header.X-New missing_in_mock, got %+v", diffs)
	}
	if _, ok := got["header.Authorization"]; ok {
		t.Errorf("noised header must not be reported: %+v", diffs)
	}
	if _, ok := got["header.Keploy-Test-Id"]; ok {
		t.Errorf("keploy header must not be reported: %+v", diffs)
	}
}

func TestQueryParamDiffs(t *testing.T) {
	recorded := map[string][]string{"page": {"1"}, "old": {"x"}}
	live := map[string][]string{"page": {"2"}, "new": {"y"}}

	diffs := QueryParamDiffs(recorded, live)
	got := map[string]models.MockFieldDiff{}
	for _, d := range diffs {
		got[d.Path] = d
	}
	if d := got["query.page"]; d.Kind != models.DiffKindValueChanged || d.Expected != "1" || d.Actual != "2" {
		t.Errorf("query.page diff wrong: %+v", d)
	}
	if d := got["query.old"]; d.Kind != models.DiffKindMissingInLive {
		t.Errorf("query.old diff wrong: %+v", d)
	}
	if d := got["query.new"]; d.Kind != models.DiffKindMissingInMock {
		t.Errorf("query.new diff wrong: %+v", d)
	}
}

func TestRenderFieldDiffs_AllKinds(t *testing.T) {
	out := RenderFieldDiffs([]models.MockFieldDiff{
		{Path: "body.a", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "2"},
		{Path: "header.B", Kind: models.DiffKindMissingInLive, Expected: "x"},
		{Path: "query.c", Kind: models.DiffKindMissingInMock, Actual: "y"},
		{Path: "body.d", Kind: models.DiffKindTypeChanged, Expected: "string: s", Actual: "number: 1"},
	})
	for _, want := range []string{"body.a", "header.B", "query.c", "body.d", "absent in live", "absent in recording", "type changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered diff missing %q: %s", want, out)
		}
	}
}

// Every canonical protocol names the kinds its calls are recorded as
// (models.KindsOfProtocol), which a miss of a test recorded beside others
// reads to say which of their recordings its own may be.
func TestEveryCanonicalProtocolNamesItsKinds(t *testing.T) {
	// Each canonical protocol, and exactly the kinds its calls are recorded
	// as. A miss of one names, as the recordings it may be, only recordings
	// of these kinds: a wrong entry would have a MongoDB miss name an HTTP
	// recording.
	want := map[string][]models.Kind{
		ProtocolHTTP:     {models.HTTP},
		ProtocolHTTP2:    {models.HTTP2},
		ProtocolGRPC:     {models.GRPC_EXPORT},
		ProtocolMySQL:    {models.MySQL},
		ProtocolPostgres: {models.Postgres, models.PostgresV2, models.PostgresV3},
		ProtocolMongo:    {models.Mongo},
		ProtocolRedis:    {models.REDIS},
		ProtocolGeneric:  {models.GENERIC},
		ProtocolDNS:      {models.DNS},
	}
	of := map[models.Kind]string{}
	for p, kinds := range want {
		if got := models.KindsOfProtocol(p); !slices.Equal(got, kinds) {
			t.Errorf("%s is recorded as %v; want %v", p, got, kinds)
		}
		// No kind is another protocol's too.
		for _, k := range models.KindsOfProtocol(p) {
			if other, dup := of[k]; dup {
				t.Errorf("kind %s is recorded under both %s and %s", k, other, p)
			}
			of[k] = p
		}
	}
	if len(models.KindsOfProtocol("unknown")) != 0 {
		t.Error("a protocol keploy does not name names a kind")
	}
}
