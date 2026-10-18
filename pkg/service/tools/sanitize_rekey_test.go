package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// fileRekeyer names a recording by its file's bytes and keeps every re-key.
type fileRekeyer struct {
	mu    sync.Mutex
	calls [][2]string
}

func (f *fileRekeyer) Hash(_ string, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:]), nil
}

func (f *fileRekeyer) Rekey(_, _, _, old, newHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]string{old, newHash})
	return nil
}

// movedTo follows an entry naming from through every re-key.
func (f *fileRekeyer) movedTo(from string) (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, moves := from, 0
	for _, c := range f.calls {
		if c[0] == cur {
			cur, moves = c[1], moves+1
		}
	}
	return cur, moves
}

// keploy sanitize and desanitize rewrite a set's recordings where they lie,
// outside testdb, and keploy normalize of a sanitized set desanitizes it,
// rewrites the test and sanitizes it again: an entry of verdicts.yaml that
// named a recording follows it to the recording now on disk (row 2d,
// round 1), sanitize and desanitize each with one re-key per test.
func TestSanitizeDesanitizeAndNormalizeOfASanitizedSetReKeyTheEntry(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ctx := context.Background()
	for _, step := range []string{"sanitize", "desanitize", "normalize's steps"} {
		t.Run(step, func(t *testing.T) {
			r := &fileRekeyer{}
			testdb.SetRekeyer(r)
			t.Cleanup(func() { testdb.SetRekeyer(nil) })
			root := t.TempDir()
			db := testdb.New(zap.NewNop(), root)
			tc := &models.TestCase{Name: "test-1", Kind: models.HTTP, Version: models.GetVersion(),
				HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://api.test/v1/items", Header: map[string]string{"Authorization": "Bearer ghp_R8xQ2mT5vK9pL3nW7yZ1aB4cD6eF0gH2jK"}},
				HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"v":"old"}`}}
			if err := db.InsertTestCase(ctx, tc, "test-set-0", false); err != nil {
				t.Fatal(err)
			}
			tools := &Tools{logger: zap.NewNop()}
			set := filepath.Join(root, "test-set-0")
			path := filepath.Join(set, "tests", "test-1.yaml")
			if step != "sanitize" {
				if err := tools.SanitizeTestSetDir(ctx, set); err != nil {
					t.Fatal(err)
				}
			}
			entry, err := r.Hash(root, path)
			if err != nil {
				t.Fatal(err)
			}
			r.calls = nil
			switch step {
			case "sanitize":
				if err := tools.SanitizeTestSetDir(ctx, set); err != nil {
					t.Fatal(err)
				}
			case "desanitize":
				if done, err := tools.DesanitizeTestSet("test-set-0", root); err != nil || !done {
					t.Fatalf("DesanitizeTestSet: %v, %v", done, err)
				}
			default:
				if _, err := tools.DesanitizeTestSet("test-set-0", root); err != nil {
					t.Fatal(err)
				}
				got, err := db.GetTestCase(ctx, "test-set-0", "test-1")
				if err != nil {
					t.Fatal(err)
				}
				got.HTTPResp.Body = `{"v":"new"}`
				if err := db.UpdateTestCase(ctx, got, "test-set-0", false); err != nil {
					t.Fatal(err)
				}
				if err := tools.SanitizeTestSetDir(ctx, set); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if sanitized := strings.Contains(string(raw), ".secret."); sanitized == (step == "desanitize") {
				t.Fatalf("precondition: sanitized %v after %s: %s", sanitized, step, raw)
			}
			now, err := r.Hash(root, path)
			if err != nil {
				t.Fatal(err)
			}
			if now == entry {
				t.Fatal("precondition: the recording did not change")
			}
			at, moves := r.movedTo(entry)
			if at != now {
				t.Fatalf("the entry naming %s is on %s, not on the recording now on disk (%s); re-keys %v", entry, at, now, r.calls)
			}
			if step != "normalize's steps" && moves != 1 {
				t.Fatalf("%s re-keyed the entry %d times, want once: %v", step, moves, r.calls)
			}
		})
	}
}

// rekeyReportDB is the report of a run in which test-1 failed with answer.
type rekeyReportDB struct{ answer string }

func (rekeyReportDB) GetAllTestRunIDs(context.Context) ([]string, error) {
	return []string{"test-run-0"}, nil
}

func (r rekeyReportDB) GetReport(context.Context, string, string) (*models.TestReport, error) {
	return &models.TestReport{Tests: []models.TestResult{{
		TestCaseID: "test-1", Status: models.TestStatusFailed,
		Res: models.HTTPResp{StatusCode: 200, Body: r.answer},
	}}}, nil
}

// keploy normalize of a sanitized set (normalizeTestSet) desanitizes it,
// writes the app's new answer and sanitizes it again, and re-keys once, when
// it is done, from the sanitized recording the entry named to the sanitized
// one now on disk: no entry ever names the desanitized recording between.
func TestNormalizeOfASanitizedSetReKeysOnceFromTheSanitizedRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ctx := context.Background()
	r := &fileRekeyer{}
	testdb.SetRekeyer(r)
	t.Cleanup(func() { testdb.SetRekeyer(nil) })
	root := t.TempDir()
	db := testdb.New(zap.NewNop(), root)
	tc := &models.TestCase{Name: "test-1", Kind: models.HTTP, Version: models.GetVersion(),
		HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://api.test/v1/items", Header: map[string]string{"Authorization": "Bearer ghp_R8xQ2mT5vK9pL3nW7yZ1aB4cD6eF0gH2jK"}},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"v":"old"}`}}
	if err := db.InsertTestCase(ctx, tc, "test-set-0", false); err != nil {
		t.Fatal(err)
	}
	tools := &Tools{logger: zap.NewNop(), testDB: db, reportDB: rekeyReportDB{answer: `{"v":"new"}`}, config: &config.Config{Path: root}}
	set := filepath.Join(root, "test-set-0")
	path := filepath.Join(set, "tests", "test-1.yaml")
	if err := tools.SanitizeTestSetDir(ctx, set); err != nil {
		t.Fatal(err)
	}
	entry, err := r.Hash(root, path)
	if err != nil {
		t.Fatal(err)
	}
	r.calls = nil

	if err := tools.normalizeTestSet(ctx, "test-run-0", config.SelectedTests{TestSet: "test-set-0"}); err != nil {
		t.Fatalf("normalizeTestSet: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), ".secret.") || !strings.Contains(string(raw), `new`) {
		t.Fatalf("precondition: the set is not normalized and sanitized again: %s", raw)
	}
	now, err := r.Hash(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0] != [2]string{entry, now} {
		t.Fatalf("re-keyed %v, want once from %s to %s", r.calls, entry, now)
	}
}
