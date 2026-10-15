package mockdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The prune must keep, with every mock it keeps, the mocks that mock was
// recorded against (keploy #4810): a MySQL COM_STMT_EXECUTE names its
// statement by an ID only its COM_STMT_PREPARE resolves.

const depsQuery = "INSERT INTO orders (idmp_key, created_at) VALUES (?, ?)"

func stmtTestMock(i int, connID string, req mysql.PacketBundle, resp *mysql.PacketBundle) *models.Mock {
	ts := pruneT0.Add(time.Duration(i) * time.Second)
	m := &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks", "connID": connID},
			MySQLRequests:    []mysql.Request{{PacketBundle: req}},
			Created:          ts.Unix(),
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(time.Millisecond),
		},
	}
	if resp != nil {
		m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: *resp}}
	}
	return m
}

func prepareTestMock(i int, connID string, stmtID uint32) *models.Mock {
	m := stmtTestMock(i, connID,
		mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(depsQuery) + 1)}, Type: mysql.CommandStatusToString(mysql.COM_STMT_PREPARE)},
			Message: &mysql.StmtPreparePacket{Command: mysql.COM_STMT_PREPARE, Query: depsQuery},
		},
		&mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 1}, Type: mysql.COM_STMT_PREPARE_OK},
			Message: &mysql.StmtPrepareOkPacket{StatementID: stmtID, NumParams: 2},
		})
	m.Spec.Metadata["type"] = "connection"
	return m
}

// executeTestMock is a COM_STMT_EXECUTE of stmtID; stored, when set, is the
// query the recorder kept on it.
func executeTestMock(i int, connID string, stmtID uint32, stored string) *models.Mock {
	req := mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 20}, Type: mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE)},
		Message: &mysql.StmtExecutePacket{Status: mysql.COM_STMT_EXECUTE, StatementID: stmtID, IterationCount: 1, ParameterCount: 2},
	}
	req.SetStoredQuery(stored)
	return stmtTestMock(i, connID, req, &mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 7, SequenceID: 1}, Type: mysql.StatusToString(mysql.OK)},
		Message: &mysql.OKPacket{Header: mysql.OK, AffectedRows: 1, StatusFlags: 2},
	})
}

func closeTestMock(i int, connID string, stmtID uint32) *models.Mock {
	return stmtTestMock(i, connID, mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 5}, Type: mysql.CommandStatusToString(mysql.COM_STMT_CLOSE)},
		Message: &mysql.StmtClosePacket{Status: mysql.COM_STMT_CLOSE, StatementID: stmtID},
	}, nil)
}

// readSet reads every mock of the fixture's set back, by name.
func readDepsSet(t *testing.T, dir string) map[string]*models.Mock {
	t.Helper()
	ys := New(zap.NewNop(), dir, "mocks")
	set, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*models.Mock{}
	for _, pool := range [][]*models.Mock{set.Filtered, set.Unfiltered, set.AllPerTest, set.AllSession} {
		for _, m := range pool {
			out[m.Name] = m
		}
	}
	return out
}

func depsNamesOf(set map[string]*models.Mock) []string {
	var names []string
	for i := 0; i < 64; i++ {
		if _, ok := set[fmt.Sprintf("mock-%d", i)]; ok {
			names = append(names, fmt.Sprintf("mock-%d", i))
		}
	}
	return names
}

func depsConsumed(names ...string) map[string]models.MockState {
	out := map[string]models.MockState{}
	for _, n := range names {
		out[n] = models.MockState{Name: n}
	}
	return out
}

// TestUpdateMocks_KeepsThePrepareEveryKeptMockNames replays the recording of
// keploy #4810: the same INSERT prepared again for every order on one
// connection, each live PREPARE answered by the first recorded one, so only
// that one is consumed, while every EXECUTE is.
func TestUpdateMocks_KeepsThePrepareEveryKeptMockNames(t *testing.T) {
	fixture := []*models.Mock{
		prepareTestMock(0, "0", 1), // mock-0: consumed (every live PREPARE matched it)
		executeTestMock(1, "0", 1, ""),
		closeTestMock(2, "0", 1),
		prepareTestMock(3, "0", 2), // mock-3, 5: unconsumed, the PREPARE and CLOSE of mock-4
		executeTestMock(4, "0", 2, ""),
		closeTestMock(5, "0", 2),
		prepareTestMock(6, "0", 3), // mock-6, 8: the same of mock-7, which kept its query
		executeTestMock(7, "0", 3, depsQuery),
		closeTestMock(8, "0", 3),
		prepareTestMock(9, "0", 4), // mock-9..11: a statement no test ran: all go
		executeTestMock(10, "0", 4, ""),
		closeTestMock(11, "0", 4),
		prepareTestMock(12, "1", 2), // mock-12, 13: statement 2 of ANOTHER connection: not needed
		closeTestMock(13, "1", 2),
		closeTestMock(14, "0", 5), // mock-14: a consumed CLOSE keeps its PREPARE only: none here
	}
	keep := depsConsumed("mock-0", "mock-1", "mock-2", "mock-4", "mock-7", "mock-14")
	want := []string{"mock-0", "mock-1", "mock-2", "mock-3", "mock-4", "mock-5", "mock-6", "mock-7", "mock-8", "mock-14"}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			writePruneFixture(t, dir, format, fixture, "")
			core, logs := observer.New(zapcore.DebugLevel)
			ys := New(zap.New(core), dir, "mocks")
			if err := ys.UpdateMocks(context.Background(), pruneSet, keep, pruneT0.Add(time.Hour), time.Time{}); err != nil {
				t.Fatal(err)
			}
			got := readDepsSet(t, dir)
			if names := depsNamesOf(got); !reflect.DeepEqual(names, want) {
				t.Fatalf("the prune kept %v, want %v: every kept EXECUTE keeps the PREPARE and the CLOSE of its statement, and nothing more", names, want)
			}
			// The log names what was kept only as a dependency, so a reader
			// can tell a kept PREPARE from a kept CLOSE.
			done := logs.FilterMessageSnippet("pruned mocks successfully").All()
			if len(done) != 1 {
				t.Fatalf("%d prune summaries logged, want 1", len(done))
			}
			if deps := done[0].ContextMap()["keptAsDependencyMocks"]; !reflect.DeepEqual(deps, []interface{}{"mock-3", "mock-5", "mock-6", "mock-8"}) {
				t.Errorf("the prune log names %v as kept as a dependency, want mock-3, mock-5, mock-6, mock-8", deps)
			}
			var check models.MockDepsCheck
			for _, m := range got {
				check.Add(m)
			}
			if unmet := check.Unmet(); len(unmet) != 0 {
				t.Errorf("the pruned set names statements it does not hold: %+v", unmet)
			}
			if q := got["mock-7"].Spec.MySQLRequests[0].StoredQuery(); q != depsQuery {
				t.Errorf("the rewrite lost the query the EXECUTE kept: %q", q)
			}
		})
	}
}

// depsByMetadata declares, for the HTTP mocks of a test, links read from
// their metadata: "provides" and "requires", comma-separated keys.
func depsByMetadata(m *models.Mock) models.MockDeps {
	var d models.MockDeps
	if p := m.Spec.Metadata["provides"]; p != "" {
		d.Provides = strings.Split(p, ",")
	}
	if r := m.Spec.Metadata["requires"]; r != "" {
		for _, k := range strings.Split(r, ",") {
			d.Requires = append(d.Requires, models.MockRequirement{Key: k})
		}
	}
	return d
}

func linkedHTTPMock(i int, provides, requires string) *models.Mock {
	m := pruneHTTPMock(i)
	m.Spec.Metadata = map[string]string{"provides": provides, "requires": requires}
	return m
}

// TestUpdateMocks_KeepsWhatAKeptMockNeedsTransitively pins the closure the
// prune takes over the declared links: a kept mock keeps every provider of
// each key it requires, and each mock kept that way keeps what it requires in
// turn, whatever the order of the file.
func TestUpdateMocks_KeepsWhatAKeptMockNeedsTransitively(t *testing.T) {
	models.RegisterMockDeps(models.HTTP, depsByMetadata)
	t.Cleanup(func() { models.RegisterMockDeps(models.HTTP, nil) })
	fixture := []*models.Mock{
		linkedHTTPMock(0, "lease", ""),           // mock-0: provides lease
		linkedHTTPMock(1, "session", "lease"),    // mock-1: provides session, requires lease
		linkedHTTPMock(2, "", "session"),         // mock-2: consumed, requires session
		linkedHTTPMock(3, "session", ""),         // mock-3: a second provider of session
		linkedHTTPMock(4, "other", ""),           // mock-4: provides what nothing kept needs
		linkedHTTPMock(5, "", "missing"),         // mock-5: consumed, requires what nothing provides
		linkedHTTPMock(6, "lease-2", "lease"),    // mock-6: provides what nothing kept needs
		linkedHTTPMock(7, "", ""),                // mock-7: plain, unconsumed
		linkedHTTPMock(8, "", "late,session"),    // mock-8: consumed, requires late
		linkedHTTPMock(9, "late", ""),            // mock-9: provides late, after its requirer
		linkedHTTPMock(10, "cycle-a", "cycle-b"), // mock-10, 11: a cycle nothing kept needs
		linkedHTTPMock(11, "cycle-b", "cycle-a"),
	}
	keep := depsConsumed("mock-2", "mock-5", "mock-8")
	want := []string{"mock-0", "mock-1", "mock-2", "mock-3", "mock-5", "mock-8", "mock-9"}
	for _, format := range []string{"yaml", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			writePruneFixture(t, dir, format, fixture, "")
			ys := New(zap.NewNop(), dir, "mocks")
			if err := ys.UpdateMocks(context.Background(), pruneSet, keep, pruneT0.Add(time.Hour), time.Time{}); err != nil {
				t.Fatal(err)
			}
			if names := depsNamesOf(readDepsSet(t, dir)); !reflect.DeepEqual(names, want) {
				t.Fatalf("the prune kept %v, want %v", names, want)
			}
		})
	}
}

// TestPruneVerdict_RefusesAFileThatChangedBetweenPasses: the second pass
// keeps mocks by their place in the file, which only the first pass's file
// gives meaning.
func TestPruneVerdict_RefusesAFileThatChangedBetweenPasses(t *testing.T) {
	models.RegisterMockDeps(models.HTTP, depsByMetadata)
	t.Cleanup(func() { models.RegisterMockDeps(models.HTTP, nil) })
	named := func(m *models.Mock, name string) *models.Mock { m.Name = name; return m }
	keeps := func(m *models.Mock) bool { return m.Name == "mock-0" }
	firstPass := func() *pruneVerdict {
		v := &pruneVerdict{keeps: keeps}
		for _, m := range []*models.Mock{named(linkedHTTPMock(0, "", "k"), "mock-0"), named(linkedHTTPMock(1, "k", ""), "mock-1")} {
			if _, err := v.judge(m); err != nil {
				t.Fatal(err)
			}
		}
		if !v.secondPass() {
			t.Fatal("the first pass dropped mock-1, which mock-0 requires, and asks for no second pass")
		}
		return v
	}

	v := firstPass()
	if _, err := v.judge(named(linkedHTTPMock(0, "", "k"), "mock-0")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.judge(named(linkedHTTPMock(1, "k", ""), "mock-9")); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("a second pass that found another mock in mock-1's place kept it: %v", err)
	}

	v = firstPass()
	if _, err := v.judge(named(linkedHTTPMock(0, "", "k"), "mock-0")); err != nil {
		t.Fatal(err)
	}
	if err := v.checkPlaces(); err == nil {
		t.Error("a second pass that read fewer mocks than the first was not refused")
	}
	if v.secondPass() {
		t.Error("a second pass asks for a third")
	}
}

// TestReadMockPools_WarnsOfMocksThatLostWhatTheyNeed: a set an older keploy
// pruned cannot be repaired from its mocks, so its read says which mocks lost
// what, once; a mock that carries what it lost, and a whole set, say nothing.
func TestReadMockPools_WarnsOfMocksThatLostWhatTheyNeed(t *testing.T) {
	pruned := []*models.Mock{
		prepareTestMock(0, "0", 1),
		executeTestMock(1, "0", 1, ""),
		executeTestMock(2, "0", 2, ""),        // mock-2: its PREPARE was pruned
		executeTestMock(3, "0", 3, depsQuery), // mock-3: so was this one's, but it kept its query
		closeTestMock(4, "0", 2),              // mock-4: a CLOSE replays without its PREPARE
	}
	whole := []*models.Mock{prepareTestMock(0, "0", 1), executeTestMock(1, "0", 1, ""), closeTestMock(2, "0", 1)}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				mocks []*models.Mock
				want  int
			}{{"pruned", pruned, 1}, {"whole", whole, 0}} {
				dir := t.TempDir()
				writePruneFixture(t, dir, format, tc.mocks, "")
				core, logs := observer.New(zapcore.WarnLevel)
				ys := New(zap.New(core), dir, "mocks")
				if _, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Now(), nil, nil); err != nil {
					t.Fatal(err)
				}
				warned := logs.FilterMessageSnippet("does not hold").All()
				if len(warned) != tc.want {
					t.Fatalf("%s: %d warnings of mocks that lost what they need, want %d: %v", tc.name, len(warned), tc.want, logs.All())
				}
				if tc.want == 0 {
					continue
				}
				// A second read of the same file says nothing new.
				if _, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Now(), nil, nil); err != nil {
					t.Fatal(err)
				}
				if n := logs.FilterMessageSnippet("does not hold").Len(); n != 1 {
					t.Errorf("%d warnings after a second read of the unchanged file, want 1", n)
				}
				// A changed file (pruned again, or restored from a copy) is
				// another version, and its read warns again.
				if err := os.RemoveAll(filepath.Join(dir, pruneSet)); err != nil {
					t.Fatal(err)
				}
				writePruneFixture(t, dir, format, append(append([]*models.Mock{}, tc.mocks...), executeTestMock(5, "0", 5, "")), "")
				if _, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Now(), nil, nil); err != nil {
					t.Fatal(err)
				}
				if n := logs.FilterMessageSnippet("does not hold").Len(); n != 2 {
					t.Errorf("%d warnings after a read of the changed file, want 2", n)
				}
				fields := warned[0].ContextMap()
				if !reflect.DeepEqual(fields["mocks"], []interface{}{"mock-2"}) || fields["count"] != int64(1) {
					t.Errorf("the warning names %v (count %v), want only mock-2", fields["mocks"], fields["count"])
				}
				if !strings.Contains(warned[0].Message, models.MySQLStatementKey("0", 2)) {
					t.Errorf("the warning does not say what mock-2 needs: %s", warned[0].Message)
				}
			}
		})
	}
}

// TestStoredQuerySurvivesEveryMockFormat: the query a COM_STMT_EXECUTE kept is
// written and read back in every format, and by a rewrite.
func TestStoredQuerySurvivesEveryMockFormat(t *testing.T) {
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			path := writePruneFixture(t, dir, format, []*models.Mock{executeTestMock(0, "0", 9, depsQuery)}, "")
			if q := readDepsSet(t, dir)["mock-0"].Spec.MySQLRequests[0].StoredQuery(); q != depsQuery {
				t.Fatalf("read back %q from %s, want %q", q, filepath.Base(path), depsQuery)
			}
			f := yaml.FormatYAML // a mocks.gob is found by its presence
			if format == "json" {
				f = yaml.FormatJSON
			}
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			if err := ys.PersistMockNoise(context.Background(), pruneSet, map[string]models.MockState{
				"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.parameters.1.value": {}}},
			}); err != nil {
				t.Fatal(err)
			}
			if q := readDepsSet(t, dir)["mock-0"].Spec.MySQLRequests[0].StoredQuery(); q != depsQuery {
				t.Fatalf("the noise write-back lost the stored query: %q", q)
			}
		})
	}
}
