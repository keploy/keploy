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
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// A test case the client reads from its agent names, as captured
// (FormData.Captured), each file part's file: the client's own copy of a part
// it received in the stream, or the agent's path for one it did not; in the
// legacy JSON stream, the agent's path. Only those are files the test store
// copies a part from.
func TestTheClientMarksTheFilesItsAgentCaptured(t *testing.T) {
	const agentPath, otherPath = "/agent/tmp/keploy-multipart-1.bin", "/agent/tmp/keploy-multipart-2.bin"
	form := func(path string) []models.FormData {
		return []models.FormData{{Key: "upload", FileNames: []string{"a.txt"}, Paths: []string{path}}}
	}
	read := func(t *testing.T, serve http.HandlerFunc) []*models.TestCase {
		t.Helper()
		mux := http.NewServeMux()
		mux.HandleFunc("/incoming", serve)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		cfg := &config.Config{}
		cfg.Agent.AgentURI = srv.URL
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		grp, gctx := errgroup.WithContext(ctx)
		gctx = context.WithValue(gctx, models.ErrGroupKey, grp)
		tcs, err := httpclient.New(zap.NewNop(), nil, cfg).GetIncoming(gctx, models.IncomingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var got []*models.TestCase
		timeout := time.After(time.Minute) // only turns a hang into a failure
		for len(got) < 2 {
			select {
			case tc, ok := <-tcs:
				if !ok {
					return got
				}
				got = append(got, tc)
			case <-timeout:
				t.Fatalf("read %d test cases, want 2", len(got))
			}
		}
		return got
	}
	hold := func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}

	t.Run("multipart", func(t *testing.T) {
		got := read(t, func(w http.ResponseWriter, r *http.Request) {
			mw := multipart.NewWriter(w)
			w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
			part := func(name string) *textproto.MIMEHeader {
				h := textproto.MIMEHeader{}
				h.Set("Content-Disposition", `form-data; name="`+name+`"`)
				return &h
			}
			p, _ := mw.CreatePart(*part("metadata"))
			_ = json.NewEncoder(p).Encode(&models.TestCase{Name: "test-1", Kind: models.HTTP, HasBinaryFile: true,
				HTTPReq: models.HTTPReq{Method: "POST", Form: form(agentPath)}})
			fh := textproto.MIMEHeader{}
			fh.Set("Content-Disposition", `form-data; name="file"; filename="a.txt"`)
			f, _ := mw.CreatePart(fh)
			_, _ = f.Write([]byte("uploaded"))
			_, _ = mw.CreatePart(*part("delimiter"))
			p, _ = mw.CreatePart(*part("metadata"))
			_ = json.NewEncoder(p).Encode(&models.TestCase{Name: "test-2", Kind: models.HTTP,
				HTTPReq: models.HTTPReq{Method: "POST", Form: form(otherPath)}})
			_, _ = mw.CreatePart(*part(models.StreamEndPart))
			_, _ = mw.CreatePart(*part("delimiter"))
			hold(w, r)
		})
		if len(got) != 2 {
			t.Fatalf("got %d test cases, want 2", len(got))
		}
		received := got[0].HTTPReq.Form[0]
		if len(received.Captured) != 1 || received.Captured[0] == agentPath || received.Captured[0] != received.Paths[0] {
			t.Fatalf("a part received in the stream: captured %q, paths %q, want the client's copy", received.Captured, received.Paths)
		}
		t.Cleanup(func() { _ = os.Remove(received.Captured[0]) })
		if b, err := os.ReadFile(received.Captured[0]); err != nil || string(b) != "uploaded" {
			t.Fatalf("the client's copy holds %q, %v", b, err)
		}
		if c := got[1].HTTPReq.Form[0].Captured; len(c) != 1 || c[0] != otherPath {
			t.Fatalf("a part not received in the stream: captured %q, want the agent's %s", c, otherPath)
		}
	})

	t.Run("legacy JSON", func(t *testing.T) {
		got := read(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			_ = enc.Encode(&models.TestCase{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "POST", Form: form(agentPath)}})
			_ = enc.Encode(&models.TestCase{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "POST", Form: form(otherPath)}})
			hold(w, r)
		})
		if len(got) != 2 {
			t.Fatalf("got %d test cases, want 2", len(got))
		}
		for i, want := range []string{agentPath, otherPath} {
			if c := got[i].HTTPReq.Form[0].Captured; len(c) != 1 || c[0] != want {
				t.Fatalf("test case %d: captured %q, want the agent's %s", i, c, want)
			}
		}
	})
}
