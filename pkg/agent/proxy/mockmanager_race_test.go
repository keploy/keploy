package proxy

import (
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
	// Real callers pass a mock they matched out of a pool snapshot, not a live
	// dereference of the tree pointer — MarkMockAsUsed takes models.Mock BY
	// VALUE, so `*livePointer` would copy HitCount non-atomically in the
	// CALLER. Snapshot once, like a matcher does.
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
