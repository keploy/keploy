package contract

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// keptBodyPath is where the testdb keeps the request body of test-1 of
// test-set-0, relative to the recording's directory.
var keptBodyPath = filepath.Join("test-set-0", "assets", "test-1", "body.txt")

// recordLargeBody records, in a real testdb at <tmp>/keploy, test-1 of
// test-set-0: a POST /users whose 2 MiB JSON body has the field "inside". The
// testdb keeps a body over 1 MB in a file apart from the test case, under
// assets/ (keptBodyPath). It also writes, outside the recording, a JSON body
// with the field "outsideSecret" at <tmp>/outside.json and at
// <tmp>/outside/body.txt. It returns the testdb and tmp.
func recordLargeBody(t *testing.T) (*testdb.TestYaml, string) {
	t.Helper()
	tmp := t.TempDir()
	db := testdb.New(zap.NewNop(), filepath.Join(tmp, "keploy"))
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	tc := &models.TestCase{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Name:    "test-1",
		HTTPReq: models.HTTPReq{
			Method: models.Method(http.MethodPost), ProtoMajor: 1, ProtoMinor: 1,
			URL:       "http://localhost:8080/users",
			Header:    map[string]string{"Content-Type": "application/json"},
			Body:      `{"inside":"` + strings.Repeat("x", 2<<20) + `"}`,
			Timestamp: at,
		},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"id":1}`, Timestamp: at.Add(time.Millisecond)},
	}
	if err := db.InsertTestCase(context.Background(), tc, "test-set-0", false); err != nil {
		t.Fatalf("InsertTestCase: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "keploy", keptBodyPath)); err != nil {
		t.Fatalf("the testdb kept no body apart from the test case: %v", err)
	}
	outside := []byte(`{"outsideSecret":"not the recording's"}`)
	if err := os.WriteFile(filepath.Join(tmp, "outside.json"), outside, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "outside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "outside", "body.txt"), outside, 0o600); err != nil {
		t.Fatal(err)
	}
	return db, tmp
}

// bodyPathDB is a testdb whose test cases name path as the file their request
// body is kept in, as a recording edited by hand, or written by another tool,
// may.
type bodyPathDB struct {
	*testdb.TestYaml
	path string
}

func (d bodyPathDB) GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error) {
	tcs, err := d.TestYaml.GetTestCases(ctx, testSetID)
	for _, tc := range tcs {
		if tc != nil && tc.HTTPReq.BodyRef.Path != "" {
			tc.HTTPReq.BodyRef.Path = d.path
		}
	}
	return tcs, err
}

// generateFromTests runs GenerateFromTests on the recording at <tmp>/keploy
// and returns the contract it wrote and what it logged.
func generateFromTests(t *testing.T, db TestDB, tmp string) (string, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	s := New(zap.New(core), db, nil, nil, &config.Config{Path: tmp})
	if err := s.GenerateFromTests(context.Background()); err != nil {
		t.Fatalf("GenerateFromTests: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(tmp, "keploy", "contract.yaml"))
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	return string(out), logs
}

// link replaces the file or directory at <tmp>/keploy/<at> with a symbolic
// link to target. The test is skipped where no symbolic link can be made.
func link(t *testing.T, tmp, at, target string) {
	t.Helper()
	at = filepath.Join(tmp, "keploy", at)
	if err := os.RemoveAll(at); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Skipf("no symbolic link: %v", err)
	}
}

// The contract inferred from a recording describes a request body the testdb
// kept in a file apart from its test case, and one kept as a symbolic link
// to a file that is still inside the recording.
func TestTheContractDescribesARequestBodyKeptApartFromItsTestCase(t *testing.T) {
	for _, c := range []struct {
		name string
		// at changes the recording at <tmp>/keploy, if at all, and returns
		// the field the contract must then describe.
		at func(t *testing.T, tmp string) string
	}{
		{"as the testdb keeps it", func(t *testing.T, tmp string) string { return "inside:" }},
		{"a symbolic link to a file inside the recording", func(t *testing.T, tmp string) string {
			other := filepath.Join(tmp, "keploy", "test-set-0", "assets", "test-2")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, "body.txt"), []byte(`{"linkedInside":"in the recording"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			link(t, tmp, keptBodyPath, filepath.Join("..", "test-2", "body.txt"))
			return "linkedInside:"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, tmp := recordLargeBody(t)
			field := c.at(t, tmp)
			contract, logs := generateFromTests(t, db, tmp)
			if !strings.Contains(contract, field) {
				t.Fatalf("the contract has no %q of the kept request body:\n%.2000s", field, contract)
			}
			for _, e := range logs.All() {
				if e.Level >= zapcore.WarnLevel {
					t.Fatalf("the contract warned of a body it read: %s %v", e.Message, e.ContextMap())
				}
			}
		})
	}
}

// A stored test case cannot make the contract read a file outside its
// recording: a body kept as a symbolic link out of the recording, whether
// the link's target is absolute or relative, kept in a directory that is
// such a link, named by an absolute path, or by a path up and out of it is
// refused. The user is warned of it, with the test set, the test case and
// why, and the contract is inferred without that body.
func TestTheContractReadsNoRequestBodyOutsideItsRecording(t *testing.T) {
	for _, c := range []struct {
		name string
		// at makes the recording at <tmp>/keploy name a file outside it and
		// returns the testdb to read it with and the path its test case names.
		at func(t *testing.T, db *testdb.TestYaml, tmp string) (TestDB, string)
	}{
		{"a symbolic link out of the recording", func(t *testing.T, db *testdb.TestYaml, tmp string) (TestDB, string) {
			link(t, tmp, keptBodyPath, filepath.Join(tmp, "outside.json"))
			return db, keptBodyPath
		}},
		{"a directory linked out of the recording", func(t *testing.T, db *testdb.TestYaml, tmp string) (TestDB, string) {
			link(t, tmp, filepath.Dir(keptBodyPath), filepath.Join(tmp, "outside"))
			return db, keptBodyPath
		}},
		// A relative link climbs out of the recording wherever the
		// recording is copied to, such as a clone of a repository.
		{"a relative symbolic link up and out of the recording", func(t *testing.T, db *testdb.TestYaml, tmp string) (TestDB, string) {
			link(t, tmp, keptBodyPath, filepath.Join("..", "..", "..", "..", "outside.json"))
			return db, keptBodyPath
		}},
		{"a directory linked relatively up and out of the recording", func(t *testing.T, db *testdb.TestYaml, tmp string) (TestDB, string) {
			link(t, tmp, filepath.Dir(keptBodyPath), filepath.Join("..", "..", "..", "outside"))
			return db, keptBodyPath
		}},
		{"an absolute path", func(t *testing.T, db *testdb.TestYaml, tmp string) (TestDB, string) {
			path := filepath.Join(tmp, "outside.json")
			return bodyPathDB{db, path}, path
		}},
		{"up and out of the recording", func(t *testing.T, db *testdb.TestYaml, tmp string) (TestDB, string) {
			path := filepath.Join("..", "outside.json")
			return bodyPathDB{db, path}, path
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, tmp := recordLargeBody(t)
			read, path := c.at(t, db, tmp)
			contract, logs := generateFromTests(t, read, tmp)
			if strings.Contains(contract, "outsideSecret") {
				t.Fatalf("the contract describes the file outside the recording:\n%.2000s", contract)
			}
			if strings.Contains(contract, "inside:") {
				t.Fatalf("the contract describes the request body it was not given:\n%.2000s", contract)
			}
			if !strings.Contains(contract, "/users") {
				t.Fatalf("the contract left out the test case:\n%.2000s", contract)
			}
			// The warning names the test set and the test case, and os.Root
			// says why: the path escapes the recording's directory.
			said := false
			for _, e := range logs.All() {
				fields := e.ContextMap()
				msg, _ := fields["error"].(string)
				if e.Level >= zapcore.WarnLevel && fields["testSetID"] == "test-set-0" && fields["testCase"] == "test-1" &&
					strings.Contains(msg, path) && strings.Contains(msg, "is read only inside its recording") &&
					strings.Contains(msg, "path escapes from parent") {
					said = true
				}
			}
			if !said {
				t.Fatalf("no warning named test-1 of test-set-0 and said %q was refused as escaping the recording; logged:\n%v", path, logs.All())
			}
		})
	}
}
