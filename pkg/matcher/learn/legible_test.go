package learn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// withSet registers a set reader that reads mocks for every set folder, and
// a leaf reader of HTTP mocks that reads each mock's body.note and
// header.x-ref from its metadata, for the test only.
func withSet(t *testing.T, mocks []*models.Mock) {
	t.Helper()
	readersMu.RLock()
	prevSet, prevLeaf, prevWords := setReader, leafReaders[models.HTTP], wordsKinds[models.HTTP]
	readersMu.RUnlock()
	RegisterSetReader(func(string) ([]*models.Mock, error) { return mocks, nil })
	RegisterWordsKind(models.HTTP)
	RegisterLeafReader(models.HTTP, func(m *models.Mock) map[string]Leaf {
		out := map[string]Leaf{}
		for _, p := range []string{"body.note", "header.x-ref", "body.id"} {
			if v, ok := m.Spec.Metadata[p]; ok {
				out[p] = Leaf{Path: p, Type: String, Value: v}
			}
		}
		return out
	})
	t.Cleanup(func() {
		RegisterSetReader(prevSet)
		RegisterLeafReader(models.HTTP, prevLeaf)
		readersMu.Lock()
		wordsKinds[models.HTTP] = prevWords
		readersMu.Unlock()
	})
}

// A value a learned file holds is redacted where its place names a
// credential, the recording's obfuscator matches it, or it is written as a
// credential is; else it is cut at 128 bytes, on a rune's start.
func TestRedactLearnedValue(t *testing.T) {
	long := strings.Repeat("é", 100)
	for _, tc := range []struct{ path, value, want string }{
		{"body.note", "ORD-1 paid 100", "ORD-1 paid 100"},
		{"header.authorization", "abc", redacted},
		// A path's segments split at each of . [ ] { }: a credential's name
		// between any of them is one.
		{"body.user[pin]", "1234", redacted},
		{"body[pin].user", "1234", redacted},
		{"request{cookie}", "a=b", redacted},
		{"body.x{otp}.y", "1", redacted},
		{"body.user.db_password", "hunter2", redacted},
		{"header.x-api-key", "k1", redacted},
		{"body.note", "Bearer abc.def", redacted},
		{"body.note", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig", redacted},
		{"body.note", "user=a password=b", redacted},
		{"body.note", "secret-ORD", redacted},
	} {
		noise := []string{}
		if tc.value == "secret-ORD" {
			noise = []string{"^secret-"}
		}
		if got := RedactLearnedValue(tc.path, tc.value, noise); got != tc.want {
			t.Errorf("RedactLearnedValue(%q, %q) = %q, want %q", tc.path, tc.value, got, tc.want)
		}
	}
	got := RedactLearnedValue("body.note", long, nil)
	if !strings.HasSuffix(got, cutMark) || len(got)-len(cutMark) > maxLearnedValue || !strings.HasPrefix(long, strings.TrimSuffix(got, cutMark)) {
		t.Errorf("a long value is %q", got)
	}
	if RedactLearnedValue("body.note", "x", []string{"("}) != redacted {
		t.Error("an obfuscator pattern keploy cannot read left a value unredacted")
	}
}

// The .gitignore ensure adds only the lines it lacks, keeps every other line,
// is written whole, and with the switch off writes nothing and fails.
func TestEnsureLearnedIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(provenswitch.Env, "")
	if err := EnsureLearnedIgnored(dir); err == nil {
		t.Fatal("the ensure succeeded with the switch off: a learned file could be written")
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("the switch off wrote a .gitignore (%v)", err)
	}
	t.Setenv(provenswitch.Env, "1")
	path := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(path, []byte("/reports\n/*/learned.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := EnsureLearnedIgnored(dir); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(path)
	want := "/reports\n/*/learned.yaml\n/*/learned.local.yaml\n/*/.learned-*\n"
	if string(b) != want {
		t.Fatalf(".gitignore is %q, want %q", b, want)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the ensure left %d files, want the .gitignore alone", len(entries))
	}
}

// Legible says every leaf the set's mocks learned: noise words with their
// slots marked, a variable's and an echo's words, and noise learned whole
// before words, each with its rule, its bases and how to undo it; an entry
// this build cannot read, and a void one, are compared whole.
func TestLegibleSaysWhatEachMockLearned(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	note, noteF := "ORD-1 paid 100 ref=3f2b8c1e9a4d at "+wAt.Format(time.RFC3339), "ORD-1 paid 100 ref=a7c4e2d10b3f at "+later.Format(time.RFC3339)
	e, why := NewDriftCache().Words(note, noteF, nil)
	if why != "" {
		t.Fatal(why)
	}
	idE, _ := NewDriftCache().Words("o-1111", "o-9999", nil)
	m := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:     map[string]string{"body.note": note, "header.x-ref": "abc", "body.id": "o-1111"},
		LearnedWords: map[string]models.WordsEntry{"body.note": e, "header.x-ref": {V: models.WordsVersion + 1, Unknown: `{"digest":"x"}`}},
		ReqBodyNoise: map[string][]string{"body.at": {}},
		Variables: models.MockVariables{
			"id":  {Recorded: "o-1111", Bind: "body.id", Echo: true, Words: &idE},
			"old": {Recorded: "o-1111", Bind: "url.query.old"},
		},
	}}
	// A Postgres mock: its kind's learn step learns a leaf only whole
	// (r9 §5), and no reader of its leaves is registered here.
	pg := &models.Mock{Name: "mock-2", Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqBodyNoise: map[string][]string{"bind.$1": {}},
		Variables:    models.MockVariables{"id": {Recorded: "7", Bind: "bind.$2", Echo: true}},
	}}
	withSet(t, []*models.Mock{m, pg})
	leaves, err := Legible(filepath.Join(t.TempDir(), "test-set-0"))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]models.LearnedLeaf{}
	for _, l := range leaves {
		by[l.Mock+":"+l.Path+"/"+l.Variable] = l
	}
	for k, l := range by {
		if strings.HasPrefix(k, "mock-1:") {
			delete(by, k)
			by[strings.TrimPrefix(k, "mock-1:")] = l
		}
	}
	n := by["body.note/"]
	if n.Rule != models.RuleNoise || n.Recorded != note || n.F != "" || len(n.Slots) != 2 ||
		n.Rendered != "body.note = ORD-1 paid 100 ref=⟨changes⟩ at ⟨changes⟩" || !strings.Contains(n.Why, "delete its entry under noise.words") {
		t.Errorf("body.note: %+v", n)
	}
	if got := n.Basis; len(got) != 2 || got[0] != models.BasisChanged || got[1] != models.BasisClockRead {
		t.Errorf("body.note bases %v", got)
	}
	if x := by["header.x-ref/"]; len(x.Slots) != 0 || !strings.Contains(x.Rendered, "compared whole") {
		t.Errorf("an entry of a newer keploy: %+v", x)
	}
	if v := by["body.id/id"]; v.Rule != models.RuleEcho || len(v.Slots) == 0 || v.Basis[0] != models.BasisEcho || v.Rendered != "body.id = o-⟨changes⟩" {
		t.Errorf("the echo: %+v", v)
	}
	if v := by["url.query.old/old"]; v.Rule != models.RuleVariable || len(v.Slots) != 0 || !strings.Contains(v.Why, "re-record to tighten") ||
		len(v.Basis) != 2 || v.Basis[1] != models.BasisBeforeWords {
		t.Errorf("a variable learned before words: %+v", v)
	}
	if v := by["body.id/id"]; slices.Contains(v.Basis, models.BasisBeforeWords) {
		t.Errorf("an echo with words is no echo bound before words: %+v", v)
	}
	// body.at is no leaf the HTTP reader reads of the mock: said unread.
	if w := by["body.at/"]; w.Rule != models.RuleWholeNoise || len(w.Basis) != 1 || w.Basis[0] != models.BasisBeforeWords ||
		!strings.Contains(w.Why, "before keploy learned words. Re-record to tighten") || w.Recorded != notALeaf {
		t.Errorf("HTTP whole noise: %+v", w)
	}
	// The Postgres mock's leaves: learned whole as keploy learns them today,
	// never "re-record to tighten", their recorded values said unread.
	w := by["mock-2:bind.$1/"]
	if w.Rule != models.RuleWholeNoise || len(w.Basis) != 1 || w.Basis[0] != models.BasisWhole || w.Recorded != notRead ||
		strings.Contains(strings.ToLower(w.Why), "re-record to tighten") || !strings.Contains(w.Why, "only whole, when record's full round saw every word of it change") ||
		!strings.Contains(w.Why, "a re-record learns it whole again") || w.Rendered != "bind.$1 = ⟨changes⟩" {
		t.Errorf("Postgres whole noise: %+v", w)
	}
	if e := by["mock-2:bind.$2/id"]; e.Rule != models.RuleEcho || len(e.Basis) != 1 || e.Basis[0] != models.BasisEcho ||
		strings.Contains(strings.ToLower(e.Why), "re-record to tighten") || !strings.Contains(e.Why, "binds a leaf of a "+string(models.PostgresV3)+" mock only whole") {
		t.Errorf("Postgres echo: %+v", e)
	}
	again, _ := Legible(filepath.Join(t.TempDir(), "test-set-0"))
	for i := range leaves {
		if leaves[i].ID != again[i].ID || leaves[i].Path != again[i].Path {
			t.Fatalf("Legible read the same mocks otherwise: %+v, %+v", leaves[i], again[i])
		}
	}
	t.Setenv(provenswitch.Env, "1")
	lines := RetightenLines(leaves, true)
	want := []string{
		"mock-1 url.query.old: re-record to tighten (bound whole, before keploy learned words)",
		"mock-1 body.at: re-record to tighten (learned whole, before keploy learned words)",
	}
	if !slices.Equal(lines, want) || RetightenLines(leaves, false) != nil {
		t.Errorf("N6 lines %q, want %q", lines, want)
	}
	t.Setenv(provenswitch.Env, "")
	if RetightenLines(leaves, true) != nil {
		t.Error("N6 printed with the switch off")
	}
}

// Record's end writes learned.yaml from the mocks and learned.local.yaml with
// F's values, only once the .gitignore ignores both; the copy-back merges a
// staging copy's local values by ID, the staging copy winning, and prunes
// what the staging mocks no longer learn. With the switch off nothing is
// written, and a set that learned nothing gets no file.
// learned.local.yaml redacts F's values as learned.yaml redacts recorded
// ones (RedactLearnedValue): a value F saw that reads as a credential is
// never written, even where the recorded one did not read as one, and a
// long one is cut.
func TestTheLocalFileRedactsFsValues(t *testing.T) {
	keploy := filepath.Join(t.TempDir(), "keploy")
	set := filepath.Join(keploy, "test-set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	e, _ := NewDriftCache().Words("ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	mocks := []*models.Mock{{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"body.note": "ORD-1 ref=a1"}, LearnedWords: map[string]models.WordsEntry{"body.note": e}}}}
	withSet(t, nil)
	RegisterSetReader(func(string) ([]*models.Mock, error) { return mocks, nil })
	t.Setenv(provenswitch.Env, "1")
	long := "ORD-1 ref=" + strings.Repeat("b", 200)
	for _, tc := range []struct{ f, want string }{
		{"ORD-1 ref=b2 user=a password=hunter2", redacted},
		{"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig", redacted},
		{long, RedactLearnedValue("body.note", long, nil)},
	} {
		if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: tc.f}}); err != nil {
			t.Fatal(err)
		}
		local, err := readLearned(filepath.Join(set, models.LearnedLocalFile))
		if err != nil || len(local) != 1 || local[0].F != tc.want {
			t.Fatalf("F %q written as %+v (%v), want %q", tc.f, local, err, tc.want)
		}
		if b, _ := os.ReadFile(filepath.Join(set, models.LearnedLocalFile)); strings.Contains(string(b), "hunter2") || strings.Contains(string(b), strings.Repeat("b", 200)) {
			t.Fatalf("learned.local.yaml holds what it redacts:\n%s", b)
		}
	}
}

// learned.local.yaml is a function of F's values, not of the order they come
// in: a leaf two states saw take two values (a mock answered more than once)
// keeps the least, in either order.
func TestTheLocalFileIsDeterministic(t *testing.T) {
	keploy := filepath.Join(t.TempDir(), "keploy")
	set := filepath.Join(keploy, "test-set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	e, _ := NewDriftCache().Words("ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	mocks := []*models.Mock{{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"body.note": "ORD-1 ref=a1"}, LearnedWords: map[string]models.WordsEntry{"body.note": e}}}}
	withSet(t, nil)
	RegisterSetReader(func(string) ([]*models.Mock, error) { return mocks, nil })
	t.Setenv(provenswitch.Env, "1")
	b2, c3 := LocalValue{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=b2"}, LocalValue{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=c3"}
	var written []string
	for _, local := range [][]LocalValue{{b2, c3}, {c3, b2}} {
		if err := os.Remove(filepath.Join(set, models.LearnedLocalFile)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := WriteLearned(set, local); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(set, models.LearnedLocalFile))
		if err != nil {
			t.Fatal(err)
		}
		written = append(written, string(b))
	}
	if written[0] != written[1] || !strings.Contains(written[0], "ref=b2") {
		t.Fatalf("learned.local.yaml depends on the order of F's values:\n%s\n---\n%s", written[0], written[1])
	}
}

func TestTheLearnedFilesAreWrittenIgnoredFirst(t *testing.T) {
	root := t.TempDir()
	keploy := filepath.Join(root, "keploy")
	set := filepath.Join(keploy, "test-set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	e, _ := NewDriftCache().Words("ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	m := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"body.note": "ORD-1 ref=a1"}, LearnedWords: map[string]models.WordsEntry{"body.note": e}}}
	mocks := []*models.Mock{m}
	withSet(t, nil)
	RegisterSetReader(func(string) ([]*models.Mock, error) { return mocks, nil })

	t.Setenv(provenswitch.Env, "")
	if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=b2"}}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(keploy); len(entries) != 1 {
		t.Fatalf("the switch off wrote %d entries in keploy/", len(entries))
	}
	if entries, _ := os.ReadDir(set); len(entries) != 0 {
		t.Fatalf("the switch off wrote %d files in the set", len(entries))
	}

	t.Setenv(provenswitch.Env, "1")
	if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=b2"}}); err != nil {
		t.Fatal(err)
	}
	gi, err := os.ReadFile(filepath.Join(keploy, ".gitignore"))
	if err != nil || !strings.Contains(string(gi), "/*/learned.local.yaml") {
		t.Fatalf(".gitignore %q (%v)", gi, err)
	}
	local, err := readLearned(filepath.Join(set, models.LearnedLocalFile))
	if err != nil || len(local) != 1 || local[0].F != "ORD-1 ref=b2" {
		t.Fatalf("learned.local.yaml %+v (%v)", local, err)
	}
	public, err := readLearned(filepath.Join(set, models.LearnedFile))
	if err != nil || len(public) != 1 || public[0].F != "" || public[0].ID != local[0].ID {
		t.Fatalf("learned.yaml %+v (%v)", public, err)
	}
	if b, _ := os.ReadFile(filepath.Join(set, models.LearnedFile)); !strings.HasPrefix(string(b), "# ") || strings.Contains(string(b), "ref=b2") {
		t.Errorf("learned.yaml holds F's value or no header:\n%s", b)
	}
	// Both files are one document shape (models.LearnedDocument), the one
	// the enterprise verbs write too: the leaves under learned, nothing else.
	for _, name := range []string{models.LearnedFile, models.LearnedLocalFile} {
		b, _ := os.ReadFile(filepath.Join(set, name))
		var doc map[string][]map[string]interface{}
		if err := yaml.Unmarshal(b, &doc); err != nil || len(doc) != 1 || len(doc["learned"]) != 1 {
			t.Errorf("%s is not {learned: [one leaf]}: %v (%v)\n%s", name, doc, err, b)
		}
	}

	// The copy-back: the staging copy's value wins, and a stale entry goes.
	stage := filepath.Join(root, "stage", "keploy", "test-set-0")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	staged := local[0]
	staged.F = "ORD-1 ref=c3"
	stale := staged
	stale.ID, stale.F = "0000000000000000", "gone"
	if err := writeLearned(stage, models.LearnedLocalFile, []models.LearnedLeaf{staged, stale}); err != nil {
		t.Fatal(err)
	}
	// The copy-back ensures the project's .gitignore, never the staging
	// folder's (which goes when its verb ends).
	stageIgnore, projectIgnore := filepath.Join(filepath.Dir(stage), ".gitignore"), filepath.Join(filepath.Dir(set), ".gitignore")
	_ = os.Remove(stageIgnore)
	_ = os.Remove(projectIgnore)
	if err := MergeLocal(set, stage); err != nil {
		t.Fatal(err)
	}
	merged, _ := readLearned(filepath.Join(set, models.LearnedLocalFile))
	if len(merged) != 1 || merged[0].F != "ORD-1 ref=c3" {
		t.Fatalf("merged %+v", merged)
	}
	if _, err := os.Stat(stageIgnore); !os.IsNotExist(err) {
		t.Errorf("the copy-back wrote the staging folder's .gitignore (%v)", err)
	}
	if b, err := os.ReadFile(projectIgnore); err != nil || !strings.Contains(string(b), "/*/"+models.LearnedLocalFile) {
		t.Errorf("the copy-back left the project's .gitignore without its lines: %q (%v)", b, err)
	}

	// The mocks learn nothing any more: both files go.
	mocks = []*models.Mock{{Name: "mock-1", Kind: models.HTTP}}
	if err := WriteLearned(set, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{models.LearnedFile, models.LearnedLocalFile} {
		if _, err := os.Stat(filepath.Join(set, name)); !os.IsNotExist(err) {
			t.Errorf("%s is left though the mocks learn nothing (%v)", name, err)
		}
	}
	if entries, _ := os.ReadDir(set); len(entries) != 0 {
		t.Errorf("a temporary file is left: %v", entries)
	}

	// A set that never learned anything gets no file and no .gitignore.
	fresh := filepath.Join(t.TempDir(), "keploy", "test-set-1")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteLearned(fresh, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(fresh), ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("a set that learned nothing wrote a .gitignore (%v)", err)
	}
}

// A learned file is written only when its bytes change: one that holds them
// already is left as it is (its inode, mode and time stay), so a status, a
// pull's prune or a copy-back with nothing to change touches nothing; one
// whose bytes change is replaced with the mode it had, and a new one is
// 0644. A set whose mocks learn nothing any more loses its learned.yaml
// without a .gitignore line written for it.
func TestTheLearnedFilesAreWrittenOnlyWhenTheyChange(t *testing.T) {
	root := t.TempDir()
	keploy := filepath.Join(root, "keploy")
	set := filepath.Join(keploy, "test-set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	e, _ := NewDriftCache().Words("ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	mocks := []*models.Mock{{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"body.note": "ORD-1 ref=a1"}, LearnedWords: map[string]models.WordsEntry{"body.note": e}}}}
	withSet(t, nil)
	RegisterSetReader(func(string) ([]*models.Mock, error) { return mocks, nil })
	t.Setenv(provenswitch.Env, "1")

	if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=b2"}}); err != nil {
		t.Fatal(err)
	}
	public, local := filepath.Join(set, models.LearnedFile), filepath.Join(set, models.LearnedLocalFile)
	for _, p := range []string{public, local} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("a new learned file %s: %v (%v); want 0644", p, info, err)
		}
	}
	if err := os.Chmod(local, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, p := range []string{public, local} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(p string) os.FileInfo {
		t.Helper()
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	beforePublic, beforeLocal := stat(public), stat(local)

	// Nothing changes: a status's refresh, a pull's prune of the set into
	// itself, a copy-back of the same entries.
	if err := WriteLegible(set); err != nil {
		t.Fatal(err)
	}
	if err := MergeLocal(set, set); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path   string
		before os.FileInfo
	}{{public, beforePublic}, {local, beforeLocal}} {
		after := stat(c.path)
		if !os.SameFile(c.before, after) || !after.ModTime().Equal(old) || after.Mode() != c.before.Mode() {
			t.Fatalf("%s was written though its bytes did not change: %v -> %v", c.path, c.before, after)
		}
	}

	// The bytes change (an entry the mocks no longer learn is pruned): the
	// file is replaced, its mode kept.
	have, err := readLearned(local)
	if err != nil || len(have) != 1 {
		t.Fatalf("learned.local.yaml %+v (%v)", have, err)
	}
	stale := have[0]
	stale.ID, stale.Path = "0000000000000000", "body.gone"
	if err := writeLearned(set, models.LearnedLocalFile, []models.LearnedLeaf{have[0], stale}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(local, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MergeLocal(set, set); err != nil {
		t.Fatal(err)
	}
	if pruned, err := readLearned(local); err != nil || len(pruned) != 1 || pruned[0].ID != have[0].ID {
		t.Fatalf("pruned learned.local.yaml %+v (%v)", pruned, err)
	}
	if info := stat(local); info.Mode().Perm() != 0o600 {
		t.Fatalf("the replaced learned.local.yaml is %v; want the 0600 it had", info.Mode().Perm())
	}

	// The mocks learn nothing any more: learned.yaml goes, and no .gitignore
	// line is written to remove it.
	if err := os.Remove(filepath.Join(keploy, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	mocks = []*models.Mock{{Name: "mock-1", Kind: models.HTTP}}
	if err := WriteLegible(set); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(public); !os.IsNotExist(err) {
		t.Fatalf("learned.yaml is left though the mocks learn nothing (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(keploy, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("removing learned.yaml wrote a .gitignore (%v)", err)
	}
	if entries, _ := os.ReadDir(set); len(entries) != 1 {
		t.Fatalf("the set holds %v; want learned.local.yaml alone", entries)
	}
}

// A long value is cut at the start of the rune that holds its byte 128, the
// cut marked: an ASCII value at 128 exactly, one whose rune at 128 began a
// byte earlier there, and bytes that start no rune to nothing at all.
func TestALongValueIsCutAtARunesStart(t *testing.T) {
	ascii := strings.Repeat("a", 200)
	odd := "a" + strings.Repeat("é", 100)
	for _, tc := range []struct{ value, want string }{
		{strings.Repeat("a", 128), strings.Repeat("a", 128)},
		{ascii, ascii[:128] + cutMark},
		{odd, odd[:127] + cutMark},
		{strings.Repeat("\x80", 200), cutMark},
	} {
		if got := cutValue(tc.value); got != tc.want {
			t.Errorf("cutValue(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// A learned leaf's ID is its set, its mock, its path and its words' digest,
// each apart: stable, and another of any of them is another ID.
func TestALearnedLeafsIDIsItsPlace(t *testing.T) {
	id := learnedID("test-set-0", "mock-1", "body.note", "9f2c41d07a3be815")
	if id != learnedID("test-set-0", "mock-1", "body.note", "9f2c41d07a3be815") || len(id) != 16 {
		t.Fatalf("ID %q is not stable", id)
	}
	for _, other := range [][4]string{
		{"test-set-1", "mock-1", "body.note", "9f2c41d07a3be815"},
		{"test-set-0", "mock-2", "body.note", "9f2c41d07a3be815"},
		{"test-set-0", "mock-1", "body.id", "9f2c41d07a3be815"},
		{"test-set-0", "mock-1", "body.note", ""},
		{"test-set-0mock-1", "", "body.note", "9f2c41d07a3be815"},
	} {
		if learnedID(other[0], other[1], other[2], other[3]) == id {
			t.Errorf("%q has the ID of another leaf", other)
		}
	}
}

// An entry's bases are its slots' kinds, in order, each once.
func TestAnEntrysBasesAreEachOnce(t *testing.T) {
	e := models.WordsEntry{Slots: []models.WordSlot{{Seg: 0}, {Seg: 1}, {Seg: 2, Kind: models.WordSlotToken}, {Seg: 3, Kind: models.WordSlotInstant}, {Seg: 4, Kind: models.WordSlotToken}}}
	if got := slotBases(e); !slices.Equal(got, []models.LearnBasis{models.BasisChanged, models.BasisToken, models.BasisClockRead}) {
		t.Fatalf("bases %v", got)
	}
}

// A rendered value marks each slot where the recorded value's own collapse
// puts it: an instant learned beside one that did not change (the second of
// two of one layout), and two slots that touch (an instant, and the token
// right after it), each marked.
func TestEachSlotIsMarkedWhereItStands(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	static := wAt.Add(-48 * time.Hour).Format(time.RFC3339)
	rec, f := "from "+static+" to "+wAt.Format(time.RFC3339), "from "+static+" to "+later.Format(time.RFC3339)
	e := learnedWords(t, rec, f)
	if got := renderSlots(rec, e); got != "from "+static+" to ⟨changes⟩" {
		t.Errorf("rendered %q", got)
	}
	rec, f = wAt.Format(time.RFC3339)+"-ab12", later.Format(time.RFC3339)+"-cd_34"
	e = learnedWords(t, rec, f)
	if got := slotsOf(e); got != "s0i:iso8601:d4-d2-d2Td2:d2:d2FZ s1t" {
		t.Fatalf("slots %s", got)
	}
	if got := renderSlots(rec, e); got != "⟨changes⟩⟨changes⟩" {
		t.Errorf("two slots that touch rendered %q", got)
	}
	// What stands after the last slot is written too, and the slots are
	// marked in the value's order whatever order the entry lists them in.
	rec = "id ab-12-cd tail"
	e = learnedWords(t, rec, "id ab-34-ce tail")
	if got := renderSlots(rec, e); got != "id ab-⟨changes⟩-⟨changes⟩ tail" {
		t.Errorf("rendered %q", got)
	}
	e.Slots[0], e.Slots[1] = e.Slots[1], e.Slots[0]
	if got := renderSlots(rec, e); got != "id ab-⟨changes⟩-⟨changes⟩ tail" {
		t.Errorf("slots listed out of order rendered %q", got)
	}
}

// What a learned file says of a leaf: unread, redacted (by its place, or by
// the recording's obfuscator), learned whole, compared whole and why, or its
// slots marked.
func TestWhatALearnedLeafIsRenderedAs(t *testing.T) {
	e := learnedWords(t, "ORD-1 ref=a1", "ORD-1 ref=b2")
	void := models.VoidWords(e.Digest)
	for _, tc := range []struct {
		name, path, rec string
		read            bool
		noise           []string
		e               *models.WordsEntry
		want            string
	}{
		{"unread", "body.note", "", false, nil, &e, "body.note = " + notALeaf},
		{"redacted by its place", "header.authorization", "ORD-1 ref=a1", true, nil, &e, "header.authorization = " + redacted},
		{"redacted by the obfuscator", "body.note", "ORD-1 ref=a1", true, []string{"^ORD-1 ref=a1$"}, &e, "body.note = " + redacted},
		{"learned whole", "body.note", "ORD-1 ref=a1", true, nil, nil, "body.note = ⟨changes⟩"},
		{"compared whole", "body.note", "ORD-1 ref=a1", true, nil, &void, "body.note = ORD-1 ref=a1 (compared whole: " + whyVoidMerged + ")"},
		{"its slots marked", "body.note", "ORD-1 ref=a1", true, nil, &e, "body.note = ORD-1 ref=⟨changes⟩"},
	} {
		if got := rendered(tc.path, tc.rec, tc.read, notALeaf, tc.noise, tc.e); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Legible lists a mock's learned leaves in order (its words' places, then
// its variables by name, then its leaves learned whole, each sorted), redacts
// every recorded value the recording's obfuscator matches, says a leaf its
// kind's reader does not hold is no leaf (a reader that reads nothing of the
// mock read it), and lists no variable that is not bound at the mock.
func TestLegibleListsInOrderAndRedacts(t *testing.T) {
	const n = 8
	words, vars, whole := map[string]models.WordsEntry{}, models.MockVariables{}, map[string][]string{}
	var wantWords, wantVars, wantWhole []string
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("body.w%d", (i*5)%n)
		words[p] = models.VoidWords("9f2c41d07a3be815")
		wantWords = append(wantWords, p)
		v := fmt.Sprintf("v%d", (i*3)%n)
		vars[v] = models.MockVariable{Recorded: "x", Bind: "body." + v}
		wantVars = append(wantVars, v)
		w := fmt.Sprintf("body.n%d", (i*7)%n)
		whole[w] = nil
		wantWhole = append(wantWhole, w)
	}
	vars["used"] = models.MockVariable{Recorded: "x", Use: []string{"body.used"}}
	sort.Strings(wantWords)
	sort.Strings(wantVars)
	sort.Strings(wantWhole)
	m := &models.Mock{Name: "mock-1", Kind: models.HTTP, Noise: []string{"^s3cr3t-"}, Spec: models.MockSpec{
		Metadata:     map[string]string{"body.note": "s3cr3t-1", "body.id": "s3cr3t-2"},
		LearnedWords: words, Variables: vars, ReqBodyNoise: whole,
	}}
	m.Spec.LearnedWords["body.note"] = learnedWords(t, "s3cr3t-1", "s3cr3t-9")
	m.Spec.ReqBodyNoise["body.id"] = nil
	wantWords = append([]string{"body.note"}, wantWords...)
	wantWhole = append([]string{"body.id"}, wantWhole...)
	withSet(t, []*models.Mock{m})
	leaves, err := Legible(filepath.Join(t.TempDir(), "test-set-0"))
	if err != nil {
		t.Fatal(err)
	}
	var gotWords, gotVars, gotWhole []string
	for _, l := range leaves {
		switch {
		case l.Variable != "":
			gotVars = append(gotVars, l.Variable)
		case l.Rule == models.RuleWholeNoise:
			gotWhole = append(gotWhole, l.Path)
		default:
			gotWords = append(gotWords, l.Path)
		}
		if (l.Path == "body.note" || l.Path == "body.id") && (l.Recorded != redacted || strings.Contains(l.Rendered, "s3cr3t")) {
			t.Errorf("%s holds %q (%q), which the obfuscator matches", l.Path, l.Recorded, l.Rendered)
		}
	}
	if !slices.Equal(gotWords, wantWords) || !slices.Equal(gotVars, wantVars) || !slices.Equal(gotWhole, wantWhole) {
		t.Fatalf("listed\n words %v\n vars %v\n whole %v\nwant\n words %v\n vars %v\n whole %v", gotWords, gotVars, gotWhole, wantWords, wantVars, wantWhole)
	}
	// A reader that reads nothing of a mock read it: each of its leaves is
	// no leaf of the recorded request, never one keploy reads nothing of.
	readersMu.RLock()
	prev := leafReaders[models.HTTP]
	readersMu.RUnlock()
	RegisterLeafReader(models.HTTP, func(*models.Mock) map[string]Leaf { return nil })
	t.Cleanup(func() { RegisterLeafReader(models.HTTP, prev) })
	leaves, err = Legible(filepath.Join(t.TempDir(), "test-set-0"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range leaves {
		if l.Variable == "" && l.Recorded != notALeaf {
			t.Errorf("%s: %q, want %q", l.Path, l.Recorded, notALeaf)
		}
	}
}

// Legible lists the mocks by name, whatever order the store reads them in;
// says so when no store is registered; and says of each leaf what it is: an
// entry this build cannot read and a void one compared whole, an echo of
// this call alone, a variable's words where it binds, its ID of its words'
// digest, and every recorded value the obfuscator matches redacted.
func TestLegibleSaysWhatEachLeafIs(t *testing.T) {
	note := learnedWords(t, "ORD-1 ref=a1", "ORD-1 ref=b2")
	newer := note.Clone()
	newer.V = models.WordsVersion + 1
	vw := learnedWords(t, "o-1111", "o-9999")
	m2 := &models.Mock{Name: "mock-2", Kind: models.HTTP, Noise: []string{"^s3cr3t-"}, Spec: models.MockSpec{
		Metadata:     map[string]string{"body.note": "ORD-1 ref=a1", "body.id": "o-1111", "header.x-ref": "ORD-1 ref=a1"},
		LearnedWords: map[string]models.WordsEntry{"body.note": newer, "header.x-ref": models.VoidWords(note.Digest)},
		Variables: models.MockVariables{
			"echo": {Recorded: "o-1111", Bind: "body.id", Echo: true},
			"word": {Recorded: "o-1111", Bind: "body.w", Words: &vw},
			"sec":  {Recorded: "s3cr3t-3", Bind: "body.s"},
		},
	}}
	m1 := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{ReqBodyNoise: map[string][]string{"body.at": nil}}}
	withSet(t, []*models.Mock{m2, m1})
	set := filepath.Join(t.TempDir(), "test-set-0")
	leaves, err := Legible(set)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) == 0 || leaves[0].Mock != "mock-1" {
		t.Fatalf("leaves %+v, want mock-1's first", leaves)
	}
	by := map[string]models.LearnedLeaf{}
	for _, l := range leaves {
		by[l.Path] = l
	}
	if l := by["body.note"]; len(l.Slots) != 0 || !strings.Contains(l.Why, "is compared whole: its learned words entry is "+models.WordsWrittenByNewer) {
		t.Errorf("an entry of a newer keploy: %+v", l)
	}
	if l := by["header.x-ref"]; !strings.Contains(l.Why, "in two ways, so its learned words are void") {
		t.Errorf("a void entry: %+v", l)
	}
	if l := by["body.id"]; !strings.Contains(l.Why, "of this call alone") {
		t.Errorf("an echo: %+v", l)
	}
	if l := by["body.w"]; l.ID != learnedID("test-set-0", "mock-2", "body.w", vw.Digest) || !strings.Contains(l.Why, "only its marked words may change where it binds") {
		t.Errorf("a variable's words: %+v", l)
	}
	if l := by["body.s"]; l.Recorded != redacted || l.Rendered != "body.s = "+redacted {
		t.Errorf("a variable's recorded value the obfuscator matches: %+v", l)
	}
	RegisterSetReader(nil)
	if _, err := Legible(set); err == nil || !strings.Contains(err.Error(), "no mock store is registered") {
		t.Errorf("no store: %v", err)
	}
}

// A learned file keploy cannot read (here a folder) is an error, never one
// with no leaves; a local file's leaf this run has no value for keeps the
// value the file holds; and with no local value and no learned file the
// copy-back reads nothing, the staging mocks included.
func TestWhatTheLocalFileKeepsAndWhatItCannotRead(t *testing.T) {
	_, set := localSet(t, "ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	two := learnedWords(t, "ORD-2 ref=a1", "ORD-2 ref=b2")
	one := learnedWords(t, "ORD-1 ref=a1", "ORD-1 ref=b2")
	RegisterSetReader(func(string) ([]*models.Mock, error) {
		return []*models.Mock{{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:     map[string]string{"body.note": "ORD-1 ref=a1", "body.id": "ORD-2 ref=a1"},
			LearnedWords: map[string]models.WordsEntry{"body.note": one, "body.id": two}}}}, nil
	})
	if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=b2"}, {Mock: "mock-1", Path: "body.id", F: "ORD-2 ref=b2"}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=c3"}}); err != nil {
		t.Fatal(err)
	}
	local, err := readLearned(filepath.Join(set, models.LearnedLocalFile))
	if err != nil || len(local) != 2 {
		t.Fatalf("local %+v (%v)", local, err)
	}
	for _, l := range local {
		if want := map[string]string{"body.note": "ORD-1 ref=c3", "body.id": "ORD-2 ref=b2"}[l.Path]; l.F != want {
			t.Errorf("%s: F %q, want %q", l.Path, l.F, want)
		}
	}
	stage := filepath.Join(t.TempDir(), "stage", "test-set-0")
	if err := os.MkdirAll(filepath.Join(stage, models.LearnedLocalFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := MergeLocal(set, stage); err == nil {
		t.Error("a staging local file that is a folder merged")
	}
	fresh := filepath.Join(t.TempDir(), "keploy", "test-set-0")
	empty := filepath.Join(t.TempDir(), "stage", "test-set-0")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	failingReader(t)
	if err := MergeLocal(fresh, empty); err != nil {
		t.Errorf("nothing to merge and nothing to prune read the staging mocks: %v", err)
	}
}

// An echo learned before keploy learned words was bound whole, as a variable
// was.
func TestAnEchoBoundBeforeWordsIsBoundWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	lines := RetightenLines([]models.LearnedLeaf{{Mock: "m", Path: "body.id", Rule: models.RuleEcho, Basis: []models.LearnBasis{models.BasisEcho, models.BasisBeforeWords}}}, true)
	if !slices.Equal(lines, []string{"m body.id: re-record to tighten (bound whole, before keploy learned words)"}) {
		t.Fatalf("lines %q", lines)
	}
}

// localSet is a keploy folder with one set whose one mock learned the words
// of body.note, recorded rec; the set reader reads it.
func localSet(t *testing.T, rec, f string, noise []string) (keploy, set string) {
	t.Helper()
	keploy = filepath.Join(t.TempDir(), "keploy")
	set = filepath.Join(keploy, "test-set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	e := learnedWords(t, rec, f)
	mocks := []*models.Mock{{Name: "mock-1", Kind: models.HTTP, Noise: noise, Spec: models.MockSpec{
		Metadata: map[string]string{"body.note": rec}, LearnedWords: map[string]models.WordsEntry{"body.note": e}}}}
	withSet(t, nil)
	RegisterSetReader(func(string) ([]*models.Mock, error) { return mocks, nil })
	t.Setenv(provenswitch.Env, "1")
	return keploy, set
}

// failingReader makes the set reader fail, for the test only.
func failingReader(t *testing.T) {
	t.Helper()
	readersMu.RLock()
	prev := setReader
	readersMu.RUnlock()
	RegisterSetReader(func(string) ([]*models.Mock, error) { return nil, errors.New("the disk went away") })
	t.Cleanup(func() { RegisterSetReader(prev) })
}

// learned.local.yaml holds F's value of a leaf whose recorded value is
// redacted redacted too, starts with the comment that says it stays on this
// machine, and holds each leaf once, a leaf with no value of F left out.
func TestTheLocalFileRedactsWhatTheRecordingRedactsAndSaysItStaysHere(t *testing.T) {
	_, set := localSet(t, "s3cr3t-ORD-1 ref=a1", "s3cr3t-ORD-1 ref=b2", []string{"^s3cr3t-"})
	if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "plain ref=b2"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(set, models.LearnedLocalFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), learnedHeader(models.LearnedLocalFile)) || !strings.Contains(string(b), "it stays on this machine") {
		t.Errorf("learned.local.yaml starts:\n%s", b)
	}
	if strings.Contains(string(b), "plain ref=b2") {
		t.Errorf("F's value of a leaf whose recording is redacted is written:\n%s", b)
	}
	learned, _ := os.ReadFile(filepath.Join(set, models.LearnedFile))
	if strings.Contains(string(learned), "it stays on this machine") || !strings.HasPrefix(string(learned), learnedHeader(models.LearnedFile)) {
		t.Errorf("learned.yaml starts:\n%s", learned)
	}
	// A leaf of current with no value is left out; one ID twice is written
	// once, with its value.
	a := models.LearnedLeaf{ID: "a", Mock: "mock-1", Path: "body.a"}
	z := models.LearnedLeaf{ID: "z", Mock: "mock-1", Path: "body.z"}
	if err := os.Remove(filepath.Join(set, models.LearnedLocalFile)); err != nil {
		t.Fatal(err)
	}
	if err := mergeLocalInto(set, []models.LearnedLeaf{a, z, a}, []models.LearnedLeaf{{ID: "a", F: "fa"}}); err != nil {
		t.Fatal(err)
	}
	local, err := readLearned(filepath.Join(set, models.LearnedLocalFile))
	if err != nil || len(local) != 1 || local[0].ID != "a" || local[0].F != "fa" {
		t.Fatalf("local %+v (%v), want a alone, once", local, err)
	}
}

// A set reader that fails writes no learned file and removes none: Legible's
// error is the write's, never a set with nothing learned.
func TestAFailedReadOfTheMocksWritesAndRemovesNothing(t *testing.T) {
	_, set := localSet(t, "ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=b2"}}); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, name := range []string{models.LearnedFile, models.LearnedLocalFile} {
		before[name], _ = os.ReadFile(filepath.Join(set, name))
	}
	failingReader(t)
	stage := filepath.Join(t.TempDir(), "stage", "test-set-0")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, models.LearnedLocalFile), before[models.LearnedLocalFile], 0o644); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"WriteLearned": WriteLearned(set, nil),
		"WriteLegible": WriteLegible(set),
		"MergeLocal":   MergeLocal(set, stage),
	} {
		if err == nil || !strings.Contains(err.Error(), "the disk went away") {
			t.Errorf("%s: %v, want the read's error", name, err)
		}
	}
	for name, b := range before {
		if now, err := os.ReadFile(filepath.Join(set, name)); err != nil || string(now) != string(b) {
			t.Errorf("%s changed (%v)", name, err)
		}
	}
}

// learned.local.yaml that does not read is an error, and is left as it is.
func TestALocalFileThatDoesNotReadIsLeftAsItIs(t *testing.T) {
	_, set := localSet(t, "ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	path := filepath.Join(set, models.LearnedLocalFile)
	if err := os.WriteFile(path, []byte("learned: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteLearned(set, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=b2"}}); err == nil {
		t.Fatal("a local file that does not read was written over")
	}
	if b, _ := os.ReadFile(path); string(b) != "learned: [\n" {
		t.Fatalf("the local file is now %q", b)
	}
}

// The copy-back writes the staging copy's values into a project set that has
// no learned file yet, prunes a project file whose leaves the staging mocks
// no longer learn though the staging copy has none, and with the switch off
// writes nothing.
func TestTheCopyBackWritesAFreshSetAndPrunesAStaleOne(t *testing.T) {
	_, stage := localSet(t, "ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	if err := WriteLearned(stage, []LocalValue{{Mock: "mock-1", Path: "body.note", F: "ORD-1 ref=b2"}}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "keploy", "test-set-0")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(provenswitch.Env, "")
	if err := MergeLocal(dst, stage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, models.LearnedLocalFile)); !os.IsNotExist(err) {
		t.Fatalf("the switch off wrote a local file (%v)", err)
	}
	t.Setenv(provenswitch.Env, "1")
	if err := MergeLocal(dst, stage); err != nil {
		t.Fatal(err)
	}
	local, err := readLearned(filepath.Join(dst, models.LearnedLocalFile))
	if err != nil || len(local) != 1 || local[0].F != "ORD-1 ref=b2" {
		t.Fatalf("the fresh set's local file %+v (%v)", local, err)
	}
	// The staging copy now holds no local value, and its mocks learn
	// another leaf: the project's entry is pruned.
	if err := os.Remove(filepath.Join(stage, models.LearnedLocalFile)); err != nil {
		t.Fatal(err)
	}
	other := learnedWords(t, "ORD-2 ref=a1", "ORD-2 ref=b2")
	RegisterSetReader(func(string) ([]*models.Mock, error) {
		return []*models.Mock{{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata: map[string]string{"body.id": "ORD-2 ref=a1"}, LearnedWords: map[string]models.WordsEntry{"body.id": other}}}}, nil
	})
	if err := MergeLocal(dst, stage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, models.LearnedLocalFile)); !os.IsNotExist(err) {
		t.Fatalf("the stale local file is kept (%v)", err)
	}
}

// With the switch off WriteLegible writes nothing, and says nothing failed.
func TestWriteLegibleWithTheSwitchOff(t *testing.T) {
	_, set := localSet(t, "ORD-1 ref=a1", "ORD-1 ref=b2", nil)
	t.Setenv(provenswitch.Env, "")
	if err := WriteLegible(set); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(set, models.LearnedFile)); !os.IsNotExist(err) {
		t.Fatalf("the switch off wrote learned.yaml (%v)", err)
	}
}
