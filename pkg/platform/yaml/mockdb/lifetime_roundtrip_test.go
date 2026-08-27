// Round-trip tests for mock LIFETIME classification through the real
// mockdb write and read paths.
//
// Why this file exists: TestModeInfo.Lifetime and
// TestModeInfo.LifetimeDerived are tagged `json:"-" bson:"-"` in
// pkg/models/mock.go, so they are NEVER persisted by the yaml/json
// formats — they are re-derived by (*Mock).DeriveLifetime() from the
// on-disk Spec.Metadata["type"] tag on every load. The disk loader is
// therefore the only path a real recording travels on its way into the
// matcher, yet every other Lifetime test in the tree ASSIGNS the constant
// as a fixture instead of deriving it from a persisted tag. These tests
// close that gap: construct mocks -> InsertMock (real write path) ->
// GetFilteredMocks/GetUnFilteredMocks (real read path) -> assert
// TestModeInfo.Lifetime. Nothing here hand-builds a reloaded mock and
// nothing calls DeriveLifetime directly; the loader must do the work.
//
// Readers chosen: GetFilteredMocks + GetUnFilteredMocks, invoked as a pair
// over the window (models.BaseTime, time.Now()). That is exactly what
// production replay does — see pkg/service/replay/replay.go:3127-3132,
// pkg/service/mock/replay.go:116-121 and
// pkg/service/runner/runner.go:433-437. Between them the pair owns all four
// mock.DeriveLifetime() call sites in db.go (a gob branch and a
// yaml/json branch inside each reader), and it partitions the corpus by
// Lifetime: GetFilteredMocks keeps only LifetimePerTest while
// GetUnFilteredMocks keeps only LifetimeSession/LifetimeConnection. So the
// union of the two pools is the whole recording, and pool membership is an
// independent cross-check on the Lifetime field itself.
package mockdb

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/postgres"
	"go.uber.org/zap"
)

// lifetimeRoundTripBaseTime is the recorded request timestamp given to
// every fixture. It sits comfortably inside the (models.BaseTime,
// time.Now()) window the readers are called with, so no mock can be
// dropped for timestamp reasons and the assertions can never pass
// vacuously on an empty slice.
var lifetimeRoundTripBaseTime = time.Unix(1_700_000_000, 0).UTC()

// strictMockWindowEnvEnabled mirrors the gate behind
// pkg/models.laxKindFallbackDisabled(). That gate reads
// KEPLOY_STRICT_MOCK_WINDOW into laxKindFallbackDisabledCache at pkg/models
// package init, so a test cannot flip it at runtime with t.Setenv. Rows
// whose expectation depends on the lax-mode promotion (rule 5 of
// DeriveLifetime) therefore have to skip when strict mode is in force.
func strictMockWindowEnvEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KEPLOY_STRICT_MOCK_WINDOW"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// lifetimeCase is one on-disk shape and the Lifetime the loader must
// derive for it after a real write/read round trip.
type lifetimeCase struct {
	name string
	kind models.Kind

	// metadata is written verbatim to Spec.Metadata. nil models a legacy
	// recording with no metadata block at all.
	metadata map[string]string

	// HTTP request shape. Only read when kind == models.HTTP.
	method  models.Method
	url     string
	headers map[string]string

	want models.Lifetime

	// needsLax marks rows that only resolve to `want` via DeriveLifetime's
	// rule-5 lax promotion, which KEPLOY_STRICT_MOCK_WINDOW disables.
	needsLax bool

	// skipYAML, when non-empty, is the reason this row cannot travel the
	// YAML write path (it is still exercised over gob).
	skipYAML string

	// preStamped reproduces what the LIVE HTTP recorders actually emit
	// rather than a pristine fixture: they stamp
	// TestModeInfo{Lifetime: ptLifetime, LifetimeDerived: true} at emit
	// time — see pkg/agent/proxy/integrations/http/http.go:331-334 and
	// pkg/agent/proxy/integrations/http/recordv2.go:576-579.
	//
	// This matters because gob IGNORES struct tags. The `json:"-"
	// bson:"-"` tags on TestModeInfo.Lifetime / .LifetimeDerived keep
	// them out of the yaml and json wire formats, but the gob encoder
	// serialises every exported field, so LifetimeDerived=true survives
	// to disk in mocks.gob. On reload DeriveLifetime short-circuits on
	// its very first statement (pkg/models/lifetime.go:131-133) and NONE
	// of the classification rules run — the stale recorder-stamped
	// classification is replayed verbatim. See the case comment on
	// http_recorder_prestamped_preflight_is_session below.
	preStamped bool
}

// lifetimeRoundTripCases is the on-disk-tag -> derived-Lifetime contract
// this branch depends on. Each row is materialised fresh for every format
// pass because TestModeInfo.LifetimeDerived short-circuits re-derivation,
// so a reused fixture would only ever be classified once.
var lifetimeRoundTripCases = []lifetimeCase{
	{
		// The branch's core fix. The HTTP recorder stamps every capture
		// type=HTTP_CLIENT; HTTP is excluded from
		// kindsWithLaxTaggedSessionPromotion, so a tagged HTTP capture must
		// stay per-test and be consumed on match. Untested anywhere before
		// this file.
		name:     "http_client_get_is_per_test",
		kind:     models.HTTP,
		metadata: map[string]string{"type": "HTTP_CLIENT", "operation": "GET"},
		method:   "GET",
		url:      "http://svc/users",
		headers:  map[string]string{"Accept": "application/json"},
		want:     models.LifetimePerTest,
	},
	{
		// Rule 1(b): a browser CORS preflight (OPTIONS + an
		// Access-Control-Request-Method header) is input-independent, so it
		// is promoted to session ahead of the tag switch even though the
		// recorder tagged it HTTP_CLIENT.
		name:     "http_client_options_preflight_is_session",
		kind:     models.HTTP,
		metadata: map[string]string{"type": "HTTP_CLIENT", "operation": "OPTIONS"},
		method:   "OPTIONS",
		url:      "http://svc/query",
		headers: map[string]string{
			"Access-Control-Request-Method": "POST",
			"Origin":                        "http://localhost:3000",
		},
		want: models.LifetimeSession,
	},
	{
		// The other side of the preflight rule: a bare OPTIONS with no
		// Access-Control-Request-Method is a genuine data endpoint and must
		// stay per-test, or its first recorded response replays forever.
		name:     "http_client_options_without_acrm_is_per_test",
		kind:     models.HTTP,
		metadata: map[string]string{"type": "HTTP_CLIENT", "operation": "OPTIONS"},
		method:   "OPTIONS",
		url:      "http://svc/data",
		headers:  map[string]string{"Accept": "*/*"},
		want:     models.LifetimePerTest,
	},
	{
		// Legacy pre-tag recording, no metadata block at all: rule 4's
		// kind fallback must keep it session-reusable so it still replays.
		name:     "http_no_metadata_is_session",
		kind:     models.HTTP,
		metadata: nil,
		method:   "GET",
		url:      "http://svc/users",
		headers:  map[string]string{"Accept": "application/json"},
		want:     models.LifetimeSession,
	},
	{
		// Same rule, the shape actually found on disk: a metadata block
		// that carries recorder bookkeeping but no "type" key.
		name:     "http_metadata_without_type_key_is_session",
		kind:     models.HTTP,
		metadata: map[string]string{"name": "Http", "operation": "GET"},
		method:   "GET",
		url:      "http://svc/users",
		headers:  map[string]string{"Accept": "application/json"},
		want:     models.LifetimeSession,
	},
	{
		// Canonical session tag.
		name:     "http_config_tag_is_session",
		kind:     models.HTTP,
		metadata: map[string]string{"type": "config", "operation": "GET"},
		method:   "GET",
		url:      "http://svc/users",
		headers:  map[string]string{"Accept": "application/json"},
		want:     models.LifetimeSession,
	},
	{
		// Rule 5: a non-HTTP kind in kindsWithLaxTaggedSessionPromotion with
		// an explicit non-canonical tag is promoted to session under lax
		// mode only. PostgresV2 rather than models.Postgres because
		// models.Postgres (v1) has no arm in this package's EncodeMock /
		// DecodeMocks kind switch (util.go), so it cannot travel the YAML
		// write path at all; both kinds sit in the same
		// kindsWithLaxTaggedSessionPromotion list and hit the identical
		// rule-5 branch.
		name:     "postgres_v2_mocks_tag_is_session_under_lax",
		kind:     models.PostgresV2,
		metadata: map[string]string{"type": "mocks"},
		want:     models.LifetimeSession,
		needsLax: true,
	},
	{
		// The v1 Postgres kind from the original contract table. gob-only:
		// see the skipYAML reason and the note on the row above.
		name:     "postgres_v1_mocks_tag_is_session_under_lax",
		kind:     models.Postgres,
		metadata: map[string]string{"type": "mocks"},
		want:     models.LifetimeSession,
		needsLax: true,
		skipYAML: "Kind models.Postgres (v1) has no arm in mockdb EncodeMock/DecodeMocks, so it cannot round-trip through mocks.yaml",
	},
	{
		// KNOWN-DEFECT REPRODUCER — expected to PASS on yaml and FAIL on gob.
		// Deliberately NOT skipped: the failure is the finding.
		//
		// Identical on-disk shape to http_client_options_preflight_is_session
		// (a CORS preflight tagged HTTP_CLIENT, which rule 1(b) must classify
		// LifetimeSession), with ONE difference: TestModeInfo is pre-stamped
		// {Lifetime: LifetimePerTest, LifetimeDerived: true} before the write,
		// which is what the live recorders genuinely emit
		// (integrations/http/http.go:331-334, recordv2.go:576-579).
		//
		// yaml/json drop TestModeInfo entirely (NetworkTrafficDoc does not
		// embed it), so the reloaded mock has LifetimeDerived=false,
		// DeriveLifetime runs, rule 1(b) fires, and the row passes.
		//
		// gob ignores struct tags and encodes every exported field, so
		// LifetimeDerived=true reaches mocks.gob. readGobMocks
		// (db.go:1328-1360) decodes it verbatim and DeriveLifetime returns at
		// its first statement (pkg/models/lifetime.go:131-133) without running
		// any rule. The stale per-test classification survives, the preflight
		// lands in the per-test pool, is consumed on first match, and is
		// invisible to every other test — i.e. regression B28 is NOT actually
		// fixed for gob-format recordings.
		name:     "http_recorder_prestamped_preflight_is_session",
		kind:     models.HTTP,
		metadata: map[string]string{"type": "HTTP_CLIENT", "operation": "OPTIONS"},
		method:   "OPTIONS",
		url:      "http://svc/query",
		headers: map[string]string{
			"Access-Control-Request-Method": "POST",
			"Origin":                        "http://localhost:3000",
		},
		want:       models.LifetimeSession,
		preStamped: true,
	},
}

// mock materialises the case as a fresh *models.Mock. TestModeInfo is left
// zero unless the row opts into preStamped, so for every ordinary row the
// Lifetime observed after reload can only have come from the loader.
func (c lifetimeCase) mock() *models.Mock {
	var md map[string]string
	if c.metadata != nil {
		md = make(map[string]string, len(c.metadata))
		for k, v := range c.metadata {
			md[k] = v
		}
	}

	m := &models.Mock{
		Version: models.GetVersion(),
		Kind:    c.kind,
		Spec: models.MockSpec{
			Metadata:         md,
			ReqTimestampMock: lifetimeRoundTripBaseTime,
			ResTimestampMock: lifetimeRoundTripBaseTime.Add(time.Second),
		},
	}

	switch c.kind {
	case models.HTTP:
		// EncodeMock dereferences both HTTPReq and HTTPResp for kind HTTP.
		m.Spec.HTTPReq = &models.HTTPReq{
			Method:     c.method,
			ProtoMajor: 1,
			ProtoMinor: 1,
			URL:        c.url,
			Header:     c.headers,
			Timestamp:  lifetimeRoundTripBaseTime,
		}
		m.Spec.HTTPResp = &models.HTTPResp{
			StatusCode:    200,
			StatusMessage: "OK",
			Header:        map[string]string{"Content-Type": "application/json"},
			Body:          `{"ok":true}`,
		}
	default:
		m.Spec.PostgresRequestsV2 = []postgres.Request{{
			PacketBundle: postgres.PacketBundle{
				Packets: []postgres.Packet{{
					Header:  &postgres.PacketInfo{Type: "Query", Header: &postgres.Header{PayloadLength: 9, PacketID: "Q"}},
					Message: map[string]interface{}{"query": "SELECT 1"},
				}},
			},
		}}
		m.Spec.PostgresResponsesV2 = []postgres.Response{{
			PacketBundle: postgres.PacketBundle{
				Packets: []postgres.Packet{{
					Header:  &postgres.PacketInfo{Type: "CommandComplete", Header: &postgres.Header{PayloadLength: 5, PacketID: "C"}},
					Message: map[string]interface{}{"tag": "SELECT 1"},
				}},
			},
		}}
	}

	if c.preStamped {
		// Exactly what integrations/http/http.go:331-334 emits.
		m.TestModeInfo = models.TestModeInfo{
			Lifetime:        models.LifetimePerTest,
			LifetimeDerived: true,
		}
	}
	return m
}

// TestLifetimeRoundTrip_YAML persists the corpus as mocks.yaml (the
// default record format) and asserts the derived Lifetime after reload.
func TestLifetimeRoundTrip_YAML(t *testing.T) {
	// Neutralise the env override so InsertMock takes the structured
	// (yaml) branch regardless of the ambient environment.
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	runLifetimeRoundTrip(t, false)
}

// TestLifetimeRoundTrip_Gob persists the corpus as mocks.gob. Both formats
// are live record paths and each has its own DeriveLifetime call site
// inside GetFilteredMocks / GetUnFilteredMocks, so both need covering.
//
// NOTE: the http_recorder_prestamped_preflight_is_session row is EXPECTED
// to fail here and to pass in the yaml test above. That asymmetry is a real
// defect in the gob read path, not a defect in the test — see the case
// comment on that row.
func TestLifetimeRoundTrip_Gob(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	runLifetimeRoundTrip(t, true)
}

func runLifetimeRoundTrip(t *testing.T, gobFormat bool) {
	t.Helper()

	strict := strictMockWindowEnvEnabled()

	// Partition the table before writing anything: only the included rows
	// are persisted, so the "expected number of mocks came back" check
	// below is exact.
	type skipped struct {
		c      lifetimeCase
		reason string
	}
	var included []lifetimeCase
	var excluded []skipped
	for _, c := range lifetimeRoundTripCases {
		switch {
		case !gobFormat && c.skipYAML != "":
			excluded = append(excluded, skipped{c, c.skipYAML})
		case c.needsLax && strict:
			excluded = append(excluded, skipped{c,
				"KEPLOY_STRICT_MOCK_WINDOW is set, which disables DeriveLifetime's lax rule-5 promotion; " +
					"pkg/models snapshots that env var at package init so the test cannot flip it at runtime"})
		default:
			included = append(included, c)
		}
	}

	const testSetID = "test-set-0"
	ctx := context.Background()
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")

	// Real write path. InsertMock assigns the on-disk name itself
	// (mock-<n>), so capture it to correlate the reloaded mock back to its
	// row.
	nameByCase := make(map[string]string, len(included))
	for _, c := range included {
		m := c.mock()
		if err := ys.InsertMock(ctx, m, testSetID); err != nil {
			t.Fatalf("%s: InsertMock: %v", c.name, err)
		}
		nameByCase[c.name] = m.Name
	}
	// Drains and closes the async gob writer; a no-op for the yaml path.
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Real read path, with the same window arguments production replay
	// uses. BaseTime is 2000-01-01 and every fixture is stamped 2023, so
	// nothing is window-filtered.
	afterTime, beforeTime := models.BaseTime, time.Now()

	perTestPool, err := ys.GetFilteredMocks(ctx, testSetID, afterTime, beforeTime, nil, nil)
	if err != nil {
		t.Fatalf("GetFilteredMocks: %v", err)
	}
	sessionPool, err := ys.GetUnFilteredMocks(ctx, testSetID, afterTime, beforeTime, nil, nil)
	if err != nil {
		t.Fatalf("GetUnFilteredMocks: %v", err)
	}

	// Pool-routing invariant, independent of the per-row expectations
	// below: GetFilteredMocks must yield only per-test mocks and
	// GetUnFilteredMocks only reusable ones.
	for _, m := range perTestPool {
		if m.TestModeInfo.Lifetime != models.LifetimePerTest {
			t.Errorf("GetFilteredMocks returned %s with lifetime %s; the per-test pool must contain only per-test mocks",
				m.Name, m.TestModeInfo.Lifetime)
		}
	}
	for _, m := range sessionPool {
		if m.TestModeInfo.Lifetime == models.LifetimePerTest {
			t.Errorf("GetUnFilteredMocks returned %s with lifetime per-test; the session pool must contain only session/connection mocks",
				m.Name)
		}
	}

	// Union the two pools by name. A mock may legitimately appear in both
	// (PostgresV2 keeps a dual-pool quirk in the gob reader) but the
	// derived lifetime must agree.
	got := make(map[string]models.Lifetime)
	for _, m := range append(append([]*models.Mock{}, perTestPool...), sessionPool...) {
		if prev, seen := got[m.Name]; seen && prev != m.TestModeInfo.Lifetime {
			t.Fatalf("%s came back from the two readers with disagreeing lifetimes: %s vs %s",
				m.Name, prev, m.TestModeInfo.Lifetime)
		}
		got[m.Name] = m.TestModeInfo.Lifetime
	}

	// Guard against a vacuous pass: assert the corpus survived the round
	// trip before asserting anything about lifetimes.
	if len(got) != len(included) {
		t.Fatalf("reload returned %d distinct mocks, want %d (per-test pool %d, session pool %d) — assertions below would be vacuous",
			len(got), len(included), len(perTestPool), len(sessionPool))
	}

	for _, c := range included {
		c := c
		t.Run(c.name, func(t *testing.T) {
			diskName := nameByCase[c.name]
			lifetime, ok := got[diskName]
			if !ok {
				t.Fatalf("mock %q (%s) did not come back from either reader", diskName, c.name)
			}
			if lifetime == c.want {
				return
			}
			msg := fmt.Sprintf("metadata=%v kind=%s method=%s: reloaded lifetime = %s, want %s",
				c.metadata, c.kind, c.method, lifetime, c.want)
			if c.preStamped {
				msg += "\n\n" + strings.Join([]string{
					"KNOWN DEFECT (gob read path) — this failure is the finding, not a broken test.",
					"The recorders stamp TestModeInfo{Lifetime, LifetimeDerived: true} at emit time",
					"(pkg/agent/proxy/integrations/http/http.go:331-334, recordv2.go:576-579).",
					"gob ignores the `json:\"-\" bson:\"-\"` tags and encodes every exported field, so",
					"LifetimeDerived=true is persisted into mocks.gob. readGobMocks (db.go:1328-1360)",
					"decodes it verbatim, DeriveLifetime returns at pkg/models/lifetime.go:131-133,",
					"and NO classification rule runs — the CORS-preflight promotion (rule 1(b)) never",
					"fires, so regression B28 is unfixed for gob-format recordings. yaml/json are",
					"unaffected because NetworkTrafficDoc does not carry TestModeInfo at all.",
				}, "\n")
			}
			t.Fatalf("%s", msg)
		})
	}
	for _, s := range excluded {
		s := s
		t.Run(s.c.name, func(t *testing.T) { t.Skip(s.reason) })
	}
}
