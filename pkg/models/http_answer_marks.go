package models

import (
	"strings"
	"time"

	yamlLib "gopkg.in/yaml.v3"
)

// HTTPAnswerMarks is when each part of a recorded HTTP answer reached the
// application: the time of the write that completed its head, then one mark
// per write the proxy made of its body. A recording keeps marks only for an
// answer the proxy wrote in more than one write (a stream, a chunked body, a
// slow body); an answer written in one write has its one time in the mock's
// ResTimestampMock, and keeps none.
//
// The offsets index the body as the response holds it (HTTPResp.Body): its
// bytes de-chunked and decompressed. A recording that keeps the body as
// frames (SSE events, NDJSON lines) keeps the offsets in the body its frames
// read back as (streamChunksToLegacyBody), each frame's piece ending with the
// separator after it, and each frame's ts is the time of the write that
// completed it.
type HTTPAnswerMarks struct {
	// Head is the time of the write that completed the answer's head.
	Head time.Time `json:"head" yaml:"head"`
	// Writes are the answer's body writes, in order. Each ends at Offset in
	// the body, and the last ends the answer: its Offset is the body's
	// length, and its TS the time of the answer's last byte.
	Writes []HTTPAnswerWrite `json:"writes" yaml:"writes"`
}

// HTTPAnswerWrite is one write of an answer's body: the body offset it ended
// at, and when.
type HTTPAnswerWrite struct {
	Offset int       `json:"offset" yaml:"offset"`
	TS     time.Time `json:"ts" yaml:"ts"`
}

// MarshalYAML writes a write on one line: a long stream keeps one per write.
func (w HTTPAnswerWrite) MarshalYAML() (interface{}, error) {
	node := &yamlLib.Node{Kind: yamlLib.MappingNode, Tag: "!!map", Style: yamlLib.FlowStyle}
	var offset yamlLib.Node
	if err := offset.Encode(w.Offset); err != nil {
		return nil, err
	}
	node.Content = append(node.Content, stringNode("offset"), &offset, stringNode("ts"), stringNode(w.TS.UTC().Format(time.RFC3339Nano)))
	return node, nil
}

func (m *HTTPAnswerMarks) clone() *HTTPAnswerMarks {
	if m == nil {
		return nil
	}
	c := *m
	c.Writes = append([]HTTPAnswerWrite(nil), m.Writes...)
	return &c
}

// httpAnswerMarksBSON keeps the marks' times as FormatMockTimestamp strings:
// a BSON date keeps milliseconds, and a write's place is read against
// windows that open within one.
type httpAnswerMarksBSON struct {
	Head   string                `bson:"head"`
	Writes []httpAnswerWriteBSON `bson:"writes"`
}

type httpAnswerWriteBSON struct {
	Offset int    `bson:"offset"`
	TS     string `bson:"ts"`
}

func (m *HTTPAnswerMarks) toBSON() *httpAnswerMarksBSON {
	if m == nil {
		return nil
	}
	b := &httpAnswerMarksBSON{Head: FormatMockTimestamp(m.Head), Writes: make([]httpAnswerWriteBSON, len(m.Writes))}
	for i, w := range m.Writes {
		b.Writes[i] = httpAnswerWriteBSON{Offset: w.Offset, TS: FormatMockTimestamp(w.TS)}
	}
	return b
}

func (b *httpAnswerMarksBSON) marks() (*HTTPAnswerMarks, error) {
	if b == nil {
		return nil, nil
	}
	head, err := ParseMockTimestamp(b.Head)
	if err != nil {
		return nil, err
	}
	m := &HTTPAnswerMarks{Head: head, Writes: make([]HTTPAnswerWrite, len(b.Writes))}
	for i, w := range b.Writes {
		ts, err := ParseMockTimestamp(w.TS)
		if err != nil {
			return nil, err
		}
		m.Writes[i] = HTTPAnswerWrite{Offset: w.Offset, TS: ts}
	}
	return m, nil
}

// End is the time of the answer's last byte: its last write's.
func (m *HTTPAnswerMarks) End() time.Time {
	if m == nil || len(m.Writes) == 0 {
		return time.Time{}
	}
	return m.Writes[len(m.Writes)-1].TS
}

// writeAt is the time of the first write that had delivered the body up to
// offset: the write that completed a frame ending there.
func (m *HTTPAnswerMarks) writeAt(offset int) time.Time {
	for _, w := range m.Writes {
		if w.Offset >= offset {
			return w.TS
		}
	}
	return m.End()
}

// marksOnFrames is the frames a fresh recording keeps of resp's body (split
// from Body, as streamChunksForYAML does), each with the time of the write
// that completed it, and resp's marks moved to the body those frames read
// back as. A body whose frames this cannot place (a split it does not see as
// the frames' parser does) keeps its frames as they are and no marks: replay
// then writes it at once, as one with no marks.
func marksOnFrames(resp HTTPResp, chunks []HTTPStreamChunk) ([]HTTPStreamChunk, *HTTPAnswerMarks) {
	marks := resp.Marks
	var ends []int
	switch recordedFramesOf(getHeaderValueCaseInsensitiveModel(resp.Header, "Content-Type")) {
	case HTTPStreamSSE:
		ends = sseFrameEnds(resp.Body)
	case HTTPStreamNDJSON:
		ends = rawFrameEnds(resp.Body)
	case HTTPStreamBinary:
		ends = []int{len(resp.Body)}
	}
	if len(ends) != len(chunks) {
		return chunks, nil
	}
	kind := detectStreamBodyKind(resp.Header)
	pieces, ok := legacyFramePieces(chunks, kind)
	if !ok {
		return chunks, nil
	}
	out := &HTTPAnswerMarks{Head: marks.Head}
	for i := range chunks {
		chunks[i].TS = marks.writeAt(ends[i])
		w := HTTPAnswerWrite{Offset: pieces[i], TS: chunks[i].TS}
		if n := len(out.Writes); n > 0 && out.Writes[n-1].TS.Equal(w.TS) {
			out.Writes[n-1] = w
			continue
		}
		out.Writes = append(out.Writes, w)
	}
	// Each chunk wrote one, so out.Writes holds at least one.
	end := HTTPAnswerWrite{Offset: pieces[len(pieces)-1], TS: marks.End()}
	if !out.Writes[len(out.Writes)-1].TS.Equal(end.TS) {
		out.Writes = append(out.Writes, end)
	}
	return chunks, out
}

// legacyFramePieces is, for each of chunks, the offset in the body they read
// back as (streamChunksToLegacyBody) where its piece ends: the frame and the
// separator after it, the last frame at the body's end (an SSE body's with
// the blank line that ends its last event). A frame the body leaves out ends
// where the one before it did. ok is false when the pieces do not end at the
// body's end.
func legacyFramePieces(chunks []HTTPStreamChunk, kind streamBodyKind) ([]int, bool) {
	sep, end := 1, 0
	if kind == streamBodyKindSSE {
		sep, end = 2, 2
	}
	body := streamChunksToLegacyBody(chunks, kind)
	pieces := make([]int, len(chunks))
	pos, kept, last := 0, false, -1
	for i, c := range chunks {
		if frame, ok := legacyFrame(c, kind); ok {
			if kept {
				pos += sep
			}
			pos += len(frame)
			kept, last = true, i
		}
		pieces[i] = pos
	}
	// An SSE body ends its last event with a blank line. A raw body ends at
	// its last line: the frames placed here (marksOnFrames: NDJSON lines,
	// which are never blank, and a binary body whole) never end in the blank
	// line a text body's newline would be added for.
	if last < 0 || pos+end != len(body) {
		return nil, false
	}
	for i := range pieces {
		switch {
		case i >= last:
			pieces[i] = len(body)
		case pieces[i] > 0:
			pieces[i] += sep
		}
	}
	return pieces, true
}

// legacyFrame is chunk as streamChunksToLegacyBody writes it, without the
// separator between frames (an SSE event without the blank line that ends
// it); ok is false for one it leaves out. A line is its
// raw field, which every chunk a fresh body splits into has
// (splitStreamBody): a chunk without one is no frame here, so the pieces do
// not end at the body's end, and the answer keeps no marks (written at once).
func legacyFrame(chunk HTTPStreamChunk, kind streamBodyKind) (string, bool) {
	if kind == streamBodyKindSSE {
		frame := sseFrame(chunk)
		return frame, frame != ""
	}
	return chunk.ValueByKey("raw")
}

// lineEnd is the end of the line that starts at start in body, and the end of
// its terminator (CRLF, CR or LF, as normalizeModelLineEndings reads them);
// next is -1 for a last line with none.
func lineEnd(body string, start int) (end, next int) {
	i := strings.IndexAny(body[start:], "\r\n")
	if i < 0 {
		return len(body), -1
	}
	end = start + i
	if body[end] == '\r' && end+1 < len(body) && body[end+1] == '\n' {
		return end, end + 2
	}
	return end, end + 1
}

// sseFrameEnds is, for each frame parseSSEBodyToChunks keeps of body, the
// offset in body where the frame and the blank line that ends it end.
func sseFrameEnds(body string) []int {
	var ends []int
	frameStart, start := 0, 0
	for start < len(body) {
		end, next := lineEnd(body, start)
		if next < 0 {
			break
		}
		if end == start {
			if len(parseSSEBodyToChunks(body[frameStart:start], time.Time{})) > 0 {
				ends = append(ends, next)
			}
			frameStart = next
		}
		start = next
	}
	if len(parseSSEBodyToChunks(body[frameStart:], time.Time{})) > 0 {
		ends = append(ends, len(body))
	}
	return ends
}

// rawFrameEnds is, for each NDJSON line parseRawBodyToChunks keeps of body
// (blank ones are not frames), the offset in body where the line and its
// terminator end. A body it keeps as one frame (no line in it) ends at the
// body's end.
func rawFrameEnds(body string) []int {
	var ends []int
	for start := 0; start < len(body); {
		end, next := lineEnd(body, start)
		if strings.TrimSpace(body[start:end]) != "" {
			if next < 0 {
				next = len(body)
			}
			ends = append(ends, next)
		}
		if next < 0 {
			break
		}
		start = next
	}
	if len(ends) == 0 && body != "" {
		return []int{len(body)}
	}
	return ends
}
