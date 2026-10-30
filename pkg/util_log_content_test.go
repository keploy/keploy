package pkg

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A stream's mismatch line names the frame that differs and how long each
// side's is, never either side's text: a frame of an answer is the app's
// content (an event's data, a line of a report).
func TestStreamMismatchLinesLogNoFrameText(t *testing.T) {
	const recorded, answered = "recorded-frame-SECRET-REC1", "answered-frame-SECRET-ANS2"
	for _, c := range []struct {
		name string
		rec  string
		ans  string
	}{
		{"SSE", "data: " + recorded + "\n\n", "data: " + answered + "\n\n"},
		{"NDJSON", `{"v":"` + recorded + `"}` + "\n", `{"v":"` + answered + `"}` + "\n"},
		{"plain text", recorded + "\n", answered + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			want := models.HTTPResp{Body: c.rec}
			var ok bool
			var err error
			switch c.name {
			case "SSE":
				ok, _, _, err = compareSSEStream(want, strings.NewReader(c.ans), nil, logger)
			case "NDJSON":
				ok, _, _, err = compareNDJSONStream(want, strings.NewReader(c.ans), nil, logger)
			default:
				ok, _, _, err = comparePlainTextStream(want, strings.NewReader(c.ans), logger)
			}
			if ok || err != nil {
				t.Fatalf("a differing frame compared ok=%v err=%v", ok, err)
			}
			got := out.String()
			if !strings.Contains(got, "mismatch") || !strings.Contains(got, "frame_bytes") && !strings.Contains(got, "actual_bytes") {
				t.Fatalf("the mismatch was not logged by its frame:\n%s", got)
			}
			for _, v := range []string{recorded, answered} {
				if found := logleak.Find(t, got, []byte(v)); len(found) > 0 {
					t.Errorf("the log holds a frame's text %q: %v\n%s", v, found, got)
				}
			}
		})
	}
}

// requestShape names a stalled request (AnswerStallError's Target) by its
// method and its path's shape: never a query value or a path segment that
// holds one. A CONNECT's authority is named whole.
func TestRequestShapeNamesNoQueryValueAndNoValueSegment(t *testing.T) {
	const query, segment = "tok-SECRET-QUERY9", "8812734455"
	u, err := url.Parse("http://app.local/users/" + segment + "/orders?token=" + query)
	if err != nil {
		t.Fatal(err)
	}
	got := requestShape(&http.Request{Method: http.MethodGet, URL: u})
	if want := "GET /users/{}/orders"; got != want {
		t.Errorf("requestShape = %q, want %q", got, want)
	}
	for _, v := range []string{query, segment} {
		if strings.Contains(got, v) {
			t.Errorf("requestShape %q holds %q", got, v)
		}
	}
	connect := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: "upstream.invalid:443"}}
	if got, want := requestShape(connect), "CONNECT upstream.invalid:443"; got != want {
		t.Errorf("requestShape of a CONNECT = %q, want %q", got, want)
	}
}

// A gRPC test's target is a bare authority, which holds no path, query or
// credential: each step logs it whole (targetMeta). utils.URLMeta read
// "localhost:8080" as a scheme with no host and refused "127.0.0.1:8080" as
// unparseable, so the line named no target.
func TestResolveTestTargetLogsABareAuthorityWhole(t *testing.T) {
	for _, c := range []struct{ target, want, line string }{
		{"localhost:8080", "localhost:8080", "Final resolved target"},
		{"127.0.0.1:8080", "127.0.0.1:8080", "Final resolved target"},
		{"[::1]:50051", "[::1]:50051", "Final resolved target"},
		{"grpc.internal", "grpc.internal:443", "Authority has no explicit port"},
	} {
		t.Run(c.target, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			got, err := ResolveTestTarget(c.target, nil, nil, "", 0, 0, false, logger)
			if err != nil || got != c.want {
				t.Fatalf("ResolveTestTarget = %q, %v; want %q", got, err, c.want)
			}
			if log := out.String(); !strings.Contains(log, c.line) || !strings.Contains(log, `"host": "`+c.target+`"`) && !strings.Contains(log, `"host":"`+c.target+`"`) {
				t.Errorf("the line %q does not name the target %s whole: %s", c.line, c.target, log)
			}
		})
	}
}

// A target is logged whole only when it is a bare authority: one with a
// path, a query, a credential or a space is logged by utils.URLMeta, though
// it splits as a host and a port, or holds no colon at all.
func TestTargetMetaIsWholeOnlyForABareAuthority(t *testing.T) {
	const user, token = "u_8812345Zq", "tok_SeCrEt99x"
	for _, c := range []struct {
		target string
		whole  bool
	}{
		{"localhost:8080", true},
		{"127.0.0.1:8080", true},
		{"[::1]:50051", true},
		{"grpc.internal", true},
		{"api.local/users/" + user + "?token=" + token, false},
		{token + "@grpc.internal:443", false},
		{"grpc.internal:443/" + user, false},
		{"grpc.internal?token=" + token, false},
		{"grpc.internal#" + token, false},
		{"grpc.internal:" + token, false},
		{"", false},
	} {
		got := fmt.Sprint(targetMeta(c.target))
		if whole := got == fmt.Sprint(map[string]string{"host": c.target}); whole != c.whole {
			t.Errorf("targetMeta(%q) = %s; whole %v, want %v", c.target, got, whole, c.whole)
		}
		if c.whole {
			continue
		}
		for _, v := range []string{user, token} {
			if strings.Contains(got, v) {
				t.Errorf("targetMeta(%q) = %s holds %q", c.target, got, v)
			}
		}
	}
}

// A test case URL that does not parse is said to be one, and nothing of it
// is: url.Parse's error quotes the URL whole (unquotedURLError), on the Error
// line and in the error keploy test reports. The line quoted the URL, its
// query's token with it.
func TestResolveTestTargetSaysNothingOfAURLThatDoesNotParse(t *testing.T) {
	const token = "tok_SeCrEt99x"
	raw := "http://api.local/users/%zz?token=" + token
	for _, c := range []struct{ name, host, line string }{
		{"the URL", "", "failed to parse test case URL"},
		{"the config's host", "otherhost", "failed to replace host with config host"},
	} {
		t.Run(c.name, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			_, err := ResolveTestTarget(raw, nil, nil, c.host, 0, 0, true, logger)
			if err == nil || !strings.Contains(err.Error(), "the test case's URL does not parse") {
				t.Fatalf("ResolveTestTarget's error = %v; want the URL does not parse", err)
			}
			log := out.String()
			if !strings.Contains(log, c.line) {
				t.Fatalf("the log does not say %q: %s", c.line, log)
			}
			for what, got := range map[string]string{"the log": log, "the error": err.Error()} {
				for _, v := range []string{token, "api.local/users"} {
					if found := logleak.Find(t, got, []byte(v)); len(found) > 0 {
						t.Errorf("%s holds the URL's %q: %v", what, v, found)
					}
				}
			}
		})
	}
}

// Resolving where a test's request goes logs each step's target by its host,
// its path's shape and its query's names (utils.URLMeta): never a query value
// nor a path segment that holds one. It logged the URL whole at each step.
func TestResolveTestTargetLogsNoQueryValueAndNoValueSegment(t *testing.T) {
	const user, token = "u_8812345Zq", "tok_SeCrEt99x"
	raw := "http://api.local/users/" + user + "?token=" + token
	for _, c := range []struct {
		name, host, want, line string
		replace                map[string]string
	}{
		{"a replaceWith", "", "http://localhost:9090/users/" + user + "?token=" + token, "Applied replaceWith substitution", map[string]string{"api.local": "localhost:9090"}},
		{"the config's host", "otherhost", "http://otherhost:80/users/" + user + "?token=" + token, "Replaced host with config host", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			got, err := ResolveTestTarget(raw, c.replace, nil, c.host, 0, 0, true, logger)
			if err != nil || got != c.want {
				t.Fatalf("ResolveTestTarget = %q, %v; want %q", got, err, c.want)
			}
			log := out.String()
			if !strings.Contains(log, c.line) || !strings.Contains(log, "/users/{}") {
				t.Fatalf("the log does not say %q of /users/{}: %s", c.line, log)
			}
			for what, v := range map[string]string{"the path's user": user, "the query's token": token} {
				if found := logleak.Find(t, log, []byte(v)); len(found) > 0 {
					t.Errorf("the log holds %s: %v", what, found)
				}
			}
		})
	}
}

// stallingReader gives its bytes, then the stall rule's cut.
type stallingReader struct {
	r   *strings.Reader
	cut error
}

func (s *stallingReader) Read(p []byte) (int, error) {
	if s.r.Len() == 0 {
		return 0, s.cut
	}
	return s.r.Read(p)
}

// mime/multipart's error for a part it cannot read quotes a line of the
// body: of the app's streamed answer (which replay logs at Error, "failed to
// read streaming response"), or of the recording. compareMultipartStream
// returns either by what held the part, with no byte of it; the stall
// rule's cut is still told as the stall.
func TestMultipartStreamErrorHoldsNoByteOfEitherBody(t *testing.T) {
	const secret = "Zq7STREAMsecret31"
	good := "--XYZ\r\nContent-Type: text/plain\r\n\r\nframe one\r\n--XYZ--\r\n"
	bad := "--XYZ\r\nX-Session " + secret + "\r\n\r\nframe one\r\n--XYZ--\r\n"
	recorded := func(body string) models.HTTPResp {
		return models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "multipart/x-mixed-replace; boundary=XYZ"}, Body: body}
	}
	for _, c := range []struct{ name, recorded, answer, want string }{
		{"the app's answer", good, bad, "the app's multipart answer has a part that does not parse"},
		{"the recording", bad, good, fmt.Sprintf("failed to read the recording's parts: the recorded multipart body (%d bytes) has a part that does not parse", len(bad))},
	} {
		t.Run(c.name, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			_, _, _, err := compareMultipartStream(recorded(c.recorded), strings.NewReader(c.answer), "XYZ", nil, logger)
			if err == nil {
				t.Fatalf("precondition: a part header net/textproto cannot parse was compared")
			}
			if err.Error() != c.want {
				t.Errorf("compareMultipartStream's error = %q, want %q", err, c.want)
			}
			if found := logleak.Find(t, out.String()+"\n"+err.Error(), []byte(secret)); len(found) > 0 {
				t.Errorf("the stream compare's error or log holds a byte of a body: %v\n%v", found, err)
			}
		})
	}
	t.Run("a stall", func(t *testing.T) {
		cut := &AnswerStallError{Answered: true}
		logger, _ := logleak.Logger(nil)
		answer := &stallingReader{r: strings.NewReader("--XYZ\r\nContent-Type: text/plain\r\n"), cut: cut}
		_, _, _, err := compareMultipartStream(recorded(good), answer, "XYZ", nil, logger)
		var st *AnswerStallError
		if !errors.As(err, &st) {
			t.Fatalf("compareMultipartStream's error = %v, not the stall rule's cut", err)
		}
	})
}

// trailerSecret is planted in a chunked answer's trailer line, without its
// colon: net/http reads the trailer as it reads the body, and its error
// (textproto's) quotes the line whole.
const trailerSecret = "Zq7TRAILERsecret94"

// chunkedAnswerBody is the body of a chunked answer carrying body, whose
// trailer line does not parse, as net/http's body reader gives it.
func chunkedAnswerBody(t *testing.T, contentType, body string) io.Reader {
	t.Helper()
	raw := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: %s\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\nX-Sig %s\r\n\r\n", contentType, len(body), body, trailerSecret)
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Body
}

// Each stream compare reads a chunked answer whose trailer does not parse:
// its error, which replay logs at Error, names the trailer, never the line
// (models.BodyReadError, in streamCut and the binary compare's read).
func TestStreamCompareErrorHoldsNoByteOfAnUnparsedTrailer(t *testing.T) {
	const want = "the app's streamed answer's trailer does not parse"
	for _, c := range []struct {
		name    string
		compare func(io.Reader) error
	}{
		{"SSE", func(r io.Reader) error {
			_, _, _, err := compareSSEStream(models.HTTPResp{StatusCode: 200, Body: "data: one\n\ndata: two\n\n"}, r, nil, zap.NewNop())
			return err
		}},
		{"NDJSON", func(r io.Reader) error {
			_, _, _, err := compareNDJSONStream(models.HTTPResp{StatusCode: 200, Body: "{\"a\":1}\n{\"a\":2}\n"}, r, nil, zap.NewNop())
			return err
		}},
		{"plain text", func(r io.Reader) error {
			_, _, _, err := comparePlainTextStream(models.HTTPResp{StatusCode: 200, Body: "one\ntwo\n"}, r, zap.NewNop())
			return err
		}},
		{"binary", func(r io.Reader) error {
			_, _, _, err := compareBinaryStream(models.HTTPResp{StatusCode: 200, Body: "one\ntwo\n"}, r, zap.NewNop())
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]string{"SSE": "data: one\n\n", "NDJSON": "{\"a\":1}\n", "plain text": "one\n", "binary": "one\n"}[c.name]
			err := c.compare(chunkedAnswerBody(t, "text/event-stream", body))
			if err == nil || err.Error() != want {
				t.Fatalf("the compare's error = %v, want %q", err, want)
			}
			if found := logleak.Find(t, err.Error(), []byte(trailerSecret)); len(found) > 0 {
				t.Errorf("the compare's error holds the trailer line: %v", found)
			}
		})
	}
}
