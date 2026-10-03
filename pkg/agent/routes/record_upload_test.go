package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// withUpload is a test case over [start, start+1ms] whose request carried an
// upload, written to a file of its own in dir, as conn.Capture writes one.
func withUpload(t *testing.T, dir, name string, start time.Time) (*models.TestCase, string) {
	t.Helper()
	path := filepath.Join(dir, name+".bin")
	if err := os.WriteFile(path, []byte("the bytes a user uploaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &models.TestCase{
		Name:          name,
		HasBinaryFile: true,
		HTTPReq: models.HTTPReq{Timestamp: start, Form: []models.FormData{
			{Key: "upload", FileNames: []string{"photo.bin"}, Paths: []string{path}},
		}},
		HTTPResp: models.HTTPResp{Timestamp: start.Add(time.Millisecond)},
	}, path
}

// waitGone waits until the file at path is deleted, and fails the test if it
// is still there after a while.
func waitGone(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the upload of %s is still on disk", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// openStream opens a HandleIncoming stream fed from tcs.
func openStream(t *testing.T, tcs chan *models.TestCase) *http.Response {
	t.Helper()
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// The files a test case's upload was written to are its own, and the stream
// is the last to have it: it deletes them once it has streamed them, and as
// well when it leaves the test case out. A test case is left out much more
// often than it was (each mock a parser leaves out leaves out the test cases
// over it), and each one left out kept a user's upload on the agent's disk for
// the life of the process.
func TestHandleIncoming_DeletesTheUploadOfATestCaseItLeavesOut(t *testing.T) {
	syncmgr.Get().SetWatermark(nil)
	dir := t.TempDir()
	tcs := make(chan *models.TestCase, 2)
	resp := openStream(t, tcs)

	// In a stretch of its own: no span of another test reaches it.
	start := newStretch()
	if over, n := syncmgr.Get().WasMockOrphanedInWindow(start, start.Add(30*time.Second)); over {
		t.Fatalf("%d spans recorded before this test already cover its test cases", n)
	}
	syncmgr.Get().RecordOrphanWindow(start, start.Add(time.Millisecond))
	leftOut, leftOutPath := withUpload(t, dir, "over-a-span", start)
	streamed, streamedPath := withUpload(t, dir, "clean", start.Add(20*time.Second))
	tcs <- leftOut
	tcs <- streamed
	close(tcs)

	if got := streamedNames(t, resp); len(got) != 1 || got[0] != "clean" {
		t.Fatalf("streamed %v, want only [clean]", got)
	}
	waitGone(t, streamedPath, "a test case streamed")
	waitGone(t, leftOutPath, "a test case left out")
}

// A stream that ends while it holds test cases for their verdict (its client
// went away) sends none of them, and deletes their uploads.
func TestHandleIncoming_DeletesTheUploadOfATestCaseItHeldWhenItsClientGoes(t *testing.T) {
	w := &switchWatermark{} // never settles: the test case stays held
	syncmgr.Get().SetWatermark(w)
	t.Cleanup(func() { syncmgr.Get().SetWatermark(nil) })
	dir := t.TempDir()
	tcs := make(chan *models.TestCase, 1)
	resp := openStream(t, tcs)

	held, path := withUpload(t, dir, "held", newStretch())
	tcs <- held
	deadline := time.Now().Add(5 * time.Second)
	for len(tcs) != 0 { // the handler took it, and holds it
		if time.Now().After(deadline) {
			t.Fatal("the handler never took the test case")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture: the upload of a test case still held is gone: %v", err)
	}
	_ = resp.Body.Close() // the client goes away
	waitGone(t, path, "a test case held when its stream ended")
}

// brokenStream is the stream of a client that is gone: every write to it
// fails.
type brokenStream struct{ header http.Header }

func (b *brokenStream) Header() http.Header       { return b.header }
func (b *brokenStream) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }
func (b *brokenStream) WriteHeader(int)           {}
func (b *brokenStream) Flush()                    {}

// A stream that breaks as it sends what its hold let go sends none of the
// rest, and deletes their uploads too. The hold has handed them over, so the
// stream's end (TestCaseHold.Drain) no longer sees them: left to it, they
// would stay on the agent's disk for the life of the process.
func TestHandleIncoming_DeletesTheUploadsOfTestCasesLostToABrokenStream(t *testing.T) {
	w := &switchWatermark{} // unsettled: both test cases stay held
	syncmgr.Get().SetWatermark(w)
	t.Cleanup(func() { syncmgr.Get().SetWatermark(nil) })
	dir := t.TempDir()
	tcs := make(chan *models.TestCase, 2)
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.HandleIncoming(&brokenStream{header: http.Header{}},
			httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
	}()

	// In a stretch of its own: no span of another test leaves either out, so
	// the first is written to the stream, and the stream breaks on it.
	start := newStretch()
	if over, n := syncmgr.Get().WasMockOrphanedInWindow(start, start.Add(30*time.Second)); over {
		t.Fatalf("%d spans recorded before this test already cover its test cases", n)
	}
	first, firstPath := withUpload(t, dir, "first", start)
	second, secondPath := withUpload(t, dir, "second", start.Add(time.Second))
	tcs <- first
	tcs <- second
	// Once the handler has taken both, both are held: it holds each in the
	// turn of its loop that takes it, and looks at the hold again only in a
	// later turn.
	deadline := time.Now().Add(5 * time.Second)
	for len(tcs) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the handler never took the test cases")
		}
		time.Sleep(time.Millisecond)
	}
	for _, path := range []string{firstPath, secondPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("fixture: the upload of a test case still held is gone: %v", err)
		}
	}
	w.settled.Store(true) // the next look at the hold lets both go

	select {
	case <-done:
	case <-time.After(streamWait):
		t.Fatal("the handler did not return when its stream broke")
	}
	waitGone(t, firstPath, "the test case the stream broke on")
	waitGone(t, secondPath, "a test case let go behind the one the stream broke on")
}
