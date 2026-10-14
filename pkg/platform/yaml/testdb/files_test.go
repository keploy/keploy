package testdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// TestCaseFiles returns the files GetTestCases reads and no other: of two
// files of one name the one in the store's format, never a mocks file, a file
// of another extension or an empty one, in test case order.
func TestTestCaseFiles_AreTheFilesGetTestCasesReads(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	yamlDB, jsonDB := NewWithFormat(zap.NewNop(), dir, yamlLib.FormatYAML), NewWithFormat(zap.NewNop(), dir, yamlLib.FormatJSON)
	for _, name := range []string{"test-10", "test-2", "test-1"} {
		tc := namedTC(name, "http://h/"+name)
		tc.Noise = map[string][]string{}
		if err := yamlDB.UpdateTestCase(ctx, tc, "set", false); err != nil {
			t.Fatal(err)
		}
	}
	// A JSON sibling of test-2, and a test case stored as JSON only.
	for _, name := range []string{"test-2", "test-3"} {
		tc := namedTC(name, "http://h/json/"+name)
		tc.Noise = map[string][]string{}
		if err := jsonDB.UpdateTestCase(ctx, tc, "set", false); err != nil {
			t.Fatal(err)
		}
	}
	tests := filepath.Join(dir, "set", "tests")
	for name, text := range map[string]string{"mocks.yaml": "kind: Http\n", "notes.txt": "{{string .id}}", "test-4.yaml": "", "test-5.yaml.tmp": "x"} {
		if err := os.WriteFile(filepath.Join(tests, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	names := func(files []yamlLib.File) []string {
		var out []string
		for _, f := range files {
			out = append(out, filepath.Base(f.Path))
			onDisk, err := os.ReadFile(f.Path)
			if err != nil || string(onDisk) != string(f.Data) {
				t.Fatalf("%s: the bytes returned are not the file's (%v)", f.Path, err)
			}
		}
		return out
	}
	files, err := yamlDB.TestCaseFiles(ctx, "set")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(files), []string{"test-1.yaml", "test-2.yaml", "test-3.json", "test-10.yaml"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the yaml store's files = %v, want %v", got, want)
	}
	files, err = jsonDB.TestCaseFiles(ctx, "set")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(files), []string{"test-1.yaml", "test-2.json", "test-3.json", "test-10.yaml"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the json store's files = %v, want %v", got, want)
	}
	// As many as GetTestCases reads.
	tcs, err := yamlDB.GetTestCases(ctx, "set")
	if err != nil {
		t.Fatal(err)
	}
	if len(tcs) != len(files) {
		t.Fatalf("GetTestCases read %d test cases, TestCaseFiles returned %d files", len(tcs), len(files))
	}
	// A set with no tests directory has no file.
	if files, err := yamlDB.TestCaseFiles(ctx, "no-such-set"); err != nil || len(files) != 0 {
		t.Fatalf("a set that does not exist: %v, %v", files, err)
	}
}

// What an interrupted ReplaceFiles left in the set, beside a test case or
// beside config.yaml, fails the read: the set's files are then not what one
// writer wrote. The error names each file and the steps for what was left: a
// file's new bytes still waiting mean the run had not put every file in
// place, and the copies put the set back; only copies left mean it had, and
// they are deleted. Interrupted, which keploy test asks, says the same.
func TestTestCaseFiles_FailOnWhatAnInterruptedReplaceLeft(t *testing.T) {
	for _, c := range []struct {
		left  []string
		steps string
	}{
		{[]string{"tests/test-1.yaml" + yamlLib.ReplaceOldSuffix}, "it had put every file in place and was removing the copies it kept, so the files are as that run wrote them: delete tests/test-1.yaml.keploy-old; then run the command again"},
		{[]string{"config.yaml" + yamlLib.ReplaceOldSuffix}, "so the files are as that run wrote them: delete config.yaml.keploy-old; then"},
		{[]string{"tests/test-1.yaml" + yamlLib.ReplaceNewSuffix}, "it had not put every file in place, so put them back as they were (or restore them from version control): delete tests/test-1.yaml.keploy-new; then run the command again"},
		{[]string{"tests/test-1.yaml" + yamlLib.ReplaceOldSuffix, "tests/test-2.yaml" + yamlLib.ReplaceNewSuffix, "tests/test-2.yaml" + yamlLib.ReplaceOldSuffix},
			"it had not put every file in place, so put them back as they were (or restore them from version control): rename tests/test-1.yaml.keploy-old to tests/test-1.yaml; rename tests/test-2.yaml.keploy-old to tests/test-2.yaml; delete tests/test-2.yaml.keploy-new; then"},
	} {
		t.Run(strings.Join(c.left, ","), func(t *testing.T) {
			dir := t.TempDir()
			db := New(zap.NewNop(), dir)
			tc := namedTC("test-1", "http://h/x")
			tc.Noise = map[string][]string{}
			if err := db.UpdateTestCase(context.Background(), tc, "set", false); err != nil {
				t.Fatal(err)
			}
			if err := db.Interrupted(context.Background(), "set"); err != nil {
				t.Fatalf("a set nothing interrupted: %v", err)
			}
			for _, left := range c.left {
				if err := os.WriteFile(filepath.Join(dir, "set", filepath.FromSlash(left)), []byte("as it was"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := db.TestCaseFiles(context.Background(), "set")
			if !errors.Is(err, yamlLib.ErrReplaceInterrupted) {
				t.Fatalf("error %v, want ErrReplaceInterrupted", err)
			}
			for _, want := range []string{"in test set set (" + strings.Join(c.left, ", ") + ")", c.steps} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q, want it to say %q", err, want)
				}
			}
			// keploy test asks Interrupted, and is told the same.
			if again := db.Interrupted(context.Background(), "set"); again == nil || again.Error() != err.Error() {
				t.Fatalf("Interrupted says %v, want %v", again, err)
			}
		})
	}
}

// TestCaseFile returns, byte for byte, the file UpdateTestCase writes for a
// test case whose assets are in place (an uploaded file named by its path),
// and writes nothing itself.
func TestTestCaseFile_IsTheFileUpdateTestCaseWrites(t *testing.T) {
	for _, format := range []yamlLib.Format{yamlLib.FormatYAML, yamlLib.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			db := NewWithFormat(zap.NewNop(), dir, format)
			newTC := func() *models.TestCase {
				tc := namedTC("test-1", "http://h/users/{{string .id}}")
				tc.Noise = map[string][]string{"header.Date": {}}
				tc.HTTPReq.Header = map[string]string{"X-Id": "{{string .id}}"}
				tc.HTTPResp = models.HTTPResp{StatusCode: 201, Header: map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/"}, Body: `{"id":"{{capture string .id}}"}`}
				tc.HTTPReq.Form = []models.FormData{{Key: "note", Values: []string{"n"}}, {Key: "file", Paths: []string{"set/assets/test-1/file/a.png"}}}
				return tc
			}
			file, err := db.TestCaseFile(context.Background(), newTC(), "set")
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(dir, "set", "tests", "test-1."+format.FileExtension())
			if file.Path != want {
				t.Fatalf("path %s, want %s", file.Path, want)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "set")); !os.IsNotExist(statErr) {
				t.Fatalf("TestCaseFile wrote into the set: %v", statErr)
			}
			if err := db.UpdateTestCase(context.Background(), newTC(), "set", false); err != nil {
				t.Fatal(err)
			}
			onDisk, err := os.ReadFile(want)
			if err != nil {
				t.Fatal(err)
			}
			if string(onDisk) != string(file.Data) {
				t.Fatalf("TestCaseFile returned\n%s\nUpdateTestCase wrote\n%s", file.Data, onDisk)
			}
		})
	}
	db := New(zap.NewNop(), t.TempDir())
	if _, err := db.TestCaseFile(context.Background(), httpTC("GET", "http://h/x"), "set"); err == nil {
		t.Fatal("a test case without a name has no file, and got one")
	}
	if _, err := db.TestCaseFile(context.Background(), namedTC("../escape", "http://h/x"), "set"); err == nil {
		t.Fatal("a name that leaves the set got a file")
	}
	if _, err := db.TestCaseFile(context.Background(), namedTC("test-1", "http://h/x"), "../set"); err == nil {
		t.Fatal("a set that leaves the keploy directory got a file")
	}
}

// TestCaseFile ENCODES A TEST CASE AS IT IS GIVEN AND TOUCHES NO ASSET. The
// recorder's asset step, which UpdateTestCase runs first, moves a request
// body over LargeBodyThreshold into the assets directory (where a replay
// sends it as it is, rendering no placeholder in it) and drops a response
// body of that size. A test case keploy templatize rewrites was read back
// from its set: bodies of that size, as an older keploy stored them or a
// person wrote them, stay where they are, and so does the path of an asset.
func TestTestCaseFile_TouchesNoAsset(t *testing.T) {
	for _, format := range []yamlLib.Format{yamlLib.FormatYAML, yamlLib.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			db := NewWithFormat(zap.NewNop(), dir, format)
			big := `{"id":"{{string .id}}","pad":"` + strings.Repeat("a", LargeBodyThreshold) + `"}`
			tc := namedTC("test-1", "http://h/upload")
			tc.Noise = map[string][]string{}
			tc.HTTPReq.Body = big
			tc.HTTPReq.Form = []models.FormData{{Key: "file", Paths: []string{"set/assets/test-1/file/a.png"}}}
			tc.HTTPResp = models.HTTPResp{StatusCode: 200, Body: big}

			file, err := db.TestCaseFile(ctx, tc, "set")
			if err != nil {
				t.Fatal(err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "set")); !os.IsNotExist(statErr) {
				t.Fatalf("TestCaseFile wrote into the set: %v", statErr)
			}
			if tc.HTTPReq.Body != big || tc.HTTPReq.BodyRef.Path != "" || tc.HTTPResp.Body != big || tc.HTTPResp.BodySkipped {
				t.Fatal("TestCaseFile changed the test case it was given")
			}

			// The file, put in place, reads back as the test case given.
			if err := os.MkdirAll(filepath.Dir(file.Path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file.Path, file.Data, 0o644); err != nil {
				t.Fatal(err)
			}
			tcs, err := db.GetTestCases(ctx, "set")
			if err != nil || len(tcs) != 1 {
				t.Fatalf("read back %d test cases: %v", len(tcs), err)
			}
			got := tcs[0]
			if got.HTTPReq.Body != big || got.HTTPReq.BodyRef.Path != "" {
				t.Fatalf("the request body was not kept in the file: %d bytes, body_ref %q", len(got.HTTPReq.Body), got.HTTPReq.BodyRef.Path)
			}
			if got.HTTPResp.Body != big || got.HTTPResp.BodySkipped {
				t.Fatalf("the response body was not kept in the file: %d bytes, skipped %v", len(got.HTTPResp.Body), got.HTTPResp.BodySkipped)
			}
			if len(got.HTTPReq.Form) != 1 || !reflect.DeepEqual(got.HTTPReq.Form[0].Paths, []string{"set/assets/test-1/file/a.png"}) {
				t.Fatalf("the uploaded file's path was not kept: %+v", got.HTTPReq.Form)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "set", "assets")); !os.IsNotExist(statErr) {
				t.Fatalf("an assets directory was written: %v", statErr)
			}
		})
	}
}

// ReplaceFiles puts a group of the set's files in place together, and an
// error means none is: the test case a failed group had replaced is back as
// it was.
func TestReplaceFiles_ReplacesTheSetsFilesTogether(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db := New(zap.NewNop(), dir)
	for _, name := range []string{"test-1", "test-2"} {
		tc := namedTC(name, "http://h/"+name)
		tc.Noise = map[string][]string{}
		if err := db.UpdateTestCase(ctx, tc, "set", false); err != nil {
			t.Fatal(err)
		}
	}
	before, err := db.TestCaseFiles(ctx, "set")
	if err != nil {
		t.Fatal(err)
	}
	var group []yamlLib.File
	for _, name := range []string{"test-1", "test-2"} {
		tc := namedTC(name, "http://h/changed/"+name)
		tc.Noise = map[string][]string{}
		f, err := db.TestCaseFile(ctx, tc, "set")
		if err != nil {
			t.Fatal(err)
		}
		group = append(group, f)
	}

	prev := replaceFiles
	t.Cleanup(func() { replaceFiles = prev })
	replaceFiles = func(logger *zap.Logger, files []yamlLib.File) error {
		return yamlLib.ReplaceFilesWith(logger, yamlLib.ReplaceHooks{Rename: func(from, to string) error {
			if strings.HasSuffix(from, "test-2.yaml"+yamlLib.ReplaceNewSuffix) {
				return &os.LinkError{Op: "rename", Old: from, New: to, Err: errSharingViolation}
			}
			return os.Rename(from, to)
		}}, files)
	}
	if err := db.ReplaceFiles(ctx, group); err == nil {
		t.Fatal("no error from a group whose second file cannot be put in place")
	}
	after, err := db.TestCaseFiles(ctx, "set")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a failed group changed the set:\n%v\n%v", before, after)
	}

	replaceFiles = prev
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.ReplaceFiles(cancelled, group); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled run: %v", err)
	}
	if err := db.ReplaceFiles(ctx, group); err != nil {
		t.Fatal(err)
	}
	after, err = db.TestCaseFiles(ctx, "set")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(group, after) {
		t.Fatalf("the set's files are not the group's:\n%v\n%v", group, after)
	}
}
