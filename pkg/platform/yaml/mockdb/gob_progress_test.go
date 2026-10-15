package mockdb

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// insertGobMocks hands n mocks of about 16 KiB each to a gob store at dir.
func insertGobMocks(t *testing.T, ys *MockYaml, n int) {
	t.Helper()
	body := strings.Repeat("x", 8<<10)
	for i := 0; i < n; i++ {
		m := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Name: fmt.Sprintf("mock-%d", i), Spec: models.MockSpec{
			Metadata: map[string]string{"i": fmt.Sprint(i)},
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y", Body: body},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: body},
		}}
		if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
			t.Fatal(err)
		}
	}
}

// Each mock the writer encodes is progress, before the file's buffer hands
// any byte to the disk: encoding a large mock takes time of its own.
func TestGobWriter_CountsEachMockItEncodes(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	const n = 3 // well within the file's 256 KiB buffer: no byte is written yet
	insertGobMocks(t, ys, n)
	deadline := time.Now().Add(time.Minute)
	for ys.gobEncoded.Load() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := ys.gobProgress.Load(); got != n {
		t.Fatalf("progress %d after %d mocks encoded and none written, want %d", got, ys.gobEncoded.Load(), n)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
}

// MocksLeft is how many of the session's mocks the writer has yet to
// encode: none once Close has written them.
func TestGobWriter_MocksLeft(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	ys := New(zap.NewNop(), t.TempDir(), "mocks")
	ys.gobMu.Lock() // the writer waits for it before it encodes any
	insertGobMocks(t, ys, 5)
	held := ys.MocksLeft()
	ys.gobMu.Unlock()
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	if held != 5 {
		t.Fatalf("%d mocks left of the 5 handed to a writer that had encoded none, want 5", held)
	}
	if left := ys.MocksLeft(); left != 0 {
		t.Fatalf("%d mocks left once Close wrote them, want 0", left)
	}
}

// The file's writes are progress while they go: a write the disk takes in
// pieces counts each piece.
func TestProgressWriter_CountsEachPieceTheFileTakes(t *testing.T) {
	var n atomic.Uint64
	var pieces []int
	w := progressWriter{w: writerFunc(func(b []byte) (int, error) { pieces = append(pieces, len(b)); return len(b), nil }), n: &n}
	if written, err := w.Write(make([]byte, 2*progressChunk+5)); err != nil || written != 2*progressChunk+5 {
		t.Fatalf("wrote %d (%v)", written, err)
	}
	if len(pieces) != 3 || pieces[0] != progressChunk || pieces[2] != 5 || n.Load() != uint64(2*progressChunk+5) {
		t.Fatalf("pieces %v, counted %d", pieces, n.Load())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
