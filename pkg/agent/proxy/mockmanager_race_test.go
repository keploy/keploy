package proxy

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Building a tier-local tree key must not read HitCount.
//
// The key used to be a wholesale copy of TestModeInfo, and HitCount lives in
// that struct and is written with atomic.AddUint64 by bumpHitCount — so every
// key build raced every concurrent bump. It was latent for the startup tier
// only because startup mocks were never bumped; indexing them makes it live.
//
// Benign in effect (customComparator orders on SortOrder and ID alone, so
// HitCount cannot affect placement) but undefined behaviour, and it trips
// -race. Run this package with -race or the test proves nothing.
func TestTierKeyDoesNotRaceWithHitCountBumps(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	at := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	pool := make([]*models.Mock, 0, 20)
	for i := 0; i < 20; i++ {
		pool = append(pool, newMockForTest("m"+string(rune('a'+i)), at.Add(-time.Minute), models.LifetimePerTest))
	}
	mm.SetMocksWithWindow(pool, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow(pool, nil, at, at.Add(time.Second))
	// Snapshot once so this test isolates the MANAGER's half of the race.
	//
	// It is not what every caller does: the consume doors take models.Mock BY
	// VALUE and real callers dereference a live tree pointer into them —
	// http/match.go:1209 `deleteMock := *matchedMock`, mysql/replayer/conn.go:764
	// `DeleteUnFilteredMock(*initialHandshakeMock)`. That copy reads HitCount
	// non-atomically in the CALLER and still races after this fix. tierKey
	// closes the manager's half only; closing the caller's half means changing
	// the by-value API, which is a separate change.
	snaps := make([]models.Mock, 0, len(pool))
	for _, mk := range pool {
		snaps = append(snaps, *mk)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					for i := range snaps {
						mm.MarkMockAsUsed(snaps[i])
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				mm.SetMocksWithWindow(pool, nil, at, at.Add(time.Second))
			}
		}
	}()
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()
}

// SetMocksWithWindowThreeTier must not hold treesMu while seeding hitIdx.
//
// bumpHitCount's slow path takes hitMu and THEN treesMu. Seeding from inside
// the tree-swap block takes them in the opposite order, so a ThreeTier call
// racing a MarkMockAsUsed miss deadlocks: one goroutine holds treesMu waiting
// for hitMu, the other holds hitMu waiting for treesMu. An ordinary run never
// shows it — the window is the few instructions between the two acquisitions.
func TestThreeTierSeedingDoesNotInvertTheHitMuLockOrder(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()

	at := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	startup := make([]*models.Mock, 0, 16)
	for i := 0; i < 16; i++ {
		startup = append(startup, newMockForTest("tt"+string(rune('a'+i)), at.Add(-time.Minute), models.LifetimePerTest))
	}

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Writers: repeatedly take swapMu -> treesMu, then seed hitIdx.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					mm.SetMocksWithWindowThreeTier(nil, nil, startup, at, at.Add(time.Second))
				}
			}
		}()
	}
	// Readers: force the slow path (a name in no tree) so it takes
	// hitMu -> treesMu, the opposite order.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					mm.MarkMockAsUsed(models.Mock{Name: "absent-everywhere", Kind: models.HTTP})
				}
			}
		}()
	}

	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()

	time.Sleep(2 * time.Second)
	close(done)
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: ThreeTier seeded hitIdx while holding treesMu, inverting the " +
			"hitMu -> treesMu order that bumpHitCount's slow path takes")
	}
}

// tierKey hand-copies TestModeInfo field by field (to avoid reading HitCount),
// so the field list is a maintenance hazard: drop one and the key silently
// loses it. SortOrder is the dangerous one — customComparator orders on it, and
// DeleteStartupMock's contract depends on the startup tier staying in recorded
// order — but nothing else in the package asserts startup ordering, so the
// omission compiles and the whole suite stays green.
func TestTierKeyPreservesEveryFieldButHitCount(t *testing.T) {
	src := &models.Mock{
		Name: "m",
		Kind: models.HTTP,
		TestModeInfo: models.TestModeInfo{
			ID:              7,
			IsFiltered:      true,
			SortOrder:       42,
			Lifetime:        models.LifetimeSession,
			LifetimeDerived: true,
			HitCount:        99,
			IsStartup:       true,
		},
	}

	got := tierKey(src, 123)

	if got.ID != 123 {
		t.Fatalf("ID = %d, want the explicit 123", got.ID)
	}
	if got.SortOrder != 42 {
		t.Fatal("SortOrder was dropped: customComparator orders on it, so the startup " +
			"tier would lose its recorded order and DeleteStartupMock's chronological " +
			"contract with it")
	}
	if !got.IsFiltered {
		t.Fatal("IsFiltered was dropped")
	}
	if got.Lifetime != models.LifetimeSession {
		t.Fatal("Lifetime was dropped: tier routing reads it directly")
	}
	if !got.LifetimeDerived {
		t.Fatal("LifetimeDerived was dropped: DeriveLifetime would re-run and could " +
			"reclassify the mock")
	}
	if !got.IsStartup {
		t.Fatal("IsStartup was dropped")
	}
	if got.HitCount != 0 {
		t.Fatalf("HitCount = %d, want 0: reading it is exactly the race tierKey exists "+
			"to avoid", got.HitCount)
	}

	// Guard the hand-maintained list itself: if a field is ADDED to
	// TestModeInfo, tierKey must be updated to carry it (or deliberately not).
	if n := reflect.TypeOf(models.TestModeInfo{}).NumField(); n != 7 {
		t.Fatalf("TestModeInfo now has %d fields, not 7: tierKey copies them by hand, "+
			"so decide explicitly whether the new field belongs in a tree key and "+
			"update this count", n)
	}
}
