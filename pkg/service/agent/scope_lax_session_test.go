package agent

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/postgres"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
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

// A store whose per-test region holds session mocks: the lax rule's
// "mocks"-tagged HTTP calls of a recording made before the HTTP recorder
// stated lifetimes, handed to StoreMocks in the per-test slice. No loader
// keploy ships builds this store (mockdb files session and connection
// lifetimes in the session region, and the lifetime the CLI derived travels
// with each mock to the agent), so the row hands the slice in itself. The
// pool with no test begun answers GET /q from the reusable tier, the first
// recording of the cursor first. A begin without a pid stages the test's
// mapping; a session mock is no test's own, so the begin files it where the
// window staging does, in the reusable tier, the per-test tier holds no
// session mock, and the call is answered as with no begin. Before, the begin
// staged mock-b in the per-test tier, where no claim of a session mock
// looks, and the matcher retried the claim forever (99% CPU, no answer).
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
			per, _ := p.mm.GetPerTestMocksInWindow()
			if names := namesOf(per); hasName(names, "mock-a") || hasName(names, "mock-b") {
				t.Fatalf("the per-test tier holds session mocks after the begin: %v", names)
			}
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

// A mock whose lifetime is its connection's, in the per-test region of a
// store built as the row above builds it (mockdb files one, such as a MySQL
// statement's lifecycle, in the session region): a claim of one looks in the
// reusable tier as a session mock's does. The row files an HTTP recording
// with that lifetime, so the real HTTP claim reads it: a pid-less begin
// answers GET /c as no begin does. The calls wait for the answer, never a
// timer.
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

// Either region may hold a nil entry (the filters pass over one): the split
// keeps it where it was and files only the session and connection mocks in
// the reusable tier, less those the session region holds already by name
// (gob PostgresV2's dual-pool copies), which stay per-test.
func TestReusableByLifetimeFilesOnlySessionAndConnectionMocks(t *testing.T) {
	session := laxSessionMock("s", "/q", stageT0)
	conn := stageMock("c", "/c", stageT0)
	conn.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimeConnection}
	own := stageMock("own", "/o", stageT0)
	dual := laxSessionMock("dual", "/d", stageT0)
	dualSession := laxSessionMock("dual", "/d", stageT0)
	perTest, reusable := reusableByLifetime([]*models.Mock{nil, session, own, conn, dual}, []*models.Mock{nil, dualSession})
	if len(perTest) != 3 || perTest[0] != nil || perTest[1] != own || perTest[2] != dual {
		t.Fatalf("per-test %v; want [nil own dual]", perTest)
	}
	if len(reusable) != 2 || reusable[0] != session || reusable[1] != conn {
		t.Fatalf("reusable %v; want [s c]", reusable)
	}
}

// gobPostgresMock is a PostgresV2 call as the recorder wrote it to a
// mocks.gob, tagged by md.
func gobPostgresMock(query string, md map[string]string, at time.Time) *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(), Kind: models.PostgresV2,
		Spec: models.MockSpec{
			Metadata: md,
			PostgresRequestsV2: []postgres.Request{{PacketBundle: postgres.PacketBundle{
				Packets: []postgres.Packet{{
					Header:  &postgres.PacketInfo{Type: "Query", Header: &postgres.Header{PayloadLength: 9, PacketID: "Q"}},
					Message: map[string]interface{}{"query": query},
				}},
			}}},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
	}
}

// mockdb's gob reader files a PostgresV2 call of session or connection
// lifetime in both regions of the store: under the proven switch, and with
// the switch off for a call recorded with no timestamps (the lax window
// filter drops the per-test copy of the others). Staged by a test's mapping
// (enterprise's sandbox runner under the switch, keploy test with mappings
// with it off), the reusable tier holds each such recording once, the
// session region's copy, and the per-test copy is narrowed by the mapping as
// before the split filed session mocks of the per-test region reusable.
// Filed reusable too, the recording was in the reusable tier twice.
func TestAMappingStagingKeepsAGobPostgresRecordingOnceInTheReusableTier(t *testing.T) {
	for _, proven := range []bool{true, false} {
		name := "switch off, no timestamps"
		if proven {
			name = "switch on"
		}
		t.Run(name, func(t *testing.T) {
			if proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			at := func(d time.Duration) time.Time {
				if !proven {
					return time.Time{}
				}
				return stageT0.Add(d)
			}
			ctx := context.Background()
			ys := mockdb.New(zap.NewNop(), t.TempDir(), "mocks")
			ys.MockFormat = "gob"
			for _, m := range []*models.Mock{
				gobPostgresMock("SELECT 1", map[string]string{"type": "config"}, at(0)),                                // mock-0: session
				gobPostgresMock("PREPARE p", map[string]string{"type": "connection", "connID": "c1"}, at(time.Second)), // mock-1: connection
				gobPostgresMock("SELECT 2", map[string]string{"type": "mocks"}, at(2*time.Second)),                     // mock-2: session (lax rule)
			} {
				if err := ys.InsertMock(ctx, m, "set-0"); err != nil {
					t.Fatal(err)
				}
			}
			if err := ys.Close(); err != nil { // the gob writer is asynchronous
				t.Fatal(err)
			}
			set, err := ys.GetTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), map[string]bool{}, map[string]bool{})
			if err != nil {
				t.Fatal(err)
			}
			if per, session := namesOf(set.Filtered), namesOf(set.Unfiltered); !hasName(per, "mock-0") || !hasName(per, "mock-1") || !hasName(session, "mock-0") || !hasName(session, "mock-1") {
				t.Fatalf("mockdb's gob reader: per-test %v, session %v; want mock-0 and mock-1 in both", per, session)
			}
			mm := proxyPkg.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			p := &stageProxy{mm: mm}
			cfg := &config.Config{}
			cfg.Agent.Mode = models.MODE_TEST
			a := New(zap.NewNop(), nil, p, nil, nil, cfg)
			if err := a.StoreMocks(ctx, set.Filtered, set.Unfiltered); err != nil {
				t.Fatal(err)
			}
			if err := a.UpdateMockParams(ctx, models.MockFilterParams{
				AfterTime: stageT0.Add(-time.Second), BeforeTime: stageT0.Add(3 * time.Second),
				UseMappingBased: true, MockMapping: []string{"mock-0", "mock-1", "mock-2"}, StrictMockWindow: true,
			}); err != nil {
				t.Fatal(err)
			}
			reusable, err := p.mm.GetUnFilteredMocks()
			if err != nil {
				t.Fatal(err)
			}
			count := map[string]int{}
			for _, m := range reusable {
				count[m.Name]++
				if count[m.Name] > 1 {
					t.Fatalf("reusable tier %v; want each recording once", namesOf(reusable))
				}
			}
			if count["mock-0"] != 1 || count["mock-1"] != 1 {
				t.Fatalf("reusable tier %v; want mock-0 and mock-1", namesOf(reusable))
			}
			per, err := p.mm.GetPerTestMocksInWindow()
			if err != nil {
				t.Fatal(err)
			}
			if names := namesOf(per); !hasName(names, "mock-0") || !hasName(names, "mock-1") {
				t.Fatalf("per-test tier %v; want the mapped per-test copies of mock-0 and mock-1, as before", names)
			}
		})
	}
}
