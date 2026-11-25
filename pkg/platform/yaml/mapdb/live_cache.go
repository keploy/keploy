package mapdb

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// A recording's writes of its mappings file hold little more than the file.
//
// A recording flushes its test-mock mappings every couple of seconds (and
// every few dozen tests), and each flush rewrote the file whole: it read the
// file, parsed it into a YAML document, decoded the mapping from it, merged the
// batch and encoded the file again over that document, to keep what a hand
// edit added. On a long recording the file reaches tens of MB, and the parse
// tree and the YAML encoder each held tens of times that: 2.3 GB for one flush
// at 35 MB (172,000 mock entries).
//
// So the file is encoded a few test entries at a time (marshalMappingYAML),
// a test entry of many mock entries a few of those at a time
// (encodeLargeEntry), and the db keeps the mapping it last wrote whole while the file holds no
// hand edit (it is byte for byte what the db encoded from that mapping): the
// next write of the same set that finds the file as it left it changes that
// mapping and encodes it, with no read and no parse of the file. A file
// anything else wrote since, or one a person edited, is read and parsed as
// before, its hand edits kept. One set is kept, the one written last: a
// recording writes one set at a time.

// liveMapping is the mapping a db last wrote whole, of one test set, and its
// file as the write left it.
type liveMapping struct {
	testSetID string
	format    yaml.Format
	path      string
	// info is the file right after the write: a file found otherwise was
	// written by another, or edited.
	info os.FileInfo
	// mapping is what the file holds. The next write changes it in place, and
	// forgets it when that write fails.
	mapping *models.Mapping
	// head is the comment lines the file starts with that a rewrite keeps
	// (commentHead): the YAML encoding goes after them.
	head []byte
}

// liveMappings is a db's liveMapping, nil for none.
type liveMappings struct {
	p atomic.Pointer[liveMapping]
}

// take is the kept mapping of the set's file at path in format, when the file
// is as the db's last write left it; nil otherwise, and then nothing is kept.
func (l *liveMappings) take(root, testSetID, path string, format yaml.Format) *liveMapping {
	lm := l.p.Load()
	if lm == nil {
		return nil
	}
	if lm.testSetID != testSetID || lm.format != format || lm.path != path {
		return nil
	}
	info, err := yaml.StatInRecording(root, path)
	if err != nil || !os.SameFile(info, lm.info) || info.Size() != lm.info.Size() || !info.ModTime().Equal(lm.info.ModTime()) {
		l.p.CompareAndSwap(lm, nil)
		return nil
	}
	return lm
}

// forget drops what is kept.
func (l *liveMappings) forget() { l.p.Store(nil) }

// encodeKept is mapping encoded as a write of a file that holds no hand edit
// encodes it: what encodeOver writes over the file the kept mapping is of,
// with no read of it.
func encodeKept(mapping *models.Mapping, format yaml.Format, head []byte) (encoded, error) {
	if format == yaml.FormatJSON {
		data, err := marshalMapping(format, mapping)
		return encoded{data: data}, err
	}
	return encoded{head: head, mapping: mapping}, nil
}

// encoded is a mappings file a write makes: data, its bytes; or, for a YAML
// file that holds no hand edit (mapping set), head followed by mapping's
// encoding (marshalMappingYAML), which writeTo makes a few entries at a time
// as it writes them: such a file is never whole in memory. A recording's
// file reaches tens of MB, and a buffer of it, made at every flush, held
// that much again.
type encoded struct {
	data    []byte
	head    []byte
	mapping *models.Mapping
}

// writeTo writes the file to w.
func (e encoded) writeTo(w io.Writer) error {
	if e.mapping == nil {
		_, err := w.Write(e.data)
		return err
	}
	if _, err := w.Write(e.head); err != nil {
		return err
	}
	var werr error
	err := encodeMappingYAML(e.mapping, func(p []byte) bool {
		_, werr = w.Write(p)
		return werr == nil
	})
	if werr != nil {
		return werr
	}
	return err
}

// bytes is the file, whole, for a caller that hands it on as bytes.
func (e encoded) bytes() ([]byte, error) {
	if e.mapping == nil {
		return e.data, nil
	}
	return marshalMappingYAMLAfter(e.head, e.mapping, 0)
}

// keep keeps mapping, which out, just written to the set's file at path,
// encodes, when the file holds no hand edit: out is mapping's encoding after
// a head (encoded.mapping), or its bytes are what encodeKept makes of it.
// Otherwise nothing is kept.
func (l *liveMappings) keep(root, testSetID, path string, format yaml.Format, mapping *models.Mapping, out encoded, encodedKept bool) {
	var head []byte
	plain := encodedKept || out.mapping != nil
	switch {
	case out.mapping != nil:
		// The encoding starts with a key: the comment lines the file starts
		// with are out's head's.
		head = commentHead(out.head)
	case format != yaml.FormatJSON:
		head = commentHead(out.data)
	}
	if !plain {
		data := out.data
		if format == yaml.FormatJSON {
			enc, err := marshalMapping(format, mapping)
			plain = err == nil && bytes.Equal(enc, data)
		} else {
			// Compared as it is encoded, a few entries at a time.
			plain = bytes.HasPrefix(data, head) && encodesTo(mapping, data[len(head):])
		}
		if !plain {
			l.forget()
			return
		}
	}
	info, err := yaml.StatInRecording(root, path)
	if err != nil {
		l.forget()
		return
	}
	l.p.Store(&liveMapping{testSetID: testSetID, format: format, path: path, info: info, mapping: mapping, head: head})
}

// mappingFilePath is the set's mappings file in format.
func mappingFilePath(mappingPath, fileName string, format yaml.Format) string {
	return filepath.Join(mappingPath, fileName+"."+format.FileExtension())
}

// marshalMapping is yaml.MarshalGeneric(format, mapping), a YAML file made a
// few test entries at a time (marshalMappingYAML).
func marshalMapping(format yaml.Format, mapping *models.Mapping) ([]byte, error) {
	if format == yaml.FormatJSON {
		return yaml.MarshalGeneric(format, mapping)
	}
	return marshalMappingYAML(mapping)
}

// mappingChunk is how many test entries marshalMappingYAML encodes at a time,
// and decodeMappingYAML decodes, at most; chunkMocks how many mock entries an
// encoded chunk holds, and chunkBytes how long a decoded chunk is, past its
// first test entry: a test of hundreds of mocks is a chunk of its own, and one
// of more than chunkMocks (or past chunkBytes, read) is encoded and decoded
// that many of its mock entries at a time (encodeLargeEntry, decodeLargeEntry).
const (
	mappingChunk = 64
	chunkMocks   = 256
	chunkBytes   = 64 << 10
)

// chunkEnd is where the chunk of tests that starts at start ends: after
// mappingChunk tests, or once it holds chunkMocks mock entries.
func chunkEnd(tests []models.MappedTestCase, start int) int {
	end, mocks := start, 0
	for end < len(tests) && end-start < mappingChunk && (end == start || mocks+len(tests[end].Mocks) <= chunkMocks) {
		mocks += len(tests[end].Mocks)
		end++
	}
	return end
}

// marshalMappingYAML is yaml.MarshalGeneric(yaml.FormatYAML, mapping), byte
// for byte, made mappingChunk test entries at a time. The YAML encoder holds
// what it encodes many times over while it encodes it (410 MB for a 7 MB
// mappings file), and a recording encodes its mappings file at every flush.
// Encoded a few entries at a time, and a test entry of many mock entries a few
// of those at a time (encodeLargeEntry), it holds a few entries' worth, and
// the file being made.
//
// The test entries are the items of the file's tests: list, which YAML lays
// out under the key the way it lays out a list of its own at the top of a
// document, four columns in: so each chunk, encoded as a list of its own, is
// put in place with its every line but the empty ones (a block of text keeps
// them empty) moved four columns right.
func marshalMappingYAML(mapping *models.Mapping) ([]byte, error) {
	return marshalMappingYAMLAfter(nil, mapping, 0)
}

// marshalMappingYAMLAfter is head followed by marshalMappingYAML(mapping),
// made in one buffer of about head and size bytes (the file's length before,
// 0 when unknown): a copy of the encoding to put head before it, or a buffer
// that doubles as it grows, would each hold the file again.
func marshalMappingYAMLAfter(head []byte, mapping *models.Mapping, size int) ([]byte, error) {
	var out bytes.Buffer
	if size > 0 {
		out.Grow(len(head) + size + size/8 + 4096)
	}
	out.Write(head)
	err := encodeMappingYAML(mapping, func(p []byte) bool {
		out.Write(p)
		return true
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// encodesTo reports whether marshalMappingYAML(mapping) is want, byte for
// byte, comparing it as it is made: no more of the encoding than a few test
// entries' is held at once, and the encoding stops at the first difference.
func encodesTo(mapping *models.Mapping, want []byte) bool {
	pos, same := 0, true
	err := encodeMappingYAML(mapping, func(p []byte) bool {
		if len(p) > len(want)-pos || !bytes.Equal(want[pos:pos+len(p)], p) {
			same = false
			return false
		}
		pos += len(p)
		return true
	})
	return err == nil && same && pos == len(want)
}

// encodeMappingYAML hands emit marshalMappingYAML(mapping), piece by piece,
// in order, until emit returns false. Each piece is whole lines.
func encodeMappingYAML(mapping *models.Mapping, emit func([]byte) bool) error {
	if mapping == nil || len(mapping.TestCases) <= mappingChunk && countMocks(mapping.TestCases) <= chunkMocks {
		data, err := yaml.MarshalGeneric(yaml.FormatYAML, mapping)
		if err == nil {
			emit(data)
		}
		return err
	}
	skeleton := *mapping
	skeleton.TestCases = []models.MappedTestCase{}
	head, err := yaml.MarshalGeneric(yaml.FormatYAML, &skeleton)
	if err != nil {
		return err
	}
	const emptyTests = "\ntests: []\n"
	at := bytes.Index(head, []byte(emptyTests))
	if at < 0 {
		// A layout this does not know: encoded whole.
		data, err := yaml.MarshalGeneric(yaml.FormatYAML, mapping)
		if err == nil {
			emit(data)
		}
		return err
	}
	// The tests list's items lie four columns in (see marshalMappingYAML).
	var buf []byte
	inTests := func(p []byte) bool {
		buf = indented(buf[:0], p, testsIndent)
		return emit(buf)
	}
	if !emit(head[:at]) || !emit([]byte("\ntests:\n")) {
		return nil
	}
	for start, end := 0, 0; start < len(mapping.TestCases); start = end {
		end = chunkEnd(mapping.TestCases, start)
		if end == start+1 && len(mapping.TestCases[start].Mocks) > chunkMocks {
			more, err := encodeLargeEntry(mapping.TestCases[start], inTests)
			if err != nil || !more {
				return err
			}
			continue
		}
		part, err := yaml.MarshalGeneric(yaml.FormatYAML, mapping.TestCases[start:end])
		if err != nil {
			return err
		}
		if !inTests(part) {
			return nil
		}
	}
	emit(head[at+len(emptyTests):])
	return nil
}

// testsIndent is how far right of a list encoded on its own the items of the
// tests list lie in a mappings file, and mockEntriesIndent how far right of
// a list encoded on its own the items of a test entry's mock_entries list lie
// in the test entry encoded on its own.
var (
	testsIndent       = []byte("    ")
	mockEntriesIndent = []byte("    ")
)

// countMocks is how many mock entries tests hold.
func countMocks(tests []models.MappedTestCase) int {
	n := 0
	for _, t := range tests {
		n += len(t.Mocks)
	}
	return n
}

// indented is dst followed by part, whole lines, each but the empty ones (a
// block of text keeps them empty) moved indent columns right.
func indented(dst, part, indent []byte) []byte {
	for len(part) > 0 {
		line := part
		if nl := bytes.IndexByte(part, '\n'); nl >= 0 {
			line = part[:nl+1]
		}
		part = part[len(line):]
		if len(line) > 1 {
			dst = append(dst, indent...)
		}
		dst = append(dst, line...)
	}
	return dst
}

// encodeLargeEntry hands emit what yaml.MarshalGeneric encodes the list
// []models.MappedTestCase{tc} to, byte for byte, a few of its mock entries
// at a time (chunkMocks): a test entry holds a mock entry for each call its
// test made, and one test of a long recording (every call keploy could not
// place in a test, which it files under one entry) holds hundreds of
// thousands. Encoded at once, such an entry held 390 MB in the YAML encoder.
//
// The entry is encoded with its first mock entry only: its every other field
// is laid out as for the whole entry, after that mock entry's lines, which
// lie under the key "mock_entries" as the list of that one mock entry encoded
// on its own does, mockEntriesIndent columns right. The other mock entries
// are encoded as lists of their own, a few at a time, and put in place
// after it the same way. It reports false once emit has.
func encodeLargeEntry(tc models.MappedTestCase, emit func([]byte) bool) (bool, error) {
	first := tc
	first.Mocks = tc.Mocks[:1]
	entry, err := yaml.MarshalGeneric(yaml.FormatYAML, []models.MappedTestCase{first})
	if err != nil {
		return false, err
	}
	one, err := yaml.MarshalGeneric(yaml.FormatYAML, tc.Mocks[:1])
	if err != nil {
		return false, err
	}
	want := indented([]byte("\n  mock_entries:\n"), one, mockEntriesIndent)
	at := bytes.Index(entry, want)
	if at < 0 {
		// A layout this does not know: the entry encoded whole.
		whole, err := yaml.MarshalGeneric(yaml.FormatYAML, []models.MappedTestCase{tc})
		if err != nil {
			return false, err
		}
		return emit(whole), nil
	}
	split := at + len(want)
	if !emit(entry[:split]) {
		return false, nil
	}
	var buf []byte
	for start := 1; start < len(tc.Mocks); start += chunkMocks {
		part, err := yaml.MarshalGeneric(yaml.FormatYAML, tc.Mocks[start:min(start+chunkMocks, len(tc.Mocks))])
		if err != nil {
			return false, err
		}
		buf = indented(buf[:0], part, mockEntriesIndent)
		if !emit(buf) {
			return false, nil
		}
	}
	return emit(entry[split:]), nil
}
