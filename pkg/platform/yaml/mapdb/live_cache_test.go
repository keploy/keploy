package mapdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// A recording's flush of its mappings file holds little more than the file:
// on a long recording each flush parsed a file of tens of MB into a tree, and
// encoded it whole, each holding tens of times the file's size (2.5 GB at
// 35 MB), and took the process past its memory limit.

func liveBatch(from, tests, perTest int) map[string][]models.MockEntry {
	b := map[string][]models.MockEntry{}
	for t := from; t < from+tests; t++ {
		name := fmt.Sprintf("test-%d", t)
		for m := 0; m < perTest; m++ {
			b[name] = append(b[name], models.MockEntry{Name: fmt.Sprintf("mock-%d-%d", t, m), Kind: "Generic",
				ReqTimestampMock: "2026-11-19T10:00:00.000000001Z", ResTimestampMock: "2026-11-19T10:00:00.000000002Z"})
		}
	}
	return b
}

// peakHeapOf is the most heap fn's run held above what was live before it,
// sampled every millisecond, with the collector at its default pace.
func peakHeapOf(fn func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(100))
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	base := sample[0].Value.Uint64()
	var peak atomic.Uint64
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
			metrics.Read(s)
			if v := s[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	fn()
	close(stop)
	<-stopped
	if p := peak.Load(); p > base {
		return p - base
	}
	return 0
}

func TestAFlushOfALargeMappingsFileHoldsLittleMoreThanTheFile(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()
	// A file of 2,000 tests of 20 entries; then one flush of 32 more tests,
	// as a recording makes every couple of seconds.
	for from := 0; from < 2000; from += 500 {
		if err := db.UpsertBatch(ctx, "set-0", liveBatch(from, 500, 20)); err != nil {
			t.Fatalf("build the file: %v", err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "set-0", "mappings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var flushErr error
	peak := peakHeapOf(func() { flushErr = db.UpsertBatch(ctx, "set-0", liveBatch(2000, 32, 20)) })
	if flushErr != nil {
		t.Fatalf("flush: %v", flushErr)
	}
	// The file being made, and a few entries' encoding at a time; a parse
	// tree of the file, or its encoding at once, is tens of times its size.
	t.Logf("one flush onto a %d-byte mappings file held %d bytes of heap (%.1fx)", info.Size(), peak, float64(peak)/float64(info.Size()))
	if limit := uint64(info.Size()) * 15; peak > limit {
		t.Fatalf("one flush onto a %d-byte mappings file held %d bytes of heap (%.1fx the file): it parsed or encoded the whole file at once",
			info.Size(), peak, float64(peak)/float64(info.Size()))
	}
	got, _, err := db.Get(ctx, "set-0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2032 || len(got["test-2031"]) != 20 || len(got["test-0"]) != 20 {
		t.Fatalf("after the flush the file maps %d tests (test-0: %d, test-2031: %d entries), want 2032 of 20", len(got), len(got["test-0"]), len(got["test-2031"]))
	}
}

func TestAFlushAfterAHandEditKeepsTheEdit(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := db.UpsertBatch(ctx, "set-0", liveBatch(i*10, 10, 2)); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
	}
	path := filepath.Join(dir, "set-0", "mappings.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A person notes something on the file while the recording goes on.
	edited := strings.Replace(string(data), "  - id: test-5\n", "  - id: test-5 # flaky upstream, see ticket\n", 1)
	if edited == string(data) {
		t.Fatalf("the file has no test-5 item to edit:\n%s", data)
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertBatch(ctx, "set-0", liveBatch(30, 5, 2)); err != nil {
		t.Fatalf("flush after the edit: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "# flaky upstream, see ticket") {
		t.Fatalf("a flush after a hand edit dropped the edit:\n%s", after)
	}
	if !strings.Contains(string(after), "test-34") {
		t.Fatalf("the flush after the edit did not write its batch:\n%s", after)
	}
}

// The mappings file made a few test entries at a time is, byte for byte, the
// file encoded whole: values that YAML writes as a block of text (empty lines
// in it too), quoted, long, or not ASCII, and the golden files of this
// package.
func TestAMappingEncodedInChunksIsTheMappingEncodedWhole(t *testing.T) {
	values := []string{
		"plain", "", "line one\nline two\n", "a\n\nb with an empty line\n\n", "  leading spaces\n\ttab",
		strings.Repeat("long value ", 40), "quote's \"double\" and: colon", "- dash", "# hash", "ünïcödé ✓",
		"trailing space \n", "null", "123", "true", "*alias", "&anchor",
	}
	m := &models.Mapping{Version: string(models.V1Beta1), Kind: models.MappingKind, TestSetID: "set-0",
		Startup: []models.MockEntry{{Name: "mock-boot", Kind: "Generic"}}}
	for i := 0; i < 3*mappingChunk+7; i++ {
		v := values[i%len(values)]
		m.TestCases = append(m.TestCases, models.MappedTestCase{
			ID:    fmt.Sprintf("test-%d", i),
			Mocks: []models.MockEntry{{Name: fmt.Sprintf("mock-%d", i), Kind: "Generic", ReqTimestampMock: "2026-11-19T10:00:00Z"}},
			Real:  []string{v},
			Given: []models.GivenValue{{Name: "g", Value: v}},
			Key:   v,
			Cases: []string{fmt.Sprintf("case-%d", i)},
		})
	}
	check := func(name string, m *models.Mapping) {
		t.Helper()
		whole, err := yaml.MarshalGeneric(yaml.FormatYAML, m)
		if err != nil {
			t.Fatalf("%s: encode whole: %v", name, err)
		}
		chunked, err := marshalMappingYAML(m)
		if err != nil {
			t.Fatalf("%s: encode in chunks: %v", name, err)
		}
		if !bytes.Equal(whole, chunked) {
			at := 0
			for at < len(whole) && at < len(chunked) && whole[at] == chunked[at] {
				at++
			}
			from := max(at-200, 0)
			t.Fatalf("%s: the chunked encoding differs from the whole one at byte %d:\nwhole:   %q\nchunked: %q", name,
				at, whole[from:min(at+200, len(whole))], chunked[from:min(at+200, len(chunked))])
		}
	}
	check("generated", m)
	for _, f := range []string{"mappings.yaml", "moved.yaml"} {
		data, err := os.ReadFile(filepath.Join("testdata", "switchoff_golden", f))
		if err != nil {
			t.Fatal(err)
		}
		var g models.Mapping
		if err := yaml.UnmarshalGeneric(yaml.FormatYAML, data, &g); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		// Past one chunk, as a recording's file is.
		for len(g.TestCases) > 0 && len(g.TestCases) <= mappingChunk {
			g.TestCases = append(g.TestCases, g.TestCases...)
		}
		check(f, &g)
	}
}

// A rewrite whose change says it changed nothing writes nothing, even when
// its change touched the mapping it was handed: the next flush writes what the
// file holds, never what that change left in the mapping kept for it.
func TestARewriteThatChangesNothingLeavesTheNextFlushToTheFile(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := db.UpsertBatch(ctx, "set-0", liveBatch(i*10, 10, 2)); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
	}
	if err := db.rewrite(ctx, "set-0", false, true, func(m *models.Mapping) bool {
		m.TestCases[0].ID = "touched-and-not-written"
		return false
	}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := db.UpsertBatch(ctx, "set-0", liveBatch(20, 2, 2)); err != nil {
		t.Fatalf("flush after the rewrite: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "set-0", "mappings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "touched-and-not-written") {
		t.Fatal("a rewrite that wrote nothing had its change written by the next flush")
	}
	if !strings.Contains(string(data), "test-21") || !strings.Contains(string(data), "test-0") {
		t.Fatalf("the flush after the rewrite lost tests:\n%s", data)
	}
}

// manyMocks is a test entry of n mock entries, each with values from values in
// turn (YAML writes some as a block of text, some quoted, long or not ASCII),
// and every other field a test entry has.
func manyMocks(id string, n int, values []string) models.MappedTestCase {
	tc := models.MappedTestCase{ID: id, Cases: []string{"case-a", "case-b"}, CaseMocks: map[string][]string{"case-a": {"mock-0"}},
		CaseSteps: map[string]string{"case-a": "step"}, Dir: "dir", Starts: 2,
		At: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 19, 10, 0, 1, 0, time.UTC),
		Real: []string{"real-host"}, Given: []models.GivenValue{{Name: "g", Value: "v"}}, Key: "claim", Seed: "seed"}
	for i := 0; i < n; i++ {
		v := values[i%len(values)]
		tc.Mocks = append(tc.Mocks, models.MockEntry{Name: fmt.Sprintf("mock-%d", i), Kind: "Redis",
			ReqTimestampMock: "2026-11-19T10:00:00.000000001Z", ResTimestampMock: "2026-11-19T10:00:00.000000002Z", Reason: v, Place: v})
	}
	return tc
}

// entryValues are values YAML writes as a block of text, quoted, long or not
// ASCII, for the fields of mock entries.
var entryValues = []string{
	"", "plain", "line one\nline two\n", "a\n\nb with an empty line\n\n", "  leading spaces\n\ttab",
	strings.Repeat("long value ", 40), "quote's \"double\" and: colon", "- dash", "# hash", "ünïcödé ✓",
	"trailing space \n", "null", "123", "true", "*alias", "&anchor", "        - indented dash", "mock_entries:",
}

// A test of more mock entries than a chunk holds is encoded a few mock
// entries at a time, and the encoding is the test encoded whole, byte for
// byte: first in its file, among others, alone, with no field after its mock
// entries, and its first mock entry a block of text.
func TestATestOfManyMocksEncodedAFewAtATimeIsTheTestEncodedWhole(t *testing.T) {
	base := func() *models.Mapping {
		return &models.Mapping{Version: string(models.V1Beta1), Kind: models.MappingKind, TestSetID: "set-0",
			Startup: []models.MockEntry{{Name: "mock-boot", Kind: "Generic"}}}
	}
	cases := map[string]*models.Mapping{}
	m := base()
	m.TestCases = append(m.TestCases, manyMocks("", 3*chunkMocks+17, entryValues))
	for i := 0; i < 2*mappingChunk; i++ {
		m.TestCases = append(m.TestCases, manyMocks(fmt.Sprintf("test-%d", i), 1+i%3, entryValues))
	}
	cases["first of many tests"] = m
	m = base()
	for i := 0; i < mappingChunk+3; i++ {
		m.TestCases = append(m.TestCases, manyMocks(fmt.Sprintf("test-%d", i), 2, entryValues))
	}
	m.TestCases = append(m.TestCases[:20], append([]models.MappedTestCase{manyMocks("in the middle", chunkMocks+1, entryValues)}, m.TestCases[20:]...)...)
	cases["among others"] = m
	m = base()
	m.TestCases = []models.MappedTestCase{manyMocks("alone", 2*chunkMocks, entryValues)}
	cases["alone"] = m
	m = base()
	bare := manyMocks("no field after its mocks", chunkMocks+5, entryValues)
	bare.Cases, bare.CaseMocks, bare.CaseSteps, bare.Dir, bare.Starts = nil, nil, nil, "", 0
	bare.At, bare.End, bare.Real, bare.Given, bare.Key, bare.Seed = time.Time{}, time.Time{}, nil, nil, "", ""
	m.TestCases = []models.MappedTestCase{manyMocks("t", 3, entryValues), bare}
	cases["no field after its mocks, last"] = m
	m = base()
	m.TestCases = []models.MappedTestCase{manyMocks("block first", chunkMocks+5, entryValues[2:])}
	cases["its first mock entry a block of text"] = m
	for name, m := range cases {
		whole, err := yaml.MarshalGeneric(yaml.FormatYAML, m)
		if err != nil {
			t.Fatalf("%s: encode whole: %v", name, err)
		}
		chunked, err := marshalMappingYAML(m)
		if err != nil {
			t.Fatalf("%s: encode in chunks: %v", name, err)
		}
		if !bytes.Equal(whole, chunked) {
			at := 0
			for at < len(whole) && at < len(chunked) && whole[at] == chunked[at] {
				at++
			}
			from := max(at-200, 0)
			t.Fatalf("%s: the chunked encoding differs from the whole encoding at byte %d:\nwhole: %q\ngot:   %q", name,
				at, whole[from:min(at+200, len(whole))], chunked[from:min(at+200, len(chunked))])
		}
		if !encodesTo(m, whole) {
			t.Fatalf("%s: the whole encoding is not found to be what the mapping encodes to", name)
		}
	}
}
