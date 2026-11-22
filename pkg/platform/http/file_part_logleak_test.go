package http_test

import (
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"golang.org/x/sync/errgroup"
)

// A recording's client reads a test case's upload from its agent's stream,
// in a part named as the app's client named the file (filename=): a user's
// name, a document's title. The client keeps it in a file of its own, and its
// lines about the part and that file name the name's size, never the name.
func TestTheClientLogsNoUploadsFileName(t *testing.T) {
	const s = logleak.Canary
	name := "passport-" + s + ".pdf"
	mux := http.NewServeMux()
	mux.HandleFunc("/incoming", func(w http.ResponseWriter, r *http.Request) {
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
		part := func(name string) textproto.MIMEHeader {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="`+name+`"`)
			return h
		}
		p, _ := mw.CreatePart(part("metadata"))
		_ = json.NewEncoder(p).Encode(&models.TestCase{Name: "upload-1", Kind: models.HTTP, HasBinaryFile: true,
			HTTPReq: models.HTTPReq{Method: "POST", Form: []models.FormData{{Key: "upload", FileNames: []string{name}, Paths: []string{"/agent/tmp/keploy-multipart-1.bin"}}}}})
		fh := textproto.MIMEHeader{}
		fh.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
		f, _ := mw.CreatePart(fh)
		_, _ = f.Write([]byte("the bytes a user uploaded"))
		_, _ = mw.CreatePart(part("delimiter"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)
	logger, logs := logleak.Observe()
	tcs, err := httpclient.New(logger, nil, cfg).GetIncoming(gctx, models.IncomingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case tc := <-tcs:
		if tc == nil || len(tc.HTTPReq.Form) != 1 || len(tc.HTTPReq.Form[0].Captured) != 1 {
			t.Fatalf("read %+v, want the test case with its upload", tc)
		}
		t.Cleanup(func() { _ = os.Remove(tc.HTTPReq.Form[0].Captured[0]) })
	case <-time.After(time.Minute):
		t.Fatal("no test case came")
	}
	if logs.FilterMessage("Received binary file part").Len() == 0 {
		t.Fatal("precondition: the file part was not read; the test drove none of its lines")
	}
	logleak.Check(t, logs, s, "the client reading an upload")
}
