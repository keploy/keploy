package http

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// fullRoundTestDb is an in-memory RoundReader + ExactRoundReader + SharedReads
// + StagingGenerations store for deterministic unit testing of learnIndexes
// and tryExactLearnMatch inside package http.
type fullRoundTestDb struct {
	mu           sync.Mutex
	epoch        uint64
	gen          uint64
	ownedGen     uint64
	test         string
	owned        []*models.Mock
	ownedIdx     map[string]int
	consumed     []bool
	sharedPT     []*models.Mock
	sharedS      []*models.Mock
	sharedG      integrations.SharedGen
	stageG       uint64
	spent        map[string]bool
	voided       map[string]struct{}
	windows      *models.WindowSchedule
	winStart     time.Time
	winEnd       time.Time
	inFlight     map[string]bool
	bgPool       map[string]struct{}
	restage      []func()
	snaps        int
	unspentScans int
	onDelete     func(models.Mock) bool
	claimed      map[string]struct{}
}

func newFullRoundTestDb(test string, owned, sharedPT, sharedS []*models.Mock) *fullRoundTestDb {
	idx := make(map[string]int, len(owned))
	for i, m := range owned {
		idx[m.Name] = i
	}
	return &fullRoundTestDb{
		epoch:    1,
		gen:      1,
		ownedGen: 1,
		test:     test,
		owned:    owned,
		ownedIdx: idx,
		consumed: make([]bool, len(owned)),
		sharedPT: sharedPT,
		sharedS:  sharedS,
		sharedG:  integrations.SharedGen{View: 1, Store: 1},
		stageG:   1,
		spent:    map[string]bool{},
		inFlight: map[string]bool{},
	}
}

func (d *fullRoundTestDb) ExactServing() bool { return true }
func (d *fullRoundTestDb) StagingGeneration() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stageG
}
func (d *fullRoundTestDb) StagingGenerationByKind(models.Kind) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stageG
}
func (d *fullRoundTestDb) MockSpent(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.spent[name]
}
func (d *fullRoundTestDb) SharedGeneration(models.Kind) (integrations.SharedGen, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.test == "" {
		return integrations.SharedGen{}, false
	}
	return d.sharedG, true
}
func (d *fullRoundTestDb) SharedMocks() ([]*models.Mock, []*models.Mock, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.test == "" {
		return nil, nil, false
	}
	return append([]*models.Mock(nil), d.sharedPT...), append([]*models.Mock(nil), d.sharedS...), true
}
func (d *fullRoundTestDb) OwnedUnanswered() ([]*models.Mock, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.test == "" {
		return nil, false
	}
	var out []*models.Mock
	for i, m := range d.owned {
		if !d.consumed[i] {
			out = append(out, m)
		}
	}
	return out, true
}
func (d *fullRoundTestDb) OwnedRecordings() (uint64, []*models.Mock, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.test == "" {
		return 0, nil, false
	}
	return d.ownedGen, d.owned, true
}
func (d *fullRoundTestDb) OwnedUnansweredAt(view uint64, at []int) ([]int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.test == "" || d.ownedGen != view {
		return nil, false
	}
	var out []int
	for _, i := range at {
		if i >= 0 && i < len(d.owned) && !d.consumed[i] {
			out = append(out, i)
		}
	}
	return out, true
}
func (d *fullRoundTestDb) WhenRestaged(_ any, fn func()) {
	d.mu.Lock()
	d.restage = append(d.restage, fn)
	d.mu.Unlock()
}
func (d *fullRoundTestDb) triggerRestage() {
	d.mu.Lock()
	d.epoch++
	d.stageG++
	d.ownedGen++
	d.claimed = nil
	fns := append([]func(){}, d.restage...)
	d.restage = nil
	d.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func (d *fullRoundTestDb) unspentFromSlice(viewOwned []*models.Mock, viewConsumed []bool, posMap, positions []int, from int) (int, int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if from < 0 {
		from = 0
	}
	n := len(positions)
	if positions == nil {
		n = len(viewOwned)
	}
	nextFrom := from
	leading := true
	for i := from; i < n; i++ {
		d.unspentScans++
		p := i
		if positions != nil {
			p = positions[i]
			if posMap != nil {
				if p < 0 || p >= len(posMap) {
					if leading {
						nextFrom = i + 1
					}
					continue
				}
				p = posMap[p]
			}
		}
		if p < 0 || p >= len(viewOwned) || viewConsumed[p] {
			if leading {
				nextFrom = i + 1
			}
			continue
		}
		return i, nextFrom, true
	}
	return 0, nextFrom, false
}

func (d *fullRoundTestDb) RoundMeta(tracker *learn.Tracker) (integrations.RoundMeta, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ps := tracker.PlacedState()
	phase := integrations.RoundBetween
	if d.test != "" && ps.Open {
		phase = integrations.RoundInTest
	} else if d.test != "" && !ps.Open {
		phase = integrations.RoundBoundary
	} else if d.test == "" && ps.Open {
		phase = integrations.RoundBoundary
	}
	viewOwned, viewConsumed, viewSpent := d.owned, d.consumed, d.spent
	meta := integrations.RoundMeta{
		At:         integrations.RoundAt{Epoch: d.epoch, Gen: d.gen, Bounds: d.gen, Taken: d.takenCountLocked()},
		Test:       d.test,
		Phase:      phase,
		Open:       phase == integrations.RoundInTest,
		Background: len(d.bgPool) > 0,
		Learn: integrations.LearnRead{
			Bound:    tracker != nil,
			Number:   ps.Test,
			Open:     ps.Open,
			Begun:    ps.Begun,
			Awaiting: ps.Awaiting,
		},
		LearnHasHTTPPut: ps.HasHTTPPut,
		OwnedGen:        d.ownedGen,
		Owned:           viewOwned,
		HasShared:       d.test != "",
		SharedGen:       d.sharedG,
		StagingGen:      d.stageG,
	}
	return meta.WithMetaParts(integrations.RoundMetaParts{
		ViewGen:    d.gen,
		OwnedIndex: d.ownedIdx,
		Voided:     d.voided,
		InFlight:   func(m *models.Mock) bool { return m != nil && d.inFlight[m.Name] },
		Unspent: func(posMap, positions []int, from int) (int, int, bool) {
			return d.unspentFromSlice(viewOwned, viewConsumed, posMap, positions, from)
		},
		Spent: func(name string) bool {
			d.mu.Lock()
			defer d.mu.Unlock()
			return viewSpent[name]
		},
		BGPool:      d.bgPool,
		Windows:     d.windows,
		WindowStart: d.winStart,
		WindowEnd:   d.winEnd,
	}), true
}

func (d *fullRoundTestDb) takenCountLocked() int {
	n := len(d.spent)
	for _, c := range d.consumed {
		if c {
			n++
		}
	}
	return n
}

func (d *fullRoundTestDb) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.snaps++
	var pt, sess, consumed []*models.Mock
	owners := make(map[string]integrations.RoundOwner, len(d.owned))
	for i, m := range d.owned {
		owners[m.Name] = integrations.OwnerTest
		if d.inFlight[m.Name] {
			owners[m.Name] = integrations.OwnerInFlight
		}
		if d.consumed[i] {
			consumed = append(consumed, m)
		} else {
			pt = append(pt, m)
			sess = append(sess, m)
		}
	}
	for _, m := range d.sharedPT {
		if !d.spent[m.Name] {
			pt = append(pt, m)
		}
	}
	for _, m := range d.sharedS {
		if !d.spent[m.Name] {
			sess = append(sess, m)
		}
	}
	var lr integrations.LearnRead
	if q.Tracker != nil {
		p := q.Tracker.Placed()
		lr = integrations.LearnRead{
			Bound:    true,
			Number:   p.Test,
			Open:     p.Open,
			Begun:    p.Begun,
			Earlier:  p.Earlier,
			Served:   p.Served,
			Before:   p.Before,
			Set:      p.Set,
			Order:    p.Order,
			Awaiting: p.Awaiting,
			Answered: q.Tracker.AnsweredMocks(),
		}
	}
	phase := integrations.RoundBetween
	if d.test != "" && lr.Open {
		phase = integrations.RoundInTest
	} else if d.test != "" != lr.Open {
		phase = integrations.RoundBoundary
	}
	var takes []integrations.Take
	for name := range d.voided {
		if i, ok := d.ownedIdx[name]; ok {
			takes = append(takes, integrations.Take{How: integrations.TakeVoided, Mock: d.owned[i]})
		} else {
			takes = append(takes, integrations.Take{How: integrations.TakeVoided, Mock: &models.Mock{Name: name}})
		}
	}
	snap := integrations.RoundSnapshot{
		At:         integrations.RoundAt{Epoch: d.epoch, Gen: d.gen, Bounds: d.gen, Taken: d.takenCountLocked()},
		Test:       d.test,
		Phase:      phase,
		Open:       phase == integrations.RoundInTest,
		Background: len(d.bgPool) > 0,
		PerTest:    pt,
		Session:    sess,
		Consumed:   consumed,
		Taken:      takes,
		Learn:      lr,
	}
	return snap.WithParts(integrations.RoundParts{
		Owners:         owners,
		BackgroundPool: d.bgPool,
		Windows:        d.windows,
		WindowStart:    d.winStart,
		WindowEnd:      d.winEnd,
		ViewGen:        d.gen,
	}), true
}

func (d *fullRoundTestDb) WithinRound(at integrations.RoundAt) integrations.RoundScope {
	return &fullRoundTestScope{fullRoundTestDb: d, at: at}
}
func (d *fullRoundTestDb) ClaimPairingIn(*models.Mock, integrations.RoundAt) (bool, bool, string) {
	return false, false, ""
}
func (d *fullRoundTestDb) SpendByKindIn(*models.Mock, integrations.RoundAt) (bool, bool, string) {
	return false, false, ""
}
func (d *fullRoundTestDb) TakenLog(uint64, int, int) []integrations.Take { return nil }

// ClaimOnce is the manager's once-per-staging claim (MockManager.ClaimOnce):
// true only for the first claim of key since the fake's last restage, which
// clears the claims as the manager's staging epoch bump does.
func (d *fullRoundTestDb) ClaimOnce(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.claimed[key]; ok {
		return false
	}
	if d.claimed == nil {
		d.claimed = make(map[string]struct{})
	}
	d.claimed[key] = struct{}{}
	return true
}

func (d *fullRoundTestDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*models.Mock
	for i, m := range d.owned {
		if !d.consumed[i] {
			out = append(out, m)
		}
	}
	for _, m := range d.sharedPT {
		if !d.spent[m.Name] {
			out = append(out, m)
		}
	}
	return out, nil
}
func (d *fullRoundTestDb) GetSessionMocks() ([]*models.Mock, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*models.Mock
	for i, m := range d.owned {
		if !d.consumed[i] {
			out = append(out, m)
		}
	}
	for _, m := range d.sharedS {
		if !d.spent[m.Name] {
			out = append(out, m)
		}
	}
	return out, nil
}
func (d *fullRoundTestDb) GetFilteredMocks() ([]*models.Mock, error) {
	return d.GetPerTestMocksInWindow()
}
func (d *fullRoundTestDb) GetFilteredMocksInWindow() ([]*models.Mock, error) {
	return d.GetPerTestMocksInWindow()
}
func (d *fullRoundTestDb) GetUnFilteredMocks() ([]*models.Mock, error) { return d.GetSessionMocks() }
func (d *fullRoundTestDb) GetStartupMocks() ([]*models.Mock, error)    { return nil, nil }
func (d *fullRoundTestDb) GetStartupMocksByKind(models.Kind) ([]*models.Mock, error) {
	return nil, nil
}
func (d *fullRoundTestDb) GetSessionScopedMocks() ([]*models.Mock, error) {
	return d.GetSessionMocks()
}
func (d *fullRoundTestDb) GetConnectionMocks(string) ([]*models.Mock, error) { return nil, nil }
func (d *fullRoundTestDb) SessionMockHitCounts() map[string]uint64           { return nil }
func (d *fullRoundTestDb) DeleteStartupMock(mock models.Mock) bool           { return d.DeleteFilteredMock(mock) }
func (d *fullRoundTestDb) UpdateUnFilteredMock(old *models.Mock, _ *models.Mock) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i, ok := d.ownedIdx[old.Name]; ok {
		if d.consumed[i] {
			return false
		}
		d.consumed[i] = true
		return true
	}
	if d.spent[old.Name] {
		return false
	}
	return true
}
func (d *fullRoundTestDb) DeleteFilteredMock(mock models.Mock) bool {
	d.mu.Lock()
	hook := d.onDelete
	d.mu.Unlock()
	if hook != nil && hook(mock) {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if i, ok := d.ownedIdx[mock.Name]; ok {
		if d.consumed[i] {
			return false
		}
		d.consumed[i] = true
		return true
	}
	if d.spent[mock.Name] {
		return false
	}
	d.spent[mock.Name] = true
	return true
}
func (d *fullRoundTestDb) DeleteUnFilteredMock(mock models.Mock) bool {
	return d.DeleteFilteredMock(mock)
}
func (d *fullRoundTestDb) MarkMockAsUsed(mock models.Mock) bool {
	return d.DeleteFilteredMock(mock)
}
func (d *fullRoundTestDb) GetMySQLCounts() (int, int, int) { return 0, 0, 0 }
func (d *fullRoundTestDb) SetCurrentTestWindow(start, end time.Time) {
	d.mu.Lock()
	d.winStart, d.winEnd = start, end
	d.mu.Unlock()
}
func (d *fullRoundTestDb) IsTestWindowActive() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.test != ""
}
func (d *fullRoundTestDb) HasFirstTestFired() bool { return false }
func (d *fullRoundTestDb) FirstTestWindowStart() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.winStart
}
func (d *fullRoundTestDb) WindowSnapshot() models.WindowSnapshot {
	return models.WindowSnapshot{Active: d.IsTestWindowActive()}
}
func (d *fullRoundTestDb) CurrentTestWindow() (time.Time, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.winStart, d.winEnd
}

type fullRoundTestScope struct {
	*fullRoundTestDb
	at    integrations.RoundAt
	moved string
}

func (s *fullRoundTestScope) Moved() string { return s.moved }
func (s *fullRoundTestScope) DeleteFilteredMock(m models.Mock) bool {
	s.mu.Lock()
	if s.epoch != s.at.Epoch || s.gen != s.at.Gen {
		s.moved = "moved"
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	return s.fullRoundTestDb.DeleteFilteredMock(m)
}
func (s *fullRoundTestScope) UpdateUnFilteredMock(old, new *models.Mock) bool {
	s.mu.Lock()
	if s.epoch != s.at.Epoch || s.gen != s.at.Gen {
		s.moved = "moved"
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	return s.fullRoundTestDb.UpdateUnFilteredMock(old, new)
}

func TestFullRoundExactMatchVisitsOnlyOneRecording(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	for _, size := range []int{100, 2000} {
		t.Run("unique_paths_and_distinct_bodies/n="+strconv.Itoa(size), func(t *testing.T) {
			mocks := make([]*models.Mock, size)
			for i := range mocks {
				if i%2 == 0 {
					mocks[i] = learnRecorded(fmt.Sprintf("mock-%d", i), time.Duration(i)*time.Millisecond, "GET", "/item/"+strconv.Itoa(i/2), "", nil, `{"id":`+strconv.Itoa(i)+`}`)
				} else {
					body := fmt.Sprintf(`{"id":"b-%d","v":%d}`, i, i)
					mocks[i] = learnRecorded(fmt.Sprintf("mock-%d", i), time.Duration(i)*time.Millisecond, "POST", "/item", body, nil, `{"id":`+strconv.Itoa(i)+`}`)
				}
			}
			db := newFullRoundTestDb("T1", mocks, nil, nil)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)

			// First GET builds shared/owned learnIx once (no bucket filing since GET bucket has size 1).
			builds0 := learnIndexBuilds.Load()
			filings0 := learnBodyFilings.Load()
			served, miss, err := h.learnMatch(ctx, live("GET", "/item/0", "", nil), db, n, opts)
			if err != nil || miss != nil || served == nil || served.Name != "mock-0" {
				t.Fatalf("first GET: served=%v miss=%+v err=%v", served, miss, err)
			}
			if got := learnIndexBuilds.Load() - builds0; got != 1 { // shared learnIndex (ownedIndex built alongside)
				t.Fatalf("first GET builds=%d, want 1", got)
			}
			if got := learnBodyFilings.Load() - filings0; got != 0 {
				t.Fatalf("first GET filed %d buckets, want 0", got)
			}

			// Subsequent GET visits exactly 1 recording, builds 0 indexes, takes 0 full snapshots.
			visits0 := fullRoundVisits.Load()
			snaps0 := db.snaps
			builds1 := learnIndexBuilds.Load()
			midGet := (size / 2) &^ 1
			served, miss, err = h.learnMatch(ctx, live("GET", "/item/"+strconv.Itoa(midGet/2), "", nil), db, n, opts)
			if err != nil || miss != nil || served == nil || served.Name != fmt.Sprintf("mock-%d", midGet) {
				t.Fatalf("mid GET: served=%v miss=%+v err=%v", served, miss, err)
			}
			if v := fullRoundVisits.Load() - visits0; v != 1 {
				t.Fatalf("mid GET at n=%d visited %d recordings, want 1", size, v)
			}
			if db.snaps != snaps0 {
				t.Fatalf("mid GET took %d full snapshots, want 0", db.snaps-snaps0)
			}
			if learnIndexBuilds.Load() != builds1 {
				t.Fatalf("mid GET rebuilt index")
			}

			// First POST lazily files the POST /item bucket once.
			filings1 := learnBodyFilings.Load()
			served, miss, err = h.learnMatch(ctx, live("POST", "/item", `{"id":"b-1","v":1}`, nil), db, n, opts)
			if err != nil || miss != nil || served == nil || served.Name != "mock-1" {
				t.Fatalf("first POST: served=%v miss=%+v err=%v", served, miss, err)
			}
			if got := learnBodyFilings.Load() - filings1; got != 1 {
				t.Fatalf("first POST filed %d buckets, want 1", got)
			}

			// Subsequent POST visits exactly 1 recording, files 0 buckets, takes 0 full snapshots.
			visits1 := fullRoundVisits.Load()
			filings2 := learnBodyFilings.Load()
			snaps1 := db.snaps
			midPost := (size/2 | 1)
			served, miss, err = h.learnMatch(ctx, live("POST", "/item", fmt.Sprintf(`{"id":"b-%d","v":%d}`, midPost, midPost), nil), db, n, opts)
			if err != nil || miss != nil || served == nil || served.Name != fmt.Sprintf("mock-%d", midPost) {
				t.Fatalf("mid POST: served=%v miss=%+v err=%v", served, miss, err)
			}
			if v := fullRoundVisits.Load() - visits1; v != 1 {
				t.Fatalf("mid POST at n=%d visited %d recordings, want 1", size, v)
			}
			if learnBodyFilings.Load() != filings2 {
				t.Fatalf("mid POST re-filed bucket")
			}
			if db.snaps != snaps1 {
				t.Fatalf("mid POST took %d full snapshots, want 0", db.snaps-snaps1)
			}

			// Unrecorded path files 0 buckets.
			filings3 := learnBodyFilings.Load()
			served, miss, err = h.learnMatch(ctx, live("POST", "/unrecorded", `{"a":1}`, nil), db, n, opts)
			if err != nil || served != nil || miss == nil {
				t.Fatalf("unrecorded POST: served=%v miss=%+v err=%v", served, miss, err)
			}
			if learnBodyFilings.Load() != filings3 {
				t.Fatalf("unrecorded path filed a bucket")
			}
		})

		t.Run("distinct_queries/n="+strconv.Itoa(size), func(t *testing.T) {
			mocks := make([]*models.Mock, size)
			for i := range mocks {
				mocks[i] = learnRecorded(fmt.Sprintf("q-%d", i), time.Duration(i)*time.Millisecond, "GET", "/items?id="+strconv.Itoa(i), "", nil, `{"q":`+strconv.Itoa(i)+`}`)
			}
			db := newFullRoundTestDb("T1", mocks, nil, nil)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)

			// First query call files the GET /items bucket once.
			if served, miss, err := h.learnMatch(ctx, live("GET", "/items?id=0", "", nil), db, n, opts); err != nil || miss != nil || served == nil || served.Name != "q-0" {
				t.Fatalf("first query: served=%v miss=%+v err=%v", served, miss, err)
			}
			visits0 := fullRoundVisits.Load()
			snaps0 := db.snaps
			mid := size / 2
			served, miss, err := h.learnMatch(ctx, live("GET", "/items?id="+strconv.Itoa(mid), "", nil), db, n, opts)
			if err != nil || miss != nil || served == nil || served.Name != fmt.Sprintf("q-%d", mid) {
				t.Fatalf("mid query: served=%v miss=%+v err=%v", served, miss, err)
			}
			if v := fullRoundVisits.Load() - visits0; v != 1 {
				t.Fatalf("mid query at n=%d visited %d recordings, want 1", size, v)
			}
			if db.snaps != snaps0 {
				t.Fatalf("mid query took %d full snapshots, want 0", db.snaps-snaps0)
			}
		})

		t.Run("identical_repeats_amortised_cursor/n="+strconv.Itoa(size), func(t *testing.T) {
			mocks := make([]*models.Mock, size)
			for i := range mocks {
				mocks[i] = learnRecorded(fmt.Sprintf("same-%d", i), time.Duration(i)*time.Millisecond, "POST", "/same", `{"k":"same"}`, nil, `{"i":`+strconv.Itoa(i)+`}`)
			}
			db := newFullRoundTestDb("T1", mocks, nil, nil)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)

			visits0 := fullRoundVisits.Load()
			snaps0 := db.snaps
			scans0 := db.unspentScans
			for i := 0; i < size; i++ {
				served, miss, err := h.learnMatch(ctx, live("POST", "/same", `{"k":"same"}`, nil), db, n, opts)
				if err != nil || miss != nil || served == nil || served.Name != fmt.Sprintf("same-%d", i) {
					t.Fatalf("identical call %d: served=%v miss=%+v err=%v", i, served, miss, err)
				}
			}
			// Every one of the n identical calls must visit exactly 1 recording
			// and scan O(1) positions thanks to the monotonically advancing cursor.
			if got := fullRoundVisits.Load() - visits0; got != int64(size) {
				t.Fatalf("n=%d identical calls visited %d recordings total, want %d", size, got, size)
			}
			if got := db.unspentScans - scans0; got > 2*size {
				t.Fatalf("n=%d identical calls scanned %d cursor positions, want <= %d (O(1) amortised)", size, got, 2*size)
			}
			if db.snaps != snaps0 {
				t.Fatalf("identical calls took %d full snapshots, want 0", db.snaps-snaps0)
			}
		})

		t.Run("identical_with_wild_mock_merged_cursor/n="+strconv.Itoa(size), func(t *testing.T) {
			mocks := make([]*models.Mock, size+1)
			for i := 0; i < size; i++ {
				mocks[i] = learnRecorded(fmt.Sprintf("same-%d", i), time.Duration(i)*time.Millisecond, "POST", "/same", `{"k":"same"}`, nil, `{"i":`+strconv.Itoa(i)+`}`)
			}
			// Adding one wild mock (m.Noise) on a different path with the same
			// segment count forces matchOpenTestMerged; lazy owned cursor
			// streaming must keep scans and visits O(1) amortised.
			wild := learnRecorded("wild-0", time.Duration(size)*time.Millisecond, "POST", "/other", `{"k":"other"}`, nil, `{"w":1}`)
			wild.Noise = []string{"other"}
			mocks[size] = wild
			db := newFullRoundTestDb("T1", mocks, nil, nil)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)

			visits0 := fullRoundVisits.Load()
			snaps0 := db.snaps
			scans0 := db.unspentScans
			for i := 0; i < size; i++ {
				served, miss, err := h.learnMatch(ctx, live("POST", "/same", `{"k":"same"}`, nil), db, n, opts)
				if err != nil || miss != nil || served == nil || served.Name != fmt.Sprintf("same-%d", i) {
					t.Fatalf("merged identical call %d: served=%v miss=%+v err=%v", i, served, miss, err)
				}
			}
			if got := fullRoundVisits.Load() - visits0; got != int64(size) {
				t.Fatalf("n=%d merged identical calls visited %d recordings, want %d", size, got, size)
			}
			if got := db.unspentScans - scans0; got > 4*size {
				t.Fatalf("n=%d merged identical calls scanned %d cursor positions, want <= %d (O(1) amortised)", size, got, 4*size)
			}
			if db.snaps != snaps0 {
				t.Fatalf("merged identical calls took %d full snapshots, want 0", db.snaps-snaps0)
			}
		})
	}
}

func TestFullRoundPrintSoundnessAndCollisions(t *testing.T) {
	h := New(zap.NewNop()).(*HTTP)
	plain := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, nil)
	withURLNoise := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, []string{`tok-[0-9]+`})
	withBodyNoise := h.newLearnNoise(models.OutgoingOptions{Learn: true}, map[string][]string{"id": {}}, nil)

	partsOf := func(method, target, ct, body string) httpParts {
		u, _ := url.Parse(target)
		hdr := map[string]string{"host": "api.test"}
		if ct != "" {
			hdr["content-type"] = ct
		}
		return httpParts{method: method, host: "api.test", path: u.Path, query: u.Query(), header: hdr, mediaType: mediaTypeOf(ct), body: []byte(body)}
	}

	// Equivalent JSON (key order, whitespace, and 1 vs 1.0) -> identical print.
	p1, ok1 := learnPartsPrint(partsOf("POST", "/x", "application/json", `{"a":1,"b":"hi","c":[true,null,{},[]]}`), plain)
	p2, ok2 := learnPartsPrint(partsOf("POST", "/x", "application/json", `{ "c" : [ true , null , {} , [] ] , "b" : "hi" , "a" : 1.0 }`), plain)
	if !ok1 || !ok2 || p1 != p2 {
		t.Fatalf("equivalent JSON prints differ: (%d,%v) vs (%d,%v)", p1, ok1, p2, ok2)
	}

	// Distinct JSON scalar types & containers -> distinct prints.
	variants := []string{
		`{"v":1}`, `{"v":2}`, `{"v":"1"}`, `{"v":true}`, `{"v":false}`,
		`{"v":null}`, `{"v":{}}`, `{"v":[]}`, `{"v":[1,2]}`, `{"v":[2,1]}`,
	}
	seen := map[uint64]string{}
	for _, body := range variants {
		sum, ok := learnPartsPrint(partsOf("POST", "/x", "application/json", body), plain)
		if !ok {
			t.Fatalf("variant %s unexpectedly refused", body)
		}
		if prev, dup := seen[sum]; dup {
			t.Fatalf("variants %s and %s collided at print %d", prev, body, sum)
		}
		seen[sum] = body
	}

	// Colliding JSON keys ('.' or '[') must return ok=false so they go to rest.
	for _, body := range []string{`{"a.b":1}`, `{"a[0]":1}`, `{"x":{"y.z":2}}`, `{"x":[{"k[1]":3}]}`} {
		if _, ok := learnPartsPrint(partsOf("POST", "/x", "application/json", body), plain); ok {
			t.Fatalf("colliding key body %s returned ok=true, want false", body)
		}
	}

	// Form and multipart bodies must return ok=false on learnLivePrint / learnPlainPrint.
	if _, ok := plain.learnLivePrint(partsOf("POST", "/x", "application/x-www-form-urlencoded", "a=1&b=2")); ok {
		t.Fatal("form body returned ok=true, want false")
	}
	if _, ok := plain.learnLivePrint(partsOf("POST", "/x", "multipart/form-data; boundary=abc", "--abc--\r\n")); ok {
		t.Fatal("multipart body returned ok=true, want false")
	}

	// Non-JSON text body prints by raw bytes and differs from JSON / empty.
	t1, tok1 := learnPartsPrint(partsOf("POST", "/x", "text/plain", "hello"), plain)
	t2, tok2 := learnPartsPrint(partsOf("POST", "/x", "text/plain", "world"), plain)
	e1, eok1 := learnPartsPrint(partsOf("POST", "/x", "text/plain", ""), plain)
	if !tok1 || !tok2 || !eok1 || t1 == t2 || t1 == e1 {
		t.Fatalf("text/empty prints invalid: t1=%d t2=%d e1=%d", t1, t2, e1)
	}

	// Media type differs -> different prints.
	m1, _ := learnPartsPrint(partsOf("POST", "/x", "application/json", "plain"), plain)
	m2, _ := learnPartsPrint(partsOf("POST", "/x", "text/plain", "plain"), plain)
	if m1 == m2 {
		t.Fatalf("different media types collided: %d", m1)
	}

	// Query params: repeated values in any order match; keploy* ignored; url noise masked; distinct values differ.
	q1, _ := learnPartsPrint(partsOf("GET", "/x?tag=b&tag=a&keploy_foo=1", "", ""), plain)
	q2, _ := learnPartsPrint(partsOf("GET", "/x?tag=a&tag=b&KeployBar=2", "", ""), plain)
	q3, _ := learnPartsPrint(partsOf("GET", "/x?tag=a&tag=c", "", ""), plain)
	if q1 != q2 || q1 == q3 {
		t.Fatalf("query prints unexpected: q1=%d q2=%d q3=%d", q1, q2, q3)
	}
	qn1, _ := learnPartsPrint(partsOf("GET", "/x?t=tok-111", "", ""), withURLNoise)
	qn2, _ := learnPartsPrint(partsOf("GET", "/x?t=tok-999", "", ""), withURLNoise)
	if qn1 != qn2 {
		t.Fatalf("url-noise masked query prints differ: %d vs %d", qn1, qn2)
	}
	// Multi-value query params join with NUL in learnNoise.shape; prints must agree.
	nul1 := partsOf("GET", "/x", "", "")
	nul1.query = url.Values{"k": {"a\x00b", "c"}}
	nul2 := partsOf("GET", "/x", "", "")
	nul2.query = url.Values{"k": {"a", "b\x00c"}}
	np1, _ := learnPartsPrint(nul1, plain)
	np2, _ := learnPartsPrint(nul2, plain)
	if np1 != np2 {
		t.Fatalf("NUL-joined query prints differ: %d vs %d", np1, np2)
	}

	// learnPlainPrint refuses mocks with ReqBodyNoise; mocks with Mock.Noise are wide (learnsWide).
	mClean := learnRecorded("m0", 0, "POST", "/x", `{"a":1}`, nil, `{}`)
	if _, ok := learnPlainPrint(mClean, recordedParts(mClean), plain); !ok {
		t.Fatal("clean mock unexpectedly refused")
	}
	mReqNoise := learnRecorded("m1", 0, "POST", "/x", `{"a":1}`, nil, `{}`)
	mReqNoise.Spec.ReqBodyNoise = map[string][]string{"body.a": {}}
	if _, ok := learnPlainPrint(mReqNoise, recordedParts(mReqNoise), plain); ok {
		t.Fatal("mock with ReqBodyNoise returned ok=true")
	}
	mObf := learnRecorded("m2", 0, "POST", "/x", `{"a":1}`, nil, `{}`)
	mObf.Noise = []string{"secret"}
	if !learnsWide(mObf) {
		t.Fatal("mock with Mock.Noise must be learnsWide so every call reads it via wild")
	}

	// learnLivePrint refuses when keploy.yml declares body noise (!n.body.Empty()).
	if _, ok := withBodyNoise.learnLivePrint(partsOf("POST", "/x", "application/json", `{"a":1}`)); ok {
		t.Fatal("live print with keploy.yml body noise returned ok=true")
	}
}

func TestFullRoundBucketFilingCtxCancel(t *testing.T) {
	h := New(zap.NewNop()).(*HTTP)
	n := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, nil)
	mocks := []*models.Mock{
		learnRecorded("m0", 0, "POST", "/item", `{"a":1}`, nil, `{}`),
		learnRecorded("m1", time.Millisecond, "POST", "/item", `{"a":2}`, nil, `{}`),
	}
	ix := buildLearnOwned(mocks, n)
	ranks := ix.byPath["POST item"]
	if len(ranks) != 2 {
		t.Fatalf("missing POST item ranks: %v", ranks)
	}

	// 1. Waiting on turn with cancelled ctx must return ctx.Err() promptly and leave bucket unfiled.
	ix.bodies.turn <- struct{}{} // hold the turn
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ix.filePath(ctx, "POST item", ranks, mocks)
		done <- err
	}()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("got err=%v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned filePath blocked on held turn despite cancelled context")
	}
	<-ix.bodies.turn
	if _, filed := ix.bodies.filed["POST item"]; filed {
		t.Fatal("bucket was marked filed after cancelled wait")
	}

	// 2. Cancelled ctx entering loop must return ctx.Err() and leave bucket unfiled.
	// Repeat 24 times so Go's select chooses turn as well as ctx.Done().
	for attempt := 0; attempt < 24; attempt++ {
		if _, err := ix.filePath(ctx, "POST item", ranks, mocks); err != context.Canceled {
			t.Fatalf("cancelled loop attempt %d got err=%v, want context.Canceled", attempt, err)
		}
		if _, filed := ix.bodies.filed["POST item"]; filed {
			t.Fatalf("bucket was marked filed after cancelled loop attempt %d", attempt)
		}
	}

	// 3. Flat learnIndex bucket cancellation works identically.
	fix := buildLearnIndex(learnIndexKey{gen: 1}, mocks, nil, n)
	fidxs := fix.byPath["POST item"]
	fix.bodies.turn <- struct{}{}
	done2 := make(chan error, 1)
	go func() {
		_, err := fix.filePath(ctx, "POST item", fidxs)
		done2 <- err
	}()
	select {
	case err := <-done2:
		if err != context.Canceled {
			t.Fatalf("flat bucket got err=%v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flat filePath blocked on held turn despite cancelled context")
	}
	<-fix.bodies.turn
	if _, filed := fix.bodies.filed["POST item"]; filed {
		t.Fatal("flat bucket was marked filed after cancelled wait")
	}
	for attempt := 0; attempt < 24; attempt++ {
		if _, err := fix.filePath(ctx, "POST item", fidxs); err != context.Canceled {
			t.Fatalf("flat cancelled loop attempt %d got err=%v, want context.Canceled", attempt, err)
		}
		if _, filed := fix.bodies.filed["POST item"]; filed {
			t.Fatalf("flat bucket was marked filed after cancelled loop attempt %d", attempt)
		}
	}
}

func TestFullRoundReadBackPutSkipAndTrigger(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	// 1. PUT to /other does NOT trigger readBackPlan on GET /item/1.
	putOther := learnRecorded("put-other", 0, http.MethodPut, "/other", `{"name":"rec-other"}`, nil, `{"ok":true}`)
	getItem := learnRecorded("get-item", time.Millisecond, http.MethodGet, "/item/1", "", nil, `{"name":"rec-item"}`)
	db := newFullRoundTestDb("T1", []*models.Mock{putOther, getItem}, nil, nil)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)

	// Serve PUT /other and notify tracker.
	servedPut, miss, err := h.learnMatch(ctx, live(http.MethodPut, "/other", `{"name":"rec-other"}`, nil), db, n, opts)
	if err != nil || miss != nil || servedPut == nil {
		t.Fatalf("PUT /other: served=%v miss=%+v err=%v", servedPut, miss, err)
	}
	tr.ServedMock(putOther)
	if !tr.PlacedState().HasHTTPPut || len(tr.HTTPPuts()) != 1 {
		t.Fatalf("expected 1 HTTP PUT tracked, got state=%+v puts=%d", tr.PlacedState(), len(tr.HTTPPuts()))
	}
	if hasMatchingPut(tr, getItem) {
		t.Fatal("hasMatchingPut returned true for GET /item/1 when only PUT /other was served")
	}

	// GET /item/1 must resolve on the fast path with 0 full snapshots.
	snaps0 := db.snaps
	servedGet, miss, err := h.learnMatch(ctx, live(http.MethodGet, "/item/1", "", nil), db, n, opts)
	if err != nil || miss != nil || servedGet == nil || servedGet.Name != "get-item" {
		t.Fatalf("GET /item/1: served=%v miss=%+v err=%v", servedGet, miss, err)
	}
	if db.snaps != snaps0 {
		t.Fatalf("GET /item/1 with non-matching PUT took %d full snapshots, want 0", db.snaps-snaps0)
	}

	// 2. PUT to /kv/alpha with a drifted value MUST trigger readBackPlan on GET /kv/alpha.
	putKV := learnRecorded("put-kv", 0, http.MethodPut, "/kv/alpha", `{"val":"old-val"}`, nil, `{"ok":true}`)
	getKV := learnRecorded("get-kv", time.Millisecond, http.MethodGet, "/kv/alpha", "", nil, `{"val":"old-val"}`)
	db2 := newFullRoundTestDb("T2", []*models.Mock{putKV, getKV}, nil, nil)
	tr2 := learn.NewTracker()
	tr2.Begin()
	ctx2 := learn.WithTracker(context.Background(), tr2)

	// Drifted PUT /kv/alpha (falls back to learn.Pair, learns drift, records exchange).
	servedPut2, miss2, err := h.learnMatch(ctx2, live(http.MethodPut, "/kv/alpha", `{"val":"live-val"}`, nil), db2, n, opts)
	if err != nil || miss2 != nil || servedPut2 == nil {
		t.Fatalf("drifted PUT /kv/alpha: served=%v miss=%+v err=%v", servedPut2, miss2, err)
	}
	tr2.ServedMock(putKV)
	if !hasMatchingPut(tr2, getKV) {
		t.Fatal("hasMatchingPut returned false for GET /kv/alpha after PUT /kv/alpha")
	}
	servedGet2, miss2, err := h.learnMatch(ctx2, live(http.MethodGet, "/kv/alpha", "", nil), db2, n, opts)
	if err != nil || miss2 != nil || servedGet2 == nil {
		t.Fatalf("GET /kv/alpha: served=%v miss=%+v err=%v", servedGet2, miss2, err)
	}
	if !strings.Contains(servedGet2.Spec.HTTPResp.Body, "live-val") {
		t.Fatalf("GET /kv/alpha answer = %q, want read-back value %q", servedGet2.Spec.HTTPResp.Body, "live-val")
	}

	// 3. Tracker.Begin and Tracker.NewSet clear httpPuts.
	tr2.Begin()
	if tr2.PlacedState().HasHTTPPut || len(tr2.HTTPPuts()) != 0 {
		t.Fatal("Begin did not clear httpPuts")
	}
	tr2.ServedMock(putKV)
	tr2.NewSet()
	if tr2.PlacedState().HasHTTPPut || len(tr2.HTTPPuts()) != 0 {
		t.Fatal("NewSet did not clear httpPuts")
	}
}

func TestFullRoundDeclaredNoiseAndBackgroundPoolFallback(t *testing.T) {
	h, _, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	// 1. Declared noise drift echoed in response MUST fall back from fast path
	// to learnMatchIn's declaredPlan so the response carries the live value.
	nDecl := h.newLearnNoise(opts, map[string][]string{"req_id": {}}, nil)
	mDecl := learnRecorded("decl-0", 0, "POST", "/echo", `{"req_id":"rec-111111","k":"v"}`, nil, `{"echo":"rec-111111"}`)
	dbDecl := newFullRoundTestDb("T1", []*models.Mock{mDecl}, nil, nil)
	trDecl := learn.NewTracker()
	trDecl.Begin()
	ctxDecl := learn.WithTracker(context.Background(), trDecl)
	snaps0 := dbDecl.snaps
	served, miss, err := h.learnMatch(ctxDecl, live("POST", "/echo", `{"req_id":"live-999999","k":"v"}`, nil), dbDecl, nDecl, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("declared-noise call: served=%v miss=%+v err=%v", served, miss, err)
	}
	if dbDecl.snaps == snaps0 {
		t.Fatal("declared-noise drift did not fall back to full learnMatchIn")
	}
	if !strings.Contains(served.Spec.HTTPResp.Body, "live-999999") {
		t.Fatalf("declared-noise response = %q, want echoed live value", served.Spec.HTTPResp.Body)
	}

	// 2. Background pool candidate must NOT win on the fast path in
	// matchOpenTestMerged (falls back so spent own candidates can outrank it).
	nPlain := h.newLearnNoise(opts, nil, nil)
	bgMock := learnRecorded("bg-0", 0, "GET", "/bg", "", nil, `{"bg":true}`)
	bgMock.TestModeInfo.Lifetime = models.LifetimeSession
	dbBG := newFullRoundTestDb("T1", nil, nil, []*models.Mock{bgMock})
	dbBG.bgPool = map[string]struct{}{"bg-0": {}}
	trBG := learn.NewTracker()
	trBG.Begin()
	ctxBG := learn.WithTracker(context.Background(), trBG)
	bgSnaps0 := dbBG.snaps
	servedBG, _, err := h.learnMatch(ctxBG, live("GET", "/bg", "", nil), dbBG, nPlain, opts)
	if err != nil || servedBG == nil {
		t.Fatalf("bg call: served=%v err=%v", servedBG, err)
	}
	if dbBG.snaps == bgSnaps0 {
		t.Fatal("background pool match did not fall back to full learnMatchIn")
	}

	// 3. Per-mock TakeVoided handling: a voided take from another test
	// ("elsewhere") must keep the fast path active (0 snapshots), while a
	// voided take held by the open view ("v-0") must be treated as already
	// answered and not served again.
	vMock := learnRecorded("v-0", 0, "GET", "/v", "", nil, `{"v":1}`)
	dbV := newFullRoundTestDb("T1", []*models.Mock{vMock}, nil, nil)
	dbV.voided = map[string]struct{}{"elsewhere": {}}
	trV := learn.NewTracker()
	trV.Begin()
	ctxV := learn.WithTracker(context.Background(), trV)
	vSnaps0 := dbV.snaps
	if servedV, missV, err := h.learnMatch(ctxV, live("GET", "/v", "", nil), dbV, nPlain, opts); err != nil || missV != nil || servedV == nil || servedV.Name != "v-0" {
		t.Fatalf("unrelated voided call: served=%v miss=%+v err=%v", servedV, missV, err)
	}
	if dbV.snaps != vSnaps0 {
		t.Fatalf("unrelated TakeVoided took %d full snapshots, want 0 (fast path must stay active)", dbV.snaps-vSnaps0)
	}

	vMock2 := learnRecorded("v-void", 0, "GET", "/v", "", nil, `{"v":2}`)
	dbV2 := newFullRoundTestDb("T2", []*models.Mock{vMock2}, nil, nil)
	dbV2.winStart = learnAt.Add(-time.Second)
	dbV2.winEnd = learnAt.Add(time.Minute)
	dbV2.windows = models.NewWindowSchedule([]models.TestWindow{{TestCase: "T2", Start: dbV2.winStart, End: dbV2.winEnd}})
	dbV2.voided = map[string]struct{}{"v-void": {}}
	trV2 := learn.NewTracker()
	trV2.Begin()
	ctxV2 := learn.WithTracker(context.Background(), trV2)
	servedV2, missV2, err := h.learnMatch(ctxV2, live("GET", "/v", "", nil), dbV2, nPlain, opts)
	if err != nil || servedV2 != nil || missV2 == nil {
		t.Fatalf("voided own mock was served: served=%v miss=%+v err=%v, want miss", servedV2, missV2, err)
	}

	// And when a second unvoided mock follows the voided one, the fast path
	// skips the voided mock and serves the unvoided one with 0 snapshots.
	vMock3a := learnRecorded("v-void-a", 0, "GET", "/v", "", nil, `{"v":"void"}`)
	vMock3b := learnRecorded("v-live-b", time.Millisecond, "GET", "/v", "", nil, `{"v":"live"}`)
	dbV3 := newFullRoundTestDb("T3", []*models.Mock{vMock3a, vMock3b}, nil, nil)
	dbV3.winStart = learnAt.Add(-time.Second)
	dbV3.winEnd = learnAt.Add(time.Minute)
	dbV3.windows = models.NewWindowSchedule([]models.TestWindow{{TestCase: "T3", Start: dbV3.winStart, End: dbV3.winEnd}})
	dbV3.voided = map[string]struct{}{"v-void-a": {}}
	trV3 := learn.NewTracker()
	trV3.Begin()
	ctxV3 := learn.WithTracker(context.Background(), trV3)
	v3Snaps0 := dbV3.snaps
	servedV3, missV3, err := h.learnMatch(ctxV3, live("GET", "/v", "", nil), dbV3, nPlain, opts)
	if err != nil || missV3 != nil || servedV3 == nil || servedV3.Name != "v-live-b" {
		t.Fatalf("voided+live call: served=%v miss=%+v err=%v, want v-live-b", servedV3, missV3, err)
	}
	if dbV3.snaps != v3Snaps0 {
		t.Fatalf("voided+live call took %d snapshots, want 0", dbV3.snaps-v3Snaps0)
	}
}

func TestFullRoundRestageReleasesIndexAndPoolReuse(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	// 1. Restaging releases cached owned and shared indexes.
	m0 := learnRecorded("m0", 0, "POST", "/a", `{"k":0}`, nil, `{"v":0}`)
	m1 := learnRecorded("m1", time.Millisecond, "POST", "/a", `{"k":1}`, nil, `{"v":1}`)
	db := newFullRoundTestDb("T1", []*models.Mock{m0, m1}, nil, nil)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	if served, miss, err := h.learnMatch(ctx, live("POST", "/a", `{"k":0}`, nil), db, n, opts); err != nil || miss != nil || served == nil {
		t.Fatalf("initial call failed: served=%v miss=%+v err=%v", served, miss, err)
	}
	if h.learnIx.owned.last == nil || h.learnIx.shared.last == nil {
		t.Fatal("expected owned and shared learnIx indexes to be populated")
	}
	// Replay's learnedGroupOf on the same view must reuse the EXACT same
	// learnIx owned+shared indexes and filed bodyBucket with 0 new builds
	// and 0 new bucket filings.
	replayBuilds0 := learnIndexBuilds.Load()
	replayFilings0 := learnBodyFilings.Load()
	lp := liveParts(ctx, live("POST", "/a", `{"k":1}`, nil))
	if grp, _, err := h.learnedGroupOf(ctx, db, db, tr, lp, n.shape(lp, sameDest), n, false); err != nil || len(grp) != 2 || grp[0].Name != "m1" || grp[1].Name != "m1" {
		t.Fatalf("learnedGroupOf got grp=%v err=%v; want [m1 m1]", strictNames(grp), err)
	}
	if learnIndexBuilds.Load() != replayBuilds0 || learnBodyFilings.Load() != replayFilings0 {
		t.Fatalf("replay rebuilt or re-filed shared learnIx: builds=%d filings=%d",
			learnIndexBuilds.Load()-replayBuilds0, learnBodyFilings.Load()-replayFilings0)
	}
	db.triggerRestage()
	if h.learnIx.owned.last != nil || h.learnIx.shared.last != nil {
		t.Fatal("expected restage to release cached learnIx indexes")
	}

	// 2. Pool view builds pool index on 2nd call at same StagingGen and reuses it.
	poolMocks := []*models.Mock{
		learnRecorded("p0", 0, "GET", "/p/0", "", nil, `{"p":0}`),
		learnRecorded("p1", time.Millisecond, "GET", "/p/1", "", nil, `{"p":1}`),
		learnRecorded("p2", 2*time.Millisecond, "GET", "/p/2", "", nil, `{"p":2}`),
	}
	pdb := newFullRoundTestDb("", nil, nil, poolMocks)
	ptr := learn.NewTracker()
	pctx := learn.WithTracker(context.Background(), ptr)

	// Call 1 sees gen=1 for first time -> falls back (does not build pool index yet).
	if served, miss, err := h.learnMatch(pctx, live("GET", "/p/0", "", nil), pdb, n, opts); err != nil || miss != nil || served == nil || served.Name != "p0" {
		t.Fatalf("pool call 0: served=%v miss=%+v err=%v", served, miss, err)
	}
	// Call 2 builds pool index once and serves via fast path.
	builds0 := learnIndexBuilds.Load()
	if served, miss, err := h.learnMatch(pctx, live("GET", "/p/1", "", nil), pdb, n, opts); err != nil || miss != nil || served == nil || served.Name != "p1" {
		t.Fatalf("pool call 1: served=%v miss=%+v err=%v", served, miss, err)
	}
	if learnIndexBuilds.Load()-builds0 != 1 {
		t.Fatalf("pool call 1 builds=%d, want 1", learnIndexBuilds.Load()-builds0)
	}
	// Call 3 reuses pool index with 0 builds, 0 snapshots, 1 visit.
	builds1 := learnIndexBuilds.Load()
	snaps1 := pdb.snaps
	visits1 := fullRoundVisits.Load()
	if served, miss, err := h.learnMatch(pctx, live("GET", "/p/2", "", nil), pdb, n, opts); err != nil || miss != nil || served == nil || served.Name != "p2" {
		t.Fatalf("pool call 2: served=%v miss=%+v err=%v", served, miss, err)
	}
	if learnIndexBuilds.Load() != builds1 || pdb.snaps != snaps1 || fullRoundVisits.Load()-visits1 != 1 {
		t.Fatalf("pool call 2: builds=%d snaps=%d visits=%d, want 0/0/1",
			learnIndexBuilds.Load()-builds1, pdb.snaps-snaps1, fullRoundVisits.Load()-visits1)
	}
	pdb.triggerRestage()
	if h.learnIx.last != nil {
		t.Fatal("expected restage to release pool index")
	}
}

// TestFullRoundSharedIndexDoesNotSkipAfterViewChange verifies that sharedIx
// (reused across sequential tests T1 -> T2 under the same SharedGen) never
// advances a permanent cursor past a recording spent only in T1's view.
func TestFullRoundSharedIndexDoesNotSkipAfterViewChange(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	m0 := learnRecorded("sh-0", 0, "GET", "/shared", "", nil, `{"v":0}`)
	m0.TestModeInfo.Lifetime = models.LifetimeSession
	m1 := learnRecorded("sh-1", time.Millisecond, "GET", "/shared", "", nil, `{"v":1}`)
	m1.TestModeInfo.Lifetime = models.LifetimeSession
	db := newFullRoundTestDb("T1", nil, nil, []*models.Mock{m0, m1})
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)

	// Call 1 in T1: serves sh-0.
	served, miss, err := h.learnMatch(ctx, live("GET", "/shared", "", nil), db, n, opts)
	if err != nil || miss != nil || served == nil || served.Name != "sh-0" {
		t.Fatalf("T1 call 1: served=%v miss=%+v err=%v", served, miss, err)
	}
	// Mark sh-0 spent in T1's per-view spent map (simulating delete door in T1)
	// without bumping SharedGen, then call again in T1 so sharedIx scans past sh-0.
	db.mu.Lock()
	db.spent["sh-0"] = true
	db.mu.Unlock()
	served, miss, err = h.learnMatch(ctx, live("GET", "/shared", "", nil), db, n, opts)
	if err != nil || miss != nil || served == nil || served.Name != "sh-1" {
		t.Fatalf("T1 call 2: served=%v miss=%+v err=%v", served, miss, err)
	}
	// Open T2 with a fresh view (per-view spent cleared) and SAME SharedGen:
	// T2 MUST see sh-0 again, never skip it via a stale sharedIx cursor.
	db.mu.Lock()
	db.test = "T2"
	db.gen++
	db.ownedGen++
	db.spent = map[string]bool{}
	db.mu.Unlock()
	tr.End()
	tr.Begin()
	served, miss, err = h.learnMatch(ctx, live("GET", "/shared", "", nil), db, n, opts)
	if err != nil || miss != nil || served == nil || served.Name != "sh-0" {
		t.Fatalf("T2 call 1: served=%v miss=%+v err=%v, want sh-0", served, miss, err)
	}
}

type noRoundMetaTestDb struct{ *fullRoundTestDb }

func (noRoundMetaTestDb) RoundMeta(*learn.Tracker) (integrations.RoundMeta, bool) {
	return integrations.RoundMeta{}, false
}

// TestFullRoundClaimMovedUnderFastPathMissesInsteadOfServingNextTest verifies
// that when h.claim fails on pass 1 because the test ended and the next test
// opened under the claim, tryExactLearnMatch pins pass 1's round and lazily
// builds the exact same diagnostic pool (including owned, shared per-test, and
// shared session variable recordings, excluding consumed/spent ones, in
// recorded order with per-test + session tiers) as the full path.
func TestFullRoundClaimMovedUnderFastPathMissesInsteadOfServingNextTest(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	run := func(fast bool) ([]string, int, int64) {
		mDone := learnRecorded("t1-done", 0, "GET", "/item/0", "", nil, `{"test":"done"}`)
		mT1 := learnRecorded("t1-get", time.Millisecond, "GET", "/item/1", "", nil, `{"test":"T1"}`)
		mVar := learnRecorded("t1-var", 2*time.Millisecond, "GET", "/item/2", "", nil, `{"test":"var"}`)
		mVar.Spec.Variables = models.MockVariables{"v": {Recorded: "2", Use: []string{"url.path[1]"}}}
		mShPTVar := learnRecorded("sh-pt-var", 2500*time.Microsecond, "GET", "/shared/varpt", "", nil, `{"sh":"vpt"}`)
		mShPTVar.Spec.Variables = models.MockVariables{"vp": {Recorded: "varpt", Use: []string{"url.path[1]"}}}
		mShSpentT1 := learnRecorded("sh-spent-t1", 3*time.Millisecond, "GET", "/shared/0", "", nil, `{"sh":0}`)
		mShSpentT1.TestModeInfo.Lifetime = models.LifetimeSession
		mShLiveT1 := learnRecorded("sh-live-t1", 4*time.Millisecond, "GET", "/shared/1", "", nil, `{"sh":1}`)
		mShLiveT1.TestModeInfo.Lifetime = models.LifetimeSession
		mShSessVar := learnRecorded("sh-sess-var", 4500*time.Microsecond, "GET", "/shared/varsess", "", nil, `{"sh":"vs"}`)
		mShSessVar.TestModeInfo.Lifetime = models.LifetimeSession
		mShSessVar.Spec.Variables = models.MockVariables{"vs": {Recorded: "varsess", Use: []string{"url.path[1]"}}}
		mT2 := learnRecorded("t2-get", 5*time.Millisecond, "GET", "/item/1", "", nil, `{"test":"T2"}`)
		db := newFullRoundTestDb("T1", []*models.Mock{mDone, mT1, mVar}, []*models.Mock{mShPTVar}, []*models.Mock{mShSpentT1, mShLiveT1, mShSessVar})
		db.consumed[0] = true          // t1-done answered earlier in T1
		db.spent["sh-spent-t1"] = true // sh-spent-t1 spent only in T1's view
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)

		moved := false
		db.onDelete = func(models.Mock) bool {
			if moved {
				return false
			}
			moved = true
			// Simulate T1 ending and T2 opening right before claim commits,
			// with T2's fresh per-view spent map (sh-spent-t1 unspent in T2,
			// sh-live-t1 spent in T2). buildRetakePool MUST read T1's spent map.
			db.mu.Lock()
			db.test = "T2"
			db.gen++
			db.ownedGen++
			db.owned = []*models.Mock{mT2}
			db.ownedIdx = map[string]int{"t2-get": 0}
			db.consumed = []bool{false}
			db.spent = map[string]bool{"sh-live-t1": true}
			db.mu.Unlock()
			tr.End()
			tr.Begin()
			return true
		}

		var store integrations.MockMemDb = db
		if !fast {
			store = noRoundMetaTestDb{db}
		}
		builds0 := fullRoundRetakePoolBuilds.Load()
		served, miss, err := h.learnMatch(ctx, live("GET", "/item/1", "", nil), store, n, opts)
		builds := fullRoundRetakePoolBuilds.Load() - builds0
		if err != nil {
			t.Fatalf("fast=%v unexpected err: %v", fast, err)
		}
		if served != nil {
			t.Fatalf("fast=%v served %s from next test T2 after round moved under claim, want moved-round miss", fast, served.Name)
		}
		if miss == nil || !miss.moved {
			t.Fatalf("fast=%v expected moved-round miss, got miss=%+v", fast, miss)
		}
		names := make([]string, len(miss.pool))
		for i, m := range miss.pool {
			names[i] = m.Name
		}
		return names, len(miss.pool), builds
	}

	fastPool, fastCount, fastBuilds := run(true)
	refPool, refCount, _ := run(false)
	if fastBuilds != 1 {
		t.Fatalf("expected 1 lazy retakePool build on moved miss, got %d", fastBuilds)
	}
	if fastCount != refCount {
		t.Fatalf("moved-miss diagnostic pool count differs: fast=%v (%d), ref=%v (%d)", fastPool, fastCount, refPool, refCount)
	}
	for i := range fastPool {
		if fastPool[i] != refPool[i] {
			t.Fatalf("moved-miss diagnostic pool differs at %d: fast=%v, ref=%v", i, fastPool, refPool)
		}
	}
}

// TestFullRoundContendedRefusalDoesNotBuildRetakePool verifies that a refused
// first-pass claim (e.g. a sibling call racing for the same identical recording
// in the same round) that retries and succeeds NEVER materialises retakePool,
// keeping contended bursts of identical requests O(1) instead of O(owned+flat).
func TestFullRoundContendedRefusalDoesNotBuildRetakePool(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	// 1. Deterministic single-call refusal where pass 1's claim of same-0 is
	// refused (simulating a sibling claiming same-0) while T1 stays open, and
	// pass 2 retakes and serves same-1 on the fast path.
	owned := make([]*models.Mock, 64)
	for i := range owned {
		owned[i] = learnRecorded(fmt.Sprintf("same-%02d", i), time.Duration(i)*time.Millisecond, "POST", "/same", `{"k":"dup"}`, nil, fmt.Sprintf(`{"s":%d}`, i))
	}
	db := newFullRoundTestDb("T1", owned, nil, nil)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)

	refusedOnce := false
	db.onDelete = func(mk models.Mock) bool {
		if !refusedOnce && mk.Name == "same-00" {
			refusedOnce = true
			// Sibling won same-00 in the same round T1.
			db.mu.Lock()
			db.consumed[0] = true
			db.mu.Unlock()
			return true
		}
		return false
	}

	builds0 := fullRoundRetakePoolBuilds.Load()
	snaps0 := db.snaps
	served, miss, err := h.learnMatch(ctx, live("POST", "/same", `{"k":"dup"}`, nil), db, n, opts)
	builds := fullRoundRetakePoolBuilds.Load() - builds0
	if err != nil || miss != nil || served == nil || served.Name != "same-01" {
		t.Fatalf("contended retake: served=%v miss=%+v err=%v, want same-01", served, miss, err)
	}
	if !refusedOnce {
		t.Fatal("expected pass-1 claim of same-00 to be refused once")
	}
	if builds != 0 {
		t.Fatalf("contended refusal that succeeded on retake built retakePool %d times, want 0", builds)
	}
	if db.snaps != snaps0 {
		t.Fatalf("contended retake fell back to %d snapshots, want 0", db.snaps-snaps0)
	}

	// 2. Concurrent burst of identical calls racing on the same cursor head:
	// every call succeeds in T1 with 0 retakePool builds.
	const burst = 24
	burstOwned := make([]*models.Mock, burst)
	for i := range burstOwned {
		burstOwned[i] = learnRecorded(fmt.Sprintf("burst-%02d", i), time.Duration(i)*time.Millisecond, "POST", "/burst", `{"k":"dup"}`, nil, fmt.Sprintf(`{"b":%d}`, i))
	}
	bdb := newFullRoundTestDb("T2", burstOwned, nil, nil)
	btr := learn.NewTracker()
	btr.Begin()
	bctx := learn.WithTracker(context.Background(), btr)
	bBuilds0 := fullRoundRetakePoolBuilds.Load()
	errs := make(chan error, burst)
	for i := 0; i < burst; i++ {
		go func() {
			s, m, e := h.learnMatch(bctx, live("POST", "/burst", `{"k":"dup"}`, nil), bdb, n, opts)
			if e != nil {
				errs <- e
				return
			}
			if m != nil || s == nil {
				errs <- fmt.Errorf("unexpected miss=%+v served=%v", m, s)
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < burst; i++ {
		if e := <-errs; e != nil {
			t.Fatalf("concurrent burst call failed: %v", e)
		}
	}
	if got := fullRoundRetakePoolBuilds.Load() - bBuilds0; got != 0 {
		t.Fatalf("concurrent identical burst built retakePool %d times, want 0", got)
	}
}

// TestFullRoundPoolCursorDoesNotSkipDriftedFirstRecording pins poolIndexSource's
// cursor monotonicity: when the first recording in a same-print pool bucket
// does not match (e.g. header differs) and later recordings in the bucket match,
// the pool cursor must NOT advance past the unspent first recording.
func TestFullRoundPoolCursorDoesNotSkipDriftedFirstRecording(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	p0 := learnRecorded("p0-special", 0, "POST", "/pool-same", `{"k":"v"}`, map[string]string{"X-Id": "special"}, `{"p":0}`)
	p1 := learnRecorded("p1-common", time.Millisecond, "POST", "/pool-same", `{"k":"v"}`, map[string]string{"X-Id": "common"}, `{"p":1}`)
	p2 := learnRecorded("p2-common", 2*time.Millisecond, "POST", "/pool-same", `{"k":"v"}`, map[string]string{"X-Id": "common"}, `{"p":2}`)
	pdb := newFullRoundTestDb("", []*models.Mock{p0, p1, p2}, nil, nil)
	// Put all 3 in sharedPT (pool per-test mocks, consumed on serve).
	pdb.owned, pdb.ownedIdx, pdb.consumed = nil, nil, nil
	pdb.sharedPT = []*models.Mock{p0, p1, p2}
	ptr := learn.NewTracker()
	pctx := learn.WithTracker(context.Background(), ptr)

	// Call 1 sees gen=1 for first time (falls back, serves p1-common).
	s1, m1, err := h.learnMatch(pctx, live("POST", "/pool-same", `{"k":"v"}`, map[string]string{"X-Id": "common"}), pdb, n, opts)
	if err != nil || m1 != nil || s1 == nil || s1.Name != "p1-common" {
		t.Fatalf("call 1: served=%v miss=%+v err=%v, want p1-common", s1, m1, err)
	}
	// Call 2 builds pool index (2nd call at gen=1), skips unspent p0-special
	// (header mismatch) and serves p2-common. Cursor MUST NOT advance past p0-special!
	s2, m2, err := h.learnMatch(pctx, live("POST", "/pool-same", `{"k":"v"}`, map[string]string{"X-Id": "common"}), pdb, n, opts)
	if err != nil || m2 != nil || s2 == nil || s2.Name != "p2-common" {
		t.Fatalf("call 2: served=%v miss=%+v err=%v, want p2-common", s2, m2, err)
	}
	// Call 3 asks for X-Id: special -> MUST still find and serve p0-special on fast path!
	snaps0 := pdb.snaps
	s3, m3, err := h.learnMatch(pctx, live("POST", "/pool-same", `{"k":"v"}`, map[string]string{"X-Id": "special"}), pdb, n, opts)
	if err != nil || m3 != nil || s3 == nil || s3.Name != "p0-special" {
		t.Fatalf("call 3: served=%v miss=%+v err=%v, want p0-special", s3, m3, err)
	}
	if pdb.snaps != snaps0 {
		t.Fatalf("call 3 fell back (%d snapshots), pool cursor wrongly skipped p0-special", pdb.snaps-snaps0)
	}
}

// TestFullRoundLearnAwaitingFallsBack verifies that while learn.Tracker awaits
// the test's request (LearnAwaiting == true), tryExactLearnMatch falls back to
// learnMatchIn's AwaitRequest wait instead of serving prematurely.
func TestFullRoundLearnAwaitingFallsBack(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	m0 := learnRecorded("await-0", 0, "GET", "/await", "", nil, `{"ok":true}`)
	db := newFullRoundTestDb("T1", []*models.Mock{m0}, nil, nil)
	tr := learn.NewTracker()
	tr.Begin()
	tr.Await()

	ctx, cancel := context.WithTimeout(learn.WithTracker(context.Background(), tr), 30*time.Millisecond)
	defer cancel()
	served, _, err := h.learnMatch(ctx, live("GET", "/await", "", nil), db, n, opts)
	if err == nil || served != nil {
		t.Fatalf("expected wait/deadline error while LearnAwaiting=true, got served=%v err=%v", served, err)
	}
}

// TestFullRoundOwnedOutsideWindowFallsBackToPool verifies that matchOwnedDirect
// honors ownership/window checks and does not serve an owned recording placed
// outside every recorded test window as the open test's own.
func TestFullRoundOwnedOutsideWindowFallsBackToPool(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	// Recording timestamp is learnAt (before test window [learnAt+10ms, learnAt+20ms]),
	// so ownership() classifies mOut as pool (own=false) rather than test's own.
	mOut := learnRecorded("out-0", 0, "GET", "/outside", "", nil, `{"out":true}`)
	db := newFullRoundTestDb("T1", []*models.Mock{mOut}, nil, nil)
	db.winStart = learnAt.Add(10 * time.Millisecond)
	db.winEnd = learnAt.Add(20 * time.Millisecond)
	db.windows = models.NewWindowSchedule([]models.TestWindow{
		{TestCase: "T1", Start: db.winStart, End: db.winEnd},
	})
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)

	snaps0 := db.snaps
	served, miss, err := h.learnMatch(ctx, live("GET", "/outside", "", nil), db, n, opts)
	if err != nil || miss != nil || served == nil || served.Name != "out-0" {
		t.Fatalf("outside-window call: served=%v miss=%+v err=%v", served, miss, err)
	}
	if db.snaps == snaps0 {
		t.Fatal("matchOwnedDirect served an outside-window recording as own without falling back")
	}
}

func TestFullRoundMergedAndFallbackEquivalence(t *testing.T) {
	// Exercise ExactViewVerify cross-checking over randomized mixes of:
	// owned mocks, shared mocks, wild (obfuscation/ReqBodyNoise) mocks,
	// colliding JSON keys, form bodies, declared noise, and out-of-order calls.
	h, _, _ := learnOn(t)
	wasVerify := integrations.ExactViewVerify.Load()
	integrations.ExactViewVerify.Store(true)
	t.Cleanup(func() { integrations.ExactViewVerify.Store(wasVerify) })

	rng := rand.New(rand.NewSource(42))
	paths := []string{"/item/1", "/item/2", "/item", "/v1/items?a=1&b=2", "/v1/items?b=2&a=1"}
	bodies := []string{
		`{"a":1,"b":"x"}`,
		`{"b":"x","a":1}`,
		`{"a.b":1}`,
		`{"a":{"b":1}}`,
		`{"a":2,"b":"y"}`,
		`not-json-text`,
		``,
	}
	opts := models.OutgoingOptions{Learn: true, LearnPool: true}

	for round := 0; round < 60; round++ {
		var bodyNoise map[string][]string
		if round%5 == 0 {
			bodyNoise = map[string][]string{"b": {}}
		}
		var urlNoise []string
		if round%7 == 0 {
			urlNoise = []string{`v[0-9]+`}
		}
		n := h.newLearnNoise(opts, bodyNoise, urlNoise)

		var owned, shared []*models.Mock
		for i := 0; i < 6+rng.Intn(8); i++ {
			method := []string{"GET", "POST"}[rng.Intn(2)]
			path := paths[rng.Intn(len(paths))]
			body := ""
			if method == "POST" {
				body = bodies[rng.Intn(len(bodies))]
			}
			m := learnRecorded(fmt.Sprintf("r%d-m%d", round, i), time.Duration(rng.Intn(5))*time.Millisecond, method, path, body, nil, fmt.Sprintf(`{"m":%d}`, i))
			switch rng.Intn(7) {
			case 0:
				m.Spec.ReqBodyNoise = map[string][]string{"body.a": {}}
			case 1:
				m.Noise = []string{"x"}
			case 2:
				m.Spec.Metadata["type"] = "config"
				m.TestModeInfo.Lifetime = models.LifetimeSession
				shared = append(shared, m)
				continue
			}
			owned = append(owned, m)
		}
		db := newFullRoundTestDb(fmt.Sprintf("T%d", round), owned, nil, shared)
		tr := learn.NewTracker()
		tr.Begin()
		ctx := learn.WithTracker(context.Background(), tr)

		for c := 0; c < 6; c++ {
			method := []string{"GET", "POST"}[rng.Intn(2)]
			path := paths[rng.Intn(len(paths))]
			body := ""
			if method == "POST" {
				body = bodies[rng.Intn(len(bodies))]
			}
			// ExactViewVerify panics on ANY divergence between fast path and reference full path.
			served, _, err := h.learnMatch(ctx, live(method, path, body, nil), db, n, opts)
			if err != nil {
				t.Fatalf("round %d call %d err: %v", round, c, err)
			}
			if served != nil {
				tr.ServedMock(served)
			}
		}
	}
}

// TestLearnIndexMemory100k measures retained heap of the single shared
// learnOwned and learnIndex structures (built and with multi-entry buckets
// filed) at 10,000 and 100,000 recordings across paths, bodies, and identical
// request shapes. Gated by KEPLOY_MEASURE_MEM=1 so normal test runs stay fast.
func TestLearnIndexMemory100k(t *testing.T) {
	if os.Getenv("KEPLOY_MEASURE_MEM") != "1" {
		t.Skip("set KEPLOY_MEASURE_MEM=1 to measure 10k/100k index memory")
	}
	h := newHTTP()
	n := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, nil)
	at := time.Unix(1_700_000_000, 0)
	heapBytes := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	mb := func(a, b uint64) string { return fmt.Sprintf("%.2fMB", (float64(b)-float64(a))/(1<<20)) }
	for _, size := range []int{10_000, 100_000} {
		for _, shape := range []string{"paths", "bodies", "identical"} {
			before := heapBytes()
			mocks := make([]*models.Mock, size)
			for i := range mocks {
				method, path, body := "GET", "/item/"+strconv.Itoa(i), ""
				switch shape {
				case "bodies":
					method, path, body = "POST", "/item", fmt.Sprintf(`{"id":"b%07d","pad":"0123456789abcdef0123456789abcdef","r":%d}`, i, i)
				case "identical":
					method, path, body = "POST", "/same", `{"kind":"same","pad":"0123456789abcdef0123456789abcdef"}`
				}
				hdr := map[string]string{"Host": "upstream", "User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}
				if body != "" {
					hdr["Content-Type"] = "application/json"
				}
				mocks[i] = &models.Mock{Name: "mock-" + strconv.Itoa(i), Kind: models.HTTP, Spec: models.MockSpec{
					Metadata:         map[string]string{"type": models.HTTPClient},
					HTTPReq:          &models.HTTPReq{Method: models.Method(method), URL: "http://upstream" + path, Header: hdr, Body: body},
					HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`},
					ReqTimestampMock: at.Add(time.Duration(i) * time.Microsecond),
				}}
			}
			afterMocks := heapBytes()
			for _, m := range mocks {
				_ = recordedParts(m)
			}
			afterParsed := heapBytes()
			own := buildLearnOwned(mocks, n)
			afterOwned := heapBytes()
			for k, ps := range own.byPath {
				if len(ps) > 1 {
					if _, err := own.filePath(context.Background(), k, ps, mocks); err != nil {
						t.Fatal(err)
					}
				}
			}
			afterOwnedFiled := heapBytes()
			flat := buildLearnIndex(learnIndexKey{gen: 1}, mocks, nil, n)
			afterFlat := heapBytes()
			for k, ps := range flat.byPath {
				if len(ps) > 1 {
					if _, err := flat.filePath(context.Background(), k, ps); err != nil {
						t.Fatal(err)
					}
				}
			}
			afterFlatFiled := heapBytes()
			t.Logf("n=%d shape=%-9s mocks=%s parsed=%s owned=%s owned+filed=%s flat=%s flat+filed=%s buckets=%d",
				size, shape, mb(before, afterMocks), mb(afterMocks, afterParsed),
				mb(afterParsed, afterOwned), mb(afterParsed, afterOwnedFiled),
				mb(afterOwnedFiled, afterFlat), mb(afterOwnedFiled, afterFlatFiled), len(own.byPath))
			runtime.KeepAlive(own)
			runtime.KeepAlive(flat)
			runtime.KeepAlive(mocks)
		}
	}
}
