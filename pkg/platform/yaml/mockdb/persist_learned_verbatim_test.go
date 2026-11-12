package mockdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// learnedFileEdit is one storage format's mock file, with a hand edit of one
// mock's document (mock): from, which the file holds once, becomes to.
type learnedFileEdit struct {
	format   yaml.Format
	mock     string
	from, to string
}

// learnedFileEdits is each storage format's hand edit of mock (a note inside
// its YAML document; its NDJSON line spaced as a hand writes it).
func learnedFileEdits(mock string) []learnedFileEdit {
	return []learnedFileEdit{
		{yaml.FormatYAML, mock, "name: " + mock + "\n", "name: " + mock + "\n# edited by hand\n"},
		{yaml.FormatJSON, mock, `"name":"` + mock + `"`, `"name": "` + mock + `"`},
	}
}

// learnedFileDocs is each document of a mock file by the mock it names
// (mock-0, mock-1), as its bytes.
func learnedFileDocs(t *testing.T, f yaml.Format, data string) map[string]string {
	t.Helper()
	sep := "\n---\n"
	if f == yaml.FormatJSON {
		sep = "\n"
	}
	out := map[string]string{}
	for _, doc := range strings.Split(strings.TrimSuffix(data, "\n"), sep) {
		for _, name := range []string{"mock-0", "mock-1"} {
			if strings.Contains(doc, `"`+name+`"`) || strings.Contains(doc, "name: "+name+"\n") {
				out[name] = doc
			}
		}
	}
	if len(out) != 2 {
		t.Fatalf("the mock file's documents by mock: %q", out)
	}
	return out
}

// learnedFileWithEdit writes mock-0 and mock-1 to set in format f, edits
// one mock's document by hand (e), and returns the file's path, its bytes
// before the edit and after it.
func learnedFileWithEdit(t *testing.T, ys *MockYaml, set string, e learnedFileEdit) (path, before, edited string) {
	t.Helper()
	ctx := context.Background()
	if err := ys.InsertMock(ctx, noiseTestMock(`{"key":"k-1"}`), set); err != nil {
		t.Fatal(err)
	}
	if err := ys.InsertMock(ctx, noiseTestMock(`{"other":"o"}`), set); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(ys.MockPath, set, "mocks.*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the set's mock files: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	before = string(data)
	if n := strings.Count(before, e.from); n != 1 {
		t.Fatalf("the %s file holds %q %d times, want once:\n%s", e.format, e.from, n, before)
	}
	edited = strings.Replace(before, e.from, e.to, 1)
	if err := os.WriteFile(files[0], []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	return files[0], before, edited
}

// Under the proven switch the learn step's write-back copies each document
// it writes nothing onto as the file holds it, so a hand edit of a mock it
// learned nothing for stays as the hand made it (keploy mock record's hand
// edits, kept by the record, are not lost to the learn step of its full
// round), in every storage format; the documents it learned for are written
// anew, with what it learned.
func TestPersistLearnedCopiesTheMocksItLearnedNothingFor(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	// The edited mock comes after the one learned for, and before it.
	for _, edit := range [][2]string{{"mock-1", "mock-0"}, {"mock-0", "mock-1"}} {
		for _, e := range learnedFileEdits(edit[0]) {
			t.Run(string(e.format)+"/"+edit[0]+"-edited", func(t *testing.T) {
				learnedOntoTheOther(t, e, edit[1])
			})
		}
	}
}

// learnedOntoTheOther is TestPersistLearnedCopiesTheMocksItLearnedNothingFor
// for one hand edit (e), with the learn step learning for the other mock,
// learnt.
func learnedOntoTheOther(t *testing.T, e learnedFileEdit, learnt string) {
	ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", e.format)
	ctx := context.Background()
	set := "test-set-0"
	path, before, edited := learnedFileWithEdit(t, ys, set, e)
	added, err := ys.PersistLearned(ctx, set, map[string]models.MockState{
		learnt: {Name: learnt, ReqBodyNoise: map[string][]string{"header.x-trace": {}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{learnt: {"header.x-trace (noise)"}}; !reflect.DeepEqual(added, want) {
		t.Fatalf("added %v, want %v", added, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now, was := learnedFileDocs(t, e.format, string(data)), learnedFileDocs(t, e.format, edited)
	if now[e.mock] != was[e.mock] {
		t.Fatalf("%s, which the learn step learned nothing for, is not as the hand edit left it:\n%s\nwant:\n%s", e.mock, now[e.mock], was[e.mock])
	}
	if now[learnt] == learnedFileDocs(t, e.format, before)[learnt] {
		t.Fatalf("%s was not written with what the learn step learned:\n%s", learnt, now[learnt])
	}
	if got := learnedReadBack(t, ys, set)[learnt].Spec.ReqBodyNoise; !reflect.DeepEqual(got, map[string][]string{"header.x-trace": {}}) {
		t.Fatalf("%s's noise read back %v", learnt, got)
	}
}

// learnedReadBack is every mock of set, read back, by name; it fails t
// unless mock-0 and mock-1 are among them.
func learnedReadBack(t *testing.T, ys *MockYaml, set string) map[string]*models.Mock {
	t.Helper()
	ctx := context.Background()
	mocks, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*models.Mock{}
	for _, m := range append(mocks, filtered...) {
		byName[m.Name] = m
	}
	if byName["mock-1"] == nil || byName["mock-0"] == nil {
		t.Fatalf("mocks read back %v", byName)
	}
	return byName
}

// A merge that changes a mock and adds nothing (an answer place on a
// variable the mock has, an echo that no longer holds) is written, whether
// another mock's merge adds something or none does, in every storage format:
// the write-back copies as read only a document it changes nothing of.
func TestPersistLearnedWritesAChangeThatAddsNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, e := range learnedFileEdits("mock-1") {
		for _, other := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/other-learns-%v", e.format, other), func(t *testing.T) {
				ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", e.format)
				ctx := context.Background()
				set := "test-set-0"
				learnedFileWithEdit(t, ys, set, e)
				first := models.MockVariable{Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key"}, Echo: true}
				if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{
					"mock-0": {Name: "mock-0", Variables: models.MockVariables{"key": first}},
				}); err != nil {
					t.Fatal(err)
				}
				states := map[string]models.MockState{
					"mock-0": {Name: "mock-0", Variables: models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key", "header.x-key"}}}},
				}
				if other {
					states["mock-1"] = models.MockState{Name: "mock-1", ReqBodyNoise: map[string][]string{"header.x-trace": {}}}
				}
				added, err := ys.PersistLearned(ctx, set, states)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := added["mock-0"]; ok {
					t.Fatalf("added %v: the second merge adds nothing to mock-0", added)
				}
				want := models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key", "header.x-key"}}}
				if got := learnedReadBack(t, ys, set)["mock-0"].Spec.Variables; !reflect.DeepEqual(got, want) {
					t.Fatalf("mock-0's variables read back %+v, want %+v (both answer places, and no echo)", got, want)
				}
			})
		}
	}
}

// A mock the learn step learns again only what it carries is changed by
// nothing, and copied as the file holds it, hand edit and all, while another
// mock's new noise is written, in every storage format.
func TestPersistLearnedCopiesAMockItLearnsNothingNewFor(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, e := range learnedFileEdits("mock-0") {
		t.Run(string(e.format), func(t *testing.T) {
			ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", e.format)
			ctx := context.Background()
			set := "test-set-0"
			known := map[string]models.MockState{"mock-0": {Name: "mock-0",
				ReqBodyNoise: map[string][]string{"header.x-trace": {}},
				Variables:    models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key"}, Echo: true}}}}
			path, _, _ := learnedFileWithEdit(t, ys, set, e)
			if _, err := ys.PersistLearned(ctx, set, known); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// mock-0 now carries what was learned: it is edited by hand again.
			edited := strings.Replace(string(data), e.from, e.to, 1)
			if edited == string(data) {
				t.Fatalf("mock-0's document, written anew, no longer holds %q:\n%s", e.from, data)
			}
			if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			states := map[string]models.MockState{"mock-0": known["mock-0"],
				"mock-1": {Name: "mock-1", ReqBodyNoise: map[string][]string{"header.x-other": {}}}}
			added, err := ys.PersistLearned(ctx, set, states)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[string][]string{"mock-1": {"header.x-other (noise)"}}; !reflect.DeepEqual(added, want) {
				t.Fatalf("added %v, want %v", added, want)
			}
			if data, err = os.ReadFile(path); err != nil {
				t.Fatal(err)
			}
			if now, was := learnedFileDocs(t, e.format, string(data))["mock-0"], learnedFileDocs(t, e.format, edited)["mock-0"]; now != was {
				t.Fatalf("mock-0, which the learn step learned nothing new for, is not as the hand edit left it:\n%s\nwant:\n%s", now, was)
			}
			if got := learnedReadBack(t, ys, set)["mock-1"].Spec.ReqBodyNoise; !reflect.DeepEqual(got, map[string][]string{"header.x-other": {}}) {
				t.Fatalf("mock-1's noise read back %v", got)
			}
		})
	}
}

// A gob set is written again for a merge that changes a mock and adds
// nothing, as a yaml or json file is: an answer place and an echo that no
// longer holds are not lost.
func TestPersistLearnedWritesAChangeThatAddsNothingToAGobSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Setenv("KEPLOY_MOCK_FORMAT", mockFormatGob)
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	ctx := context.Background()
	set := "test-set-0"
	if err := ys.InsertMock(ctx, noiseTestMock(`{"key":"k-1"}`), set); err != nil {
		t.Fatal(err)
	}
	if err := ys.InsertMock(ctx, noiseTestMock(`{"other":"o"}`), set); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, set, "mocks.gob")); err != nil {
		t.Fatalf("no gob set: %v", err)
	}
	for _, v := range []models.MockVariable{
		{Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key"}, Echo: true},
		{Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key", "header.x-key"}},
	} {
		if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0", Variables: models.MockVariables{"key": v}}}); err != nil {
			t.Fatal(err)
		}
	}
	want := models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key", "header.x-key"}}}
	if got := learnedReadBack(t, ys, set)["mock-0"].Spec.Variables; !reflect.DeepEqual(got, want) {
		t.Fatalf("mock-0's variables read back %+v, want %+v", got, want)
	}
}

// Noise at a variable's place, which an earlier keploy (or a hand) left on
// a mock, is dropped by a merge that adds nothing, and the mock is written
// without it.
func TestPersistLearnedDropsNoiseAtAVariablesPlaceItAddsNothingTo(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ys := New(zap.NewNop(), t.TempDir(), "")
	ctx := context.Background()
	set := "test-set-0"
	v := models.MockVariable{Recorded: "k-1", Bind: "body.key"}
	m := noiseTestMock(`{"key":"k-1"}`)
	m.Spec.Variables = models.MockVariables{"key": v}
	m.Spec.ReqBodyNoise = map[string][]string{"body.key": {}, "header.x-trace": {}}
	if err := ys.InsertMock(ctx, m, set); err != nil {
		t.Fatal(err)
	}
	if err := ys.InsertMock(ctx, noiseTestMock(`{"other":"o"}`), set); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	added, err := ys.PersistLearned(ctx, set, map[string]models.MockState{
		"mock-0": {Name: "mock-0", Variables: models.MockVariables{"key": v}},
	})
	if err != nil || len(added) != 0 {
		t.Fatalf("added %v %v, want nothing", added, err)
	}
	if got := learnedReadBack(t, ys, set)["mock-0"].Spec.ReqBodyNoise; !reflect.DeepEqual(got, map[string][]string{"header.x-trace": {}}) {
		t.Fatalf("mock-0's noise read back %v: a variable's place is never noise", got)
	}
}

// A YAML file whose write stopped in the separator after its last document
// ("--", no newline): the write-back copies that document, which it learned
// nothing for, whole and without the separator's start, and the file ends
// with a newline, so a later write starts on a line of its own.
func TestPersistLearnedCopiesALastMockBeforeAnUnfinishedSeparator(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", yaml.FormatYAML)
	ctx := context.Background()
	set := "test-set-0"
	path, _, edited := learnedFileWithEdit(t, ys, set, learnedFileEdits("mock-1")[0])
	if err := os.WriteFile(path, []byte(edited+"--"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{
		"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"header.x-trace": {}}},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") || strings.HasSuffix(string(data), "--") {
		t.Fatalf("the file does not end with the last mock's newline:\n%s", data)
	}
	if now, was := learnedFileDocs(t, yaml.FormatYAML, string(data))["mock-1"], learnedFileDocs(t, yaml.FormatYAML, edited)["mock-1"]; now != was {
		t.Fatalf("mock-1 is not as the hand edit left it:\n%s\nwant:\n%s", now, was)
	}
	learnedReadBack(t, ys, set)
}

// With the switch off the noise write-back writes every mock of the file
// anew, as it did before: a document it learned nothing for is encoded again.
func TestPersistMockNoiseSwitchOffWritesEveryMockAnew(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, e := range learnedFileEdits("mock-1") {
		t.Run(string(e.format), func(t *testing.T) {
			ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", e.format)
			set := "test-set-0"
			path, before, _ := learnedFileWithEdit(t, ys, set, e)
			if err := ys.PersistMockNoise(context.Background(), set, map[string]models.MockState{
				"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"header.x-trace": {}}},
			}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if now, was := learnedFileDocs(t, e.format, string(data))["mock-1"], learnedFileDocs(t, e.format, before)["mock-1"]; now != was {
				t.Fatalf("switch off: mock-1 is not written anew as keploy encodes it:\n%s\nwant:\n%s", now, was)
			}
		})
	}
}
