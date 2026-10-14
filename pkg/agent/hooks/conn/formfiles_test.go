package conn

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// uploadReq is a POST of a form with one file part. Capture writes the part to
// a temp file (ExtractFormData), and the test case's Form carries its path.
func uploadReq(t *testing.T) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("upload", "photo.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("the bytes a user uploaded")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://app.local/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// uploadsIn lists the files Capture wrote uploads to in dir.
func uploadsIn(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "keploy-multipart-*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// The files an upload is written to are the test case's: the stream that
// sends it on deletes them once it has (routes/record.go). A test case Capture
// leaves out is sent nowhere, so Capture deletes them itself: nothing else
// knows of them, and each one left behind is a user's upload kept on the
// agent's disk for the life of the process. Every way it leaves one out: the
// manager gave up its request's window while it was in flight, it is over the
// size limit, or the recording ended before it was sent.
func TestCaptureDeletesTheUploadOfATestCaseItLeavesOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		capture func(t *testing.T, req *http.Request)
	}{
		{"its request's window was given up", func(t *testing.T, req *http.Request) {
			mgr, w := givenUpWindow(t)
			ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
			Capture(ctx, zap.NewNop(), make(chan *models.TestCase, 1), req, windowResp(), time.Now().Add(-time.Minute), time.Now(), models.IncomingOptions{}, true, false, 8080)
			w.Close()
		}},
		{"it is over the size limit", func(t *testing.T, req *http.Request) {
			resp := windowResp()
			resp.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", MaxTestCaseSize+1)))
			Capture(context.Background(), zap.NewNop(), make(chan *models.TestCase, 1), req, resp, time.Now(), time.Now(), models.IncomingOptions{}, false, false, 8080)
		}},
		{"the recording ended before it was sent", func(t *testing.T, req *http.Request) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			Capture(ctx, zap.NewNop(), make(chan *models.TestCase), req, windowResp(), time.Now(), time.Now(), models.IncomingOptions{}, false, false, 8080)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			tc.capture(t, uploadReq(t))
			if left := uploadsIn(t, dir); len(left) != 0 {
				t.Fatalf("the upload of a test case left out is still on disk: %v", left)
			}
		})
	}
}

// A test case Capture sends on keeps its upload's files: the stream it is sent
// to reads them, then deletes them.
func TestCaptureKeepsTheUploadOfATestCaseItSends(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	tcs := make(chan *models.TestCase, 1)
	Capture(context.Background(), zap.NewNop(), tcs, uploadReq(t), windowResp(), time.Now(), time.Now(), models.IncomingOptions{}, false, false, 8080)
	var sent *models.TestCase
	select {
	case sent = <-tcs:
	default:
		t.Fatal("the test case was not sent")
	}
	if !sent.HasBinaryFile || len(sent.HTTPReq.Form) != 1 || len(sent.HTTPReq.Form[0].Paths) != 1 {
		t.Fatalf("fixture: the test case carries no upload: %+v", sent.HTTPReq.Form)
	}
	path := sent.HTTPReq.Form[0].Paths[0]
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the upload of a test case sent on is gone before its stream read it: %v", err)
	}
	if string(b) != "the bytes a user uploaded" {
		t.Fatalf("fixture: the upload's file holds %q", b)
	}
}
