package mockdb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// learnedWordsOf is what F learns of the words of (rec, f), by the lead.
func learnedWordsOf(t *testing.T, rec, f string) models.WordsEntry {
	t.Helper()
	e, why := learn.NewDriftCache().Words(rec, f, nil)
	if why != "" {
		t.Fatalf("%q -> %q: %s", rec, f, why)
	}
	return e
}

// setMocks are the set's mocks by name, read as Legible reads them.
func setMocks(t *testing.T, dir, set string) map[string]*models.Mock {
	t.Helper()
	mocks, err := ReadSetMocks(context.Background(), zap.NewNop(), filepath.Join(dir, set))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*models.Mock{}
	for _, m := range mocks {
		out[m.Name] = m
	}
	return out
}

// What the learn step learned of a leaf's words is written onto the
// recording (a state that carries words alone passes the persist gate), said
// per mock, read back, and never added twice; two entries of one leaf in one
// layout union their slots, and two read apart are void. A variable's place
// keeps the variable's words and loses the noise words learned there before
// the value was known to flow on. In every storage format.
func TestPersistLearnedWritesWords(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	// Recorded with a clock read in it: an F that moved it by the lead reads
	// it as one instant (one layout), an F that left it reads it as words
	// (another layout).
	const note = "ORD-1 paid 100 ref=a1b2 seq 7 at 2026-10-21T10:28:02Z"
	words := learnedWordsOf(t, note, "ORD-1 paid 100 ref=c3d4 seq 7 at 2026-10-21T10:29:03Z")
	more := learnedWordsOf(t, note, "ORD-1 paid 100 ref=a1b2 seq 8 at 2026-10-21T10:29:03Z")
	other := learnedWordsOf(t, note, "ORD-1 paid 100 ref=e5f6 seq 7 at 2026-10-21T10:28:02Z")
	if words.Layout != more.Layout || words.Layout == other.Layout {
		t.Fatalf("layouts %s %s %s", words.Layout, more.Layout, other.Layout)
	}
	idWords := learnedWordsOf(t, "k-1", "k-2")
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON, mockFormatGob} {
		t.Run(string(f), func(t *testing.T) {
			dir := t.TempDir()
			var ys *MockYaml
			if f == mockFormatGob {
				SetConfiguredMockFormat(mockFormatGob)
				t.Cleanup(func() { SetConfiguredMockFormat("") })
				ys = New(zap.NewNop(), dir, "mocks")
			} else {
				ys = NewWithFormat(zap.NewNop(), dir, "mocks", f)
			}
			ctx := context.Background()
			set := "test-set-0"
			if err := ys.InsertMock(ctx, noiseTestMock(`{"note":"`+note+`","key":"k-1"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			states := map[string]models.MockState{"mock-0": {Name: "mock-0", LearnedWords: map[string]models.WordsEntry{"body.note": words}}}
			added, err := ys.PersistLearned(ctx, set, states)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[string][]string{"mock-0": {"body.note (words)"}}; !reflect.DeepEqual(added, want) {
				t.Fatalf("added %v, want %v", added, want)
			}
			m := setMocks(t, dir, set)["mock-0"]
			if m == nil || !reflect.DeepEqual(m.Spec.LearnedWords, map[string]models.WordsEntry{"body.note": words}) || len(m.Spec.ReqBodyNoise) != 0 {
				t.Fatalf("read back %+v", m)
			}
			if again, err := ys.PersistLearned(ctx, set, states); err != nil || len(again) != 0 {
				t.Fatalf("words the mock carries are added again: %v %v", again, err)
			}
			// Another slot of the same leaf, in its layout: the union.
			if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0", LearnedWords: map[string]models.WordsEntry{"body.note": more}}}); err != nil {
				t.Fatal(err)
			}
			got := setMocks(t, dir, set)["mock-0"].Spec.LearnedWords["body.note"]
			if want := models.MergeWordsEntry(words, more); !reflect.DeepEqual(got, want) || len(got.Slots) != 3 {
				t.Fatalf("union read back %+v, want %+v", got, want)
			}
			// Read apart (another layout): void, so compared whole.
			if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0", LearnedWords: map[string]models.WordsEntry{"body.note": other}}}); err != nil {
				t.Fatal(err)
			}
			if got := setMocks(t, dir, set)["mock-0"].Spec.LearnedWords["body.note"]; !got.Void() {
				t.Fatalf("two layouts of one leaf read back %+v, want void", got)
			}
			// Words learned as noise at a place, on disk, and then a later
			// call shows the value flowed on: the place is the variable's,
			// so the noise words already written there go, and the
			// variable's own stay.
			if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0", LearnedWords: map[string]models.WordsEntry{"body.key": idWords}}}); err != nil {
				t.Fatal(err)
			}
			if _, noise := setMocks(t, dir, set)["mock-0"].Spec.LearnedWords["body.key"]; !noise {
				t.Fatal("precondition: the noise words at body.key are not on disk")
			}
			if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0",
				Variables: models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Echo: true, Answer: []string{"body.id"}, Words: &idWords}}}}); err != nil {
				t.Fatal(err)
			}
			flowed := setMocks(t, dir, set)["mock-0"].Spec
			if _, noise := flowed.LearnedWords["body.key"]; noise || flowed.Variables["key"].Words == nil {
				t.Fatalf("after the value flowed on: words %+v variables %+v", flowed.LearnedWords, flowed.Variables)
			}
			// The value turns out to flow on: the variable's words stay, the
			// noise words at its place go.
			if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0",
				LearnedWords: map[string]models.WordsEntry{"body.key": idWords},
				Variables:    models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Echo: true, Answer: []string{"body.id"}, Words: &idWords}}}}); err != nil {
				t.Fatal(err)
			}
			back := setMocks(t, dir, set)["mock-0"].Spec
			if _, noise := back.LearnedWords["body.key"]; noise || back.Variables["key"].Words == nil || !reflect.DeepEqual(*back.Variables["key"].Words, idWords) {
				t.Fatalf("words %+v variables %+v", back.LearnedWords, back.Variables)
			}
		})
	}
}

// A variable's words follow its bind in the store's merge, as in
// models.MergeVariables: a use never strips them, a bind taken from the other
// side takes its words, and two that bind at one place union theirs.
func TestMergeVariableKeepsWordsWithTheBind(t *testing.T) {
	a := learnedWordsOf(t, "o-1-a", "o-1-b")
	b := learnedWordsOf(t, "o-1-a", "o-2-a")
	if got := mergeVariable(models.MockVariable{Recorded: "o-1-a", Use: []string{"url.path[1]"}}, models.MockVariable{Recorded: "o-1-a", Bind: "body.id", Words: &a}); got.Words == nil || !reflect.DeepEqual(*got.Words, a) {
		t.Fatalf("a bind merged into a use lost its words: %+v", got.Words)
	}
	if got := mergeVariable(models.MockVariable{Recorded: "o-1-a", Bind: "body.id", Words: &a}, models.MockVariable{Recorded: "o-1-a", Use: []string{"url.query.id"}}); got.Words == nil || !reflect.DeepEqual(*got.Words, a) {
		t.Fatalf("a use stripped a bind's words: %+v", got.Words)
	}
	got := mergeVariable(models.MockVariable{Recorded: "o-1-a", Bind: "body.id", Words: &a}, models.MockVariable{Recorded: "o-1-a", Bind: "body.id", Words: &b})
	if want := models.MergeWordsEntry(a, b); got.Words == nil || !reflect.DeepEqual(*got.Words, want) {
		t.Fatalf("two binds at one place merged words %+v, want %+v", got.Words, want)
	}
}

// Backward compatibility (compat audit A6): a rewrite with the switch off (a
// noise persist, a prune) reads and writes back every mock's words, a newer
// keploy's entry included, so the next proven run reads what was learned; a
// mock that carries none is written byte for byte as before. The gob format
// (KEPLOY_MOCK_FORMAT=gob) carries them too; its bytes are the gob encoder's,
// so only the YAML and JSON files are compared byte for byte.
func TestWordsSurviveASwitchOffRewrite(t *testing.T) {
	const note, noteF = "ORD-1 paid 100 ref=a1b2", "ORD-1 paid 100 ref=c3d4"
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON, mockFormatGob} {
		t.Run(string(f), func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			words := learnedWordsOf(t, note, noteF)
			newer := models.WordsEntry{V: models.WordsVersion + 1, Unknown: `{"digest":"9f2c41d07a3be815","shape":{"k":[1,2]}}`}
			varWords := learnedWordsOf(t, "k-1", "k-2")
			dir := t.TempDir()
			var ys *MockYaml
			if f == mockFormatGob {
				SetConfiguredMockFormat(mockFormatGob)
				t.Cleanup(func() { SetConfiguredMockFormat("") })
				ys = New(zap.NewNop(), dir, "mocks")
			} else {
				ys = NewWithFormat(zap.NewNop(), dir, "mocks", f)
			}
			ctx := context.Background()
			set := "test-set-0"
			m := noiseTestMock(`{"note":"` + note + `","key":"k-1","other":"o"}`)
			m.Spec.LearnedWords = map[string]models.WordsEntry{"body.note": words, "body.later": newer}
			m.Spec.Variables = models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Echo: true, Answer: []string{"body.key"}, Words: &varWords}}
			if err := ys.InsertMock(ctx, m, set); err != nil {
				t.Fatal(err)
			}
			if err := ys.InsertMock(ctx, noiseTestMock(`{"plain":"p"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			ext := string(mockFormatGob)
			if f != mockFormatGob {
				ext = f.FileExtension()
			}
			file := filepath.Join(dir, set, "mocks."+ext)
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			check := func(when string) {
				t.Helper()
				back := setMocks(t, dir, set)
				got := back["mock-0"].Spec
				if !reflect.DeepEqual(got.LearnedWords, m.Spec.LearnedWords) || !reflect.DeepEqual(got.Variables, m.Spec.Variables) {
					t.Fatalf("%s: words %+v variables %+v, want %+v %+v", when, got.LearnedWords, got.Variables, m.Spec.LearnedWords, m.Spec.Variables)
				}
				if back["mock-1"].Spec.LearnedWords != nil {
					t.Fatalf("%s: a mock with no words gained some", when)
				}
			}
			check("as written")
			// A noise persist rewrites the file: mock-1 learns noise.
			if err := ys.PersistMockNoise(ctx, set, map[string]models.MockState{"mock-1": {Name: "mock-1", ReqBodyNoise: map[string][]string{"body.plain": {}}}}); err != nil {
				t.Fatal(err)
			}
			check("after a switch-off noise persist")
			after, _ := os.ReadFile(file)
			first := func(b []byte) []byte {
				// mock-0's document: everything before mock-1's.
				if i := bytes.Index(b, []byte("mock-1")); i > 0 {
					return b[:i]
				}
				return b
			}
			if f != mockFormatGob && !bytes.Equal(first(before), first(after)) {
				t.Fatalf("the rewrite changed the words-carrying mock's bytes:\n%s\n---\n%s", first(before), first(after))
			}
			// A prune that keeps every mock rewrites it too.
			if err := ys.UpdateMocks(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0"}, "mock-1": {Name: "mock-1"}}, models.BaseTime, models.BaseTime); err != nil {
				t.Fatal(err)
			}
			check("after a switch-off prune")
		})
	}
}

// Record's end writes the set's learned files under the switch, once the
// .gitignore ignores them, and nothing with it off.
func TestWriteLearnedFiles(t *testing.T) {
	// The HTTP parser reads an HTTP mock's leaves (learn.RegisterLeafReader);
	// this package does not link it, so a reader of the JSON body stands in.
	learn.RegisterLeafReader(models.HTTP, func(m *models.Mock) map[string]learn.Leaf {
		out := map[string]learn.Leaf{}
		if v, ok := learn.DecodeJSON([]byte(m.Spec.HTTPReq.Body)); ok {
			learn.JSONLeaves("body", v, out)
		}
		return out
	})
	t.Cleanup(func() { learn.RegisterLeafReader(models.HTTP, nil) })
	dir := t.TempDir()
	keployDir := filepath.Join(dir, "keploy")
	ys := New(zap.NewNop(), keployDir, "")
	ctx := context.Background()
	set := "test-set-0"
	m := noiseTestMock(`{"note":"ORD-1 ref=a1"}`)
	m.Spec.LearnedWords = map[string]models.WordsEntry{"body.note": learnedWordsOf(t, "ORD-1 ref=a1", "ORD-1 ref=b2")}
	if err := ys.InsertMock(ctx, m, set); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	states := map[string]models.MockState{"mock-0": {Name: "mock-0", LearnedF: map[string]map[string]string{"mock-0": {"body.note": "ORD-1 ref=b2"}}}}
	t.Setenv(provenswitch.Env, "")
	if err := ys.WriteLearnedFiles(set, states); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(keployDir, ".gitignore"), filepath.Join(keployDir, set, models.LearnedFile), filepath.Join(keployDir, set, models.LearnedLocalFile)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("the switch off wrote %s (%v)", p, err)
		}
	}
	t.Setenv(provenswitch.Env, "1")
	if err := ys.WriteLearnedFiles(set, states); err != nil {
		t.Fatal(err)
	}
	gi, _ := os.ReadFile(filepath.Join(keployDir, ".gitignore"))
	public, _ := os.ReadFile(filepath.Join(keployDir, set, models.LearnedFile))
	local, _ := os.ReadFile(filepath.Join(keployDir, set, models.LearnedLocalFile))
	if !strings.Contains(string(gi), "/*/learned.yaml") || !strings.Contains(string(gi), "/*/learned.local.yaml") {
		t.Fatalf(".gitignore %q", gi)
	}
	if !strings.Contains(string(public), "ref=⟨changes⟩") || strings.Contains(string(public), "ref=b2") {
		t.Fatalf("learned.yaml:\n%s", public)
	}
	if !strings.Contains(string(local), "ref=b2") {
		t.Fatalf("learned.local.yaml:\n%s", local)
	}
}

// The set reader Legible reads a set by (registered at init, and
// ReadSetMocks itself) fails on a mocks file it cannot read or decode, and
// reads a mocks file whose last mock was cut short with no panic and no
// log of its own.
func TestTheSetReaderSaysWhatItCannotRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	write := func(set, name, data string) string {
		d := filepath.Join(dir, set)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if _, err := ReadSetMocks(context.Background(), zap.NewNop(), write("gob", "mocks.gob", "not a gob stream")); err == nil {
		t.Error("a mocks.gob that is no gob: read")
	}
	bad := "version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\nspec: [1, 2]\n"
	if _, err := ReadSetMocks(context.Background(), zap.NewNop(), write("bad", "mocks.yaml", bad)); err == nil {
		t.Error("a mock whose spec is no mapping: read")
	}
	none, err := ReadSetMocks(context.Background(), zap.NewNop(), filepath.Join(dir, "absent"))
	if err != nil || none != nil {
		t.Errorf("a set with no mocks file: %v, %v", none, err)
	}
	cut := "version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\nspec:\n  metadata: {}\n  req:\n    method: GET\n    url: http://a/x"
	if _, err := learn.Legible(write("cut", "mocks.yaml", cut)); err != nil {
		t.Errorf("a set whose last mock was cut short: %v", err)
	}
	// A document that is no YAML, whole (its line ended), is a read error.
	if _, err := ReadSetMocks(context.Background(), zap.NewNop(), write("syntax", "mocks.yaml", "version: [\n")); err == nil || !strings.Contains(err.Error(), "read the mocks of") {
		t.Errorf("a mocks.yaml that is no YAML: %v", err)
	}
}

// A state that learned nothing writes nothing and reads nothing: the set's
// mocks file is not even opened (here it would not decode).
func TestAStateThatLearnedNothingReadsNoMocks(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	if err := os.MkdirAll(filepath.Join(dir, "test-set-0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test-set-0", "mocks.yaml"), []byte("version: x\nkind: Http\nname: mock-0\nspec: [1, 2]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	added, err := ys.PersistLearned(context.Background(), "test-set-0", map[string]models.MockState{"mock-0": {Name: "mock-0", LearnedWords: map[string]models.WordsEntry{}, Variables: models.MockVariables{}, ReqBodyNoise: map[string][]string{}}})
	if err != nil || added != nil {
		t.Fatalf("added %v err %v, want nothing read", added, err)
	}
}
