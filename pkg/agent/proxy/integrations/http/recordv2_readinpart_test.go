package http

import (
	"bytes"
	"compress/gzip"
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// An observe-only capture (a DaemonSet's, Opts.SkipTLSMITM) carries only the
// bytes the client read: over TLS, what SSL_read handed it. A client that
// reads the part of a response it wants and closes the connection leaves the
// response cut short where it stopped, and the stream ends there. The shape
// is common: a status check that reads the status line, a client that stops
// at the headers it needs, one that abandons a large body.
//
// The recording must keep that exchange as the client had it. It used to be
// dropped: buildHTTPMock parsed the cut response as a whole one and failed
// ("parse response: unexpected EOF"), so the test case that made the call was
// saved without its mock and replay answered the call with a 502, or, where
// the capture counts such a return as its parser failing, was left out of the
// recording altogether.
//
// The capture says when a stream ended because its connection did
// (Session.MarkEndedWithConnection). Only then is the end the client's: a
// stream cut by a capture hole, or by the recording stopping while the client
// still read, lost the rest of the response, and is not recorded.

// apiserverVersionResponse is a kube-apiserver answer to GET /version. A
// Python client that wants its status line calls ss.recv(256) and closes.
var apiserverVersionResponse = []byte("HTTP/1.1 200 OK\r\n" +
	"Audit-Id: 0f5d3e0a-6d1c-4c8e-9a51-2a7b1c7d4e11\r\n" +
	"Cache-Control: no-cache, private\r\n" +
	"Content-Type: application/json\r\n" +
	"X-Kubernetes-Pf-Flowschema-Uid: 6c1a4a3e-1b7e-4a0e-8f2e-7d6a9b3c2e10\r\n" +
	"X-Kubernetes-Pf-Prioritylevel-Uid: 9b2f7c1d-3e4a-4b5c-8d6e-1f2a3b4c5d6e\r\n" +
	"Date: Wed, 30 Sep 2026 21:37:38 GMT\r\n" +
	"Content-Length: 226\r\n" +
	"\r\n" +
	`{"major":"1","minor":"31","gitVersion":"v1.31.0","gitCommit":"9edcffcde5595e8a5b1a35f88c421764e575afce","gitTreeState":"clean","buildDate":"2024-08-13T07:28:49Z","goVersion":"go1.22.5","compiler":"gc","platform":"linux/amd64"}`)

var versionRequest = []byte("GET /version HTTP/1.1\r\nHost: kubernetes.default.svc\r\nConnection: close\r\n\r\n")

// streamEnd is how a response's stream ended, as the capture tells recordV2.
type streamEnd struct {
	observeOnly bool // Opts.SkipTLSMITM: the stream is what the client read
	withConn    bool // MarkEndedWithConnection: the client closed the connection there
}

var (
	clientClosed  = streamEnd{observeOnly: true, withConn: true}  // the client read that much and closed
	captureCut    = streamEnd{observeOnly: true, withConn: false} // a hole, or the recording stopping
	relayedClosed = streamEnd{observeOnly: false, withConn: true} // proxy mode: the server closed
)

// recordExchanges runs recordV2 over one connection carrying pairs of
// request and response bytes, whose streams then end as end says, and returns
// the mocks it emitted and what it returned.
func recordExchanges(t *testing.T, end streamEnd, pairs ...[]byte) ([]*models.Mock, error) {
	t.Helper()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	sess.Opts.SkipTLSMITM = end.observeOnly
	if end.withConn {
		sess.MarkEndedWithConnection(fakeconn.FromDest)
	}
	base := time.Unix(1_700_005_000, 0)
	for i := 0; i+1 < len(pairs); i += 2 {
		at := base.Add(time.Duration(i) * time.Millisecond)
		sendReq(pairs[i], at, at)
		sendResp(pairs[i+1], at.Add(time.Millisecond), at.Add(time.Millisecond))
	}
	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.recordV2(ctx, sess) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recordV2 did not exit within 2s")
	}
	var got []*models.Mock
	for {
		select {
		case m := <-mocks:
			got = append(got, m)
		default:
			return got, err
		}
	}
}

// recordOne is recordExchanges for one GET /version.
func recordOne(t *testing.T, end streamEnd, resp []byte) (*models.Mock, error) {
	t.Helper()
	got, err := recordExchanges(t, end, versionRequest, resp)
	if len(got) > 1 {
		t.Fatalf("%d mocks for one exchange", len(got))
	}
	if len(got) == 0 {
		return nil, err
	}
	return got[0], err
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRecordV2_ObserveOnly_ResponseReadToItsStatusLine(t *testing.T) {
	t.Parallel()
	// What the app's recv(256) returned: the response cut in its fifth
	// header line.
	m, err := recordOne(t, clientClosed, apiserverVersionResponse[:256])
	if err != nil {
		t.Fatalf("recordV2 returned %v for a response its client read in part; want nil (the exchange is recorded as read)", err)
	}
	if m == nil {
		t.Fatal("no mock for a response its client read in part: the test case that made the call is left without it")
	}
	if m.Spec.HTTPReq == nil || m.Spec.HTTPReq.URL != "/version" {
		t.Fatalf("mock request = %+v, want GET /version", m.Spec.HTTPReq)
	}
	r := m.Spec.HTTPResp
	if r == nil || r.StatusCode != 200 {
		t.Fatalf("mock response = %+v, want status 200", r)
	}
	// The header lines the client read in full are kept; the one it was
	// cut in, and those after it, it never had.
	for _, k := range []string{"Audit-Id", "Cache-Control", "Content-Type", "X-Kubernetes-Pf-Flowschema-Uid"} {
		if _, ok := r.Header[k]; !ok {
			t.Errorf("header %q, which the client read in full, is missing: %v", k, r.Header)
		}
	}
	for _, k := range []string{"X-Kubernetes-Pf-Prioritylevel-Uid", "Date"} {
		if v, ok := r.Header[k]; ok {
			t.Errorf("header %q = %q recorded, but the client never read it whole", k, v)
		}
	}
	if r.Body != "" {
		t.Errorf("body = %q, want empty: the client read none of it", r.Body)
	}
}

func TestRecordV2_ObserveOnly_ResponseReadInPartOfItsBody(t *testing.T) {
	t.Parallel()
	bodyAt := bytes.Index(apiserverVersionResponse, []byte("\r\n\r\n")) + 4
	m, err := recordOne(t, clientClosed, apiserverVersionResponse[:bodyAt+40])
	if err != nil {
		t.Fatalf("recordV2 returned %v; want nil", err)
	}
	if m == nil {
		t.Fatal("no mock for a response whose body its client read in part")
	}
	r := m.Spec.HTTPResp
	if want := string(apiserverVersionResponse[bodyAt : bodyAt+40]); r.Body != want {
		t.Errorf("body = %q, want the %d bytes the client read: %q", r.Body, len(want), want)
	}
	if _, ok := r.Header["Date"]; !ok {
		t.Errorf("a header the client read is missing: %v", r.Header)
	}
	// Replay serves the body it has: the length is that body's.
	if got := r.Header["Content-Length"]; got != "40" {
		t.Errorf("Content-Length = %q, want 40 (the recorded body's)", got)
	}
}

func TestRecordV2_ObserveOnly_ChunkedResponseReadInPart(t *testing.T) {
	t.Parallel()
	got, err := recordExchanges(t, clientClosed, []byte("GET /stream HTTP/1.1\r\nHost: ex.com\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n wo"))
	if err != nil {
		t.Fatalf("recordV2 returned %v; want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d mocks, want 1", len(got))
	}
	if got[0].Spec.HTTPResp.Body != "hello wo" {
		t.Errorf("body = %q, want %q (what the client read, de-chunked)", got[0].Spec.HTTPResp.Body, "hello wo")
	}
}

// A gzip body is recorded as far as it decodes: all the client could have
// decoded. One the client did not read at all (it wanted the headers) is
// recorded empty, not dropped for failing to decode.
func TestRecordV2_ObserveOnly_GzipBodyReadInPart(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("keploy records what the client read. ", 400)
	z := gzipped(t, text)
	head := "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: " + strconv.Itoa(len(z)) + "\r\n\r\n"

	m, err := recordOne(t, clientClosed, append([]byte(head), z[:len(z)/2]...))
	if err != nil || m == nil {
		t.Fatalf("half a gzip body: mock %v, err %v; want a mock", m, err)
	}
	if b := m.Spec.HTTPResp.Body; b == "" || !strings.HasPrefix(text, b) {
		t.Errorf("body is not a leading part of what the server sent (%d of %d bytes)", len(b), len(text))
	}

	m, err = recordOne(t, clientClosed, []byte(head))
	if err != nil || m == nil {
		t.Fatalf("a gzip body the client did not read: mock %v, err %v; want a mock", m, err)
	}
	if m.Spec.HTTPResp.Body != "" {
		t.Errorf("body = %q, want empty", m.Spec.HTTPResp.Body)
	}
}

// A response cut after an interim 100 Continue is recorded as its final
// response, not as the 100.
func TestRecordV2_ObserveOnly_FinalResponseReadInPartAfterInterim(t *testing.T) {
	t.Parallel()
	got, err := recordExchanges(t, clientClosed,
		[]byte("POST /up HTTP/1.1\r\nHost: ex.com\r\nContent-Length: 2\r\nExpect: 100-continue\r\n\r\nhi"),
		[]byte("HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 201 Created\r\nLocation: /up/1\r\nContent-Ty"))
	if err != nil {
		t.Fatalf("recordV2 returned %v; want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d mocks, want 1", len(got))
	}
	if r := got[0].Spec.HTTPResp; r.StatusCode != 201 || r.Header["Location"] != "/up/1" {
		t.Errorf("response = %d %v, want the final 201 with its Location", r.StatusCode, r.Header)
	}
}

// An interim response is never recorded as the answer: served at replay, the
// client would wait for the final one forever. Whether the stream ended in the
// interim's head, after it, or in the final one's status line, and whoever
// ended it, there is no final response to record.
func TestRecordV2_InterimResponseIsNeverTheAnswer(t *testing.T) {
	t.Parallel()
	for name, resp := range map[string]string{
		"cut in a 103's head":          "HTTP/1.1 103 Early Hints\r\nLink: </a>\r\nLi",
		"a 100 and nothing after":      "HTTP/1.1 100 Continue\r\n\r\n",
		"cut in the final status line": "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 2",
	} {
		for _, end := range []streamEnd{clientClosed, relayedClosed} {
			m, err := recordOne(t, end, []byte(resp))
			if m != nil {
				t.Errorf("%s (%+v): recorded a %d as the answer", name, end, m.Spec.HTTPResp.StatusCode)
			}
			if err == nil {
				t.Errorf("%s (%+v): recordV2 returned nil; want the parse error", name, end)
			}
		}
	}
}

// Nothing is made up: a client that did not read a whole status line has no
// status to record.
func TestRecordV2_ObserveOnly_StatusLineReadInPart(t *testing.T) {
	t.Parallel()
	m, err := recordOne(t, clientClosed, []byte("HTTP/1.1 20"))
	if err == nil {
		t.Error("recordV2 returned nil for a response cut in its status line; want the parse error")
	}
	if m != nil {
		t.Errorf("mock emitted for a response with no status: %+v", m.Spec.HTTPResp)
	}
}

// Where keploy relays the connection (proxy mode) it reads the whole response
// from the server whatever the client reads, so a response cut short there is
// the server's doing. And where the capture's stream did not end with its
// connection (a hole, the recording stopping), the rest of the response was
// lost. Neither is recorded, in its head or in its body: the parser fails as
// before, and the connection's test cases are left out.
func TestRecordV2_ResponseCutShortOtherwiseIsNotRecorded(t *testing.T) {
	t.Parallel()
	bodyAt := bytes.Index(apiserverVersionResponse, []byte("\r\n\r\n")) + 4
	for _, end := range []streamEnd{relayedClosed, captureCut} {
		for name, cut := range map[string]int{"in the head": 256, "in the body": bodyAt + 40} {
			m, err := recordOne(t, end, apiserverVersionResponse[:cut])
			if err == nil {
				t.Errorf("%+v, cut %s: recordV2 returned nil; want the parse error", end, name)
			}
			if m != nil {
				t.Errorf("%+v, cut %s: mock emitted: %+v", end, name, m.Spec.HTTPResp)
			}
		}
	}
}

// Whole responses record the same mock whether or not the stream ended with
// the client closing: the flag changes only what does not parse whole.
func TestRecordV2_WholeResponsesAreRecordedAlike(t *testing.T) {
	t.Parallel()
	for name, resp := range map[string]string{
		"close-delimited":      "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nhello until close",
		"bare-LF header lines": "HTTP/1.1 200 OK\r\nContent-Type: text/plain\n\nline1\r\nline2",
		"content-length":       "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello",
		"chunked":              "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
		"no content":           "HTTP/1.1 204 No Content\r\n\r\n",
	} {
		read, err := recordOne(t, clientClosed, []byte(resp))
		if err != nil || read == nil {
			t.Fatalf("%s: mock %v, err %v", name, read, err)
		}
		plain, err := recordOne(t, captureCut, []byte(resp))
		if err != nil || plain == nil {
			t.Fatalf("%s: mock %v, err %v", name, plain, err)
		}
		if !reflect.DeepEqual(read.Spec.HTTPResp, plain.Spec.HTTPResp) {
			t.Errorf("%s: recorded %+v with the client's close, %+v without", name, read.Spec.HTTPResp, plain.Spec.HTTPResp)
		}
	}
}

// On a keep-alive connection only the response the stream ends in is read in
// part; the whole ones before it are recorded as they are.
func TestRecordV2_ObserveOnly_KeepAliveLastResponseReadInPart(t *testing.T) {
	t.Parallel()
	got, err := recordExchanges(t, clientClosed,
		[]byte("GET /a HTTP/1.1\r\nHost: ex.com\r\n\r\n"), []byte("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc"),
		versionRequest, apiserverVersionResponse[:256])
	if err != nil {
		t.Fatalf("recordV2 returned %v; want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d mocks, want 2", len(got))
	}
	if got[0].Spec.HTTPResp.Body != "abc" || got[1].Spec.HTTPResp.StatusCode != 200 || got[1].Spec.HTTPReq.URL != "/version" {
		t.Errorf("mocks = %+v, %+v", got[0].Spec.HTTPResp, got[1].Spec.HTTPResp)
	}
}

func TestResponseAsRead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, in, want string }{
		{"whole head", "HTTP/1.1 200 OK\r\nA: 1\r\n\r\nbody", "HTTP/1.1 200 OK\r\nA: 1\r\n\r\nbody"},
		{"cut mid-line", "HTTP/1.1 200 OK\r\nA: 1\r\nB: 2", "HTTP/1.1 200 OK\r\nA: 1\r\n\r\n"},
		{"cut after a line's CRLF", "HTTP/1.1 200 OK\r\nA: 1\r\n", "HTTP/1.1 200 OK\r\nA: 1\r\n\r\n"},
		{"cut between CR and LF", "HTTP/1.1 200 OK\r\nA: 1\r", "HTTP/1.1 200 OK\r\n\r\n"},
		{"cut in the blank line", "HTTP/1.1 200 OK\r\nA: 1\r\n\r", "HTTP/1.1 200 OK\r\nA: 1\r\n\r\n"},
		{"cut in the status line", "HTTP/1.1 20", "HTTP/1.1 20"},
		{"past an interim", "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 201 Created\r\nL: /x\r\nC", "HTTP/1.1 201 Created\r\nL: /x\r\n\r\n"},
		{"an interim and nothing after", "HTTP/1.1 100 Continue\r\n\r\n", ""},
		{"cut in an interim", "HTTP/1.1 103 Early Hints\r\nLink: </a>\r\nLi", "HTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\n"},
	} {
		if got := string(responseAsRead([]byte(c.in))); got != c.want {
			t.Errorf("%s: responseAsRead(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// Interim responses are read off one after another, whatever their line
// endings: the final response after a bare-LF 100 is recorded, in any mode.
func TestRecordV2_FinalResponseAfterABareLFInterim(t *testing.T) {
	t.Parallel()
	for _, end := range []streamEnd{clientClosed, relayedClosed} {
		m, err := recordOne(t, end, []byte("HTTP/1.1 100 Continue\n\nHTTP/1.1 200 OK\nContent-Length: 5\n\nhello"))
		if err != nil || m == nil {
			t.Fatalf("%+v: mock %v, err %v; want the 200", end, m, err)
		}
		if r := m.Spec.HTTPResp; r.StatusCode != 200 || r.Body != "hello" {
			t.Errorf("%+v: recorded %d %q, want 200 \"hello\"", end, r.StatusCode, r.Body)
		}
	}
}

// A gzip body with no length of its own (delimited by the close) that its
// client stopped reading decodes as far as it goes, like one with a length.
func TestRecordV2_ObserveOnly_CloseDelimitedGzipBodyReadInPart(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("keploy records what the client read. ", 400)
	z := gzipped(t, text)
	m, err := recordOne(t, clientClosed, append([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\n\r\n"), z[:len(z)/2]...))
	if err != nil || m == nil {
		t.Fatalf("mock %v, err %v; want a mock", m, err)
	}
	if b := m.Spec.HTTPResp.Body; b == "" || !strings.HasPrefix(text, b) {
		t.Errorf("body is not a leading part of what the server sent (%d of %d bytes)", len(b), len(text))
	}
}

// Only a stream that ran out ended where its capture did. One whose reader was
// closed under it (fakeconn.ErrClosed: an abort, with chunks possibly still on
// their way) is not read in part, even where the capture had marked the end.
func TestRecordV2_ObserveOnly_ClosedReaderIsNotReadInPart(t *testing.T) {
	t.Parallel()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, _, mocks := newTestSession(t)
	sess.Opts.SkipTLSMITM = true
	sess.MarkEndedWithConnection(fakeconn.FromDest)
	at := time.Unix(1_700_005_000, 0)
	sendReq(versionRequest, at, at)
	closeReq()
	sendResp(apiserverVersionResponse[:256], at, at)

	done := make(chan error, 1)
	go func() { done <- h.recordV2(context.Background(), sess) }()
	deadline := time.Now().Add(2 * time.Second)
	for waiting, _ := sess.DestStream.Waiting(); !waiting; waiting, _ = sess.DestStream.Waiting() {
		if time.Now().After(deadline) {
			t.Fatal("the parser never waited for more of the response")
		}
		time.Sleep(time.Millisecond)
	}
	_ = sess.DestStream.Close() // its reader is closed; its chunk channel is not
	select {
	case err := <-done:
		if err == nil {
			t.Error("recordV2 returned nil for a response whose reader was closed under it; want the parse error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recordV2 did not exit within 2s")
	}
	select {
	case m := <-mocks:
		t.Errorf("mock emitted for a response whose reader was closed under it: %+v", m.Spec.HTTPResp)
	default:
	}
}

// An interim net/http will not read (a malformed header line in a 103) does not
// cost the final response behind it: it is framed by its blank lines, as it
// was before the interims were read one by one.
func TestRecordV2_FinalResponseBehindAMalformedInterim(t *testing.T) {
	t.Parallel()
	for _, end := range []streamEnd{relayedClosed, captureCut, clientClosed} {
		m, err := recordOne(t, end, []byte("HTTP/1.1 103 Early Hints\r\nBad Header Line\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
		if err != nil || m == nil {
			t.Fatalf("%+v: mock %v, err %v; want the 200", end, m, err)
		}
		if r := m.Spec.HTTPResp; r.StatusCode != 200 || r.Body != "ok" {
			t.Errorf("%+v: recorded %d %q, want 200 \"ok\"", end, r.StatusCode, r.Body)
		}
	}
}

// A final response read in part behind a bare-LF interim is read as the client
// had it from where it begins, not from the interim.
func TestRecordV2_ObserveOnly_FinalReadInPartBehindABareLFInterim(t *testing.T) {
	t.Parallel()
	m, err := recordOne(t, clientClosed, []byte("HTTP/1.1 100 Continue\n\nHTTP/1.1 200 OK\r\nA: b\r\nC"))
	if err != nil || m == nil {
		t.Fatalf("mock %v, err %v; want the 200", m, err)
	}
	if r := m.Spec.HTTPResp; r.StatusCode != 200 || r.Header["A"] != "b" {
		t.Errorf("recorded %d %v, want 200 with A: b", r.StatusCode, r.Header)
	}
}

// A response with no body by rule (to a HEAD, a 204, a 304) is recorded, or
// not, as a whole one is, even read in part: its Content-Encoding describes a
// body it does not carry, and replay would put one on the wire.
func TestRecordV2_ObserveOnly_NoBodyResponseReadInPartIsRecordedAsAWholeOne(t *testing.T) {
	t.Parallel()
	head := []byte("HEAD /f HTTP/1.1\r\nHost: ex.com\r\n\r\n")
	whole, wholeErr := recordExchanges(t, relayedClosed, head, []byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: 50\r\n\r\n"))
	read, readErr := recordExchanges(t, clientClosed, head, []byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Len"))
	if (wholeErr == nil) != (readErr == nil) || len(whole) != len(read) {
		t.Errorf("whole: %d mock(s), err %v; read in part: %d mock(s), err %v; want the same outcome", len(whole), wholeErr, len(read), readErr)
	}
}
