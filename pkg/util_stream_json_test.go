package pkg

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A stream test case that reaches the replay as JSON (a cloud replay's, or
// any carried as JSON) is compared by what its recording kept: a stream that
// changed fails, under either switch. JSON carries its body, not its frames
// (StreamBody), and the replay compared the app's stream with no recorded
// frame: with the switch off it passed on the first frame that came, however
// different, and under the switch it failed, however alike.

// recordedThroughYAML is resp as a recording keeps it: written as a test
// case's YAML and read back, so a stream's body is kept as its frames.
func recordedThroughYAML(t *testing.T, resp models.HTTPResp) models.HTTPResp {
	t.Helper()
	out, err := yamlLib.Marshal(resp)
	require.NoError(t, err)
	var back models.HTTPResp
	require.NoError(t, yamlLib.Unmarshal(out, &back))
	return back
}

// throughJSON is tc as the replay gets it from JSON.
func throughJSON(t *testing.T, tc *models.TestCase) *models.TestCase {
	t.Helper()
	out, err := json.Marshal(tc)
	require.NoError(t, err)
	var back models.TestCase
	require.NoError(t, json.Unmarshal(out, &back))
	return &back
}

// streamingApp answers every request with body under contentType, frame by
// frame (each flushed), and ends the response.
func streamingApp(t *testing.T, contentType string, frames []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = w.Write([]byte(f))
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// compareAgainstApp sends tc to an app streaming frames and compares its
// answer as the replay does.
func compareAgainstApp(t *testing.T, tc *models.TestCase, contentType string, frames []string) (bool, *StreamMismatchInfo, error) {
	t.Helper()
	srv := streamingApp(t, contentType, frames)
	sent := *tc
	sent.HTTPReq.URL = srv.URL + "/events"
	resp, err := SimulateHTTPStreaming(t.Context(), &sent, "test-set-0", zap.NewNop(), SimulationConfig{APITimeout: 5})
	require.NoError(t, err)
	defer resp.Reader.Close()
	require.NotEqual(t, HTTPStreamModeNone, resp.StreamConfig.Mode, "the answer is read as a stream")
	ok, _, info, err := CompareHTTPStream(sent.HTTPResp, resp.Reader, resp.StreamConfig, map[string]struct{}{}, zap.NewNop())
	return ok, info, err
}

type jsonStreamCase struct {
	name        string
	contentType string
	recorded    []string // the frames the recording saw, as they went out
	changed     []string // the same stream, its content changed throughout
}

var jsonStreamCases = []jsonStreamCase{
	{
		name:        "sse",
		contentType: "text/event-stream",
		recorded:    []string{"id: 1\nevent: price\ndata: {\"sku\":\"a\",\"price\":10}\n\n", "id: 2\nevent: price\ndata: {\"sku\":\"b\",\"price\":20}\n\n"},
		changed:     []string{"id: 7\nevent: stock\ndata: {\"sku\":\"x\",\"price\":99}\n\n", "id: 8\nevent: stock\ndata: {\"sku\":\"y\",\"price\":98}\n\n"},
	},
	{
		name:        "ndjson",
		contentType: "application/x-ndjson",
		recorded:    []string{"{\"sku\":\"a\",\"price\":10}\n", "{\"sku\":\"b\",\"price\":20}\n"},
		changed:     []string{"{\"sku\":\"x\",\"price\":99}\n", "{\"sku\":\"y\",\"price\":98}\n"},
	},
	// No text/plain row: a text body is a stream only on its recording's
	// frames (models.HTTPStreamOf), which JSON does not carry, so carried as
	// JSON it is compared whole (TestATextStreamCarriedAsJSONIsComparedWhole).
}

// A text/plain test case is a stream test only on the frames its recording
// kept (an older keploy's: a recording now keeps a text body whole), and JSON
// does not carry them. Carried as JSON it is no stream test, and is compared
// whole, as any response is, by its body: the wire of its lines, a blank one
// last included, so the unchanged answer is that body byte for byte.
func TestATextStreamCarriedAsJSONIsComparedWhole(t *testing.T) {
	const wire = "started\n\ndone\n\n"
	var frames []models.HTTPStreamChunk
	for _, line := range []string{"started", "", "done", ""} {
		frames = append(frames, models.HTTPStreamChunk{Data: []models.HTTPStreamDataField{{Key: "raw", Value: line}}})
	}
	switchStates(t, func(t *testing.T) {
		recorded := streamTestCase("text/plain", "")
		recorded.HTTPResp.StreamBody = frames
		recorded.HTTPResp = recordedThroughYAML(t, recorded.HTTPResp)
		require.Len(t, recorded.HTTPResp.StreamBody, len(frames), "its YAML keeps the frames an older keploy recorded")
		require.True(t, IsHTTPStreamingTestCase(recorded), "framed, it is a stream test")

		carried := throughJSON(t, recorded)
		require.Empty(t, carried.HTTPResp.StreamBody)
		require.False(t, IsHTTPStreamingTestCase(carried), "carried as JSON, a text body is no stream: it is compared whole")
		require.Equal(t, wire, carried.HTTPResp.Body, "its body is the wire of its lines")
	})
}

func switchStates(t *testing.T, run func(t *testing.T)) {
	for _, on := range []bool{false, true} {
		name := "switch-off"
		if on {
			name = "switch-on"
		}
		t.Run(name, func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			run(t)
		})
	}
}

func streamTestCase(contentType, body string) *models.TestCase {
	return &models.TestCase{
		Name: "stream-1",
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    "http://localhost/events",
			Header: map[string]string{"Accept": contentType},
		},
		HTTPResp: models.HTTPResp{
			StatusCode: http.StatusOK,
			Header:     map[string]string{"Content-Type": contentType},
			Body:       body,
			Timestamp:  time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
		},
	}
}

// A recorded stream test case carried as JSON fails when its stream changed,
// and passes when it did not.
func TestAStreamTestCarriedAsJSONFailsWhenItsStreamChanged(t *testing.T) {
	for _, c := range jsonStreamCases {
		t.Run(c.name, func(t *testing.T) {
			switchStates(t, func(t *testing.T) {
				recorded := streamTestCase(c.contentType, strings.Join(c.recorded, ""))
				recorded.HTTPResp = recordedThroughYAML(t, recorded.HTTPResp)
				require.Len(t, recorded.HTTPResp.StreamBody, len(c.recorded), "the recording kept the stream's frames")

				carried := throughJSON(t, recorded)

				ok, info, err := compareAgainstApp(t, carried, c.contentType, c.changed)
				require.NoError(t, err)
				require.False(t, ok, "a stream whose content changed throughout must not pass")
				require.NotNil(t, info)
				require.Equal(t, 0, info.FrameIndex, "the first frame differs")

				ok, info, err = compareAgainstApp(t, carried, c.contentType, c.recorded)
				require.NoError(t, err)
				require.True(t, ok, "the recorded stream passes: %+v", info)
			})
		})
	}
}

// A cloud test case arrives as JSON with its recorded body and no frames (the
// DaemonSet agent records a body, and api-server keeps and serves it as
// JSON): it is compared by its body split into frames, as a recording of it
// would have kept them.
func TestACloudStreamTestCaseIsComparedByItsRecordedBody(t *testing.T) {
	for _, c := range jsonStreamCases {
		t.Run(c.name, func(t *testing.T) {
			switchStates(t, func(t *testing.T) {
				wire, err := json.Marshal(map[string]any{
					"name": "stream-1",
					"kind": "Http",
					"http_req": map[string]any{
						"method": "GET",
						"url":    "http://localhost/events",
						"header": map[string]string{"Accept": c.contentType},
					},
					"http_resp": map[string]any{
						"status_code": http.StatusOK,
						"header":      map[string]string{"Content-Type": c.contentType},
						"body":        strings.Join(c.recorded, ""),
					},
				})
				require.NoError(t, err)
				var tc models.TestCase
				require.NoError(t, json.Unmarshal(wire, &tc))
				require.Empty(t, tc.HTTPResp.StreamBody)

				ok, info, err := compareAgainstApp(t, &tc, c.contentType, c.changed)
				require.NoError(t, err)
				require.False(t, ok, "a stream whose content changed throughout must not pass")
				require.NotNil(t, info)
				require.Equal(t, 0, info.FrameIndex)

				ok, info, err = compareAgainstApp(t, &tc, c.contentType, c.recorded)
				require.NoError(t, err)
				require.True(t, ok, "the recorded stream passes: %+v", info)

				// One frame changed, the last: still a failure, at that frame.
				lastChanged := append(append([]string{}, c.recorded[:len(c.recorded)-1]...), c.changed[len(c.changed)-1])
				ok, info, err = compareAgainstApp(t, &tc, c.contentType, lastChanged)
				require.NoError(t, err)
				require.False(t, ok)
				require.NotNil(t, info)
				require.Equal(t, len(c.recorded)-1, info.FrameIndex)
			})
		})
	}
}

// The body JSON carries of a stream read from its recorded frames reads, as
// the app's stream is read, as those frames: an SSE event whose data spans
// lines, a blank one among them, or starts with a space, and a comment, are
// no other event read back from it. Carried as JSON, the recorded stream
// passes, and a changed one fails at the frame that changed.
func TestAStreamCarriedAsJSONIsComparedByTheFramesItsRecordingKept(t *testing.T) {
	const contentType = "text/event-stream"
	recordedFrames := []string{
		"id: 1\nevent: log\ndata: line one\ndata: line two\n\n",
		"data:  indented\n\n",
		"data: before\ndata:\ndata: after\n\n",
		": keepalive\n\n",
		"event: tick\ndata:\n\n",
	}
	switchStates(t, func(t *testing.T) {
		recorded := streamTestCase(contentType, strings.Join(recordedFrames, ""))
		recorded.HTTPResp = recordedThroughYAML(t, recorded.HTTPResp)
		require.Len(t, recorded.HTTPResp.StreamBody, len(recordedFrames))

		carried := throughJSON(t, recorded)

		ok, info, err := compareAgainstApp(t, carried, contentType, recordedFrames)
		require.NoError(t, err)
		require.True(t, ok, "the recorded stream passes: %+v", info)

		changed := append([]string{}, recordedFrames...)
		changed[2] = "data: before\ndata:\ndata: AFTER\n\n"
		ok, info, err = compareAgainstApp(t, carried, contentType, changed)
		require.NoError(t, err)
		require.False(t, ok)
		require.NotNil(t, info)
		require.Equal(t, 2, info.FrameIndex)
	})
}

// A recorded body that cannot be read as the answer is read (a frame or line
// longer than a stream's read takes) fails the test with that reason: it is
// neither compared by the frames before it alone nor taken for a recording of
// nothing.
func TestARecordedStreamBodyThatCannotBeReadFails(t *testing.T) {
	long := strings.Repeat("a", maxStreamTokenSize+1)
	for _, c := range []struct {
		name, contentType, body string
		cfg                     HTTPStreamConfig
	}{
		{"sse", "text/event-stream", "data: " + long + "\n\n", HTTPStreamConfig{Mode: HTTPStreamModeSSE}},
		{"ndjson", "application/x-ndjson", "{\"a\":1}\n" + long + "\n", HTTPStreamConfig{Mode: HTTPStreamModeNDJSON}},
		{"plain-text", "text/plain", "first\n" + long + "\n", HTTPStreamConfig{Mode: HTTPStreamModePlainText}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := streamTestCase(c.contentType, c.body)
			ok, _, info, err := CompareHTTPStream(tc.HTTPResp, strings.NewReader(c.body), c.cfg, map[string]struct{}{}, zap.NewNop())
			require.False(t, ok)
			require.Nil(t, info)
			require.Error(t, err)
			require.False(t, errors.Is(err, ErrStreamNotRecorded), "not a recording of nothing: %v", err)
			require.Contains(t, err.Error(), "failed to read the recorded")
		})
	}
}

// A stream test whose recording saved nothing of its stream (a body over 1 MB,
// which a recording does not save: BodySkipped) has nothing to compare the
// app's stream with. It is refused, with the reason, under either switch, and
// never passes. A recording that saved its body is compared with it, however
// little it holds (TestAStreamRecordedEmptyIsComparedWithItsEmptyStream,
// TestAStreamRecordedWithNoEventIsComparedWithWhatItHolds).
func TestAStreamTestWithNothingRecordedIsRefused(t *testing.T) {
	modes := []struct {
		name        string
		contentType string
		cfg         HTTPStreamConfig
		answer      string
	}{
		{"sse", "text/event-stream", HTTPStreamConfig{Mode: HTTPStreamModeSSE}, "data: {\"a\":1}\n\n"},
		{"ndjson", "application/x-ndjson", HTTPStreamConfig{Mode: HTTPStreamModeNDJSON}, "{\"a\":1}\n"},
		{"plain-text", "text/plain", HTTPStreamConfig{Mode: HTTPStreamModePlainText}, "line one\n"},
		{"multipart", "multipart/x-mixed-replace; boundary=frame", HTTPStreamConfig{Mode: HTTPStreamModeMultipart, Boundary: "frame"}, "--frame\r\nContent-Type: text/plain\r\n\r\npart one\r\n--frame--\r\n"},
		{"binary", "application/octet-stream", HTTPStreamConfig{Mode: HTTPStreamModeBinary}, "\x00\x01\x02"},
	}
	recordings := []struct {
		name string
		resp func(contentType string) models.HTTPResp
		why  string
	}{
		{"body not saved", func(ct string) models.HTTPResp {
			return models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": ct}, BodySkipped: true, BodySize: 2 << 20}
		}, "its body, 2097152 bytes, was over 1 MB"},
		{"body not saved, its size not either", func(ct string) models.HTTPResp {
			return models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": ct}, BodySkipped: true}
		}, "(its body was over 1 MB"},
	}
	for _, m := range modes {
		for _, rec := range recordings {
			t.Run(m.name+"/"+rec.name, func(t *testing.T) {
				switchStates(t, func(t *testing.T) {
					resp := rec.resp(m.contentType)
					// What a recording and JSON make of it.
					resp = recordedThroughYAML(t, resp)
					tc := throughJSON(t, &models.TestCase{Kind: models.HTTP, HTTPResp: resp})
					answer := &countingReader{r: strings.NewReader(m.answer)}
					ok, _, info, err := CompareHTTPStream(tc.HTTPResp, answer, m.cfg, map[string]struct{}{}, zap.NewNop())
					require.False(t, ok, "nothing recorded must never pass")
					require.Nil(t, info, "refused, not judged different")
					require.True(t, errors.Is(err, ErrStreamNotRecorded), "refused: %v", err)
					require.Contains(t, err.Error(), rec.why)
					require.Zero(t, answer.n, "refused before its stream is read")
				})
			})
		}
	}
}

// A stream recorded empty (its body saved, and empty: the app ended the
// stream with nothing in it) is compared with that, as any recording is, under
// either switch, YAML or carried as JSON: the app's stream is read to its end,
// and an answer that ends with nothing passes, one that streams a frame fails,
// judged different, and one of a keepalive has nothing of it to compare, not
// judged. It is not refused: re-recording gives the same empty stream. With
// the switch off a stream that came passed, compared with no recorded frame.
func TestAStreamRecordedEmptyIsComparedWithItsEmptyStream(t *testing.T) {
	modes := []struct {
		name        string
		contentType string
		cfg         HTTPStreamConfig
		frame       string // an answer of a frame
		keepalive   string // an answer of a keepalive, where the format has one
		differs     string // the mismatch's reason
	}{
		{"sse", "text/event-stream", HTTPStreamConfig{Mode: HTTPStreamModeSSE}, "data: {\"a\":1}\n\n", ": ping\n\n", "the stream went on after its 0 recorded frames"},
		{"ndjson", "application/x-ndjson", HTTPStreamConfig{Mode: HTTPStreamModeNDJSON}, "{\"a\":1}\n", "\n", "the stream went on after its 0 recorded frames"},
		{"plain-text", "text/plain", HTTPStreamConfig{Mode: HTTPStreamModePlainText}, "\n", "", "the stream went on after its 0 recorded frames"},
		{"multipart", "multipart/x-mixed-replace; boundary=frame", HTTPStreamConfig{Mode: HTTPStreamModeMultipart, Boundary: "frame"}, "--frame\r\nContent-Type: text/plain\r\n\r\npart one\r\n--frame--\r\n", "", "the stream went on after its 0 recorded frames"},
		{"binary", "application/octet-stream", HTTPStreamConfig{Mode: HTTPStreamModeBinary}, "\x00", "", "size mismatch: expected 0 bytes, got 1 bytes"},
	}
	for _, m := range modes {
		for _, carried := range []string{"yaml", "json"} {
			t.Run(m.name+"/"+carried, func(t *testing.T) {
				switchStates(t, func(t *testing.T) {
					resp := recordedThroughYAML(t, models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": m.contentType}})
					if carried == "json" {
						resp = throughJSON(t, &models.TestCase{Kind: models.HTTP, HTTPResp: resp}).HTTPResp
					}
					compare := func(answer string) (bool, *StreamMismatchInfo, error) {
						ok, _, info, err := CompareHTTPStream(resp, strings.NewReader(answer), m.cfg, map[string]struct{}{}, zap.NewNop())
						return ok, info, err
					}

					ok, info, err := compare("")
					require.NoError(t, err)
					require.True(t, ok, "an empty stream, as recorded, passes: %+v", info)

					ok, info, err = compare(m.frame)
					require.NoError(t, err, "judged, not refused")
					require.False(t, ok, "a stream that came where none was recorded must not pass")
					require.NotNil(t, info)
					require.Equal(t, m.differs, info.Reason)

					if m.keepalive != "" {
						ok, info, err = compare(m.keepalive)
						require.False(t, ok, "a keepalive is nothing of the answer to compare")
						require.Nil(t, info)
						require.True(t, errors.Is(err, ErrAnswerStalled), "not judged: %v", err)
						require.False(t, errors.Is(err, ErrStreamNotRecorded), "compared, not refused: %v", err)
					}
				})
			})
		}
	}
}

// A stream recorded with no event in it, its body saved and holding
// keepalives or blank lines alone (an SSE body of comments, or of blank lines,
// an NDJSON body of blank lines, a multipart body of no part), is compared
// with what it holds, as one recorded empty is, under either switch, kept as
// YAML or carried as JSON: the app's stream is read to its end, a stream that
// ends as recorded passes, one with an event (a line, a part) the recording
// does not have fails, judged different, and one that goes on with keepalives
// past the recording has nothing but keepalives of it to compare, not judged.
// It is not refused: re-recording the app gives the same recording. With the
// switch off a stream passed once the recorded keepalives had come, whatever
// followed.
func TestAStreamRecordedWithNoEventIsComparedWithWhatItHolds(t *testing.T) {
	const (
		pass      = "passes"
		notJudged = "not judged"
		part      = "--frame\r\nContent-Type: text/plain\r\n\r\npart one\r\n--frame--\r\n"
	)
	sse := HTTPStreamConfig{Mode: HTTPStreamModeSSE}
	ndjson := HTTPStreamConfig{Mode: HTTPStreamModeNDJSON}
	multi := HTTPStreamConfig{Mode: HTTPStreamModeMultipart, Boundary: "frame"}
	type answer struct {
		body string
		want string // pass, notJudged, or the reason it differs
	}
	for _, c := range []struct {
		name, contentType, body string
		cfg                     HTTPStreamConfig
		answers                 []answer
	}{
		{"sse of a keepalive", "text/event-stream", ": ok\n\n", sse, []answer{
			{": ok\n\n", pass},
			{": ok\n\ndata: x\n\n", "the stream went on after its 1 recorded frames"},
			{"data: x\n\n", "comment-position mismatch"},
			{": ok\n\n: ok\n\n", notJudged},
			{"", "expected 1 frames but only received 0"},
		}},
		{"sse of keepalives", "text/event-stream", ": ping\n\n: ping\n\n", sse, []answer{
			{": ping\n\n: ping\n\n", pass},
			{": ping\n\n: ping\n\nevent: tick\ndata: 1\n\n", "the stream went on after its 2 recorded frames"},
			{": ping\n\n: ping\n\n: ping\n\n", notJudged},
		}},
		{"sse of blank lines", "text/event-stream", "\n\n\n\n", sse, []answer{
			{"", pass},
			{"\n\n", pass},
			{"data: x\n\n", "the stream went on after its 0 recorded frames"},
			{": ping\n\n", notJudged},
		}},
		{"ndjson of blank lines", "application/x-ndjson", "\n\n", ndjson, []answer{
			{"", pass},
			{"{\"a\":1}\n", "the stream went on after its 0 recorded frames"},
			{"\n", notJudged},
		}},
		{"multipart of no part", "multipart/x-mixed-replace; boundary=frame", "--frame--\r\n", multi, []answer{
			{"", pass},
			{"--frame--\r\n", pass},
			{part, "the stream went on after its 0 recorded frames"},
		}},
		{"multipart of blank lines", "multipart/x-mixed-replace; boundary=frame", "\r\n\r\n", multi, []answer{
			{"", pass},
			{part, "the stream went on after its 0 recorded frames"},
		}},
	} {
		for _, carried := range []string{"yaml", "json"} {
			t.Run(c.name+"/"+carried, func(t *testing.T) {
				switchStates(t, func(t *testing.T) {
					resp := recordedThroughYAML(t, models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": c.contentType}, Body: c.body})
					if carried == "json" {
						resp = throughJSON(t, &models.TestCase{Kind: models.HTTP, HTTPResp: resp}).HTTPResp
					}
					for _, a := range c.answers {
						ok, _, info, err := CompareHTTPStream(resp, strings.NewReader(a.body), c.cfg, map[string]struct{}{}, zap.NewNop())
						require.False(t, errors.Is(err, ErrStreamNotRecorded), "answer %q: compared, not refused: %v", a.body, err)
						switch a.want {
						case pass:
							require.NoError(t, err, "answer %q", a.body)
							require.True(t, ok, "answer %q, as recorded, passes: %+v", a.body, info)
						case notJudged:
							require.False(t, ok, "answer %q: keepalives alone are nothing of the answer to compare", a.body)
							require.Nil(t, info, "answer %q", a.body)
							require.True(t, errors.Is(err, ErrAnswerStalled), "answer %q: not judged: %v", a.body, err)
						default:
							require.NoError(t, err, "answer %q: judged", a.body)
							require.False(t, ok, "answer %q must not pass", a.body)
							require.NotNil(t, info, "answer %q", a.body)
							require.Equal(t, a.want, info.Reason, "answer %q", a.body)
						}
					}
				})
			})
		}
	}
}

// A multipart stream recorded empty fails an answer of bytes that hold no
// part, none being multipart (no boundary), or a boundary line or a part cut
// off: those bytes are no empty stream. multipart.Reader reads the first two
// as the error "multipart: NextPart: EOF", which an answer of no byte is too.
func TestAMultipartStreamRecordedEmptyFailsAnAnswerOfNoPart(t *testing.T) {
	cfg := HTTPStreamConfig{Mode: HTTPStreamModeMultipart, Boundary: "frame"}
	for _, carried := range []string{"yaml", "json"} {
		t.Run(carried, func(t *testing.T) {
			switchStates(t, func(t *testing.T) {
				resp := recordedThroughYAML(t, models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "multipart/x-mixed-replace; boundary=frame"}})
				if carried == "json" {
					resp = throughJSON(t, &models.TestCase{Kind: models.HTTP, HTTPResp: resp}).HTTPResp
				}
				for _, answer := range []string{
					"hello",
					"--frame",
					"--frame\r\nContent-Type: text/plain\r\n\r\npart one",
				} {
					ok, _, info, err := CompareHTTPStream(resp, strings.NewReader(answer), cfg, map[string]struct{}{}, zap.NewNop())
					require.False(t, ok, "answer %q holds no part, and must not pass", answer)
					require.True(t, info != nil || err != nil, "answer %q: failed, with why", answer)
					require.False(t, errors.Is(err, ErrStreamNotRecorded), "answer %q: compared, not refused: %v", answer, err)
				}
			})
		})
	}
}

// A response HTTP says has no body (the answer to a HEAD, a 1xx, 204 or 304)
// is no stream, whatever its Content-Type says: it is compared as any
// response is, by its status and headers, YAML or carried as JSON. Which
// response has none the answer says, not the recording. A HEAD's answer has
// none whatever the app does, so a HEAD test is no stream test. A recorded
// 204, 304 or 1xx is a stream test as any of its Content-Type is, and is sent
// as one: an answer with no body is compared as a response, and an answer of
// a stream is read as a stream, bounded as a stream's read is (its first
// keepalive, or its first frame past the recording, ends it). Read whole, as
// a response's is, a stream the app never ends is never read to its end. An
// answer of no body to a recorded stream is not read as a stream either: it
// differs from the recording by its status.
func TestAResponseWithNoBodyIsNoStream(t *testing.T) {
	contentTypes := []string{"text/event-stream", "application/x-ndjson", "multipart/x-mixed-replace; boundary=frame"}
	answers := []struct {
		name   string
		method string
		status int
	}{
		{"HEAD", http.MethodHead, http.StatusOK},
		{"204", http.MethodGet, http.StatusNoContent},
		{"304", http.MethodGet, http.StatusNotModified},
		{"103", http.MethodGet, http.StatusEarlyHints},
	}
	for _, ct := range contentTypes {
		for _, a := range answers {
			for _, carried := range []string{"yaml", "json"} {
				t.Run(ct+"/"+a.name+"/"+carried, func(t *testing.T) {
					tc := streamTestCase(ct, "")
					tc.HTTPReq.Method = models.Method(a.method)
					tc.HTTPResp.StatusCode = a.status
					tc.HTTPResp = recordedThroughYAML(t, tc.HTTPResp)
					if carried == "json" {
						tc = throughJSON(t, tc)
					}
					if a.method == http.MethodHead {
						require.False(t, IsHTTPStreamingTestCase(tc), "a HEAD's answer has no body: no stream test")
					} else {
						require.True(t, IsHTTPStreamingTestCase(tc), "a recorded %d is sent as a stream test: the app's answer says whether it has a body", a.status)
					}
					answer := &http.Response{StatusCode: a.status, Header: http.Header{"Content-Type": {ct}}, Request: &http.Request{Method: a.method}}
					require.Equal(t, HTTPStreamModeNone, DetectHTTPStreamConfig(tc, answer).Mode, "an answer with no body is no stream")
					if a.method != http.MethodHead {
						streamed := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {ct}}, Request: &http.Request{Method: a.method}}
						require.NotEqual(t, HTTPStreamModeNone, DetectHTTPStreamConfig(tc, streamed).Mode, "a recorded %d answered with a stream is read as one", a.status)
					}
				})
			}
		}
	}
	t.Run("a recorded stream answered with no body", func(t *testing.T) {
		recorded := streamTestCase("text/event-stream", "data: a\n\n")
		require.True(t, IsHTTPStreamingTestCase(recorded))
		for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
			answer := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: &http.Request{Method: http.MethodGet}}
			require.Equal(t, HTTPStreamModeNone, DetectHTTPStreamConfig(recorded, answer).Mode, "a %d is compared as a response, by its status", status)
		}
		ok := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: &http.Request{Method: http.MethodGet}}
		require.Equal(t, HTTPStreamModeSSE, DetectHTTPStreamConfig(recorded, ok).Mode, "a 200 of the stream is read as one")
	})
}

// countingReader counts the bytes read from r.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// A stream recorded in one format and answered in another (frames kept as
// SSE events, an answer of NDJSON lines) is compared by its recorded body
// read as the answer is read: judged different, neither passed nor refused
// as a recording of nothing.
func TestAStreamAnsweredInAnotherFormatThanItsRecordingFails(t *testing.T) {
	switchStates(t, func(t *testing.T) {
		recorded := streamTestCase("text/event-stream", "id: 1\ndata: {\"sku\":\"a\"}\n\n")
		recorded.HTTPResp = recordedThroughYAML(t, recorded.HTTPResp)
		require.Len(t, recorded.HTTPResp.StreamBody, 1)

		ok, info, err := compareAgainstApp(t, recorded, "application/x-ndjson", []string{"{\"sku\":\"a\"}\n"})
		require.NoError(t, err)
		require.False(t, ok)
		require.NotNil(t, info, "judged different")
	})
}

// A multipart stream is read by its own boundary, its recording by the one it
// was recorded with: a multipart writer picks a new boundary for each response
// (Go's does). Read by the answer's, the recorded body held no part, and the
// stream was compared with none: with the switch off a changed stream passed.
func TestAMultipartStreamIsComparedWithItsRecordingReadByItsRecordedBoundary(t *testing.T) {
	part := func(boundary, body string) string {
		return "--" + boundary + "\r\nContent-Type: text/plain\r\n\r\n" + body + "\r\n"
	}
	recordedBody := part("rec0rded", "frame one") + part("rec0rded", "frame two") + "--rec0rded--\r\n"
	const answerType = "multipart/x-mixed-replace; boundary=answ3r"
	switchStates(t, func(t *testing.T) {
		recorded := streamTestCase("multipart/x-mixed-replace; boundary=rec0rded", recordedBody)
		recorded.HTTPResp = recordedThroughYAML(t, recorded.HTTPResp)
		carried := throughJSON(t, recorded)
		require.Equal(t, recordedBody, carried.HTTPResp.Body, "a multipart body is kept whole")

		ok, info, err := compareAgainstApp(t, carried, answerType, []string{part("answ3r", "frame one"), part("answ3r", "frame two"), "--answ3r--\r\n"})
		require.NoError(t, err)
		require.True(t, ok, "the recorded parts pass: %+v", info)

		ok, info, err = compareAgainstApp(t, carried, answerType, []string{part("answ3r", "frame one"), part("answ3r", "frame 2"), "--answ3r--\r\n"})
		require.NoError(t, err)
		require.False(t, ok, "a changed part must not pass")
		require.NotNil(t, info)
		require.Equal(t, 1, info.FrameIndex)
	})
}

// A recorded body JSON carries (a cloud test case's, as the app sent it) is
// read as the app's stream is read, so a stream answered as recorded passes,
// whatever its bytes: an SSE field line with no colon, a field named
// "comment" (no comment, which starts with ':'), a text stream's lines
// carriage returns run within.
func TestARecordedStreamBodyIsReadAsTheAnswerIsRead(t *testing.T) {
	for _, c := range []struct {
		name, contentType, body, changed string
		cfg                              HTTPStreamConfig
	}{
		{"sse field with no colon", "text/event-stream", "retry\ndata: a\n\n", "retry\ndata: b\n\n", HTTPStreamConfig{Mode: HTTPStreamModeSSE}},
		{"sse field named comment", "text/event-stream", "comment: hi\ndata: a\n\n", "comment: ho\ndata: a\n\n", HTTPStreamConfig{Mode: HTTPStreamModeSSE}},
		{"text with carriage returns", "text/plain", "10%\r20%\r30%\ndone\n", "10%\r20%\r31%\ndone\n", HTTPStreamConfig{Mode: HTTPStreamModePlainText}},
	} {
		t.Run(c.name, func(t *testing.T) {
			switchStates(t, func(t *testing.T) {
				tc := throughJSON(t, streamTestCase(c.contentType, c.body))
				ok, _, info, err := CompareHTTPStream(tc.HTTPResp, strings.NewReader(c.body), c.cfg, map[string]struct{}{}, zap.NewNop())
				require.NoError(t, err)
				require.True(t, ok, "the recorded stream passes: %+v", info)

				ok, _, info, err = CompareHTTPStream(tc.HTTPResp, strings.NewReader(c.changed), c.cfg, map[string]struct{}{}, zap.NewNop())
				require.NoError(t, err)
				require.False(t, ok, "a changed stream must not pass")
				require.NotNil(t, info)
			})
		})
	}
}
