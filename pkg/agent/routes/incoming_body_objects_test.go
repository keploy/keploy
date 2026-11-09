package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// Bytes no UTF-8 decoder accepts: a PNG's signature and a protobuf varint.
const (
	pngReqBody    = "\x89PNG\r\n\x1a\n\x00\xff\xfe\x01upload"
	protoRespBody = "\x08\x96\x01\x12\x04\xc3\x28\xff\xfeproto"
)

// bodyCases are a test case with binary bodies between two with text, in the
// stretch that starts at at.
func bodyCases(at time.Time) []*models.TestCase {
	var tcs []*models.TestCase
	for i, b := range [][2]string{{`{"a":"<b>"}`, "ok é"}, {pngReqBody, protoRespBody}, {"x=1", `{"done":true}`}} {
		ts := at.Add(time.Duration(i) * 10 * time.Millisecond)
		tcs = append(tcs, &models.TestCase{Name: []string{"text-1", "binary", "text-2"}[i], Kind: models.HTTP,
			HTTPReq:  models.HTTPReq{Method: "POST", URL: "http://app/x", Body: b[0], Timestamp: ts},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: b[1], Timestamp: ts.Add(time.Millisecond)}})
	}
	return tcs
}

// streamedMetadata streams cases through HandleIncoming to a client whose
// request carries header (none if empty) and returns each test case's JSON as
// it went.
func streamedMetadata(t *testing.T, header string, cases []*models.TestCase) [][]byte {
	t.Helper()
	tcs := make(chan *models.TestCase, len(cases))
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	defer srv.Close()
	body, _ := json.Marshal(models.IncomingReq{})
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if header != "" {
		req.Header.Set(models.BodyEncodingHeader, header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, tc := range cases {
		tcs <- tc
	}
	close(tcs)
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	var got [][]byte
	for len(got) < len(cases) {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("the stream ended after %d test cases: %v", len(got), err)
		}
		if strings.Contains(p.Header.Get("Content-Disposition"), `name="metadata"`) {
			b, err := io.ReadAll(p)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, b)
		}
	}
	return got
}

// bodyTokens is the first byte of each body's JSON value in a test case's
// JSON: '"' for a string, '{' for a body object.
func bodyTokens(t *testing.T, b []byte) (req, resp byte) {
	t.Helper()
	var doc struct {
		Req struct {
			Body json.RawMessage `json:"body"`
		} `json:"http_req"`
		Resp struct {
			Body json.RawMessage `json:"body"`
		} `json:"http_resp"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Req.Body) == 0 || len(doc.Resp.Body) == 0 {
		t.Fatalf("%s: %v", b, err)
	}
	return doc.Req.Body[0], doc.Resp.Body[0]
}

// A client that does not ask, one built before body objects among them, gets
// every test case as encoding/json writes it: a reader whose bodies are plain
// strings reads all of them, the binary one's bodies as today's text.
func TestHandleIncoming_AClientThatDoesNotAskGetsBodiesAsStrings(t *testing.T) {
	cases := bodyCases(newStretch())
	for _, header := range []string{"", "gzip", "base64x"} {
		got := streamedMetadata(t, header, cases)
		for i, b := range got {
			if req, resp := bodyTokens(t, b); req != '"' || resp != '"' {
				t.Errorf("header %q, %s: bodies are not strings: %s", header, cases[i].Name, b)
			}
			var old struct {
				Name string `json:"name"`
				Req  struct {
					Body string `json:"body"`
				} `json:"http_req"`
				Resp struct {
					Body string `json:"body"`
				} `json:"http_resp"`
			}
			if err := json.Unmarshal(b, &old); err != nil {
				t.Fatalf("header %q: a reader built before body objects: %v", header, err)
			}
			wantReq, _ := json.Marshal(cases[i].HTTPReq.Body)
			wantResp, _ := json.Marshal(cases[i].HTTPResp.Body)
			if gotReq, _ := json.Marshal(old.Req.Body); old.Name != cases[i].Name || !bytes.Equal(gotReq, wantReq) {
				t.Errorf("header %q, %s: request body %q", header, cases[i].Name, old.Req.Body)
			}
			if gotResp, _ := json.Marshal(old.Resp.Body); !bytes.Equal(gotResp, wantResp) {
				t.Errorf("header %q, %s: response body %q", header, cases[i].Name, old.Resp.Body)
			}
		}
	}
}

// A client that asks gets a binary body as a body object, and a text body as
// the string it always was.
func TestHandleIncoming_AClientThatAsksGetsBodyObjects(t *testing.T) {
	cases := bodyCases(newStretch())
	got := streamedMetadata(t, models.BodyEncodingBase64, cases)
	for i, b := range got {
		want := byte('"')
		if cases[i].Name == "binary" {
			want = '{'
		}
		if req, resp := bodyTokens(t, b); req != want || resp != want {
			t.Errorf("%s: bodies start %q %q, want %q: %s", cases[i].Name, req, resp, want, b)
		}
		var tc models.TestCase
		if err := json.Unmarshal(b, &tc); err != nil {
			t.Fatal(err)
		}
		if tc.Name != cases[i].Name || tc.HTTPReq.Body != cases[i].HTTPReq.Body || tc.HTTPResp.Body != cases[i].HTTPResp.Body {
			t.Errorf("%s read back as %q: %q %q", cases[i].Name, tc.Name, tc.HTTPReq.Body, tc.HTTPResp.Body)
		}
	}
}

// keploy's own recorder asks: a test case with binary bodies reaches it from
// the agent with its bytes, on OSS `keploy record`'s path.
func TestHandleIncoming_TheRecorderGetsBinaryBodiesExactly(t *testing.T) {
	cases := bodyCases(newStretch())
	tcs := make(chan *models.TestCase, len(cases))
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	mux := http.NewServeMux()
	mux.HandleFunc("/incoming", a.HandleIncoming)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)
	got, err := httpclient.New(zap.NewNop(), nil, cfg).GetIncoming(gctx, models.IncomingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		tcs <- tc
	}
	for i := range cases {
		select {
		case tc, ok := <-got:
			if !ok {
				t.Fatalf("the stream ended after %d test cases", i)
			}
			if tc.Name != cases[i].Name || tc.HTTPReq.Body != cases[i].HTTPReq.Body || tc.HTTPResp.Body != cases[i].HTTPResp.Body {
				t.Errorf("%s reached the recorder as %q: %q %q", cases[i].Name, tc.Name, tc.HTTPReq.Body, tc.HTTPResp.Body)
			}
		case <-time.After(streamWait): // only turns a hang into a failure
			t.Fatalf("test case %d did not reach the recorder within %v", i, streamWait)
		}
	}
	close(tcs)
}
