package mapdb

import (
	"bytes"
	"regexp"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

// A mappings file is read a few test entries at a time.
//
// YAML parses a document into a tree of nodes before it decodes one value of
// it, and the tree of a mappings file is tens of times the file: a read of a
// 7 MB file held 16 times the file, and a write, which parsed it to keep what
// a hand edit added and decoded it, 32 times. A long recording's file reaches
// tens of MB, and k8s-proxy's auto-replay round reads its live recording's
// file three times as it starts and writes it as it ends, while the recorder
// rewrites it every few seconds: on a file of 35 MB each of those held from
// half a GB to more than 2 GB.
//
// So the test entries of a file in the layout keploy writes (marshalMappingYAML)
// are decoded a few at a time (mappingChunk, chunkBytes), each chunk parsed
// and decoded as the tests list of a document of its own, its lines as they
// are in the file, and the rest of the file as a document whose tests list
// is empty: the mapping it decodes to is the one the whole file decodes to. A
// test entry of many mock entries (keploy files every call it could not
// place in a test under one entry, which a long recording fills with most of
// its mocks) is decoded a few of its mock entries at a time the same way
// (decodeLargeEntry): taken whole, as one chunk of its own, its parse tree was
// the file's again. A file in any other layout, or one a part of which does
// not decode alone (an alias to an anchor in another part), is decoded whole,
// as before.
//
// A writer keeps what a hand edit added to the file it rewrites (keepEdits),
// which needs the file's tree. A file that is, after the comment lines it
// starts with, byte for byte keploy's encoding of the mapping it decodes to
// holds no hand edit there is to keep (plainDoc): it is not parsed whole.

// plainDoc is the document a writer is handed for an old file that holds no
// hand edit (decodeInto): keepEdits keeps nothing of it, and the comment lines
// it starts with are kept as for any file (commentHead).
var plainDoc = &yamlLib.Node{}

// topKey is a line that starts a key of the document's top mapping.
var topKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*:( |\n|\r\n|$)`)

// testsList is where the tests list of a YAML mappings file in keploy's
// layout lies: the line "tests:" at column 0 starts at key, its entries are
// data[entries[i][0]:entries[i][1]], and what follows the list starts at
// rest.
type testsList struct {
	key, rest int
	entries   [][2]int
}

// findTests finds the tests list of data laid out as keploy writes it: a line
// "tests:" at column 0, then the entries, each begun by a line "    - " (or a
// "    -" alone), every other line of them empty or indented five columns or
// more, up to a line at column 0 that starts another key, or the end of the
// file. ok is false for any other layout, and for a file with no entry.
func findTests(data []byte) (l testsList, ok bool) {
	at := 0
	for at < len(data) {
		line := lineAt(data, at)
		if string(bytes.TrimRight(line, "\r\n")) == "tests:" {
			l.key = at
			at += len(line)
			break
		}
		at += len(line)
		if at >= len(data) {
			return testsList{}, false
		}
	}
	if l.key == 0 && !bytes.HasPrefix(data, []byte("tests:")) {
		return testsList{}, false
	}
	l.rest = len(data)
	for at < len(data) {
		line := lineAt(data, at)
		body := bytes.TrimRight(line, "\r\n")
		switch {
		case len(body) == 0:
			if len(l.entries) == 0 {
				return testsList{}, false
			}
		case bytes.HasPrefix(body, []byte("    - ")) || string(body) == "    -":
			if n := len(l.entries); n > 0 {
				l.entries[n-1][1] = at
			}
			l.entries = append(l.entries, [2]int{at, len(data)})
		case bytes.HasPrefix(body, []byte("     ")):
			if len(l.entries) == 0 {
				return testsList{}, false
			}
		case body[0] != ' ' && topKey.Match(line):
			if len(l.entries) == 0 {
				return testsList{}, false
			}
			l.entries[len(l.entries)-1][1] = at
			l.rest = at
			return l, true
		default:
			// A comment, a document marker, or an indent keploy does not
			// write.
			return testsList{}, false
		}
		at += len(line)
	}
	return l, len(l.entries) > 0
}

// lineAt is the line of data that starts at at, with its newline.
func lineAt(data []byte, at int) []byte {
	if i := bytes.IndexByte(data[at:], '\n'); i >= 0 {
		return data[at : at+i+1]
	}
	return data[at:]
}

// decodeMappingYAML decodes data, a YAML mappings file, into m as
// yaml.Unmarshal does, a few test entries at a time when the file is
// in keploy's layout. It reports false, with m untouched, when it did not: the
// caller decodes the file whole.
func decodeMappingYAML(data []byte, m *models.Mapping) bool {
	l, ok := findTests(data)
	if !ok {
		return false
	}
	rest := make([]byte, 0, l.key+len("tests: []\n")+len(data)-l.rest)
	rest = append(rest, data[:l.key]...)
	rest = append(rest, "tests: []\n"...)
	rest = append(rest, data[l.rest:]...)
	var out models.Mapping
	if err := yamlLib.Unmarshal(rest, &out); err != nil {
		return false
	}
	out.TestCases = make([]models.MappedTestCase, 0, len(l.entries))
	// Each chunk is parsed under the tests key, its lines as they are in the
	// file: YAML reads them in the context it reads them in whole (a block
	// of text's indent is counted from its parents'), so a value reads the
	// same either way.
	var chunk []byte
	for start, end := 0, 0; start < len(l.entries); start = end {
		end = start + 1
		for end < len(l.entries) && end-start < mappingChunk && l.entries[end][1]-l.entries[start][0] <= chunkBytes {
			end++
		}
		if e := l.entries[start]; end == start+1 && e[1]-e[0] > chunkBytes {
			if tc, ok := decodeLargeEntry(data, e, &chunk); ok {
				out.TestCases = append(out.TestCases, tc)
				continue
			}
		}
		chunk = append(append(chunk[:0], "tests:\n"...), data[l.entries[start][0]:l.entries[end-1][1]]...)
		var part struct {
			Tests []models.MappedTestCase `yaml:"tests"`
		}
		if err := yamlLib.Unmarshal(chunk, &part); err != nil {
			return false
		}
		out.TestCases = append(out.TestCases, part.Tests...)
	}
	*m = out
	return true
}

// mockItems is where the items of the mock_entries list of the test entry
// at data[e[0]:e[1]] lie, laid out as keploy writes it: the line
// "      mock_entries:" at the entry's key column, then the items, each begun
// by a line "        - " (or a "        -" alone), every other line of
// them empty or indented ten columns or more, up to a line at the entry's
// key column, or the entry's end. ok is false for any other layout.
func mockItems(data []byte, e [2]int) (items [][2]int, ok bool) {
	at := e[0]
	for at < e[1] {
		line := lineAt(data[:e[1]], at)
		at += len(line)
		if string(bytes.TrimRight(line, "\r\n")) == "      mock_entries:" {
			break
		}
		if at >= e[1] {
			return nil, false
		}
	}
	for at < e[1] {
		line := lineAt(data[:e[1]], at)
		body := bytes.TrimRight(line, "\r\n")
		switch {
		case len(body) == 0:
			if len(items) == 0 {
				return nil, false
			}
		case bytes.HasPrefix(body, []byte("        - ")) || string(body) == "        -":
			if n := len(items); n > 0 {
				items[n-1][1] = at
			}
			items = append(items, [2]int{at, e[1]})
		case bytes.HasPrefix(body, []byte("          ")):
			if len(items) == 0 {
				return nil, false
			}
		case len(body) > 6 && bytes.HasPrefix(body, []byte("      ")) && body[6] != ' ' && body[6] != '-' && body[6] != '#':
			if len(items) == 0 {
				return nil, false
			}
			items[len(items)-1][1] = at
			return items, true
		default:
			return nil, false
		}
		at += len(line)
	}
	return items, len(items) > 0
}

// decodeLargeEntry decodes the test entry at data[e[0]:e[1]], laid out as
// keploy writes it, as the tests list of a document of its own does, a few
// of its mock entries at a time: one test of a long recording holds hundreds
// of thousands, and the parse tree of such an entry, read at once, held 510
// MB. The entry is decoded with its first mock entry only, in its place, as
// any other entry is (models.MappedTestCase.UnmarshalYAML: the shape it is
// read in is the one the whole entry is read in); its other mock entries are
// decoded as the mock_entries list of documents of their own, their lines as
// they are in the file, chunkMocks at a time and no more than about
// chunkBytes, and follow it. ok is false, and the entry is to be decoded
// whole, for any other layout, or when a part does not decode alone (an
// alias to an anchor in another part). chunk is a buffer to reuse.
func decodeLargeEntry(data []byte, e [2]int, chunk *[]byte) (models.MappedTestCase, bool) {
	items, ok := mockItems(data, e)
	if !ok || len(items) < 2 {
		return models.MappedTestCase{}, false
	}
	last := items[len(items)-1][1]
	c := append((*chunk)[:0], "tests:\n"...)
	c = append(c, data[e[0]:items[0][1]]...)
	c = append(c, data[last:e[1]]...)
	*chunk = c
	var head struct {
		Tests []models.MappedTestCase `yaml:"tests"`
	}
	if err := yamlLib.Unmarshal(c, &head); err != nil || len(head.Tests) != 1 {
		return models.MappedTestCase{}, false
	}
	tc := head.Tests[0]
	tc.Mocks = append(make([]models.MockEntry, 0, len(items)), tc.Mocks...)
	for start := 1; start < len(items); {
		end := start + 1
		for end < len(items) && end-start < chunkMocks && items[end][1]-items[start][0] <= chunkBytes {
			end++
		}
		c = append(append((*chunk)[:0], "mock_entries:\n"...), data[items[start][0]:items[end-1][1]]...)
		*chunk = c
		var part struct {
			MockEntries []models.MockEntry `yaml:"mock_entries"`
		}
		if err := yamlLib.Unmarshal(c, &part); err != nil {
			return models.MappedTestCase{}, false
		}
		tc.Mocks = append(tc.Mocks, part.MockEntries...)
		start = end
	}
	return tc, true
}

// plainYAML reports whether data, a YAML mappings file that decodes to m, is
// after the comment lines it starts with keploy's encoding of m, byte for
// byte: it then holds no hand edit a rewrite must keep (plainDoc).
func plainYAML(data []byte, m *models.Mapping) bool {
	return encodesTo(m, data[headLen(data):])
}

// headLen is how many bytes of data the comment and empty lines it starts
// with take.
func headLen(data []byte) int {
	at := 0
	for at < len(data) {
		line := lineAt(data, at)
		t := bytes.TrimSpace(line)
		if len(t) > 0 && t[0] != '#' {
			break
		}
		at += len(line)
	}
	return at
}
