package agent

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// laxSessionMock is stageMock as an HTTP recorder that stated no lifetime
// wrote it, "mocks"-tagged, after DeriveLifetime's lax rule (pkg/models
// lifetime.go, KEPLOY_STRICT_MOCK_WINDOW unset) classified it: a session mock
// served as a cursor that saturates on its last recording. It is set as the
// rule sets it, so the row does not depend on this process's environment.
func laxSessionMock(name, path string, at time.Time) *models.Mock {
	m := stageMock(name, path, at)
	m.TestModeInfo = models.TestModeInfo{
		Lifetime:        models.LifetimeSession,
		Consume:         models.ConsumeCursorSaturate,
		LifetimeDerived: true,
	}
	return m
}

// A recording made before the HTTP recorder stated lifetimes, replayed with
// the default window mode: its "mocks"-tagged HTTP calls are session mocks.
// The pool with no test begun answers GET /q from the reusable tier, the
// first recording of the cursor first. A begin without a pid stages the
// test's mapping; a session mock is no test's own, so the begin leaves it
// where the window staging puts it, in the reusable tier, and the call is
// answered as with no begin. Before, the begin staged mock-b in the per-test
// tier, where no claim of a session mock looks, and the matcher retried the
// claim forever (99% CPU, no answer).
//
// The calls wait for the answer or the parser's return, never a timer: a hang
// ends in go test's own -timeout, which names this row.
func TestAPidlessBeginServesALaxSessionMockAsWithNoBegin(t *testing.T) {
	pool := func() ([]*models.Mock, []*models.Mock, map[string][]string) {
		return []*models.Mock{laxSessionMock("mock-a", "/q", stageT0), laxSessionMock("mock-b", "/q", stageT0.Add(3*time.Second))},
			[]*models.Mock{stageSessionMock("cfg", "/cfg")},
			map[string][]string{"TestA": {"mock-a"}, "TestB": {"mock-b"}}
	}
	served := func(t *testing.T, begin bool) []string {
		per, session, table := pool()
		a, p := newStageReplay(t, per, session, table)
		if begin {
			scopeBegin(t, a, "TestB")
			defer scopeEnd(t, a, "TestB")
		}
		var got []string
		for i := 0; i < 3; i++ {
			got = append(got, answer(t, p.mm, "/q"))
		}
		return got
	}
	want := served(t, false)
	if want[0] != "mock-a" || want[1] != "mock-b" || want[2] != "mock-b" {
		t.Fatalf("no begin: GET /q three times answered %q; want the cursor [mock-a mock-b mock-b]", want)
	}
	got := served(t, true)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pid-less begin of TestB: GET /q three times answered %q; want %q, as with no begin", got, want)
		}
	}
}

// A mock whose lifetime is its connection's (a MySQL statement's lifecycle
// in a recording from before its recorder stated lifetimes, with the
// connection it was recorded on) sits in the per-test region as a lax
// session mock does, and a claim of one looks in the reusable tier as a
// session mock's does. The row files an HTTP recording with that lifetime,
// so the real HTTP claim reads it: a pid-less begin answers GET /c as no
// begin does. The calls wait for the answer, never a timer.
func TestAPidlessBeginServesAConnectionMockAsWithNoBegin(t *testing.T) {
	served := func(t *testing.T, begin bool) []string {
		conn := stageMock("conn-a", "/c", stageT0)
		conn.Spec.Metadata["connID"] = "7"
		conn.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimeConnection, LifetimeDerived: true}
		a, p := newStageReplay(t, []*models.Mock{conn, laxSessionMock("mock-b", "/q", stageT0.Add(3*time.Second))},
			[]*models.Mock{stageSessionMock("cfg", "/cfg")},
			map[string][]string{"TestA": {"conn-a"}, "TestB": {"mock-b"}})
		if begin {
			scopeBegin(t, a, "TestB")
			defer scopeEnd(t, a, "TestB")
		}
		return []string{answer(t, p.mm, "/c"), answer(t, p.mm, "/c")}
	}
	want := served(t, false)
	if want[0] != "conn-a" || want[1] != "conn-a" {
		t.Fatalf("no begin: GET /c twice answered %q; want conn-a both times", want)
	}
	if got := served(t, true); got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pid-less begin of TestB: GET /c twice answered %q; want %q, as with no begin", got, want)
	}
}

// newMappingStagedReplay is a replaying agent under the proven switch that
// its caller stages by mapping, never exactly, as enterprise's sandbox runner
// (pkg/service/runner) does: the set's mocks stored, then the set staged over
// the widest window with an empty mapping and UseMappingBased, and no
// models.ExactServing (the runner's first UpdateMockParams).
func newMappingStagedReplay(t *testing.T, perTest, session []*models.Mock) (*Agent, *stageProxy) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	p := &stageProxy{mm: mm}
	cfg := &config.Config{}
	cfg.Agent.Mode = models.MODE_TEST
	a := New(zap.NewNop(), nil, p, nil, nil, cfg)
	ctx := context.Background()
	if err := a.StoreMocks(ctx, perTest, session); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime: models.BaseTime, BeforeTime: time.Now(), MockMapping: []string{}, UseMappingBased: true, StrictMockWindow: true,
	}); err != nil {
		t.Fatal(err)
	}
	return a, p
}

// Under the proven switch a caller that stages each test by its mapping, not
// exactly (the sandbox runner's per-test UpdateMockParams: UseMappingBased,
// the test's mapping and window, no models.ExactServing), reaches the same
// mapping staging a pid-less begin reaches with the switch off. Over a
// recording made before the HTTP recorder stated lifetimes, the lax rule's
// session mocks are filed in the reusable tier there too, so TestB's GET /q
// is answered as with the set staged and no test's mapping sent. Left in the
// per-test tier under the switch, where no claim of a session mock looks,
// every call missed, though TestB's mapping names mock-b; before the matcher
// left a refused recording out of the call, every call hung at 99% CPU.
//
// The calls wait for the answer or the parser's return, never a timer: a hang
// ends in go test's own -timeout, which names this row.
func TestAMappingStagingUnderTheSwitchServesALaxSessionMockAsTheSetStaged(t *testing.T) {
	served := func(t *testing.T, mapped bool) []string {
		a, p := newMappingStagedReplay(t,
			[]*models.Mock{laxSessionMock("mock-a", "/q", stageT0), laxSessionMock("mock-b", "/q", stageT0.Add(3*time.Second))},
			[]*models.Mock{stageSessionMock("cfg", "/cfg")})
		if mapped {
			if err := a.UpdateMockParams(context.Background(), models.MockFilterParams{
				AfterTime: stageT0.Add(2 * time.Second), BeforeTime: stageT0.Add(4 * time.Second),
				UseMappingBased: true, MockMapping: []string{"mock-b"}, StrictMockWindow: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		for i := 0; i < 3; i++ {
			got = append(got, answer(t, p.mm, "/q"))
		}
		return got
	}
	want := served(t, false)
	got := served(t, true)
	if want[0] != "mock-a" || want[1] != "mock-b" || want[2] != "mock-b" {
		t.Fatalf("set staged: GET /q three times answered %q; want the cursor [mock-a mock-b mock-b]", want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TestB's mapping staged under the switch: GET /q three times answered %q; want %q, as with the set staged", got, want)
		}
	}
}

// The per-test region may hold a nil entry (the filters pass over one): the
// split keeps it where it was and files only the session and connection
// mocks in the reusable tier.
func TestReusableByLifetimeFilesOnlySessionAndConnectionMocks(t *testing.T) {
	session := laxSessionMock("s", "/q", stageT0)
	conn := stageMock("c", "/c", stageT0)
	conn.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimeConnection}
	own := stageMock("own", "/o", stageT0)
	perTest, reusable := reusableByLifetime([]*models.Mock{nil, session, own, conn})
	if len(perTest) != 2 || perTest[0] != nil || perTest[1] != own {
		t.Fatalf("per-test %v; want [nil own]", perTest)
	}
	if len(reusable) != 2 || reusable[0] != session || reusable[1] != conn {
		t.Fatalf("reusable %v; want [s c]", reusable)
	}
}
