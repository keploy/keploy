package http

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// keyedMemDb is the test double with the agent's keyed reads
// (integrations.PerTestKeyRanger, integrations.SessionKeyReader), so a call
// with the proven switch off is answered through the keyed passes
// (keyedExactBody, keyedLaxRead). A key holds the double's recordings its
// index files under it, in the double's order; filed, when set, says which
// recordings a key of requestIndex holds instead, as a store whose keys
// collide would. Its first claim is refused (failsFirstClaim). wholeReads
// counts the reads of the whole pool.
type keyedMemDb struct {
	*failsFirstClaim
	filed      func(mk *models.Mock) bool
	keyedReads int
	wholeReads int
}

func (s *keyedMemDb) holds(ix *integrations.MockIndex, key string, mk *models.Mock) bool {
	if ix == requestIndex && s.filed != nil {
		return s.filed(mk)
	}
	return slices.Contains(ix.Keys(mk), key)
}

// The double's recordings are all of the session tier (GetSessionMocks).
func (s *keyedMemDb) RangePerTestMocksWithKey(*integrations.MockIndex, string, func(*models.Mock) bool) error {
	s.keyedReads++
	return nil
}

func (s *keyedMemDb) RangeSessionMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	for _, mk := range s.mocks {
		if s.holds(ix, key, mk) && !fn(mk) {
			return nil
		}
	}
	return nil
}

func (s *keyedMemDb) GetSessionMocks() ([]*models.Mock, error) {
	s.wholeReads++
	return s.failsFirstClaim.GetSessionMocks()
}

func keyedDB(mocks ...*models.Mock) *keyedMemDb {
	return &keyedMemDb{failsFirstClaim: &failsFirstClaim{mockMemDb: &mockMemDb{updateUnFilteredReturn: true, mocks: mocks}}}
}

// matchKeyed runs the matcher with the proven switch off, stateful mocks on,
// and fails the row when the call does not end within 10 s.
func matchKeyed(t *testing.T, in *req, db integrations.MockMemDb) *models.Mock {
	t.Helper()
	type answer struct {
		served *models.Mock
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		ok, served, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, true, false)
		if err == nil && !ok {
			err = errors.New("no recording")
		}
		done <- answer{served, err}
	}()
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatal(a.err)
		}
		return a.served
	case <-time.After(10 * time.Second):
		t.Fatal("the call never ended")
	}
	return nil
}

// A heavy call answered by its own request's recordings keeps the one turn
// it took when its claim is lost to another call: it picks again in that
// turn, asking for none beside it, and gives it back once answered. The
// stateful cursor moves only for the recording served, so the retry serves
// the first of the request's recordings and the next call the second, with
// no read of the whole pool.
func TestAKeyedHeavyExactCallKeepsItsTurnAndCursorAcrossALostClaim(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	turns := heavyTurns()
	takeTurns(t, cap(turns)-1)
	body := jsonQ(fuzzyLetters(600000, 11))
	var mocks []*models.Mock
	for _, name := range []string{"first", "second"} {
		m := recorded(name, "/poll", jsonHeader, body)
		m.TestModeInfo.Consume = models.ConsumeCursorSaturate
		mocks = append(mocks, m)
	}
	db := keyedDB(mocks...)
	for i, want := range []string{"first", "second"} {
		in := sent("/poll", jsonHeader, body)
		in.raw = in.body
		served := matchKeyed(t, in, db)
		if served == nil || served.Name != want {
			t.Fatalf("call %d served %v, want %q", i, served, want)
		}
		if len(turns) != cap(turns)-1 {
			t.Fatalf("call %d: %d turns held after it, want the row's %d", i, len(turns), cap(turns)-1)
		}
	}
	if db.claims != 3 {
		t.Fatalf("%d claims, want the refused one and one for each call", db.claims)
	}
	if db.keyedReads == 0 || db.wholeReads != 0 {
		t.Fatalf("%d keyed reads and %d whole reads, want the keyed pass alone", db.keyedReads, db.wholeReads)
	}
}

// A heavy call that its path's recordings answer (keyedLaxRead) keeps the
// one turn it took when its claim is lost: it picks again in that turn, and
// gives it back once answered.
func TestAKeyedHeavyLaxCallKeepsItsTurnAcrossALostClaim(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	turns := heavyTurns()
	takeTurns(t, cap(turns)-1)
	db := keyedDB(heavyDB(t, 8, 200000).mocks...)
	in := sent("/poll", jsonHeader, jsonQ(fuzzyLetters(200000, 3)))
	in.raw = in.body
	if served := matchKeyed(t, in, db); served == nil {
		t.Fatal("no recording served")
	}
	if db.claims != 2 {
		t.Fatalf("%d claims, want the refused one and the one after", db.claims)
	}
	if len(turns) != cap(turns)-1 {
		t.Fatalf("%d turns held after the call, want the row's %d: the call kept one", len(turns), cap(turns)-1)
	}
	if db.keyedReads == 0 || db.wholeReads != 0 {
		t.Fatalf("%d keyed reads and %d whole reads, want the keyed passes alone", db.keyedReads, db.wholeReads)
	}
}

// A call whose own request's recordings weigh little takes no turn in the
// exact-body pass, and so still takes one in the passes after it when its
// path's recordings are heavy: with every turn held it waits there, and is
// answered once a turn is free.
func TestAKeyedLightExactPassLeavesTheTurnToTheHeavyPassAfterIt(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	body := jsonQ("light")
	// Filed under the call's request, but of another media type, so the
	// schema match drops it and the call goes on to its path's recordings.
	text := recorded("text", "/poll", map[string]string{"Content-Type": "text/plain"}, body)
	db := keyedDB(append([]*models.Mock{text}, heavyDB(t, 8, 200000).mocks...)...)
	db.claims = 1 // no claim refused
	giveBack := takeAllTurns(t)
	in := sent("/poll", jsonHeader, body)
	in.raw = in.body
	// Answered or missed alike: the row is about when the call is matched.
	done := make(chan error, 1)
	go func() {
		_, _, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, true, false)
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("a call heavy past its exact-body pass was matched with no turn free")
	case <-time.After(300 * time.Millisecond):
	}
	giveBack()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not end once a turn was free")
	}
	if db.keyedReads == 0 {
		t.Fatal("no keyed read: the row needs the keyed passes")
	}
}

// A call that is a recording's very request is answered from its request's
// recordings alone (keyedExactBody), however heavy its path's other
// recordings are: with every turn held it is answered at once, the light
// recordings of its request taking no turn.
func TestAKeyedExactCallIsAnsweredFromItsRequestAlone(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	body := jsonQ("light")
	db := keyedDB(append(heavyDB(t, 8, 200000).mocks, recorded("own", "/poll", jsonHeader, body))...)
	db.claims = 1 // no claim refused
	giveBack := takeAllTurns(t)
	defer giveBack()
	in := sent("/poll", jsonHeader, body)
	in.raw = in.body
	if served := matchKeyed(t, in, db); served == nil || served.Name != "own" {
		t.Fatalf("served %v, want the recording of the call's very request", served)
	}
	if db.wholeReads != 0 {
		t.Fatalf("%d whole reads, want the keyed pass alone", db.wholeReads)
	}
}

// A recording a key of requestIndex holds answers a call through the
// exact-body pass only when its body is the call's, byte for byte: under a
// key whose body hash collides with the call's, another body is passed over
// and the call is answered as its path's recordings answer it.
func TestAKeyedExactPassServesOnlyTheCallsVeryBody(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	far := recorded("far", "/poll", jsonHeader, jsonQ("far"))
	near := recorded("near", "/poll", jsonHeader, jsonQ("near"))
	db := keyedDB(far, near)
	db.claims = 1 // no claim refused
	db.filed = func(mk *models.Mock) bool { return mk.Name == "far" }
	in := sent("/poll", jsonHeader, jsonQ("near"))
	in.raw = in.body
	if served := matchKeyed(t, in, db); served == nil || served.Name != "near" {
		t.Fatalf("served %v, want the recording of the call's very body", served)
	}
	if db.keyedReads == 0 {
		t.Fatal("no keyed read: the row needs the keyed passes")
	}
}

// A heavy call with no recording filed under its request's key leaves the
// keyed exact pass at once and takes no turn: the pass read nothing to weigh,
// and the whole read that follows weighs what it reads itself (admit). With
// every turn held, the pass ends at once, nothing found.
func TestAKeyedExactPassWithNothingUnderItsKeyTakesNoTurn(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	db := keyedDB(recorded("other", "/other", jsonHeader, jsonQ("other")))
	giveBack := takeAllTurns(t)
	defer giveBack()
	in := sent("/poll", jsonHeader, jsonQ(fuzzyLetters(heavyCallBytes, 5)))
	in.raw = in.body
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hit, err := newHTTP().keyedExactBody(ctx, in, db, nil, nil, true, true)
	if hit.end != nil {
		hit.end()
		t.Fatal("the pass took a turn with nothing under the call's key")
	}
	if err != nil || hit.found {
		t.Fatalf("the pass with nothing under the call's key: found %v, err %v; want nothing found and no error", hit.found, err)
	}
	if db.keyedReads == 0 {
		t.Fatal("no keyed read: the row needs the keyed pass")
	}
}

// ownKeyedDb is keyedMemDb with recordings of the per-test tier too
// (perTest), which its keyed per-test read reads by key, in a store that does
// not tell which recordings are the open test's own (no
// integrations.TestOwner), so its per-test pool is the test's own
// (openTestOwner). It marks used and keeps for repeats
// (integrations.RepeatKeeper) what it is told to.
type ownKeyedDb struct {
	*keyedMemDb
	perTest    []*models.Mock
	used, kept map[string]bool
}

func (s *ownKeyedDb) GetPerTestMocksInWindow() ([]*models.Mock, error) { return s.perTest, nil }

func (s *ownKeyedDb) RangePerTestMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	s.keyedReads++
	for _, mk := range s.perTest {
		if slices.Contains(ix.Keys(mk), key) && !fn(mk) {
			return nil
		}
	}
	return nil
}

func (s *ownKeyedDb) MarkMockAsUsed(m models.Mock) bool {
	s.used[m.Name] = true
	return true
}

func (s *ownKeyedDb) KeepForRepeats(name string) { s.kept[name] = true }

func (s *ownKeyedDb) KeptForRepeats() func(string) bool {
	return func(name string) bool { return s.kept[name] }
}

// A per-test recording of a stateful dependency (its lifetime stated) that a
// call is answered through the keyed exact pass is the open test's own where
// the store does not tell whose it is: of the per-test pool. So it is kept
// for the test's byte-identical repeats and marked used, as the whole read
// keeps it, not served as a reusable recording, which would only be marked.
func TestAKeyedExactCallKeepsTheTestsOwnRecordingForItsRepeats(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	own := recorded("own", "/count", jsonHeader, jsonQ("n"))
	own.TestModeInfo.Lifetime, own.TestModeInfo.Consume = models.LifetimePerTest, models.ConsumeCursorSaturate
	own.Spec.Metadata = map[string]string{models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	if !own.StatesLifetime() {
		t.Fatal("precondition: the recording does not state its lifetime")
	}
	db := &ownKeyedDb{keyedMemDb: keyedDB(), perTest: []*models.Mock{own}, used: map[string]bool{}, kept: map[string]bool{}}
	in := sent("/count", jsonHeader, jsonQ("n"))
	in.raw = in.body
	if served := matchKeyed(t, in, db); served == nil || served.Name != "own" {
		t.Fatalf("served %v, want the test's own recording of the call's very request", served)
	}
	if !db.used["own"] || !db.kept["own"] {
		t.Fatalf("the test's own recording: marked used %v, kept for repeats %v; want both", db.used["own"], db.kept["own"])
	}
	if db.keyedReads == 0 || db.wholeReads != 0 {
		t.Fatalf("%d keyed reads and %d whole reads, want the keyed pass alone", db.keyedReads, db.wholeReads)
	}
}

// sessionKeyFails is keyedMemDb with recordings of the per-test tier too
// (perTest), which its keyed per-test read reads by key, and its keyed
// session read failing: a store whose keyed read fails, so the call reads
// the whole pool.
type sessionKeyFails struct {
	*keyedMemDb
	perTest []*models.Mock
}

func (s *sessionKeyFails) GetPerTestMocksInWindow() ([]*models.Mock, error) { return s.perTest, nil }

func (s *sessionKeyFails) RangePerTestMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	s.keyedReads++
	for _, mk := range s.perTest {
		if slices.Contains(ix.Keys(mk), key) && !fn(mk) {
			return nil
		}
	}
	return nil
}

func (s *sessionKeyFails) RangeSessionMocksWithKey(*integrations.MockIndex, string, func(*models.Mock) bool) error {
	return errors.New("the keyed session read fails")
}

// A call whose store fails its keyed session read is matched as the whole
// read matches it, not from the per-test recordings the keyed read gave
// before it failed: of a light per-test recording of the call's very request
// and heavy session recordings of its path, the whole read weighs them all,
// so with every turn held the call waits for one, and is answered the
// per-test recording once a turn is free.
func TestAKeyedReadThatFailsLeavesTheCallToTheWholeRead(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	body := jsonQ(fuzzyLetters(300000, 5))
	own := recorded("own", "/poll", jsonHeader, body)
	db := &sessionKeyFails{keyedMemDb: keyedDB(heavyDB(t, 4, 300000).mocks...), perTest: []*models.Mock{own}}
	db.claims = 1 // no claim refused
	db.deleteFilteredReturn = true
	giveBack := takeAllTurns(t)
	defer giveBack()
	in := sent("/poll", jsonHeader, body)
	in.raw = in.body
	type answer struct {
		served *models.Mock
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		_, served, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, true, false)
		done <- answer{served, err}
	}()
	select {
	case <-done:
		t.Fatal("the call was matched with no turn free: it was answered from the keyed read that failed")
	case <-time.After(300 * time.Millisecond):
	}
	giveBack()
	select {
	case a := <-done:
		if a.err != nil {
			t.Fatal(a.err)
		}
		if a.served == nil || a.served.Name != "own" {
			t.Fatalf("served %v, want the per-test recording of the call's very request", a.served)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not end once a turn was free")
	}
	if db.keyedReads == 0 {
		t.Fatal("no keyed read: the row needs the keyed passes")
	}
}

// A call answered by its very request's recording, whose response the store
// keeps apart (a spilled response) and does not load, ends with that error,
// as the whole read's claim ends it: it does not pick the recording again.
func TestAKeyedExactCallWhoseResponseDoesNotLoadEndsWithTheError(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	body := jsonQ("spilled")
	m := recorded("spilled", "/poll", jsonHeader, body)
	m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return nil, nil, errors.New("the parked response is gone")
	})
	db := keyedDB(m)
	db.claims = 1 // no claim refused
	in := sent("/poll", jsonHeader, body)
	in.raw = in.body
	done := make(chan error, 1)
	go func() {
		_, _, _, err := newHTTP().match(context.Background(), in, db, nil, nil, nil, false, false, false, true, false)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "load the recorded response") {
			t.Fatalf("the call ended with %v, want the response's load error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call never ended: it picked the recording again and again")
	}
	if db.keyedReads == 0 {
		t.Fatal("no keyed read: the row needs the keyed passes")
	}
}

// A url noise's index is made once (noiseIndexOf): a call under a noise an
// earlier call was matched under reads the index the stores already hold,
// another noise has one of its own, and at most noiseIndexesCap are kept, so
// a replay of many noises holds a bounded set.
func TestANoiseIndexIsMadeOncePerNoise(t *testing.T) {
	noiseIndexes.mu.Lock()
	noiseIndexes.m = nil
	noiseIndexes.mu.Unlock()
	log := zap.NewNop()
	a := noiseIndexOf(log, []string{"/a/[0-9]+"})
	b := noiseIndexOf(log, []string{"/b/[0-9]+"})
	if a == b {
		t.Fatal("two noises share one index")
	}
	if again := noiseIndexOf(log, []string{"/a/[0-9]+"}); again != a {
		t.Fatal("a noise met again was given a new index")
	}
	for i := 0; i <= noiseIndexesCap; i++ {
		noiseIndexOf(log, []string{fmt.Sprintf("/c%d/[0-9]+", i)})
		noiseIndexes.mu.Lock()
		kept := len(noiseIndexes.m)
		noiseIndexes.mu.Unlock()
		if kept > noiseIndexesCap {
			t.Fatalf("%d noises' indexes kept, want at most %d", kept, noiseIndexesCap)
		}
	}
}

// The keyed reads' keys tell requests apart by method and by each segment
// of the path (idKey keeps where a segment ends), and a path key from an id
// key; a mock that is not an HTTP recording, or whose URL does not parse, is
// filed under no key, by requestIndex, pathIndex and a noise's index alike.
func TestTheKeyedReadsKeys(t *testing.T) {
	if idKey("GET", "/a/b") == idKey("GET", "/ab") {
		t.Fatal("/a/b and /ab share an id key")
	}
	if idKey("GET", "/a/b") == idKey("POST", "/a/b") {
		t.Fatal("two methods share an id key")
	}
	if pathKey("GET", "/a") == idKey("GET", "/a") {
		t.Fatal("a path key is an id key")
	}
	grpc := recorded("grpc", "/a", jsonHeader, "")
	grpc.Kind = models.GRPC_EXPORT
	noReq := recorded("noReq", "/a", jsonHeader, "")
	noReq.Spec.HTTPReq = nil
	badURL := recorded("badURL", "/a", jsonHeader, "")
	badURL.Spec.HTTPReq.URL = "http://[::1"
	for name, keys := range map[string]func(*models.Mock) []string{
		"requestIndex": requestIndex.Keys,
		"pathIndex":    pathIndex.Keys,
		"noise":        noiseIndexOf(zap.NewNop(), []string{"/a/[0-9]+"}).Keys,
	} {
		for i, m := range []*models.Mock{nil, grpc, noReq, badURL} {
			if got := keys(m); got != nil {
				t.Fatalf("%s filed mock %d (nil, gRPC, no request, a bad URL) under %q", name, i, got)
			}
		}
		if got := keys(recorded("ok", "/a", jsonHeader, "")); len(got) == 0 {
			t.Fatalf("%s filed an HTTP recording under no key", name)
		}
	}
}

// A heavy call that no recording of its path answers, given up by its
// caller while it waits for a turn, ends with its caller's end, not with a
// miss: no report is made of a call no one waits for.
func TestAHeavyCallGivenUpWhileWaitingForATurnEndsWithoutAMiss(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	db := keyedDB(recorded("elsewhere", "/elsewhere", jsonHeader, jsonQ("x")))
	db.claims = 1 // no claim refused
	giveBack := takeAllTurns(t)
	defer giveBack()
	in := sent("/nowhere", jsonHeader, jsonQ(fuzzyLetters(int(heavyCallBytes)+1, 9)))
	in.raw = in.body
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, diag, err := newHTTP().match(ctx, in, db, nil, nil, nil, false, false, false, true, false)
		if err == nil && diag != nil {
			err = fmt.Errorf("a miss of phase %q", diag.phase)
		}
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the call given up ended with %v, want its caller's end", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call given up never ended")
	}
	if db.keyedReads == 0 {
		t.Fatal("no keyed read: the row needs the keyed passes")
	}
}
