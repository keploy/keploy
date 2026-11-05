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
