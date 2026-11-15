package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/logleak"
	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// A test case whose request carried an upload is streamed to the recording's
// client with the file in a part of its own, named as the app's client named
// it (filename=): a user's name, a document's title. The agent's lines about
// the part name the name's size, never the name, at every level: the agent
// runs at debug in production.
func TestHandleIncoming_LogsNoUploadsFileName(t *testing.T) {
	const s = logleak.Canary
	syncmgr.Get().SetWatermark(nil)
	path := filepath.Join(t.TempDir(), "keploy-multipart-1.bin")
	if err := os.WriteFile(path, []byte("the bytes a user uploaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := newStretch()
	tc := &models.TestCase{
		Name:          "upload-1",
		HasBinaryFile: true,
		HTTPReq: models.HTTPReq{Timestamp: start, Form: []models.FormData{
			{Key: "upload", FileNames: []string{"passport-" + s + ".pdf"}, Paths: []string{path}},
		}},
		HTTPResp: models.HTTPResp{Timestamp: start.Add(time.Millisecond)},
	}
	tcs := make(chan *models.TestCase, 1)
	logger, logs := logleak.Observe()
	a := &Agent{logger: logger, svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	tcs <- tc
	close(tcs)
	if got := streamedNames(t, resp); len(got) != 1 || got[0] != "upload-1" {
		t.Fatalf("streamed %v, want [upload-1]", got)
	}
	if logs.FilterMessage("Successfully streamed file part").Len() == 0 {
		t.Fatal("precondition: the file part was not streamed; the test drove none of its lines")
	}
	logleak.Check(t, logs, s, "HandleIncoming streaming an upload")
}
