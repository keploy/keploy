package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
)

// stepInstr is an agent connection that accepts every call a step makes.
type stepInstr struct{ setupInstr }

func (*stepInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) { return nil, nil }

// bodyApp is an app that keeps the bodies of the POSTs it is sent.
type bodyApp struct {
	mu     sync.Mutex
	bodies []string
}

func (a *bodyApp) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			a.mu.Lock()
			a.bodies = append(a.bodies, string(b))
			a.mu.Unlock()
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (a *bodyApp) received() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.bodies...)
}

// recordedPost stores test-1, a POST of body, in set test-set-0 of the testdb
// at root, as a recording does; ref, when set, is the body's stored path.
func recordedPost(t *testing.T, db *testdb.TestYaml, body string, ref models.BodyRef) {
	t.Helper()
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	tc := &models.TestCase{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Name:    "test-1",
		HTTPReq: models.HTTPReq{
			Method: models.Method(http.MethodPost), ProtoMajor: 1, ProtoMinor: 1,
			URL:       "http://localhost:8080/upload",
			Header:    map[string]string{"Content-Type": "text/plain"},
			Body:      body,
			BodyRef:   ref,
			Timestamp: at,
		},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok", Timestamp: at.Add(time.Millisecond)},
	}
	if err := db.InsertTestCase(context.Background(), tc, "test-set-0", false); err != nil {
		t.Fatalf("InsertTestCase: %v", err)
	}
}

func newBodyRunner(db TestCaseDB) Service {
	return New(zap.NewNop(), db, &stepInstr{}, &skippingMockDB{},
		&stubMappingDB{mappings: map[string][]models.MockEntry{}}, nil, nil)
}

// A body over the testdb's bound is kept beside its test case, under the
// recording's root, and the test case names it by a path relative to that
// root. The runner sends it whole from the root of its own testdb, whatever
// the process's working directory: the sandbox lays a set out in a directory
// of its own and passes no path of the recording to the runner.
func TestTheRunnerSendsABodyKeptBesideItsTestCase(t *testing.T) {
	root := filepath.Join(t.TempDir(), "keploy")
	db := testdb.New(zap.NewNop(), root)
	body := strings.Repeat("0123456789abcdef", (2<<20)/16)
	recordedPost(t, db, body, models.BodyRef{})
	stored, err := db.GetTestCase(context.Background(), "test-set-0", "test-1")
	if err != nil {
		t.Fatalf("GetTestCase: %v", err)
	}
	if stored.HTTPReq.Body != "" || stored.HTTPReq.BodyRef.Path == "" || filepath.IsAbs(stored.HTTPReq.BodyRef.Path) {
		t.Fatalf("the testdb kept the body in the test case (%d bytes, ref %+v); want it beside it, by a relative path",
			len(stored.HTTPReq.Body), stored.HTTPReq.BodyRef)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, stored.HTTPReq.BodyRef.Path)); err == nil {
		t.Fatal("the body's path also names a file under the working directory; the row would not tell the two roots apart")
	}

	app := &bodyApp{}
	res := newBodyRunner(db).RunTest(context.Background(), RunTestOpts{TestSetID: "test-set-0", TestStepID: "test-1", ServiceURL: app.start(t)})
	if res.Error != "" {
		t.Fatalf("RunTest: %s", res.Error)
	}
	got := app.received()
	if len(got) != 1 || got[0] != body {
		lens := make([]int, len(got))
		for i, b := range got {
			lens[i] = len(b)
		}
		t.Fatalf("the app received bodies of %v bytes; want one of %d, the recorded body", lens, len(body))
	}
}

// A stored test case cannot make the runner read a file outside its
// recording: a body path that leads out of the testdb's directory is refused
// with why, and the app is sent nothing. A testdb with no directory has its
// test cases' files refused too, whatever the working directory holds.
func TestTheRunnerReadsNoFileOutsideItsRecording(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "keploy")
	if err := os.WriteFile(filepath.Join(tmp, "outside.txt"), []byte("not the recording's"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := testdb.New(zap.NewNop(), root)
	recordedPost(t, db, "", models.BodyRef{Path: "../outside.txt", Size: 19})
	app := &bodyApp{}
	res := newBodyRunner(db).RunTest(context.Background(), RunTestOpts{TestSetID: "test-set-0", TestStepID: "test-1", ServiceURL: app.start(t)})
	if res.Passed || !strings.Contains(res.Error, "../outside.txt") || !strings.Contains(res.Error, "is read only inside its recording") {
		t.Fatalf("RunTest: passed %v, %q; want it refused, naming the path, with why", res.Passed, res.Error)
	}
	if got := app.received(); len(got) != 0 {
		t.Fatalf("the app was sent %d POSTs; want none", len(got))
	}

	t.Chdir(tmp)
	stored, err := db.GetTestCase(context.Background(), "test-set-0", "test-1")
	if err != nil {
		t.Fatal(err)
	}
	stored.HTTPReq.BodyRef.Path = "outside.txt" // under the working directory
	app = &bodyApp{}
	res = newBodyRunner(oneCaseDB{tc: stored}).RunTest(context.Background(), RunTestOpts{TestSetID: "test-set-0", TestStepID: "test-1", ServiceURL: app.start(t)})
	if res.Passed || !strings.Contains(res.Error, "no recording directory was given") {
		t.Fatalf("RunTest with a testdb that has no directory: passed %v, %q; want the file refused", res.Passed, res.Error)
	}
	if got := app.received(); len(got) != 0 {
		t.Fatalf("the app was sent %d POSTs; want none", len(got))
	}
}
