package http

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// sourcedDb is a store whose scope table declares sources for recordings
// by name (integrations.DeclaredSourcesOf), as keploy mock replay hands the
// agent keploy.yml's rules, and whose background serves the recordings
// background names (integrations.BackgroundMember), with the time default
// on where timeForm is set (integrations.TimeFormReader).
type sourcedDb struct {
	*mockMemDb
	sources    map[string][]models.DeclaredSource
	background map[string]bool
	timeForm   bool
}

func (d sourcedDb) SourcesOf(mock string) []models.DeclaredSource { return d.sources[mock] }

func (d sourcedDb) InBackground(name string) bool { return d.background[name] }

func (d sourcedDb) TimeForm() bool { return d.timeForm }

var (
	_ integrations.DeclaredSourcesOf = sourcedDb{}
	_ integrations.BackgroundMember  = sourcedDb{}
	_ integrations.TimeFormReader    = sourcedDb{}
)

// patTouch is the PAT store's last-use touch as a record kept it (the PUT
// a once series serves in worker:): its body stamps lastUsedAt, an epoch
// second, and updatedAt, RFC 3339, from the clock at the call.
func patTouch(at time.Time) (*models.Mock, func(lastUsed, updated time.Time) *req) {
	body := func(lastUsed, updated time.Time) string {
		return `{"data":{"_id":"e2e-go-key","cid":"keploy.io","createdAt":1767225600,"keyType":"pat","lastUsedAt":` +
			strconv.FormatInt(lastUsed.Unix(), 10) + `,"name":"e2e-go","status":"active","updatedAt":"` + updated.UTC().Format(time.RFC3339) + `"},"options":{"cas":1}}`
	}
	m := httpMock("mock-220", "PUT", patTouchPath)
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(20*time.Millisecond)
	m.Spec.HTTPReq.Header = map[string]string{"Host": "localhost:8200", "X-Vault-Request": "true"}
	m.Spec.HTTPReq.Body = body(at, at)
	m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: `{"data":{"version":2}}`}
	call := func(lastUsed, updated time.Time) *req {
		return patTouchCall(body(lastUsed, updated))
	}
	return m, call
}

// patTouchPath is the path of the PAT touch's PUT.
const patTouchPath = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/e2e-go-key"

// patTouchCall is the PAT touch's PUT carrying body.
func patTouchCall(body string) *req {
	r := sent(patTouchPath, map[string]string{"X-Vault-Request": "true"}, body)
	r.method = "PUT"
	r.header.Set("Host", "localhost:8200")
	return r
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
//
// A rule's range is read on the background's recordings only: a test's own
// recording keeps what its learning decided, so its recorded values (a
// copy of a stored time, a zero lastUsedAt) are answered as recorded and a
// changed value is a change the learn step decides. Under the time default
// (timeForm) the background's declared field is checked by its form alone:
// a lastUsedAt of any moment in epoch seconds passes, one in milliseconds
// or sent as a text does not. A recorded updatedAt keploy reads as no
// time compares as recorded, under the time default or not: the rule does
// not describe that recording.
func TestADeclaredClockIsCheckedInARecordingNoLearningRead(t *testing.T) {
	base := time.Date(2026, 11, 11, 14, 16, 33, 0, time.UTC)
	recAt := time.Date(2026, 11, 11, 14, 6, 39, 0, time.UTC)
	now := base.Add(time.Second)
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	lastUsed := models.DeclaredSource{Rule: "pat-last-used-at", Kind: models.DeclaredSourceClock, Field: "body.data.lastUsedAt", Clock: plain}
	updated := models.DeclaredSource{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: plain}
	both := []models.DeclaredSource{lastUsed, updated}
	// withLastUsed is the touch's body stamped from now with lastUsedAt
	// written as v.
	withLastUsed := func(v string) string {
		return `{"data":{"_id":"e2e-go-key","cid":"keploy.io","createdAt":1767225600,"keyType":"pat","lastUsedAt":` + v +
			`,"name":"e2e-go","status":"active","updatedAt":"` + now.Format(time.RFC3339) + `"},"options":{"cas":1}}`
	}
	// withUpdated is the touch's body with lastUsedAt stamped from now and
	// updatedAt written as v.
	withUpdated := func(v string) string {
		return `{"data":{"_id":"e2e-go-key","cid":"keploy.io","createdAt":1767225600,"keyType":"pat","lastUsedAt":` + strconv.FormatInt(now.Unix(), 10) +
			`,"name":"e2e-go","status":"active","updatedAt":"` + v + `"},"options":{"cas":1}}`
	}
	// stale is the recording holding a lastUsedAt two years before its
	// call: a stored moment, no read of the clock near it.
	stale := func(m *models.Mock) {
		m.Spec.HTTPReq.Body = strings.Replace(m.Spec.HTTPReq.Body, strconv.FormatInt(recAt.Unix(), 10), strconv.FormatInt(recAt.AddDate(-2, 0, 0).Unix(), 10), 1)
	}
	never := func(m *models.Mock) {
		m.Spec.HTTPReq.Body = strings.Replace(m.Spec.HTTPReq.Body, recAt.Format(time.RFC3339), "never", 1)
	}
	// formOnly and rangedForm are what the learn step learns at lastUsedAt,
	// an epoch second, under the time default: its form alone, where it
	// learned no range, and its range with its form. The time default passes
	// any value of the form; strict sets the form aside, so the range alone
	// decides and a form with no range is refused.
	epochSecond := learn.TimeForm(learn.Leaf{Type: learn.Number, Value: strconv.FormatInt(recAt.Unix(), 10)})
	formOnly := map[string]models.ClockOffset{"body.data.lastUsedAt": {Form: epochSecond}}
	rangedForm := map[string]models.ClockOffset{"body.data.lastUsedAt": {Res: time.Second, Form: epochSecond}}
	for _, c := range []struct {
		name      string
		sources   []models.DeclaredSource
		learned   map[string]models.ClockOffset
		own       bool // a test's own recording, not the background's
		timeForm  bool
		lastUsed  time.Time
		updated   time.Time // zero: now
		body      string    // the call's body, in place of lastUsed and updated
		recording func(m *models.Mock)
		noise     string // a field learned as noise on the recording
		differs   []string
	}{
		{name: "both declared, stamped in the call's window", sources: both, lastUsed: now},
		{name: "no rules", lastUsed: now, differs: []string{"body.data.lastUsedAt", "body.data.updatedAt"}},
		{name: "the recorded lastUsedAt", sources: both, lastUsed: recAt, differs: []string{"body.data.lastUsedAt"}},
		{name: "only updatedAt declared", sources: []models.DeclaredSource{updated}, lastUsed: now, differs: []string{"body.data.lastUsedAt"}},
		{name: "learned an hour ahead at lastUsedAt", sources: both, learned: map[string]models.ClockOffset{"body.data.lastUsedAt": {Min: time.Hour, Max: time.Hour + time.Second, Res: time.Second}}, lastUsed: now, differs: []string{"body.data.lastUsedAt"}},
		{name: "a test's own recording, its recorded values", sources: both, own: true, lastUsed: recAt, updated: recAt},
		{name: "a test's own recording, stamped in the call's window", sources: both, own: true, lastUsed: now, differs: []string{"body.data.lastUsedAt", "body.data.updatedAt"}},
		{name: "a test's own recording, learned at lastUsedAt", sources: both, own: true, learned: map[string]models.ClockOffset{"body.data.lastUsedAt": {Res: time.Second}}, lastUsed: now, updated: recAt},
		{name: "the time default: the recorded lastUsedAt passes by its form", sources: both, timeForm: true, lastUsed: recAt},
		{name: "the time default: any moment in epoch seconds", sources: both, timeForm: true, lastUsed: now.Add(-400 * 24 * time.Hour), updated: recAt.Add(time.Hour)},
		{name: "a test's own recording, noise stored at lastUsedAt", sources: both, own: true, lastUsed: now, updated: recAt, noise: "body.data.lastUsedAt", differs: []string{"body.data.lastUsedAt"}},
		{name: "a test's own recording, noise stored on body.data", sources: both, own: true, lastUsed: now, updated: recAt, noise: "body.data", differs: []string{"body.data.lastUsedAt"}},
		{name: "a test's own recording, noise stored at lastUsedAt, no rule", own: true, lastUsed: now, updated: recAt, noise: "body.data.lastUsedAt"},
		{name: "the background's, noise stored at lastUsedAt, a ms stamp", sources: both, timeForm: true, body: withLastUsed(strconv.FormatInt(now.UnixMilli(), 10)), noise: "body.data.lastUsedAt", differs: []string{"body.data.lastUsedAt"}},
		{name: "the time default: lastUsedAt in milliseconds", sources: both, timeForm: true, body: withLastUsed(strconv.FormatInt(now.UnixMilli(), 10)), differs: []string{"body.data.lastUsedAt"}},
		{name: "the time default: lastUsedAt sent as a text", sources: both, timeForm: true, body: withLastUsed(`"` + strconv.FormatInt(now.Unix(), 10) + `"`), differs: []string{"body.data.lastUsedAt"}},
		{name: "the time default: a field no rule names", sources: []models.DeclaredSource{updated}, timeForm: true, lastUsed: now, differs: []string{"body.data.lastUsedAt"}},
		{name: "the time default: a recorded no-time compares as recorded, a time in the window", sources: both, timeForm: true, lastUsed: now, recording: never, differs: []string{"body.data.updatedAt"}},
		{name: "the time default: a recorded no-time compares as recorded, the recorded value", sources: both, timeForm: true, body: withUpdated("never"), recording: never},
		{name: "a recorded no-time compares as recorded, the recorded value", sources: both, body: withUpdated("never"), recording: never},
		{name: "a recorded no-time compares as recorded, a time in the window", sources: both, lastUsed: now, recording: never, differs: []string{"body.data.updatedAt"}},
		{name: "the time default: a recorded epoch far from its call keeps the range", sources: both, timeForm: true, lastUsed: recAt, recording: stale, differs: []string{"body.data.lastUsedAt"}},
		{name: "the time default: a recorded epoch far from its call keeps the range, in the window", sources: both, timeForm: true, lastUsed: now, recording: stale},
		{name: "the time default: a test's own place learned by its form alone, any moment of it", sources: both, own: true, learned: formOnly, timeForm: true, lastUsed: now.Add(-400 * 24 * time.Hour), updated: recAt},
		{name: "strict: a test's own place learned by its form alone is refused", sources: both, own: true, learned: formOnly, lastUsed: now, updated: recAt, differs: []string{"body.data.lastUsedAt"}},
		{name: "the time default: a test's own place learned with its range and form, an hour out", sources: both, own: true, learned: rangedForm, timeForm: true, lastUsed: now.Add(time.Hour), updated: recAt},
		{name: "strict: a test's own place learned with its range and form keeps the range, an hour out", sources: both, own: true, learned: rangedForm, lastUsed: now.Add(time.Hour), updated: recAt, differs: []string{"body.data.lastUsedAt"}},
		{name: "strict: a test's own place learned with its range and form, in the window", sources: both, own: true, learned: rangedForm, lastUsed: now, updated: recAt},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", "1")
			m, call := patTouch(recAt)
			m.Spec.Clocks = c.learned
			if c.recording != nil {
				c.recording(m)
			}
			if c.noise != "" {
				m.Spec.ReqBodyNoise = map[string][]string{c.noise: {}}
			}
			upd := c.updated
			if upd.IsZero() {
				upd = now
			}
			in := call(c.lastUsed, upd)
			if c.body != "" {
				in = patTouchCall(c.body)
			}
			in.arrived = base.Add(2 * time.Second)
			db := sourcedDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, sources: map[string][]models.DeclaredSource{"mock-220": c.sources},
				background: map[string]bool{"mock-220": !c.own}, timeForm: c.timeForm}
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
// no sources. It reads the store's background and its time default
// (declaredIn): a test's own recording gets no rule's range, and under the
// time default the background's gets the form its recorded value writes.
func TestADeclaredClockIsReadUnderTheProvenSwitchOnly(t *testing.T) {
	m, _ := patTouch(time.Date(2026, 11, 11, 14, 6, 39, 0, time.UTC))
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	db := sourcedDb{mockMemDb: &mockMemDb{}, sources: map[string][]models.DeclaredSource{
		"mock-220": {{Rule: "pat-last-used-at", Kind: models.DeclaredSourceClock, Field: "body.data.lastUsedAt", Clock: plain}},
	}, background: map[string]bool{"mock-220": true}}
	t.Setenv("KEPLOY_PROVEN", "")
	if got := clocksWith(m, declaredIn(db)); len(got) != 0 {
		t.Fatalf("switch off: %v; want none", got)
	}
	if got := declaredClockFields(m, declaredIn(db)); len(got) != 0 {
		t.Fatalf("switch off: declared fields %v; want none", got)
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	if got := clocksWith(m, declaredIn(db)); len(got) != 1 || got["body.data.lastUsedAt"] != (models.ClockOffset{Res: time.Second}) {
		t.Fatalf("switch on: %v; want body.data.lastUsedAt at a cut of 1s", got)
	}
	db.timeForm = true
	if got := clocksWith(m, declaredIn(db)); len(got) != 1 || got["body.data.lastUsedAt"] != (models.ClockOffset{Form: "number 9999999999"}) {
		t.Fatalf("the time default: %v; want body.data.lastUsedAt by its form", got)
	}
	db.background = nil
	if got := clocksWith(m, declaredIn(db)); len(got) != 0 {
		t.Fatalf("a test's own recording: %v; want none", got)
	}
	if got := clocksWith(m, declaredRules{}); len(got) != 0 {
		t.Fatalf("no sources: %v; want none", got)
	}
	if got := clocksWith(m, declaredRules{sources: sourcesIn(db)}); len(got) != 0 {
		t.Fatalf("no background reader: %v; want none", got)
	}
	if got := sourcesIn(nil)("mock-220"); got != nil {
		t.Fatalf("a nil store: %v; want none", got)
	}
	if got := declaredIn(nil); got.background(m) || got.timeForm {
		t.Fatalf("a nil store: %+v; want no background and no time default", got)
	}
}

// The learn step reads a recording's clock rules as the replay does (its
// noise built withSources, then known): noise an earlier learning stored
// at a clock rule's field, or at a container of it, excuses nothing there,
// in the background's recording and in a test's own, so the learn step
// reads the field again and the rule decides it. A field no rule names
// keeps the noise stored there.
func TestTheLearnStepReadsADeclaredClockAsTheReplayDoes(t *testing.T) {
	recAt := time.Date(2026, 11, 11, 14, 6, 39, 0, time.UTC)
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	rules := []models.DeclaredSource{{Rule: "pat-last-used-at", Kind: models.DeclaredSourceClock, Field: "body.data.lastUsedAt", Clock: plain}}
	field := learn.Leaf{Path: "body.data.lastUsedAt", Noise: "body.data.lastUsedAt", Type: learn.Number, Value: strconv.FormatInt(recAt.Unix(), 10)}
	for _, c := range []struct {
		name       string
		background bool
		noise      string
		rules      []models.DeclaredSource
		excused    bool
	}{
		{"the background's recording", true, "body.data.lastUsedAt", rules, false},
		{"a test's own recording", false, "body.data.lastUsedAt", rules, false},
		{"a test's own recording, noise on the field's container", false, "body.data", rules, false},
		{"a test's own recording, no rule", false, "body.data.lastUsedAt", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, n, _ := learnOn(t)
			m, _ := patTouch(recAt)
			m.Spec.ReqBodyNoise = map[string][]string{c.noise: {}}
			db := sourcedDb{mockMemDb: &mockMemDb{}, sources: map[string][]models.DeclaredSource{"mock-220": c.rules}, background: map[string]bool{"mock-220": c.background}}
			k, ok := n.withSources(db).known(m).(learn.PairKnown)
			if !ok {
				t.Fatalf("known(m) is no learn.PairKnown")
			}
			if excused, _ := k.ExcusedPair(field, field, true, true); excused != c.excused {
				t.Fatalf("the stored noise at body.data.lastUsedAt excused it: %v; want %v", excused, c.excused)
			}
		})
	}
}

// A replay lets no noise learned on a field a clock rule names excuse its
// value (strictKnown.learnedAt), there or on a container of it, unless
// learning learned a read of the clock there, which clockDiffs checks.
func TestLearnedNoiseExcusesNoFieldAClockRuleNames(t *testing.T) {
	const f = "body.data.lastUsedAt"
	declared := map[string]models.ClockOffset{f: {Res: time.Second}}
	for _, c := range []struct {
		name     string
		learned  string
		declared map[string]models.ClockOffset
		clocks   map[string]models.ClockOffset
		want     bool
	}{
		{"noise at a declared field", f, declared, nil, false},
		{"noise on a declared field's container", "body.data", declared, nil, false},
		{"noise at a field no rule names", f, nil, nil, true},
		{"a read of the clock learned at a declared field", f, declared, declared, true},
	} {
		k := strictKnown{learned: map[string][]string{c.learned: {}}, declared: c.declared, clocks: c.clocks}
		if got := k.learnedAt(f, ""); got != c.want {
			t.Errorf("%s: learnedAt %v; want %v", c.name, got, c.want)
		}
	}
}
