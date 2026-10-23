package http

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

const (
	recordedRole = "e2e-grouprole-1a2b3c4d"
	fRole        = "e2e-grouprole-9f8e7d6c"
)

// roleRecording is a Playwright spec's recording: its beforeAll seeds a role
// whose name the spec's module minted (a pool call of the claim key, made
// while no test is open), and a test lists the roles and assigns it.
func roleRecording() []*models.Mock {
	pool := gql("pool-0", 0, `{"operationName":"createRole","variables":{"name":"`+recordedRole+`"}}`, `{"data":{"createRole":{"ok":true}}}`)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	return []*models.Mock{
		pool,
		gql("mock-1", 2, `{"operationName":"roles","variables":{}}`, `{"data":{"roles":[{"name":"admin"},{"name":"`+recordedRole+`"}]}}`),
		gql("mock-2", 3, `{"operationName":"assignRole","variables":{"role":"`+recordedRole+`","user":"u-1"}}`, `{"data":{"assignRole":true}}`),
	}
}

func roleCalls(name string) []*req {
	return []*req{
		gqlLive(`{"operationName":"createRole","variables":{"name":"` + name + `"}}`),
		gqlLive(`{"operationName":"roles","variables":{}}`),
		gqlLive(`{"operationName":"assignRole","variables":{"role":"` + name + `","user":"u-1"}}`),
	}
}

// roleDb is the F store of roleRecording: the pool call in the pool, the
// test's calls its own.
func roleDb(tr *learn.Tracker) (*learnDb, []*models.Mock) {
	recs := roleRecording()
	cp := func(m *models.Mock) *models.Mock { c := *m; return &c }
	return &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{cp(recs[0])}}, perTest: []*models.Mock{cp(recs[1]), cp(recs[2])}, observe: tr.ServedMock}, recs
}

// Problem B: a name a beforeAll seeds, outside every test, under the claim
// key, in a full record. F pairs the pool call (noise there), and the key's
// test that lists the roles is answered with this run's name; the test that
// sends it again uses it. Both are the key's variable (Outside), bound at
// the pool call. In the replays after F the pool call binds it for every
// test of the key; a new claim (the test process run again, its beforeAll
// with it) binds it anew, and without it a test's use has nothing bound.
func TestLearnMatchAValueABeforeAllMadeIsTheClaimKeysVariable(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.NewKey("e2e/roles.spec.ts")
	ctx := learn.WithTracker(context.Background(), tr)
	db, recs := roleDb(tr)
	calls := roleCalls(fRole)
	if out, miss, err := h.learnMatch(ctx, calls[0], db, n, opts); out == nil || miss != nil || err != nil {
		t.Fatalf("the beforeAll's call: %+v %v", miss, err)
	}
	tr.Begin()
	out, miss, _ := h.learnMatch(ctx, calls[1], db, n, opts)
	if out == nil || miss != nil || out.Spec.HTTPResp.Body != `{"data":{"roles":[{"name":"admin"},{"name":"`+fRole+`"}]}}` {
		t.Fatalf("the list: served %v miss %+v", out, miss)
	}
	if out, miss, _ := h.learnMatch(ctx, calls[2], db, n, opts); out == nil || miss != nil {
		t.Fatalf("the assign: %+v", miss)
	}
	if len(db.pooled) != 1 || !reflect.DeepEqual(db.pooled[0].Spec.ReqBodyNoise, map[string][]string{"body.variables.name": {}}) {
		t.Fatalf("the pool call learned %+v", db.pooled)
	}
	bind := map[string]models.MockVariables{"pool-0": {"name": {Recorded: recordedRole, Bind: "body.variables.name", Outside: true}}}
	list, assign := db.consumed[0], db.consumed[1]
	if want := (models.MockVariables{"name": {Recorded: recordedRole, Answer: []string{"body.data.roles[1].name"}, Outside: true}}); !reflect.DeepEqual(list.Spec.Learned(), want) {
		t.Fatalf("the list learned %+v", list.Spec.Learned())
	}
	if want := (models.MockVariables{"name": {Recorded: recordedRole, Use: []string{"body.variables.role"}, Outside: true}}); !reflect.DeepEqual(assign.Spec.Learned(), want) {
		t.Fatalf("the assign learned %+v", assign.Spec.Learned())
	}
	for _, m := range []models.Mock{list, assign} {
		if !reflect.DeepEqual(m.Spec.LearnedBinds(), bind) {
			t.Fatalf("%s binds %+v, want %+v", m.Name, m.Spec.LearnedBinds(), bind)
		}
	}

	// The replays after F.
	mocks := learnedOnto(recs, append(append([]models.Mock(nil), db.consumed...), *db.pooled[0]))
	if want := (models.MockVariables{"name": {Recorded: recordedRole, Bind: "body.variables.name", Outside: true}}); !reflect.DeepEqual(mocks[0].Spec.Variables, want) || len(mocks[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("the pool call carries %+v and noise %v", mocks[0].Spec.Variables, mocks[0].Spec.ReqBodyNoise)
	}
	replay := func() *varDb {
		var per []*models.Mock
		for _, m := range mocks[1:] {
			c := *m
			per = append(per, &c)
		}
		return &varDb{session: []*models.Mock{mocks[0]}, perTest: per}
	}
	listed := func(name string) string {
		return `{"data":{"roles":[{"name":"admin"},{"name":"` + name + `"}]}}`
	}
	ci := replay()
	const ciRole = "e2e-grouprole-55aa66bb"
	c := roleCalls(ciRole)
	if ok, _, diag := matchVar(t, h, ci, c[0]); !ok {
		t.Fatalf("the beforeAll in CI: %+v", diag)
	}
	ci.table.Reset() // the test begins
	if ok, stub, diag := matchVar(t, h, ci, c[1]); !ok || stub.Spec.HTTPResp.Body != listed(ciRole) {
		t.Fatalf("the list in CI: ok %v diag %+v", ok, diag)
	}
	if ok, _, diag := matchVar(t, h, ci, c[2]); !ok {
		t.Fatalf("the assign in CI: %+v", diag)
	}

	// The test process runs again: a new claim, whose beforeAll mints anew.
	ci = replay()
	if ok, _, _ := matchVar(t, h, ci, roleCalls("e2e-grouprole-00000001")[0]); !ok {
		t.Fatal("the first process's beforeAll")
	}
	ci.table.StartOutside()
	const again = "e2e-grouprole-00000002"
	if ok, _, diag := matchVar(t, h, ci, roleCalls(again)[0]); !ok {
		t.Fatalf("the beforeAll run again binds anew: %+v", diag)
	}
	ci.table.Reset()
	if ok, stub, diag := matchVar(t, h, ci, roleCalls(again)[1]); !ok || stub.Spec.HTTPResp.Body != listed(again) {
		t.Fatalf("the list after the new claim: ok %v diag %+v", ok, diag)
	}

	// A new claim whose beforeAll did not run: nothing is bound.
	ci = replay()
	if ok, _, _ := matchVar(t, h, ci, c[0]); !ok {
		t.Fatal("the beforeAll")
	}
	ci.table.StartOutside()
	ci.table.Reset()
	ok, _, diag := matchVar(t, h, ci, c[1])
	wantProblem(t, ok, diag, "before the call that binds it")
}

// The claim key's scope is learned only under a claim, in a full record, and
// for the claim the value was made in: with no claim (keploy test, record's
// auto-replay), a test's use of a value a pool call sent is refused as
// before; in a partial record the pool call is not paired at all; and after
// a new claim the previous claim's values are no earlier places.
func TestLearnMatchTheClaimKeysScopeIsLearnedOnlyUnderItsClaimInAFullRecord(t *testing.T) {
	h, n, opts := learnOn(t)
	listBody := `{"data":{"roles":[{"name":"admin"},{"name":"` + recordedRole + `"}]}}`
	refused := func(t *testing.T, ctx context.Context, tr *learn.Tracker, db *learnDb, opts models.OutgoingOptions) {
		t.Helper()
		tr.Begin()
		out, miss, _ := h.learnMatch(ctx, roleCalls(fRole)[1], db, n, opts)
		if out == nil || miss != nil || out.Spec.HTTPResp.Body != listBody {
			t.Fatalf("the list is served as recorded: %v %+v", out, miss)
		}
		if got := db.consumed[0].Spec.Learned(); len(got) != 0 {
			t.Fatalf("learned %+v", got)
		}
		out, miss, _ = h.learnMatch(ctx, roleCalls(fRole)[2], db, n, opts)
		if out != nil || miss == nil || miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, "pool-0") {
			t.Fatalf("the assign: served %v miss %+v", out, miss)
		}
	}

	t.Run("no claim", func(t *testing.T) {
		o := opts
		o.LearnPool = true
		tr := learn.NewTracker()
		ctx := learn.WithTracker(context.Background(), tr)
		db, _ := roleDb(tr)
		if out, miss, _ := h.learnMatch(ctx, roleCalls(fRole)[0], db, n, o); out == nil || miss != nil {
			t.Fatalf("the pool call is paired in a full record: %+v", miss)
		}
		refused(t, ctx, tr, db, o)
	})

	t.Run("a partial record", func(t *testing.T) {
		tr := learn.NewTracker()
		tr.NewKey("e2e/roles.spec.ts")
		ctx := learn.WithTracker(context.Background(), tr)
		db, _ := roleDb(tr)
		if out, miss, _ := h.learnMatch(ctx, roleCalls(fRole)[0], db, n, opts); out != nil || miss == nil || miss.res.Reason != models.MissRequestChanged {
			t.Fatalf("a partial record never pairs the pool's: served %v miss %+v", out, miss)
		}
	})

	t.Run("a new claim", func(t *testing.T) {
		o := opts
		o.LearnPool = true
		tr := learn.NewTracker()
		tr.NewKey("e2e/roles.spec.ts")
		ctx := learn.WithTracker(context.Background(), tr)
		db, _ := roleDb(tr)
		if out, miss, _ := h.learnMatch(ctx, roleCalls(fRole)[0], db, n, o); out == nil || miss != nil {
			t.Fatalf("the beforeAll's call: %+v", miss)
		}
		tr.NewKey("e2e/roles.spec.ts")
		refused(t, ctx, tr, db, o)
	})
}
