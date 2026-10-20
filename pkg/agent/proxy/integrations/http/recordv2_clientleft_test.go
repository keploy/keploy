package http

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	yamlLib "gopkg.in/yaml.v3"
)

// Where keploy relays a connection (proxy mode, TLS MITM), a browser that
// navigates away while it reads a progress stream closes the connection in
// the middle of the response. keploy's next write of the server's bytes to it
// fails, and the relay stops: the server's stream ends there, in the middle
// of the response. The recorder used to fail on it ("read response body:
// unexpected EOF"), and with the write error marked on the exchange the mock
// was left out: the test that made the call had no recorded answer at replay.
//
// The relay says why the stream ended (Session.MarkReceiverGone: the client
// left, and how much of the failed write never reached it). Under the proven
// switch the response is then recorded as keploy relayed it to the client.

var progressRequest = []byte("POST /atg/progress HTTP/1.1\r\nHost: localhost:8083\r\n" +
	"Content-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")

const progressHead = "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n"

// ndjsonChunk is one NDJSON line as the server flushes it: one HTTP chunk.
func ndjsonChunk(line string) []byte {
	return []byte(fmt.Sprintf("%x\r\n%s\n\r\n", len(line)+1, line))
}

// recordClientLeft runs recordV2 over one relayed connection that carried
// req and then the response's chunks, the last of which keploy could not
// write on to the client: undelivered bytes at its end never reached it. The
// session is marked as the relay marks it (MarkReceiverGone, then the
// incomplete-mock flag with mark), before the streams end.
func recordClientLeft(t *testing.T, gone bool, mark string, undelivered int, req []byte, resp ...[]byte) ([]*models.Mock, error) {
	t.Helper()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	at := time.Unix(1_700_006_000, 0)
	sendReq(req, at, at)
	for i, c := range resp {
		ts := at.Add(time.Duration(i+1) * time.Millisecond)
		sendResp(c, ts, ts)
	}
	if gone {
		sess.MarkReceiverGone(fakeconn.FromDest, undelivered)
	}
	if mark != "" {
		sess.MarkMockIncomplete(mark)
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

func TestRecordV2_ClientLeftAStreamMidway(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	last := ndjsonChunk(`{"progress":3}`)
	got, err := recordClientLeft(t, true, relay.MarkWriteError, len(last), progressRequest,
		[]byte(progressHead), ndjsonChunk(`{"progress":1}`), ndjsonChunk(`{"progress":2}`), last)
	if err != nil {
		t.Fatalf("recordV2 returned %v; want nil (the exchange is recorded as the client had it)", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d mocks, want 1: the call the client left midway has no recorded answer", len(got))
	}
	m := got[0]
	// The write that failed never reached the client: its line is not in
	// the answer. The lines the client was handed are, each with its newline.
	if want := "{\"progress\":1}\n{\"progress\":2}\n"; m.Spec.HTTPResp.Body != want {
		t.Errorf("body %q, want %q", m.Spec.HTTPResp.Body, want)
	}
	if cl := m.Spec.HTTPResp.Header["Content-Length"]; cl != "30" {
		t.Errorf("Content-Length %q, want 30 (the recorded part's)", cl)
	}
	if m.Spec.Metadata[metaClientLeft] != "true" {
		t.Errorf("metadata %v: the mock does not say its client left before the end", m.Spec.Metadata)
	}
	// It is the data of the test that made it, as every exchange is.
	if m.Spec.Metadata[models.MetadataLifetimeKey] != models.MetadataLifetimePerTest || m.IsProtocolPhase() {
		t.Errorf("metadata %v: a client-left exchange is still its test's data", m.Spec.Metadata)
	}
}

// Of the failed write, what reached the client is kept, and the rest is not.
func TestRecordV2_ClientLeftKeepsWhatAFailedWriteDelivered(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	body := strings.Repeat("a", 30) + strings.Repeat("b", 30)
	got, err := recordClientLeft(t, true, relay.MarkWriteError, 10, versionRequest,
		[]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 100\r\n\r\n"+body[:30]), []byte(body[30:]))
	if err != nil || len(got) != 1 {
		t.Fatalf("recordV2: %d mocks, err %v; want 1, nil", len(got), err)
	}
	if want := body[:50]; got[0].Spec.HTTPResp.Body != want {
		t.Errorf("body %q, want the %d bytes the client was handed", got[0].Spec.HTTPResp.Body, len(want))
	}
}

// A response whose end is the server's close (no length, not chunked) that
// the client left is unfinished too: it never saw the close.
func TestRecordV2_ClientLeftACloseDelimitedResponse(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	got, err := recordClientLeft(t, true, relay.MarkWriteError, len("line2\n"), versionRequest,
		[]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nline1\n"), []byte("line2\n"))
	if err != nil || len(got) != 1 {
		t.Fatalf("recordV2: %d mocks, err %v; want 1, nil", len(got), err)
	}
	if got[0].Spec.HTTPResp.Body != "line1\n" || got[0].Spec.Metadata[metaClientLeft] != "true" {
		t.Errorf("body %q, metadata %v; want line1 and the client's leaving", got[0].Spec.HTTPResp.Body, got[0].Spec.Metadata)
	}
}

// Only the client's going away makes a cut response one to record, and only
// under the proven switch: a stream the server ended (nothing marked), the
// switch off, or a mark of a capture loss (a dropped chunk) records nothing,
// as before.
func TestRecordV2_ACutResponseIsRecordedOnlyWhenTheClientLeft(t *testing.T) {
	chunks := [][]byte{[]byte(progressHead), ndjsonChunk(`{"progress":1}`)}
	for _, tc := range []struct {
		name    string
		proven  string
		gone    bool
		mark    string
		wantErr bool
	}{
		{"the server cut it", "1", false, "", true},
		{"the switch is off", "", true, relay.MarkWriteError, true},
		{"the capture lost a chunk", "1", true, relay.DropPerConnCap, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			got, err := recordClientLeft(t, tc.gone, tc.mark, 0, progressRequest, chunks...)
			if len(got) != 0 {
				t.Fatalf("mock recorded: %+v", got[0].Spec.HTTPResp)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("recordV2 returned %v; want an error: %v", err, tc.wantErr)
			}
		})
	}
}

// A replay serves a client-left response as the client had it: the recorded
// part, byte for byte (an NDJSON line's newline too, which the app's reader
// waits for), and no end: the response stays open until the client goes.
// Recorded, written as mocks.yaml writes it, read back and served.
func TestAClientLeftResponseIsServedUnfinished(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	last := ndjsonChunk(`{"progress":3}`)
	got, err := recordClientLeft(t, true, relay.MarkWriteError, len(last), progressRequest,
		[]byte(progressHead), ndjsonChunk(`{"progress":1}`), ndjsonChunk(`{"progress":2}`), last)
	if err != nil || len(got) != 1 {
		t.Fatalf("recordV2: %d mocks, err %v", len(got), err)
	}
	served := serveFromDisk(t, got[0])
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(served)), nil)
	if err != nil {
		t.Fatalf("the served response does not parse: %v\n%s", err, served)
	}
	if resp.ContentLength != -1 || len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("served framing: length %d, transfer-encoding %v; want an open-ended chunked response\n%s",
			resp.ContentLength, resp.TransferEncoding, served)
	}
	body, err := io.ReadAll(resp.Body)
	if want := "{\"progress\":1}\n{\"progress\":2}\n"; string(body) != want {
		t.Errorf("served body %q, want %q", body, want)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("reading the served response to its end: %v; want it never to end (io.ErrUnexpectedEOF here)", err)
	}
}

// A whole NDJSON answer is served as it was recorded, byte for byte: its
// last line keeps its newline. A mock wrote it as the stream's lines, which
// read back without it, so the app's line reader never saw the last line.
func TestAStreamedMockIsServedByteForByte(t *testing.T) {
	resp := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nContent-Length: 30\r\n\r\n" +
		"{\"progress\":1}\n{\"progress\":2}\n")
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	m, err := h.buildHTTPMock(&FinalHTTP{Req: versionRequest, Resp: resp, ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
		80, "test-conn", models.OutgoingOptions{})
	if err != nil || m == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", m, err)
	}
	served := serveFromDisk(t, m)
	got, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(served)), nil)
	if err != nil {
		t.Fatalf("the served response does not parse: %v\n%s", err, served)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read served body: %v", err)
	}
	if want := "{\"progress\":1}\n{\"progress\":2}\n"; string(body) != want {
		t.Errorf("served body %q, want %q", body, want)
	}
}

// serveFromDisk writes m as mocks.yaml writes it, reads it back and serves it.
func serveFromDisk(t *testing.T, m *models.Mock) []byte {
	t.Helper()
	doc, err := mockdb.EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	onDisk, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var readBack yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(onDisk, &readBack); err != nil {
		t.Fatal(err)
	}
	mocks, err := mockdb.DecodeMocks([]*yaml.NetworkTrafficDoc{&readBack}, zap.NewNop())
	if err != nil || len(mocks) != 1 {
		t.Fatalf("DecodeMocks = (%d mocks, %v)", len(mocks), err)
	}
	served, err := (&HTTP{Logger: zap.NewNop()}).buildMockResponseBytes(mocks[0])
	if err != nil {
		t.Fatalf("buildMockResponseBytes: %v", err)
	}
	return served
}
