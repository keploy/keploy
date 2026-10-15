package pkg

import (
	"io"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// frames is a recorded stream of chunks, each one field.
func frames(key string, values ...string) []models.HTTPStreamChunk {
	out := make([]models.HTTPStreamChunk, len(values))
	for i, v := range values {
		out[i] = models.HTTPStreamChunk{Data: []models.HTTPStreamDataField{{Key: key, Value: v}}}
	}
	return out
}

// Under the proven switch a streamed answer is read to its end, and a frame
// after the recorded ones is a difference (O-K): the comparison stopped at
// the last recorded frame, so a stream that went on (an error event after the
// answer, a second answer) passed. Without the switch it stops there, as
// before. A stream that ends with the recording passes either way.
func TestCompareHTTPStream_ProvenSwitchReadsTheStreamToItsEnd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      HTTPStreamConfig
		expected models.HTTPResp
		same     string
		// goesOn is the stream that goes on after the recorded frames, in
		// the reads it arrives in: the trailing data comes in a read of its
		// own, as it does from a server that writes it later.
		goesOn []string
	}{
		{
			name:     "SSE",
			cfg:      HTTPStreamConfig{Mode: HTTPStreamModeSSE},
			expected: models.HTTPResp{StreamBody: frames("data", "a", "b")},
			same:     "data: a\n\ndata: b\n\n",
			goesOn:   []string{"data: a\n\ndata: b\n\n", "event: error\ndata: boom\n\n"},
		},
		{
			name:     "NDJSON",
			cfg:      HTTPStreamConfig{Mode: HTTPStreamModeNDJSON},
			expected: models.HTTPResp{StreamBody: frames("raw", `{"id":1}`, `{"id":2}`)},
			same:     "{\"id\":1}\n{\"id\":2}\n",
			goesOn:   []string{"{\"id\":1}\n{\"id\":2}\n", "{\"error\":\"boom\"}\n"},
		},
		{
			name:     "plain text",
			cfg:      HTTPStreamConfig{Mode: HTTPStreamModePlainText},
			expected: models.HTTPResp{StreamBody: frames("raw", "line 1", "line 2")},
			same:     "line 1\nline 2\n",
			goesOn:   []string{"line 1\nline 2\n", "line 3\n"},
		},
		{
			name:     "binary",
			cfg:      HTTPStreamConfig{Mode: HTTPStreamModeBinary},
			expected: models.HTTPResp{StreamBody: frames("raw", "abcd")},
			same:     "abcd",
			goesOn:   []string{"abcd", "ef"},
		},
		{
			name:     "multipart",
			cfg:      HTTPStreamConfig{Mode: HTTPStreamModeMultipart, Boundary: "b"},
			expected: models.HTTPResp{Body: "--b\r\nContent-Type: text/plain\r\n\r\none\r\n--b--\r\n"},
			same:     "--b\r\nContent-Type: text/plain\r\n\r\none\r\n--b--\r\n",
			goesOn:   []string{"--b\r\nContent-Type: text/plain\r\n\r\none\r\n", "--b\r\nContent-Type: text/plain\r\n\r\ntwo\r\n--b--\r\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compare := func(reads ...string) (bool, *StreamMismatchInfo) {
				parts := make([]io.Reader, len(reads))
				for i, r := range reads {
					parts[i] = strings.NewReader(r)
				}
				ok, _, info, err := CompareHTTPStream(tc.expected, io.MultiReader(parts...), tc.cfg, map[string]struct{}{}, zap.NewNop())
				if err != nil {
					t.Fatalf("compare: %v", err)
				}
				return ok, info
			}

			t.Setenv(provenswitch.Env, "")
			if ok, _ := compare(tc.same); !ok {
				t.Fatal("control: the recorded stream does not match without the switch")
			}
			if ok, _ := compare(tc.goesOn...); !ok {
				t.Fatal("control: without the switch the comparison stops at the last recorded frame, as before")
			}

			t.Setenv(provenswitch.Env, "1")
			if ok, info := compare(tc.same); !ok {
				t.Fatalf("KEPLOY_PROVEN=1: the recorded stream did not match: %+v", info)
			}
			ok, info := compare(tc.goesOn...)
			if ok {
				t.Fatal("KEPLOY_PROVEN=1: a stream that went on after its recorded frames matched")
			}
			if info == nil {
				t.Fatal("KEPLOY_PROVEN=1: the trailing frame is not named")
			}
		})
	}
}

// Under the proven switch a keepalive after the recorded frames (an SSE frame
// of comments alone) is not a difference: it carries no event, and how many
// arrive depends on how long the stream took. An event after them still is.
func TestCompareHTTPStream_ProvenSwitchSkipsATrailingSSEKeepalive(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	expected := models.HTTPResp{StreamBody: frames("data", "a", "b")}
	compare := func(body string) (bool, *StreamMismatchInfo) {
		ok, _, info, err := CompareHTTPStream(expected, strings.NewReader(body), HTTPStreamConfig{Mode: HTTPStreamModeSSE}, map[string]struct{}{}, zap.NewNop())
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		return ok, info
	}
	if ok, info := compare("data: a\n\ndata: b\n\n: ping\n\n"); !ok {
		t.Fatalf("a trailing keepalive failed the stream: %+v", info)
	}
	if ok, _ := compare("data: a\n\ndata: b\n\n: ping\n\nevent: error\ndata: boom\n\n"); ok {
		t.Fatal("an event after a trailing keepalive passed")
	}
	if ok, _ := compare("data: a\n\ndata: b\n\n: ping\ndata: boom\n\n"); ok {
		t.Fatal("a frame with a comment and an event after the recorded frames passed")
	}
}
