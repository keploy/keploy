package proxy

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Staging must not race match counting.
//
// The match count used to live in TestModeInfo and be written with
// atomic.AddUint64, so building a tier-local key from TestModeInfo raced every
// concurrent count. Counts now live in the manager (hitIdx), which every
// staging rebuilds, and a set boundary starts afresh, while matchers bump it.
// Run this package with -race or the test proves nothing.
func TestStagingDoesNotRaceWithHitCounting(t *testing.T) {
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	at := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	pool := make([]*models.Mock, 0, 20)
	for i := 0; i < 20; i++ {
		pool = append(pool, newMockForTest("m"+string(rune('a'+i)), at.Add(-time.Minute), models.LifetimePerTest))
	}
	mm.SetMocksWithWindow(pool, nil, models.BaseTime, time.Now())
	mm.SetMocksWithWindow(pool, nil, at, at.Add(time.Second))
	// Snapshot once so this test isolates the manager's side: the callers'
	// whole-mock copies of live pooled mocks are covered, as far as counting
	// goes, by TestCountingAMatchDoesNotWriteThePooledMock.
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
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				if i%8 == 0 { // a set boundary now and then
					mm.ResetForReplaySession()
					mm.SetMocksWithWindow(pool, nil, models.BaseTime, time.Now())
				}
				mm.SetMocksWithWindow(pool, nil, at, at.Add(time.Second))
			}
		}
	}()
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()
}

// SetMocksWithWindowThreeTier's startup-insert block must never take hitMu.
//
// bumpHitCount's slow path takes hitMu and THEN treesMu. Indexing hit counts
// from inside the tree-swap block would take them in the opposite order, so a
// ThreeTier call racing a MarkMockAsUsed miss would deadlock: one goroutine
// holds treesMu waiting for hitMu, the other holds hitMu waiting for treesMu.
// An ordinary run never shows it — the window is the few instructions between
// the two acquisitions.
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

	// Writers: repeatedly take swapMu -> treesMu, and index hit counts.
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
		t.Fatal("deadlock: ThreeTier took hitMu while holding treesMu, inverting the " +
			"hitMu -> treesMu order that bumpHitCount's slow path takes")
	}
}

// tierKey must keep every field of the mock's TestModeInfo but the ID it sets:
// customComparator orders on SortOrder, so a key that dropped it would lose the
// startup tier's recorded order and DeleteStartupMock's chronological contract
// with it, and tier routing reads Lifetime straight from the key.
func TestTierKeyKeepsEveryFieldButTheID(t *testing.T) {
	src := &models.Mock{
		Name: "m",
		Kind: models.HTTP,
		TestModeInfo: models.TestModeInfo{
			ID:              7,
			IsFiltered:      true,
			SortOrder:       42,
			Lifetime:        models.LifetimeSession,
			LifetimeDerived: true,
			IsStartup:       true,
			Consume:         models.ConsumeCursorSaturate,
		},
	}
	v := reflect.ValueOf(src.TestModeInfo)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("fixture leaves TestModeInfo.%s zero; set it so the key can be checked",
				v.Type().Field(i).Name)
		}
	}

	got := tierKey(src, 123)

	want := src.TestModeInfo
	want.ID = 123
	if got != want {
		t.Fatalf("tierKey = %+v, want %+v", got, want)
	}
	if src.TestModeInfo.ID != 7 {
		t.Fatal("tierKey wrote the ID into the mock")
	}
}
