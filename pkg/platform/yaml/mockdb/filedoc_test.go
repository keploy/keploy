package mockdb

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

func httpDocYAML(name, kind, path string) string {
	return "version: api.keploy.io/v1beta1\nkind: " + kind + "\nname: " + name + "\nspec:\n    req:\n        method: GET\n        url: http://h/" + path + "\n"
}

func httpDocJSON(name, kind, path string) string {
	return `{"version":"api.keploy.io/v1beta1","kind":"` + kind + `","name":"` + name + `","spec":{"req":{"method":"GET","url":"http://h/` + path + `"}}}`
}

// readDocs is every document MockFileReader returns of the set's mock file.
func readDocs(t *testing.T, dir string, format yaml.Format) []MockFileDoc {
	t.Helper()
	fr, err := NewMockFileReader(context.Background(), nil, filepath.Join(dir, pruneSet), "mocks", format)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fr.Close() }()
	var out []MockFileDoc
	for {
		d, err := fr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
}

func writeSetFile(t *testing.T, dir string, format yaml.Format, body string) {
	t.Helper()
	ext := "yaml"
	if format == yaml.FormatJSON {
		ext = "json"
	}
	if err := os.MkdirAll(filepath.Join(dir, pruneSet), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pruneSet, "mocks."+ext), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// MockFileReader is the replay's own read of a set's mock file, one document
// at a time: of each document of a hand-edited file it says what the read of
// the set (GetTestSetMocks) does with it. It serves the mocks the set serves,
// and names, with why, each document the set skips: a mistyped kind, a
// removed kind, a connection failure this keploy cannot replay, and the last
// document of a file with no final newline, which parses whole and is still
// skipped as a write cut short. A caller (keploy's refusal, enterprise's
// check) so never reads a file differently from the replay.
func TestTheMockFileReaderSaysOfEachDocumentWhatTheReplayDoes(t *testing.T) {
	cases := map[yaml.Format]string{
		yaml.FormatYAML: httpDocYAML("mock-0", "Http", "a") +
			"---\n" + httpDocYAML("mock-1", "HTTP", "b") +
			"---\n" + strings.Replace(httpDocYAML("mock-2", "Http", "c"), "kind: Http\n", "", 1) +
			"---\n" + cfDocYAML("cf-newer", "request", "reset", true) +
			"---\n" + cfDocYAML("cf-ok", "connect", "refused", true) +
			"---\n" + strings.TrimSuffix(httpDocYAML("mock-9", "Http", "z"), "\n"),
		yaml.FormatJSON: httpDocJSON("mock-0", "Http", "a") + "\n" +
			httpDocJSON("mock-1", "HTTP", "b") + "\n" +
			strings.Replace(httpDocJSON("mock-2", "Http", "c"), `"kind":"Http",`, "", 1) + "\n" +
			cfDocJSON("cf-newer", "request", "reset", true) + "\n" +
			cfDocJSON("cf-ok", "connect", "refused", true) + "\n" +
			strings.TrimSuffix(httpDocJSON("mock-9", "Http", "z"), "}"),
	}
	want := map[string]models.MockSkip{"mock-0": "", "mock-1": models.MockSkipUnknownKind, "mock-2": models.MockSkipNoKind, "cf-newer": models.MockSkipConnFailure, "cf-ok": "", "mock-9": models.MockSkipCutShort}
	for format, body := range cases {
		t.Run(string(format), func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", "")
			dir := t.TempDir()
			writeSetFile(t, dir, format, body)
			docs := readDocs(t, dir, format)
			got := map[string]models.MockSkip{}
			var served []string
			for _, d := range docs {
				if d.Err != nil {
					t.Fatalf("%s: %v", d.Name, d.Err)
				}
				if format == yaml.FormatJSON && d.Name == "" && d.Skip == models.MockSkipCutShort {
					d.Name = "mock-9" // an NDJSON line cut short does not parse: it has no name
				}
				got[d.Name] = d.Skip
				if (d.Skip == "") != (len(d.Mocks) == 1) {
					t.Fatalf("%s: skip %q with %d mocks", d.Name, d.Skip, len(d.Mocks))
				}
				for _, mk := range d.Mocks {
					served = append(served, mk.Name)
				}
				if format == yaml.FormatYAML && d.Doc == nil {
					t.Fatalf("%s: a YAML document that parses has its parsed form", d.Name)
				}
			}
			if len(got) != len(want) {
				t.Fatalf("documents: got %v, want %v", got, want)
			}
			for n, w := range want {
				if got[n] != w {
					t.Fatalf("%s: skip %q, want %q", n, got[n], w)
				}
			}
			// The oracle: the set's own read serves exactly these mocks, and
			// lists as skipped exactly the named documents the reader skips.
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", format)
			set, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var read []string
			for _, mk := range append(append([]*models.Mock(nil), set.AllPerTest...), set.AllSession...) {
				read = append(read, mk.Name)
			}
			sort.Strings(read)
			sort.Strings(served)
			if strings.Join(read, ",") != strings.Join(served, ",") {
				t.Fatalf("the set serves %v, the reader %v", read, served)
			}
			for _, d := range docs {
				if _, listed := set.Skipped[d.Name]; d.Name != "" && listed != (d.Skip != "") {
					t.Fatalf("%s: the set lists it skipped %v, the reader says %q", d.Name, listed, d.Skip)
				}
			}
			// SkippedMocks names the file the set's read takes and what it
			// skips, as TestSetMocks.Skipped lists it.
			file, skipped := ys.SkippedMocks(context.Background(), pruneSet)
			if filepath.Base(file) != "mocks."+format.FileExtension() || skipped["mock-1"].Kind != "HTTP" || skipped["mock-1"].Why != models.MockSkipUnknownKind || skipped["mock-2"].Why != models.MockSkipNoKind || skipped["cf-newer"].Why != models.MockSkipConnFailure {
				t.Fatalf("SkippedMocks: %s %v", file, skipped)
			}
			for n := range skipped {
				if _, listed := set.Skipped[n]; !listed {
					t.Fatalf("SkippedMocks lists %s, which the set's read does not skip", n)
				}
			}
		})
	}
}

// A document the replay cannot read fails the read of the set; the reader
// returns it with why, named when it parses, and reads on: a document that
// does not parse, one that parses and does not decode, and the document
// before an unfinished separator that does not parse. The start of a
// separator the file ends in is no document.
func TestTheMockFileReaderReadsOnPastADocumentTheReplayCannotRead(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	dir := t.TempDir()
	body := httpDocYAML("mock-0", "Http", "a") +
		"---\nname: [mock-1\n" +
		"---\n" + strings.Replace(httpDocYAML("mock-2", "Http", "c"), "    req:\n        method: GET\n        url: http://h/c\n", "    req: [1, 2]\n", 1) +
		"---\n" + httpDocYAML("mock-3", "Http", "d") + "--"
	writeSetFile(t, dir, yaml.FormatYAML, body)
	docs := readDocs(t, dir, yaml.FormatYAML)
	if len(docs) != 4 {
		t.Fatalf("want 4 documents, got %d: %+v", len(docs), docs)
	}
	if docs[0].Err != nil || len(docs[0].Mocks) != 1 || docs[3].Err != nil || docs[3].Name != "mock-3" || len(docs[3].Mocks) != 1 {
		t.Fatalf("the readable documents: %+v / %+v", docs[0], docs[3])
	}
	if docs[1].Err == nil || docs[1].Name != "" || !strings.Contains(docs[1].Err.Error(), "line") {
		t.Fatalf("a document that does not parse: %+v", docs[1])
	}
	if docs[2].Err == nil || docs[2].Name != "mock-2" || docs[2].Doc == nil || len(docs[2].Mocks) != 0 {
		t.Fatalf("a document that does not decode: %+v", docs[2])
	}
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	if _, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil); err == nil {
		t.Fatal("the set's read must fail on a document it cannot read")
	}

	// The document before an unfinished separator that does not parse says
	// why, with the parse's line.
	writeSetFile(t, dir, yaml.FormatYAML, httpDocYAML("mock-0", "Http", "a")+"---\nname: [mock-1\n--")
	docs = readDocs(t, dir, yaml.FormatYAML)
	if len(docs) != 2 || docs[1].Err == nil || !strings.Contains(docs[1].Err.Error(), "unfinished separator") || !strings.Contains(docs[1].Err.Error(), "line") {
		t.Fatalf("before an unfinished separator: %+v", docs)
	}

	// A file that ends in the start of a separator after the last one ends
	// in no document; the set reads it whole.
	writeSetFile(t, dir, yaml.FormatYAML, httpDocYAML("mock-0", "Http", "a")+"---\n--")
	docs = readDocs(t, dir, yaml.FormatYAML)
	if len(docs) != 1 || docs[0].Name != "mock-0" || len(docs[0].Mocks) != 1 {
		t.Fatalf("a file ending in the start of a separator: %+v", docs)
	}
	set, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil)
	if err != nil || len(set.AllPerTest)+len(set.AllSession) != 1 || len(set.Skipped) != 0 {
		t.Fatalf("the set's read of a file ending in the start of a separator: %v, %+v", err, set)
	}
}

// readSetOracle is the set's own read (GetTestSetMocks) of the set's mock
// file in dir: what it serves, by name, and what it lists as skipped.
func readSetOracle(t *testing.T, dir string, format yaml.Format) (map[string]bool, map[string]models.Kind) {
	t.Helper()
	set, err := NewWithFormat(zap.NewNop(), dir, "mocks", format).GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	for _, mk := range append(append([]*models.Mock(nil), set.AllPerTest...), set.AllSession...) {
		served[mk.Name] = true
	}
	return served, set.Skipped
}

// The shapes a hand edit or another build leaves that enterprise's check
// reads through MockFileReader, each against the set's own read: a kind
// with a "-" in it (an enterprise kind), a kind a registered mapper decodes,
// two documents with one name, and a named NDJSON last line cut short. In
// YAML a mapped kind is served and an unmapped "-" kind is one this keploy
// cannot read; in NDJSON both are kinds keploy reads only from YAML, which no
// upgrade changes. Of one name listed twice, SkippedMocks says what the set's
// read lists, the last. A named NDJSON line cut short whose JSON is whole
// says why its spec does not decode.
func TestTheMockFileReaderSaysWhatTheReplayDoesOfKindsNamesAndCutsTheReplaySkips(t *testing.T) {
	const mapped = models.Kind("EnterpriseKind")
	clearRegistry(t)
	RegisterMockYAMLMapper(mapped, MockYAMLMapper{
		Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil },
		Decode: func(doc *yaml.NetworkTrafficDoc, m *models.Mock) error {
			*m = models.Mock{Version: doc.Version, Name: doc.Name, Kind: doc.Kind}
			return nil
		},
	})
	cases := map[yaml.Format]string{
		yaml.FormatYAML: httpDocYAML("mock-0", "Http", "a") +
			"---\n" + httpDocYAML("mock-1", "Redis-Ent", "b") +
			"---\n" + httpDocYAML("mock-2", string(mapped), "c") +
			"---\n" + httpDocYAML("mock-3", "HTTP", "d") +
			"---\n" + strings.Replace(httpDocYAML("mock-3", "Http", "e"), "kind: Http\n", "", 1),
		yaml.FormatJSON: httpDocJSON("mock-0", "Http", "a") + "\n" +
			httpDocJSON("mock-1", "Redis-Ent", "b") + "\n" +
			httpDocJSON("mock-2", string(mapped), "c") + "\n" +
			httpDocJSON("mock-3", "HTTP", "d") + "\n" +
			strings.Replace(httpDocJSON("mock-3", "Http", "e"), `"kind":"Http",`, "", 1) + "\n" +
			`{"version":"api.keploy.io/v1beta1","kind":"Http","name":"mock-9","spec":{"req":[1,2]}}`,
	}
	wantSkip := map[yaml.Format]map[string]models.MockSkip{
		yaml.FormatYAML: {"mock-0": "", "mock-1": models.MockSkipUnknownKind, "mock-2": "", "mock-3": models.MockSkipNoKind},
		yaml.FormatJSON: {"mock-0": "", "mock-1": models.MockSkipYAMLOnlyKind, "mock-2": models.MockSkipYAMLOnlyKind, "mock-3": models.MockSkipNoKind, "mock-9": models.MockSkipCutShort},
	}
	for format, body := range cases {
		t.Run(string(format), func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", "")
			dir := t.TempDir()
			writeSetFile(t, dir, format, body)
			served, listed := readSetOracle(t, dir, format)
			docs := readDocs(t, dir, format)
			var dup []models.MockSkip
			for _, d := range docs {
				if d.Err != nil {
					t.Fatalf("%s: %v", d.Name, d.Err)
				}
				if d.Name == "mock-3" {
					dup = append(dup, d.Skip)
				}
				if served[d.Name] != (len(d.Mocks) == 1 && d.Skip == "") {
					t.Fatalf("%s: the set serves it %v, the reader skips it %q", d.Name, served[d.Name], d.Skip)
				}
				if _, ok := listed[d.Name]; ok != (d.Skip != "") && d.Name != "mock-3" {
					t.Fatalf("%s: the set lists it skipped %v, the reader says %q", d.Name, ok, d.Skip)
				}
				if (d.Cause != nil) != (d.Name == "mock-9") {
					t.Fatalf("%s: cause %v", d.Name, d.Cause)
				}
			}
			if len(dup) != 2 || dup[0] != models.MockSkipUnknownKind || dup[1] != models.MockSkipNoKind {
				t.Fatalf("both documents named mock-3 are returned, each with its own reason: %v", dup)
			}
			_, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", format).SkippedMocks(context.Background(), pruneSet)
			if len(skipped) != len(listed) {
				t.Fatalf("SkippedMocks %v, the set lists %v", skipped, listed)
			}
			for n, w := range wantSkip[format] {
				if w == "" {
					if _, ok := skipped[n]; ok || !served[n] {
						t.Fatalf("%s: served %v, SkippedMocks %v", n, served[n], skipped[n])
					}
					continue
				}
				if skipped[n].Why != w || skipped[n].Kind != listed[n] {
					t.Fatalf("%s: SkippedMocks %+v, want %q of the kind the set lists, %q", n, skipped[n], w, listed[n])
				}
			}
			if format == yaml.FormatJSON && !strings.Contains(skipped["mock-9"].Cause, "cannot unmarshal array") {
				t.Fatalf("a named NDJSON line cut short says why its spec does not decode: %+v", skipped["mock-9"])
			}
		})
	}
}

// A set recorded as mocks.gob is replayed from it, whatever YAML or NDJSON
// file sits beside it (one left after a change of format): the reader
// refuses to read that other file, and SkippedMocks names the gob file and
// the connection failure the gob read skips.
func TestAGobSetIsReadAsTheReplayReadsIt(t *testing.T) {
	newer := cfMock(0, models.ConnFailurePhaseRequest, "reset")
	newer.Name = "cf-newer"
	ok := cfMock(1, models.ConnFailurePhaseConnect, models.ConnFailureRefused)
	ok.Name = "cf-ok"
	h := pruneHTTPMock(1)
	h.Name = "mock-1"
	dir := t.TempDir()
	writeSetFile(t, dir, yaml.FormatYAML, httpDocYAML("stale", "HTTP", "a"))
	var buf bytes.Buffer
	buf.WriteString(gobMockMagic)
	enc := gob.NewEncoder(&buf)
	for _, m := range []*models.Mock{h, newer, ok} {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	gobPath := filepath.Join(dir, pruneSet, "mocks.gob")
	if err := os.WriteFile(gobPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMockFileReader(context.Background(), nil, filepath.Join(dir, pruneSet), "mocks", yaml.FormatYAML); !errors.Is(err, ErrGobMockFile) || !strings.Contains(err.Error(), gobPath) {
		t.Fatalf("the reader must not read the YAML file a replay of a gob set never reads: %v", err)
	}
	served, listed := readSetOracle(t, dir, yaml.FormatYAML)
	if !served["mock-1"] || !served["cf-ok"] || served["stale"] || served["cf-newer"] {
		t.Fatalf("the set's read: %v", served)
	}
	file, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML).SkippedMocks(context.Background(), pruneSet)
	if file != gobPath || len(skipped) != 1 || skipped["cf-newer"].Why != models.MockSkipConnFailure || skipped["cf-newer"].Kind != listed["cf-newer"] {
		t.Fatalf("SkippedMocks: %s %+v (the set lists %v)", file, skipped, listed)
	}
}

// When the file the replay reads cannot be opened, SkippedMocks names that
// file, not another one beside it, and says nothing of what it skips.
func TestSkippedMocksNamesTheFileItCannotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	dir := t.TempDir()
	writeSetFile(t, dir, yaml.FormatYAML, httpDocYAML("stale", "HTTP", "a"))
	writeSetFile(t, dir, yaml.FormatJSON, httpDocJSON("mock-0", "HTTP", "a")+"\n")
	jsonPath := filepath.Join(dir, pruneSet, "mocks.json")
	if err := os.Chmod(jsonPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(jsonPath, 0o644) })
	file, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatJSON).SkippedMocks(context.Background(), pruneSet)
	if file != jsonPath || skipped != nil {
		t.Fatalf("SkippedMocks: %s %v, want %s and nothing", file, skipped, jsonPath)
	}
}
