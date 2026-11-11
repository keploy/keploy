package http

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// sourcedDb is a store whose scope table declares sources for recordings
// by name (integrations.DeclaredSourcesOf), as keploy mock replay hands the
// agent keploy.yml's rules.
type sourcedDb struct {
	*mockMemDb
	sources map[string][]models.DeclaredSource
}

func (d sourcedDb) SourcesOf(mock string) []models.DeclaredSource { return d.sources[mock] }

var _ integrations.DeclaredSourcesOf = sourcedDb{}

// patTouch is the PAT store's last-use touch as a record kept it (the PUT
// a once series serves in worker:): its body stamps lastUsedAt, an epoch
// second, and updatedAt, RFC 3339, from the clock at the call.
func patTouch(at time.Time) (*models.Mock, func(lastUsed, updated time.Time) *req) {
	const path = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/e2e-go-key"
	body := func(lastUsed, updated time.Time) string {
		return `{"data":{"_id":"e2e-go-key","cid":"keploy.io","createdAt":1767225600,"keyType":"pat","lastUsedAt":` +
			strconv.FormatInt(lastUsed.Unix(), 10) + `,"name":"e2e-go","status":"active","updatedAt":"` + updated.UTC().Format(time.RFC3339) + `"},"options":{"cas":1}}`
	}
	m := httpMock("mock-220", "PUT", path)
	m.Spec.HTTPReq.Header = map[string]string{"Host": "localhost:8200", "X-Vault-Request": "true"}
	m.Spec.HTTPReq.Body = body(at, at)
	m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: `{"data":{"version":2}}`}
	call := func(lastUsed, updated time.Time) *req {
		r := sent(path, map[string]string{"X-Vault-Request": "true"}, body(lastUsed, updated))
		r.method = "PUT"
		r.header.Set("Host", "localhost:8200")
		return r
	}
	return m, call
}

// The PAT touch's two clock rules make a replay check lastUsedAt and
// updatedAt as reads of the clock in a recording no learning read: the
// once series a judge placed in worker:, whose call the app makes only
// inside a test, where the background answers a call exactly. A call that
// stamps both from its own window is answered. Without the rules, the
// same call misses on both fields, as the replay refused it ("body.data.
// lastUsedAt and body.data.updatedAt differ"); a value the call did not
// read in its window (the recorded one), a field no rule names, and a
// place where the learn step learned another offset are refused.
func TestADeclaredClockIsCheckedInARecordingNoLearningRead(t *testing.T) {
	base := time.Date(2026, 11, 11, 14, 16, 33, 0, time.UTC)
	recAt := time.Date(2026, 11, 11, 14, 6, 39, 0, time.UTC)
	now := base.Add(time.Second)
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	lastUsed := models.DeclaredSource{Rule: "pat-last-used-at", Kind: models.DeclaredSourceClock, Field: "body.data.lastUsedAt", Clock: plain}
	updated := models.DeclaredSource{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: plain}
	both := []models.DeclaredSource{lastUsed, updated}
	for _, c := range []struct {
		name     string
		sources  []models.DeclaredSource
		learned  map[string]models.ClockOffset
		lastUsed time.Time
		differs  []string
	}{
		{"both declared, stamped in the call's window", both, nil, now, nil},
		{"no rules", nil, nil, now, []string{"body.data.lastUsedAt", "body.data.updatedAt"}},
		{"the recorded lastUsedAt", both, nil, recAt, []string{"body.data.lastUsedAt"}},
		{"only updatedAt declared", []models.DeclaredSource{updated}, nil, now, []string{"body.data.lastUsedAt"}},
		{"learned an hour ahead at lastUsedAt", both, map[string]models.ClockOffset{"body.data.lastUsedAt": {Min: time.Hour, Max: time.Hour + time.Second, Res: time.Second}}, now, []string{"body.data.lastUsedAt"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", "1")
			m, call := patTouch(recAt)
			m.Spec.Clocks = c.learned
			in := call(c.lastUsed, now)
			in.arrived = base.Add(2 * time.Second)
			db := sourcedDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, sources: map[string][]models.DeclaredSource{"mock-220": c.sources}}
			ok, stub, diag, err := newHTTP().match(clockConn(true, base), in, db, models.ProvenFlakyHeaderNoise(), nil, nil, true, false, false, true, true)
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			if len(c.differs) == 0 {
				if !ok || stub == nil {
					t.Fatalf("refused (%v); want it answered", differs(diag))
				}
				return
			}
			if ok {
				t.Fatalf("answered; want refused naming %v", c.differs)
			}
			got := differs(diag)
			for _, f := range c.differs {
				if !hasField(got, f) {
					t.Fatalf("differs %v; want it to name %s", got, f)
				}
			}
			if len(got) != len(c.differs) {
				t.Fatalf("differs %v; want only %v", got, strings.Join(c.differs, ", "))
			}
		})
	}
}

// clocksWith adds a declared clock under the proven switch only: with it
// off every check runs as before, and a noise built with no store reads
// no sources.
func TestADeclaredClockIsReadUnderTheProvenSwitchOnly(t *testing.T) {
	m, _ := patTouch(time.Date(2026, 11, 11, 14, 6, 39, 0, time.UTC))
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	sources := sourcesIn(sourcedDb{mockMemDb: &mockMemDb{}, sources: map[string][]models.DeclaredSource{
		"mock-220": {{Rule: "pat-last-used-at", Kind: models.DeclaredSourceClock, Field: "body.data.lastUsedAt", Clock: plain}},
	}})
	t.Setenv("KEPLOY_PROVEN", "")
	if got := clocksWith(m, sources); len(got) != 0 {
		t.Fatalf("switch off: %v; want none", got)
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if got := clocksWith(m, sources); len(got) != 1 || got["body.data.lastUsedAt"] != (models.ClockOffset{Res: time.Second}) {
		t.Fatalf("switch on: %v; want body.data.lastUsedAt at a cut of 1s", got)
	}
	if got := clocksWith(m, nil); len(got) != 0 {
		t.Fatalf("no sources: %v; want none", got)
	}
	if got := sourcesIn(nil)("mock-220"); got != nil {
		t.Fatalf("a nil store: %v; want none", got)
	}
}
