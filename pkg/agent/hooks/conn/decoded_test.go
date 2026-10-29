package conn

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// decodeLogs counts the bodies pkg.Decompress inflated, by the debug line it
// logs for each (gzip and br).
func decodeLogs(logs *observer.ObservedLogs) int {
	return logs.FilterMessage("decoding gzip compressed data").Len() +
		logs.FilterMessage("decompressing brotli compressed data").Len()
}

// encodeBody returns body as sent with the Content-Encoding enc ("" = as is).
func encodeBody(t *testing.T, enc string, body []byte) []byte {
	t.Helper()
	if enc == "" {
		return body
	}
	out, err := pkg.Compress(zap.NewNop(), enc, body)
	if err != nil {
		t.Fatalf("compress %s: %v", enc, err)
	}
	return out
}

type bodiesDecodedCall struct {
	reqBytes, respBytes int
	decodedBefore       int // bodies inflated when it was called
}

// TestCaptureTellsItsCallerTheBodiesItsTestCaseHoldsOnceDecoded pins the
// hook a caller that holds room for its captures weighs them by
// (BodiesDecodedFunc): Capture calls it once, after it has decoded every
// encoded body and before it builds the test case, with the request body
// decoded and the response body as the test case holds it; and it inflates
// each encoded body once, so its caller needs no decode of its own.
func TestCaptureTellsItsCallerTheBodiesItsTestCaseHoldsOnceDecoded(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())                                                  // the multipart case's form files
	json := []byte(strings.Repeat(`{"id":12345,"name":"a repeated record"},`, 3000)) // 120 KB, inflates ~100x
	large := bytes.Repeat([]byte("x"), LargeBodyThreshold+1)

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	_ = mw.WriteField("note", strings.Repeat("n", 4096))
	fw, _ := mw.CreateFormFile("file", "a.bin")
	_, _ = fw.Write(bytes.Repeat([]byte{7}, 8192))
	_ = mw.Close()

	form := []byte(url.Values{"q": {strings.Repeat("a b&", 500)}}.Encode())

	cases := []struct {
		name              string
		reqEnc, respEnc   string
		reqBody, respBody []byte
		reqType           string
		wantReq, wantResp int
		wantDecodes       int
		wantRespBodyKept  bool
		wantReqBodyInCase []byte
	}{
		{name: "plain", reqBody: json, respBody: json, wantReq: len(json), wantResp: len(json), wantRespBodyKept: true, wantReqBodyInCase: json},
		{name: "gzip both", reqEnc: "gzip", respEnc: "gzip", reqBody: json, respBody: json, wantReq: len(json), wantResp: len(json), wantDecodes: 2, wantRespBodyKept: true, wantReqBodyInCase: json},
		{name: "br both", reqEnc: "br", respEnc: "br", reqBody: json, respBody: json, wantReq: len(json), wantResp: len(json), wantDecodes: 2, wantRespBodyKept: true, wantReqBodyInCase: json},
		{name: "GZIP request, br response", reqEnc: " GZIP ", respEnc: "br", reqBody: json, respBody: json, wantReq: len(json), wantResp: len(json), wantDecodes: 2, wantRespBodyKept: true, wantReqBodyInCase: json},
		{name: "gzip response over the large-body threshold", respEnc: "gzip", reqBody: json, respBody: large, wantReq: len(json), wantResp: 0, wantDecodes: 1, wantReqBodyInCase: json},
		{name: "multipart request", reqBody: mp.Bytes(), reqType: mw.FormDataContentType(), respBody: json, wantReq: mp.Len(), wantResp: len(json), wantRespBodyKept: true, wantReqBodyInCase: []byte{}},
		{name: "url-encoded request, gzip'd", reqEnc: "gzip", reqBody: form, reqType: "application/x-www-form-urlencoded", respBody: json, wantReq: len(form), wantResp: len(json), wantDecodes: 1, wantRespBodyKept: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			logger := zap.New(core)
			var calls []bodiesDecodedCall
			ctx := WithBodiesDecoded(context.Background(), func(reqBytes, respBytes int) bool {
				calls = append(calls, bodiesDecodedCall{reqBytes, respBytes, decodeLogs(logs)})
				return true
			})

			req, err := http.NewRequest(http.MethodPost, "http://localhost:8080/orders", bytes.NewReader(encodeBody(t, strings.ToLower(strings.TrimSpace(c.reqEnc)), c.reqBody)))
			if err != nil {
				t.Fatal(err)
			}
			if c.reqEnc != "" {
				req.Header.Set("Content-Encoding", c.reqEnc)
			}
			if c.reqType != "" {
				req.Header.Set("Content-Type", c.reqType)
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
				Body: &trackCloser{Reader: bytes.NewReader(encodeBody(t, c.respEnc, c.respBody))}}
			if c.respEnc != "" {
				resp.Header.Set("Content-Encoding", c.respEnc)
			}
			tcs := make(chan *models.TestCase, 1)
			Capture(ctx, logger, tcs, req, resp, time.Now(), time.Now(), models.IncomingOptions{}, false, false, 8080)

			if len(calls) != 1 {
				t.Fatalf("BodiesDecodedFunc called %d times, want once: %+v", len(calls), calls)
			}
			got := calls[0]
			if got.reqBytes != c.wantReq || got.respBytes != c.wantResp {
				t.Errorf("told req=%d resp=%d bytes, want req=%d resp=%d (decoded, as the test case holds them)",
					got.reqBytes, got.respBytes, c.wantReq, c.wantResp)
			}
			if got.decodedBefore != c.wantDecodes {
				t.Errorf("called with %d of %d encoded bodies decoded: it must come after the decode", got.decodedBefore, c.wantDecodes)
			}
			if n := decodeLogs(logs); n != c.wantDecodes {
				t.Errorf("inflated %d bodies, want %d: each encoded body once", n, c.wantDecodes)
			}
			var tc *models.TestCase
			select {
			case tc = <-tcs:
			default:
				t.Fatal("no test case sent, though BodiesDecodedFunc said yes")
			}
			if c.wantReqBodyInCase != nil && tc.HTTPReq.Body != string(c.wantReqBodyInCase) {
				t.Errorf("test case request body is %d bytes, want %d", len(tc.HTTPReq.Body), len(c.wantReqBodyInCase))
			}
			if len(tc.HTTPReq.Body) > got.reqBytes {
				t.Errorf("test case holds %d bytes of request body, more than the %d it was weighed by", len(tc.HTTPReq.Body), got.reqBytes)
			}
			if len(tc.HTTPResp.Body) != got.respBytes {
				t.Errorf("test case holds %d bytes of response body, told %d", len(tc.HTTPResp.Body), got.respBytes)
			}
			if c.wantRespBodyKept && tc.HTTPResp.Body != string(c.respBody) {
				t.Errorf("test case response body is %d bytes, want the %d decoded", len(tc.HTTPResp.Body), len(c.respBody))
			}
			RemoveFormFiles(zap.NewNop(), tc.HTTPReq.Form)
		})
	}
}

// TestABodiesDecodedFuncThatSaysNoLeavesTheExchangeOut pins that Capture
// honours the answer: an exchange its caller leaves out is sent nowhere, its
// response body is closed, and the files its multipart parts were written to
// are removed, as for any exchange Capture leaves out.
func TestABodiesDecodedFuncThatSaysNoLeavesTheExchangeOut(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		dir := t.TempDir()
		t.Setenv("TMPDIR", dir)
		var mp bytes.Buffer
		mw := multipart.NewWriter(&mp)
		fw, _ := mw.CreateFormFile("file", "a.bin")
		_, _ = fw.Write(bytes.Repeat([]byte{7}, 8192))
		_ = mw.Close()

		calls := 0
		ctx := WithBodiesDecoded(context.Background(), func(int, int) bool { calls++; return false })
		req, err := http.NewRequest(http.MethodPost, "http://localhost:8080/upload", bytes.NewReader(mp.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		body := &trackCloser{Reader: bytes.NewReader(encodeBody(t, "gzip", []byte(`{"ok":true}`)))}
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Encoding": {"gzip"}}, Body: body}
		tcs := make(chan *models.TestCase, 1)
		Capture(ctx, zap.NewNop(), tcs, req, resp, time.Now(), time.Now(), models.IncomingOptions{}, synchronous, false, 8080)

		if calls != 1 {
			t.Errorf("synchronous=%v: BodiesDecodedFunc called %d times, want once", synchronous, calls)
		}
		select {
		case tc := <-tcs:
			t.Errorf("synchronous=%v: test case %q sent, though BodiesDecodedFunc left it out", synchronous, tc.Name)
		default:
		}
		if !body.closed {
			t.Errorf("synchronous=%v: response body not closed", synchronous)
		}
		left, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Errorf("synchronous=%v: the left-out exchange's form files remain: %d files", synchronous, len(left))
		}
	}
}

// TestBodiesDecodedIsCalledOnlyForAnExchangeCaptureBuilds pins that an
// exchange Capture leaves out on its own (filtered, or over MaxTestCaseSize
// decoded) never reaches BodiesDecodedFunc, and that a context with none
// leaves every exchange in.
func TestBodiesDecodedIsCalledOnlyForAnExchangeCaptureBuilds(t *testing.T) {
	if !BodiesDecoded(context.Background(), 1, 1) || !BodiesDecoded(nil, 1, 1) { //nolint:staticcheck // a nil ctx is allowed
		t.Fatal("no BodiesDecodedFunc: the exchange must be left in")
	}
	run := func(opts models.IncomingOptions, respEnc string, respBody []byte) int {
		calls := 0
		ctx := WithBodiesDecoded(context.Background(), func(int, int) bool { calls++; return true })
		req, err := http.NewRequest(http.MethodGet, "http://localhost:8080/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &trackCloser{Reader: bytes.NewReader(respBody)}}
		if respEnc != "" {
			resp.Header.Set("Content-Encoding", respEnc)
		}
		Capture(ctx, zap.NewNop(), make(chan *models.TestCase, 1), req, resp, time.Now(), time.Now(), opts, false, false, 8080)
		return calls
	}
	filtered := models.IncomingOptions{Filters: []models.Filter{{BypassRule: models.BypassRule{Path: "/healthz"}}}}
	if n := run(filtered, "", []byte("ok")); n != 0 {
		t.Errorf("a filtered exchange reached BodiesDecodedFunc %d times", n)
	}
	if n := run(models.IncomingOptions{}, "gzip", gzipBomb(t)); n != 0 {
		t.Errorf("an exchange over MaxTestCaseSize decoded reached BodiesDecodedFunc %d times", n)
	}
	if n := run(models.IncomingOptions{}, "", []byte("ok")); n != 1 {
		t.Errorf("a kept exchange reached BodiesDecodedFunc %d times, want once", n)
	}
}
