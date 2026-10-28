package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// A yaml or json mock file's mocks are read back from the spill the index
// read wrote (one YAML decode per mock, not two), as the agent would get them
// decoded from the file, and the spill leaves nothing in the test set's
// directory.
func TestIndexedMocksAreReadBackFromTheSpill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no spill on Windows")
	}
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			if !spillRoom(dir, 1<<20) {
				t.Skip("the test's filesystem has no room to spare for a spill")
			}
			ys := writeTestSetMocks(t, dir, format)
			whole, err := ys.GetTestSetMocks(ctx, "set-0", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			idx, err := ys.IndexTestSetMocks(ctx, "set-0", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			src, ok := idx.Payloads.(*payloadSource)
			if !ok || src.spill == nil {
				t.Fatalf("the index read of a %s file made no spill (Payloads %T)", format, idx.Payloads)
			}
			entries, err := os.ReadDir(filepath.Join(dir, "set-0"))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".keploy-mock-payloads-") {
					t.Fatalf("the spill %s is in the test set's directory", e.Name())
				}
			}
			readBack := 0
			for _, pool := range []struct{ got, want []*models.Mock }{{idx.Filtered, whole.Filtered}, {idx.Unfiltered, whole.Unfiltered}} {
				if len(pool.got) != len(pool.want) {
					t.Fatalf("index pool holds %d mocks; GetTestSetMocks %d", len(pool.got), len(pool.want))
				}
				for i, m := range pool.got {
					if !m.PayloadOnDisk() {
						continue
					}
					readBack++
					sent, err := m.WithPayload(ctx)
					if err != nil {
						t.Fatalf("read back %s: %v", m.Name, err)
					}
					if g, w := asTheAgentGetsIt(t, sent), asTheAgentGetsIt(t, pool.want[i]); !reflect.DeepEqual(g, w) {
						t.Fatalf("the agent gets mock %s from the spill as\n%+v\nGetTestSetMocks sent it as\n%+v", m.Name, g, w)
					}
				}
			}
			if readBack == 0 || src.spill.hits != readBack {
				t.Fatalf("%d of the %d mocks read back came from the spill", src.spill.hits, readBack)
			}
		})
	}
}

// A document changed in place since the index read is not answered from the
// spill: it is decoded again, which finds the change.
func TestSpillIsNotReadForADocumentChangedInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no spill on Windows")
	}
	content := grpcDoc("mock-0", "/a.A/One") + "---\n" + grpcDoc("mock-1", "/a.A/Two")
	_, path, mocks := indexOne(t, yaml.FormatYAML, content)
	ctx := context.Background()
	if _, err := mocks[1].WithPayload(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(strings.Replace(content, "/a.A/Two", "/a.A/Six", 1)), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got, err := mocks[1].WithPayload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if op := got.Spec.Metadata["operation"]; op != "/a.A/Six" {
		t.Fatalf("a document changed in place was read back from the spill: operation %q, want the file's /a.A/Six", op)
	}
	if got, err := mocks[0].WithPayload(ctx); err != nil || got.Spec.Metadata["operation"] != "/a.A/One" {
		t.Fatalf("the unchanged document read back as %v, %v", got, err)
	}
}

// No spill is made where it would leave its filesystem short of room: the
// mocks are read back from the mock file.
func TestNoSpillWithoutRoomForIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mocks.yaml")
	if s := newPayloadSpill(&payloadSource{}, path, 1<<62); s != nil {
		t.Fatal("a spill of 4 EiB was made")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the refused spill left %s behind", entries[0].Name())
	}
}
