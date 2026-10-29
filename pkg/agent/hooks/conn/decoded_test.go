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

// bufferFor is the capacity pkg.DecompressReserving's output buffer has for
// a body of n bytes decoded: 64 KiB, doubled until it is past n.
func bufferFor(n int) int64 {
	c := int64(64 << 10)
	for int64(n) >= c {
		c *= 2
	}
	return c
}

// decodeBooks is a DecodeReserveFunc's books in a Capture row.
type decodeBooks struct {
	held, asks int64
	refuseAt   int // refuse the refuseAt-th ask for room (1-based; 0 never)
	refused    int
}

func (b *decodeBooks) reserve(n int64) bool {
	if n > 0 {
		b.asks++
		if b.refuseAt > 0 && int(b.asks) == b.refuseAt {
			b.refused++
			return false
		}
	}
	b.held += n
	return true
}

// TestCaptureDecodesOnlyIntoRoomItsCallerReserved pins the second hook a
// caller that bounds its captures uses (DecodeReserveFunc): Capture's decodes
// of an encoded request and response ask it for room before they hold their
// output, so when BodiesDecodedFunc is called the decoded bodies are
// reserved (at their buffers' capacity, the decoders' state given back); a
// plain exchange asks nothing.
func TestCaptureDecodesOnlyIntoRoomItsCallerReserved(t *testing.T) {
	reqJSON := []byte(strings.Repeat(`{"q":"a repeated query"},`, 4000))   // 100 KB
	respJSON := []byte(strings.Repeat(`{"id":1,"name":"a record"},`, 20000)) // 540 KB
	for _, c := range []struct {
		name, reqEnc, respEnc string
		wantAsks              bool
	}{
		{"gzip request, br response", "gzip", "br", true},
		{"br request, gzip response", "br", "gzip", true},
		{"plain", "", "", false},
	} {
		var books decodeBooks
		var atDecoded int64 = -1
		var got bodiesDecodedCall
		ctx := WithDecodeReserve(context.Background(), books.reserve)
		ctx = WithBodiesDecoded(ctx, func(reqBytes, respBytes int) bool {
			atDecoded, got = books.held, bodiesDecodedCall{reqBytes: reqBytes, respBytes: respBytes}
			return true
		})
		req, err := http.NewRequest(http.MethodPost, "http://localhost:8080/q", bytes.NewReader(encodeBody(t, c.reqEnc, reqJSON)))
		if err != nil {
			t.Fatal(err)
		}
		if c.reqEnc != "" {
			req.Header.Set("Content-Encoding", c.reqEnc)
		}
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &trackCloser{Reader: bytes.NewReader(encodeBody(t, c.respEnc, respJSON))}}
		if c.respEnc != "" {
			resp.Header.Set("Content-Encoding", c.respEnc)
		}
		tcs := make(chan *models.TestCase, 1)
		Capture(ctx, zap.NewNop(), tcs, req, resp, time.Now(), time.Now(), models.IncomingOptions{}, false, false, 8080)
		select {
		case <-tcs:
		default:
			t.Fatalf("%s: no test case sent", c.name)
		}
		if got.reqBytes != len(reqJSON) || got.respBytes != len(respJSON) {
			t.Fatalf("%s: BodiesDecodedFunc told %+v, want %d and %d", c.name, got, len(reqJSON), len(respJSON))
		}
		if !c.wantAsks {
			if books.asks != 0 || atDecoded != 0 {
				t.Errorf("%s: a plain exchange asked for room %d times (%d held)", c.name, books.asks, atDecoded)
			}
			continue
		}
		// Each decode's buffer grows by doubling from 64 KiB, so its
		// capacity is the first such size past its length; the decoders'
		// own state is given back as they close.
		want := bufferFor(len(reqJSON)) + bufferFor(len(respJSON))
		if atDecoded != want {
			t.Errorf("%s: %d bytes reserved when the bodies were decoded, want both decoded bodies' buffers, %d", c.name, atDecoded, want)
		}
	}
}

// TestARefusedDecodeLeavesTheExchangeOut pins a refusal from the caller's
// DecodeReserveFunc, on the request's decode and on the response's, in both
// modes: Capture stops there, sends nothing, never calls BodiesDecodedFunc,
// closes the response body, and leaves nothing reserved.
func TestARefusedDecodeLeavesTheExchangeOut(t *testing.T) {
	body := []byte(strings.Repeat(`{"id":1,"name":"a record"},`, 20000))
	for _, synchronous := range []bool{false, true} {
		for _, refuseAt := range []int{1, 2, 4, 6} { // the request's decoder, its buffer, the response's ...
			books := decodeBooks{refuseAt: refuseAt}
			calls := 0
			ctx := WithDecodeReserve(context.Background(), books.reserve)
			ctx = WithBodiesDecoded(ctx, func(int, int) bool { calls++; return true })
			req, err := http.NewRequest(http.MethodPost, "http://localhost:8080/q", bytes.NewReader(encodeBody(t, "gzip", body)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Encoding", "gzip")
			rb := &trackCloser{Reader: bytes.NewReader(encodeBody(t, "br", body))}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Encoding": {"br"}}, Body: rb}
			tcs := make(chan *models.TestCase, 1)
			Capture(ctx, zap.NewNop(), tcs, req, resp, time.Now(), time.Now(), models.IncomingOptions{}, synchronous, false, 8080)
			if books.refused != 1 {
				t.Fatalf("synchronous=%v, refuse at %d: refused %d times; the row's exchange must reach it", synchronous, refuseAt, books.refused)
			}
			select {
			case tc := <-tcs:
				t.Errorf("synchronous=%v, refuse at %d: test case %q sent though its decode was refused", synchronous, refuseAt, tc.Name)
			default:
			}
			if calls != 0 {
				t.Errorf("synchronous=%v, refuse at %d: BodiesDecodedFunc called %d times after a refusal", synchronous, refuseAt, calls)
			}
			if !rb.closed {
				t.Errorf("synchronous=%v, refuse at %d: response body not closed", synchronous, refuseAt)
			}
			// What the request's decode left reserved (its body, decoded) is
			// the caller's: it gives the room back as the capture returns. The
			// refused decode itself left nothing.
			if refuseAt <= 2 && books.held != 0 {
				t.Errorf("synchronous=%v, refuse at %d: %d bytes reserved after the request's decode was refused", synchronous, refuseAt, books.held)
			}
		}
	}
}
