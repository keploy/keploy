package mockdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// DeleteMocks takes the named mocks out of the set's mock file in the format
// it is stored in, keeps every other mock, keeps a named one a kept mock
// requires, returns only what it took out, and leaves a file it takes nothing
// out of unwritten.
func TestDeleteMocksTakesOutOnlyWhatNoKeptMockRequires(t *testing.T) {
	prev := models.RegisterMockDeps(models.HTTP, depsByMetadata)
	t.Cleanup(func() { models.RegisterMockDeps(models.HTTP, prev) })
	fixture := []*models.Mock{
		linkedHTTPMock(0, "lease", ""), // mock-0: named, but mock-1 requires it
		linkedHTTPMock(1, "", "lease"), // mock-1: kept, requires lease
		linkedHTTPMock(2, "", ""),      // mock-2: named
		linkedHTTPMock(3, "", ""),      // mock-3: kept
		linkedHTTPMock(4, "", ""),      // mock-4: named
	}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			path := writePruneFixture(t, dir, format, fixture, "")
			ys := New(zap.NewNop(), dir, "mocks")
			ctx := context.Background()
			got, err := ys.DeleteMocks(ctx, pruneSet, []string{"mock-4", "mock-0", "mock-2", "mock-9", "mock-2"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, []string{"mock-2", "mock-4"}) {
				t.Fatalf("took out %v, want [mock-2 mock-4]", got)
			}
			if names := depsNamesOf(readDepsSet(t, dir)); !reflect.DeepEqual(names, []string{"mock-0", "mock-1", "mock-3"}) {
				t.Fatalf("the file holds %v, want [mock-0 mock-1 mock-3]", names)
			}
			if filepath.Ext(path) != "."+format {
				t.Fatalf("fixture at %s, want a .%s file", path, format)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// Nothing named is in the file (mock-0 is still required): it is
			// not written.
			got, err = ys.DeleteMocks(ctx, pruneSet, []string{"mock-2", "mock-0"})
			if err != nil || got != nil {
				t.Fatalf("again: took out %v, err %v; want nothing", got, err)
			}
			if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
				t.Fatalf("taking nothing out rewrote the file (err %v)", err)
			}
		})
	}
	// A set with no mock file has nothing to take out.
	ys := New(zap.NewNop(), t.TempDir(), "mocks")
	if got, err := ys.DeleteMocks(context.Background(), pruneSet, []string{"mock-0"}); err != nil || got != nil {
		t.Fatalf("no mock file: %v, %v; want nothing, no error", got, err)
	}
}

// gibRowsEnv set to 1 runs the heap rows below on mock files past a GiB (on a
// test box: three such files, one at a time, and the copy each prune
// writes); without it they run on files of about 20 MB. The budget is the
// same either way: what DeleteMocks holds does not grow with the file.
const gibRowsEnv = "KEPLOY_GIB_ROWS"

// writeMocksUntil records pruneMySQLMock(i, 40) mocks into <dir>/set-0 in the
// given format, one at a time through InsertMock, until the mock file holds at
// least size bytes. It returns the file and how many mocks it holds; no more
// than one mock is in memory at a time. How many it takes is read from a
// first 256 written to another directory: the gob writer is asynchronous, so
// the file's size is known only once its writer is closed.
func writeMocksUntil(t *testing.T, dir, format string, size int64) (string, int) {
	t.Helper()
	write := func(dir string, n int) string {
		t.Setenv("KEPLOY_MOCK_FORMAT", "")
		f := yaml.FormatYAML
		switch format {
		case "json":
			f = yaml.FormatJSON
		case "gob":
			t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
		}
		ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
		for i := range n {
			if err := ys.InsertMock(context.Background(), pruneMySQLMock(i, 40), pruneSet); err != nil {
				t.Fatalf("InsertMock %d: %v", i, err)
			}
		}
		if err := ys.Close(); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KEPLOY_MOCK_FORMAT", "")
		return filepath.Join(dir, pruneSet, "mocks."+format)
	}
	const probe = 256
	fi, err := os.Stat(write(t.TempDir(), probe))
	if err != nil {
		t.Fatal(err)
	}
	n := int(size/(fi.Size()/probe)) + probe
	path := write(dir, n)
	if fi, err = os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Size() < size {
		t.Fatalf("%d mocks wrote %d bytes, under the %d the row needs", n, fi.Size(), size)
	}
	return path, n
}

// DeleteMocks streams the mock file as UpdateMocks does (keploy's rule:
// memory must not grow with a recording's size): taking every tenth mock out
// of a file of any size holds, at every point it checks its context, under
// pruneHeapBudget above its caller's heap, in each format. With gibRowsEnv
// set the files are past a GiB. Mutation: decode every mock of the file
// before writing any back.
func TestDeleteMocks_HeapDoesNotScaleWithTheMockFile(t *testing.T) {
	size := int64(20 << 20)
	if os.Getenv(gibRowsEnv) == "1" {
		size = 1 << 30
	}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			path, n := writeMocksUntil(t, dir, format, size)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for i := 0; i < n; i += 10 {
				names = append(names, fmt.Sprintf("mock-%d", i))
			}
			// A gob prune checks its context every ctxCheckEvery mocks, a
			// yaml or json prune on every line.
			every := max(1, fileLines(t, path)/256)
			if format == "gob" {
				every = 1
			}
			f := yaml.FormatYAML
			if format == "json" {
				f = yaml.FormatJSON
			}
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			var dropped []string
			peak, samples, err := heapPeakEvery(every, func(ctx context.Context) error {
				var err error
				dropped, err = ys.DeleteMocks(ctx, pruneSet, names)
				return err
			})
			if err != nil {
				t.Fatalf("DeleteMocks: %v", err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d mocks, %d-byte file (%d after), took out %d, held at most %.2f MiB over %d samples (budget %.0f MiB)",
				format, n, before.Size(), after.Size(), len(dropped), float64(peak)/(1<<20), samples, float64(pruneHeapBudget)/(1<<20))
			if len(dropped) != len(names) || after.Size() >= before.Size() {
				t.Fatalf("took out %d of the %d named mocks (file %d -> %d bytes); the row measures nothing", len(dropped), len(names), before.Size(), after.Size())
			}
			// Fewer samples than the prune's checks of its context measure
			// nothing: every line of yaml or json, every ctxCheckEvery-th
			// gob mock.
			want := 4
			if format == "gob" {
				want = n / ctxCheckEvery
			}
			if samples < want {
				t.Fatalf("DeleteMocks was sampled %d times, under %d; it no longer checks its context where this test measures it", samples, want)
			}
			if peak > pruneHeapBudget {
				t.Fatalf("taking mocks out of a %d-byte mock file held %d bytes of live heap; the budget is %d whatever the file's size", before.Size(), peak, pruneHeapBudget)
			}
		})
	}
}

// DeleteMocks copies every mock it keeps as it was read: a mock saved by hand
// (its own indent, key order, quoting and comments, a field this keploy does
// not know) stays byte-identical, in yaml and in json, so what a recording's
// hash reads for the mocks that stay does not change when another is taken
// out. Encoded again, neither hand-written mock would read the same. Both
// passes copy: taking out mock-b alone is one pass; asking for mock-a too,
// which the kept mock-c requires, is a second pass that keeps it.
func TestDeleteMocksLeavesAHandWrittenMockByteIdentical(t *testing.T) {
	prev := models.RegisterMockDeps(models.HTTP, depsByMetadata)
	t.Cleanup(func() { models.RegisterMockDeps(models.HTTP, prev) })
	handA := `kind: Http
# written by hand: key order, indent and quoting are its own
version: api.keploy.io/v1beta1
name: mock-a
spec:
  futureField: kept as written
  metadata: {name: Http, note: "hand-written", provides: lease}
  req: {method: GET, url: 'http://svc/a', header: {Accept: "*/*"}, body: ""}
  resp:
    status_code: 200
    header: {}
    body: |
      {"a": 1}
`
	auto := "version: api.keploy.io/v1beta1\nkind: Http\nname: mock-b\nspec:\n    metadata: {}\n    req:\n        method: GET\n        url: http://svc/b\n        header: {}\n        body: \"\"\n    resp:\n        status_code: 204\n        header: {}\n        body: \"\"\n"
	handC := "kind: Http\nname: mock-c # trailing comment\nversion: api.keploy.io/v1beta1\nspec:\n  metadata: {requires: lease}\n  req: {method: POST, url: \"http://svc/c\", header: {}, body: \"x=1\"}\n  resp: {status_code: 201, header: {}, body: \"\"}\n"
	jsonA := `{"name":"mock-a", "kind":"Http","version":"api.keploy.io/v1beta1","spec":{"futureField":"kept as written","metadata":{"name":"Http","provides":"lease"},"req":{"method":"GET","url":"http://svc/a","header":{"Accept":"*/*"},"body":""},"resp":{"status_code":200,"header":{},"body":"{\"a\": 1}\n"}}}`
	jsonB := `{"version":"api.keploy.io/v1beta1","kind":"Http","name":"mock-b","spec":{"metadata":{},"req":{"method":"GET","url":"http://svc/b","header":{},"body":""},"resp":{"status_code":204,"header":{},"body":""}}}`
	jsonC := `{"kind":"Http", "name":"mock-c","version":"api.keploy.io/v1beta1","spec":{"metadata":{"requires":"lease"},"req":{"method":"POST","url":"http://svc/c","header":{},"body":"x=1"},"resp":{"status_code":201,"header":{},"body":""}}}`
	cases := []struct {
		format           yaml.Format
		file, want       string
		handWritten      []string
		handWrittenNames []string
	}{
		{
			format:           yaml.FormatYAML,
			file:             "# my own mocks\n" + handA + "---\n" + auto + "---\n" + handC,
			want:             utils.GetVersionAsComment() + handA + "---\n" + handC,
			handWritten:      []string{handA, handC},
			handWrittenNames: []string{"mock-a", "mock-c"},
		},
		{
			format:           yaml.FormatJSON,
			file:             jsonA + "\n" + jsonB + "\n" + jsonC + "\n",
			want:             jsonA + "\n" + jsonC + "\n",
			handWritten:      []string{jsonA, jsonC},
			handWrittenNames: []string{"mock-a", "mock-c"},
		},
	}
	for _, c := range cases {
		for _, names := range [][]string{{"mock-b"}, {"mock-a", "mock-b"}} {
			t.Run(fmt.Sprintf("%s/%v", c.format, names), func(t *testing.T) {
				t.Setenv("KEPLOY_MOCK_FORMAT", "")
				dir := t.TempDir()
				path := filepath.Join(dir, pruneSet, "mocks."+c.format.FileExtension())
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(c.file), 0o644); err != nil {
					t.Fatal(err)
				}
				ys := NewWithFormat(zap.NewNop(), dir, "mocks", c.format)
				// The row holds only if this keploy's encoder writes each
				// hand-written mock differently.
				byName := map[string]*models.Mock{}
				for _, m := range readSetMocks(t, ys) {
					byName[m.Name] = m
				}
				for i, name := range c.handWrittenNames {
					m := byName[name]
					if m == nil {
						t.Fatalf("the fixture's %s does not decode", name)
					}
					if enc := encodedAgain(t, m, c.format); enc == c.handWritten[i] {
						t.Fatalf("the fixture's %s is what the encoder writes; it must differ:\n%s", name, enc)
					}
				}
				got, err := ys.DeleteMocks(context.Background(), pruneSet, names)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, []string{"mock-b"}) {
					t.Fatalf("took out %v, want [mock-b]", got)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != c.want {
					t.Fatalf("the file after taking mock-b out:\n%s\nwant (the hand-written mocks as written):\n%s", after, c.want)
				}
				var left []string
				for _, m := range readSetMocks(t, ys) {
					left = append(left, m.Name)
				}
				if !reflect.DeepEqual(left, c.handWrittenNames) {
					t.Fatalf("the file reads as %v, want %v", left, c.handWrittenNames)
				}
			})
		}
	}
}

// readSetMocks is every mock of pruneSet's mock file, decoded as a rewrite
// reads it (nextDoc), by name.
func readSetMocks(t *testing.T, ys *MockYaml) []*models.Mock {
	t.Helper()
	reader, err := yaml.NewMockReaderF(context.Background(), zap.NewNop(), filepath.Join(ys.MockPath, pruneSet), "mocks", ys.Format)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var mocks []*models.Mock
	for {
		doc, readErr, decodeErr := nextDoc(reader, zap.NewNop(), false)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil || decodeErr != nil {
			t.Fatalf("read the mock file: %v, %v", readErr, decodeErr)
		}
		if doc.verbatim {
			t.Fatalf("the fixture's %q is not one keploy decodes", doc.name)
		}
		mocks = append(mocks, doc.mocks...)
	}
	sort.Slice(mocks, func(i, j int) bool { return mocks[i].Name < mocks[j].Name })
	return mocks
}

// encodedAgain is m as this keploy's rewrite encodes one document of format.
func encodedAgain(t *testing.T, m *models.Mock, format yaml.Format) string {
	t.Helper()
	if format == yaml.FormatJSON {
		doc, handled, err := EncodeMockJSON(m, zap.NewNop())
		if err != nil || !handled {
			t.Fatalf("encode %s as json: handled %v, %v", m.Name, handled, err)
		}
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	b, err := yamlLib.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
