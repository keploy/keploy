package testdb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// formCase is a test case with one form file part, a.txt, as form says.
func formCase(name string, form models.FormData) *models.TestCase {
	tc := simpleCase(name)
	tc.HTTPReq.Method = "POST"
	tc.HTTPReq.Form = []models.FormData{form}
	return tc
}

// requireNothingHolds fails when a regular file under root holds secret.
func requireNothingHolds(t *testing.T, root, secret string) {
	t.Helper()
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), secret) {
				t.Errorf("the file outside was copied into the recording at %s", p)
			}
		}
		return nil
	})
}

// A form file part's path a test case names is never a file the test store
// copies into the recording or removes: a test case read back from a set's
// JSON file (keploy normalize, an updated test) or built from the cloud's
// copy (a pull) names one as the repository or the cloud wrote it. The file
// it names, absolute or led to by a relative path out of the recording, is
// neither read into the recording nor removed. An absolute one, where a
// capture leaves a part, with no contents for it, has the test case refused
// with the part named, not stored without the part's file.
func TestAFormFilePathATestCaseNamesIsNeitherCopiedNorRemoved(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		path    func(victim, root string) string
		refused bool
	}{
		{"absolute", func(victim, _ string) string { return victim }, true},
		{"relative out of the recording", func(victim, root string) string {
			rel, err := filepath.Rel(root, victim)
			if err != nil {
				panic(err)
			}
			return rel
		}, false},
	} {
		setup := func(t *testing.T) (root, victim string) {
			base := t.TempDir()
			root = filepath.Join(base, "project", "keploy")
			victim = filepath.Join(base, "outside", "id_rsa")
			if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(victim, []byte("SECRET-OUTSIDE"), 0o600); err != nil {
				t.Fatal(err)
			}
			return root, victim
		}
		check := func(t *testing.T, root, victim string) {
			t.Helper()
			requireFileIs(t, "the file the test case names", victim, "SECRET-OUTSIDE")
			requireNothingHolds(t, root, "SECRET-OUTSIDE")
		}
		// stored says whether the write stored the test case, as c says it
		// must: refused, with the part named, its file name by its size and
		// the path by its folder (the file name is the app's).
		stored := func(t *testing.T, err error, victim string) bool {
			t.Helper()
			if !c.refused {
				if err != nil {
					t.Fatalf("write: %v", err)
				}
				return true
			}
			if err == nil {
				t.Fatal("the test case was stored without its part's file; want it refused")
			}
			for _, want := range []string{`form part "file"`, "(file name withheld, 6 bytes)", shapedFormPath(victim)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %s", err, want)
				}
			}
			return false
		}
		t.Run(c.name+": read back from a set's JSON file", func(t *testing.T) {
			root, victim := setup(t)
			tc := formCase("test-1", models.FormData{Key: "file", FileNames: []string{"id_rsa"}, Paths: []string{c.path(victim, root)}})
			doc, err := EncodeTestcaseJSON(*tc, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "file_names") {
				t.Fatalf("the JSON test case must carry its form file names: %s", data)
			}
			file := filepath.Join(root, "set", "tests", "test-1.json")
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, data, 0o644); err != nil {
				t.Fatal(err)
			}
			db := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON)
			tcs, err := db.GetTestCases(ctx, "set")
			if err != nil || len(tcs) != 1 {
				t.Fatalf("read: %d test cases, %v", len(tcs), err)
			}
			stored(t, db.UpdateTestCase(ctx, tcs[0], "set", false), victim)
			check(t, root, victim)
		})
		for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
			t.Run(c.name+": from the cloud, "+string(format), func(t *testing.T) {
				root, victim := setup(t)
				tc := formCase("test-1", models.FormData{Key: "file", FileNames: []string{"id_rsa"}, Paths: []string{c.path(victim, root)}})
				ok := stored(t, NewWithFormat(zap.NewNop(), root, format).InsertTestCase(ctx, tc, "set", false), victim)
				check(t, root, victim)
				if !ok {
					if files, _ := filepath.Glob(filepath.Join(root, "set", "tests", "*")); len(files) != 0 {
						t.Errorf("a refused test case was stored: %q", files)
					}
					return
				}
				if p := tc.HTTPReq.Form[0].Paths; len(p) != 0 {
					t.Errorf("the stored test case names %q, a file it does not keep", p)
				}
			})
		}
	}
}

// A form file part the capture left in a file (Captured) is kept beside the
// test case and its file removed; read back and written again (keploy
// normalize, an updated test), in either format, the part stays where it is
// kept, with what it held.
func TestACapturedFormFileIsKeptAndStaysKept(t *testing.T) {
	ctx := context.Background()
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "keploy")
			tmp := filepath.Join(t.TempDir(), "keploy_1_a.txt")
			if err := os.WriteFile(tmp, []byte("uploaded"), 0o600); err != nil {
				t.Fatal(err)
			}
			db := NewWithFormat(zap.NewNop(), root, format)
			tc := formCase("test-1", models.FormData{Key: "file", FileNames: []string{"a.txt"}, Paths: []string{tmp}, Captured: []string{tmp}})
			if err := db.InsertTestCase(ctx, tc, "set", false); err != nil {
				t.Fatalf("insert: %v", err)
			}
			kept := filepath.Join("set", "assets", "test-1", "file", "a.txt")
			requireFileIs(t, "the kept part", filepath.Join(root, kept), "uploaded")
			if _, err := os.Stat(tmp); !os.IsNotExist(err) {
				t.Errorf("the captured file is still there: %v", err)
			}
			// The same test case written again, as it is in memory.
			if err := db.UpdateTestCase(ctx, tc, "set", false); err != nil {
				t.Fatalf("update: %v", err)
			}
			for round := 0; round < 2; round++ {
				tcs, err := db.GetTestCases(ctx, "set")
				if err != nil || len(tcs) != 1 {
					t.Fatalf("read: %d test cases, %v", len(tcs), err)
				}
				if p := tcs[0].HTTPReq.Form[0].Paths; len(p) != 1 || p[0] != kept {
					t.Fatalf("round %d: the test case names %q, want [%s]", round, p, kept)
				}
				if err := db.UpdateTestCase(ctx, tcs[0], "set", false); err != nil {
					t.Fatalf("update: %v", err)
				}
				requireFileIs(t, "the kept part", filepath.Join(root, kept), "uploaded")
			}
		})
	}
}

// An absolute path a test case names is not a kept part, as the replay never
// opens one, even where the same path under the recording names a file: with
// no file a capture left and no contents for it, the test case is refused,
// with the part named, and the file is left as it was.
func TestAnAbsoluteFormFilePathIsNotKept(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a path with no volume is not absolute on windows")
	}
	root := filepath.Join(t.TempDir(), "keploy")
	kept := filepath.Join("set", "assets", "test-1", "file", "a.txt")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, kept)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, kept), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	tc := formCase("test-1", models.FormData{Key: "file", FileNames: []string{"a.txt"}, Paths: []string{"/" + kept}})
	err := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON).InsertTestCase(context.Background(), tc, "set", false)
	if err == nil || !strings.Contains(err.Error(), `form part "file"`) {
		t.Fatalf("insert = %v, want the test case refused with its part named", err)
	}
	requireFileIs(t, "the file under the recording", filepath.Join(root, kept), "kept")
	if files, _ := filepath.Glob(filepath.Join(root, "set", "tests", "*")); len(files) != 0 {
		t.Errorf("a refused test case was stored: %q", files)
	}
}

// The refusal above is for a part that names an absolute path. A part that
// names no path at all, and carries nothing to keep its file from, is stored
// as before, without the file: nothing is refused, and saving it reads no
// path the part does not have.
func TestAFormPartWithNoPathIsNotRefused(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	tc := formCase("test-1", models.FormData{Key: "file", FileNames: []string{"a.txt"}})
	var err error
	panicked := func() (p any) {
		defer func() { p = recover() }()
		err = NewWithFormat(zap.NewNop(), root, yaml.FormatJSON).InsertTestCase(context.Background(), tc, "set", false)
		return nil
	}()
	if panicked != nil {
		t.Fatalf("insert panicked: %v", panicked)
	}
	if err != nil {
		t.Fatalf("insert = %v, want the test case stored as before", err)
	}
	if files, _ := filepath.Glob(filepath.Join(root, "set", "tests", "*")); len(files) != 1 {
		t.Errorf("stored %q, want the test case", files)
	}
}

// A path a test case names that is kept inside the recording, but through a
// link that leads out of it, is not kept: the part is not stored with it.
func TestAKeptFormFileThatLinksOutIsNotKept(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join("set", "assets", "test-1", "file", "a.txt")
	linkTo(t, out, filepath.Join(root, kept))
	tc := formCase("test-1", models.FormData{Key: "file", FileNames: []string{"a.txt"}, Paths: []string{kept}})
	if err := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON).InsertTestCase(context.Background(), tc, "set", false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	requireFileIs(t, "the file the link leads to", out, "KEEP ME\n")
	if p := tc.HTTPReq.Form[0].Paths; len(p) != 0 {
		t.Errorf("the stored test case names %q, a file outside the recording", p)
	}
}

// A path a test case names that is a folder inside the recording is not a
// kept part: the replay sends a file part only from a file.
func TestAFormFilePathNamingAFolderIsNotKept(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keploy")
	kept := filepath.Join("set", "assets", "test-1", "file")
	if err := os.MkdirAll(filepath.Join(root, kept), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := formCase("test-1", models.FormData{Key: "file", FileNames: []string{"a.txt"}, Paths: []string{kept}})
	if err := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON).InsertTestCase(context.Background(), tc, "set", false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if p := tc.HTTPReq.Form[0].Paths; len(p) != 0 {
		t.Errorf("the stored test case names %q, a folder", p)
	}
}

// A form file part that names a relative path leading out of the recording,
// or naming no file in it, and carries no contents, costs the part, not the
// test case: the case is stored without the part, the file outside is neither
// read nor removed, and the store keeps the part, with the path it named, for
// the recording's summary. It hands each part over once.
func TestARelativeFormFilePathWithNoFileIsLeftOutAndNamed(t *testing.T) {
	for name, tt := range map[string]struct {
		path    func(base string) string
		outside bool
	}{
		"leads out of the recording": {func(string) string { return filepath.Join("..", "outside.txt") }, true},
		"names no file in it":        {func(string) string { return filepath.Join("set", "assets", "test-1", "file", "a.txt") }, false},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "keploy")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(base, "outside.txt")
			if tt.outside {
				if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			named := tt.path(base)
			tc := formCase("test-1", models.FormData{Key: "file", FileNames: []string{"a.txt"}, Paths: []string{named}})
			core, logs := observer.New(zap.ErrorLevel)
			ts := NewWithFormat(zap.New(core), root, yaml.FormatJSON)
			if err := ts.InsertTestCase(context.Background(), tc, "set", false); err != nil {
				t.Fatalf("insert = %v, want the test case stored without the part", err)
			}
			said := logs.FilterMessageSnippet("stored without a form part's file").All()
			if len(said) != 1 || said[0].ContextMap()["path"] != shapedFormPath(named) || said[0].ContextMap()["key"] != "file" || said[0].ContextMap()["fileName"] != "(file name withheld, 5 bytes)" || said[0].ContextMap()["testCase"] != "test-1" {
				t.Errorf("the store said %v, want one ERROR line naming the test case, the part, its file's size and the path's folder", logs.All())
			}
			if files, _ := filepath.Glob(filepath.Join(root, "set", "tests", "*")); len(files) != 1 {
				t.Fatalf("stored %q, want the test case", files)
			}
			if p := tc.HTTPReq.Form[0].Paths; len(p) != 0 {
				t.Errorf("the stored test case names %q, want no file for the part", p)
			}
			if tt.outside {
				requireFileIs(t, "the file outside the recording", out, "KEEP ME\n")
				requireNothingHolds(t, root, "KEEP ME")
			}
			parts, more := ts.TakeFormPartsLeftOut()
			want := models.FormPartLeftOut{TestSet: "set", TestCase: "test-1", Key: "file", FileName: "a.txt", Path: named}
			if len(parts) != 1 || parts[0] != want || more != 0 {
				t.Fatalf("left out = %+v (and %d more), want only %+v", parts, more, want)
			}
			if s := parts[0].String(); !strings.Contains(s, "set/test-1") || !strings.Contains(s, `form part "file"`) || !strings.Contains(s, "(file name withheld, 5 bytes)") || !strings.Contains(s, "path "+strconv.Quote(shapedFormPath(named))) {
				t.Errorf("the summary's line %q does not name the test case, the part, its file's size and the path's folder", s)
			}
			if parts, more := ts.TakeFormPartsLeftOut(); len(parts) != 0 || more != 0 {
				t.Errorf("a second take = %+v (and %d more), want each part handed over once", parts, more)
			}
		})
	}
}

// A part that names no path is left out the same way, said with no path. A
// part whose file is kept (contents the test case carries, or a file in the
// recording it names) is not in the list.
func TestOnlyAPartWithNoFileIsLeftOut(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keploy")
	kept := filepath.Join("set", "assets", "test-3", "file", "a.txt")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, kept)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, kept), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON)
	for _, tc := range []*models.TestCase{
		formCase("test-1", models.FormData{Key: "file", FileNames: []string{"a.txt"}}),
		formCase("test-2", models.FormData{Key: "file", FileNames: []string{"a.txt"}, FileContents: [][]byte{[]byte("sent")}}),
		formCase("test-3", models.FormData{Key: "file", FileNames: []string{"a.txt"}, Paths: []string{kept}}),
	} {
		if err := ts.InsertTestCase(context.Background(), tc, "set", false); err != nil {
			t.Fatalf("insert %s: %v", tc.Name, err)
		}
	}
	parts, more := ts.TakeFormPartsLeftOut()
	want := models.FormPartLeftOut{TestSet: "set", TestCase: "test-1", Key: "file", FileName: "a.txt"}
	if len(parts) != 1 || parts[0] != want || more != 0 {
		t.Fatalf("left out = %+v (and %d more), want only %+v", parts, more, want)
	}
	if s := parts[0].String(); !strings.Contains(s, "no path") {
		t.Errorf("the summary's line %q does not say the part named no path", s)
	}
}

// The store keeps a bounded list: past it, parts are counted, not held.
func TestTheLeftOutListIsBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := NewWithFormat(zap.NewNop(), root, yaml.FormatJSON)
	names := make([]string, formPartsLeftOutMax+3)
	paths := make([]string, len(names))
	for i := range names {
		names[i] = "f" + strconv.Itoa(i) + ".txt"
		paths[i] = filepath.Join("..", names[i])
	}
	tc := formCase("test-1", models.FormData{Key: "file", FileNames: names, Paths: paths})
	if err := ts.InsertTestCase(context.Background(), tc, "set", false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	parts, more := ts.TakeFormPartsLeftOut()
	if len(parts) != formPartsLeftOutMax || more != 3 {
		t.Fatalf("left out %d parts and %d more, want %d and 3", len(parts), more, formPartsLeftOutMax)
	}
	for i, got := range parts {
		if got.Path != paths[i] {
			t.Fatalf("part %d is listed with the path %q, want its own, %q", i, got.Path, paths[i])
		}
	}
	if parts[0].FileName != "f0.txt" || parts[len(parts)-1].FileName != "f"+strconv.Itoa(formPartsLeftOutMax-1)+".txt" {
		t.Errorf("the list holds %q..%q, want the first parts in order", parts[0].FileName, parts[len(parts)-1].FileName)
	}
	if parts, more := ts.TakeFormPartsLeftOut(); len(parts) != 0 || more != 0 {
		t.Errorf("a second take = %d parts and %d more, want the count handed over once", len(parts), more)
	}
}

// shapedFormPath is a form part's file path as the store's lines name it:
// its folder, and its file name by its size.
func shapedFormPath(p string) string {
	dir, base := filepath.Split(p)
	return dir + "(file name withheld, " + strconv.Itoa(len(base)) + " bytes)"
}

// A form part's file name is the app's (multipart's filename=), and so is a
// path that ends in it: the store's ERROR line, the recording summary's
// line, the refusal's error and a failed write's line and error name the
// part by its key, its file name by its size and a path by its folder.
func TestAFormPartsFileNameIsNamedByItsSize(t *testing.T) {
	const s = logleak.Canary
	name := "cv-" + s + ".pdf"
	root := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	logger, logs := logleak.Observe()
	ts := NewWithFormat(logger, root, yaml.FormatJSON)
	ctx := context.Background()

	// Left out: a relative path that names no file in the recording.
	named := filepath.Join("set", "assets", "test-1", "file", name)
	if err := ts.InsertTestCase(ctx, formCase("test-1", models.FormData{Key: "file", FileNames: []string{name}, Paths: []string{named}}), "set", false); err != nil {
		t.Fatalf("insert = %v, want the test case stored without the part", err)
	}
	parts, _ := ts.TakeFormPartsLeftOut()
	if len(parts) != 1 || logleak.Holds(parts[0].String(), s) || !strings.Contains(parts[0].String(), `form part "file"`) {
		t.Errorf("the summary's lines %q hold the file's name, or do not name the part", parts)
	}
	// Refused: an absolute path no capture of this run left.
	if runtime.GOOS != "windows" {
		err := ts.InsertTestCase(ctx, formCase("test-2", models.FormData{Key: "file", FileNames: []string{name}, Paths: []string{"/tmp/up/" + name}}), "set", false)
		if err == nil || logleak.Holds(err.Error(), s) || !strings.Contains(err.Error(), `form part "file"`) || !strings.Contains(err.Error(), shapedFormPath("/tmp/up/"+name)) {
			t.Errorf("insert = %v, want the refusal to name the part and the path's folder, not the file's name", err)
		}
	}
	// A write that fails: a folder stands where the part's file goes.
	if err := os.MkdirAll(filepath.Join(root, "set", "assets", "test-3", "file", name), 0o755); err != nil {
		t.Fatal(err)
	}
	err := ts.InsertTestCase(ctx, formCase("test-3", models.FormData{Key: "file", FileNames: []string{name}, FileContents: [][]byte{[]byte("x")}}), "set", false)
	if err == nil || logleak.Holds(err.Error(), s) {
		t.Errorf("insert = %v, want the write's failure without the file's name", err)
	}
	logleak.Check(t, logs, s, "the test store's form part lines")
}
