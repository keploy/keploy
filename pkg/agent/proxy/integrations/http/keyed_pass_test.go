package http

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
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
func matchKeyed(t *testing.T, in *req, db *keyedMemDb) *models.Mock {
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
