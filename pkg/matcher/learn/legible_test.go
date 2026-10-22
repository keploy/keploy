package learn

import (
	"os"
	"path/filepath"
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
	prevSet, prevLeaf := setReader, leafReaders[models.HTTP]
	readersMu.RUnlock()
	RegisterSetReader(func(string) ([]*models.Mock, error) { return mocks, nil })
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
	withSet(t, []*models.Mock{m})
	leaves, err := Legible(filepath.Join(t.TempDir(), "test-set-0"))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]models.LearnedLeaf{}
	for _, l := range leaves {
		by[l.Path+"/"+l.Variable] = l
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
	if v := by["url.query.old/old"]; v.Rule != models.RuleVariable || len(v.Slots) != 0 || !strings.Contains(v.Why, "re-record to tighten") {
		t.Errorf("a variable learned before words: %+v", v)
	}
	if w := by["body.at/"]; w.Rule != models.RuleWholeNoise || w.Basis[0] != models.BasisWhole {
		t.Errorf("whole noise: %+v", w)
	}
	again, _ := Legible(filepath.Join(t.TempDir(), "test-set-0"))
	for i := range leaves {
		if leaves[i].ID != again[i].ID || leaves[i].Path != again[i].Path {
			t.Fatalf("Legible read the same mocks otherwise: %+v, %+v", leaves[i], again[i])
		}
	}
	t.Setenv(provenswitch.Env, "1")
	lines := RetightenLines(leaves, true)
	if len(lines) != 2 || RetightenLines(leaves, false) != nil {
		t.Errorf("N6 lines %v", lines)
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
	if err := MergeLocal(set, stage); err != nil {
		t.Fatal(err)
	}
	merged, _ := readLearned(filepath.Join(set, models.LearnedLocalFile))
	if len(merged) != 1 || merged[0].F != "ORD-1 ref=c3" {
		t.Fatalf("merged %+v", merged)
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
