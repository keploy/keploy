package http

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// pipelined is a pipeline of three requests, each with a different body
// framing (none, Content-Length, chunked), and their answers in order, each
// framed differently too.
var (
	pipelinedReqs = []string{
		"GET /one HTTP/1.1\r\nHost: ex\r\n\r\n",
		"POST /two HTTP/1.1\r\nHost: ex\r\nContent-Length: 3\r\n\r\nabc",
		"POST /three HTTP/1.1\r\nHost: ex\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nxyz\r\n0\r\n\r\n",
	}
	pipelinedResps = []string{
		"HTTP/1.1 200 OK\r\nContent-Length: 1\r\n\r\n1",
		"HTTP/1.1 201 Created\r\nTransfer-Encoding: chunked\r\n\r\n1\r\n2\r\n0\r\n\r\n",
		"HTTP/1.1 202 Accepted\r\nContent-Length: 1\r\n\r\n3",
	}
	pipelinedURLs     = []string{"/one", "/two", "/three"}
	pipelinedReqBody  = []string{"", "abc", "xyz"}
	pipelinedStatuses = []int{200, 201, 202}
	pipelinedRespBody = []string{"1", "2", "3"}
)

// splitAt cuts s at the given offsets, in order, into the pieces a capture
// would hand on as chunks.
func splitAt(s string, offsets ...int) []string {
	var out []string
	from := 0
	for _, o := range offsets {
		out = append(out, s[from:o])
		from = o
	}
	return append(out, s[from:])
}

// A client that pipelines its requests (HTTP/1.1 keep-alive: it sends the
// next request before the answer to the one before has come) writes them in
// one write as often as not, and the server writes its answers back to back,
// so a capture's chunk can hold more than one message, and a message can end
// partway through one. Each request/answer pair is recorded, in order, however
// the capture cut the two streams: before, the parser took whole chunks, so a
// chunk holding several requests or answers was recorded as the first of them
// and the rest were lost, with nothing reporting them.
func TestRecordV2_PipelinedRequestsAreEachRecorded(t *testing.T) {
	t.Parallel()
	reqs := strings.Join(pipelinedReqs, "")
	resps := strings.Join(pipelinedResps, "")
	r1, r2 := len(pipelinedReqs[0]), len(pipelinedReqs[0])+len(pipelinedReqs[1])
	a1, a2 := len(pipelinedResps[0]), len(pipelinedResps[0])+len(pipelinedResps[1])
	cases := []struct {
		name         string
		reqs, resps  []string
		wantReqChunk []int
	}{
		{
			name:         "every request in one write, every answer in one",
			reqs:         []string{reqs},
			resps:        []string{resps},
			wantReqChunk: []int{0, 0, 0},
		},
		{
			name:         "every request in one write, each answer on its own",
			reqs:         []string{reqs},
			resps:        pipelinedResps,
			wantReqChunk: []int{0, 0, 0},
		},
		{
			name: "requests and answers cut inside messages",
			// The second request starts in the first chunk and ends in the
			// second, where the third starts; each answer but the first ends
			// in a chunk the next one starts in.
			reqs:         splitAt(reqs, r1+10, r2+5),
			resps:        splitAt(resps, a1+3, a2-2),
			wantReqChunk: []int{0, 0, 1},
		},
		{
			name: "requests one per write, answers in one",
			// The client sent all three before the server answered any.
			reqs:         pipelinedReqs,
			resps:        []string{resps},
			wantReqChunk: []int{0, 1, 2},
		},
		{
			name: "a request cut at its head's end, the rest with the next",
			reqs: splitAt(reqs, strings.Index(reqs, "\r\n\r\n")+4-1,
				r1+strings.Index(pipelinedReqs[1], "\r\n\r\n")+4),
			resps:        pipelinedResps,
			wantReqChunk: []int{0, 1, 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := recordChunks(t, tc.reqs, tc.resps)
			if err != nil {
				t.Fatalf("recordV2: %v", err)
			}
			want := make([]pipeMock, len(pipelinedURLs))
			for i := range want {
				want[i] = pipeMock{pipelinedURLs[i], pipelinedReqBody[i], pipelinedStatuses[i], pipelinedRespBody[i],
					chunkReadAt(tc.wantReqChunk[i])}
			}
			checkRecorded(t, got, want)
		})
	}
}

// pipeMock is what a test wants of one recorded mock.
type pipeMock struct {
	url, reqBody string
	status       int
	respBody     string
	reqAt        time.Time
}

// chunkReadAt and chunkWrittenAt are the capture times recordChunks gives
// the i-th request chunk and the i-th response chunk.
func chunkReadAt(i int) time.Time {
	return time.Unix(1_700_010_000, 0).Add(time.Duration(i) * time.Millisecond)
}

func chunkWrittenAt(i int) time.Time {
	return time.Unix(1_700_010_001, 0).Add(time.Duration(i) * time.Millisecond)
}

// recordChunks runs recordV2 over a connection whose client sent reqs and
// whose server then sent resps, each a chunk as the capture cut them, every
// request chunk captured before any response chunk. It returns the mocks
// recordV2 emitted, in order, and its error.
func recordChunks(t *testing.T, reqs, resps []string) ([]*models.Mock, error) {
	t.Helper()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	for i, c := range reqs {
		sendReq([]byte(c), chunkReadAt(i), chunkReadAt(i))
	}
	for i, c := range resps {
		sendResp([]byte(c), chunkWrittenAt(i), chunkWrittenAt(i))
	}
	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := h.recordV2(ctx, sess)
	close(mocks)
	var got []*models.Mock
	for m := range mocks {
		got = append(got, m)
	}
	return got, err
}

// checkRecorded fails t unless got are the mocks want names, in order.
func checkRecorded(t *testing.T, got []*models.Mock, want []pipeMock) {
	t.Helper()
	if len(got) != len(want) {
		urls := make([]string, 0, len(got))
		for _, m := range got {
			urls = append(urls, m.Spec.HTTPReq.URL)
		}
		t.Fatalf("recorded %d mocks %v, want one per request: %d", len(got), urls, len(want))
	}
	for i, m := range got {
		w := want[i]
		if m.Spec.HTTPReq.URL != w.url || m.Spec.HTTPReq.Body != w.reqBody {
			t.Errorf("mock %d's request = %s %q, want %s %q", i, m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Body, w.url, w.reqBody)
		}
		if m.Spec.HTTPResp.StatusCode != w.status || m.Spec.HTTPResp.Body != w.respBody {
			t.Errorf("mock %d's answer = %d %q, want %d %q", i, m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body,
				w.status, w.respBody)
		}
		if !m.Spec.ReqTimestampMock.Equal(w.reqAt) {
			t.Errorf("mock %d's request time = %v, want its first chunk's %v", i, m.Spec.ReqTimestampMock, w.reqAt)
		}
	}
}

// The answers a pipeline's framing reads past without a body: the answer to a
// HEAD, a 204 and a 304, whatever their Content-Length says, end at their
// head, and the next answer starts there. The empty line a client may send
// after a request's body, before the next request, is no part of either. The
// last request ends with its Content-Length's last byte, where the client's
// stream ends, and the last answer, with neither Content-Length nor chunked
// framing, runs to the server's close.
func TestRecordV2_PipelinedAnswersWithoutBodies(t *testing.T) {
	t.Parallel()
	reqs := "HEAD /head HTTP/1.1\r\nHost: ex\r\n\r\n" +
		"POST /none HTTP/1.1\r\nHost: ex\r\nContent-Length: 2\r\n\r\nhi\r\n" +
		"GET /same HTTP/1.1\r\nHost: ex\r\n\r\n" +
		"POST /end HTTP/1.1\r\nHost: ex\r\nContent-Length: 3\r\n\r\nend"
	resps := "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n" +
		"HTTP/1.1 204 No Content\r\nContent-Length: 3\r\n\r\n" +
		"HTTP/1.1 304 Not Modified\r\nContent-Length: 4\r\n\r\n" +
		"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nto the end"
	got, err := recordChunks(t, []string{reqs}, []string{resps})
	if err != nil {
		t.Fatalf("recordV2: %v", err)
	}
	at := chunkReadAt(0)
	checkRecorded(t, got, []pipeMock{
		{"/head", "", 200, "", at},
		{"/none", "hi", 204, "", at},
		{"/same", "", 304, "", at},
		{"/end", "end", 200, "to the end", at},
	})
}

// A chunk with no bytes, in the middle of a request or an answer, is taken
// and the message read on past it.
func TestRecordV2_PipelinedPastAnEmptyChunk(t *testing.T) {
	t.Parallel()
	reqs := strings.Join(pipelinedReqs, "")
	resps := strings.Join(pipelinedResps, "")
	cut, cutResp := len(pipelinedReqs[0])+5, len(pipelinedResps[0])+5
	got, err := recordChunks(t, []string{reqs[:cut], "", reqs[cut:]}, []string{resps[:cutResp], "", resps[cutResp:]})
	if err != nil {
		t.Fatalf("recordV2: %v", err)
	}
	want := make([]pipeMock, len(pipelinedURLs))
	for i := range want {
		want[i] = pipeMock{pipelinedURLs[i], pipelinedReqBody[i], pipelinedStatuses[i], pipelinedRespBody[i], chunkReadAt(0)}
	}
	want[2].reqAt = chunkReadAt(2)
	checkRecorded(t, got, want)
}

// A negative Content-Length frames no message: the request is a decode error,
// as it was when net/http refused it after the whole chunk was taken for it,
// not a message that ends inside its own head.
func TestRecordV2_NegativeContentLengthIsADecodeError(t *testing.T) {
	t.Parallel()
	got, err := recordChunks(t,
		[]string{"POST /neg HTTP/1.1\r\nHost: ex\r\nContent-Length: -2\r\n\r\nGET /next HTTP/1.1\r\nHost: ex\r\n\r\n"},
		[]string{"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"})
	if err == nil || !errors.Is(err, errBadContentLength) {
		t.Fatalf("recordV2 = %v, want the Content-Length's decode error", err)
	}
	if len(got) != 0 {
		t.Fatalf("recorded %d mocks (first %s), want none", len(got), got[0].Spec.HTTPReq.URL)
	}
}

// A response's time is the WrittenAt of the last chunk it took bytes of that
// carries one: a chunk with none moves nothing.
func TestRecordV2_PipelinedAnswerTimeIsItsLastStampedChunks(t *testing.T) {
	t.Parallel()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	reqs := strings.Join(pipelinedReqs[:2], "")
	resps := strings.Join(pipelinedResps[:2], "")
	cut := len(pipelinedResps[0]) + 4
	sendReq([]byte(reqs), chunkReadAt(0), chunkReadAt(0))
	sendResp([]byte(resps[:cut]), chunkWrittenAt(0), chunkWrittenAt(0))
	sendResp([]byte(resps[cut:cut+4]), chunkWrittenAt(1), chunkWrittenAt(1))
	sendResp([]byte(resps[cut+4:]), chunkWrittenAt(2), time.Time{})
	closeReq()
	closeResp()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.recordV2(ctx, sess); err != nil {
		t.Fatalf("recordV2: %v", err)
	}
	close(mocks)
	var got []*models.Mock
	for m := range mocks {
		got = append(got, m)
	}
	checkRecorded(t, got, []pipeMock{
		{pipelinedURLs[0], pipelinedReqBody[0], pipelinedStatuses[0], pipelinedRespBody[0], chunkReadAt(0)},
		{pipelinedURLs[1], pipelinedReqBody[1], pipelinedStatuses[1], pipelinedRespBody[1], chunkReadAt(0)},
	})
	for i, want := range []time.Time{chunkWrittenAt(0), chunkWrittenAt(1)} {
		if !got[i].Spec.ResTimestampMock.Equal(want) {
			t.Errorf("mock %d's answer time = %v, want %v", i, got[i].Spec.ResTimestampMock, want)
		}
	}
}
