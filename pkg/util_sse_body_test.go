package pkg

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// The body an SSE stream comparison gives back is the app's frames as the
// recording's body reads back: each event ended by its blank line, the last
// one too. A test whose stream differs shows the recorded body beside that
// one, so the diff holds the difference alone, never a blank line after the
// last event that only the recording has. No frame is no body.
func TestCompareHTTPStream_TheSSEBodyEndsItsLastEventAsTheRecordingDoes(t *testing.T) {
	out, err := yamlLib.Marshal(models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "text/event-stream"},
		Body:       "data:{\"n\":1}\n\ndata:{\"n\":2}\n\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	var recorded models.HTTPResp
	if err := yamlLib.Unmarshal(out, &recorded); err != nil || len(recorded.StreamBody) != 2 {
		t.Fatalf("the recording did not read back as its 2 frames (%v):\n%s", err, out)
	}
	for _, c := range []struct {
		name, stream, want string
	}{
		{"an event that differs", "data:{\"n\":1}\n\ndata:{\"n\":3}\n\n", "data:{\"n\":1}\n\ndata:{\"n\":3}\n\n"},
		{"a stream that ended an event short", "data:{\"n\":1}\n\n", "data:{\"n\":1}\n\n"},
		{"a stream of no event", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ok, body, _, err := CompareHTTPStream(recorded, strings.NewReader(c.stream), HTTPStreamConfig{Mode: HTTPStreamModeSSE}, map[string]struct{}{}, zap.NewNop())
			if ok || err != nil {
				t.Fatalf("CompareHTTPStream = %v, %v; want a difference", ok, err)
			}
			if body != c.want {
				t.Errorf("the stream's body is %q; want %q, its events ended as the recording's %q", body, c.want, recorded.Body)
			}
		})
	}
}

// A test case whose recorded stream ended inside its last event keeps its
// body as recorded, with no frames (models.HTTPResp), and the app's stream is
// compared with it as its events are read, under either switch: the bytes as
// recorded match, and an app whose last event differs, or that sent one
// event fewer, is told so.
func TestCompareHTTPStream_AStreamRecordedCutInsideItsLastEventCompares(t *testing.T) {
	const wire = "data: {\"n\":1}\n\ndata: {\"n\":2}"
	out, err := yamlLib.Marshal(models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "text/event-stream"},
		Body:       wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	var recorded models.HTTPResp
	if err := yamlLib.Unmarshal(out, &recorded); err != nil || len(recorded.StreamBody) != 0 || recorded.Body != wire {
		t.Fatalf("the recording did not read back as its bytes (%v):\n%s", err, out)
	}
	for _, c := range []struct {
		name, stream string
		ok           bool
	}{
		{"the stream as recorded", wire, true},
		{"its last event differs", "data: {\"n\":1}\n\ndata: {\"n\":3}", false},
		{"an event fewer", "data: {\"n\":1}\n\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ok, body, mismatch, err := CompareHTTPStream(recorded, strings.NewReader(c.stream), HTTPStreamConfig{Mode: HTTPStreamModeSSE}, map[string]struct{}{}, zap.NewNop())
			if err != nil || ok != c.ok || (mismatch == nil) != c.ok {
				t.Fatalf("CompareHTTPStream = %v, %q, %+v, %v; want ok %v", ok, body, mismatch, err, c.ok)
			}
		})
	}
}

// A frame after the recorded stream ended is a difference under the proven
// switch, and the body the comparison gives back for the diff ends each of
// the app's events as the recording's body does, the one past the recording
// too. Without the switch the read stops at the recording's end, and the
// stream matches.
func TestCompareHTTPStream_AFrameAfterTheRecordedStreamEndedIsShownEnded(t *testing.T) {
	out, err := yamlLib.Marshal(models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "text/event-stream"},
		Body:       "data:{\"n\":1}\n\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	var recorded models.HTTPResp
	if err := yamlLib.Unmarshal(out, &recorded); err != nil || len(recorded.StreamBody) != 1 {
		t.Fatalf("the recording did not read back as its frame (%v):\n%s", err, out)
	}
	ok, body, mismatch, err := CompareHTTPStream(recorded, strings.NewReader("data:{\"n\":1}\n\ndata:{\"n\":2}\n\n"), HTTPStreamConfig{Mode: HTTPStreamModeSSE}, map[string]struct{}{}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if !provenswitch.On() {
		if !ok || mismatch != nil {
			t.Fatalf("CompareHTTPStream = %v, %+v; want a match, the read stopped at the recording's end", ok, mismatch)
		}
		return
	}
	if ok || mismatch == nil || mismatch.ExpectedFrame != "(the recorded stream ended)" {
		t.Fatalf("CompareHTTPStream = %v, %+v; want the frame past the recording", ok, mismatch)
	}
	if want := "data:{\"n\":1}\n\ndata:{\"n\":2}\n\n"; body != want {
		t.Errorf("the stream's body is %q; want %q, its events ended as the recording's %q", body, want, recorded.Body)
	}
}
