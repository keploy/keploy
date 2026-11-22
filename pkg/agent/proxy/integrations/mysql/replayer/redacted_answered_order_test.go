package replayer_test

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

const (
	orderSecret = "orgLIVEsecret88"
	// orderFirst is the first test's recording: it redacted the key.
	orderFirst     = "SELECT v FROM kv WHERE api_key = '" + closePh1 + "' AND org = 'acme'"
	orderLiveFirst = "SELECT v FROM kv WHERE api_key = '" + closeSecret + "' AND org = 'acme'"
	// orderSecond is the next test's: it redacted the org, which its call
	// sends as orderSecret.
	orderSecond     = "SELECT v FROM kv WHERE api_key = 'pub' AND org = '" + closePh2 + "'"
	orderLiveSecond = "SELECT v FROM kv WHERE api_key = 'pub' AND org = '" + orderSecret + "'"
)

// orderMocks are the first test's recording, at s1, and the next test's, at
// s2.
func orderMocks(s1, s2 time.Time) (first, second *models.Mock) {
	first = diffMock("kv-1", "c1", []mysql.Request{queryReq(orderFirst)}, s1, models.LifetimePerTest, "mocks")
	first.Noise = []string{"^" + regexp.QuoteMeta(closePh1) + "$"}
	second = diffMock("kv-2", "c1", []mysql.Request{queryReq(orderSecond)}, s2, models.LifetimePerTest, "mocks")
	second.Noise = []string{"^" + regexp.QuoteMeta(closePh2) + "$"}
	return first, second
}

// gapStore is the agent's store, with the gap after a read of its window
// made wide: an armed read runs gap, once, after it took its value and
// before the reader goes on, as if the reader were descheduled there.
type gapStore struct {
	*proxy.MockManager
	mu  sync.Mutex
	gap func()
	// atNote arms the first read after a call asked the store for its
	// answered note (Keep): the read the note is told by.
	atNote bool
	armed  bool
	ran    bool
	// afterTake runs once, right after the store took a recording out of
	// its pool for an answer, before the answer goes on.
	afterTake func()
	// afterPoolRead runs once, right after a read of the test's per-test
	// pool, before the reader goes on.
	afterPoolRead func()
}

// once takes f and clears it under s.mu.
func (s *gapStore) once(f *func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := *f
	*f = nil
	return g
}

// gapAtNote runs gap in the first read of the window after a call asked
// the store for its answered note.
func (s *gapStore) gapAtNote(gap func()) {
	s.mu.Lock()
	s.gap, s.atNote, s.armed = gap, true, false
	s.mu.Unlock()
}

// gapAtNextRead runs gap in the next read of the window.
func (s *gapStore) gapAtNextRead(gap func()) {
	s.mu.Lock()
	s.gap, s.atNote, s.armed = gap, false, true
	s.mu.Unlock()
}

func (s *gapStore) gapRan() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ran
}

func (s *gapStore) DeleteFilteredMock(m models.Mock) bool {
	ok := s.MockManager.DeleteFilteredMock(m)
	if ok {
		if f := s.once(&s.afterTake); f != nil {
			f()
		}
	}
	return ok
}

func (s *gapStore) Keep(slot any, mk func() any) (any, bool) {
	s.mu.Lock()
	if s.gap != nil && s.atNote && slot == replayer.AnsweredSlotForTest() {
		s.armed = true
	}
	s.mu.Unlock()
	return s.MockManager.Keep(slot, mk)
}

func (s *gapStore) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	m, err := s.MockManager.GetPerTestMocksInWindow()
	if f := s.once(&s.afterPoolRead); f != nil {
		f()
	}
	return m, err
}

func (s *gapStore) CurrentTestWindow() (time.Time, time.Time) {
	start, end := s.MockManager.CurrentTestWindow()
	s.mu.Lock()
	var gap func()
	if s.armed {
		gap, s.gap, s.armed, s.ran = s.gap, nil, false, true
	}
	s.mu.Unlock()
	if gap != nil {
		gap()
	}
	return start, end
}

// inGap is a gap that runs work as a caller racing the reader: work starts
// in the gap, and the gap waits for it when the read was made with db's
// answered note free (outside its lock), so work is done within the gap;
// else the reader holds the note, and work goes on beside it and waits for
// the note where it takes it. It tells the two apart only while nothing but
// work and the reader touch the note. wait returns once work is done (at
// once when the gap never ran); the row's cleanup waits too.
func inGap(t *testing.T, db *gapStore, work func()) (gap, wait func()) {
	var started atomic.Bool
	done := make(chan struct{})
	gap = func() {
		free := replayer.AnsweredNoteFreeForTest(db)
		started.Store(true)
		go func() {
			defer close(done)
			work()
		}()
		if free {
			<-done
		}
	}
	wait = func() {
		if started.Load() {
			<-done
		}
	}
	t.Cleanup(wait)
	return gap, wait
}

// gapRows is a store for a row of a gap, its first test's window open with
// first, a recording that redacted the key, staged, and the next test's
// window and recording, second, which redacted the org.
func gapRows(t *testing.T) (mm *proxy.MockManager, db *gapStore, s1, e1, s2, e2 time.Time, first, second *models.Mock) {
	t.Helper()
	replayer.ForgetAnsweredForTest()
	t.Cleanup(replayer.ForgetAnsweredForTest)
	mm = proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	db = &gapStore{MockManager: mm}
	s1, e1 = diffWindow(1)
	s2, e2 = diffWindow(2)
	first, second = orderMocks(s1.Add(time.Millisecond), s2.Add(time.Millisecond))
	mm.SetMocksWithWindow([]*models.Mock{first}, nil, s1, e1)
	return mm, db, s1, e1, s2, e2, first, second
}

// nextTestOpens is the work of a gap where the first test ends, the next
// opens with second staged, and the next test's call is answered.
func nextTestOpens(t *testing.T, mm *proxy.MockManager, db *gapStore, s2, e2 time.Time, second *models.Mock) func() {
	return func() {
		mm.SetMocksWithWindow([]*models.Mock{second}, nil, s2, e2)
		if ok, miss, _, err := replayer.MatchShownForTest(closeConn, zap.NewNop(), queryReq(orderLiveSecond), db, closeDctx()); err != nil || !ok {
			t.Errorf("precondition: the next test's call was not answered: ok %v, err %v, miss %s", ok, err, miss)
		}
	}
}

// A call of the first test is answered, and its answer reads the store's
// window (the first test's) to note its recording's places just before the
// replay moves the store on to the next test and an answer of the next
// test (by a recording that redacted the org its call sends) is noted,
// before the first answer goes on. The late answer of the first test must
// not take the next test's places from it: the next test's call, made
// once more than recorded (a retry), misses, and its report and its
// command as shown redact the org.
func TestMySQLALateAnswerOfATestKeepsTheNextTestsPlaces(t *testing.T) {
	mm, db, _, _, s2, e2, _, second := gapRows(t)
	gap, wait := inGap(t, db, func() {
		mm.SetCurrentTestWindow(s2, e2)
		replayer.AnswerForTest(db, second)
	})
	db.gapAtNote(gap)
	closeAnswer(t, db, "first", orderLiveFirst)
	if !db.gapRan() {
		t.Fatalf("precondition: the first test's answer read no window once it asked for its note")
	}
	wait()
	if s, _ := mm.CurrentTestWindow(); !s.Equal(s2) {
		t.Fatalf("precondition: the store tells the window at %v; want the next test's, at %v", s, s2)
	}
	shown, miss := closeMiss(t, db, "next", orderLiveSecond)
	if strings.Contains(shown, orderSecret) || strings.Contains(miss, orderSecret) {
		t.Errorf("the next test's retry shows the org its answered recording redacted:\nshown %s\nreport %s", shown, miss)
	}
}

// A miss of the first test reads the places of the recordings that
// answered in it (one redacted the key the miss sends) as the replay moves
// the store on to the next test and the next test's call is answered: the
// miss read the first test's window, and is shown by the first test's
// places, not by none.
func TestMySQLAMissReadAsTheNextTestOpensIsShownByItsTestsPlaces(t *testing.T) {
	mm, db, s1, _, s2, e2, _, second := gapRows(t)
	closeAnswer(t, db, "first", orderLiveFirst)
	gap, wait := inGap(t, db, nextTestOpens(t, mm, db, s2, e2, second))
	db.gapAtNextRead(gap)
	shown := replayer.AnsweredShownForTest(db, s1, orderLiveFirst)
	if !db.gapRan() {
		t.Fatalf("precondition: the miss read no window")
	}
	wait()
	if strings.Contains(shown, closeSecret) {
		t.Errorf("the first test's miss, read in its window, shows the key its answered recording redacted: %s", shown)
	}
	if s, _ := mm.CurrentTestWindow(); s.Equal(s1) {
		t.Fatalf("precondition: the store still tells the first test's window")
	}
}

// A store's release of a note, at a staging that finds the store's window
// is not the note's, lets go of what the note held when it found that, and
// no more: the window moved back to the note's test (SetCurrentTestWindow,
// which moves it with no staging) and an answer of that test made while
// the release runs are kept, and a miss of the test is shown by them.
func TestMySQLAReleaseLetsGoOfNoAnswerMadeAfterItFoundTheWindowGone(t *testing.T) {
	mm, db, s1, e1, s2, e2, first, second := gapRows(t)
	replayer.AnswerForTest(db, first)
	mm.SetCurrentTestWindow(s2, e2)
	gap, wait := inGap(t, db, func() {
		mm.SetCurrentTestWindow(s1, e1)
		replayer.AnswerForTest(db, second)
	})
	db.gapAtNextRead(gap)
	// A staging that keeps the window (a mid-set one, at BaseTime) lets the
	// store's staged recordings go: the release reads the window.
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now().Add(time.Hour))
	if !db.gapRan() {
		t.Fatalf("precondition: the release read no window")
	}
	wait()
	if s, _ := mm.CurrentTestWindow(); !s.Equal(s1) {
		t.Fatalf("precondition: the store tells the window at %v; want the note's, at %v", s, s1)
	}
	if shown := replayer.AnsweredShownForTest(db, s1, orderLiveSecond); strings.Contains(shown, orderSecret) {
		t.Errorf("a miss of the test shows the org its answer, made once the window was back, redacted: %s", shown)
	}
}

// A miss of the first test is matched against the first test's pool, out
// of which its answered recording (it redacted the key) was taken, and the
// replay moves the store on to the next test, whose call may be answered,
// before the miss's report and its logs read the places they show it by.
// They do not show the key: the places of the test the miss was matched in
// are not those of the window the store tells now.
func TestMySQLAMissMatchedAsTheNextTestOpensDoesNotShowItsTestsSecret(t *testing.T) {
	for _, answered := range []bool{false, true} {
		t.Run(map[bool]string{false: "the next test not yet answered", true: "the next test answered"}[answered], func(t *testing.T) {
			mm, db, _, _, s2, e2, _, second := gapRows(t)
			closeAnswer(t, db, "first", orderLiveFirst)
			ran := false
			db.mu.Lock()
			db.afterPoolRead = func() {
				ran = true
				if answered {
					nextTestOpens(t, mm, db, s2, e2, second)()
					return
				}
				mm.SetMocksWithWindow([]*models.Mock{second}, nil, s2, e2)
			}
			db.mu.Unlock()
			shown, miss := closeMiss(t, db, "first", orderLiveFirst)
			if !ran {
				t.Fatalf("precondition: the miss read no per-test pool")
			}
			if s, _ := mm.CurrentTestWindow(); !s.Equal(s2) {
				t.Fatalf("precondition: the store tells the window at %v; want the next test's, at %v", s, s2)
			}
			if strings.Contains(shown, closeSecret) || strings.Contains(miss, closeSecret) {
				t.Errorf("the first test's miss, matched in its window, shows the key its answered recording redacted:\nshown %s\nreport %s", shown, miss)
			}
		})
	}
}

// Two calls of a test send the same statement at once, on two connections:
// one is answered by the test's recording (it redacted the key), which the
// store takes out of the pool, and the other misses against the pool left.
// The miss is matched and reported right after the store took the
// recording out, before the answer goes on, and is redacted by it.
func TestMySQLAMissRightAfterAnAnswerTookItsRecordingIsRedactedByIt(t *testing.T) {
	_, db, _, _, _, _, _, _ := gapRows(t)
	var ok, ran bool
	var miss, shown string
	var err error
	db.mu.Lock()
	db.afterTake = func() {
		ran = true
		other := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c2")
		done := make(chan struct{})
		go func() {
			defer close(done)
			ok, miss, shown, err = replayer.MatchShownForTest(other, zap.NewNop(), queryReq(orderLiveFirst), db, closeDctx())
		}()
		<-done
	}
	db.mu.Unlock()
	closeAnswer(t, db, "first", orderLiveFirst)
	if !ran {
		t.Fatalf("precondition: the answer took no recording out of the pool")
	}
	if err != nil || ok || miss == "" {
		t.Fatalf("precondition: the other call was not a miss: ok %v, err %v, miss %q", ok, err, miss)
	}
	if strings.Contains(shown, closeSecret) || strings.Contains(miss, closeSecret) {
		t.Errorf("the other call's miss shows the key the recording that answered redacted:\nshown %s\nreport %s", shown, miss)
	}
}

// The same with no gap made: on the agent's store, answers of the first
// test race the move to the next test and the next test's answer, round
// after round. Every round, the places of the next test's answer redact a
// miss of the next test.
func TestMySQLAnswersRacingTheNextTestKeepItsPlaces(t *testing.T) {
	const rounds, answerers, answers = 5000, 4, 16
	s1, e1 := diffWindow(1)
	s2, e2 := diffWindow(2)
	first, second := orderMocks(s1.Add(time.Millisecond), s2.Add(time.Millisecond))
	lost := 0
	for r := 0; r < rounds; r++ {
		mm := proxy.NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow(nil, nil, s1, e1)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < answerers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for j := 0; j < answers; j++ {
					replayer.AnswerForTest(mm, first)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			mm.SetMocksWithWindow(nil, nil, s2, e2)
			replayer.AnswerForTest(mm, second)
		}()
		close(start)
		wg.Wait()
		if strings.Contains(replayer.AnsweredShownForTest(mm, s2, orderLiveSecond), orderSecret) {
			lost++
		}
		mm.Close()
	}
	if lost > 0 {
		t.Errorf("%d of %d rounds: a miss of the next test shows the org the recording that answered in it redacted", lost, rounds)
	}
}
