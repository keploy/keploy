package mockdb

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// asTheAgentGetsIt is m as the agent decodes it off the /storemocks stream.
func asTheAgentGetsIt(t *testing.T, m *models.Mock) *models.Mock {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(m); err != nil {
		t.Fatalf("encode %s: %v", m.Name, err)
	}
	var out models.Mock
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", m.Name, err)
	}
	return &out
}

// IndexTestSetMocks leaves every payload of a yaml or json file on disk, and
// the agent is sent, pool by pool and mock for mock, what GetTestSetMocks
// would have sent it; the reports describe each mock as they did. A gob file's
// mocks stay whole.
func TestIndexTestSetMocksSendsWhatGetTestSetMocksSends(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			ys := writeTestSetMocks(t, t.TempDir(), format)
			for _, c := range testSetMocksCases() {
				t.Run(c.window+"/"+c.maps, func(t *testing.T) {
					whole, err := ys.GetTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
					if err != nil {
						t.Fatal(err)
					}
					idx, err := ys.IndexTestSetMocks(ctx, "set-0", c.after, c.before, c.mapped, c.needed)
					if err != nil {
						t.Fatal(err)
					}
					if (idx.Payloads == nil) != (format == "gob") {
						t.Fatalf("Payloads is %v for a %s file", idx.Payloads, format)
					}
					for _, pool := range []struct {
						name        string
						got, want   []*models.Mock
						sentToAgent bool
					}{
						{"Filtered", idx.Filtered, whole.Filtered, true},
						{"Unfiltered", idx.Unfiltered, whole.Unfiltered, true},
						{"AllSession", idx.AllSession, whole.AllSession, false},
						{"AllPerTest", idx.AllPerTest, whole.AllPerTest, false},
					} {
						if g, w := poolString(pool.got), poolString(pool.want); g != w {
							t.Fatalf("%s is %s; GetTestSetMocks gives %s", pool.name, g, w)
						}
						for i, m := range pool.got {
							if m.PayloadOnDisk() != (format != "gob") {
								t.Fatalf("%s: mock %s has its payload on disk: %v", pool.name, m.Name, m.PayloadOnDisk())
							}
							if s, w := models.MockSummaryFromSpec(m), models.MockSummaryFromSpec(pool.want[i]); s != w {
								t.Errorf("%s: mock %s is summed up as %q; whole as %q", pool.name, m.Name, s, w)
							}
							if s, w := models.MockTargetFromSpec(m), models.MockTargetFromSpec(pool.want[i]); s != w {
								t.Errorf("%s: mock %s targets %q; whole %q", pool.name, m.Name, s, w)
							}
							if !pool.sentToAgent {
								continue
							}
							sent, err := m.WithPayload(ctx)
							if err != nil {
								t.Fatalf("%s: read back %s: %v", pool.name, m.Name, err)
							}
							if g, w := asTheAgentGetsIt(t, sent), asTheAgentGetsIt(t, pool.want[i]); !reflect.DeepEqual(g, w) {
								t.Fatalf("%s: the agent gets mock %s as\n%+v\nGetTestSetMocks sent it as\n%+v", pool.name, m.Name, g, w)
							}
						}
					}
				})
			}
		})
	}
}

// heapHeld is how much more heap holding what read returns takes.
func heapHeld(t *testing.T, read func() any) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	held := read()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(held)
	if after.HeapAlloc < before.HeapAlloc {
		return 0
	}
	return after.HeapAlloc - before.HeapAlloc
}

// What the index read holds is a small entry per mock, not the set: the
// payloads, almost all of a recording's bytes, stay in the file.
func TestIndexTestSetMocksHoldsTheSetWithoutItsPayloads(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	body := strings.Repeat(`{"doc":"`+strings.Repeat("x", 120)+`"},`, 400) // ~50 KB
	for i := 0; i < 300; i++ {
		typ := "mocks"
		if i%3 == 0 {
			typ = "config"
		}
		m := &models.Mock{Version: models.GetVersion(), Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": typ, "operation": "GET"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://svc/items/%d", i), ProtoMajor: 1, ProtoMinor: 1},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body},
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
	whole := heapHeld(t, func() any {
		set, err := ys.GetTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return set
	})
	index := heapHeld(t, func() any {
		set, err := ys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return set
	})
	t.Logf("held: whole %d bytes, index %d bytes", whole, index)
	if whole < 10<<20 {
		t.Fatalf("precondition: the whole set holds %d bytes; want the ~15 MB of payloads", whole)
	}
	if index*20 > whole {
		t.Fatalf("the index read holds %d bytes, more than a twentieth of the whole set's %d", index, whole)
	}
}

// indexOne writes mocks.<ext> holding content into a new set and indexes it,
// returning the per-test pool, every mock of which is per-test.
func indexOne(t *testing.T, format yaml.Format, content string) (*MockYaml, string, []*models.Mock) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "set-0", "mocks."+format.FileExtension())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", format)
	set, err := ys.IndexTestSetMocks(context.Background(), "set-0", models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ys, path, set.Filtered
}

func grpcDoc(name, op string) string {
	return "version: api.keploy.io/v1beta1\nkind: gRPC\nname: " + name + "\nspec:\n    metadata:\n        type: mocks\n        operation: " + op +
		"\n    grpcReq:\n        headers: {}\n    grpcResponse:\n        headers: {}\n" +
		"    reqTimestampMock: 2024-01-02T10:00:00Z\n    resTimestampMock: 2024-01-02T10:00:01Z\n"
}

// The payload is read back from the mock file the mocks were read from, and
// from no other: a file put in its place (the prune's and the noise
// write-back's rewrite), a removed one, and one changed in place are refused.
// A file only appended to since (a live recording's) is read as before.
func TestPayloadSourceReadsBackOnlyTheFileItRead(t *testing.T) {
	ctx := context.Background()
	content := grpcDoc("mock-0", "/a.A/One") + "---\n" + grpcDoc("mock-1", "/a.A/Two")
	sent := func(m *models.Mock) (string, error) {
		w, err := m.WithPayload(ctx)
		if err != nil {
			return "", err
		}
		// The operation is the one metadata value a mock whose payload is
		// on disk does not keep: only the file's document has it.
		return w.Spec.Metadata["operation"], nil
	}

	t.Run("appended to", func(t *testing.T) {
		_, path, mocks := indexOne(t, yaml.FormatYAML, content)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("---\n" + grpcDoc("mock-2", "/a.A/Three")); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		for i, want := range []string{"/a.A/One", "/a.A/Two"} {
			if got, err := sent(mocks[i]); err != nil || got != want {
				t.Fatalf("mock-%d read back as %q, %v; want %q", i, got, err, want)
			}
		}
	})
	t.Run("replaced", func(t *testing.T) {
		_, path, mocks := indexOne(t, yaml.FormatYAML, content)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		if _, err := sent(mocks[0]); !errors.Is(err, models.ErrPayloadFileReplaced) {
			t.Fatalf("a replaced file read back with %v; want ErrPayloadFileReplaced", err)
		}
	})
	t.Run("removed", func(t *testing.T) {
		_, path, mocks := indexOne(t, yaml.FormatYAML, content)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, err := sent(mocks[1]); !errors.Is(err, models.ErrPayloadFileReplaced) {
			t.Fatalf("a removed file read back with %v; want ErrPayloadFileReplaced", err)
		}
	})
	t.Run("changed in place", func(t *testing.T) {
		_, path, mocks := indexOne(t, yaml.FormatYAML, content)
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte(strings.Replace(content, "mock-0", "mock-9", 1)), 0); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if _, err := sent(mocks[0]); err == nil || !strings.Contains(err.Error(), "changed in place") {
			t.Fatalf("a file changed in place read back with %v; want it refused", err)
		}
	})
}

// The mock before an unfinished separator at the end of the file (a write
// cut off between documents) is read, and is read back from the same lines.
// So is the last NDJSON line, with no newline.
func TestPayloadSourceReadsBackTheLastDocument(t *testing.T) {
	ctx := context.Background()
	_, _, mocks := indexOne(t, yaml.FormatYAML, grpcDoc("mock-0", "/a.A/One")+"---\n"+grpcDoc("mock-1", "/a.A/Two")+"--")
	if len(mocks) != 2 {
		t.Fatalf("read %d mocks; want 2", len(mocks))
	}
	w, err := mocks[1].WithPayload(ctx)
	if err != nil || w.Spec.Metadata["operation"] != "/a.A/Two" || w.Spec.GRPCReq == nil {
		t.Fatalf("the document before the unfinished separator read back as %+v, %v", w, err)
	}

	jys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", yaml.FormatJSON)
	for _, op := range []string{"/a.A/One", "/a.A/Two"} {
		m := &models.Mock{Version: models.GetVersion(), Kind: models.HTTP, Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://svc" + op, ProtoMajor: 1, ProtoMinor: 1},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: op},
		}}
		if err := jys.InsertMock(ctx, m, "set-0"); err != nil {
			t.Fatal(err)
		}
	}
	if err := jys.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(jys.MockPath, "set-0", "mocks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.TrimRight(data, "\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := jys.IndexTestSetMocks(ctx, "set-0", models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	all := append(append([]*models.Mock(nil), set.Filtered...), set.Unfiltered...)
	if len(all) != 2 {
		t.Fatalf("read %d json mocks; want 2", len(all))
	}
	for _, m := range all {
		w, err := m.WithPayload(ctx)
		if err != nil || !strings.HasSuffix(w.Spec.HTTPReq.URL, w.Spec.HTTPResp.Body) {
			t.Fatalf("json mock %s read back as %+v, %v", m.Name, w, err)
		}
	}
}
