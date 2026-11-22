package mockdb

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// A recording that an auto-replay round replays and prunes while it records
// appends to the very mock file the round reads and rewrites. These rows hold
// a whole-file pass of that file part-way and append a mock meanwhile: the
// append completes while the pass is still going, and the pass's result keeps
// it. Before, each pass held the file's lock for its whole length, so the
// append waited for the pass: 49 s for a 658 MB file, which ended the
// recorder's mock stream for the rest of the recording.

// liveAppendBound is how long an append made during a pass may take: far over
// an append's own milliseconds, far under the 30 s the recorder's hand-off
// used to give a consumer.
const liveAppendBound = 5 * time.Second

func liveMock(i int, at time.Time) *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.GENERIC,
		Spec: models.MockSpec{
			Metadata: map[string]string{"operation": "GET"},
			GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{
				Type: "binary", Data: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("GET key%05d", i))),
			}}}},
			GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{
				Type: "binary", Data: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("v", 512))),
			}}}},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
	}
}

// newLiveSet records n mocks (mock-0 ... mock-<n-1>) into a set of its own.
func newLiveSet(t *testing.T, n int) (*MockYaml, string, time.Time) {
	t.Helper()
	ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", "")
	start := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		if err := ys.InsertMock(context.Background(), liveMock(i, start.Add(time.Duration(i)*time.Millisecond)), "set-0"); err != nil {
			t.Fatalf("record mock %d: %v", i, err)
		}
	}
	return ys, "set-0", start
}

// setMockNames is every mock of the set, by name, sorted.
func setMockNames(t *testing.T, ys *MockYaml, set string) []string {
	t.Helper()
	var names []string
	if err := ys.WalkTestSetMocks(context.Background(), set, func(m *models.Mock, _, _ bool) {
		names = append(names, m.Name)
	}); err != nil {
		t.Fatalf("walk the set: %v", err)
	}
	sort.Strings(names)
	return names
}

// appendDuring appends one mock to a set from its own goroutine, once, the
// first time run is called, and waits up to liveAppendBound for that append to
// complete. wait waits for the append however long it takes, so no row leaves
// it running.
type appendDuring struct {
	ys    *MockYaml
	set   string
	mock  *models.Mock
	once  sync.Once
	done  chan error
	late  bool
	ran   bool
	taken time.Duration
}

func newAppendDuring(ys *MockYaml, set string, mock *models.Mock) *appendDuring {
	return &appendDuring{ys: ys, set: set, mock: mock, done: make(chan error, 1)}
}

func (a *appendDuring) run() {
	a.once.Do(func() {
		a.ran = true
		start := time.Now()
		go func() { a.done <- a.ys.InsertMock(context.Background(), a.mock, a.set) }()
		select {
		case err := <-a.done:
			a.done <- err
			a.taken = time.Since(start)
		case <-time.After(liveAppendBound):
			a.late = true
		}
	})
}

func (a *appendDuring) wait(t *testing.T) {
	t.Helper()
	if !a.ran {
		t.Fatal("the pass never reached the point the append is made at")
	}
	if err := <-a.done; err != nil {
		t.Fatalf("the append made during the pass failed: %v", err)
	}
	if a.late {
		t.Fatalf("an append made during a whole-file pass of its mock file waited for the pass (more than %s)", liveAppendBound)
	}
}

func TestAnAppendIsNotHeldByAPruneOfItsFile(t *testing.T) {
	const n = 200
	keepEven := func() map[string]models.MockState {
		keep := map[string]models.MockState{}
		for i := 0; i < n; i += 2 {
			name := fmt.Sprintf("mock-%d", i)
			keep[name] = models.MockState{Name: name}
		}
		return keep
	}

	// The same prune, of the same file, with nothing appended meanwhile.
	alone, aloneSet, _ := newLiveSet(t, n)
	if err := alone.UpdateMocks(context.Background(), aloneSet, keepEven(), time.Now(), time.Time{}); err != nil {
		t.Fatalf("prune alone: %v", err)
	}
	want := setMockNames(t, alone, aloneSet)

	ys, set, start := newLiveSet(t, n)
	during := newAppendDuring(ys, set, liveMock(n, start.Add(time.Minute)))
	// The prune judges each mock its replay kept through mergeLearned: the
	// append is made there, part-way through the pass.
	orig := mergeLearned
	t.Cleanup(func() { mergeLearned = orig })
	mergeLearned = func(m *models.Mock, st models.MockState, binds models.MockVariables) []string {
		if m.Name == "mock-100" {
			during.run()
		}
		return orig(m, st, binds)
	}
	if err := ys.UpdateMocks(context.Background(), set, keepEven(), time.Now(), time.Time{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	during.wait(t)

	want = append(want, fmt.Sprintf("mock-%d", n))
	sort.Strings(want)
	if got := setMockNames(t, ys, set); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after a prune with an append during it the set holds %d mocks %v,\nwant the prune's own result and the appended mock: %d mocks %v", len(got), got, len(want), want)
	}
}

func TestAnAppendIsNotHeldByAReadOfItsFile(t *testing.T) {
	const n = 200
	ys, set, start := newLiveSet(t, n)
	during := newAppendDuring(ys, set, liveMock(n, start.Add(time.Minute)))
	visited := 0
	if err := ys.WalkTestSetMocks(context.Background(), set, func(m *models.Mock, _, _ bool) {
		visited++
		if m.Name == "mock-100" {
			during.run()
		}
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	during.wait(t)
	if visited != n {
		t.Fatalf("the read visited %d mocks, want the %d the file held when it began", visited, n)
	}
	if got := setMockNames(t, ys, set); len(got) != n+1 {
		t.Fatalf("after the read the set holds %d mocks, want %d", len(got), n+1)
	}
}

func TestAnAppendIsNotHeldByAWriteBackOfWhatARoundLearned(t *testing.T) {
	const n = 200
	ys, set, start := newLiveSet(t, n)
	during := newAppendDuring(ys, set, liveMock(n, start.Add(time.Minute)))
	orig := mergeLearned
	t.Cleanup(func() { mergeLearned = orig })
	mergeLearned = func(m *models.Mock, st models.MockState, binds models.MockVariables) []string {
		during.run()
		return orig(m, st, binds)
	}
	learned := map[string]models.MockState{
		"mock-150": {Name: "mock-150", ReqBodyNoise: map[string][]string{"body.ts": {}}},
	}
	if _, err := ys.PersistLearned(context.Background(), set, learned); err != nil {
		t.Fatalf("persist learned: %v", err)
	}
	during.wait(t)
	got := setMockNames(t, ys, set)
	if len(got) != n+1 {
		t.Fatalf("after the write-back the set holds %d mocks, want %d", len(got), n+1)
	}
	noise := false
	if err := ys.WalkTestSetMocks(context.Background(), set, func(m *models.Mock, _, _ bool) {
		if m.Name == "mock-150" {
			_, noise = m.Spec.ReqBodyNoise["body.ts"]
		}
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !noise {
		t.Fatal("the write-back did not put the learned noise on its mock")
	}
}

// A rewrite that keeps none of the mocks it read keeps the mocks appended
// while it ran: the file then holds those alone, read as any file is.
func TestARewriteThatKeepsNothingKeepsWhatWasAppendedMeanwhile(t *testing.T) {
	const n = 5
	ys, set, start := newLiveSet(t, n)
	dir := filepath.Join(ys.MockPath, set)
	reader, err := yaml.NewRecordingMockReaderF(context.Background(), ys.Logger, ys.MockPath, dir, "mocks", yaml.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	pinned, lock, err := pinMockFile(dir, "mocks", reader, nil)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := ys.InsertMock(context.Background(), liveMock(n, start.Add(time.Minute)), set); err != nil {
		t.Fatal(err)
	}
	rw := newMockFileRewriter(ys.Logger, ys.MockPath, dir, "mocks", yaml.FormatYAML)
	defer rw.discard()
	if err := rw.commitLive(lock, pinned); err != nil {
		t.Fatalf("commit: %v", err)
	}
	appended := fmt.Sprintf("mock-%d", n)
	if got := setMockNames(t, ys, set); strings.Join(got, ",") != appended {
		t.Fatalf("after a rewrite that kept nothing the set holds %v, want the appended %s alone", got, appended)
	}
	data, err := os.ReadFile(filepath.Join(dir, "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("\n---\n")) || bytes.HasPrefix(data, []byte("---\n")) {
		t.Fatalf("the file that holds one mock holds a document separator, an empty document before it:\n%s", data)
	}
}

// Two rewrites of one mock file run one after the other: a write-back of what
// a round learned that comes while a prune runs waits for it, and neither
// puts in place a file without what the other changed.
func TestTwoRewritesOfAMockFileDoNotInterleave(t *testing.T) {
	const n = 100
	ys, set, _ := newLiveSet(t, n)
	orig := mergeLearned
	t.Cleanup(func() { mergeLearned = orig })
	var once sync.Once
	learnedDone := make(chan error, 1)
	early := false
	mergeLearned = func(m *models.Mock, st models.MockState, binds models.MockVariables) []string {
		if m.Name == "mock-50" {
			once.Do(func() {
				go func() {
					_, err := ys.PersistLearned(context.Background(), set, map[string]models.MockState{
						"mock-20": {Name: "mock-20", ReqBodyNoise: map[string][]string{"body.ts": {}}},
					})
					learnedDone <- err
				}()
				select {
				case <-learnedDone:
					early = true
				case <-time.After(300 * time.Millisecond):
				}
			})
		}
		return orig(m, st, binds)
	}
	keep := map[string]models.MockState{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("mock-%d", i)
		keep[name] = models.MockState{Name: name}
	}
	if err := ys.UpdateMocks(context.Background(), set, keep, time.Now(), time.Time{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if early {
		t.Fatal("a write-back of what a round learned ran while a prune of the same file was rewriting it")
	}
	if err := <-learnedDone; err != nil {
		t.Fatalf("write-back after the prune: %v", err)
	}
	noise := false
	if err := ys.WalkTestSetMocks(context.Background(), set, func(m *models.Mock, _, _ bool) {
		if m.Name == "mock-20" {
			_, noise = m.Spec.ReqBodyNoise["body.ts"]
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got := setMockNames(t, ys, set); len(got) != n || !noise {
		t.Fatalf("after both rewrites the set holds %d mocks (want %d), learned noise on mock-20: %v", len(got), n, noise)
	}
}

// A rewrite whose file was put in place of by another while it ran, or cut,
// is refused and leaves the file as it is: the bytes past where it read are
// another file's, not what was appended to the one it read.
func TestARewriteOfAFileReplacedMeanwhileIsRefused(t *testing.T) {
	const n = 5
	ys, set, start := newLiveSet(t, n)
	dir := filepath.Join(ys.MockPath, set)
	path := filepath.Join(dir, "mocks.yaml")
	reader, err := yaml.NewRecordingMockReaderF(context.Background(), ys.Logger, ys.MockPath, dir, "mocks", yaml.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	pinned, lock, err := pinMockFile(dir, "mocks", reader, nil)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Another writer puts a longer file in place of the one read.
	other := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", yaml.FormatYAML)
	for i := 0; i < 2*n; i++ {
		if err := other.InsertMock(context.Background(), liveMock(100+i, start), "s"); err != nil {
			t.Fatal(err)
		}
	}
	replacement, err := os.ReadFile(filepath.Join(other.MockPath, "s", "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".other"
	if err := os.WriteFile(tmp, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	rw := newMockFileRewriter(ys.Logger, ys.MockPath, dir, "mocks", yaml.FormatYAML)
	defer rw.discard()
	if err := rw.write(liveMock(0, start)); err != nil {
		t.Fatal(err)
	}
	if err := rw.commitLive(lock, pinned); err == nil {
		t.Fatal("a rewrite of a file another put in its place was put in place")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, replacement) {
		t.Fatal("a refused rewrite changed the file another put in place")
	}
}
