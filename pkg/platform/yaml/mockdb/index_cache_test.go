package mockdb

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/cespare/xxhash/v2"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// withIndexCache turns the process's index cache on at budget for the test,
// emptied, and off again after it.
func withIndexCache(t *testing.T, budget int64) {
	t.Helper()
	SetIndexCacheBudget(0)
	SetIndexCacheBudget(budget)
	t.Cleanup(func() { SetIndexCacheBudget(0) })
}

// indexCacheDelta is what the cache did since before.
func indexCacheDelta(before IndexCacheStats) IndexCacheStats {
	now := MockIndexCacheStats()
	now.SameFileHits -= before.SameFileHits
	now.ContentHits -= before.ContentHits
	now.Misses -= before.Misses
	now.Evictions -= before.Evictions
	return now
}

// sameAsRead fails unless got is, pool by pool and mock by mock, what want
// is: the same mocks in the same order, described the same, and sent to the
// agent the same once read back from got's own file; the same skipped
// documents.
func sameAsRead(t *testing.T, got, want models.TestSetMocks) {
	t.Helper()
	ctx := context.Background()
	for _, pool := range []struct {
		name      string
		got, want []*models.Mock
	}{
		{"Filtered", got.Filtered, want.Filtered},
		{"Unfiltered", got.Unfiltered, want.Unfiltered},
		{"AllSession", got.AllSession, want.AllSession},
		{"AllPerTest", got.AllPerTest, want.AllPerTest},
	} {
		if g, w := poolString(pool.got), poolString(pool.want); g != w {
			t.Fatalf("%s is %s; a whole read gives %s", pool.name, g, w)
		}
		for i, m := range pool.got {
			w := pool.want[i]
			if !m.PayloadOnDisk() {
				t.Fatalf("%s: mock %s holds its payload", pool.name, m.Name)
			}
			if m.TestModeInfo != w.TestModeInfo || m.ConnectionID != w.ConnectionID || !reflect.DeepEqual(m.Spec.Metadata, w.Spec.Metadata) ||
				!m.Spec.ReqTimestampMock.Equal(w.Spec.ReqTimestampMock) || !m.Spec.ResTimestampMock.Equal(w.Spec.ResTimestampMock) || !reflect.DeepEqual(m.Spec.Async, w.Spec.Async) {
				t.Fatalf("%s: mock %s is\n%+v\na whole read gives\n%+v", pool.name, m.Name, m, w)
			}
			if s, ws := models.MockSummaryFromSpec(m), models.MockSummaryFromSpec(w); s != ws {
				t.Fatalf("%s: mock %s is summed up as %q; a whole read as %q", pool.name, m.Name, s, ws)
			}
			if s, ws := models.MockTargetFromSpec(m), models.MockTargetFromSpec(w); s != ws {
				t.Fatalf("%s: mock %s targets %q; a whole read %q", pool.name, m.Name, s, ws)
			}
			sent, err := m.WithPayload(ctx)
			if err != nil {
				t.Fatalf("%s: read back %s: %v", pool.name, m.Name, err)
			}
			wantSent, err := w.WithPayload(ctx)
			if err != nil {
				t.Fatalf("%s: read back %s from the whole read: %v", pool.name, w.Name, err)
			}
			if g, ws := asTheAgentGetsIt(t, sent), asTheAgentGetsIt(t, wantSent); !reflect.DeepEqual(g, ws) {
				t.Fatalf("%s: the agent gets mock %s as\n%+v\na whole read sends\n%+v", pool.name, m.Name, g, ws)
			}
		}
	}
	if !reflect.DeepEqual(got.Skipped, want.Skipped) || !reflect.DeepEqual(got.SkippedWindowed, want.SkippedWindowed) {
		t.Fatalf("skipped %v %v; a whole read skips %v %v", got.Skipped, got.SkippedWindowed, want.Skipped, want.SkippedWindowed)
	}
}

// A read answered from the index cache, of the same file or of a copy of it
// in another directory, is what a read of the whole file returns, for every
// window and mapping, in yaml and json; and the copy's payloads are read back
// from the copy.
func TestIndexCacheAnswersAsAWholeReadDoes(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			ys := writeTestSetMocks(t, t.TempDir(), format)
			// A document of a kind this keploy cannot read is skipped and
			// listed, and stays skipped when the index answers.
			appendDoc(t, ys, format, "Acme-Queue", "q-data")
			copyDir := t.TempDir()
			copyTestSet(t, ys, copyDir)
			copied := NewWithFormat(zap.NewNop(), copyDir, "mocks", ys.Format)
			for _, c := range testSetMocksCases() {
				t.Run(c.window+"/"+c.maps, func(t *testing.T) {
					SetIndexCacheBudget(0)
					whole, err := ys.IndexTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
					if err != nil {
						t.Fatal(err)
					}
					if len(whole.Skipped) == 0 {
						t.Fatal("precondition: the set lists no skipped document")
					}
					withIndexCache(t, 64<<20)
					before := MockIndexCacheStats()
					first, err := ys.IndexTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
					if err != nil {
						t.Fatal(err)
					}
					sameAsRead(t, first, whole)
					if d := indexCacheDelta(before); d.Misses != 1 || d.Files != 1 {
						t.Fatalf("the first read: %+v; want one miss, the index kept", d)
					}
					again, err := ys.IndexTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
					if err != nil {
						t.Fatal(err)
					}
					sameAsRead(t, again, whole)
					if d := indexCacheDelta(before); d.SameFileHits != 1 || d.Misses != 1 {
						t.Fatalf("the second read: %+v; want it answered from the same file", d)
					}
					fromCopy, err := copied.IndexTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
					if err != nil {
						t.Fatal(err)
					}
					if d := indexCacheDelta(before); d.ContentHits != 1 || d.Misses != 1 {
						t.Fatalf("the copy's read: %+v; want it answered from the same content", d)
					}
					sameAsRead(t, fromCopy, whole)
					// The copy is now a file the index was read from: its next
					// read is answered as the same file, without a pass over it.
					if _, err := copied.IndexTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed); err != nil {
						t.Fatal(err)
					}
					if d := indexCacheDelta(before); d.SameFileHits != 2 || d.ContentHits != 1 || d.Misses != 1 {
						t.Fatalf("the copy's second read: %+v; want it answered from the same file", d)
					}
					// The copy's payloads come from the copy: gone, they cannot
					// be read back, though the original is still there.
					if err := os.RemoveAll(filepath.Join(copyDir, "set-0")); err != nil {
						t.Fatal(err)
					}
					defer copyTestSet(t, ys, copyDir)
					for _, m := range fromCopy.Unfiltered {
						if _, err := m.WithPayload(ctx); err == nil {
							t.Fatalf("mock %s was read back with its copy gone", m.Name)
						}
					}
				})
			}
		})
	}
}

// mockFilePath is the set's mock file.
func mockFilePath(t *testing.T, ys *MockYaml) string {
	t.Helper()
	for _, ext := range []string{"yaml", "json"} {
		p := filepath.Join(ys.MockPath, "set-0", "mocks."+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatal("no mock file")
	return ""
}

func copyTestSet(t *testing.T, ys *MockYaml, dir string) {
	t.Helper()
	src := mockFilePath(t, ys)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "set-0", filepath.Base(src))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// appendDoc appends a per-test document of kind named name to the set's file.
func appendDoc(t *testing.T, ys *MockYaml, format, kind, name string) {
	t.Helper()
	doc := "---\nversion: api.keploy.io/v1beta1\nkind: " + kind + "\nname: " + name + "\nspec:\n    metadata:\n        type: mocks\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.02Z\n"
	if format == "json" {
		doc = `{"version":"api.keploy.io/v1beta1","kind":"` + kind + `","name":"` + name + `","spec":{"metadata":{"type":"mocks"},"reqTimestampMock":"2026-10-08T11:12:00.01Z","resTimestampMock":"2026-10-08T11:12:00.02Z"}}` + "\n"
	}
	f, err := os.OpenFile(mockFilePath(t, ys), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(doc); err != nil {
		t.Fatal(err)
	}
}

// A kept index answers only the content it was read from, by the rules it was
// read by: a file appended to, a file put in its place with other bytes of the
// same size, an edit at either end that kept the size and the modification
// time (on a file longer than both ends, each end in turn), and a
// registration since (a models hook, a YAML mapper), are each read whole
// again.
func TestIndexCacheRereadsWhatChanged(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	read := func(t *testing.T, ys *MockYaml) models.TestSetMocks {
		t.Helper()
		set, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	whole := func(t *testing.T, ys *MockYaml) models.TestSetMocks {
		t.Helper()
		budget := MockIndexCacheStats().Budget
		SetIndexCacheBudget(0)
		defer SetIndexCacheBudget(budget)
		return read(t, ys)
	}
	for _, c := range []struct {
		name string
		// big pads the file past two edgeBytes, so its middle is in
		// neither end the same-file check hashes.
		big    bool
		change func(t *testing.T, ys *MockYaml)
	}{
		{"appended", false, func(t *testing.T, ys *MockYaml) { appendDoc(t, ys, "yaml", "Acme-Queue", "late-mock") }},
		{"edited in place mid-file, the size kept", true, func(t *testing.T, ys *MockYaml) {
			p := mockFilePath(t, ys)
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			editMiddle(t, p, p)
			// A write moves the modification time on, but within one tick
			// of the clock the file system stamps with it may not.
			if err := os.Chtimes(p, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced by a file of the same size and ends, other bytes mid-file", true, func(t *testing.T, ys *MockYaml) {
			p := mockFilePath(t, ys)
			editMiddle(t, p, p+".tmp")
			if err := os.Rename(p+".tmp", p); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced, same size", false, func(t *testing.T, ys *MockYaml) {
			p := mockFilePath(t, ys)
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			edited := strings.Replace(string(data), "http://svc/config-again", "http://svc/config-AGAIN", 1)
			if edited == string(data) {
				t.Fatal("precondition: nothing to edit")
			}
			tmp := p + ".tmp"
			if err := os.WriteFile(tmp, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"edited in place, a file shorter than both ends, size and time kept", false, func(t *testing.T, ys *MockYaml) {
			editKeepingTime(t, mockFilePath(t, ys), func(data []byte) int {
				return strings.LastIndex(string(data), "config-again")
			}, "CONFIG")
		}},
		// On a file longer than both ends, an edit in either end with the size
		// and the time kept is seen only by that end's hash.
		{"edited in place in its first 64 KiB, size and time kept", true, func(t *testing.T, ys *MockYaml) {
			editKeepingTime(t, mockFilePath(t, ys), func(data []byte) int {
				at := strings.LastIndex(string(data), "config-again")
				if at < 0 || at+len("CONFIG") > edgeBytes || len(data) <= 2*edgeBytes {
					t.Fatalf("precondition: config-again at %d of %d bytes is not in the first end only", at, len(data))
				}
				return at
			}, "CONFIG")
		}},
		{"edited in place in its last 64 KiB, size and time kept", true, func(t *testing.T, ys *MockYaml) {
			editKeepingTime(t, mockFilePath(t, ys), func(data []byte) int {
				// The last byte of padTestSet's pad, before its line's end.
				at := len(data) - 2
				if data[at] != 'x' || at < edgeBytes || len(data) <= 2*edgeBytes {
					t.Fatalf("precondition: byte %d of %d is %q, not the pad's last, in the last end only", at, len(data), data[at])
				}
				return at
			}, "y")
		}},
		{"a registration since", false, func(t *testing.T, _ *MockYaml) {
			models.RegisterIdleAnswer("Acme-Queue", func(*models.Mock) bool { return false })()
		}},
		// padTestSet's pad is an Acme-Queue document, which a mapper now decodes.
		{"a YAML mapper registered since", true, func(t *testing.T, _ *MockYaml) { registerAcmeQueueMapper(t) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			ys := writeTestSetMocks(t, t.TempDir(), "yaml")
			if c.big {
				padTestSet(t, ys)
			}
			withIndexCache(t, 64<<20)
			read(t, ys)
			c.change(t, ys)
			before := MockIndexCacheStats()
			got := read(t, ys)
			if d := indexCacheDelta(before); d.Misses != 1 || d.SameFileHits+d.ContentHits != 0 {
				t.Fatalf("the read after the change: %+v; want it read whole", d)
			}
			sameAsRead(t, got, whole(t, ys))
		})
	}
}

// editKeepingTime writes with over the file at p at the offset at picks,
// in place, and puts the file's modification time back.
func editKeepingTime(t *testing.T, p string, at func(data []byte) int, with string) {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	off := at(data)
	if off < 0 || off+len(with) > len(data) || string(data[off:off+len(with)]) == with {
		t.Fatalf("precondition: nothing to edit at %d", off)
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(with), int64(off)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if now, err := os.Stat(p); err != nil || !now.ModTime().Equal(info.ModTime()) || now.Size() != info.Size() {
		t.Fatalf("precondition: the edit did not keep the size and time: %v", err)
	}
}

// acmeQueue is a kind this keploy cannot read (padTestSet's and the rows'
// appended documents) unless a YAML mapper decodes it.
const acmeQueue models.Kind = "Acme-Queue"

// registerAcmeQueueMapper registers, until the test ends, a YAML mapper that
// decodes each acmeQueue document to a mock of its name.
func registerAcmeQueueMapper(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { mockYAMLMappers.Delete(acmeQueue) })
	RegisterMockYAMLMapper(acmeQueue, MockYAMLMapper{
		Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil },
		Decode: func(doc *yaml.NetworkTrafficDoc, m *models.Mock) error {
			*m = models.Mock{Version: doc.Version, Name: doc.Name, Kind: doc.Kind}
			return nil
		},
	})
}

// padTestSet appends a document of a kind this keploy cannot read, big
// enough that the middle of the file is in it and neither end of the file
// edgeBytes long reaches it.
func padTestSet(t *testing.T, ys *MockYaml) {
	t.Helper()
	pad := "---\nversion: api.keploy.io/v1beta1\nkind: Acme-Queue\nname: pad\nspec:\n    metadata:\n        type: mocks\n        pad: " + strings.Repeat("x", 6*edgeBytes) + "\n"
	f, err := os.OpenFile(mockFilePath(t, ys), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(pad); err != nil {
		t.Fatal(err)
	}
}

// editMiddle writes src with its middle byte, in padTestSet's pad, changed,
// to dst.
func editMiddle(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(data) / 2
	if len(data) <= 2*edgeBytes+2 || data[mid] != 'x' {
		t.Fatalf("precondition: the middle of the %d-byte file is %q, not the pad", len(data), data[mid])
	}
	if src != dst {
		data[mid] = 'y'
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	f, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt([]byte("y"), int64(mid)); err != nil {
		t.Fatal(err)
	}
}

// What a replay does to the mocks a read gave it, and the payload mutator it
// sets, stay with that read: the next read answered from the same index gets
// the mocks as recorded.
func TestIndexCacheGivesEachReadItsOwnMocks(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	ys := writeTestSetMocks(t, t.TempDir(), "yaml")
	SetIndexCacheBudget(0)
	want, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withIndexCache(t, 64<<20)
	for i := 0; i < 2; i++ {
		set, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		sameAsRead(t, set, want)
		for _, pool := range [][]*models.Mock{set.Filtered, set.Unfiltered, set.AllSession, set.AllPerTest} {
			for _, m := range pool {
				m.TestModeInfo.IsFiltered = !m.TestModeInfo.IsFiltered
				m.TestModeInfo.SortOrder = 99
				m.TestModeInfo.Lifetime = models.LifetimeConnection
				m.Spec.Metadata["type"] = "changed"
				m.Spec.Async = &models.AsyncMeta{Lane: "changed"}
				m.MarkPooled()
			}
		}
		set.Payloads.SetPayloadMutator(func(_ context.Context, m *models.Mock) error {
			m.Spec.HTTPResp = &models.HTTPResp{Body: "mutated"}
			return nil
		})
	}
	if d := MockIndexCacheStats(); d.SameFileHits == 0 {
		t.Fatalf("precondition: no read was answered from the cache: %+v", d)
	}
}

// The cache holds no more than its budget: the least recently used index goes
// first, an index bigger than the whole budget is not kept, and budget 0
// keeps nothing.
func TestIndexCacheStaysWithinItsBudget(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	a := writeTestSetMocks(t, t.TempDir(), "yaml")
	b := writeTestSetMocks(t, t.TempDir(), "json")
	read := func(ys *MockYaml) {
		t.Helper()
		if _, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	withIndexCache(t, 64<<20)
	read(a)
	one := MockIndexCacheStats().Bytes
	if one <= 0 {
		t.Fatalf("precondition: the index holds %d bytes", one)
	}
	read(b)
	two := MockIndexCacheStats().Bytes
	if two <= one {
		t.Fatalf("precondition: two indexes hold %d bytes, one %d", two, one)
	}

	// Room for the larger of the two only: the second read evicts the first.
	withIndexCache(t, max(one, two-one))
	before := MockIndexCacheStats()
	read(a)
	read(b)
	d := indexCacheDelta(before)
	if d.Files != 1 || d.Evictions != 1 || d.Bytes > d.Budget {
		t.Fatalf("%+v; want one index, one evicted, within the budget", d)
	}
	read(a)
	if d := indexCacheDelta(before); d.Misses != 3 || d.SameFileHits != 0 {
		t.Fatalf("%+v; want the evicted index read whole again", d)
	}

	// Room for neither: nothing is kept, and every read is whole.
	withIndexCache(t, min(one, two-one)/2)
	before = MockIndexCacheStats()
	read(a)
	read(a)
	if d := indexCacheDelta(before); d.Files != 0 || d.Misses != 2 {
		t.Fatalf("%+v; want nothing kept", d)
	}

	// Off: not asked at all.
	SetIndexCacheBudget(0)
	before = MockIndexCacheStats()
	read(a)
	if d := indexCacheDelta(before); d.Files != 0 || d.Misses != 0 {
		t.Fatalf("%+v; want the cache off", d)
	}
}

// The budget counts what a kept index holds: the estimate is within 0.8x to
// 1.5x of the heap the index takes. The second case's URLs are long, so each
// mock's own summary and target (each kept up to 256 bytes, models'
// maxKeptDisplay) are most of what its entry holds.
func TestIndexCacheCountsTheHeapItHolds(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	long := strings.Repeat("p", 200)
	for _, tc := range []struct {
		name string
		url  func(i int) string
	}{
		{"short urls", func(i int) string { return fmt.Sprintf("http://svc/items/%d", i) }},
		{"long urls", func(i int) string { return fmt.Sprintf("http://svc/%s/%d", long, i) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ys := New(zap.NewNop(), dir, "mocks")
			base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
			for i := 0; i < 3000; i++ {
				typ := "mocks"
				if i%3 == 0 {
					typ = "config"
				}
				m := &models.Mock{Version: models.GetVersion(), Kind: models.HTTP, Spec: models.MockSpec{
					Metadata:         map[string]string{"type": typ, "operation": "GET", "connID": fmt.Sprint(i % 7)},
					HTTPReq:          &models.HTTPReq{Method: "GET", URL: tc.url(i), ProtoMajor: 1, ProtoMinor: 1},
					HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: strings.Repeat("x", 512)},
					ReqTimestampMock: base.Add(time.Duration(i) * time.Second),
					ResTimestampMock: base.Add(time.Duration(i)*time.Second + time.Millisecond),
				}}
				if s := models.MockSummaryFromSpec(m); len(s) > 256 {
					t.Fatalf("precondition: the summary %q is longer than a mock keeps", s)
				}
				if err := ys.InsertMock(ctx, m, "set-0"); err != nil {
					t.Fatal(err)
				}
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			withIndexCache(t, 1<<30)
			held := heapHeld(t, func() any {
				set, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				return set
			})
			// What the read returned is as big as what it kept; dropping it
			// leaves the index.
			SetIndexCacheBudget(0)
			withIndexCache(t, 1<<30)
			kept := heapHeld(t, func() any {
				if _, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
					t.Fatal(err)
				}
				return nil
			})
			counted := MockIndexCacheStats().Bytes
			t.Logf("3000 mocks, %s: the read and its index hold %d bytes; the index alone %d; counted %d", tc.name, held, kept, counted)
			if kept == 0 || float64(counted) < 0.8*float64(kept) || float64(counted) > 1.5*float64(kept) {
				t.Fatalf("the index is counted as %d bytes; it holds %d", counted, kept)
			}
		})
	}
}

// What the read that made an index logged as it decoded, it logs again when
// the index answers a read: the file's incomplete last document is reported
// every time.
func TestIndexCacheLogsWhatTheReadLogged(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	dir := t.TempDir()
	ys := writeTestSetMocks(t, dir, "yaml")
	f, err := os.OpenFile(mockFilePath(t, ys), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("---\nversion: api.keploy.io/v1beta1\nkind: Http\nname: q-data\nspec:\n    metadata:\n        type: mo"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	logged := func() []string {
		t.Helper()
		core, logs := observer.New(zapcore.InfoLevel)
		reader := NewWithFormat(zap.New(core), dir, "mocks", yaml.FormatYAML)
		if _, err := reader.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range logs.All() {
			out = append(out, fmt.Sprintf("%s %s %v", e.Level, e.Message, e.ContextMap()))
		}
		return out
	}
	SetIndexCacheBudget(0)
	want := logged()
	if !strings.Contains(strings.Join(want, "\n"), "warn") {
		t.Fatalf("precondition: the whole read does not warn of the incomplete document: %q", want)
	}
	withIndexCache(t, 64<<20)
	logged()
	before := MockIndexCacheStats()
	got := logged()
	if d := indexCacheDelta(before); d.SameFileHits != 1 {
		t.Fatalf("precondition: %+v; want the read answered from the cache", d)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a read answered from the cache logged\n%s\nthe whole read logged\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// whileDecoding has do run once, the first time the read now under way
// decodes an Http mock, and returns a func that ends the hook. The hook is a
// registered idle-answer predicate that answers false, so the mocks are
// derived as without it; registering it starts a new rules generation, so it
// is registered before the reads it is for.
func whileDecoding(t *testing.T, do func()) {
	t.Helper()
	var done bool
	unregister := models.RegisterIdleAnswer(models.HTTP, func(*models.Mock) bool {
		if !done {
			done = true
			do()
		}
		return false
	})
	t.Cleanup(unregister)
}

// A read the file changed under, in place and after the read began, keeps
// no index: what it decoded is not what the file now holds, so the file's
// next read is whole, and returns what the file now holds. That holds for a
// change while the read decodes, and for one after it decoded, before the
// pass that hashes the file: the hash is of the new bytes, not of those
// decoded.
func TestIndexCacheKeepsNoIndexOfAFileChangedAsItWasRead(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	for _, c := range []struct {
		name string
		// when has change run once, at its moment of the read.
		when func(t *testing.T, change func())
	}{
		{"while it decodes", func(t *testing.T, change func()) { whileDecoding(t, change) }},
		{"before it hashes", func(t *testing.T, change func()) {
			hashAllHook = change
			t.Cleanup(func() { hashAllHook = nil })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ys := writeTestSetMocks(t, t.TempDir(), "yaml")
			p := mockFilePath(t, ys)
			var changed bool
			c.when(t, func() {
				if changed {
					return
				}
				changed = true
				info, err := os.Stat(p)
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				at := strings.LastIndex(string(data), "config-again")
				if at < 0 {
					t.Fatal("precondition: nothing to edit")
				}
				f, err := os.OpenFile(p, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteAt([]byte("CONFIG"), int64(at)); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
				if err := os.Chtimes(p, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			})
			withIndexCache(t, 64<<20)
			before := MockIndexCacheStats()
			if _, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
				t.Fatal(err)
			}
			if d := indexCacheDelta(before); !changed || d.Misses != 1 || d.Files != 0 {
				t.Fatalf("the read the file changed under (changed %v): %+v; want it read whole and nothing kept", changed, d)
			}
			got, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if d := indexCacheDelta(before); d.Misses != 2 || d.SameFileHits+d.ContentHits != 0 {
				t.Fatalf("the next read: %+v; want it read whole", d)
			}
			SetIndexCacheBudget(0)
			want, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			sameAsRead(t, got, want)
		})
	}
}

// Two reads of one content that both found no index keep one: the one that
// ends last replaces the other's, and the budget counts it once.
func TestIndexCacheKeepsOneIndexOfOneContent(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	ys := writeTestSetMocks(t, t.TempDir(), "yaml")
	copyDir := t.TempDir()
	copyTestSet(t, ys, copyDir)
	copied := NewWithFormat(zap.NewNop(), copyDir, "mocks", ys.Format)
	read := func(ys *MockYaml) {
		t.Helper()
		if _, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	withIndexCache(t, 64<<20)
	read(copied)
	one := MockIndexCacheStats().Bytes
	whileDecoding(t, func() { read(copied) })
	withIndexCache(t, 64<<20)
	before := MockIndexCacheStats()
	// The copy is read whole, and kept, while the original is decoded.
	read(ys)
	if d := indexCacheDelta(before); d.Misses != 2 || d.Files != 1 || d.Bytes != one {
		t.Fatalf("%+v; want two reads whole and one index of %d bytes kept", d, one)
	}
}

// The least recently used index goes first: a read answered from an index
// makes it the most recently used.
func TestIndexCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	sets := map[string]*MockYaml{}
	for _, name := range []string{"a", "b", "c"} {
		ys := writeTestSetMocks(t, t.TempDir(), "yaml")
		appendDoc(t, ys, "yaml", "Acme-Queue", "only-in-"+name)
		sets[name] = ys
	}
	read := func(name string) {
		t.Helper()
		if _, err := sets[name].IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	withIndexCache(t, 64<<20)
	read("a")
	one := MockIndexCacheStats().Bytes
	// Room for two.
	withIndexCache(t, 2*one+one/2)
	before := MockIndexCacheStats()
	read("a")
	read("b")
	read("a") // a is now the most recently used, b the least
	read("c") // so b goes
	if d := indexCacheDelta(before); d.Misses != 3 || d.SameFileHits != 1 || d.Evictions != 1 || d.Files != 2 {
		t.Fatalf("%+v; want a answered again, and one evicted for c", d)
	}
	read("a")
	if d := indexCacheDelta(before); d.SameFileHits != 2 {
		t.Fatalf("%+v; want a still kept", d)
	}
	read("b")
	if d := indexCacheDelta(before); d.Misses != 4 {
		t.Fatalf("%+v; want b evicted, read whole", d)
	}
}

// An index bigger than the whole budget is not kept, and evicts nothing for
// room: the indexes kept stay.
func TestIndexCacheKeepsNoIndexBiggerThanItsBudget(t *testing.T) {
	c := &indexCache{budget: 100}
	probe := func(name string) *fileProbe {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return &fileProbe{path: p, format: yaml.FormatYAML, info: info, sum: uint64(len(name)), hasSum: true}
	}
	c.keep(&indexedFile{bytes: 60}, probe("a"))
	c.keep(&indexedFile{bytes: 101}, probe("bb"))
	if c.lru.Len() != 1 || c.used != 60 {
		t.Fatalf("%d indexes of %d bytes kept; want the first, of 60", c.lru.Len(), c.used)
	}
}

// A read's index builder gives up, holding nothing, once what it collected
// outgrows its budget, and on a mock whose payload is not left in the file.
func TestIndexBuilderGivesUp(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ys := writeTestSetMocks(t, t.TempDir(), "yaml")
	SetIndexCacheBudget(0)
	set, err := ys.IndexTestSetMocks(context.Background(), "set-0", models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mocks := append(append([]*models.Mock(nil), set.Filtered...), set.Unfiltered...)
	if len(mocks) < 2 {
		t.Fatalf("precondition: %d mocks", len(mocks))
	}
	whole := newIndexBuilder(1 << 30)
	for _, m := range mocks {
		whole.add([]*models.Mock{m}, nil)
	}
	if whole.full || len(whole.f.docs) != len(mocks) {
		t.Fatalf("a builder with room gave up, or kept %d of %d", len(whole.f.docs), len(mocks))
	}
	short := newIndexBuilder(whole.f.bytes - 1)
	for _, m := range mocks {
		short.add([]*models.Mock{m}, nil)
	}
	if !short.full || short.f.docs != nil {
		t.Fatalf("a builder %d bytes short of the set holds %d documents, full %v", 1, len(short.f.docs), short.full)
	}
	inMemory := newIndexBuilder(1 << 30)
	inMemory.add([]*models.Mock{{Name: "held", Kind: models.HTTP}}, nil)
	if !inMemory.full {
		t.Fatal("a builder kept a mock whose payload is in memory")
	}
}

// What the read that made an index logged as it decoded is kept up to
// maxIndexLogs entries; a read the index answers logs those, and how many
// more there were.
func TestIndexCacheLogsAgainAtMostMaxIndexLogs(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	dir := t.TempDir()
	ys := writeTestSetMocks(t, dir, "yaml")
	const more = 6
	for i := 0; i < maxIndexLogs+more; i++ {
		// A document with no kind is reported at ERROR.
		appendDoc(t, ys, "yaml", "", fmt.Sprintf("q-%03d", i))
	}
	logged := func() []observer.LoggedEntry {
		t.Helper()
		core, logs := observer.New(zapcore.InfoLevel)
		reader := NewWithFormat(zap.New(core), dir, "mocks", yaml.FormatYAML)
		if _, err := reader.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
			t.Fatal(err)
		}
		return logs.All()
	}
	withIndexCache(t, 64<<20)
	whole := logged()
	if len(whole) < maxIndexLogs+more {
		t.Fatalf("precondition: the whole read logged %d entries", len(whole))
	}
	before := MockIndexCacheStats()
	again := logged()
	if d := indexCacheDelta(before); d.SameFileHits != 1 {
		t.Fatalf("precondition: %+v; want the read answered from the cache", d)
	}
	if len(again) != maxIndexLogs+1 {
		t.Fatalf("the answered read logged %d entries; want the %d kept and one saying %d more were not", len(again), maxIndexLogs, more)
	}
	for i := 0; i < maxIndexLogs; i++ {
		if again[i].Message != whole[i].Message || !reflect.DeepEqual(again[i].ContextMap(), whole[i].ContextMap()) {
			t.Fatalf("entry %d is %q %v; the whole read logged %q %v", i, again[i].Message, again[i].ContextMap(), whole[i].Message, whole[i].ContextMap())
		}
	}
	last := again[maxIndexLogs]
	if last.Level != zapcore.WarnLevel || last.ContextMap()["not_logged_again"] != int64(more) {
		t.Fatalf("the last entry is %s %q %v; want a warning that %d were not logged again", last.Level, last.Message, last.ContextMap(), more)
	}
}

// The recorder keeps an entry with the fields of the logger it was logged
// through (With) before its own, and nothing below Info.
func TestIndexLogRecorderKeepsTheLoggersFields(t *testing.T) {
	sink := &logSink{}
	logger := zap.New(&logRecorder{sink: sink}).With(zap.String("mock_file", "f"))
	logger.Debug("not kept")
	logger.Info("kept", zap.Int("n", 1))
	entries, dropped := sink.take()
	if len(entries) != 1 || dropped != 0 || entries[0].entry.Message != "kept" {
		t.Fatalf("kept %+v, dropped %d", entries, dropped)
	}
	if f := entries[0].fields; len(f) != 2 || f[0].Key != "mock_file" || f[1].Key != "n" {
		t.Fatalf("the entry's fields are %+v; want mock_file then n", f)
	}
}

// A read the index answers warns as the whole read of its file does: of the
// requirements no mock of the set meets, of the connection failures it does
// not replay, and of a file with no documents.
func TestIndexCacheWarnsAsAWholeReadDoes(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	prev := models.RegisterMockDeps(models.HTTP, depsByMetadata)
	t.Cleanup(func() { models.RegisterMockDeps(models.HTTP, prev) })
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		write func(t *testing.T, dir string)
		warns []string
	}{
		{"unmet requirements and connection failures", func(t *testing.T, dir string) {
			ys := New(zap.NewNop(), dir, "mocks")
			if err := ys.InsertMock(ctx, linkedHTTPMock(1, "", "stmt-nowhere"), "set-0"); err != nil {
				t.Fatal(err)
			}
			if err := ys.InsertMock(ctx, cfCorpus()[0], "set-0"); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
		}, []string{"stmt-nowhere", "connection-failure mock"}},
		{"no documents", func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "set-0"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "set-0", "mocks.yaml"), []byte("# Generated by Keploy (2.0.0)\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, []string{"mock file contains no mocks"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Each read is of its own copy: a warning of unmet requirements is
			// said once per file.
			logged := func() string {
				t.Helper()
				dir := t.TempDir()
				c.write(t, dir)
				core, logs := observer.New(zapcore.WarnLevel)
				reader := NewWithFormat(zap.New(core), dir, "mocks", yaml.FormatYAML)
				if _, err := reader.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
					t.Fatal(err)
				}
				var out []string
				for _, e := range logs.All() {
					m := e.ContextMap()
					for k, v := range m {
						if s, ok := v.(string); ok && strings.Contains(s, dir) {
							m[k] = strings.ReplaceAll(s, dir, "DIR")
						}
					}
					out = append(out, fmt.Sprintf("%s %s %v", e.Level, strings.ReplaceAll(e.Message, dir, "DIR"), m))
				}
				return strings.Join(out, "\n")
			}
			SetIndexCacheBudget(0)
			want := logged()
			for _, w := range c.warns {
				if !strings.Contains(want, w) {
					t.Fatalf("precondition: the whole read does not warn of %q:\n%s", w, want)
				}
			}
			withIndexCache(t, 64<<20)
			logged()
			before := MockIndexCacheStats()
			got := logged()
			if d := indexCacheDelta(before); d.ContentHits != 1 {
				t.Fatalf("precondition: %+v; want the read answered from the cache", d)
			}
			if got != want {
				t.Fatalf("a read answered from the cache warned\n%s\nthe whole read\n%s", got, want)
			}
		})
	}
}

// An index is of one format: the same bytes in a file of the other format
// are read whole, whether that file is a copy (the content key) or the same
// file under the other name, a hard link (the same-file key).
func TestIndexCacheIsPerFormat(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		link  bool
		place func(src, dst string) error
	}{
		{"a copy", false, func(src, dst string) error {
			data, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, data, 0o644)
		}},
		{"a hard link", true, os.Link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			js := writeTestSetMocks(t, t.TempDir(), "json")
			src := mockFilePath(t, js)
			// One json document a line is yaml too: a flow mapping per
			// document.
			yamlDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(yamlDir, "set-0"), 0o755); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(yamlDir, "set-0", "mocks.yaml")
			if err := tc.place(src, dst); err != nil {
				t.Fatal(err)
			}
			a, errA := os.Stat(src)
			b, errB := os.Stat(dst)
			if errA != nil || errB != nil || os.SameFile(a, b) != tc.link {
				t.Fatalf("precondition: %s and %s one file: %v, want %v (%v, %v)", src, dst, !tc.link, tc.link, errA, errB)
			}
			ys := NewWithFormat(zap.NewNop(), yamlDir, "mocks", yaml.FormatYAML)
			withIndexCache(t, 64<<20)
			for i := 0; i < 2; i++ {
				before := MockIndexCacheStats()
				if _, err := js.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
					t.Fatal(err)
				}
				if d := indexCacheDelta(before); i == 1 && d.SameFileHits != 1 {
					t.Fatalf("precondition: the json file read again: %+v; want it answered as the same file", d)
				}
			}
			before := MockIndexCacheStats()
			_, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
			if d := indexCacheDelta(before); d.Misses != 1 || d.ContentHits+d.SameFileHits != 0 {
				t.Fatalf("the yaml read of the json file as %s: %+v (%v); want it read whole", tc.name, d, err)
			}
		})
	}
}

// The budget counts the log entries an index keeps: the index of a file
// whose documents each logged an ERROR as they were skipped counts at least
// maxIndexLogs entries more than that of a file of as many documents skipped
// at Debug, which keeps none.
func TestIndexCacheCountsTheLogsItKeeps(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	const docs, kind = 300, "Acme-Que"
	counted := func(kind string) IndexCacheStats {
		t.Helper()
		ys := writeTestSetMocks(t, t.TempDir(), "yaml")
		for i := 0; i < docs; i++ {
			appendDoc(t, ys, "yaml", kind, fmt.Sprintf("q-%03d", i))
		}
		withIndexCache(t, 1<<30)
		if _, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
			t.Fatal(err)
		}
		return MockIndexCacheStats()
	}
	logged, quiet := counted(""), counted(kind)
	// The quiet file's documents name their kind, which the other's do not.
	more := logged.Bytes - quiet.Bytes + docs*int64(len(kind))
	if least := int64(maxIndexLogs) * int64(unsafe.Sizeof(loggedEntry{})); logged.Files != 1 || quiet.Files != 1 || more < least {
		t.Fatalf("the index that kept %d log entries counts %d bytes more than one that kept none; want at least %d (%+v, %+v)", maxIndexLogs, more, least, logged, quiet)
	}
}

// An index kept by a read under one window and mapping answers a read under
// every other window and mapping as a whole read under that one does: prod's
// auto-replay reads one set again under another window or mapping (a cycle
// that runs a subset of its tests, a re-run of one test).
func TestIndexCacheAnswersUnderEveryOtherWindowAndMapping(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			ys := writeTestSetMocks(t, t.TempDir(), format)
			appendDoc(t, ys, format, string(acmeQueue), "q-data")
			cases := testSetMocksCases()
			wholes := make([]models.TestSetMocks, len(cases))
			SetIndexCacheBudget(0)
			for i, c := range cases {
				w, err := ys.IndexTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
				if err != nil {
					t.Fatal(err)
				}
				wholes[i] = w
			}
			for _, a := range cases {
				t.Run("kept under "+a.window+"/"+a.maps, func(t *testing.T) {
					withIndexCache(t, 64<<20)
					if _, err := ys.IndexTestSetMocks(ctx, "set-0", a.after, a.before, a.mapped, a.needed); err != nil {
						t.Fatal(err)
					}
					for j, b := range cases {
						before := MockIndexCacheStats()
						got, err := ys.IndexTestSetMocks(ctx, "set-0", b.after, b.before, b.mapped, b.needed)
						if err != nil {
							t.Fatal(err)
						}
						if d := indexCacheDelta(before); d.SameFileHits != 1 || d.ContentHits+d.Misses != 0 {
							t.Fatalf("read under %s/%s: %+v; want a same-file hit", b.window, b.maps, d)
						}
						t.Run("read under "+b.window+"/"+b.maps, func(t *testing.T) {
							sameAsRead(t, got, wholes[j])
						})
					}
				})
			}
		})
	}
}

// A read that keeps the payloads in memory (GetTestSetMocks, no payload
// source) never consults the cache, even for a file an index read kept the
// index of: it returns what it returns with the cache off, every payload in
// memory.
func TestIndexCacheIsNotUsedByAFullRead(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			ys := writeTestSetMocks(t, t.TempDir(), format)
			for _, c := range testSetMocksCases() {
				t.Run(c.window+"/"+c.maps, func(t *testing.T) {
					SetIndexCacheBudget(0)
					want, err := ys.GetTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
					if err != nil {
						t.Fatal(err)
					}
					withIndexCache(t, 64<<20)
					if _, err := ys.IndexTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed); err != nil {
						t.Fatal(err)
					}
					before := MockIndexCacheStats()
					got, err := ys.GetTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
					if err != nil {
						t.Fatal(err)
					}
					if d := indexCacheDelta(before); d.SameFileHits+d.ContentHits+d.Misses != 0 {
						t.Fatalf("a full read consulted the cache: %+v", d)
					}
					for _, pool := range [][2][]*models.Mock{
						{got.Filtered, want.Filtered}, {got.Unfiltered, want.Unfiltered},
						{got.AllSession, want.AllSession}, {got.AllPerTest, want.AllPerTest},
					} {
						if g, w := poolString(pool[0]), poolString(pool[1]); g != w {
							t.Fatalf("full read with an index kept: %s; cache off: %s", g, w)
						}
						for i, m := range pool[0] {
							if m.PayloadOnDisk() {
								t.Fatalf("full read gave %s with its payload on disk", m.Name)
							}
							if !reflect.DeepEqual(m.Spec, pool[1][i].Spec) {
								t.Fatalf("full read gave %s as %+v; cache off %+v", m.Name, m.Spec, pool[1][i].Spec)
							}
						}
					}
				})
			}
		})
	}
}

// A read the rules change during keeps its index under the generation the
// read began at, so the index never answers a read by the new rules: the
// next read is whole, and returns what the new rules decode. That holds for
// a change while the read decodes, and for one after it decoded, before the
// pass that hashes the file (where the index is not yet kept).
func TestIndexCacheKeepsAnIndexUnderTheRulesItsReadBeganWith(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	for _, c := range []struct {
		name string
		// when has change run once, at its moment of the read.
		when func(t *testing.T, change func())
		// decodedBefore says the whole read decoded the acmeQueue document
		// before the change, so what the read returned is not what the new
		// rules return.
		decodedBefore bool
	}{
		{"while it decodes", func(t *testing.T, change func()) { whileDecoding(t, change) }, false},
		{"before it hashes", func(t *testing.T, change func()) {
			hashAllHook = change
			t.Cleanup(func() { hashAllHook = nil })
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ys := writeTestSetMocks(t, t.TempDir(), "yaml")
			appendDoc(t, ys, "yaml", string(acmeQueue), "q-data")
			var changed bool
			c.when(t, func() {
				if !changed {
					changed = true
					registerAcmeQueueMapper(t)
				}
			})
			withIndexCache(t, 64<<20)
			before := MockIndexCacheStats()
			first, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if d := indexCacheDelta(before); !changed || d.Misses != 1 || d.Files != 1 {
				t.Fatalf("the read the rules changed during (changed %v): %+v; want it read whole and kept", changed, d)
			}
			got, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if d := indexCacheDelta(before); d.Misses != 2 || d.SameFileHits+d.ContentHits != 0 {
				t.Fatalf("the next read: %+v; want it read whole", d)
			}
			SetIndexCacheBudget(0)
			want, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			sameAsRead(t, got, want)
			if c.decodedBefore && reflect.DeepEqual(first.Skipped, want.Skipped) {
				t.Fatalf("precondition: the mapper changed nothing the read skipped (%v)", want.Skipped)
			}
		})
	}
}

// RegisterMockYAMLMapper starts a new rules generation, as each models
// registry does (TestEveryRuleChangeStartsANewGeneration): a document of its
// kind decodes otherwise once it is registered, and again once replaced.
func TestRegisterMockYAMLMapperStartsANewGeneration(t *testing.T) {
	for _, step := range []string{"registered", "replaced"} {
		gen := models.MockRulesGeneration()
		registerAcmeQueueMapper(t)
		if models.MockRulesGeneration() == gen {
			t.Fatalf("a mapper %s left the generation at %d", step, gen)
		}
	}
}

// hashSeedEnv makes TestIndexCacheHashesAreSeededPerProcess's child print its
// process's hashSeed and stop.
const hashSeedEnv = "KEPLOY_TEST_PRINT_INDEX_HASH_SEED"

// The hashes the cache keys a file by are seeded with hashSeed, a random seed
// per process: not xxhash64's fixed seed, and another in another process.
func TestIndexCacheHashesAreSeededPerProcess(t *testing.T) {
	if os.Getenv(hashSeedEnv) == "1" {
		fmt.Printf("seed=%d\n", hashSeed)
		return
	}
	ys := writeTestSetMocks(t, t.TempDir(), "yaml")
	padTestSet(t, ys)
	path := mockFilePath(t, ys)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &fileProbe{path: path, info: info}
	if err := p.hashAll(); err != nil {
		t.Fatal(err)
	}
	seeded := func(parts ...[]byte) uint64 {
		h := xxhash.NewWithSeed(hashSeed)
		for _, b := range parts {
			_, _ = h.Write(b)
		}
		return h.Sum64()
	}
	ends := [][]byte{data[:edgeBytes], data[len(data)-edgeBytes:]}
	if p.sum != seeded(data) || p.edges != seeded(ends...) {
		t.Fatalf("sum %x edges %x; want the file hashed with hashSeed %x, %x", p.sum, p.edges, seeded(data), seeded(ends...))
	}
	if p.sum == xxhash.Sum64(data) {
		t.Fatalf("the content key is xxhash64 with its fixed seed")
	}
	seeds := map[string]bool{fmt.Sprintf("seed=%d", hashSeed): true}
	for range 2 {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestIndexCacheHashesAreSeededPerProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), hashSeedEnv+"=1")
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		var seed string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "seed=") {
				seed = line
			}
		}
		if seed == "" || seeds[seed] {
			t.Fatalf("a child process's seed %q is not its own (seen %v)", seed, seeds)
		}
		seeds[seed] = true
	}
}
