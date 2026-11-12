package models

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlLib "gopkg.in/yaml.v3"
)

// sseEventsDispatched is the data of each event an SSE client dispatches
// from body, read as the WHATWG event-stream parse reads it: an event goes
// out at the blank line that ends it, and the one a stream ends inside,
// before its blank line, is dropped. Only data matters here: an event with
// no data line is not dispatched.
func sseEventsDispatched(body string) []string {
	var events, data []string
	pending := false
	for {
		end := strings.IndexAny(body, "\r\n")
		if end < 0 {
			// A line with no end: the stream ended inside it, and inside
			// its event, which is dropped.
			return events
		}
		line := body[:end]
		if body[end] == '\r' && end+1 < len(body) && body[end+1] == '\n' {
			end++
		}
		body = body[end+1:]
		switch {
		case line == "":
			if pending {
				events = append(events, strings.Join(data, "\n"))
			}
			data, pending = nil, false
		case strings.HasPrefix(line, ":"):
		default:
			name, value, _ := strings.Cut(line, ":")
			if name == "data" {
				data = append(data, strings.TrimPrefix(value, " "))
				pending = true
			}
		}
	}
}

// sseReadBack is an SSE response with body as a recording keeps it (its
// frames, in YAML) and reads it back: the body a mock replays.
func sseReadBack(t *testing.T, body string) HTTPResp {
	t.Helper()
	out, err := yamlLib.Marshal(HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "text/event-stream"},
		Body:       body,
		Timestamp:  time.Unix(1700000000, 0).UTC(),
	})
	require.NoError(t, err)
	var got HTTPResp
	require.NoError(t, yamlLib.Unmarshal(out, &got))
	require.NotEmpty(t, got.StreamBody, "the stream was not kept as its frames:\n%s", out)
	return got
}

// A recorded SSE stream reads back with each event's end, the last one's
// too, so a client dispatches every event it dispatched on record: an LLM
// stream's last event, "data: [DONE]", went out on record and never on
// replay, its client waiting for the end of the stream. The body ends in one
// blank line, never two: a recorded stream that ended in its blank line
// reads back so, and its frames are those it was recorded with.
func TestAnSSEStreamReadsBackWithItsLastEventEnded(t *testing.T) {
	for _, c := range []struct {
		name   string
		body   string
		events []string
		want   string
	}{
		{
			name:   "one event",
			body:   "data: only\n\n",
			events: []string{"only"},
			want:   "data:only\n\n",
		},
		{
			name:   "many events",
			body:   "id: 1\nevent: delta\ndata: {\"t\":\"Hel\"}\n\n: keepalive\n\nid: 2\ndata: {\"t\":\"lo\"}\ndata: second line\n\ndata: [DONE]\n\n",
			events: []string{`{"t":"Hel"}`, "{\"t\":\"lo\"}\nsecond line", "[DONE]"},
			want:   "id:1\nevent:delta\ndata:{\"t\":\"Hel\"}\n\n: keepalive\n\nid:2\ndata:{\"t\":\"lo\"}\ndata:second line\n\ndata:[DONE]\n\n",
		},
		{
			name:   "a stream that ended in more than one blank line",
			body:   "data: a\n\ndata: b\n\n\n\n",
			events: []string{"a", "b"},
			want:   "data:a\n\ndata:b\n\n",
		},
		{
			name:   "CRLF line ends",
			body:   "data: a\r\n\r\ndata: b\r\n\r\n",
			events: []string{"a", "b"},
			want:   "data:a\n\ndata:b\n\n",
		},
		{
			name:   "a blank line of a CR alone",
			body:   "data: a\r\rdata: b\r\r",
			events: []string{"a", "b"},
			want:   "data:a\n\ndata:b\n\n",
		},
		{
			name:   "mixed line ends",
			body:   "data: a\r\n\ndata: b\n\r\n",
			events: []string{"a", "b"},
			want:   "data:a\n\ndata:b\n\n",
		},
		{
			name:   "a blank line of a CR alone after a CRLF",
			body:   "data: a\n\ndata: b\r\n\r",
			events: []string{"a", "b"},
			want:   "data:a\n\ndata:b\n\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.events, sseEventsDispatched(c.body), "the row's recorded stream")
			got := sseReadBack(t, c.body)
			assert.Equal(t, c.want, got.Body, "the body a mock replays")
			assert.Equal(t, c.events, sseEventsDispatched(got.Body), "the events a client dispatches on replay: %q", got.Body)
			assert.False(t, strings.HasSuffix(got.Body, "\n\n\n"), "more than one blank line after the last event: %q", got.Body)
			assert.Equal(t, framesData(got.StreamBody), framesData(throughYAML(t, got).StreamBody),
				"the body did not split back into its frames: %q", got.Body)
		})
	}
}

// A stream that ended inside its last event, before its blank line, is kept
// as its bytes, never as frames, and reads back as recorded: its client
// dropped the unfinished event at record, and drops it on replay. Its frames
// would read back with that event ended, one event more than the client
// dispatched (an LLM stream cut before its "data: [DONE]" ended). A stream
// cut inside its first event is one: its client dispatched nothing. So is one
// that ends in a line of spaces, which is a line, not the blank line. It stays
// its bytes when written again.
func TestAnSSEStreamCutInsideItsLastEventIsKeptAsItsBytes(t *testing.T) {
	for _, c := range []struct {
		name, wire string
		events     []string // what its client dispatched at record
	}{
		{"cut inside a field", "data: a\n\ndata: b", []string{"a"}},
		{"cut after a field's line", "data: a\n\ndata: b\n", []string{"a"}},
		{"cut before the blank line of its last event", "data: a\n\ndata: [DONE]", []string{"a"}},
		{"cut after a CRLF", "data: a\r\n\r\ndata: b\r\n", []string{"a"}},
		{"cut after a CR", "data: a\r\rdata: b\r", []string{"a"}},
		{"cut inside a comment", "data: a\n\n: keep", []string{"a"}},
		{"cut inside its first event", "data: a", nil},
		{"cut before the blank line of its first event", "data: [DONE]", nil},
		{"cut after a line of spaces", "data: a\n\ndata: b\n \n", []string{"a"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.events, sseEventsDispatched(c.wire), "the row's recorded stream")
			resp := HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "text/event-stream"},
				Body:       c.wire,
				Timestamp:  time.Unix(1700000000, 0).UTC(),
			}
			out, err := yamlLib.Marshal(resp)
			require.NoError(t, err)
			var got HTTPResp
			require.NoError(t, yamlLib.Unmarshal(out, &got))
			assert.Empty(t, got.StreamBody, "a stream cut inside its last event was kept as frames:\n%s", out)
			assert.Equal(t, c.wire, got.Body, "the body a mock replays")
			assert.Equal(t, c.events, sseEventsDispatched(got.Body), "the events a client dispatches on replay")
			again := throughYAML(t, got)
			assert.Empty(t, again.StreamBody, "written again, it was kept as frames")
			assert.Equal(t, c.wire, again.Body, "written again")
		})
	}
}

// Frames that carry no line (no field with a name, no comment) are no event:
// a stream of only those reads back as no body, never a lone blank line.
func TestSSEFramesOfNoLineReadBackAsNoBody(t *testing.T) {
	assert.Equal(t, "", streamChunksToLegacyBody(nil, streamBodyKindSSE))
	assert.Equal(t, "", streamChunksToLegacyBody([]HTTPStreamChunk{
		{Data: []HTTPStreamDataField{{Key: " ", Value: "x"}}},
		{},
	}, streamBodyKindSSE))
	assert.Equal(t, "data:x\n\n", streamChunksToLegacyBody([]HTTPStreamChunk{
		{},
		{Data: []HTTPStreamDataField{{Key: "data", Value: "x"}}},
		{Data: []HTTPStreamDataField{{Key: "", Value: "y"}}},
	}, streamBodyKindSSE), "a frame of no line between events adds no blank line")
}
