package http

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// keploy.yml's noise stops the comparison at a field, never the answer
// rewrite: a call that differs from its recording only where keploy.yml
// declares noise is served, and a value an earlier call of the test sent
// that it carries there is still that variable's use, so the answer that
// echoes it holds this run's value. Nothing is learned as noise at a
// declared field, and a declared field that carries no variable learns
// nothing.
func TestLearnMatchFollowsAVariableWhereKeployYmlDeclaresNoise(t *testing.T) {
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, map[string][]string{"q.id": {}, "note": {}, "ref": {}}, nil)
	setup := func() (context.Context, *learnDb) {
		tr := learn.NewTracker()
		tr.Begin()
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "POST", "/v1/find", `{"q":{"id":"o-1"},"note":"a"}`, nil, `{"id":"o-1","state":"new"}`),
			learnRecorded("mock-2", 2*time.Second, "POST", "/v1/echo", `{"ref":"zeta"}`, nil, `{"ref":"zeta"}`),
		}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts); served == nil || miss != nil {
			t.Fatalf("the first call is paired: %+v", miss)
		}
		return ctx, db
	}

	// The find carries the order this run sent at a declared field.
	ctx, db := setup()
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/find", `{"q":{"id":"o-9"},"note":"b"}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if served.Spec.HTTPResp.Body != `{"id":"o-9","state":"new"}` {
		t.Fatalf("served %s: the answer holds this run's value", served.Spec.HTTPResp.Body)
	}
	use := db.consumed[1]
	if want := (models.MockVariables{"order": {Recorded: "o-1", Use: []string{"body.q.id"}, Answer: []string{"body.id"}}}); !reflect.DeepEqual(use.Spec.Learned(), want) {
		t.Fatalf("variables %+v, want %+v", use.Spec.Learned(), want)
	}
	if want := (map[string]models.MockVariables{"mock-0": {"order": {Recorded: "o-1", Bind: "body.order"}}}); !reflect.DeepEqual(use.Spec.LearnedBinds(), want) {
		t.Fatalf("binds %+v, want %+v", use.Spec.LearnedBinds(), want)
	}
	if len(use.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("keploy.yml's noise is never learned onto the recording: %v", use.Spec.ReqBodyNoise)
	}

	// A call that differs only where keploy.yml declares noise, with a value
	// there keploy cannot follow (not the one the test sent), is served as
	// recorded and learns nothing: the user's noise stops the comparison.
	ctx, db = setup()
	served, miss, err = h.learnMatch(ctx, live("POST", "/v1/find", `{"q":{"id":"o-5"},"note":"b"}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if served.Spec.HTTPResp.Body != `{"id":"o-1","state":"new"}` {
		t.Fatalf("served %s: nothing carries this run's value, so the answer is as recorded", served.Spec.HTTPResp.Body)
	}
	if got := db.consumed[1]; len(got.Spec.Learned()) != 0 || len(got.Spec.LearnedBinds()) != 0 || len(got.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("learned %+v binds %+v noise %v: a declared field keploy cannot follow learns nothing", got.Spec.Learned(), got.Spec.LearnedBinds(), got.Spec.ReqBodyNoise)
	}

	// A fresh value at a declared field that the call's answer echoes is the
	// call's own variable: the answer holds this run's value.
	ctx, db = setup()
	served, miss, err = h.learnMatch(ctx, live("POST", "/v1/echo", `{"ref":"kappa"}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil || served.Spec.HTTPResp.Body != `{"ref":"kappa"}` {
		t.Fatalf("served %v miss %+v err %v: the echo holds this run's value", served, miss, err)
	}
}

// A declared field the answer echoes where this run's value cannot stand
// (a number place, sent a value that is no number) is a variable keploy
// cannot answer with: the call is served as recorded and learns nothing.
func TestLearnMatchServesAsRecordedWhereTheAnswerCannotHoldADeclaredValue(t *testing.T) {
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, map[string][]string{"ref": {}}, nil)
	tr := learn.NewTracker()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/echo", `{"ref":"41"}`, nil, `{"ref":41}`),
	}}
	ctx := learn.WithTracker(context.Background(), tr)
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/echo", `{"ref":"4x"}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if served.Spec.HTTPResp.Body != `{"ref":41}` {
		t.Fatalf("served %s, want the recorded answer", served.Spec.HTTPResp.Body)
	}
	if len(db.consumed) != 1 {
		t.Fatalf("%d consumed, want the recording", len(db.consumed))
	}
	if got := db.consumed[0]; len(got.Spec.Learned()) != 0 || len(got.Spec.LearnedBinds()) != 0 || len(got.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("learned %+v binds %+v noise %v, want nothing", got.Spec.Learned(), got.Spec.LearnedBinds(), got.Spec.ReqBodyNoise)
	}
}

// keploy.yml's header noise is declared too; keploy's own list of headers
// that change every run (models.FlakyHeaders) is not: a value carried there
// is served as recorded, as before.
func TestLearnMatchFollowsAVariableInAHeaderKeployYmlDeclaresNoise(t *testing.T) {
	h, _, _ := learnOn(t)
	opts := models.OutgoingOptions{Learn: true, NoiseConfig: map[string]map[string][]string{"header": {"X-Order": {}}}}
	n := h.newLearnNoise(opts, nil, nil)
	for _, row := range []struct{ header, want string }{
		{"X-Order", `{"id":"o-9"}`},
		{"X-Request-Id", `{"id":"o-1"}`},
	} {
		tr := learn.NewTracker()
		tr.Begin()
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "GET", "/v1/find", "", map[string]string{row.header: "o-1"}, `{"id":"o-1"}`),
		}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts); served == nil || miss != nil {
			t.Fatalf("the first call is paired: %+v", miss)
		}
		served, miss, err := h.learnMatch(ctx, live("GET", "/v1/find", "", map[string]string{row.header: "o-9"}), db, n, opts)
		if err != nil || miss != nil || served == nil {
			t.Fatalf("%s: served %v miss %+v err %v", row.header, served, miss, err)
		}
		if served.Spec.HTTPResp.Body != row.want {
			t.Fatalf("%s: served %s, want %s", row.header, served.Spec.HTTPResp.Body, row.want)
		}
	}
}

// A header keploy's own list already excuses (X-Request-Id, traceparent,
// the proven switch's amz-sdk-retry) stays keploy's own noise when
// keploy.yml names it too: no leaf of the call's shape, so a call whose id
// drifts is exact, never read against every earlier exchange, and a value
// carried there is served as recorded, as when keploy.yml names nothing. A
// header only keploy.yml names (X-Order), a credential the list leaves
// compared (Authorization), and any header once the list is off
// (DisableAutoHeaderNoise) are leaves keploy.yml declares, followed.
func TestLearnMatchADeclaredHeaderOnKeploysOwnListStaysKeploysNoise(t *testing.T) {
	h, _, _ := learnOn(t)
	declared := map[string][]string{"X-Order": {}, "X-Request-Id": {}, "traceparent": {}, "Authorization": {}, "amz-sdk-retry": {}}
	for _, row := range []struct {
		header  string
		listOff bool
		leaf    bool
		want    string
	}{
		{"X-Order", false, true, `{"id":"o-9"}`},
		{"Authorization", false, true, `{"id":"o-9"}`},
		{"X-Request-Id", false, false, `{"id":"o-1"}`},
		{"Traceparent", false, false, `{"id":"o-1"}`},
		{"Amz-Sdk-Retry", false, false, `{"id":"o-1"}`},
		{"X-Request-Id", true, true, `{"id":"o-9"}`},
	} {
		opts := models.OutgoingOptions{Learn: true, DisableAutoHeaderNoise: row.listOff, NoiseConfig: map[string]map[string][]string{"header": declared}}
		n := h.newLearnNoise(opts, nil, nil)
		name := row.header
		if row.listOff {
			name += " (list off)"
		}
		find := live("GET", "/v1/find", "", map[string]string{row.header: "o-9"})
		if _, leaf := n.shape(liveParts(context.Background(), find), "").Leaves["header."+strings.ToLower(row.header)]; leaf != row.leaf {
			t.Fatalf("%s: a leaf of the call's shape: %v, want %v", name, leaf, row.leaf)
		}
		tr := learn.NewTracker()
		tr.Begin()
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "GET", "/v1/find", "", map[string]string{row.header: "o-1"}, `{"id":"o-1"}`),
		}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts); served == nil || miss != nil {
			t.Fatalf("%s: the first call is paired: %+v", name, miss)
		}
		served, miss, err := h.learnMatch(ctx, find, db, n, opts)
		if err != nil || miss != nil || served == nil {
			t.Fatalf("%s: served %v miss %+v err %v", name, served, miss, err)
		}
		if served.Spec.HTTPResp.Body != row.want {
			t.Fatalf("%s: served %s, want %s", name, served.Spec.HTTPResp.Body, row.want)
		}
		if got := db.consumed[1]; !row.leaf && (len(got.Spec.Learned()) != 0 || len(got.Spec.LearnedBinds()) != 0) {
			t.Fatalf("%s: learned %+v binds %+v, want nothing", name, got.Spec.Learned(), got.Spec.LearnedBinds())
		}
	}
}

// A value an earlier call sent in a header the comparison excuses, whether
// keploy.yml declares it (X-Order) or keploy's own list of headers that
// change every run names it (X-Request-Id), is a place a later call's
// value flowed from: the later call that carries it is paired, and its
// answer holds this run's value. The earlier call names that header once.
func TestLearnMatchFollowsAValueSentInAnExcusedHeader(t *testing.T) {
	h, _, _ := learnOn(t)
	opts := models.OutgoingOptions{Learn: true, NoiseConfig: map[string]map[string][]string{"header": {"X-Order": {}}}}
	n := h.newLearnNoise(opts, nil, nil)
	for _, header := range []string{"X-Order", "X-Request-Id"} {
		tr := learn.NewTracker()
		tr.Begin()
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/orders", `{}`, map[string]string{header: "o-1"}, `{}`),
			learnRecorded("mock-1", time.Second, "POST", "/v1/find", `{"q":{"id":"o-1"}}`, nil, `{"id":"o-1"}`),
		}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, err := h.learnMatch(ctx, live("POST", "/v1/orders", `{}`, map[string]string{header: "o-9"}), db, n, opts); err != nil || served == nil || miss != nil {
			t.Fatalf("%s: the first call is served: %v %+v %v", header, served, miss, err)
		}
		_, earlier, _, _ := tr.Test()
		if len(earlier) != 1 {
			t.Fatalf("%s: %d earlier exchanges, want the first call's", header, len(earlier))
		}
		named := 0
		for _, s := range earlier[0].Seen {
			if s.Leaf.Path == "header."+strings.ToLower(header) {
				named++
			}
		}
		if named != 1 {
			t.Fatalf("%s: the first call names its header %d times, want once", header, named)
		}
		served, miss, err := h.learnMatch(ctx, live("POST", "/v1/find", `{"q":{"id":"o-9"}}`, nil), db, n, opts)
		if err != nil || miss != nil || served == nil {
			t.Fatalf("%s: served %v miss %+v err %v", header, served, miss, err)
		}
		if served.Spec.HTTPResp.Body != `{"id":"o-9"}` {
			t.Fatalf("%s: served %s: the answer holds this run's value", header, served.Spec.HTTPResp.Body)
		}
		want := map[string]models.MockVariables{"mock-0": {strings.ToLower(header): {Recorded: "o-1", Bind: "header." + strings.ToLower(header)}}}
		if got := db.consumed[1].Spec.LearnedBinds(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: binds %+v, want %+v", header, got, want)
		}
	}
}

// A call is followed whole or not at all: when one field keploy.yml
// declares carries a variable's use and another carries a value keploy
// cannot answer with this run's (one its answer echoes only inside a
// longer value), the call is served as recorded and learns nothing, never
// with the one variable rewritten.
func TestLearnMatchFollowsADeclaredCallWholeOrNotAtAll(t *testing.T) {
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, map[string][]string{"q.id": {}, "ref": {}}, nil)
	tr := learn.NewTracker()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
		learnRecorded("mock-1", time.Second, "POST", "/v1/find", `{"q":{"id":"o-1"},"ref":"zeta"}`, nil, `{"id":"o-1","msg":"ref zeta ok"}`),
	}}
	ctx := learn.WithTracker(context.Background(), tr)
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts); served == nil || miss != nil {
		t.Fatalf("the first call is paired: %+v", miss)
	}
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/find", `{"q":{"id":"o-9"},"ref":"kappa"}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	if served.Spec.HTTPResp.Body != `{"id":"o-1","msg":"ref zeta ok"}` {
		t.Fatalf("served %s, want the recorded answer", served.Spec.HTTPResp.Body)
	}
	if got := db.consumed[1]; len(got.Spec.Learned()) != 0 || len(got.Spec.LearnedBinds()) != 0 || len(got.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("learned %+v binds %+v noise %v, want nothing", got.Spec.Learned(), got.Spec.LearnedBinds(), got.Spec.ReqBodyNoise)
	}
}

// A pool request recorded several times answers with each recorded answer
// in turn (cursorPick), whichever of its recordings Pair found. What
// drifted at a field keploy.yml declares is read on the recording that
// answers, against its own answer and the calls recorded before it: the
// second recording of X-Ref echoes the value this run sent in its own
// header; the first recording echoes it too when this run sends the value
// the later recording was sent, though Pair finds the later one exact; and
// one sent the body value the first recording was sent before it is served
// as recorded, as a pool call that carries an earlier call's value is (only
// a test's own calls carry variables). The log that names what was learned
// names the recording that answers and learned it, never the one Pair found.
func TestLearnMatchReadsADeclaredFieldOnTheCursorsRecording(t *testing.T) {
	h, _, _ := learnOn(t)
	core, logs := observer.New(zapcore.InfoLevel)
	h.Logger = zap.New(core)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true, NoiseConfig: map[string]map[string][]string{"header": {"X-Ref": {}}}}
	n := h.newLearnNoise(opts, map[string][]string{"ref": {}}, nil)
	recorded := func(name string, at time.Duration, body, xref, answer string) *models.Mock {
		m := learnRecorded(name, at, "POST", "/v1/echo", body, map[string]string{"X-Ref": xref}, answer)
		m.TestModeInfo.Lifetime = models.LifetimeSession
		m.TestModeInfo.Consume = models.ConsumeCursorSaturate
		return m
	}
	for _, row := range []struct {
		name               string
		first, second      *models.Mock
		body, xref1, xref2 string
		want               []string
		learnedSecond      bool
		learnedBy          []string // the recording each call's log says learned the field ("" for none)
	}{
		{"its own header", recorded("pool-1", -time.Hour, `{"ref":"zeta"}`, "r-1", `{"x":"r-1","n":1}`),
			recorded("pool-2", -time.Hour+time.Second, `{"ref":"zeta"}`, "r-2", `{"x":"r-2","n":2}`),
			`{"ref":"zeta"}`, "r-8", "r-9", []string{`{"x":"r-8","n":1}`, `{"x":"r-9","n":2}`}, true, []string{"pool-1", "pool-2"}},
		{"a body value sent before it", recorded("pool-1", -time.Hour, `{"ref":"zeta"}`, "r-1", `{"ref":"zeta","n":1}`),
			recorded("pool-2", -time.Hour+time.Second, `{"ref":"zeta"}`, "r-1", `{"ref":"zeta","n":2}`),
			`{"ref":"kappa"}`, "r-1", "r-1", []string{`{"ref":"kappa","n":1}`, `{"ref":"zeta","n":2}`}, false, []string{"pool-1", ""}},
		{"the value the later recording was sent", recorded("pool-1", -time.Hour, `{"ref":"zeta"}`, "r-1", `{"x":"r-1","n":1}`),
			recorded("pool-2", -time.Hour+time.Second, `{"ref":"zeta"}`, "r-2", `{"x":"r-2","n":2}`),
			`{"ref":"zeta"}`, "r-2", "r-2", []string{`{"x":"r-2","n":1}`, `{"x":"r-2","n":2}`}, false, []string{"pool-1", ""}},
	} {
		db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{row.first, row.second}}}
		ctx := learn.WithTracker(context.Background(), learn.NewTracker())
		for i, xref := range []string{row.xref1, row.xref2} {
			logs.TakeAll()
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/echo", row.body, map[string]string{"X-Ref": xref}), db, n, opts)
			if err != nil || miss != nil || served == nil {
				t.Fatalf("%s: call %d: served %v miss %+v err %v", row.name, i+1, served, miss, err)
			}
			if served.Spec.HTTPResp.Body != row.want[i] {
				t.Fatalf("%s: call %d served %s, want %s", row.name, i+1, served.Spec.HTTPResp.Body, row.want[i])
			}
			var names []string
			for _, e := range logs.FilterMessage("learned a field that changed between record and replay").All() {
				if name, _ := e.ContextMap()["mock"].(string); len(names) == 0 || names[len(names)-1] != name {
					names = append(names, name)
				}
			}
			if by := strings.Join(names, ","); by != row.learnedBy[i] {
				t.Fatalf("%s: call %d: the log says %q learned the field, want %q", row.name, i+1, by, row.learnedBy[i])
			}
		}
		if got := len(db.mocks[1].Spec.Learned()) > 0; got != row.learnedSecond {
			t.Fatalf("%s: pool-2 learned %+v", row.name, db.mocks[1].Spec.Learned())
		}
		for _, m := range db.mocks {
			if len(m.Spec.ReqBodyNoise) != 0 {
				t.Fatalf("%s: keploy.yml's noise is never learned onto %s: %v", row.name, m.Name, m.Spec.ReqBodyNoise)
			}
		}
	}
}

// What drifted at a field keploy.yml declares is read on the noise learned
// onto the recording that answers, never onto the one Pair found. On the
// second call Pair finds the first recording again (a session's recording
// is not spent once it answers), while the cursor answers with the second.
// Noise keploy learned onto the first leaves the second's answer echoing
// the value this run sent; noise learned onto the second serves it as
// recorded. Learned noise wins only on its own recording.
func TestLearnMatchReadsTheCursorsRecordingsLearnedNoise(t *testing.T) {
	h, _, _ := learnOn(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true, NoiseConfig: map[string]map[string][]string{"header": {"X-Ref": {}}}}
	n := h.newLearnNoise(opts, map[string][]string{"ref": {}}, nil)
	recorded := func(name string, at time.Duration, xref, answer string) *models.Mock {
		m := learnRecorded(name, at, "POST", "/v1/echo", `{"ref":"zeta"}`, map[string]string{"X-Ref": xref}, answer)
		m.TestModeInfo.Lifetime = models.LifetimeSession
		m.TestModeInfo.Consume = models.ConsumeCursorSaturate
		return m
	}
	for _, row := range []struct {
		name    string
		learned int // the recording keploy learned X-Ref as noise onto
		want    []string
	}{
		{"learned onto the recording Pair finds", 0, []string{`{"x":"r-1","n":1}`, `{"x":"r-9","n":2}`}},
		{"learned onto the recording that answers", 1, []string{`{"x":"r-8","n":1}`, `{"x":"r-2","n":2}`}},
	} {
		mocks := []*models.Mock{
			recorded("pool-1", -time.Hour, "r-1", `{"x":"r-1","n":1}`),
			recorded("pool-2", -time.Hour+time.Second, "r-2", `{"x":"r-2","n":2}`),
		}
		mocks[row.learned].Spec.ReqBodyNoise = map[string][]string{"header.x-ref": {}}
		db := &learnDb{mockMemDb: mockMemDb{mocks: mocks}}
		ctx := learn.WithTracker(context.Background(), learn.NewTracker())
		for i, xref := range []string{"r-8", "r-9"} {
			served, miss, err := h.learnMatch(ctx, live("POST", "/v1/echo", `{"ref":"zeta"}`, map[string]string{"X-Ref": xref}), db, n, opts)
			if err != nil || miss != nil || served == nil {
				t.Fatalf("%s: call %d: served %v miss %+v err %v", row.name, i+1, served, miss, err)
			}
			if served.Spec.HTTPResp.Body != row.want[i] {
				t.Fatalf("%s: call %d served %s, want %s", row.name, i+1, served.Spec.HTTPResp.Body, row.want[i])
			}
		}
		if other := mocks[1-row.learned]; len(other.Spec.ReqBodyNoise) != 0 {
			t.Fatalf("%s: keploy.yml's noise is never learned onto %s: %v", row.name, other.Name, other.Spec.ReqBodyNoise)
		}
	}
}

// A keploy.yml body entry with patterns declares a field only where both
// the recorded value and the live one match a pattern: a pattern constrains
// the live value too. The echo then holds this run's value. A live value no
// pattern matches is never followed into the answer, and an obfuscated
// recorded value (one the recording's own noise excuses) is no value
// keploy.yml declares, whatever the live value: each is answered as
// recorded or refused, and learns nothing.
func TestLearnMatchADeclaredPatternConstrainsBothSides(t *testing.T) {
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, map[string][]string{"ref": {"^[a-z]+$"}}, nil)
	for _, c := range []struct {
		name, rec, live, want string
	}{
		{"both values match the pattern", "zeta", "kappa", `{"ref":"kappa"}`},
		{"the live value matches no pattern", "zeta", "KAPPA-9", `{"ref":"zeta"}`},
		{"an obfuscated recorded value", "OBF-1", "kappa", `{"ref":"OBF-1"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := learn.NewTracker()
			tr.Begin()
			m := learnRecorded("mock-0", 0, "POST", "/v1/echo", `{"ref":"`+c.rec+`"}`, nil, `{"ref":"`+c.rec+`"}`)
			m.Noise = []string{"^OBF-[0-9]+$"}
			db := &learnDb{perTest: []*models.Mock{m}}
			served, _, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/echo", `{"ref":"`+c.live+`"}`, nil), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if served != nil && served.Spec.HTTPResp.Body != c.want {
				t.Fatalf("served %s, want %s", served.Spec.HTTPResp.Body, c.want)
			}
			if c.want == `{"ref":"kappa"}` && served == nil {
				t.Fatal("a value both sides of the pattern hold is followed, never refused")
			}
			for _, got := range db.consumed {
				if c.want != `{"ref":"kappa"}` && (len(got.Spec.Learned()) != 0 || len(got.Spec.LearnedBinds()) != 0) || len(got.Spec.ReqBodyNoise) != 0 {
					t.Fatalf("learned %+v binds %+v noise %v", got.Spec.Learned(), got.Spec.LearnedBinds(), got.Spec.ReqBodyNoise)
				}
			}
		})
	}
}

// An exchange the declared path followed is named by its variables, as a
// paired one is: a later test's call that sends at a declared field the
// value an earlier test made uses the run's variable, and its second call,
// which sends that value again, uses the run's variable too, never one of
// the test's own bound at the first call's declared field.
func TestLearnMatchALaterCallReadsTheRunsVariableThroughADeclaredExchange(t *testing.T) {
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, map[string][]string{"q.id": {}}, nil)
	tr := learn.NewTracker()
	tr.NewSet()
	ctx := learn.WithTracker(context.Background(), tr)
	create := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`)
	find := learnRecorded("mock-1", time.Second, "POST", "/v1/find", `{"q":{"id":"o-1"}}`, nil, `{"state":"new"}`)
	del := learnRecorded("mock-2", 2*time.Second, "DELETE", "/v1/orders/o-1", "", nil, `{}`)
	tr.Begin()
	a := &learnDb{perTest: []*models.Mock{create}}
	if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), a, n, opts); served == nil || miss != nil {
		t.Fatalf("test A's create: %+v", miss)
	}
	tr.End()
	tr.Begin()
	b := &learnDb{perTest: []*models.Mock{find, del}}
	for _, r := range []*req{live("POST", "/v1/find", `{"q":{"id":"o-9"}}`, nil), live("DELETE", "/v1/orders/o-9", "", nil)} {
		if served, miss, _ := h.learnMatch(ctx, r, b, n, opts); served == nil || miss != nil {
			t.Fatalf("test B's %s of test A's order: %+v", r.method, miss)
		}
	}
	tr.End()
	name := models.RunVariablePrefix + "order"
	if len(b.consumed) != 2 {
		t.Fatalf("%d of test B's calls consumed, want 2", len(b.consumed))
	}
	for _, c := range b.consumed {
		use, binds := c.Spec.Learned(), c.Spec.LearnedBinds()
		if len(use) != 1 || len(use[name].Use) != 1 || len(binds) != 1 || len(binds["mock-0"]) != 1 || binds["mock-0"][name].Bind != "body.order" {
			t.Fatalf("test B's %s uses %+v and binds %+v, want only the run's %s, bound at test A's call", c.Name, use, binds, name)
		}
	}
}

// A declared field is read against the earlier places of the call's own
// recorded test (ofPairedTest, recordedBeforeOf), as a paired call's drift
// is. A test's own call inside its recorded test's window is no copy of
// another recorded test's rows: a token that test's login answered, which
// the store keeps on disk, is no place it reads, so the echo at the field
// keploy.yml declares holds this run's value. A pool call inside a window
// may carry a value of any recording made before it: the same token, there,
// may have flowed from that login, and the call is served as recorded.
func TestLearnMatchADeclaredFieldReadsTheEarlierPlacesOfTheCallsOwnTest(t *testing.T) {
	h, _, opts := learnOn(t)
	opts.LearnPool = true
	n := h.newLearnNoise(opts, map[string][]string{"token": {}}, nil)
	for _, row := range []struct {
		name string
		pool bool
		want string
	}{
		{"a test's own call", false, `{"token":"tok-9d8e7f","ok":true}`},
		{"a pool call inside a window", true, `{"token":"tok-0c41b2","ok":true}`},
	} {
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		login := learnRecorded("mock-0", 0, "POST", "/v1/login", `{"user":"u-7"}`, nil, `{"token":"tok-0c41b2"}`)
		check := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/sessions/check", `{"token":"tok-0c41b2"}`, nil, `{"token":"tok-0c41b2","ok":true}`)
		store := &recordedBeforeDb{learnDb: &learnDb{perTest: []*models.Mock{check}, otherTest: map[time.Time]bool{login.Spec.ReqTimestampMock: true}}, onDisk: []*models.Mock{login}}
		var db integrations.MockMemDb = store
		if row.pool {
			db = poolView{store}
		}
		served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions/check", `{"token":"tok-9d8e7f"}`, nil), db, n, opts)
		if err != nil || miss != nil || served == nil {
			t.Fatalf("%s: served %v miss %+v err %v", row.name, served, miss, err)
		}
		if served.Spec.HTTPResp.Body != row.want {
			t.Fatalf("%s: served %s, want %s", row.name, served.Spec.HTTPResp.Body, row.want)
		}
	}
}

// A call the learn step follows, whose recording's answer the agent parked
// on disk (diskmocks.go), has that answer read back by the learn step, which
// hands what it read to the exchange it keeps for later calls, and by the
// serve that writes this run's value into it (claimLearned): never a third
// time. The exchange states the answer F gave the application, with this
// run's value (learnedView), which a later test's call reads it by
// (learn.Earlier.View). So on the paired path and on the declared one.
func TestLearnMatchAFollowedCallsParkedAnswerIsNotReadAgainAndStatesItsView(t *testing.T) {
	for _, row := range []struct {
		name  string
		noise map[string][]string
	}{
		{"paired", nil},
		{"declared", map[string][]string{"q.id": {}}},
	} {
		h, _, opts := learnOn(t)
		n := h.newLearnNoise(opts, row.noise, nil)
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		create := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`)
		find := learnRecorded("mock-1", time.Second, "POST", "/v1/find", `{"q":{"id":"o-1"}}`, nil, "")
		parked := *find.Spec.HTTPResp
		parked.Body = `{"id":"o-1","state":"new"}`
		find.Spec.HTTPResp = nil
		reads := 0
		find.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
			reads++
			r := parked
			return &r, nil, nil
		})
		db := &learnDb{perTest: []*models.Mock{create, find}}
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts); served == nil || miss != nil {
			t.Fatalf("%s: the create is paired: %+v", row.name, miss)
		}
		served, miss, err := h.learnMatch(ctx, live("POST", "/v1/find", `{"q":{"id":"o-9"}}`, nil), db, n, opts)
		if err != nil || miss != nil || served == nil {
			t.Fatalf("%s: served %v miss %+v err %v", row.name, served, miss, err)
		}
		if served.Spec.HTTPResp.Body != `{"id":"o-9","state":"new"}` {
			t.Fatalf("%s: served %s: the answer holds this run's value", row.name, served.Spec.HTTPResp.Body)
		}
		if reads != 2 {
			t.Fatalf("%s: the answer parked on disk was read back %d times, want twice (the learn step, the serve)", row.name, reads)
		}
		_, earlier, _, _ := tr.Test()
		var view func() (*models.Mock, error)
		for _, e := range earlier {
			if e.Mock == "mock-1" {
				view = e.View
			}
		}
		if view == nil {
			t.Fatalf("%s: the find's exchange states no view of the answer F gave", row.name)
		}
		got, err := view()
		if err != nil || got.Spec.HTTPResp == nil || got.Spec.HTTPResp.Body != `{"id":"o-9","state":"new"}` {
			t.Fatalf("%s: the find's view %+v err %v, want the answer F gave", row.name, got, err)
		}
	}
}

// A declared field whose value keploy cannot follow into the answer (here a
// token another test's login answered, which a pool call carries) is served
// as recorded, and the reason keploy logs says the field is declared and why
// the value was still refused. It never tells the user to declare the field
// noise in keploy.yml: the evidence a paired call's refusal carries ends in
// that remedy, which a declared field already has.
func TestLearnMatchADeclaredFieldsRefusalSaysItIsDeclared(t *testing.T) {
	calls := func(t *testing.T, bodyNoise map[string][]string) (*models.Mock, *learnMiss, *learnDb, *observer.ObservedLogs) {
		h, _, opts := learnOn(t)
		core, logs := observer.New(zapcore.DebugLevel)
		h.Logger = zap.New(core)
		opts.LearnPool = true
		n := h.newLearnNoise(opts, bodyNoise, nil)
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)
		login := learnRecorded("mock-0", 0, "POST", "/v1/login", `{"user":"u-7"}`, nil, `{"token":"tok-0c41b2"}`)
		refresh := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/sessions/refresh", `{"token":"tok-0c41b2"}`, nil, `{"ok":true}`)
		ldb := &learnDb{perTest: []*models.Mock{refresh}, otherTest: map[time.Time]bool{login.Spec.ReqTimestampMock: true}}
		db := poolView{&recordedBeforeDb{learnDb: ldb, onDisk: []*models.Mock{login}}}
		served, miss, err := h.learnMatch(ctx, live("POST", "/v1/sessions/refresh", `{"token":"tok-9d8e7f"}`, nil), db, n, opts)
		if err != nil {
			t.Fatal(err)
		}
		return served, miss, ldb, logs
	}

	// Not declared: the refusal is the miss's, and its evidence names the
	// remedy for a field keploy.yml does not declare.
	_, miss, _, _ := calls(t, nil)
	if miss == nil || !strings.Contains(miss.res.Why, "holds earlier (in mock-0)") || !strings.Contains(miss.res.Why, "declare it noise in keploy.yml") {
		t.Fatalf("miss %+v, want the paired refusal with its evidence", miss)
	}

	served, miss, db, logs := calls(t, map[string][]string{"token": {}})
	if miss != nil || served == nil || served.Spec.HTTPResp.Body != `{"ok":true}` {
		t.Fatalf("served %v miss %+v, want mock-1 served as recorded", served, miss)
	}
	if len(db.consumed) != 1 || len(db.consumed[0].Spec.Learned()) != 0 || len(db.consumed[0].Spec.LearnedBinds()) != 0 || len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("took %+v, want mock-1 with nothing learned", db.consumed)
	}
	said := logs.FilterMessage("keploy.yml declares these fields noise, so keploy does not compare them, but it cannot follow the value one carries into the answer; the call is served as recorded").All()
	if len(said) != 1 {
		t.Fatalf("%d declared-field refusals logged, want 1: %v", len(said), logs.All())
	}
	f := said[0].ContextMap()
	why, _ := f["why"].(string)
	if !reflect.DeepEqual(f["declared"], []interface{}{"body.token"}) || !strings.HasPrefix(why, "body.token carries a value the test's recording holds earlier (in mock-0)") {
		t.Fatalf("logged %v, want the declared field and why it was refused", f)
	}
	if strings.Contains(why, "keploy.yml") || strings.Contains(why, "declare") {
		t.Fatalf("the reason %q tells the user to declare a field keploy.yml declares", why)
	}
}

// A leaf another parser's caller holds LearnEchoOnly is learned on the
// declared path only as an echo, as on the paired one (heldLearned): a
// variable's use there is refused, and the call is served as recorded with
// nothing learned; an echo there still holds this run's value.
func TestLearnMatchADeclaredFieldTheCallerHoldsEchoOnlyIsNoVariablesUse(t *testing.T) {
	h, _, opts := learnOn(t)
	core, logs := observer.New(zapcore.DebugLevel)
	h.Logger = zap.New(core)
	n := h.newLearnNoise(opts, map[string][]string{"q.id": {}, "ref": {}}, nil)
	n.hold = func(leaf string) LearnHold {
		if leaf == "body.q.id" || leaf == "body.ref" {
			return LearnEchoOnly
		}
		return LearnAny
	}
	setup := func() (context.Context, *learnDb) {
		tr := learn.NewTracker()
		tr.Begin()
		db := &learnDb{perTest: []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"order":"o-1"}`, nil, `{}`),
			learnRecorded("mock-1", time.Second, "POST", "/v1/find", `{"q":{"id":"o-1"}}`, nil, `{"id":"o-1","state":"new"}`),
			learnRecorded("mock-2", 2*time.Second, "POST", "/v1/echo", `{"ref":"zeta"}`, nil, `{"ref":"zeta"}`),
		}}
		ctx := learn.WithTracker(context.Background(), tr)
		if served, miss, _ := h.learnMatch(ctx, live("POST", "/v1/orders", `{"order":"o-9"}`, nil), db, n, opts); served == nil || miss != nil {
			t.Fatalf("the first call is paired: %+v", miss)
		}
		return ctx, db
	}

	ctx, db := setup()
	served, miss, err := h.learnMatch(ctx, live("POST", "/v1/find", `{"q":{"id":"o-9"}}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil || served.Spec.HTTPResp.Body != `{"id":"o-1","state":"new"}` {
		t.Fatalf("served %v miss %+v err %v, want mock-1 served as recorded", served, miss, err)
	}
	if got := db.consumed[1]; len(got.Spec.Learned()) != 0 || len(got.Spec.LearnedBinds()) != 0 || len(got.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("learned %+v binds %+v noise %v, want nothing at a leaf held echo-only", got.Spec.Learned(), got.Spec.LearnedBinds(), got.Spec.ReqBodyNoise)
	}
	said := logs.FilterMessage("keploy.yml declares these fields noise, so keploy does not compare them, but it cannot follow the value one carries into the answer; the call is served as recorded").All()
	if len(said) != 1 {
		t.Fatalf("%d declared-field refusals logged, want 1: %v", len(said), logs.All())
	}
	if why, _ := said[0].ContextMap()["why"].(string); why != "body.q.id changed, and keploy learns it only as a value the answer carries back, which the answer does not" {
		t.Fatalf("logged %v, want the echo-only refusal", said)
	}

	ctx, db = setup()
	served, miss, err = h.learnMatch(ctx, live("POST", "/v1/echo", `{"ref":"kappa"}`, nil), db, n, opts)
	if err != nil || miss != nil || served == nil || served.Spec.HTTPResp.Body != `{"ref":"kappa"}` {
		t.Fatalf("served %v miss %+v err %v: an echo at a leaf held echo-only holds this run's value", served, miss, err)
	}
}

// Another parser's caller asks of the answer the declared path renders
// (req.written), as of a paired call's: one it cannot write is no answer,
// and the call is served as recorded with nothing learned, the recorded
// answer being one it writes.
func TestLearnMatchADeclaredFieldsRenderedAnswerIsAskedOfTheCaller(t *testing.T) {
	h, _, opts := learnOn(t)
	n := h.newLearnNoise(opts, map[string][]string{"ref": {}}, nil)
	tr := learn.NewTracker()
	tr.Begin()
	db := &learnDb{perTest: []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/v1/echo", `{"ref":"zeta"}`, nil, `{"ref":"zeta"}`),
	}}
	ctx := learn.WithTracker(context.Background(), tr)
	in := live("POST", "/v1/echo", `{"ref":"kappa"}`, nil)
	var asked []string
	in.written = func(a *models.Mock) *models.Mock {
		asked = append(asked, a.Spec.HTTPResp.Body)
		if strings.Contains(a.Spec.HTTPResp.Body, "kappa") {
			return nil
		}
		return a
	}
	served, miss, err := h.learnMatch(ctx, in, db, n, opts)
	if err != nil || miss != nil || served == nil || served.Spec.HTTPResp.Body != `{"ref":"zeta"}` {
		t.Fatalf("served %v miss %+v err %v, want mock-0 served as recorded", served, miss, err)
	}
	if len(asked) < 2 || !reflect.DeepEqual(asked[:2], []string{`{"ref":"zeta"}`, `{"ref":"kappa"}`}) {
		t.Fatalf("the caller was asked of %q, want the recorded answer, then the rendered one", asked)
	}
	if len(db.consumed) != 1 || len(db.consumed[0].Spec.Learned()) != 0 || len(db.consumed[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("took %+v, want mock-0 with nothing learned", db.consumed)
	}
}
