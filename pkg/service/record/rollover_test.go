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
		hooks: insertedHooks{mocks: make(chan string, 1024)},
		done:  make(chan error, 1),
	}
	if cr, ok := ro.(*countRoller); ok {
		rr.ro = cr
	} else if br, ok := ro.(*blockingNextRoller); ok {
		rr.ro = &br.countRoller
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
	// Second cut: transient error is gone, so boot must be read and carried.
	sr.latest = rollT0.Add(30 * time.Millisecond)
	sr.bytes = 2000
	sr.cut(ctx)
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
