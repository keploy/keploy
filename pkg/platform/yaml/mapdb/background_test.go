package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// MarkBackground writes a judging's background mark on the named test's
// entry only, keeps every other entry and section, takes a mark off with an
// empty line, and writes nothing when no entry changes.
func TestMarkBackgroundMarksOnlyTheNamedTestsEntry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1", Kind: "Http"}, {Name: "mock-2", Kind: "Mongo"}},
		"test-2": {{Name: "mock-2", Kind: "Mongo"}, {Name: "mock-3", Kind: "Http"}},
	}); err != nil {
		t.Fatal(err)
	}
	worker := []models.MockEntry{{Name: "w-1", Kind: "Http"}}
	if err := db.UpsertWorker(ctx, "test-set-0", worker); err != nil {
		t.Fatal(err)
	}
	entries := func() map[string]map[string]string {
		t.Helper()
		m, err := db.GetMapping(ctx, "test-set-0")
		if err != nil || m == nil {
			t.Fatalf("GetMapping = %v, %v", m, err)
		}
		if !reflect.DeepEqual(m.Worker, worker) {
			t.Fatalf("worker scope %+v, want %+v kept", m.Worker, worker)
		}
		got := map[string]map[string]string{}
		for _, tc := range m.TestCases {
			got[tc.ID] = map[string]string{}
			for _, e := range tc.Mocks {
				got[tc.ID][e.Name] = e.Kind + "|" + e.Background
			}
		}
		return got
	}

	changed, err := db.MarkBackground(ctx, "test-set-0", map[string]map[string]string{
		"test-1": {"mock-2": "count_varied: why", "mock-9": "no such recording"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed, map[string][]string{"test-1": {"mock-2"}}) {
		t.Fatalf("changed %v, want test-1's mock-2 only (test-2 lists it too, unmarked)", changed)
	}
	want := map[string]map[string]string{
		"test-1": {"mock-1": "Http|", "mock-2": "Mongo|count_varied: why"},
		"test-2": {"mock-2": "Mongo|", "mock-3": "Http|"},
	}
	if got := entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after marking: %v, want %v", got, want)
	}

	// The same marks again change nothing, and the file is not written.
	path := filepath.Join(dir, "test-set-0", "mappings.yaml")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err = db.MarkBackground(ctx, "test-set-0", map[string]map[string]string{"test-1": {"mock-2": "count_varied: why"}})
	if err != nil || len(changed) != 0 {
		t.Fatalf("marking again: changed %v, err %v; want nothing changed", changed, err)
	}
	// A write replaces the file (a rename over it): the same file is unwritten.
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("marking again rewrote the file (err %v)", err)
	}

	// An empty line takes the mark off.
	changed, err = db.MarkBackground(ctx, "test-set-0", map[string]map[string]string{"test-1": {"mock-2": ""}})
	if err != nil || !reflect.DeepEqual(changed, map[string][]string{"test-1": {"mock-2"}}) {
		t.Fatalf("taking the mark off: changed %v, err %v; want test-1's mock-2", changed, err)
	}
	want["test-1"]["mock-2"] = "Mongo|"
	if got := entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after taking the mark off: %v, want %v", got, want)
	}

	// A set with no mappings file has nothing to mark.
	if _, err := db.MarkBackground(ctx, "test-set-missing", map[string]map[string]string{"test-1": {"mock-1": "x"}}); err == nil {
		t.Fatal("marking a set with no mappings file: no error")
	}
}

// RemoveBackground takes a recording off the named test's list only while
// its entry there still carries a background mark, keeps every other entry
// and section, writes nothing when nothing is taken off, and names as
// unlisted only what no list names any more.
func TestRemoveBackgroundTakesOnlyAMarkedEntryOffItsTestsList(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1", Kind: "Http"}, {Name: "tick", Kind: "Mongo"}, {Name: "shared", Kind: "Mongo"}, {Name: "sweep", Kind: "Mongo"}, {Name: "w-1", Kind: "Http"}},
		"test-2": {{Name: "shared", Kind: "Mongo"}, {Name: "mock-3", Kind: "Http"}},
	}); err != nil {
		t.Fatal(err)
	}
	worker := []models.MockEntry{{Name: "w-1", Kind: "Http"}}
	if err := db.UpsertWorker(ctx, "test-set-0", worker); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MarkBackground(ctx, "test-set-0", map[string]map[string]string{
		"test-1": {"tick": "a timer", "shared": "a timer", "w-1": "a timer"},
	}); err != nil {
		t.Fatal(err)
	}
	lists := func() map[string][]string {
		t.Helper()
		m, err := db.GetMapping(ctx, "test-set-0")
		if err != nil || m == nil {
			t.Fatalf("GetMapping = %v, %v", m, err)
		}
		if !reflect.DeepEqual(m.Worker, worker) {
			t.Fatalf("worker scope %+v, want %+v kept", m.Worker, worker)
		}
		got := map[string][]string{}
		for _, tc := range m.TestCases {
			for _, e := range tc.Mocks {
				got[tc.ID] = append(got[tc.ID], e.Name+"|"+e.Background)
			}
		}
		return got
	}

	// sweep carries no mark: it stays. mock-3 is test-2's, not test-1's.
	removed, unlisted, err := db.RemoveBackground(ctx, "test-set-0", map[string]map[string]string{
		"test-1": {"tick": "no round called it", "shared": "no round called it", "sweep": "no round called it", "w-1": "no round called it", "mock-3": "no round called it"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, map[string][]string{"test-1": {"shared", "tick", "w-1"}}) || !reflect.DeepEqual(unlisted, []string{"tick"}) {
		t.Fatalf("removed %v, unlisted %v; want test-1's [shared tick w-1], [tick] (test-2 lists shared, the worker scope w-1)", removed, unlisted)
	}
	want := map[string][]string{
		"test-1": {"mock-1|", "sweep|"},
		"test-2": {"shared|", "mock-3|"},
	}
	if got := lists(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after removing: %v, want %v", got, want)
	}
	// Each one taken off is noted on its test's entry, with the mark it
	// carried and the reason; nothing is noted on test-2, whose entries stay.
	notes := func() map[string][]models.RemovedMock {
		t.Helper()
		m, err := db.GetMapping(ctx, "test-set-0")
		if err != nil || m == nil {
			t.Fatalf("GetMapping = %v, %v", m, err)
		}
		got := map[string][]models.RemovedMock{}
		for _, tc := range m.TestCases {
			if len(tc.Removed) > 0 {
				got[tc.ID] = tc.Removed
			}
		}
		return got
	}
	note := func(name, kind string) models.RemovedMock {
		return models.RemovedMock{Name: name, Kind: kind, Background: "a timer", Reason: "no round called it"}
	}
	wantNotes := map[string][]models.RemovedMock{"test-1": {note("tick", "Mongo"), note("shared", "Mongo"), note("w-1", "Http")}}
	if got := notes(); !reflect.DeepEqual(got, wantNotes) {
		t.Fatalf("removal notes %+v, want %+v", got, wantNotes)
	}

	// Nothing marked to take off: the file is not written.
	path := filepath.Join(dir, "test-set-0", "mappings.yaml")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	removed, unlisted, err = db.RemoveBackground(ctx, "test-set-0", map[string]map[string]string{"test-1": {"sweep": "again", "tick": "again"}})
	if err != nil || removed != nil || unlisted != nil {
		t.Fatalf("removing again: %v, %v, %v; want nothing", removed, unlisted, err)
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("removing nothing rewrote the file (err %v)", err)
	}
	if got := notes(); !reflect.DeepEqual(got, wantNotes) {
		t.Fatalf("removing nothing changed the notes: %+v, want %+v", got, wantNotes)
	}

	// A record that lists tick on test-1 again brought it back: its note
	// comes off, and the others stay.
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{"test-1": {{Name: "tick", Kind: "Mongo"}}}); err != nil {
		t.Fatal(err)
	}
	wantNotes = map[string][]models.RemovedMock{"test-1": {note("shared", "Mongo"), note("w-1", "Http")}}
	if got := notes(); !reflect.DeepEqual(got, wantNotes) {
		t.Fatalf("after listing tick again: notes %+v, want %+v", got, wantNotes)
	}

	if _, _, err := db.RemoveBackground(ctx, "test-set-missing", map[string]map[string]string{"test-1": {"tick": "gone"}}); err == nil {
		t.Fatal("removing from a set with no mappings file: no error")
	}
}

// Two tests both mark one recording, and judging names it for only one of
// them (the other test called it in the confirming round, so its mark
// stands): the recording comes off the named test's list only. The other
// test keeps its marked entry, so its next replay still serves its own call,
// and the mock file keeps the recording.
func TestRemoveBackgroundTakesASharedMarkOffOnlyTheNamedTest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1", Kind: "Http"}, {Name: "shared", Kind: "Mongo"}},
		"test-2": {{Name: "shared", Kind: "Mongo"}, {Name: "mock-2", Kind: "Http"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MarkBackground(ctx, "test-set-0", map[string]map[string]string{
		"test-1": {"shared": "a timer"},
		"test-2": {"shared": "a timer"},
	}); err != nil {
		t.Fatal(err)
	}
	removed, unlisted, err := db.RemoveBackground(ctx, "test-set-0", map[string]map[string]string{
		"test-1": {"shared": "no round called it"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, map[string][]string{"test-1": {"shared"}}) || unlisted != nil {
		t.Fatalf("removed %v, unlisted %v; want only test-1's [shared], and nothing unlisted (test-2 still lists it)", removed, unlisted)
	}
	m, err := db.GetMapping(ctx, "test-set-0")
	if err != nil || m == nil {
		t.Fatalf("GetMapping = %v, %v", m, err)
	}
	got := map[string][]string{}
	for _, tc := range m.TestCases {
		for _, e := range tc.Mocks {
			got[tc.ID] = append(got[tc.ID], e.Name+"|"+e.Background)
		}
		if tc.ID == "test-2" && len(tc.Removed) > 0 {
			t.Fatalf("test-2 was not named, yet carries removal notes %+v", tc.Removed)
		}
	}
	want := map[string][]string{
		"test-1": {"mock-1|"},
		"test-2": {"shared|a timer", "mock-2|"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after removing: %v, want %v", got, want)
	}
}

// A marked recording taken off its test's list stays in the mock file while
// any other list of mappings.yaml names it: the startup, runner, worker or
// shared list, or a boot's (keploy mock record writes boots). Without that a
// recording the application needs to boot would be deleted.
func TestRemoveBackgroundKeepsARecordingAnyListStillNames(t *testing.T) {
	entry := []models.MockEntry{{Name: "tick", Kind: "Mongo"}}
	for name, set := range map[string]func(m *models.Mapping){
		"startup": func(m *models.Mapping) { m.Startup = entry },
		"runner":  func(m *models.Mapping) { m.Runner = entry },
		"worker":  func(m *models.Mapping) { m.Worker = entry },
		"shared":  func(m *models.Mapping) { m.Shared = entry },
		"boots":   func(m *models.Mapping) { m.Boots = []models.BootSpec{{Key: "app", Mocks: entry}} },
		"none":    func(*models.Mapping) {},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			m := &models.Mapping{Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
				TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "tick", Kind: "Mongo", Background: "a timer"}}}}}
			set(m)
			data, err := EncodeMappingF(m, zap.NewNop(), "")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "test-set-0"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "test-set-0", "mappings.yaml"), data, 0o644); err != nil {
				t.Fatal(err)
			}
			removed, unlisted, err := New(zap.NewNop(), dir, "mappings").RemoveBackground(ctx, "test-set-0", map[string]map[string]string{"test-1": {"tick": "gone"}})
			if err != nil || !reflect.DeepEqual(removed, map[string][]string{"test-1": {"tick"}}) {
				t.Fatalf("removed %v, err %v; want test-1's tick", removed, err)
			}
			if want := name == "none"; (len(unlisted) == 1) != want {
				t.Fatalf("unlisted %v: the %s list still names tick, so the mock file must keep it", unlisted, name)
			}
		})
	}
}

// A removal note (models.MappedTestCase.Removed) is read and kept by every
// rewrite of mappings.yaml, whatever the switch, in YAML and in JSON; a note
// taken out again for the same recording replaces the first.
func TestRemovalNoteIsKeptByEveryRewrite(t *testing.T) {
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		for _, proven := range []string{"1", ""} {
			t.Run(string(f)+"/proven="+proven, func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				ctx := context.Background()
				dir := t.TempDir()
				m := &models.Mapping{Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
					TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "tick", Kind: "Mongo", Background: "a timer"}, {Name: "mock-1", Kind: "Http"}}}}}
				data, err := EncodeMappingF(m, zap.NewNop(), f)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(dir, "test-set-0"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "test-set-0", "mappings."+f.FileExtension()), data, 0o644); err != nil {
					t.Fatal(err)
				}
				db := NewWithFormat(zap.NewNop(), dir, "mappings", f)
				if _, _, err := db.RemoveBackground(ctx, "test-set-0", map[string]map[string]string{"test-1": {"tick": "first"}}); err != nil {
					t.Fatal(err)
				}
				// Another rewrite, of another section, keeps the note.
				if err := db.UpsertWorker(ctx, "test-set-0", []models.MockEntry{{Name: "w-1", Kind: "Http"}}); err != nil {
					t.Fatal(err)
				}
				if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{"test-1": {{Name: "mock-2", Kind: "Http"}}}); err != nil {
					t.Fatal(err)
				}
				got, err := db.GetMapping(ctx, "test-set-0")
				if err != nil {
					t.Fatal(err)
				}
				want := []models.RemovedMock{{Name: "tick", Kind: "Mongo", Background: "a timer", Reason: "first"}}
				if len(got.TestCases) != 1 || !reflect.DeepEqual(got.TestCases[0].Removed, want) {
					t.Fatalf("after two rewrites: %+v, want test-1's note %+v", got.TestCases, want)
				}
				// Listed and marked again, then taken out again: one note, the
				// second reason.
				got.TestCases[0].Mocks = append(got.TestCases[0].Mocks, models.MockEntry{Name: "tick", Kind: "Mongo", Background: "a timer"})
				got.TestCases[0].Removed = want
				data, err = EncodeMappingF(got, zap.NewNop(), f)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "test-set-0", "mappings."+f.FileExtension()), data, 0o644); err != nil {
					t.Fatal(err)
				}
				if _, _, err := db.RemoveBackground(ctx, "test-set-0", map[string]map[string]string{"test-1": {"tick": "second"}}); err != nil {
					t.Fatal(err)
				}
				got, err = db.GetMapping(ctx, "test-set-0")
				if err != nil {
					t.Fatal(err)
				}
				want[0].Reason = "second"
				if !reflect.DeepEqual(got.TestCases[0].Removed, want) {
					t.Fatalf("taken out again: %+v, want %+v", got.TestCases[0].Removed, want)
				}
			})
		}
	}
}

// A removal note that is not a list of removed recordings (a hand edit gone
// wrong) is an error, never dropped by the next rewrite unsaid.
func TestRemovalNoteThatIsNotAListIsAnError(t *testing.T) {
	for _, tc := range []struct {
		format yaml.Format
		body   string
	}{
		{yaml.FormatYAML, "version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: test-set-0\ntests:\n  - id: test-1\n    mock_entries:\n      - name: mock-1\n        kind: Http\n    removed: tick\n"},
		{yaml.FormatJSON, `{"version":"api.keploy.io/v1beta1","kind":"TestMocksMapping","testSetId":"test-set-0","tests":[{"id":"test-1","mock_entries":[{"name":"mock-1","kind":"Http"}],"removed":"tick"}]}`},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			if _, err := DecodeMappingF([]byte(tc.body), zap.NewNop(), tc.format); err == nil {
				t.Fatal("a removed: that is not a list read without an error")
			}
		})
	}
}
