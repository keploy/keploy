package pkg

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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
