package http_test

import (
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// The client asks the agent for body objects, so a test case whose bodies are
// not UTF-8 reaches the recorder with its bytes: the agent writes each test
// case of its stream as JSON (routes/record.go), and a JSON string holds text
// alone, so before the body's JSON form named an encoding each invalid byte
// arrived as U+FFFD and the recording kept another body. An agent built before
// body objects ignores the ask and writes strings, which the client reads as it
// always did.
func TestTheClientReadsABodyThatIsNotUTF8(t *testing.T) {
	const reqBody = "\x89PNG\r\n\x1a\n\x00\xff\xfe\x01upload"
	const respBody = "\x08\x96\x01\x12\x04\xc3\x28\xff\xfeproto"
	for _, oldAgent := range []bool{false, true} {
		asked := make(chan bool, 1)
		mux := http.NewServeMux()
		mux.HandleFunc("/incoming", func(w http.ResponseWriter, r *http.Request) {
			asked <- models.ReadsBodyObjects(r.Header)
			mw := multipart.NewWriter(w)
			w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", `form-data; name="metadata"`)
			p, _ := mw.CreatePart(h)
			tc := &models.TestCase{Name: "test-1", Kind: models.HTTP,
				HTTPReq:  models.HTTPReq{Method: "POST", Body: reqBody},
				HTTPResp: models.HTTPResp{StatusCode: 200, Body: respBody}}
			var v interface{} = models.TestCaseBodyObjects{TestCase: tc}
			if oldAgent {
				v = tc
			}
			_ = json.NewEncoder(p).Encode(v)
			end := textproto.MIMEHeader{}
			end.Set("Content-Disposition", `form-data; name="`+models.StreamEndPart+`"`)
			_, _ = mw.CreatePart(end)
			_, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="delimiter"`}})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		cfg := &config.Config{}
		cfg.Agent.AgentURI = srv.URL
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		grp, gctx := errgroup.WithContext(ctx)
		gctx = context.WithValue(gctx, models.ErrGroupKey, grp)
		client := httpclient.New(zap.NewNop(), nil, cfg)
		tcs, err := client.GetIncoming(gctx, models.IncomingOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var got []*models.TestCase
		done := make(chan struct{})
		go func() {
			for tc := range tcs {
				got = append(got, tc)
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatal("the stream did not end at its marker")
		}
		if !<-asked {
			t.Fatalf("the client did not send %s: %s", models.BodyEncodingHeader, models.BodyEncodingBase64)
		}
		if len(got) != 1 {
			t.Fatalf("old agent %v: got %d test cases, want 1", oldAgent, len(got))
		}
		wantReq, wantResp := reqBody, respBody
		if oldAgent {
			wantReq, wantResp = asJSONText(t, reqBody), asJSONText(t, respBody)
		}
		if got[0].HTTPReq.Body != wantReq {
			t.Errorf("old agent %v: request body = %q, want %q", oldAgent, got[0].HTTPReq.Body, wantReq)
		}
		if got[0].HTTPResp.Body != wantResp {
			t.Errorf("old agent %v: response body = %q, want %q", oldAgent, got[0].HTTPResp.Body, wantResp)
		}
	}
}

// asJSONText is s as encoding/json writes and reads a string: each byte that
// is not UTF-8 becomes U+FFFD.
func asJSONText(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
