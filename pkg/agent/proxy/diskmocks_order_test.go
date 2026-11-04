package proxy

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// tiedRecording is a mock file as a recorder writes it: three connections,
// each written whole in turn, each connection's 60 mocks in three bursts of
// 20 that share one request time (the frames of one client write are all
// read at that write's time), the connections' bursts interleaving in time,
// so the file is not in time order and every time holds 20 mocks. Each
// mock's response comes a millisecond after its request, so the mocks of one
// burst share their answer's time too. Every mock is per-test, as the
// on-disk store holds them.
func tiedRecording() []*models.Mock {
	var out []*models.Mock
	for c := 0; c < 3; c++ {
		for b := 0; b < 3; b++ {
			at := time.Unix(int64(100+3*b+2-c), 0).UTC()
			for i := 0; i < 20; i++ {
				m := &models.Mock{
					Name: fmt.Sprintf("c%d-b%d-%02d", c, b, i),
					Kind: models.HTTP,
					Spec: models.MockSpec{
						ReqTimestampMock: at,
						ResTimestampMock: at.Add(time.Millisecond),
						Metadata:         map[string]string{"half": [2]string{"even", "odd"}[i%2]},
					},
					TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
				}
				out = append(out, m)
			}
		}
	}
	return out
}

// recordedOrder is the mocks of rec by at, the ones of one time in the order
// the file holds them: the order each read of the store must give them in.
func recordedOrder(rec []*models.Mock, at func(*models.Mock) time.Time) []string {
	sorted := append([]*models.Mock(nil), rec...)
	sort.SliceStable(sorted, func(i, j int) bool { return at(sorted[i]).Before(at(sorted[j])) })
	out := make([]string, len(sorted))
	for i, m := range sorted {
		out[i] = m.Name
	}
	return out
}

func requestTime(m *models.Mock) time.Time { return m.Spec.ReqTimestampMock }
func answerTime(m *models.Mock) time.Time  { return m.Spec.ResTimestampMock }

// sameOrder fails t unless got is want, saying how many places differ and
// the first that does.
func sameOrder(t *testing.T, read string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s read %d mocks; want %d: %v", read, len(got), len(want), got)
	}
	off, first := 0, -1
	for i := range want {
		if got[i] != want[i] {
			off++
			if first < 0 {
				first = i
			}
		}
	}
	if off > 0 {
		t.Fatalf("%s read %d of %d mocks out of their recorded order; the first at %d is %s, want %s",
			read, off, len(want), first, got[first], want[first])
	}
}

// Every read of the on-disk store gives the mocks that share a time in the
// order the mock file holds them, the order they were recorded in, whatever
// order the file holds the times in: a parser answers a repeated command
// from its recordings in turn (each read of a counter, one after another),
// so a pipeline of one client write, all of whose frames share one request
// time, is answered in the order it was recorded only if its mocks come out
// of the store in that order. The agent adds the mocks in the file's order
// and finalizes the store before any read (Agent.StoreMocks), as each case
// does.
func TestEveryReadOfTheDiskStoreKeepsTheRecordedOrderOfMocksThatShareATime(t *testing.T) {
	window := func(s *DiskMocks) ([]*models.Mock, error) {
		return s.LoadWindow(time.Unix(0, 0), time.Unix(1000, 0))
	}
	walk := func(s *DiskMocks, rangeOver func(visit func(name string, load func() (*models.Mock, error)) bool) error) ([]string, error) {
		var out []string
		err := rangeOver(func(name string, _ func() (*models.Mock, error)) bool {
			out = append(out, name)
			return true
		})
		return out, err
	}
	loaded := func(load func(*DiskMocks) ([]*models.Mock, error)) func(*DiskMocks) ([]string, error) {
		return func(s *DiskMocks) ([]string, error) {
			ms, err := load(s)
			return mockNames(ms), err
		}
	}
	all := func(*models.Mock) bool { return true }
	odd := func(m *models.Mock) bool { return m.Spec.Metadata["half"] == "odd" }
	// The rules a case reads under: each sets one and returns its undo.
	keyedByHalf := func() func() {
		return models.RegisterRequestKeys(models.HTTP, func(m *models.Mock) []string { return []string{m.Spec.Metadata["half"]} })
	}
	proven := func() func() {
		provenswitch.Pin(true)
		return provenswitch.Unpin
	}
	// oddAtAnswer places half of each burst at its answer, a millisecond
	// after its request, which sorts them after the burst's other half: a
	// read by place and one by request time differ in every burst.
	oddAtAnswer := func() func() { return models.RegisterAnswerPlaced(models.HTTP, odd) }
	// with combines rules into one, which sets each in turn and returns
	// their undo, the last first.
	with := func(rules ...func() func()) func() func() {
		return func() func() {
			undo := make([]func(), len(rules))
			for i, rule := range rules {
				undo[i] = rule()
			}
			return func() {
				for i := len(undo) - 1; i >= 0; i-- {
					undo[i]()
				}
			}
		}
	}
	// The keyed walk gathers each key's mocks in turn: a burst's odd ones
	// before its even ones.
	keyed := func(s *DiskMocks) ([]string, error) {
		return walk(s, func(visit func(string, func() (*models.Mock, error)) bool) error {
			return s.RangeKeys(models.HTTP, []string{"odd", "even"}, visit)
		})
	}
	byKind := func(s *DiskMocks) ([]string, error) {
		return walk(s, func(visit func(string, func() (*models.Mock, error)) bool) error {
			return s.RangeKind(models.HTTP, visit)
		})
	}
	for _, tc := range []struct {
		name string
		// register sets the rules the case reads under and returns their undo.
		register func() func()
		// edit changes the mocks before they are added, for the case's rules.
		edit func(*models.Mock)
		at   func(*models.Mock) time.Time
		read func(*DiskMocks) ([]string, error)
	}{
		{name: "window", at: requestTime, read: loaded(window)},
		{name: "start-up band", at: requestTime, read: loaded(func(s *DiskMocks) ([]*models.Mock, error) {
			return s.LoadBefore(time.Unix(1000, 0))
		})},
		{name: "whole store", at: requestTime, read: loaded(func(s *DiskMocks) ([]*models.Mock, error) { return s.LoadAll() })},
		{
			name: "in flight", at: requestTime,
			edit: func(m *models.Mock) { m.Spec.ResTimestampMock = time.Unix(600, 0).UTC() },
			read: loaded(func(s *DiskMocks) ([]*models.Mock, error) {
				return s.LoadInFlight(time.Unix(50, 0), time.Unix(500, 0))
			}),
		},
		{
			name: "carry-over", at: requestTime,
			register: func() func() { return models.RegisterCarryOver(models.HTTP, all) },
			read: loaded(func(s *DiskMocks) ([]*models.Mock, error) {
				return s.LoadCarryOver(time.Unix(0, 0), time.Unix(1000, 0))
			}),
		},
		{
			name: "staged", at: requestTime,
			register: func() func() {
				return models.RegisterStagingNeed(models.HTTP, models.StagingNeed{Use: models.StagingWithStartup, Match: all})
			},
			read: loaded(func(s *DiskMocks) ([]*models.Mock, error) {
				return s.LoadStaged(time.Unix(0, 0), time.Unix(1000, 0))
			}),
		},
		{
			name: "answer-placed", at: answerTime,
			register: func() func() { return models.RegisterAnswerPlaced(models.HTTP, all) },
			read: loaded(func(s *DiskMocks) ([]*models.Mock, error) {
				return s.LoadAnswerPlaced(time.Unix(0, 0), time.Unix(1000, 0))
			}),
		},
		{
			name: "window of mocks placed at their answer", at: answerTime,
			register: func() func() { return models.RegisterAnswerPlaced(models.HTTP, all) },
			read:     loaded(window),
		},
		{
			// Half of each burst is placed at its answer, which the index
			// sorts after the other half; the walk is by request time.
			name: "recorded before, half placed at their answer", at: requestTime,
			register: oddAtAnswer,
			read: func(s *DiskMocks) ([]string, error) {
				var out []string
				err := s.RangeRecordedBefore(time.Unix(1000, 0), func(m *models.Mock) bool {
					out = append(out, m.Name)
					return true
				})
				return out, err
			},
		},
		{name: "keyed", at: requestTime, register: keyedByHalf, read: keyed},
		{name: "by kind", at: requestTime, register: proven, read: byKind},
		{
			// This row and the next: the keyed and by-kind walks read by
			// request time, mocks placed at their answer included.
			name: "keyed, half placed at their answer", at: requestTime,
			register: with(keyedByHalf, oddAtAnswer), read: keyed,
		},
		{
			name: "by kind, half placed at their answer", at: requestTime,
			register: with(proven, oddAtAnswer), read: byKind,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.register != nil {
				t.Cleanup(tc.register())
			}
			rec := tiedRecording()
			s, err := NewDiskMocks(zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			for _, m := range rec {
				if tc.edit != nil {
					tc.edit(m)
				}
				if err := s.Add(m); err != nil {
					t.Fatal(err)
				}
			}
			s.Finalize()
			got, err := tc.read(s)
			if err != nil {
				t.Fatal(err)
			}
			sameOrder(t, tc.name, got, recordedOrder(rec, tc.at))
		})
	}
}

// A store finalized, added to and read again keeps the recorded order of
// the mocks that share a time across both: the order is the one they were
// added in, not the one the first sort left.
func TestTheDiskStoreKeepsTheRecordedOrderOfTiedMocksAddedAfterARead(t *testing.T) {
	rec := tiedRecording()
	s, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	half := len(rec) / 2
	for _, m := range rec[:half] {
		if err := s.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.LoadWindow(time.Unix(0, 0), time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	for _, m := range rec[half:] {
		if err := s.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadWindow(time.Unix(0, 0), time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	sameOrder(t, "window after a second add", mockNames(got), recordedOrder(rec, requestTime))
}

// The statement and destination catalogs keep, of the mocks of one
// statement or destination that share a time, the one the file holds first,
// and list what they keep by request time, the ones of one time in the
// file's order: with every mock placed at its request, and with the odd
// statements' and destinations' mocks placed at their answer, a millisecond
// after it, which a list by place would put after the even ones.
func TestTheDiskStoreCatalogsKeepTheFirstRecordedOfMocksThatShareATime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		atAnswer bool
	}{{"placed at their request", false}, {"half placed at their answer", true}} {
		t.Run(tc.name, func(t *testing.T) {
			// odd names the odd statements' and destinations' mocks.
			odd := map[string]bool{}
			for i := 1; i < 20; i += 2 {
				for _, name := range []string{"stmt%02d-first", "stmt%02d-again", "dest%02d-first", "dest%02d-again"} {
					odd[fmt.Sprintf(name, i)] = true
				}
			}
			if tc.atAnswer {
				placed := func(m *models.Mock) bool { return odd[m.Name] }
				t.Cleanup(models.RegisterAnswerPlaced(models.PostgresV3, placed))
				t.Cleanup(models.RegisterAnswerPlaced(models.MySQL, placed))
			}
			s, err := NewDiskMocks(zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			add := func(ms ...*models.Mock) {
				for _, m := range ms {
					if tc.atAnswer && odd[m.Name] != models.IsAnswerPlaced(m) {
						t.Fatalf("%s placed at its answer: %v; want %v", m.Name, models.IsAnswerPlaced(m), odd[m.Name])
					}
					if err := s.Add(m); err != nil {
						t.Fatal(err)
					}
				}
			}
			var wantStatements, wantDestinations []string
			for i := 0; i < 20; i++ {
				// Statement i's first recording, then a later one of it at
				// the same time, all twenty statements at one time.
				first := pgStatementMock(fmt.Sprintf("stmt%02d-first", i), fmt.Sprintf("h%02d", i), 100)
				again := pgStatementMock(fmt.Sprintf("stmt%02d-again", i), fmt.Sprintf("h%02d", i), 100)
				wantStatements = append(wantStatements, first.Name)
				add(first, again)
			}
			for i := 0; i < 20; i++ {
				first := perTestMockAt(fmt.Sprintf("dest%02d-first", i), 50)
				again := perTestMockAt(fmt.Sprintf("dest%02d-again", i), 50)
				for _, m := range []*models.Mock{first, again} {
					m.Kind = models.MySQL
					m.Spec.Metadata = map[string]string{"destAddr": fmt.Sprintf("10.0.0.%d:3306", i+1)}
					m.TestModeInfo.LifetimeDerived = true
				}
				wantDestinations = append(wantDestinations, first.Name)
				add(first, again)
			}
			s.Finalize()
			statements, err := s.LoadStatements()
			if err != nil {
				t.Fatal(err)
			}
			sameOrder(t, "statement catalog", mockNames(statements), wantStatements)
			destinations, err := s.LoadDestinations()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, m := range destinations {
				if m.Kind == models.MySQL {
					got = append(got, m.Name)
				}
			}
			sameOrder(t, "destination catalog", got, wantDestinations)
		})
	}
}

// A mock added again under its name is read as its last add, which the
// store holds at that add's offset: the keyed and by-kind walks visit it
// there among the mocks of its time, as the window load lists that record.
func TestTheDiskStoreWalksAMockAddedAgainWhereItsLastAddPutsIt(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	t.Cleanup(models.RegisterRequestKeys(models.HTTP, func(*models.Mock) []string { return []string{"k"} }))
	s, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var want []string
	for i := 0; i < 20; i++ {
		if err := s.Add(perTestMockAt(fmt.Sprintf("x%02d", i), 100)); err != nil {
			t.Fatal(err)
		}
		if i != 5 {
			want = append(want, fmt.Sprintf("x%02d", i))
		}
	}
	if err := s.Add(perTestMockAt("x05", 100)); err != nil {
		t.Fatal(err)
	}
	want = append(want, "x05")
	s.Finalize()
	for _, walk := range []struct {
		name string
		run  func(visit func(string, func() (*models.Mock, error)) bool) error
	}{
		{"keyed walk", func(visit func(string, func() (*models.Mock, error)) bool) error {
			return s.RangeKeys(models.HTTP, []string{"k"}, visit)
		}},
		{"by-kind walk", func(visit func(string, func() (*models.Mock, error)) bool) error {
			return s.RangeKind(models.HTTP, visit)
		}},
	} {
		var got []string
		if err := walk.run(func(name string, _ func() (*models.Mock, error)) bool {
			got = append(got, name)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		sameOrder(t, walk.name, got, want)
	}
}
