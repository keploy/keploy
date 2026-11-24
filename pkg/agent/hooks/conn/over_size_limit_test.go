package conn

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// overSizeSession is a recording session on a manager of its own, logging to
// logger: the context a capture runs on for it, its not-recorded feed and its
// tally of requests over the size limit, both open.
type overSizeSession struct {
	ctx   context.Context
	mgr   *syncMock.SyncMockManager
	feed  <-chan models.NotRecordedCall
	tally *syncMock.OverSizeTally
}

func openOverSizeSession(t *testing.T, logger *zap.Logger) overSizeSession {
	t.Helper()
	mgr := syncMock.New(logger)
	feed, closeFeed := mgr.OpenNotRecordedFeed()
	t.Cleanup(func() { closeFeed() })
	tally := mgr.OpenOverSizeTally()
	t.Cleanup(tally.Close)
	return overSizeSession{ctx: syncMock.NewContext(context.Background(), mgr), mgr: mgr, feed: feed, tally: tally}
}

// toldNotRecorded is the calls the feed was told of so far.
func toldNotRecorded(feed <-chan models.NotRecordedCall) []models.NotRecordedCall {
	var out []models.NotRecordedCall
	for {
		select {
		case c := <-feed:
			out = append(out, c)
		default:
			return out
		}
	}
}

// captureOf runs Capture of a POST to target with reqBody and an answer of
// respBody, each with its Content-Encoding when one is given, on ctx, and
// returns the test cases it handed on.
func captureOf(t *testing.T, ctx context.Context, logger *zap.Logger, target string, reqBody []byte, reqEnc string, respBody []byte, respEnc string, opts models.IncomingOptions) []*models.TestCase {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://app.local"+target, bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	if reqEnc != "" {
		req.Header.Set("Content-Encoding", reqEnc)
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(respBody))}
	if respEnc != "" {
		resp.Header.Set("Content-Encoding", respEnc)
	}
	ch := make(chan *models.TestCase, 1)
	at := time.Unix(1000, 0)
	Capture(ctx, logger, ch, req, resp, at, at.Add(time.Millisecond), opts, false, false, 8080)
	close(ch)
	var out []*models.TestCase
	for tc := range ch {
		out = append(out, tc)
	}
	return out
}

// An HTTP exchange the capture leaves out for its size is counted on the
// recording's manager, once, by its call (method, path shape, path tag), and
// told to the recording's client by its path with why: the app served it, and
// the recording lacks it. Whatever made it too large: its two bodies together,
// or a body that inflates past the limit as it is decoded. One under the limit
// is a test case, and counted nowhere.
func TestCaptureCountsAndTellsAnExchangeOverTheSizeLimit(t *testing.T) {
	big := bytes.Repeat([]byte("x"), MaxTestCaseSize/2+1)
	for _, tc := range []struct {
		name              string
		reqBody, respBody []byte
		reqEnc, respEnc   string
		over              bool
	}{
		{"under the limit", []byte(`{"a":1}`), []byte("ok"), "", "", false},
		{"its two bodies together over the limit", big, big, "", "", true},
		{"a request body that inflates past the limit", gzipBomb(t), []byte("ok"), "gzip", "", true},
		{"a response body that inflates past the limit", []byte("hi"), gzipBomb(t), "", "gzip", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openOverSizeSession(t, zap.NewNop())
			ctx := s.ctx
			got := captureOf(t, ctx, zap.NewNop(), "/upload/abc", tc.reqBody, tc.reqEnc, tc.respBody, tc.respEnc, models.IncomingOptions{})
			counted := s.tally.Read()
			told := toldNotRecorded(s.feed)
			if !tc.over {
				if len(got) != 1 || counted.Total != 0 || len(told) != 0 {
					t.Fatalf("an exchange under the limit: %d test cases, %d counted over it, told %v; want a test case and nothing counted", len(got), counted.Total, told)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("an exchange over the limit was handed on as a test case")
			}
			tag := models.URLPathTag(ctx, http.MethodPost, &url.URL{Path: "/upload/abc"})
			call := models.WithPathTag("POST /{}/{}", tag)
			if counted.Total != 1 || counted.ByCall[call] != 1 {
				t.Fatalf("counted %+v, want the one request on %q", counted, call)
			}
			want := models.NotRecordedCall{Method: http.MethodPost, Path: "/upload/abc", PathTag: tag, Why: TestCaseLimit.Why()}
			if len(told) != 1 || told[0] != want {
				t.Fatalf("the recording's client was told %+v, want %+v", told, want)
			}
		})
	}
}

// An exchange over the limit that the recording's filters leave out anyway is
// neither counted nor told as one the size limit cost: the user asked for it
// not to be recorded.
func TestCaptureDoesNotCountAFilteredExchangeOverTheSizeLimit(t *testing.T) {
	s := openOverSizeSession(t, zap.NewNop())
	opts := models.IncomingOptions{Filters: []models.Filter{{BypassRule: models.BypassRule{Path: "/upload/.*"}, FilterPolicy: models.Exclude}}}
	for _, tc := range []struct {
		body []byte
		enc  string
	}{{bytes.Repeat([]byte("x"), MaxTestCaseSize+1), ""}, {gzipBomb(t), "gzip"}} {
		if got := captureOf(t, s.ctx, zap.NewNop(), "/upload/abc", tc.body, tc.enc, []byte("ok"), "", opts); len(got) != 0 {
			t.Fatal("a filtered exchange was handed on")
		}
	}
	if n := s.tally.Read().Total; n != 0 {
		t.Fatalf("filtered exchanges were counted over the size limit %d times", n)
	}
	if told := toldNotRecorded(s.feed); len(told) != 0 {
		t.Fatalf("filtered exchanges were told as not recorded: %v", told)
	}
}

// Capture reads the recording's filters once, on the request as it came, and
// then cuts a multipart body's Content-Type to its media type. An exchange it
// then finds over the limit (its response inflating past it, or its bodies
// together) is counted on that reading: read again on the cut header, a filter
// that kept the request (an Include on the boundary) would leave it out, and
// the exchange would go uncounted.
func TestCaptureReadsTheFiltersOnceForAnExchangeOverTheSizeLimit(t *testing.T) {
	opts := models.IncomingOptions{Filters: []models.Filter{{Headers: map[string]string{"Content-Type": "boundary="}, FilterPolicy: models.Include}}}
	for _, tc := range []struct {
		name     string
		respBody []byte
		respEnc  string
	}{
		{"a response body that inflates past the limit", gzipBomb(t), "gzip"},
		{"its bodies together over the limit", bytes.Repeat([]byte("y"), MaxTestCaseSize+1), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openOverSizeSession(t, zap.NewNop())
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			if err := mw.WriteField("note", "hello"); err != nil {
				t.Fatal(err)
			}
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, "http://app.local/upload", &body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", mw.FormDataContentType())
			if IsFiltered(zap.NewNop(), req, opts) {
				t.Fatal("precondition: the filter leaves the request out as it came")
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(tc.respBody))}
			if tc.respEnc != "" {
				resp.Header.Set("Content-Encoding", tc.respEnc)
			}
			ch := make(chan *models.TestCase, 1)
			at := time.Unix(1000, 0)
			Capture(s.ctx, zap.NewNop(), ch, req, resp, at, at.Add(time.Millisecond), opts, false, false, 8080)
			if len(ch) != 0 {
				t.Fatal("an exchange over the limit was handed on as a test case")
			}
			if n := s.tally.Read().Total; n != 1 {
				t.Fatalf("the exchange over the limit, kept by the filters as it came, was counted %d times, want once", n)
			}
		})
	}
}

// The ingress's own request carries its host in req.Host alone, where the
// request Capture filters has it in its Host header too: an exchange a filter
// on the Host header leaves out is not counted over the size limit at the
// ingress either, as Capture would not have recorded it.
func TestNoteOverSizeLimitReadsTheHostAsTheFiltersDo(t *testing.T) {
	s := openOverSizeSession(t, zap.NewNop())
	opts := models.IncomingOptions{Filters: []models.Filter{{Headers: map[string]string{"Host": "internal.app"}, FilterPolicy: models.Exclude}}}
	req, _ := http.NewRequest(http.MethodPost, "/upload", nil) // as the ingress reads one: no Host header
	req.Host = "internal.app"
	if req.Header.Get("Host") != "" {
		t.Fatal("precondition: the request carries a Host header")
	}
	NoteOverSizeLimit(s.ctx, zap.NewNop(), req, opts, BodyLimit)
	if n := s.tally.Read().Total; n != 0 {
		t.Fatalf("an exchange a filter on its Host leaves out was counted over the size limit %d times", n)
	}
	req.Host = "public.app"
	NoteOverSizeLimit(s.ctx, zap.NewNop(), req, opts, BodyLimit)
	if n := s.tally.Read().Total; n != 1 {
		t.Fatalf("an exchange no filter leaves out was counted %d times, want once", n)
	}
	if req.Header.Get("Host") != "" {
		t.Fatal("the count wrote a Host header into the ingress's request")
	}
}

// A gRPC call over the limit is counted and told as an HTTP call over it is:
// a POST to its :path.
func TestCaptureGRPCCountsAndTellsACallOverTheSizeLimit(t *testing.T) {
	s := openOverSizeSession(t, zap.NewNop())
	ctx := s.ctx
	const path = "/orders.Orders/Get"
	stream := &pkg.HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":path": path, ":authority": "app:9090", ":method": "POST", ":scheme": "http"}, OrdinaryHeaders: map[string]string{}},
			Body:    models.GrpcLengthPrefixedMessage{DecodedData: strings.Repeat("c", MaxTestCaseSize+1)},
		},
		GRPCResp: &models.GrpcResp{},
	}
	ch := make(chan *models.TestCase, 1)
	CaptureGRPC(ctx, zap.NewNop(), ch, stream, 8080, false, false)
	if len(ch) != 0 {
		t.Fatal("a gRPC call over the limit was handed on as a test case")
	}
	tag := models.URLPathTag(ctx, http.MethodPost, &url.URL{Path: path})
	if got := s.tally.Read(); got.Total != 1 || got.ByCall[models.WithPathTag("POST /{}/{}", tag)] != 1 {
		t.Fatalf("counted %+v, want the one call", got)
	}
	want := models.NotRecordedCall{Method: http.MethodPost, Path: path, PathTag: tag, Why: TestCaseLimit.Why()}
	if told := toldNotRecorded(s.feed); len(told) != 1 || told[0] != want {
		t.Fatalf("the recording's client was told %+v, want %+v", told, want)
	}
}

// NoteOverSizeLimit warns of the agent's first request over a limit as it
// comes, naming the call by its shape and tag and the limit, and of the next
// ones only at the sampling rate: the summary counts them all.
func TestNoteOverSizeLimitWarnsOfTheFirst(t *testing.T) {
	s := openOverSizeSession(t, zap.NewNop())
	ctx := s.ctx
	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)
	req, _ := http.NewRequest(http.MethodGet, "http://app.local/files/1?token=t", nil)
	for i := 0; i < 3; i++ {
		NoteOverSizeLimit(ctx, logger, req, models.IncomingOptions{}, BodyLimit)
	}
	if n := s.tally.Read().Total; n != 3 {
		t.Fatalf("counted %d, want 3", n)
	}
	warned := logs.All()
	if len(warned) != 1 {
		t.Fatalf("warned %d times, want once: %v", len(warned), warned)
	}
	f := warned[0].ContextMap()
	if !strings.Contains(warned[0].Message, BodyLimit.Why()) || f["limit_bytes"] != int64(MaxTestCaseBodySize) ||
		f["call"] != models.WithPathTag("GET /{}/{}", models.URLPathTag(ctx, http.MethodGet, req.URL)) || f["next_step"] != models.OverSizeLimitNextStep {
		t.Fatalf("the warning %q says %v", warned[0].Message, f)
	}
}

// A request no recording takes (its recording has ended and none has started
// since) is counted as one no recording took, not as one a recording lost to
// its size, and told to no one.
func TestNoteOverSizeLimitAfterTheRecordingEnded(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	s := openOverSizeSession(t, zap.New(core))
	s.mgr.EndRecording()
	req, _ := http.NewRequest(http.MethodPost, "http://app.local/upload", nil)
	NoteOverSizeLimit(s.ctx, zap.NewNop(), req, models.IncomingOptions{}, BodyLimit)
	if n := s.mgr.OverSizeLimitCount(); n != 0 {
		t.Fatalf("a request after the recording ended was counted over the size limit (%d)", n)
	}
	if told := toldNotRecorded(s.feed); len(told) != 0 {
		t.Fatalf("a request after the recording ended was told: %v", told)
	}
	s.mgr.SayNotCaptured()
	said := logs.FilterMessage("agent: no recording took the application's traffic after the last one ended").All()
	if len(said) != 1 || said[0].ContextMap()["requests_not_captured"] != uint64(1) {
		t.Fatalf("the request after the recording ended was said as not captured %v, want one request", said)
	}
}

// The limits are said as the user reads sizes: 5 MiB for an exchange, 2.5 MiB
// for one body, each limit with its own sentence.
func TestSizeLimitsAreSaidInMiB(t *testing.T) {
	if MaxTestCaseBodySize*2 != MaxTestCaseSize {
		t.Fatalf("one body's limit %d is not half the exchange's %d", MaxTestCaseBodySize, MaxTestCaseSize)
	}
	if got := TestCaseLimit.Why(); !strings.Contains(got, "over 5 MiB") {
		t.Fatalf("the exchange's limit is said %q", got)
	}
	if got := BodyLimit.Why(); !strings.Contains(got, "body is over 2.5 MiB") {
		t.Fatalf("a body's limit is said %q", got)
	}
}
