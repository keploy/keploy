package record

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The mocks taken ahead are written, and finished, in the order they came,
// named in that order, each written once, the file the same as one written a
// mock at a time; a mock that cannot be encoded is finished with its error in
// its place.
func TestMockLookaheadWritesInOrder(t *testing.T) {
	const n = 300
	ctx := context.Background()
	logger := zap.NewNop()
	set := &rollSet{id: "s0"}

	write := func(dir string, ahead bool) ([]string, []string, []byte) {
		store := mockdb.New(logger, dir, "mocks")
		// Hooks other than the no-op ones are handed each document written.
		doc := NewInsertedMockDoc(ctx, &encodedHooks{})
		var finished, errs []string
		var docs [][]byte
		finish := func(mock *models.Mock, tempID string, s *rollSet, encoded []byte, format string, err error) {
			if s != set {
				t.Errorf("mock %s finished in another set", tempID)
			}
			if err == nil {
				if len(encoded) == 0 || format != "yaml" {
					t.Errorf("mock %s: no document handed back (%q)", tempID, format)
				}
				docs = append(docs, encoded)
			}
			finished = append(finished, tempID+"="+mock.Name)
			if err != nil {
				errs = append(errs, tempID)
				if !errors.Is(err, models.ErrMockEncode) {
					t.Errorf("mock %s: %v, want an encode error", tempID, err)
				}
			}
		}
		var la *mockLookahead
		if ahead {
			la = lookaheadFor(nil, store, false, doc, finish)
			if la == nil {
				t.Fatal("no look-ahead for a MockYaml that does not roll")
			}
		}
		// Unbuffered, from a producer that pauses now and then: the loop
		// finds no frame ready with mocks pending, and writes the oldest.
		frames := make(chan *models.Mock)
		go func() {
			defer close(frames)
			for i := range n {
				m := rollMock(fmt.Sprintf("t%03d", i), rollT0.Add(time.Duration(i)*time.Millisecond), (i*37)%900)
				switch {
				case i%97 == 50:
					// A fault on a non-HTTP call: refused as it is prepared.
					m.Kind = models.Postgres
					m.Spec.Fault = &models.Fault{Kind: "reset"}
				case i%89 == 30:
					// A kind no encoder knows: refused as it is encoded,
					// on an encoder.
					m.Kind = models.Kind("ThisKindDoesNotExist")
				}
				frames <- m
				if i%25 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		}()
		for {
			m, ok := la.next(frames)
			if !ok {
				break
			}
			tempID := m.Name
			if la.take(m, tempID, set) {
				continue
			}
			encoded, format, err := doc.Insert(store, m, set.id)
			finish(m, tempID, set, encoded, format, err)
		}
		if ahead && la.mostPending < 2 {
			t.Errorf("the look-ahead never held more than %d mock at once", la.mostPending)
		}
		la.stop()
		raw, err := os.ReadFile(filepath.Join(dir, "s0", "mocks.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		// The documents handed back are what the file holds, in its order.
		if want := append([]byte(utils.GetVersionAsComment()), bytes.Join(docs, []byte("---\n"))...); !bytes.Equal(raw, want) {
			t.Errorf("the file is not the documents handed back (%d bytes, want %d)", len(raw), len(want))
		}
		return finished, errs, raw
	}

	oneByOne, oneErrs, want := write(t.TempDir(), false)
	ahead, aheadErrs, got := write(t.TempDir(), true)
	if len(ahead) != n {
		t.Fatalf("%d mocks finished, want %d", len(ahead), n)
	}
	if strings.Join(ahead, ",") != strings.Join(oneByOne, ",") {
		t.Fatalf("mocks finished out of order or named otherwise:\n ahead %v\n one by one %v", ahead[:10], oneByOne[:10])
	}
	if len(aheadErrs) == 0 || strings.Join(aheadErrs, ",") != strings.Join(oneErrs, ",") {
		t.Fatalf("mocks refused: %v ahead, %v one by one", aheadErrs, oneErrs)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the mock file written ahead differs from the one written a mock at a time (%d bytes, want %d)", len(got), len(want))
	}
}

// A recording that rolls over to new sets, or a MockDB that cannot insert in
// steps, takes no mock ahead.
func TestMockLookaheadOffWhenItCannot(t *testing.T) {
	doc := NewInsertedMockDoc(context.Background(), BaseRecordHooks{})
	if la := lookaheadFor(nil, mockdb.New(zap.NewNop(), t.TempDir(), "mocks"), true, doc, nil); la != nil {
		t.Fatal("a recording that rolls over took mocks ahead")
	}
	if la := lookaheadFor(nil, plainMockDB{}, false, doc, nil); la != nil {
		t.Fatal("a MockDB that cannot insert in steps took mocks ahead")
	}
	wrapped := &wrappedMockYaml{MockYaml: mockdb.New(zap.NewNop(), t.TempDir(), "mocks")}
	if la := lookaheadFor(nil, wrapped, false, doc, nil); la != nil {
		t.Fatal("a MockDB that wraps a MockYaml and overrides InsertMock had its inserts taken around it")
	}
	var la *mockLookahead
	if la.take(&models.Mock{}, "t", &rollSet{}) {
		t.Fatal("a nil look-ahead took a mock")
	}
	la.flush()
	la.stop()
}

// wrappedMockYaml wraps a MockYaml as k8s-proxy does, overriding InsertMock.
type wrappedMockYaml struct{ *mockdb.MockYaml }

func (w *wrappedMockYaml) InsertMock(ctx context.Context, m *models.Mock, set string) error {
	return w.MockYaml.InsertMock(ctx, m, set)
}

type plainMockDB struct{}

func (plainMockDB) InsertMock(context.Context, *models.Mock, string) error { return nil }
func (plainMockDB) DeleteMocksForSet(context.Context, string) error        { return nil }
func (plainMockDB) GetCurrMockID() int64                                   { return 0 }
func (plainMockDB) ResetCounterID()                                        {}

// skippingHooks skips the mock whose temp name is skip in BeforeMockInsert, as
// the AsyncRecorder skips an unchanged poll cycle, and tells each mock written.
type skippingHooks struct {
	insertedHooks
	skip string
}

func (h skippingHooks) BeforeMockInsert(_ context.Context, info *MockContext) error {
	info.Skip = info.Mock.Name == h.skip
	return nil
}

// The recorder's own mock loop over a bare MockYaml that does not roll over
// takes the mocks waiting for it ahead of their writes, and loses nothing:
// every mock is written once, named in the order it came; the one a hook skips
// is not written; a revoke frame mid-stream changes none of that; and a test's
// mapping names its mocks by the names they were saved under.
func TestStart_LooksAheadOverABareMockYaml(t *testing.T) {
	const n = 300
	dir := t.TempDir()
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	hooks := skippingHooks{insertedHooks: insertedHooks{mocks: make(chan string, n+1)}, skip: "t050"}
	r := New(logger, testdb.New(logger, dir), mockdb.New(logger, dir, "mocks"), mapdb.New(logger, dir, "mappings"),
		&recTelemetry{}, &blockingInstr{f}, recTestSetConf{}, hooks, &config.Config{}).(*Recorder)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	send := func(m *models.Mock) {
		select {
		case f.outgoing <- m:
		case <-time.After(10 * time.Second):
			t.Fatal("the recorder stopped taking mocks")
		}
	}
	for i := range n {
		send(rollMock(fmt.Sprintf("t%03d", i), rollT0.Add(time.Duration(i)*time.Millisecond), (i*53)%700))
		if i == 150 {
			send(&models.Mock{Kind: models.RevokedTests, Spec: models.MockSpec{Metadata: map[string]string{"revoked_tests": "test-gone"}}})
		}
	}
	for range n - 1 {
		select {
		case <-hooks.mocks:
		case <-time.After(10 * time.Second):
			t.Fatal("not every mock was written")
		}
	}
	select {
	case f.incoming <- rollTest("test-a", rollT0, rollT0.Add(time.Duration(n)*time.Millisecond)):
	case <-time.After(10 * time.Second):
		t.Fatal("the recorder never took the test case")
	}
	waitForSavedTest(t, dir, "test-set-0", "test-a")
	select {
	case f.mappings <- models.TestMockMapping{TestName: "test-a", MockIDs: []string{"t003", "t004", "t250"}}:
	case <-time.After(10 * time.Second):
		t.Fatal("the recorder never took the mapping")
	}
	time.Sleep(3 * mappingFlushInterval)
	cancel()
	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(90 * time.Second):
		t.Fatal("Start did not return")
	}

	if logs.FilterMessage("the mock loop encodes the mocks waiting for it ahead of their writes").Len() != 1 {
		t.Fatal("the look-ahead was not on for a bare MockYaml that does not roll over")
	}
	ended := logs.FilterMessage("the mock loop's look-ahead ended").All()
	if len(ended) != 1 {
		t.Fatalf("the look-ahead's end was said %d times, want once", len(ended))
	}
	if most, ok := ended[0].ContextMap()["mostMocksAheadAtOnce"].(int64); !ok || most < 2 {
		t.Fatalf("the loop never took more than one mock ahead: %v", ended[0].ContextMap())
	}
	set := readSet(t, dir, "test-set-0")
	if len(set.mocks) != n-1 {
		t.Fatalf("%d mocks written, want %d (all but the skipped one)", len(set.mocks), n-1)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "test-set-0", "mocks.yaml"))
	require.NoError(t, err)
	// Named in the order they came (from mock-0), the skipped one taking no
	// name.
	next := 0
	for i := range n {
		if i == 50 {
			continue
		}
		name := fmt.Sprintf("mock-%d", next)
		next++
		doc, ok := set.mocks[name]
		if !ok || !bytes.Contains(doc, []byte(fmt.Sprintf("url: http://dep.local/t%03d\n", i))) {
			t.Fatalf("%s is not t%03d, the mock that came in its place", name, i)
		}
	}
	if bytes.Contains(raw, []byte("dep.local/t050\n")) {
		t.Fatal("the mock a hook skipped was written")
	}
	var names []string
	for _, e := range set.mappings["test-a"] {
		names = append(names, e.Name)
	}
	// t003 and t004 are mock-3 and mock-4; t250 is mock-249, t050 skipped.
	if got := strings.Join(names, ","); got != "mock-3,mock-4,mock-249" {
		t.Fatalf("test-a's mapping names %s, want mock-3,mock-4,mock-249", got)
	}
}
