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

// pieces is the body cut at its marks' offsets.
func pieces(resp HTTPResp) []string {
	var out []string
	start := 0
	for _, w := range resp.Marks.Writes {
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
	got := pieces(back)
	want := []string{"data:a\n\n: ping\n\n", "event:x\ndata:b"}
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
	if got := pieces(back); strings.Join(got, "|") != "{\"n\":1}\n|{\"n\":2}" {
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
	if got := pieces(back); strings.Join(got, "|") != "data:a\n\n|data:b" {
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
