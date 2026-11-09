package testdb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
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
		// must: refused, with the part and the path named.
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
			for _, want := range []string{`form part "file"`, `"id_rsa"`, victim} {
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
