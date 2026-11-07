package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	yamlLib "gopkg.in/yaml.v3"
)

var marksT0 = time.Date(2026, 11, 5, 10, 0, 0, 123456789, time.UTC)

func sec(n int) time.Time { return marksT0.Add(time.Duration(n) * time.Second) }

func roundTripYAML(t *testing.T, resp HTTPResp) (HTTPResp, string) {
	t.Helper()
	out, err := yamlLib.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var back HTTPResp
	if err := yamlLib.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	return back, string(out)
}

// pieces is the body cut at its marks' offsets. A recording with no marks,
// or one whose offsets do not run in order inside its body, fails the row.
func pieces(t *testing.T, resp HTTPResp) []string {
	t.Helper()
	if resp.Marks == nil {
		t.Fatalf("no marks on %q", resp.Body)
	}
	var out []string
	start := 0
	for _, w := range resp.Marks.Writes {
		if w.Offset < start || w.Offset > len(resp.Body) {
			t.Fatalf("mark at %d does not fit after %d in a body of %d: %q", w.Offset, start, len(resp.Body), resp.Body)
		}
		out = append(out, resp.Body[start:w.Offset])
		start = w.Offset
	}
	return out
}

// An SSE frame takes the time of the write that completed it, with CRLF line
// ends, a frame cut across two writes, and a comment frame; the read-back
// body's pieces are its frames, each with the blank line after it.
func TestFramesTakeTheTimeOfTheWriteThatCompletedThem(t *testing.T) {
	body := "data: a\r\n\r\n: ping\r\n\r\nevent: x\r\ndata: b\r\n\r\n"
	// Write 1 ends in frame 3's first line, write 2 completes it.
	resp := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "text/event-stream"},
		Body:       body,
		Timestamp:  sec(9),
		Marks: &HTTPAnswerMarks{Head: sec(1), Writes: []HTTPAnswerWrite{
			{Offset: strings.Index(body, "data: b"), TS: sec(1)},
			{Offset: len(body), TS: sec(2)},
		}},
	}
	back, out := roundTripYAML(t, resp)
	if len(back.StreamBody) != 3 {
		t.Fatalf("frames = %d, want 3:\n%s", len(back.StreamBody), out)
	}
	for i, want := range []time.Time{sec(1), sec(1), sec(2)} {
		if !back.StreamBody[i].TS.Equal(want) {
			t.Errorf("frame %d ts = %v, want %v", i, back.StreamBody[i].TS, want)
		}
	}
	got := pieces(t, back)
	want := []string{"data:a\n\n: ping\n\n", "event:x\ndata:b\n\n"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("pieces = %q, want %q", got, want)
	}
	if !back.Marks.Head.Equal(sec(1)) {
		t.Errorf("head = %v, want %v", back.Marks.Head, sec(1))
	}
}

// NDJSON keepalive (blank) lines are no frames: a line takes the time of the
// write that ended it.
func TestNDJSONLinesTakeTheirWritesTime(t *testing.T) {
	body := "{\"n\":1}\n\n{\"n\":2}\n"
	resp := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/x-ndjson"},
		Body:       body,
		Marks: &HTTPAnswerMarks{Head: sec(0), Writes: []HTTPAnswerWrite{
			{Offset: 8, TS: sec(1)},
			{Offset: 9, TS: sec(2)},
			{Offset: len(body), TS: sec(3)},
		}},
	}
	back, out := roundTripYAML(t, resp)
	if len(back.StreamBody) != 2 || !back.StreamBody[0].TS.Equal(sec(1)) || !back.StreamBody[1].TS.Equal(sec(3)) {
		t.Fatalf("frames = %+v:\n%s", back.StreamBody, out)
	}
	if got := pieces(t, back); strings.Join(got, "|") != "{\"n\":1}\n|{\"n\":2}" {
		t.Errorf("pieces = %q", got)
	}
}

// A body kept whole keeps its marks as they are: they index it.
func TestWholeBodyKeepsItsMarks(t *testing.T) {
	marks := &HTTPAnswerMarks{Head: sec(1), Writes: []HTTPAnswerWrite{{Offset: 5, TS: sec(2)}, {Offset: 11, TS: sec(3)}}}
	resp := HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"a":"done`, Marks: marks}
	back, out := roundTripYAML(t, resp)
	if back.Body != resp.Body || back.Marks == nil || len(back.Marks.Writes) != 2 || back.Marks.Writes[0].Offset != 5 ||
		!back.Marks.Writes[0].TS.Equal(sec(2)) || !back.Marks.Writes[1].TS.Equal(sec(3)) || !back.Marks.Head.Equal(sec(1)) {
		t.Fatalf("read back %+v, %+v:\n%s", back.Body, back.Marks, out)
	}
	if !strings.Contains(out, "- {offset: 5, ts: ") {
		t.Errorf("a write is not on one line:\n%s", out)
	}
}

// A stream whose frames the marks cannot place (a line too long for the
// frames' parse, which keeps the body as one frame) keeps no marks: replay
// writes it at once.
func TestUnplacedFramesKeepNoMarks(t *testing.T) {
	body := "{\"n\":1}\n" + strings.Repeat("x", maxModelStreamTokenSize+1) + "\n"
	resp := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/x-ndjson"},
		Body:       body,
		Marks:      &HTTPAnswerMarks{Head: sec(0), Writes: []HTTPAnswerWrite{{Offset: 8, TS: sec(1)}, {Offset: len(body), TS: sec(2)}}},
	}
	back, _ := roundTripYAML(t, resp)
	if back.Marks != nil {
		t.Errorf("marks = %+v, want none", back.Marks)
	}
}

// BSON keeps the marks' times to the nanosecond, JSON keeps them, and a
// clone's marks are its own.
func TestMarksSurviveBSONJSONAndClone(t *testing.T) {
	resp := HTTPResp{StatusCode: 200, Body: "abcd", Marks: &HTTPAnswerMarks{Head: sec(1), Writes: []HTTPAnswerWrite{{Offset: 2, TS: sec(2)}, {Offset: 4, TS: sec(3)}}}}
	raw, err := bson.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var fromBSON HTTPResp
	if err := bson.Unmarshal(raw, &fromBSON); err != nil {
		t.Fatal(err)
	}
	js, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON HTTPResp
	if err := json.Unmarshal(js, &fromJSON); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]*HTTPAnswerMarks{"bson": fromBSON.Marks, "json": fromJSON.Marks} {
		if got == nil || !got.Head.Equal(sec(1)) || len(got.Writes) != 2 || got.Writes[1].Offset != 4 || !got.Writes[1].TS.Equal(sec(3)) {
			t.Errorf("%s: marks = %+v", name, got)
		}
	}
	var none HTTPResp
	raw, err = bson.Marshal(HTTPResp{StatusCode: 200})
	if err != nil {
		t.Fatal(err)
	}
	if err := bson.Unmarshal(raw, &none); err != nil || none.Marks != nil {
		t.Errorf("no marks read back as %+v (%v)", none.Marks, err)
	}
	c := resp.clone()
	if c.Marks == nil || !c.Marks.Head.Equal(sec(1)) || len(c.Marks.Writes) != 2 {
		t.Fatalf("a clone's marks = %+v, want the response's", c.Marks)
	}
	c.Marks.Writes[0].Offset = 3
	if resp.Marks.Writes[0].Offset != 2 {
		t.Error("a clone shares its marks")
	}
}

// A write that ends inside the CRLF blank line that ends an SSE frame did not
// complete it: the frame takes the time of the write that ends the blank line.
func TestAFrameEndsWithItsWholeCRLFBlankLine(t *testing.T) {
	body := "data: a\r\n\r\ndata: b\r\n\r\n"
	resp := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "text/event-stream"},
		Body:       body,
		Marks: &HTTPAnswerMarks{Head: sec(0), Writes: []HTTPAnswerWrite{
			{Offset: len("data: a\r\n\r"), TS: sec(1)},
			{Offset: len("data: a\r\n\r\n"), TS: sec(2)},
			{Offset: len(body), TS: sec(3)},
		}},
	}
	back, out := roundTripYAML(t, resp)
	if len(back.StreamBody) != 2 || !back.StreamBody[0].TS.Equal(sec(2)) || !back.StreamBody[1].TS.Equal(sec(3)) {
		t.Fatalf("frames = %+v, want frame 1 at write 2 and frame 2 at write 3:\n%s", back.StreamBody, out)
	}
	if got := pieces(t, back); strings.Join(got, "|") != "data:a\n\n|data:b\n\n" {
		t.Errorf("pieces = %q", got)
	}
	if !back.Marks.Writes[0].TS.Equal(sec(2)) {
		t.Errorf("frame 1's piece at %v, want %v", back.Marks.Writes[0].TS, sec(2))
	}
}

// The terminal write of an NDJSON stream, later than its last line, keeps its
// own mark at the body's end: replay ends the answer at its place, not at
// the last line's.
func TestTheTerminalWriteKeepsItsOwnMark(t *testing.T) {
	body := "{\"n\":1}\n{\"n\":2}\n"
	resp := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/x-ndjson"},
		Body:       body,
		Marks: &HTTPAnswerMarks{Head: sec(0), Writes: []HTTPAnswerWrite{
			{Offset: 8, TS: sec(1)},
			{Offset: len(body), TS: sec(2)},
			{Offset: len(body), TS: sec(5)},
		}},
	}
	back, out := roundTripYAML(t, resp)
	if len(back.StreamBody) != 2 || !back.StreamBody[1].TS.Equal(sec(2)) {
		t.Fatalf("frames = %+v:\n%s", back.StreamBody, out)
	}
	ws := back.Marks.Writes
	if len(ws) != 3 || ws[1].Offset != len(back.Body) || !ws[1].TS.Equal(sec(2)) ||
		ws[2].Offset != len(back.Body) || !ws[2].TS.Equal(sec(5)) {
		t.Errorf("writes = %+v, want the last line at 2s and the end at 5s, both at %d", ws, len(back.Body))
	}
}

// An NDJSON body of blank lines only has no line to place: it reads back as
// the body it was, and its marks index that body.
func TestBlankNDJSONKeepsMarksOnItsBody(t *testing.T) {
	body := "\n\n\n"
	resp := HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/x-ndjson"},
		Body:       body,
		Marks:      &HTTPAnswerMarks{Head: sec(0), Writes: []HTTPAnswerWrite{{Offset: 1, TS: sec(1)}, {Offset: 3, TS: sec(2)}}},
	}
	back, out := roundTripYAML(t, resp)
	if back.Body != body || len(back.StreamBody) != 1 || back.Marks == nil {
		t.Fatalf("read back %q, %d frames, marks %+v:\n%s", back.Body, len(back.StreamBody), back.Marks, out)
	}
	if ws := back.Marks.Writes; len(ws) != 1 || ws[0].Offset != len(body) || !ws[0].TS.Equal(sec(2)) {
		t.Errorf("writes = %+v, want the whole body at 2s", ws)
	}
}

// A response a producer gave only as frames (StreamBody, no Body) keeps the
// marks it came with: they already index the body its frames read back as.
func TestFramesOnlyResponseKeepsItsMarks(t *testing.T) {
	frames := SplitStreamBody(HTTPStreamSSE, "data: a\n\ndata: b\n\n")
	frames[0].TS, frames[1].TS = sec(1), sec(2)
	marks := &HTTPAnswerMarks{Head: sec(0), Writes: []HTTPAnswerWrite{{Offset: 8, TS: sec(1)}, {Offset: 14, TS: sec(2)}}}
	resp := HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "text/event-stream"}, StreamBody: frames, Marks: marks}
	back, out := roundTripYAML(t, resp)
	if back.Marks == nil || len(back.Marks.Writes) != 2 || back.Marks.Writes[0].Offset != 8 || !back.Marks.Writes[1].TS.Equal(sec(2)) {
		t.Fatalf("marks = %+v, want them as given:\n%s", back.Marks, out)
	}
}

// recovered runs f and reports a panic in it as the row's failure.
func recovered(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: panicked: %v", what, r)
		}
	}()
	f()
}

// The end of no marks, or of marks with no write, is no time; else it is the
// last write's.
func TestAnswerMarksEndOfNoWriteIsZero(t *testing.T) {
	for name, m := range map[string]*HTTPAnswerMarks{"no marks": nil, "no write": {Head: sec(1)}} {
		recovered(t, name, func() {
			if got := m.End(); !got.IsZero() {
				t.Errorf("%s: End = %v, want zero", name, got)
			}
		})
	}
	m := &HTTPAnswerMarks{Head: sec(1), Writes: []HTTPAnswerWrite{{Offset: 4, TS: sec(2)}, {Offset: 8, TS: sec(3)}}}
	if got := m.End(); !got.Equal(sec(3)) {
		t.Errorf("End = %v, want %v", got, sec(3))
	}
}

// The BSON form of no marks is none, and of marks each of them.
func TestAnswerMarksBSONFormKeepsEachMark(t *testing.T) {
	recovered(t, "no marks", func() {
		if b := (*HTTPAnswerMarks)(nil).toBSON(); b != nil {
			t.Errorf("no marks: %+v, want none", b)
		}
	})
	m := &HTTPAnswerMarks{Head: sec(1), Writes: []HTTPAnswerWrite{{Offset: 4, TS: sec(2)}, {Offset: 8, TS: sec(3)}}}
	recovered(t, "marks", func() {
		b := m.toBSON()
		if b == nil || b.Head != FormatMockTimestamp(sec(1)) || len(b.Writes) != 2 || b.Writes[1].Offset != 8 {
			t.Errorf("marks: %+v, want the head and both writes", b)
		}
	})
}

// Each kept frame's piece of the read-back body ends after the blank line
// that follows it, and the last one at the body's end.
func TestLegacyFramePiecesEndAfterEachFrame(t *testing.T) {
	chunks := parseSSEBodyToChunks("data: a\n\ndata: b\n\ndata: c\n\n", time.Time{})
	body := streamChunksToLegacyBody(chunks, streamBodyKindSSE)
	got, ok := legacyFramePieces(chunks, streamBodyKindSSE)
	want := []int{strings.Index(body, "data:b"), strings.Index(body, "data:c"), len(body)}
	if !ok || len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("pieces of %q = %v (%v), want %v", body, got, ok, want)
	}
}

// Chunks none of which is a frame (an SSE frame with no field written, a line
// with no raw field) have no pieces, though the body they read back as is
// empty or one line, and so ends where their no frames do: the answer keeps
// no marks.
func TestLegacyFramePiecesOfNoFrameAreNone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []HTTPStreamChunk
		kind   streamBodyKind
	}{
		{"an SSE frame with no field", []HTTPStreamChunk{{Data: []HTTPStreamDataField{{Key: " ", Value: "a"}}}}, streamBodyKindSSE},
		{"a line with no raw field", []HTTPStreamChunk{{Data: []HTTPStreamDataField{{Key: "line", Value: "a"}}}}, streamBodyKindRaw},
		{"a line with no field, which reads back as no body", []HTTPStreamChunk{{}}, streamBodyKindRaw},
	} {
		if got, ok := legacyFramePieces(tc.chunks, tc.kind); ok || got != nil {
			t.Errorf("%s: pieces %v (%v), want none", tc.name, got, ok)
		}
	}
}

// Frames whose pieces stop short of the body they read back as have no
// pieces: a line with no raw field after a frame, which the body reads back
// by its first field, and a last raw line that is blank, which the body ends
// with one newline more. The answer keeps no marks, so none of them points
// into the wrong bytes.
func TestLegacyFramePiecesThatStopShortOfTheBodyAreNone(t *testing.T) {
	raw := func(v string) HTTPStreamChunk {
		return HTTPStreamChunk{Data: []HTTPStreamDataField{{Key: "raw", Value: v}}}
	}
	for _, tc := range []struct {
		name   string
		chunks []HTTPStreamChunk
		body   string
	}{
		{"a line with no raw field after a frame", []HTTPStreamChunk{raw("a"), {Data: []HTTPStreamDataField{{Key: "line", Value: "b"}}}}, "a\nb"},
		{"a blank last line", []HTTPStreamChunk{raw("a"), raw("")}, "a\n\n"},
	} {
		if body := streamChunksToLegacyBody(tc.chunks, streamBodyKindRaw); body != tc.body {
			t.Fatalf("%s: body %q, want %q", tc.name, body, tc.body)
		}
		if got, ok := legacyFramePieces(tc.chunks, streamBodyKindRaw); ok || got != nil {
			t.Errorf("%s: pieces %v (%v), want none", tc.name, got, ok)
		}
	}
}

// A chunk before the first frame has no bytes of the body: its piece ends at
// the body's start, not after a separator.
func TestLegacyFramePiecesBeforeTheFirstFrameEndAtTheStart(t *testing.T) {
	chunks := append([]HTTPStreamChunk{{Data: []HTTPStreamDataField{{Key: " ", Value: "x"}}}},
		parseSSEBodyToChunks("data: a\n\ndata: b\n\n", time.Time{})...)
	body := streamChunksToLegacyBody(chunks, streamBodyKindSSE)
	got, ok := legacyFramePieces(chunks, streamBodyKindSSE)
	want := []int{0, strings.Index(body, "data:b"), len(body)}
	if !ok || len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("pieces of %q = %v (%v), want %v", body, got, ok, want)
	}
}

// A frame ends after the blank line that ends it, read with CR, LF or CRLF
// line ends; a blank line with no frame before it ends none.
func TestSSEFrameEndsAreTheFramesTheParserKeeps(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []int
	}{
		{"data: a\r\rdata: b\r\r", []int{9, 18}},
		{"data: a\n\n\ndata: b\n\n", []int{9, 19}},
		{"data: a\r\n\r\ndata: b", []int{11, 18}},
	} {
		got := sseFrameEnds(tc.body)
		if len(got) != len(tc.want) || (len(got) > 0 && (got[0] != tc.want[0] || got[len(got)-1] != tc.want[len(tc.want)-1])) {
			t.Errorf("sseFrameEnds(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// A stream is written as its frames with its marks moved onto them, or with
// none when it has none.
func TestStreamsMarshalWithAndWithoutMarks(t *testing.T) {
	body := "data: a\n\ndata: b\n\n"
	for name, marks := range map[string]*HTTPAnswerMarks{
		"no marks": nil,
		"marks":    {Head: sec(1), Writes: []HTTPAnswerWrite{{Offset: 9, TS: sec(1)}, {Offset: len(body), TS: sec(2)}}},
	} {
		recovered(t, name, func() {
			resp := HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "text/event-stream"}, Body: body, Timestamp: sec(9), Marks: marks}
			back, out := roundTripYAML(t, resp)
			if len(back.StreamBody) != 2 || (back.Marks != nil) != (marks != nil) {
				t.Errorf("%s: frames %d, marks %+v:\n%s", name, len(back.StreamBody), back.Marks, out)
			}
		})
	}
}
