package mockdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
// time, and a registration since, are each read whole again.
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
		name   string
		change func(t *testing.T, ys *MockYaml)
	}{
		{"appended", func(t *testing.T, ys *MockYaml) { appendDoc(t, ys, "yaml", "Acme-Queue", "late-mock") }},
		{"replaced, same size", func(t *testing.T, ys *MockYaml) {
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
		{"edited in place at its end, size and time kept", func(t *testing.T, ys *MockYaml) {
			p := mockFilePath(t, ys)
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
			if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
		{"a registration since", func(t *testing.T, _ *MockYaml) {
			models.RegisterIdleAnswer("Acme-Queue", func(*models.Mock) bool { return false })()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ys := writeTestSetMocks(t, t.TempDir(), "yaml")
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
// 1.5x of the heap the index takes.
func TestIndexCacheCountsTheHeapItHolds(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
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
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://svc/items/%d", i), ProtoMajor: 1, ProtoMinor: 1},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: strings.Repeat("x", 512)},
			ReqTimestampMock: base.Add(time.Duration(i) * time.Second),
			ResTimestampMock: base.Add(time.Duration(i)*time.Second + time.Millisecond),
		}}
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
	// What the read returned is as big as what it kept; dropping it leaves
	// the index.
	SetIndexCacheBudget(0)
	withIndexCache(t, 1<<30)
	kept := heapHeld(t, func() any {
		if _, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	counted := MockIndexCacheStats().Bytes
	t.Logf("3000 mocks: the read and its index hold %d bytes; the index alone %d; counted %d", held, kept, counted)
	if kept == 0 || float64(counted) < 0.8*float64(kept) || float64(counted) > 1.5*float64(kept) {
		t.Fatalf("the index is counted as %d bytes; it holds %d", counted, kept)
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
