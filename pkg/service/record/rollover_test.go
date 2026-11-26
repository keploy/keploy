package record

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	yamlLib "gopkg.in/yaml.v3"
)

// countRoller is a Roller that names the sets after the first "<first>-s<n>"
// and counts the rollovers.
type countRoller struct {
	limit int64
	mu    sync.Mutex
	first string
	n     int
	cuts  []time.Time
	sets  []string
}

func (ro *countRoller) Limit() int64 { return ro.limit }

func (ro *countRoller) Next(_ context.Context, cur string) (string, error) {
	ro.mu.Lock()
	defer ro.mu.Unlock()
	if ro.first == "" {
		ro.first = cur
	}
	ro.n++
	return fmt.Sprintf("%s-s%d", ro.first, ro.n), nil
}

func (ro *countRoller) Rolled(prev, next string, cutAt time.Time) {
	ro.mu.Lock()
	defer ro.mu.Unlock()
	if len(ro.sets) == 0 {
		ro.sets = append(ro.sets, prev)
	}
	ro.sets = append(ro.sets, next)
	ro.cuts = append(ro.cuts, cutAt)
}

func (ro *countRoller) rolled() int {
	ro.mu.Lock()
	defer ro.mu.Unlock()
	return len(ro.cuts)
}

// insertedHooks says each mock the recorder inserted, so a row knows the mock
// loop is past it (and past the rollover it may have made).
type insertedHooks struct {
	BaseRecordHooks
	mocks chan string
}

func (h insertedHooks) AfterMockInsert(_ context.Context, info *MockContext) error {
	h.mocks <- info.Mock.Name
	return nil
}

// rollRec is a recording into real yaml stores under dir, rolled over by ro,
// fed by the row through f.
type rollRec struct {
	t      *testing.T
	dir    string
	f      *fakeInstr
	ro     *countRoller
	hooks  insertedHooks
	cancel context.CancelFunc
	done   chan error
}

func startRollRec(t *testing.T, limit int64) *rollRec {
	t.Helper()
	dir := t.TempDir()
	logger := zap.NewNop()
	rr := &rollRec{
		t:   t,
		dir: dir,
		f: &fakeInstr{
			mappings: make(chan models.TestMockMapping),
			incoming: make(chan *models.TestCase),
			outgoing: make(chan *models.Mock),
		},
		ro:    &countRoller{limit: limit},
		hooks: insertedHooks{mocks: make(chan string, 1024)},
		done:  make(chan error, 1),
	}
	r := New(logger, testdb.New(logger, dir), mockdb.New(logger, dir, "mocks"), mapdb.New(logger, dir, "mappings"),
		&recTelemetry{}, &blockingInstr{rr.f}, recTestSetConf{}, rr.hooks, &config.Config{}).(*Recorder)
	r.SetRoller(rr.ro)
	ctx, cancel := context.WithCancel(context.Background())
	rr.cancel = cancel
	go func() { rr.done <- r.Start(ctx) }()
	return rr
}

func (rr *rollRec) mock(m *models.Mock) {
	rr.t.Helper()
	select {
	case rr.f.outgoing <- m:
	case <-time.After(10 * time.Second):
		rr.t.Fatal("the recorder never took the mock")
	}
	select {
	case <-rr.hooks.mocks:
	case <-time.After(10 * time.Second):
		rr.t.Fatal("the recorder never inserted the mock")
	}
}

// rolledTo waits for the recording to have rolled over n times: the mock loop
// rolls over after it inserts the mock that fills the set.
func (rr *rollRec) rolledTo(n int) {
	rr.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for rr.ro.rolled() < n {
		if time.Now().After(deadline) {
			rr.t.Fatalf("the recording rolled over %d times; want %d", rr.ro.rolled(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (rr *rollRec) test(tc *models.TestCase) {
	rr.t.Helper()
	select {
	case rr.f.incoming <- tc:
	case <-time.After(10 * time.Second):
		rr.t.Fatal("the recorder never took the test case")
	}
}

func (rr *rollRec) mapping(test string, tempIDs ...string) {
	rr.t.Helper()
	select {
	case rr.f.mappings <- models.TestMockMapping{TestName: test, MockIDs: tempIDs}:
	case <-time.After(10 * time.Second):
		rr.t.Fatal("the recorder never took the mapping")
	}
}

func (rr *rollRec) stop() {
	rr.t.Helper()
	// What was handed over is persisted before the stop: the test cases'
	// files exist (the mock loop's are inserted already).
	rr.cancel()
	close(rr.f.outgoing)
	close(rr.f.incoming)
	close(rr.f.mappings)
	select {
	case err := <-rr.done:
		if err != nil {
			rr.t.Fatalf("Start: %v", err)
		}
	case <-time.After(90 * time.Second):
		rr.t.Fatal("Start did not return")
	}
}

var rollT0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func rollMock(tempID string, at time.Time, body int) *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(), Name: tempID, Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://dep.local/" + tempID, Timestamp: at},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: strings.Repeat("x", body), Timestamp: at.Add(time.Millisecond)},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
	}
}

func rollTest(name string, req, resp time.Time) *models.TestCase {
	return &models.TestCase{
		Version: models.GetVersion(), Name: name, Kind: models.HTTP,
		HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://app.local/" + name, Timestamp: req},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok", Timestamp: resp},
	}
}

// setOnDisk is what a test set holds on disk: its tests, its mock documents
// by name, and its mappings.
type setOnDisk struct {
	tests    []string
	mocks    map[string][]byte
	mappings map[string][]models.MockEntry
}

func readSet(t *testing.T, dir, set string) setOnDisk {
	t.Helper()
	out := setOnDisk{mocks: map[string][]byte{}}
	entries, err := os.ReadDir(filepath.Join(dir, set, "tests"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		out.tests = append(out.tests, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(out.tests)
	raw, err := os.ReadFile(filepath.Join(dir, set, "mocks.yaml"))
	if err != nil {
		t.Fatalf("set %s has no mock file: %v", set, err)
	}
	for _, doc := range bytes.Split(raw, []byte("\n---\n")) {
		var head struct {
			Name string `yaml:"name"`
		}
		if err := yamlLib.Unmarshal(doc, &head); err != nil {
			t.Fatalf("set %s: a mock document does not decode: %v", set, err)
		}
		if _, dup := out.mocks[head.Name]; dup {
			t.Fatalf("set %s holds mock %s twice", set, head.Name)
		}
		// The file's first document begins with keploy's version comment.
		doc = bytes.TrimPrefix(doc, []byte("---\n"))
		for bytes.HasPrefix(doc, []byte("#")) {
			_, doc, _ = bytes.Cut(doc, []byte("\n"))
		}
		out.mocks[head.Name] = doc
	}
	m, _, err := mapdb.New(zap.NewNop(), dir, "mappings").Get(context.Background(), set)
	if err != nil {
		t.Fatalf("set %s: mappings: %v", set, err)
	}
	out.mappings = m
	return out
}

// Crossing the size limit three times leaves four test sets, and every set is
// whole on its own: each test in exactly one set, each mock in the set of the
// test that made it, and each set's mappings naming only mocks its mock file
// holds.
func TestRolloverCrossingTheLimitThreeTimesGivesFourWholeSets(t *testing.T) {
	const body = 1000
	rr := startRollRec(t, 4096)
	type made struct{ test, tempID string }
	var all []made
	after := 0
	for i := 0; i < 200 && after < 2; i++ {
		at := rollT0.Add(time.Duration(i) * 10 * time.Millisecond)
		tempID, test := fmt.Sprintf("tmp-%d", i), fmt.Sprintf("test-%d", i)
		rr.mock(rollMock(tempID, at.Add(time.Millisecond), body))
		rr.test(rollTest(test, at, at.Add(4*time.Millisecond)))
		rr.mapping(test, tempID)
		all = append(all, made{test, tempID})
		if rr.ro.rolled() == 3 {
			after++
		}
	}
	// The mapping of the last test is written before the stop.
	time.Sleep(3 * mappingFlushInterval)
	rr.stop()

	if got := rr.ro.rolled(); got != 3 {
		t.Fatalf("the recording rolled over %d times; want 3", got)
	}
	if len(rr.ro.sets) != 4 {
		t.Fatalf("the recording has sets %v; want 4", rr.ro.sets)
	}
	testIn := map[string]string{}
	mockIn := map[string][]string{}
	for _, set := range rr.ro.sets {
		s := readSet(t, rr.dir, set)
		if len(s.tests) == 0 {
			t.Fatalf("set %s holds no test", set)
		}
		for _, tc := range s.tests {
			if prev, dup := testIn[tc]; dup {
				t.Fatalf("test %s is in set %s and in set %s", tc, prev, set)
			}
			testIn[tc] = set
		}
		for name := range s.mocks {
			mockIn[name] = append(mockIn[name], set)
		}
		for tc, entries := range s.mappings {
			if !contains(s.tests, tc) {
				t.Fatalf("set %s maps test %s, which it does not hold", set, tc)
			}
			for _, e := range entries {
				if _, ok := s.mocks[e.Name]; !ok {
					t.Fatalf("set %s maps test %s to mock %s, which its mock file does not hold", set, tc, e.Name)
				}
			}
		}
	}
	for _, m := range all {
		if testIn[m.test] == "" {
			t.Fatalf("test %s is in no set", m.test)
		}
	}
	// Every mock the agent sent is in exactly one set: none was copied, as
	// each test's mock was made inside its own set's span.
	if len(mockIn) != len(all) {
		t.Fatalf("the sets hold %d mocks; the agent sent %d", len(mockIn), len(all))
	}
	for name, sets := range mockIn {
		if len(sets) != 1 {
			t.Fatalf("mock %s is in sets %v; want one", name, sets)
		}
	}
}

// A test in flight at the cut lands whole in the set its request began in:
// the mock it made after the cut, filed in the next set by its time, is copied
// back into the test's set before the test's mapping is written. And a test
// after the cut whose mapping names a mock made before it gets that mock
// copied into its own set.
func TestRolloverATestInFlightAtTheCutLandsWholeInOneSet(t *testing.T) {
	const limit = 4096
	rr := startRollRec(t, limit)
	tX := rollT0
	rr.mock(rollMock("x1", tX.Add(time.Millisecond), 200))
	rr.mock(rollMock("z0", tX.Add(2*time.Millisecond), 200))
	// The mock that fills the set: the cut is at its time.
	cutAt := tX.Add(10 * time.Millisecond)
	rr.mock(rollMock("fill", cutAt, 2*limit))
	rr.rolledTo(1)
	// X's call made after the cut, then X itself, which began before it.
	rr.mock(rollMock("x2", cutAt.Add(time.Millisecond), 200))
	rr.test(rollTest("test-x", tX, cutAt.Add(3*time.Millisecond)))
	// Y began after the cut, and names its own mock and one made before it.
	rr.mock(rollMock("y1", cutAt.Add(5*time.Millisecond), 200))
	rr.test(rollTest("test-y", cutAt.Add(4*time.Millisecond), cutAt.Add(8*time.Millisecond)))
	rr.mapping("test-x", "x1", "x2")
	rr.mapping("test-y", "y1", "z0")
	time.Sleep(3 * mappingFlushInterval)
	rr.stop()

	if len(rr.ro.sets) != 2 {
		t.Fatalf("sets %v; want 2", rr.ro.sets)
	}
	s0, s1 := readSet(t, rr.dir, rr.ro.sets[0]), readSet(t, rr.dir, rr.ro.sets[1])
	if fmt.Sprint(s0.tests) != "[test-x]" || fmt.Sprint(s1.tests) != "[test-y]" {
		t.Fatalf("tests: %s holds %v, %s holds %v; want test-x before the cut, test-y after", rr.ro.sets[0], s0.tests, rr.ro.sets[1], s1.tests)
	}
	x, y := s0.mappings["test-x"], s1.mappings["test-y"]
	if len(x) != 2 || len(y) != 2 {
		t.Fatalf("mappings: test-x %v, test-y %v; want two mocks each", x, y)
	}
	if _, wrong := s1.mappings["test-x"]; wrong {
		t.Fatalf("test-x is mapped in %s too", rr.ro.sets[1])
	}
	for _, c := range []struct {
		set     setOnDisk
		id      string
		entries []models.MockEntry
		other   setOnDisk
	}{{s0, rr.ro.sets[0], x, s1}, {s1, rr.ro.sets[1], y, s0}} {
		copied := 0
		for _, e := range c.entries {
			doc, ok := c.set.mocks[e.Name]
			if !ok {
				t.Fatalf("set %s maps mock %s, which its mock file does not hold", c.id, e.Name)
			}
			if orig, inOther := c.other.mocks[e.Name]; inOther {
				copied++
				if !bytes.Equal(bytes.TrimSpace(orig), bytes.TrimSpace(doc)) {
					t.Fatalf("mock %s was copied into %s changed:\n%s\nvs\n%s", e.Name, c.id, doc, orig)
				}
			}
		}
		if copied != 1 {
			t.Fatalf("set %s: %d of its test's mocks are copies; want exactly the one made across the cut", c.id, copied)
		}
	}
}

// What a replay of a later set alone needs from before it began is in it:
// the session and connection state recorded before the cut (here a
// connection's handshake), and what the application did before its first
// test, are copied into the next set as it begins.
func TestRolloverTheNextSetStartsWithTheSessionStateAndTheStartup(t *testing.T) {
	const limit = 4096
	rr := startRollRec(t, limit)
	boot := rollMock("boot", rollT0, 100)
	conn := rollMock("conn", rollT0.Add(time.Millisecond), 100)
	conn.Spec.Metadata = map[string]string{"type": "connection", "connID": "c1"}
	rr.mock(boot)
	rr.mock(conn)
	t1 := rollT0.Add(10 * time.Millisecond)
	rr.mock(rollMock("t1", t1.Add(time.Millisecond), 100))
	rr.test(rollTest("test-1", t1, t1.Add(2*time.Millisecond)))
	rr.mapping("test-1", "t1")
	// The connection's same exchange again: carried once, its latest. The
	// same exchange on another connection is another connection's state.
	again := rollMock("conn", t1.Add(3*time.Millisecond), 100)
	again.Spec.Metadata = map[string]string{"type": "connection", "connID": "c1"}
	other := rollMock("conn", t1.Add(4*time.Millisecond), 100)
	other.Spec.Metadata = map[string]string{"type": "connection", "connID": "c2"}
	rr.mock(again)
	rr.mock(other)
	rr.mock(rollMock("fill", t1.Add(5*time.Millisecond), 2*limit))
	rr.rolledTo(1)
	time.Sleep(3 * mappingFlushInterval)
	rr.stop()

	s0, s1 := readSet(t, rr.dir, rr.ro.sets[0]), readSet(t, rr.dir, rr.ro.sets[1])
	var carried []string
	for name := range s1.mocks {
		carried = append(carried, name)
		if !bytes.Equal(bytes.TrimSpace(s0.mocks[name]), bytes.TrimSpace(s1.mocks[name])) {
			t.Fatalf("mock %s was carried changed", name)
		}
	}
	sort.Strings(carried)
	// mock-0 is the startup call, mock-3 connection c1's state as last
	// recorded (mock-1 is the same exchange), mock-4 connection c2's; the
	// test's own mock and the filler stay behind.
	if fmt.Sprint(carried) != "[mock-0 mock-3 mock-4]" {
		t.Fatalf("the next set starts with %v; want the startup call and each connection's state once [mock-0 mock-3 mock-4]", carried)
	}
}

// A set the recording is past (two cuts back) keeps no index; a test of it
// that arrives late still gets the mocks it names copied in, found by
// reading the files, and a mock it already holds is not copied twice.
func TestRolloverPlaceIntoAnOldSetReadsItsFile(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store := mockdb.New(logger, dir, "mocks")
	ctx := context.Background()
	write := func(set string, m *models.Mock) string {
		if err := store.InsertMock(ctx, m, set); err != nil {
			t.Fatal(err)
		}
		return m.Name
	}
	a := write("s0", rollMock("a", rollT0, 10))
	b := write("s2", rollMock("b", rollT0.Add(time.Hour), 10))
	sr := newSetRoll(logger, &countRoller{limit: 1}, store, "s0", nil, nil)
	sr.sets = append(sr.sets,
		&rollSet{id: "s1", idx: 1, after: rollT0.Add(time.Minute), names: map[string]struct{}{}},
		&rollSet{id: "s2", idx: 2, after: rollT0.Add(30 * time.Minute), names: map[string]struct{}{b: {}}})
	sr.sets[0].names = nil // aged
	sr.format = "yaml"
	missing := sr.place(ctx, sr.sets[0], []models.MockEntry{{Name: a}, {Name: b}})
	if len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	got := readSet(t, dir, "s0")
	if len(got.mocks) != 2 {
		t.Fatalf("s0 holds %d mocks; want its own and the one copied in", len(got.mocks))
	}
}

// The limit is the set's bytes, and a set that begins with a large carry
// still takes half a limit of its own before it rolls (it would otherwise
// roll at once, on every mock).
func TestRolloverDueCountsTheSetsOwnBytes(t *testing.T) {
	sr := newSetRoll(zap.NewNop(), &countRoller{limit: 100}, mockdb.New(zap.NewNop(), t.TempDir(), "mocks"), "s0", nil, nil)
	sr.bytes = 99
	if sr.due() {
		t.Fatal("due under the limit")
	}
	sr.bytes = 100
	if !sr.due() {
		t.Fatal("not due at the limit")
	}
	sr.bytes, sr.carryAt = 140, 100
	if sr.due() {
		t.Fatal("due with less than half a limit of its own")
	}
	sr.bytes = 150
	if !sr.due() {
		t.Fatal("not due with half a limit of its own")
	}
	off := newSetRoll(zap.NewNop(), &countRoller{limit: 0}, mockdb.New(zap.NewNop(), t.TempDir(), "mocks"), "s0", nil, nil)
	off.bytes = 1 << 40
	if off.due() || off.rolls() {
		t.Fatal("a zero limit rolls over")
	}
	none := newSetRoll(zap.NewNop(), &countRoller{limit: 100}, &recMockDB{}, "s0", nil, nil)
	if none.rolls() {
		t.Fatal("a mock store that cannot copy mocks rolls over")
	}
}

// A test the agent revokes is deleted from the test set its capture was saved
// in: the one before the cut for a test that began before it, the next for
// one after, whichever set the recording writes to when the revoke comes.
func TestRolloverARevokedTestIsDeletedFromItsOwnSet(t *testing.T) {
	const limit = 4096
	rr := startRollRec(t, limit)
	tA := rollT0
	rr.mock(rollMock("a1", tA.Add(time.Millisecond), 100))
	rr.test(rollTest("test-a", tA, tA.Add(2*time.Millisecond)))
	rr.test(rollTest("test-a2", tA.Add(3*time.Millisecond), tA.Add(4*time.Millisecond)))
	cutAt := tA.Add(10 * time.Millisecond)
	rr.mock(rollMock("fill", cutAt, 2*limit))
	rr.rolledTo(1)
	tB := cutAt.Add(5 * time.Millisecond)
	rr.mock(rollMock("b1", tB.Add(time.Millisecond), 100))
	rr.test(rollTest("test-b", tB, tB.Add(2*time.Millisecond)))
	rr.test(rollTest("test-b2", tB.Add(3*time.Millisecond), tB.Add(4*time.Millisecond)))
	file := func(set, test string) string { return filepath.Join(rr.dir, set, "tests", test+".yaml") }
	sets := func() []string {
		rr.ro.mu.Lock()
		defer rr.ro.mu.Unlock()
		return append([]string(nil), rr.ro.sets...)
	}
	s := sets()
	waitFile := func(path string, present bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, err := os.Stat(path)
			if (err == nil) == present {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s present=%v; want %v", path, err == nil, present)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFile(file(s[0], "test-a"), true)
	waitFile(file(s[1], "test-b"), true)
	select {
	case rr.f.outgoing <- liveRevokeFrame("test-a,test-b"):
	case <-time.After(10 * time.Second):
		t.Fatal("the recorder never took the revoke")
	}
	waitFile(file(s[0], "test-a"), false)
	waitFile(file(s[1], "test-b"), false)
	rr.stop()
	for _, f := range []string{file(s[0], "test-a2"), file(s[1], "test-b2")} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("a test nobody revoked is gone: %v", err)
		}
	}
}

// After a cut, a mapping that arrives before its test case waits for it: the
// test's set is the one its request time names, which only its test case
// tells, and the mapping is written there (with its mocks copied in), not to
// the set the recording is writing to.
func TestRolloverAMappingBeforeItsTestCaseWaitsForIt(t *testing.T) {
	const limit = 4096
	rr := startRollRec(t, limit)
	tX := rollT0
	rr.mock(rollMock("x1", tX.Add(time.Millisecond), 200))
	cutAt := tX.Add(10 * time.Millisecond)
	rr.mock(rollMock("fill", cutAt, 2*limit))
	rr.rolledTo(1)
	rr.mock(rollMock("x2", cutAt.Add(time.Millisecond), 200))
	rr.mapping("test-x", "x1", "x2")
	// Two of the consumer's flushes with the mapping held.
	time.Sleep(2 * mappingFlushInterval)
	rr.test(rollTest("test-x", tX, cutAt.Add(3*time.Millisecond)))
	time.Sleep(3 * mappingFlushInterval)
	rr.stop()

	s0, s1 := readSet(t, rr.dir, rr.ro.sets[0]), readSet(t, rr.dir, rr.ro.sets[1])
	if len(s0.mappings["test-x"]) != 2 {
		t.Fatalf("%s maps test-x to %v; want both its mocks", rr.ro.sets[0], s0.mappings["test-x"])
	}
	if _, wrong := s1.mappings["test-x"]; wrong {
		t.Fatalf("test-x is mapped in %s, the set the recording was writing to", rr.ro.sets[1])
	}
	for _, e := range s0.mappings["test-x"] {
		if _, ok := s0.mocks[e.Name]; !ok {
			t.Fatalf("%s maps mock %s, which its mock file does not hold", rr.ro.sets[0], e.Name)
		}
	}
}

// failingRoller fails the first n rollovers.
type failingRoller struct {
	countRoller
	fails int
}

func (ro *failingRoller) Next(ctx context.Context, cur string) (string, error) {
	ro.mu.Lock()
	if ro.fails > 0 {
		ro.fails--
		ro.mu.Unlock()
		return "", errors.New("no next set")
	}
	ro.mu.Unlock()
	return ro.countRoller.Next(ctx, cur)
}

// A rollover that fails leaves the recording in its set, and is tried again
// once the set has taken another half limit: not on every mock after it, and
// not never.
func TestRolloverAFailedCutIsTriedAgainAfterHalfALimit(t *testing.T) {
	ro := &failingRoller{countRoller: countRoller{limit: 1000}, fails: 1}
	sr := newSetRoll(zap.NewNop(), ro, mockdb.New(zap.NewNop(), t.TempDir(), "mocks"), "s0", nil, nil)
	sr.format = "yaml"
	sr.bytes = 1000
	if !sr.due() {
		t.Fatal("not due at the limit")
	}
	sr.cut(context.Background())
	if len(sr.all()) != 1 {
		t.Fatal("a failed rollover made a set")
	}
	sr.bytes = 1400
	if sr.due() {
		t.Fatal("due again before another half limit")
	}
	sr.bytes = 1500
	if !sr.due() {
		t.Fatal("not due after another half limit")
	}
	sr.cut(context.Background())
	if got := sr.all(); len(got) != 2 || got[1].id != "s0-s1" {
		t.Fatalf("sets after the second try: %d", len(got))
	}
}

// The calls the agent places in no test come under the mapping name "": each
// goes to the set its own time names, as it is mapped, and none waits for a
// test case. A mapping of a name no test case comes under waits two
// rollovers, then goes where its calls' times say.
func TestRolloverCallsNoTestOwnsGoWhereTheirTimeSays(t *testing.T) {
	const limit = 4096
	rr := startRollRec(t, limit)
	rr.mock(rollMock("a", rollT0.Add(time.Millisecond), 100))
	cut1 := rollT0.Add(10 * time.Millisecond)
	rr.mock(rollMock("fill1", cut1, 2*limit))
	rr.rolledTo(1)
	rr.mock(rollMock("b", cut1.Add(time.Millisecond), 100))
	rr.mock(rollMock("c", cut1.Add(2*time.Millisecond), 100))
	rr.mapping("", "a", "b")
	rr.mapping("ghost", "c")
	time.Sleep(3 * mappingFlushInterval)
	s0, s1 := readSet(t, rr.dir, rr.ro.sets[0]), readSet(t, rr.dir, rr.ro.sets[1])
	if len(s0.mappings[""]) != 1 || len(s1.mappings[""]) != 1 {
		t.Fatalf("while recording, the calls no test owns are mapped %v in %s and %v in %s; want each in the set of its time", s0.mappings[""], rr.ro.sets[0], s1.mappings[""], rr.ro.sets[1])
	}
	if _, early := s1.mappings["ghost"]; early {
		t.Fatal("a mapping of a test case that may still come was written before it could")
	}
	cut2 := cut1.Add(10 * time.Millisecond)
	rr.mock(rollMock("fill2", cut2, 2*limit))
	rr.rolledTo(2)
	cut3 := cut2.Add(10 * time.Millisecond)
	rr.mock(rollMock("fill3", cut3, 2*limit))
	rr.rolledTo(3)
	time.Sleep(3 * mappingFlushInterval)
	s1 = readSet(t, rr.dir, rr.ro.sets[1])
	if len(s1.mappings["ghost"]) != 1 {
		t.Fatalf("two rollovers on, the mapping of a test case that never came is %v in %s; want it in the set of its call's time", s1.mappings["ghost"], rr.ro.sets[1])
	}
	rr.stop()
}

// A test in flight at the cut that the agent maps to no test still lands
// whole in its set: the calls it made after the cut are copied into it, those
// written before its test case came and those written after.
func TestRolloverAnUnmappedTestInFlightAtTheCutLandsWholeInOneSet(t *testing.T) {
	const limit = 4096
	rr := startRollRec(t, limit)
	tX := rollT0
	rr.mock(rollMock("x1", tX.Add(time.Millisecond), 200))
	cutAt := tX.Add(10 * time.Millisecond)
	rr.mock(rollMock("fill", cutAt, 2*limit))
	rr.rolledTo(1)
	rr.mock(rollMock("x2", cutAt.Add(time.Millisecond), 200))
	rr.test(rollTest("test-x", tX, cutAt.Add(5*time.Millisecond)))
	waitForSavedTest(t, rr.dir, rr.ro.sets[0], "test-x")
	rr.mock(rollMock("x3", cutAt.Add(4*time.Millisecond), 200))
	// After test-x's response: not test-x's.
	rr.mock(rollMock("y1", cutAt.Add(8*time.Millisecond), 200))
	time.Sleep(3 * mappingFlushInterval)
	rr.stop()

	s0, s1 := readSet(t, rr.dir, rr.ro.sets[0]), readSet(t, rr.dir, rr.ro.sets[1])
	urls := func(s setOnDisk) []string {
		var out []string
		for _, doc := range s.mocks {
			for _, line := range strings.Split(string(doc), "\n") {
				if strings.Contains(line, "url: http://dep.local/") {
					out = append(out, strings.TrimPrefix(strings.TrimSpace(line), "url: http://dep.local/"))
				}
			}
		}
		sort.Strings(out)
		return out
	}
	if got := fmt.Sprint(urls(s0)); got != "[fill x1 x2 x3]" {
		t.Fatalf("%s holds %s; want test-x's calls before and after the cut [fill x1 x2 x3]", rr.ro.sets[0], got)
	}
	if got := fmt.Sprint(urls(s1)); got != "[x2 x3 y1]" {
		t.Fatalf("%s holds %s; want the calls of its time [x2 x3 y1]", rr.ro.sets[1], got)
	}
}

// A test case that comes with no name is saved under the name the store gives
// it, and a mapping by that name goes to its set.
func TestRolloverAMappingByTheSavedNameGoesToItsSet(t *testing.T) {
	const limit = 4096
	rr := startRollRec(t, limit)
	rr.mock(rollMock("pre", rollT0, 100))
	cutAt := rollT0.Add(10 * time.Millisecond)
	rr.mock(rollMock("fill", cutAt, 2*limit))
	rr.rolledTo(1)
	tY := cutAt.Add(5 * time.Millisecond)
	rr.mock(rollMock("y1", tY.Add(time.Millisecond), 100))
	tc := rollTest("", tY, tY.Add(2*time.Millisecond))
	rr.test(tc)
	name := waitForAnyTest(t, rr.dir, rr.ro.sets[1])
	rr.mapping(name, "y1")
	time.Sleep(3 * mappingFlushInterval)
	s1 := readSet(t, rr.dir, rr.ro.sets[1])
	if len(s1.mappings[name]) != 1 {
		t.Fatalf("the mapping of %s (the name the store gave it) is %v in %s while recording; want its call", name, s1.mappings[name], rr.ro.sets[1])
	}
	rr.stop()
}

func waitForSavedTest(t *testing.T, dir, set, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, set, "tests", name+".yaml")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("test %s was not saved in %s", name, set)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForAnyTest(t *testing.T, dir, set string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, _ := os.ReadDir(filepath.Join(dir, set, "tests"))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".yaml") {
				return strings.TrimSuffix(e.Name(), ".yaml")
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no test was saved in %s", set)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// blockingNextRoller blocks Roller.Next until release is closed, signalling
// entered once Next is in flight.
type blockingNextRoller struct {
	countRoller
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (ro *blockingNextRoller) Next(ctx context.Context, cur string) (string, error) {
	ro.once.Do(func() {
		close(ro.entered)
		<-ro.release
	})
	return ro.countRoller.Next(ctx, cur)
}

func startRollRecWith(t *testing.T, ro Roller) *rollRec {
	t.Helper()
	return startRollRecWithLogger(t, ro, zap.NewNop())
}

func startRollRecWithLogger(t *testing.T, ro Roller, logger *zap.Logger) *rollRec {
	t.Helper()
	dir := t.TempDir()
	rr := &rollRec{
		t:   t,
		dir: dir,
		f: &fakeInstr{
			mappings: make(chan models.TestMockMapping),
			incoming: make(chan *models.TestCase),
			outgoing: make(chan *models.Mock),
		},
		hooks: insertedHooks{mocks: make(chan string, 1024)},
		done:  make(chan error, 1),
	}
	switch v := ro.(type) {
	case *countRoller:
		rr.ro = v
	case *blockingNextRoller:
		rr.ro = &v.countRoller
	case *blockingNthNextRoller:
		rr.ro = &v.countRoller
	}
	r := New(logger, testdb.New(logger, dir), mockdb.New(logger, dir, "mocks"), mapdb.New(logger, dir, "mappings"),
		&recTelemetry{}, &blockingInstr{rr.f}, recTestSetConf{}, rr.hooks, &config.Config{}).(*Recorder)
	r.SetRoller(ro)
	ctx, cancel := context.WithCancel(context.Background())
	rr.cancel = cancel
	go func() { rr.done <- r.Start(ctx) }()
	return rr
}

func mockURLs(s setOnDisk) []string {
	var out []string
	for _, doc := range s.mocks {
		for _, line := range strings.Split(string(doc), "\n") {
			if strings.Contains(line, "url: http://dep.local/") {
				out = append(out, strings.TrimPrefix(strings.TrimSpace(line), "url: http://dep.local/"))
			}
		}
	}
	sort.Strings(out)
	return out
}

// Replicates the incoming-ahead-of-outgoing straddler race: the test case
// arrives and finishes InsertTestCase + roll.straddled() BEFORE the lagging
// mock goroutine reaches the filling mock and triggers roll.cut(). Its calls
// made after cutAt (up to its response) must still be copied into the earlier
// set once cut() commits.
func TestRolloverStraddlingTestArrivingBeforeCutGetsItsPostCutMocks(t *testing.T) {
	const limit = 4096
	rr := startRollRec(t, limit)
	tX := rollT0
	cutAt := tX.Add(10 * time.Millisecond)
	rr.mock(rollMock("x1", tX.Add(time.Millisecond), 200))
	// Deliver and persist test-x BEFORE fill triggers cut().
	rr.test(rollTest("test-x", tX, cutAt.Add(5*time.Millisecond)))
	waitForSavedTest(t, rr.dir, "test-set-0", "test-x")
	// Now the lagging mock loop hits the filling mock at cutAt and rolls over.
	rr.mock(rollMock("fill", cutAt, 2*limit))
	rr.rolledTo(1)
	// Post-cut calls made during test-x's window, plus one after test-x's response.
	rr.mock(rollMock("x2", cutAt.Add(2*time.Millisecond), 200))
	rr.mock(rollMock("x3", cutAt.Add(4*time.Millisecond), 200))
	rr.mock(rollMock("y1", cutAt.Add(8*time.Millisecond), 200))
	time.Sleep(3 * mappingFlushInterval)
	rr.stop()

	s0, s1 := readSet(t, rr.dir, rr.ro.sets[0]), readSet(t, rr.dir, rr.ro.sets[1])
	if got := fmt.Sprint(mockURLs(s0)); got != "[fill x1 x2 x3]" {
		t.Fatalf("%s holds %s; want test-x's post-cut calls copied back [fill x1 x2 x3]", rr.ro.sets[0], got)
	}
	if got := fmt.Sprint(mockURLs(s1)); got != "[x2 x3 y1]" {
		t.Fatalf("%s holds %s; want [x2 x3 y1]", rr.ro.sets[1], got)
	}
}

// Replicates the unlocked cut() I/O window race: a straddling test case arrives
// and completes InsertTestCase + roll.straddled() WHILE Roller.Next() is in
// flight (after cut() snapshotted cutAt and unlocked sr.mu, before the new set
// is appended to sr.sets). Its post-cut mocks must still be copied back.
func TestRolloverStraddlingTestArrivingDuringNextGetsItsPostCutMocks(t *testing.T) {
	const limit = 4096
	br := &blockingNextRoller{
		countRoller: countRoller{limit: limit},
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	rr := startRollRecWith(t, br)
	tX := rollT0
	cutAt := tX.Add(10 * time.Millisecond)
	rr.mock(rollMock("x1", tX.Add(time.Millisecond), 200))
	rr.mock(rollMock("fill", cutAt, 2*limit))
	// Wait until cut() has entered Roller.Next() (sr.mu is unlocked).
	select {
	case <-br.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("cut() never entered Roller.Next()")
	}
	// Deliver and save test-x while Roller.Next() is blocked.
	rr.test(rollTest("test-x", tX, cutAt.Add(5*time.Millisecond)))
	waitForSavedTest(t, rr.dir, "test-set-0", "test-x")
	// Release Roller.Next() so cut() commits the new set.
	close(br.release)
	rr.rolledTo(1)
	// Post-cut mocks of test-x arrive in the new set.
	rr.mock(rollMock("x2", cutAt.Add(2*time.Millisecond), 200))
	rr.mock(rollMock("y1", cutAt.Add(8*time.Millisecond), 200))
	time.Sleep(3 * mappingFlushInterval)
	rr.stop()

	s0, s1 := readSet(t, rr.dir, rr.ro.sets[0]), readSet(t, rr.dir, rr.ro.sets[1])
	if got := fmt.Sprint(mockURLs(s0)); got != "[fill x1 x2]" {
		t.Fatalf("%s holds %s; want test-x's post-cut call copied back [fill x1 x2]", rr.ro.sets[0], got)
	}
	if got := fmt.Sprint(mockURLs(s1)); got != "[x2 y1]" {
		t.Fatalf("%s holds %s; want [x2 y1]", rr.ro.sets[1], got)
	}
}

// flakyReadStore fails the first n ReadMockDocs calls, then delegates.
type flakyReadStore struct {
	mockDocStore
	fails int
}

func (f *flakyReadStore) ReadMockDocs(ctx context.Context, testSetID string, names map[string]struct{}) (map[string][]byte, string, error) {
	if f.fails > 0 {
		f.fails--
		return nil, "", errors.New("transient read error")
	}
	return f.mockDocStore.ReadMockDocs(ctx, testSetID, names)
}

// If ReadMockDocs transiently fails on the first cut, startup mocks must stay
// queued and be carried on the next cut instead of being permanently lost.
func TestRolloverTransientStartupReadFailureRetriesOnNextCut(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	base := mockdb.New(logger, dir, "mocks")
	ctx := context.Background()
	boot := rollMock("boot", rollT0, 100)
	if err := base.InsertMock(ctx, boot, "s0"); err != nil {
		t.Fatal(err)
	}
	flaky := &flakyReadStore{mockDocStore: base, fails: 1}
	ro := &countRoller{limit: 1000}
	sr := newSetRoll(logger, ro, flaky, "s0", nil, nil)
	sr.format = "yaml"
	sr.firstTest = rollT0.Add(10 * time.Millisecond)
	sr.startup = []carried{{name: boot.Name, at: rollT0}}
	sr.latest = rollT0.Add(20 * time.Millisecond)
	sr.bytes = 1000
	sr.cut(ctx)
	// Second cut: transient error is gone, so boot must be read, backfilled
	// into s0-s1, and carried into s0-s2.
	sr.latest = rollT0.Add(30 * time.Millisecond)
	sr.bytes = 2000
	sr.cut(ctx)
	s1 := readSet(t, dir, "s0-s1")
	if _, ok := s1.mocks[boot.Name]; !ok {
		t.Fatalf("s0-s1 mocks = %v; want startup mock %s backfilled after transient ReadMockDocs error", s1.mocks, boot.Name)
	}
	s2 := readSet(t, dir, "s0-s2")
	if _, ok := s2.mocks[boot.Name]; !ok {
		t.Fatalf("s0-s2 mocks = %v; want startup mock %s carried after transient ReadMockDocs error", s2.mocks, boot.Name)
	}
}

// mapLimitRoller implements both Roller and MappingLimiter.
type mapLimitRoller struct {
	countRoller
	mapLimit int64
}

func (ro *mapLimitRoller) MappingLimit() int64 { return ro.mapLimit }

// A Roller implementing MappingLimiter rolls a test set over when estimated
// mapping bytes reach MappingLimit() even though raw mock bytes are far below
// Limit(), and respects the half-limit own-bytes and retry-backoff rules.
func TestRolloverMappingLimitRollsBeforeByteLimit(t *testing.T) {
	ro := &mapLimitRoller{countRoller: countRoller{limit: 1 << 30}, mapLimit: 1000}
	dir := t.TempDir()
	logger := zap.NewNop()
	store := mockdb.New(logger, dir, "mocks")
	sr := newSetRoll(logger, ro, store, "s0", nil, nil)
	sr.format = "yaml"
	// 4 mocks (~200 B mapping estimate each = ~800 B < 1000): not due yet.
	for i := 0; i < 4; i++ {
		m := rollMock(fmt.Sprintf("m-%d", i), rollT0.Add(time.Duration(i)*time.Millisecond), 10)
		m.Name = fmt.Sprintf("mock-%d", i)
		sr.wrote(context.Background(), sr.last(), m, []byte("doc"), "yaml", nil, nil)
	}
	if sr.due() {
		t.Fatalf("due before MappingLimit (wroteMapBytes=%d, mapLimit=%d)", sr.wroteMapBytes, sr.mapLimit)
	}
	// 5th and 6th push wroteMapBytes past 1000 while raw bytes stay tiny (< 50 B vs 1 GiB).
	for i := 4; i < 6; i++ {
		m := rollMock(fmt.Sprintf("m-%d", i), rollT0.Add(time.Duration(i)*time.Millisecond), 10)
		m.Name = fmt.Sprintf("mock-%d", i)
		sr.wrote(context.Background(), sr.last(), m, []byte("doc"), "yaml", nil, nil)
	}
	if !sr.due() {
		t.Fatalf("not due past MappingLimit (wroteMapBytes=%d, mapLimit=%d)", sr.wroteMapBytes, sr.mapLimit)
	}
	sr.cut(context.Background())
	if got := len(sr.all()); got != 2 {
		t.Fatalf("sets after mapping-limit cut = %d; want 2", got)
	}
	if sr.due() {
		t.Fatal("still due right after cut")
	}
}

// copyIn deduplicates against both open sets' in-memory names and aged sets'
// mocks.yaml on disk so concurrent wrote()/straddled()/place() never duplicate
// a mock document.
func TestRolloverCopyInDeduplicatesOpenAndAgedSets(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store := mockdb.New(logger, dir, "mocks")
	ctx := context.Background()
	m := rollMock("dup", rollT0, 50)
	if err := store.InsertMock(ctx, m, "s0"); err != nil {
		t.Fatal(err)
	}
	docs, format, err := store.ReadMockDocs(ctx, "s0", map[string]struct{}{m.Name: {}})
	if err != nil {
		t.Fatal(err)
	}
	sr := newSetRoll(logger, &countRoller{limit: 1000}, store, "s0", nil, nil)
	sr.format = format
	sr.sets[0].names[m.Name] = struct{}{}
	// Open set: copyIn skips mock already in s0.names.
	sr.copyIn(ctx, sr.sets[0], format, docs)
	// Aged set (names == nil): copyIn checks disk via ReadMockDocs and skips.
	sr.sets[0].names = nil
	sr.copyIn(ctx, sr.sets[0], format, docs)
	// readSet fails the test if any mock appears twice in mocks.yaml.
	got := readSet(t, dir, "s0")
	if len(got.mocks) != 1 {
		t.Fatalf("s0 holds %d mocks; want 1", len(got.mocks))
	}
}

// Connection churn with distinct connIDs stays bounded in sr.carry at
// maxCarryState while preserving worker marks and startup mocks.
func TestRolloverCarryStateIsBoundedUnderConnectionChurn(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	sr := newSetRoll(logger, &countRoller{limit: 1 << 30}, mockdb.New(logger, dir, "mocks"), "s0", nil, nil)
	ctx := context.Background()
	for i := 0; i < maxCarryState+50; i++ {
		m := rollMock(fmt.Sprintf("c-%d", i), rollT0.Add(time.Duration(i)*time.Millisecond), 10)
		m.Name = fmt.Sprintf("mock-%d", i)
		m.Spec.Metadata = map[string]string{"type": "connection", "connID": fmt.Sprintf("conn-%d", i)}
		doc := []byte(fmt.Sprintf("name: %s\nconnID: conn-%d\n", m.Name, i))
		sr.wrote(ctx, sr.last(), m, doc, "yaml", nil, nil)
	}
	if len(sr.carryState) > maxCarryState || len(sr.carry) > maxCarryState {
		t.Fatalf("carry grew unbounded: len(carry)=%d, len(carryState)=%d, want <= %d", len(sr.carry), len(sr.carryState), maxCarryState)
	}
}

// When the agent streams mappings itself (sr.streamed() == true), aging a set
// out two cuts later clears its windowMappings so historical sets do not leak
// WindowedMocks indefinitely.
func TestRolloverAgedSetClearsWindowsWhenMappingsStreamed(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store := mockdb.New(logger, dir, "mocks")
	w0 := &windowMappings{}
	w0.addMock(rollMock("m0", rollT0, 10))
	sr := newSetRoll(logger, &countRoller{limit: 100}, store, "s0", w0, nil)
	sr.format = "yaml"
	sr.streamed = func() bool { return true }
	sr.newWindows = func() *windowMappings { return &windowMappings{} }
	sr.latest = rollT0.Add(time.Millisecond)
	sr.bytes = 100
	sr.cut(context.Background())
	sr.latest = rollT0.Add(2 * time.Millisecond)
	sr.bytes = 100
	sr.cut(context.Background())
	// s0 is now 2 cuts behind (len(sets) == 3, old == 0): its windows must be cleared.
	w0.mu.Lock()
	left := len(w0.mocks)
	w0.mu.Unlock()
	if left != 0 {
		t.Fatalf("aged set kept %d WindowedMocks after streamed rollover; want 0", left)
	}
}

// Replicates the outgoing-ahead-of-incoming startup race: the first set rolls
// over BEFORE the first test case arrives on the incoming stream (for example,
// the first test is still in flight or the incoming goroutine lags behind the
// outgoing mock goroutine). Once the first test arrives, startup mocks recorded
// before its request time must be backfilled into the already-rolled set and
// carried into subsequent sets.
func TestRolloverStartupBackfilledWhenFirstTestArrivesAfterCut(t *testing.T) {
	const limit = 4096
	rr := startRollRec(t, limit)
	boot := rollMock("boot", rollT0, 100)
	conn := rollMock("conn", rollT0.Add(time.Millisecond), 100)
	conn.Spec.Metadata = map[string]string{"type": "connection", "connID": "c1"}
	rr.mock(boot)
	rr.mock(conn)
	t1 := rollT0.Add(10 * time.Millisecond)
	rr.mock(rollMock("t1", t1.Add(time.Millisecond), 100))
	rr.mock(rollMock("fill", t1.Add(5*time.Millisecond), 2*limit))
	// Cut commits while sr.firstTest is still zero.
	rr.rolledTo(1)
	// First test arrives AFTER cut 1 has already committed.
	rr.test(rollTest("test-1", t1, t1.Add(2*time.Millisecond)))
	waitForSavedTest(t, rr.dir, rr.ro.sets[0], "test-1")
	// Startup must already be backfilled into s1 immediately (before any
	// second rollover occurs).
	s0Early := readSet(t, rr.dir, rr.ro.sets[0])
	s1Early := readSet(t, rr.dir, rr.ro.sets[1])
	var earlyGot []string
	for name := range s1Early.mocks {
		if _, in0 := s0Early.mocks[name]; in0 {
			earlyGot = append(earlyGot, name)
		}
	}
	sort.Strings(earlyGot)
	if fmt.Sprint(earlyGot) != "[mock-0 mock-1]" {
		t.Fatalf("s1 immediately after first test arrived holds %v; want [mock-0 mock-1]", earlyGot)
	}
	// Trigger a second cut to prove startup is also in sr.carry for later sets.
	rr.mock(rollMock("fill2", t1.Add(20*time.Millisecond), 2*limit))
	rr.rolledTo(2)
	rr.stop()

	s0 := readSet(t, rr.dir, rr.ro.sets[0])
	for _, id := range rr.ro.sets[1:] {
		s := readSet(t, rr.dir, id)
		var got []string
		for name := range s.mocks {
			if _, in0 := s0.mocks[name]; in0 {
				got = append(got, name)
				if !bytes.Equal(bytes.TrimSpace(s0.mocks[name]), bytes.TrimSpace(s.mocks[name])) {
					t.Fatalf("set %s: mock %s was carried changed", id, name)
				}
			}
		}
		sort.Strings(got)
		if fmt.Sprint(got) != "[mock-0 mock-1]" {
			t.Fatalf("set %s carried %v from s0; want startup + connection state [mock-0 mock-1]", id, got)
		}
	}
}

// Replicates the unlocked cut() I/O window race for startup mocks: the first
// test arrives WHILE Roller.Next() is in flight (after cut() saw sr.firstTest
// was zero and unlocked sr.mu, before the new set is appended to sr.sets).
// cut() must resolve and backfill startup into the new set before Rolled().
func TestRolloverStartupBackfilledWhenFirstTestArrivesDuringNext(t *testing.T) {
	const limit = 4096
	br := &blockingNextRoller{
		countRoller: countRoller{limit: limit},
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	rr := startRollRecWith(t, br)
	boot := rollMock("boot", rollT0, 100)
	conn := rollMock("conn", rollT0.Add(time.Millisecond), 100)
	conn.Spec.Metadata = map[string]string{"type": "connection", "connID": "c1"}
	rr.mock(boot)
	rr.mock(conn)
	t1 := rollT0.Add(10 * time.Millisecond)
	rr.mock(rollMock("t1", t1.Add(time.Millisecond), 100))
	rr.mock(rollMock("fill", t1.Add(5*time.Millisecond), 2*limit))
	// Wait until cut() is inside Roller.Next() (sr.mu unlocked, len(sets)==1).
	select {
	case <-br.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("cut() never entered Roller.Next()")
	}
	// Deliver and persist test-1 while Roller.Next() is blocked.
	rr.test(rollTest("test-1", t1, t1.Add(2*time.Millisecond)))
	waitForSavedTest(t, rr.dir, "test-set-0", "test-1")
	// Release Roller.Next() and wait for Rolled().
	close(br.release)
	rr.rolledTo(1)
	// Startup must already be present in s1 as soon as Rolled() fires.
	s0 := readSet(t, rr.dir, rr.ro.sets[0])
	s1 := readSet(t, rr.dir, rr.ro.sets[1])
	var carried []string
	for name := range s1.mocks {
		carried = append(carried, name)
		if !bytes.Equal(bytes.TrimSpace(s0.mocks[name]), bytes.TrimSpace(s1.mocks[name])) {
			t.Fatalf("mock %s was carried changed", name)
		}
	}
	sort.Strings(carried)
	if fmt.Sprint(carried) != "[mock-0 mock-1]" {
		t.Fatalf("s1 immediately after Rolled() holds %v; want [mock-0 mock-1]", carried)
	}
	rr.stop()
}

// Replicates startup spanning a rollover (within the sr.limit startup budget,
// before the first test arrives): boot0 and boot1 live in s0 (together >
// limit/2 so backfilling them into s1 also pins asCarry byte accounting) and
// boot2 lives in s1. Both s1 and s2 must hold every startup mock once, without
// duplicating mocks already native to s1 or triggering a premature rollover.
func TestRolloverStartupSpanningCutAndLateCarryBackfillsAllSets(t *testing.T) {
	const limit = 6000
	rr := startRollRec(t, limit)
	// boot0 (~1.8 KB) + connection state (~3.6 KB) + boot1 (~1.8 KB) exceeds
	// limit (6000 B) and rolls s0 -> s1 while boot0+boot1 (~3.6 KB) stays
	// under limit and exceeds limit/2 (3000 B).
	rr.mock(rollMock("boot0", rollT0, 1200))
	conn := rollMock("conn", rollT0.Add(time.Millisecond), 3000)
	conn.Spec.Metadata = map[string]string{"type": "connection", "connID": "c1"}
	rr.mock(conn)
	rr.mock(rollMock("boot1", rollT0.Add(2*time.Millisecond), 1200))
	rr.rolledTo(1)
	// Startup mock 3 written into s1, still before the first test. Total
	// startup (~4.2 KB) remains within the 6000 B startup carry budget.
	rr.mock(rollMock("boot2", rollT0.Add(5*time.Millisecond), 100))
	// First test arrives after cut 1, with request time after boot2.
	t1 := rollT0.Add(10 * time.Millisecond)
	rr.test(rollTest("test-1", t1, t1.Add(2*time.Millisecond)))
	waitForSavedTest(t, rr.dir, rr.ro.sets[1], "test-1")
	// Backfilling boot0+boot1 (> limit/2) into s1 must count as carried bytes,
	// so small new mocks in s1 must not trigger a premature rollover.
	// Sending small2 synchronizes past small's roll.wrote/due/cut check.
	rr.mock(rollMock("small", t1.Add(3*time.Millisecond), 10))
	rr.mock(rollMock("small2", t1.Add(4*time.Millisecond), 10))
	if got := rr.ro.rolled(); got != 1 {
		t.Fatalf("backfilling startup into s1 triggered a premature rollover (rolled=%d; want 1)", got)
	}
	// Fill s1 to roll into s2; s2 must carry all three startup mocks.
	rr.mock(rollMock("fill2", t1.Add(5*time.Millisecond), 2*limit))
	rr.rolledTo(2)
	rr.stop()

	s1 := readSet(t, rr.dir, rr.ro.sets[1])
	s2 := readSet(t, rr.dir, rr.ro.sets[2])
	for _, want := range []string{"mock-0", "mock-2", "mock-3"} {
		if _, ok := s1.mocks[want]; !ok {
			t.Fatalf("s1 missing startup mock %s (has %v)", want, mockURLs(s1))
		}
		if _, ok := s2.mocks[want]; !ok {
			t.Fatalf("s2 missing startup mock %s (has %v)", want, mockURLs(s2))
		}
	}
}

// blockingNthNextRoller blocks the target (1-based) Roller.Next call until
// release is closed, signalling entered once that Next call is in flight.
type blockingNthNextRoller struct {
	countRoller
	target  int
	entered chan struct{}
	release chan struct{}
}

func (ro *blockingNthNextRoller) Next(ctx context.Context, cur string) (string, error) {
	ro.mu.Lock()
	call := ro.n + 1
	ro.mu.Unlock()
	if call == ro.target {
		close(ro.entered)
		<-ro.release
	}
	return ro.countRoller.Next(ctx, cur)
}

// Replicates the second-cut concurrent resolve race: s0 rolls to s1 while
// firstTest is still zero, then s1 enters Roller.Next() for s2 (carrying no
// startup yet). While s2's Next() is blocked, test-1 arrives, resolves startup
// into sr.carry, and backfills sr.sets[1:] (which only has s1 so far). When
// s2's cut() resumes and appends s2, its lateStartup check must backfill s2
// before Rolled() fires.
func TestRolloverLateStartupResolvedDuringSecondCutNextBackfillsNewSet(t *testing.T) {
	const limit = 4096
	br := &blockingNthNextRoller{
		countRoller: countRoller{limit: limit},
		target:      2,
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	rr := startRollRecWith(t, br)

	rr.mock(rollMock("boot", rollT0, 100))
	t1 := rollT0.Add(10 * time.Millisecond)
	rr.mock(rollMock("fill1", t1.Add(5*time.Millisecond), 2*limit))
	rr.rolledTo(1)
	// Trigger cut 2 while firstTest is still zero; cut 2 blocks in Next().
	rr.mock(rollMock("fill2", t1.Add(15*time.Millisecond), 2*limit))
	select {
	case <-br.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("cut 2 never entered Roller.Next()")
	}
	// While cut 2 is blocked in Next(), test-1 arrives, resolves startup into
	// sr.carry, and backfills s1 (s2 is not in sr.sets yet).
	rr.test(rollTest("test-1", t1, t1.Add(2*time.Millisecond)))
	waitForSavedTest(t, rr.dir, rr.ro.sets[0], "test-1")
	// Release cut 2; lateStartup must backfill s2 before Rolled() fires.
	close(br.release)
	rr.rolledTo(2)
	s2 := readSet(t, rr.dir, rr.ro.sets[2])
	if _, ok := s2.mocks["mock-0"]; !ok {
		t.Fatalf("s2 immediately after Rolled() missing startup mock-0 (has %v)", mockURLs(s2))
	}
	rr.stop()
	s1 := readSet(t, rr.dir, rr.ro.sets[1])
	if _, ok := s1.mocks["mock-0"]; !ok {
		t.Fatalf("s1 missing startup mock-0 (has %v)", mockURLs(s1))
	}
}

// Three or more cuts before the first test must cap carried startup mocks to
// sr.limit bytes, emit a Warn with next_step once, and avoid backfilling
// earlier sets that never receive a test. When a long-running first test's
// request time falls inside an aged set (s1 with s1.names==nil), aged s1 and
// every set after it still receive the startup mocks without duplicates.
func TestRolloverStartupCarryBoundedAcrossMultipleCuts(t *testing.T) {
	t.Run("skips sets that never receive a test and caps carried bytes", func(t *testing.T) {
		const limit = 4096
		core, logs := observer.New(zapcore.WarnLevel)
		ro := &countRoller{limit: limit}
		rr := startRollRecWithLogger(t, ro, zap.New(core))
		// Emit startup mocks of ~2.1 KB each until 3 cuts occur before test-1
		// (so s0..s3 exist and total pre-first-test bytes vastly exceed limit).
		for i := 0; i < 20 && rr.ro.rolled() < 3; i++ {
			rr.mock(rollMock(fmt.Sprintf("boot-%02d", i), rollT0.Add(time.Duration(i)*time.Millisecond), 1500))
		}
		rr.rolledTo(3)
		// First test arrives after 3 cuts with request time in the newest set.
		t1 := rollT0.Add(50 * time.Millisecond)
		rr.test(rollTest("test-1", t1, t1.Add(2*time.Millisecond)))
		rr.ro.mu.Lock()
		lastSet := rr.ro.sets[len(rr.ro.sets)-1]
		rr.ro.mu.Unlock()
		waitForSavedTest(t, rr.dir, lastSet, "test-1")
		rr.stop()

		// Total startup bytes carried into the test's set from all earlier sets
		// must stay within sr.limit, and the earliest startup mock must be present.
		earlier := map[string]struct{}{}
		for _, id := range rr.ro.sets[:len(rr.ro.sets)-1] {
			s := readSet(t, rr.dir, id)
			for name := range s.mocks {
				earlier[name] = struct{}{}
			}
		}
		lastDisk := readSet(t, rr.dir, lastSet)
		if _, ok := lastDisk.mocks["mock-0"]; !ok {
			t.Fatalf("last set %s missing earliest startup mock-0", lastSet)
		}
		var totalCarried int64
		for name, doc := range lastDisk.mocks {
			if _, fromEarlier := earlier[name]; fromEarlier {
				totalCarried += int64(len(doc))
			}
		}
		if totalCarried > limit {
			t.Fatalf("last set %s carried %d startup bytes from earlier sets; want <= %d", lastSet, totalCarried, limit)
		}

		// Sets before firstTest that never receive a test (s1, s2, ...) are
		// not backfilled with mock-0.
		for _, id := range rr.ro.sets[1 : len(rr.ro.sets)-1] {
			s := readSet(t, rr.dir, id)
			if _, wrong := s.mocks["mock-0"]; wrong {
				t.Fatalf("set %s (never received a test) was unnecessarily backfilled with mock-0", id)
			}
		}

		// Exactly one Warn with next_step must be emitted for exceeding the cap.
		var capWarns int
		for _, entry := range logs.All() {
			if strings.Contains(entry.Message, "rollover carry budget") {
				capWarns++
				if _, ok := entry.ContextMap()["next_step"]; !ok {
					t.Fatalf("startup cap Warn missing next_step field: %+v", entry.ContextMap())
				}
			}
		}
		if capWarns != 1 {
			t.Fatalf("startup carry budget Warn count = %d; want 1", capWarns)
		}
	})

	t.Run("long-running first test in aged set backfills aged set and later sets", func(t *testing.T) {
		const limit = 4096
		rr := startRollRec(t, limit)
		// boot0 in s0, then fill1 cuts s0->s1 at rollT0+5ms.
		rr.mock(rollMock("boot0", rollT0, 100))
		rr.mock(rollMock("fill1", rollT0.Add(5*time.Millisecond), 2*limit))
		rr.rolledTo(1)
		// fill2 cuts s1->s2 at rollT0+15ms; fill3 cuts s2->s3 at rollT0+25ms
		// (so s1 is aged out with s1.names==nil when test-1 finishes).
		rr.mock(rollMock("fill2", rollT0.Add(15*time.Millisecond), 2*limit))
		rr.rolledTo(2)
		rr.mock(rollMock("fill3", rollT0.Add(25*time.Millisecond), 2*limit))
		rr.rolledTo(3)
		// Long-running test-1 began inside aged s1 (req=10ms) and finished after cut 3.
		rr.test(rollTest("test-1", rollT0.Add(10*time.Millisecond), rollT0.Add(30*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[1], "test-1")
		rr.stop()

		for _, id := range rr.ro.sets[1:] {
			s := readSet(t, rr.dir, id)
			if _, ok := s.mocks["mock-0"]; !ok {
				t.Fatalf("set %s missing startup mock-0 for first test routed to aged s1", id)
			}
		}
	})
}

// Aged-set dedup check in copyInOrdered must fail closed on non-os.ErrNotExist
// ReadMockDocs errors instead of blindly appending duplicate mocks.
func TestRolloverCopyInAgedSetFailsClosedOnReadError(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	base := mockdb.New(logger, dir, "mocks")
	ctx := context.Background()
	m := rollMock("dup", rollT0, 50)
	if err := base.InsertMock(ctx, m, "s0"); err != nil {
		t.Fatal(err)
	}
	docs, format, err := base.ReadMockDocs(ctx, "s0", map[string]struct{}{m.Name: {}})
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyReadStore{mockDocStore: base, fails: 1}
	sr := newSetRoll(logger, &countRoller{limit: 1000}, flaky, "s0", nil, nil)
	sr.format = format
	sr.sets[0].names = nil // aged
	if _, _, err := sr.copyInOrdered(ctx, sr.sets[0], format, []string{m.Name}, docs, true); err == nil {
		t.Fatal("copyInOrdered on aged set ignored ReadMockDocs error and risked duplicating mock")
	}
	got := readSet(t, dir, "s0")
	if len(got.mocks) != 1 {
		t.Fatalf("s0 holds %d mocks after failed aged dedup read; want 1", len(got.mocks))
	}
}

// A late startup mock (request timestamp before firstTest) arriving AFTER the
// first test ran sr.test() must still be captured (because test() keeps
// startupOpen=true) and backfilled into s1 at finalize (when no second cut
// happens) as well as carried into s2 when a second cut does happen.
func TestRolloverLateStartupMockArrivingAfterFirstTestIsStillCarried(t *testing.T) {
	t.Run("healed on finalize with no second cut", func(t *testing.T) {
		const limit = 4096
		rr := startRollRec(t, limit)
		rr.mock(rollMock("boot0", rollT0, 100))
		t1 := rollT0.Add(10 * time.Millisecond)
		rr.mock(rollMock("fill", t1.Add(5*time.Millisecond), 2*limit))
		rr.rolledTo(1)
		// First test arrives after cut 1 -> test() backfills boot0, leaving
		// startupOpen true.
		rr.test(rollTest("test-1", t1, t1.Add(2*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[0], "test-1")
		// A long-lived startup call (at < t1) finishes and arrives AFTER test-1.
		rr.mock(rollMock("bootLate", rollT0.Add(2*time.Millisecond), 100))
		rr.stop()

		s1 := readSet(t, rr.dir, rr.ro.sets[1])
		for _, want := range []string{"mock-0", "mock-2"} {
			if _, ok := s1.mocks[want]; !ok {
				t.Fatalf("s1 missing startup mock %s after finalize (has %v)", want, mockURLs(s1))
			}
		}
	})

	t.Run("carried on next cut", func(t *testing.T) {
		const limit = 4096
		rr := startRollRec(t, limit)
		rr.mock(rollMock("boot0", rollT0, 100))
		t1 := rollT0.Add(10 * time.Millisecond)
		rr.mock(rollMock("fill1", t1.Add(5*time.Millisecond), 2*limit))
		rr.rolledTo(1)
		rr.test(rollTest("test-1", t1, t1.Add(2*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[0], "test-1")
		// Late startup mock arrives after test-1, then cut 2 closes startupOpen.
		rr.mock(rollMock("bootLate", rollT0.Add(2*time.Millisecond), 100))
		rr.mock(rollMock("fill2", t1.Add(20*time.Millisecond), 2*limit))
		rr.rolledTo(2)
		rr.stop()

		for _, id := range rr.ro.sets[1:] {
			s := readSet(t, rr.dir, id)
			for _, want := range []string{"mock-0", "mock-2"} {
				if _, ok := s.mocks[want]; !ok {
					t.Fatalf("set %s missing startup mock %s (has %v)", id, want, mockURLs(s))
				}
			}
		}
	})
}

// If ReadMockDocs transiently fails during sr.test() after cut 1 and no second
// rollover ever happens, sr.finalize() must retry and backfill startup into s1.
func TestRolloverTransientStartupReadFailureInTestPathHealedOnFinalize(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	base := mockdb.New(logger, dir, "mocks")
	ctx := context.Background()
	boot := rollMock("boot", rollT0, 100)
	if err := base.InsertMock(ctx, boot, "s0"); err != nil {
		t.Fatal(err)
	}
	flaky := &flakyReadStore{mockDocStore: base, fails: 1}
	ro := &countRoller{limit: 4096}
	sr := newSetRoll(logger, ro, flaky, "s0", nil, nil)
	sr.format = "yaml"
	sr.startup = []carried{{name: boot.Name, at: rollT0, docBytes: 600, startup: true, from: sr.sets[0]}}
	sr.startupQueuedBytes = 600
	sr.latest = rollT0.Add(20 * time.Millisecond)
	sr.bytes = 4096
	// Cut 1 commits while firstTest is still zero.
	sr.cut(ctx)
	// First test hits the transient ReadMockDocs failure in sr.test().
	sr.test(ctx, rollTest("test-1", rollT0.Add(10*time.Millisecond), rollT0.Add(12*time.Millisecond)))
	if _, err := os.Stat(filepath.Join(dir, "s0-s1", "mocks.yaml")); err == nil {
		t.Fatal("s0-s1 unexpectedly had mocks.yaml before retry")
	}
	// Finalize retries with no second cut and heals s0-s1.
	sr.finalize(ctx)
	s1 := readSet(t, dir, "s0-s1")
	if _, ok := s1.mocks[boot.Name]; !ok {
		t.Fatalf("s0-s1 mocks = %v; want startup mock %s backfilled on finalize", s1.mocks, boot.Name)
	}

	// If finalize itself hits a read failure, a Warn naming affectedTestSets
	// and next_step must be emitted (no misleading "will retry").
	core, logs := observer.New(zapcore.WarnLevel)
	flakyPerm := &flakyReadStore{mockDocStore: base, fails: 2}
	sr2 := newSetRoll(zap.New(core), &countRoller{limit: 4096}, flakyPerm, "s0", nil, nil)
	sr2.format = "yaml"
	sr2.startup = []carried{{name: boot.Name, at: rollT0, docBytes: 600, startup: true, from: sr2.sets[0]}}
	sr2.startupQueuedBytes = 600
	sr2.latest = rollT0.Add(20 * time.Millisecond)
	sr2.bytes = 4096
	sr2.cut(ctx)
	sr2.test(ctx, rollTest("test-1", rollT0.Add(10*time.Millisecond), rollT0.Add(12*time.Millisecond)))
	sr2.finalize(ctx)
	var finWarn bool
	for _, entry := range logs.All() {
		if entry.Level == zapcore.WarnLevel && strings.Contains(entry.Message, "could not read back startup calls at finalize") {
			finWarn = true
			cm := entry.ContextMap()
			if _, ok := cm["next_step"]; !ok {
				t.Fatalf("finalize read-failure Warn missing next_step: %+v", cm)
			}
			if _, ok := cm["affectedTestSets"]; !ok {
				t.Fatalf("finalize read-failure Warn missing affectedTestSets: %+v", cm)
			}
		}
	}
	if !finWarn {
		t.Fatalf("expected finalize startup read-failure Warn, got logs: %v", logs.All())
	}
}

// blockingReadStore blocks the first ReadMockDocs call until release is closed.
type blockingReadStore struct {
	mockDocStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingReadStore) ReadMockDocs(ctx context.Context, testSetID string, names map[string]struct{}) (map[string][]byte, string, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.mockDocStore.ReadMockDocs(ctx, testSetID, names)
}

// Pins two invariants of startup backfill:
//  1. Window registration: backfilled startup mocks are added to each target
//     set's windowMappings when mappings are window-derived, and skipped when
//     streamed (so cleared aged windows are not repopulated).
//  2. Reverse concurrent ordering: if test()'s ReadMockDocs is blocked while
//     cut() publishes s2 (and while a concurrent late startup mock is appended
//     to sr.startup), releasing test() commits boot to sr.carry, backfills BOTH
//     s1 and s2, and preserves the concurrently appended startup mock.
func TestRolloverBackfillRegistersWindowsAndCutBeforeTestCommit(t *testing.T) {
	t.Run("window registration respects streamed mode", func(t *testing.T) {
		for _, streamed := range []bool{false, true} {
			dir := t.TempDir()
			logger := zap.NewNop()
			base := mockdb.New(logger, dir, "mocks")
			ctx := context.Background()
			boot := rollMock("boot", rollT0, 100)
			if err := base.InsertMock(ctx, boot, "s0"); err != nil {
				t.Fatal(err)
			}
			wm := models.NewWindowedMock(boot)
			ro := &countRoller{limit: 4096}
			sr := newSetRoll(logger, ro, base, "s0", &windowMappings{}, nil)
			sr.format = "yaml"
			sr.newWindows = func() *windowMappings { return &windowMappings{} }
			sr.streamed = func() bool { return streamed }
			sr.startup = []carried{{name: boot.Name, at: rollT0, docBytes: 600, startup: true, from: sr.sets[0], window: &wm}}
			sr.startupQueuedBytes = 600
			sr.latest = rollT0.Add(20 * time.Millisecond)
			sr.bytes = 4096
			sr.cut(ctx)
			sr.test(ctx, rollTest("test-1", rollT0.Add(10*time.Millisecond), rollT0.Add(12*time.Millisecond)))
			s1 := sr.all()[1]
			s1.windows.mu.Lock()
			got := len(s1.windows.mocks)
			s1.windows.mu.Unlock()
			want := 1
			if streamed {
				want = 0
			}
			if got != want {
				t.Fatalf("streamed=%v: s1.windows has %d mocks; want %d", streamed, got, want)
			}
		}
	})

	t.Run("cut commits s2 before test commits startup and concurrent wrote stays queued", func(t *testing.T) {
		dir := t.TempDir()
		logger := zap.NewNop()
		base := mockdb.New(logger, dir, "mocks")
		ctx := context.Background()
		boot0 := rollMock("boot0", rollT0, 100)
		if err := base.InsertMock(ctx, boot0, "s0"); err != nil {
			t.Fatal(err)
		}
		bootConcurrent := rollMock("bootConcurrent", rollT0.Add(time.Millisecond), 100)
		if err := base.InsertMock(ctx, bootConcurrent, "s0"); err != nil {
			t.Fatal(err)
		}
		br := &blockingReadStore{
			mockDocStore: base,
			entered:      make(chan struct{}),
			release:      make(chan struct{}),
		}
		ro := &countRoller{limit: 4096}
		sr := newSetRoll(logger, ro, br, "s0", nil, nil)
		sr.format = "yaml"
		sr.startup = []carried{{name: boot0.Name, at: rollT0, docBytes: 600, startup: true, from: sr.sets[0]}}
		sr.startupQueuedBytes = 600
		sr.latest = rollT0.Add(20 * time.Millisecond)
		sr.bytes = 4096
		// Cut 1 commits s1 with firstTest zero.
		sr.cut(ctx)
		// Start sr.test() in background; it blocks inside ReadMockDocs for boot0.
		done := make(chan struct{})
		go func() {
			defer close(done)
			sr.test(ctx, rollTest("test-1", rollT0.Add(10*time.Millisecond), rollT0.Add(12*time.Millisecond)))
		}()
		select {
		case <-br.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("sr.test() never entered ReadMockDocs")
		}
		// While sr.test() is blocked in ReadMockDocs:
		// 1. A late startup mock arrives via wrote() and is appended to sr.startup.
		docC, _, err := base.ReadMockDocs(ctx, "s0", map[string]struct{}{bootConcurrent.Name: {}})
		if err != nil {
			t.Fatal(err)
		}
		sr.wrote(ctx, sr.sets[0], bootConcurrent, docC[bootConcurrent.Name], "yaml", nil, nil)
		// 2. Append s2 to sr.sets BEFORE sr.test() commits boot0 to sr.carry.
		sr.mu.Lock()
		s2 := &rollSet{id: "s0-s2", idx: 2, after: rollT0.Add(30 * time.Millisecond), names: map[string]struct{}{}}
		sr.sets = append(sr.sets, s2)
		sr.mu.Unlock()
		// Release sr.test(); it commits boot0, backfills BOTH s1 and s2, and
		// prunes only boot0 (keeping bootConcurrent in sr.startup).
		close(br.release)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("sr.test() did not return")
		}
		for _, id := range []string{"s0-s1", "s0-s2"} {
			s := readSet(t, dir, id)
			if _, ok := s.mocks[boot0.Name]; !ok {
				t.Fatalf("set %s missing boot0 after reverse-order cut+test", id)
			}
		}
		// Finalize resolves the concurrently appended bootConcurrent into s1 and s2.
		sr.finalize(ctx)
		for _, id := range []string{"s0-s1", "s0-s2"} {
			s := readSet(t, dir, id)
			if _, ok := s.mocks[bootConcurrent.Name]; !ok {
				t.Fatalf("set %s missing concurrently-appended startup mock %s", id, bootConcurrent.Name)
			}
		}
	})

	t.Run("earlier test arriving during blocked read excludes its own mocks from startup", func(t *testing.T) {
		dir := t.TempDir()
		logger := zap.NewNop()
		base := mockdb.New(logger, dir, "mocks")
		ctx := context.Background()
		boot0 := rollMock("boot0", rollT0, 100)
		if err := base.InsertMock(ctx, boot0, "s0"); err != nil {
			t.Fatal(err)
		}
		mockA := rollMock("mockA", rollT0.Add(12*time.Millisecond), 100)
		if err := base.InsertMock(ctx, mockA, "s0"); err != nil {
			t.Fatal(err)
		}
		br := &blockingReadStore{
			mockDocStore: base,
			entered:      make(chan struct{}),
			release:      make(chan struct{}),
		}
		ro := &countRoller{limit: 4096}
		sr := newSetRoll(logger, ro, br, "s0", nil, nil)
		sr.format = "yaml"
		sr.startup = []carried{
			{name: boot0.Name, at: rollT0, docBytes: 600, startup: true, from: sr.sets[0]},
			{name: mockA.Name, at: rollT0.Add(12 * time.Millisecond), docBytes: 600, startup: true, from: sr.sets[0]},
		}
		sr.startupQueuedBytes = 1200
		sr.latest = rollT0.Add(25 * time.Millisecond)
		sr.bytes = 4096
		// Cut 1 commits s1 with firstTest zero.
		sr.cut(ctx)
		// test-B (req=20ms) starts resolving [boot0, mockA] and blocks in ReadMockDocs.
		done := make(chan struct{})
		go func() {
			defer close(done)
			sr.test(ctx, rollTest("test-B", rollT0.Add(20*time.Millisecond), rollT0.Add(21*time.Millisecond)))
		}()
		select {
		case <-br.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("sr.test(test-B) never entered ReadMockDocs")
		}
		// While test-B's read is blocked, test-A (req=10ms, BEFORE mockA at 12ms)
		// arrives and lowers sr.firstTest to 10ms.
		sr.test(ctx, rollTest("test-A", rollT0.Add(10*time.Millisecond), rollT0.Add(30*time.Millisecond)))
		// Release test-B's read; commit-time re-check must keep boot0 and exclude mockA.
		close(br.release)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("sr.test(test-B) did not return")
		}
		s1 := readSet(t, dir, "s0-s1")
		if _, ok := s1.mocks[boot0.Name]; !ok {
			t.Fatalf("s0-s1 missing startup mock %s", boot0.Name)
		}
		if _, wrong := s1.mocks[mockA.Name]; wrong {
			t.Fatalf("s0-s1 carried %s (after test-A's req time) despite commit-time firstTest re-check", mockA.Name)
		}
		// Trigger a second cut to prove mockA was never committed to sr.carry either.
		sr.latest = rollT0.Add(40 * time.Millisecond)
		sr.bytes = 8192
		sr.cut(ctx)
		s2 := readSet(t, dir, "s0-s2")
		if _, wrong := s2.mocks[mockA.Name]; wrong {
			t.Fatalf("s0-s2 carried %s from sr.carry (after test-A's req time) despite commit-time firstTest re-check", mockA.Name)
		}
	})
}

// When a shorter later test completes before a longer earlier test whose
// request time falls in a rolled set skipped during the initial startup
// resolve, sr.test() must backfill the already-carried startup mocks into
// that earlier set on demand (both while it is open and after it has aged).
func TestRolloverEarlierTestArrivingAfterLaterTestStillGetsStartup(t *testing.T) {
	t.Run("open earlier set", func(t *testing.T) {
		rr := startRollRec(t, 4096)
		rr.mock(rollMock("boot0", rollT0, 100))
		rr.mock(rollMock("fill1", rollT0.Add(5*time.Millisecond), 8192))
		rr.rolledTo(1)
		rr.mock(rollMock("fill2", rollT0.Add(15*time.Millisecond), 8192))
		rr.rolledTo(2)
		// Short test-B in s2 arrives first; s1 is skipped initially.
		rr.test(rollTest("test-B", rollT0.Add(20*time.Millisecond), rollT0.Add(21*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[2], "test-B")
		// Longer test-A started earlier (in s1) and arrives second.
		rr.test(rollTest("test-A", rollT0.Add(10*time.Millisecond), rollT0.Add(30*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[1], "test-A")
		rr.stop()

		for _, id := range rr.ro.sets[1:] {
			s := readSet(t, rr.dir, id)
			if _, ok := s.mocks["mock-0"]; !ok {
				t.Fatalf("set %s missing startup mock-0 after out-of-order test arrival", id)
			}
		}
	})

	t.Run("aged earlier set", func(t *testing.T) {
		rr := startRollRec(t, 4096)
		rr.mock(rollMock("boot0", rollT0, 100))
		rr.mock(rollMock("fill1", rollT0.Add(5*time.Millisecond), 8192))
		rr.rolledTo(1)
		rr.mock(rollMock("fill2", rollT0.Add(15*time.Millisecond), 8192))
		rr.rolledTo(2)
		rr.mock(rollMock("fill3", rollT0.Add(25*time.Millisecond), 8192))
		rr.rolledTo(3)
		// Short test-B in s3 arrives first (s1 is aged out with names==nil).
		rr.test(rollTest("test-B", rollT0.Add(30*time.Millisecond), rollT0.Add(31*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[3], "test-B")
		// Long test-A started in aged s1 and arrives later.
		rr.test(rollTest("test-A", rollT0.Add(10*time.Millisecond), rollT0.Add(40*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[1], "test-A")
		rr.stop()

		for _, id := range []string{rr.ro.sets[1], rr.ro.sets[3]} {
			s := readSet(t, rr.dir, id)
			if _, ok := s.mocks["mock-0"]; !ok {
				t.Fatalf("set %s missing startup mock-0 after out-of-order arrival into aged set", id)
			}
		}
		// s2 never received a test and must remain un-backfilled.
		s2 := readSet(t, rr.dir, rr.ro.sets[2])
		if _, wrong := s2.mocks["mock-0"]; wrong {
			t.Fatalf("set %s (never received a test) was unnecessarily backfilled with mock-0", rr.ro.sets[2])
		}
	})

	t.Run("in-between set after late startup resolve gets both early and late startup mocks", func(t *testing.T) {
		rr := startRollRec(t, 4096)
		rr.mock(rollMock("boot0", rollT0, 100))
		rr.mock(rollMock("fill1", rollT0.Add(5*time.Millisecond), 8192))
		rr.rolledTo(1)
		rr.mock(rollMock("fill2", rollT0.Add(15*time.Millisecond), 8192))
		rr.rolledTo(2)
		rr.mock(rollMock("fill3", rollT0.Add(25*time.Millisecond), 8192))
		rr.rolledTo(3)
		// test-B in s3 arrives first; only s3 is backfilled with boot0.
		rr.test(rollTest("test-B", rollT0.Add(30*time.Millisecond), rollT0.Add(31*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[3], "test-B")
		// test-A in aged s1 arrives next; firstTest lowers to 10ms, s1 gets boot0 on demand.
		rr.test(rollTest("test-A", rollT0.Add(10*time.Millisecond), rollT0.Add(40*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[1], "test-A")
		// Late startup mock boot1 (at 8ms < firstTest) arrives, then cut 4 resolves it
		// across s1..s3: s2 (which still has hasStartup==false) must receive BOTH boot0
		// and boot1 so test-C landing in s2 later has all startup mocks.
		rr.mock(rollMock("boot1", rollT0.Add(8*time.Millisecond), 100))
		rr.mock(rollMock("fill4", rollT0.Add(45*time.Millisecond), 8192))
		rr.rolledTo(4)
		rr.test(rollTest("test-C", rollT0.Add(20*time.Millisecond), rollT0.Add(50*time.Millisecond)))
		waitForSavedTest(t, rr.dir, rr.ro.sets[2], "test-C")
		rr.stop()

		s2 := readSet(t, rr.dir, rr.ro.sets[2])
		for _, want := range []string{"mock-0", "mock-4"} {
			if _, ok := s2.mocks[want]; !ok {
				t.Fatalf("in-between set %s missing startup mock %s (has %v)", rr.ro.sets[2], want, mockURLs(s2))
			}
		}
	})
}
