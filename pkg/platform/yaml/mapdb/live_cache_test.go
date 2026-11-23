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
	return peakHeapAt(100, fn)
}

// liveHeap is the heap live now: right after a collection.
func liveHeap() uint64 {
	runtime.GC()
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// peakLiveOf is about the most heap fn's run held live above what was live
// before it: the most the collector found live (/gc/heap/live, what each
// collection marked) in collections run at 1% (GOGC=1), so that one runs
// about as soon as the heap passes what is live. What fn left for the
// collector is not counted, however late the collector's default pace would
// have collected it; what fn allocated while a collection marked counts as
// live for that one.
func peakLiveOf(fn func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	runtime.GC()
	read := func() uint64 {
		s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
		metrics.Read(s)
		return s[0].Value.Uint64()
	}
	base := read()
	var peak atomic.Uint64
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			if v := read(); v > peak.Load() {
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
	runtime.GC()
	close(stop)
	<-stopped
	if p := peak.Load(); p > base {
		return p - base
	}
	return 0
}

// allocsOf is how many bytes fn's run allocated on the heap: exact, whatever
// the collector's pace or how busy the machine is.
func allocsOf(fn func()) uint64 {
	read := func() uint64 {
		s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
		metrics.Read(s)
		return s[0].Value.Uint64()
	}
	before := read()
	fn()
	return read() - before
}

// peakHeapAt is peakHeapOf with the collector at percent (GOGC).
func peakHeapAt(percent int, fn func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(percent))
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
// entries, its first mock entry a block of text, and written to the file as
// it is made (encoded.writeTo).
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
		var streamed bytes.Buffer
		if err := (encoded{head: []byte("# head\n"), mapping: m}).writeTo(&streamed); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		if err := (encoded{head: []byte("# head\n"), mapping: m}).writeTo(&failingWriter{after: 4096}); err == nil {
			t.Fatalf("%s: a write to a file that failed part way returned no error", name)
		}
		for what, got := range map[string][]byte{"the chunked encoding": chunked, "the written file": streamed.Bytes()[len("# head\n"):]} {
			if !bytes.Equal(whole, got) {
				at := 0
				for at < len(whole) && at < len(got) && whole[at] == got[at] {
					at++
				}
				from := max(at-200, 0)
				t.Fatalf("%s: %s differs from the whole encoding at byte %d:\nwhole: %q\ngot:   %q", name, what,
					at, whole[from:min(at+200, len(whole))], got[from:min(at+200, len(got))])
			}
		}
		if !encodesTo(m, whole) {
			t.Fatalf("%s: the whole encoding is not found to be what the mapping encodes to", name)
		}
	}
}

// A recording's flush onto a test of many mock entries holds about what the
// flush adds, however many the test holds: one test of a long recording (every
// call keploy could not place in a test, filed under one entry) held 279,000,
// and a flush encoded it whole, holding 392 MB, and the file whole besides.
// What a flush holds live is measured (peakLiveOf), on two sizes kept live
// together, so both run on one live heap; a test encoded whole holds about
// forty times its file at once (88 and 340 MB here), a few entries at a time a
// few MB, and the bound leaves room for what a busy machine adds. What a flush
// allocates is exact: no more than its encoding, a few entries at a time,
// does, and so never a buffer of the file it writes.
func TestAFlushOntoATestOfManyMocksHoldsAboutWhatTheFlushAdds(t *testing.T) {
	type fixture struct {
		db   *MappingDb
		dir  string
		add  []models.MockEntry
		size int64
	}
	ctx := context.Background()
	sizes := []int{10000, 40000}
	fx := map[int]*fixture{}
	for _, n := range sizes {
		f := &fixture{dir: t.TempDir()}
		f.db = New(zap.NewNop(), f.dir, "mappings")
		all := manyMocks("", n+64, []string{"plain"}).Mocks
		if err := f.db.UpsertBatch(ctx, "set-0", map[string][]models.MockEntry{"": all[:n:n]}); err != nil {
			t.Fatalf("build the file: %v", err)
		}
		f.add = all[n:]
		// Room in the test's list for what the flushes add, as a list grown
		// by appends has most of the time: the flush, not the list's growth,
		// is measured.
		if lm := f.db.live.p.Load(); lm != nil {
			for i := range lm.mapping.TestCases {
				if ms := lm.mapping.TestCases[i].Mocks; len(ms) == n {
					lm.mapping.TestCases[i].Mocks = append(make([]models.MockEntry, 0, n+1024), ms...)
				}
			}
		}
		info, err := os.Stat(filepath.Join(f.dir, "set-0", "mappings.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		f.size = info.Size()
		fx[n] = f
	}
	flush := func(f *fixture, round int) {
		if err := f.db.UpsertBatch(ctx, "set-0", map[string][]models.MockEntry{"": f.add[round*21 : round*21+21], "test-new": f.add[:3]}); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	// The least of two flushes each: what a flush holds, not a moment the
	// machine was busy.
	held := map[int]uint64{}
	for round := 0; round < 2; round++ {
		for _, n := range sizes {
			f := fx[n]
			h := peakLiveOf(func() { flush(f, round) })
			if round == 0 || h < held[n] {
				held[n] = h
			}
		}
	}
	f := fx[40000]
	lm := f.db.live.p.Load()
	if lm == nil {
		t.Fatal("the flushes kept no mapping")
	}
	encoding := allocsOf(func() {
		if err := encodeMappingYAML(lm.mapping, func([]byte) bool { return true }); err != nil {
			t.Fatal(err)
		}
	})
	flushing := allocsOf(func() { flush(f, 2) })
	for _, n := range sizes {
		t.Logf("a flush onto a test of %d mock entries (a %d-byte file) held %d bytes of heap", n, fx[n].size, held[n])
		got, _, err := fx[n].db.Get(ctx, "set-0")
		want := n + 42
		if n == 40000 {
			want = n + 63
		}
		if err != nil || len(got[""]) != want || len(got["test-new"]) != 3 {
			t.Fatalf("after the flushes the file maps %d mocks to the test and %d to the new one, err %v; want %d and 3", len(got[""]), len(got["test-new"]), err, want)
		}
	}
	t.Logf("a flush onto a test of 40000 mock entries allocated %d bytes; encoding its mapping a few entries at a time, %d", flushing, encoding)
	for _, n := range sizes {
		if held[n] > 48<<20 {
			t.Errorf("a flush onto a test of %d mock entries (a %d-byte file) held %d bytes of heap: it encoded the test whole", n, fx[n].size, held[n])
		}
	}
	if flushing > encoding+(2<<20) {
		t.Errorf("a flush onto a test of 40,000 mock entries (a %d-byte file) allocated %d bytes, %d more than encoding its mapping a few entries at a time: it built the file, or the test, whole",
			f.size, flushing, flushing-encoding)
	}
}

// A write made as it is written (encoded.writeTo) of a file that holds no
// hand edit keeps the mapping it wrote, as a write made in one buffer did: a
// new file, and one read and rewritten (another db wrote it last), so the
// next flush reads no file.
func TestAFileWrittenAsItIsMadeKeepsItsMappingForTheNextFlush(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db := New(zap.NewNop(), dir, "mappings")
	if err := db.UpsertBatch(ctx, "set-0", liveBatch(0, 3, 2)); err != nil {
		t.Fatal(err)
	}
	if db.live.p.Load() == nil {
		t.Fatal("the write of a new file kept no mapping: the next flush reads the file again")
	}
	other := New(zap.NewNop(), dir, "mappings")
	if err := other.UpsertBatch(ctx, "set-0", liveBatch(3, 3, 2)); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertBatch(ctx, "set-0", liveBatch(6, 3, 2)); err != nil {
		t.Fatal(err)
	}
	lm := db.live.p.Load()
	if lm == nil {
		t.Fatal("the rewrite of a file another db wrote kept no mapping: the next flush reads the file again")
	}
	if len(lm.mapping.TestCases) != 9 {
		t.Fatalf("the kept mapping holds %d tests, want 9", len(lm.mapping.TestCases))
	}
	data, err := os.ReadFile(filepath.Join(dir, "set-0", "mappings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, lm.head) || !encodesTo(lm.mapping, data[len(lm.head):]) {
		t.Fatal("the kept mapping is not what the file holds")
	}
	// A note a person put on top of the file is kept by the write that
	// reads it, and by the flushes after it, which read no file.
	path := filepath.Join(dir, "set-0", "mappings.yaml")
	if err := os.WriteFile(path, append([]byte("# a note\n"), data...), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := db.UpsertBatch(ctx, "set-0", liveBatch(9+3*i, 3, 2)); err != nil {
			t.Fatal(err)
		}
		if db.live.p.Load() == nil {
			t.Fatalf("flush %d after the note kept no mapping", i)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(data, []byte("# a note\n")) {
			t.Fatalf("flush %d after the note dropped it:\n%.200s", i, data)
		}
	}
}

// failingWriter is a file whose writes fail once it holds after bytes.
type failingWriter struct{ after, n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n+len(p) > w.after {
		return 0, os.ErrClosed
	}
	w.n += len(p)
	return len(p), nil
}

// A flush onto a test the file maps already adds the mock entries it does not
// hold, in their order, each once: the agent sends a test's mocks in more than
// one go, and a later go is a delta, never a replacement.
func TestAFlushOntoAMappedTestAddsOnlyTheMocksItDoesNotHold(t *testing.T) {
	e := func(names ...string) []models.MockEntry {
		var out []models.MockEntry
		for _, n := range names {
			out = append(out, models.MockEntry{Name: n, Kind: "Redis"})
		}
		return out
	}
	names := func(es []models.MockEntry) string {
		var out []string
		for _, x := range es {
			out = append(out, x.Name)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct{ existing, incoming, want []models.MockEntry }{
		{e("a", "b"), e("b", "c", "c", "d"), e("a", "b", "c", "d")},
		{nil, e("x", "y"), e("x", "y")},
		{e("a"), e("a"), e("a")},
		{e("a", "b"), e("d", "a", "c"), e("a", "b", "d", "c")},
	} {
		if got := mergeMockEntries(c.existing, c.incoming); names(got) != names(c.want) {
			t.Errorf("merging %s into %s gave %s, want %s", names(c.incoming), names(c.existing), names(got), names(c.want))
		}
	}
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()
	for _, batch := range [][]models.MockEntry{e("a", "b"), e("b", "c"), e("c", "d", "a")} {
		if err := db.UpsertBatch(ctx, "set-0", map[string][]models.MockEntry{"test-1": batch}); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := New(zap.NewNop(), dir, "mappings").Get(ctx, "set-0")
	if err != nil || names(got["test-1"]) != "a,b,c,d" {
		t.Fatalf("after three flushes the file maps test-1 to %s (err %v), want a,b,c,d", names(got["test-1"]), err)
	}
}

// A cancelled context passed to UpsertBatch must return ctx.Err() and clear
// db.live rather than caching an unwritten mapping when replaceRecordingFile
// leaves the old file on disk.
func TestUpsertBatchCancelledContextDoesNotPoisonLiveCache(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()
	if err := db.UpsertBatch(ctx, "set-0", map[string][]models.MockEntry{
		"test-1": {{Name: "mock-0", Kind: "Redis"}},
	}); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.UpsertBatch(cancelled, "set-0", map[string][]models.MockEntry{
		"test-2": {{Name: "mock-1", Kind: "Redis"}},
	}); err == nil {
		t.Fatal("UpsertBatch with cancelled context returned nil; want ctx.Err()")
	}
	if db.live.p.Load() != nil {
		t.Fatal("UpsertBatch with cancelled context kept an unwritten mapping in db.live")
	}
	// A subsequent flush must read what is actually on disk (only test-1) and
	// must NOT resurrect test-2 from the cache.
	if err := db.UpsertBatch(ctx, "set-0", map[string][]models.MockEntry{
		"test-3": {{Name: "mock-2", Kind: "Redis"}},
	}); err != nil {
		t.Fatalf("subsequent upsert: %v", err)
	}
	got, _, err := New(zap.NewNop(), dir, "mappings").Get(ctx, "set-0")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if _, leaked := got["test-2"]; leaked {
		t.Fatalf("cancelled write leaked test-2 into mappings.yaml: %+v", got)
	}
	if len(got["test-1"]) != 1 || len(got["test-3"]) != 1 {
		t.Fatalf("mappings = %+v; want test-1 and test-3", got)
	}
}
