package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// The content key keeps to the format too. Content A is kept as json;
// content B, of A's size, is kept as yaml; then A's bytes are read as yaml
// (one json document a line is yaml too). B's entry sends the lookup past
// the size check into the content loop, where A's json index matches by
// sum. The read must be whole.
func TestIndexCacheContentKeyKeepsToTheFormat(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	withIndexCache(t, 64<<20)
	read := func(ys *MockYaml) (models.TestSetMocks, error) {
		t.Helper()
		return ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
	}
	js := writeTestSetMocks(t, t.TempDir(), "json")
	ys := writeTestSetMocks(t, t.TempDir(), "yaml")
	size := func(ys *MockYaml) int {
		fi, err := os.Stat(mockFilePath(t, ys))
		if err != nil {
			t.Fatal(err)
		}
		return int(fi.Size())
	}
	for i := 0; size(js) < size(ys)+3; i++ {
		appendDoc(t, js, "json", "Generic", "pad-"+string(rune('a'+i%26)))
	}
	pad := size(js) - size(ys)
	f, err := os.OpenFile(mockFilePath(t, ys), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("#" + strings.Repeat("x", pad-2) + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if size(js) != size(ys) {
		t.Fatalf("precondition: sizes %d and %d", size(js), size(ys))
	}
	if _, err := read(js); err != nil {
		t.Fatal(err)
	}
	if _, err := read(ys); err != nil {
		t.Fatal(err)
	}
	if st := MockIndexCacheStats(); st.Files != 2 {
		t.Fatalf("precondition: want both indexes kept, got %+v", st)
	}
	data, err := os.ReadFile(mockFilePath(t, js))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "set-0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "set-0", "mocks.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	before := MockIndexCacheStats()
	got, gotErr := read(c)
	if d := indexCacheDelta(before); d.Misses != 1 || d.SameFileHits+d.ContentHits != 0 {
		t.Fatalf("json bytes read as yaml beside a yaml index of their size: %+v (%v); want it read whole", d, gotErr)
	}
	SetIndexCacheBudget(0)
	want, wantErr := read(c)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("errors differ: %v vs %v", gotErr, wantErr)
	}
	if gotErr == nil {
		sameAsRead(t, got, want)
	}
}
