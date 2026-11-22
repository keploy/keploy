package learn

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// runCostMocks decides costSet's call in two tests of one set over mocks,
// optionally staging the set first with WarmSet.
func runCostMocks(mocks []*models.Mock, stage bool) (got [2]string, builds, loads [2]int, scans, lookups, asks [2]int64, use SetUse, usedAfter int64) {
	const rec, live = "ids a1b2c3d4 b2c3d4e5 c3d4e5f6 d4e5f6a7 e5f6a7b8", "ids f0e9d8c7 e9d8c7b6 d8c7b6a5 c7b6a5f4 b6a5f4e3"
	tr := NewTracker()
	tr.NewSet()
	if stage {
		use = <-tr.WarmSet(srcOf(mocks))
	}
	built := func(dc *DriftCache) int {
		fs := filtersOf(dc.set)
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.builds
	}
	for c := range got {
		tr.Begin()
		dc := tr.Drifts()
		b, ld, sc, l, a := built(dc), dc.Loads(), heldScans.Load(), filterLookups.Load(), placeAsks.Load()
		got[c] = oDescribe(Decide(costCall(dc, "body.ids", rec, live, eRecAt, nil, lazyOf(dc, mocks), nil, nil)))
		builds[c], loads[c] = built(dc)-b, dc.Loads()-ld
		scans[c], lookups[c], asks[c] = heldScans.Load()-sc, filterLookups.Load()-l, placeAsks.Load()-a
		tr.End()
	}
	s := tr.derived
	s.mu.Lock()
	usedAfter = s.budget.used
	s.mu.Unlock()
	return got, builds, loads, scans, lookups, asks, use, usedAfter
}

func costSetMocks(n int) []*models.Mock {
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		name, body, at := fmt.Sprintf("mock-%d", i), fmt.Sprintf(`{"note":"Item %d of the run","ref":"Ref-%05d"}`, i, i), eRecAt.Add(-time.Duration(n-i)*time.Millisecond)
		if i == n/2 {
			name, body, at = "mock-held", `{"note":"Kept A1B2C3D4","ref":"a1b2c3d4"}`, eRecAt.Add(-time.Second)
		}
		mocks[i] = getAnswer(name, fmt.Sprintf("http://api/items/%d", i), body, at)
	}
	return mocks
}

// floorMongoMocks builds n Mongo OP_MSG recordings whose decoded forms exceed
// minSetBudget (64 MiB) when staged, matching a ~100 MB Mongo recording set
// that does not fit in the 64 MiB floor.
func floorMongoMocks(n int) []*models.Mock {
	// Each recording carries an escaped ~512 KiB string inside an OP_MSG
	// response section so WarmSet's decoded forms exceed 64 MiB (~75 MiB for
	// 150 recordings) while keeping word/instant scanning fast under -race.
	pad := `\"` + strings.Repeat(" ", 512<<10) + `\"`
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		name := fmt.Sprintf("mongo-%d", i)
		at := eRecAt.Add(-time.Duration(n-i) * time.Millisecond)
		doc := fmt.Sprintf(`{"ok":1,"cursor":{"firstBatch":[{"_id":"id-%05d","note":"Item %d","ref":"Ref-%05d","blob":"%s"}]}}`, i, i, i, pad)
		if i == n/2 {
			name = "mock-held"
			at = eRecAt.Add(-time.Second)
			doc = fmt.Sprintf(`{"ok":1,"cursor":{"firstBatch":[{"_id":"id-held","note":"Kept A1B2C3D4","ref":"a1b2c3d4","blob":"%s"}]}}`, pad)
		}
		mocks[i] = &models.Mock{
			Name: name,
			Kind: models.Mongo,
			Spec: models.MockSpec{
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
				MongoRequests: []models.MongoRequest{{
					Message: &models.MongoOpMessage{Sections: []string{fmt.Sprintf(`{ SectionSingle msg: { "find": "items", "filter": { "n": %d } } }`, i)}},
				}},
				MongoResponses: []models.MongoResponse{{
					Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: ` + doc + ` }`}},
				}},
			},
		}
	}
	return mocks
}

// Staging a set must never make its calls do more work than no staging:
//   - at a tight budget (256 KiB), where main's non-evictable held/forms
//     pinning caused 7489 scans vs 4071;
//   - at a partial-fit budget (WarmSet fits without HeldFull/FormsFull, yet
//     WarmSet + Decide together exceed the budget);
//   - at the 64 MiB floor (minSetBudget) on a >64 MiB Mongo set;
//   - at default budgets of 1, 4 and 16 GiB agents;
//   - while WarmSet runs concurrently with Decide;
//   - across LRU caches (bodies, words, recs via KeepRecording), keepRun,
//     mixed-slot staged eviction in both held and forms, multi-slot t.cap
//     self-eviction, and warmOne early abort.
func TestStagingAtTightBudgetAndFloorIsNeverWorseThanNoStaging(t *testing.T) {
	defer func(b int64) { learnBudget = b }(learnBudget)

	t.Run("tight budget 256 KiB", func(t *testing.T) {
		learnBudget = 256 << 10
		mocks := costSetMocks(300)
		got, _, loads, scans, lookups, asks, _, _ := runCostMocks(mocks, false)
		sGot, _, sLoads, sScans, sLookups, sAsks, use, usedAfter := runCostMocks(mocks, true)
		t.Logf("256 KiB, 300 mocks: staging %+v; usedAfter=%d; loads %v/%v, scans %v/%v, lookups %v/%v, places %v/%v (staged/none)",
			use, usedAfter, sLoads, loads, sScans, scans, sLookups, lookups, sAsks, asks)
		if !use.HeldFull && !use.FormsFull {
			t.Fatalf("256 KiB fixture did not fill budget during staging: %+v", use)
		}
		if sGot != got {
			t.Errorf("staged set decides %q; unstaged %q", sGot, got)
		}
		if usedAfter > learnBudget {
			t.Errorf("budget used after tests %d exceeds limit %d", usedAfter, learnBudget)
		}
		for c := range got {
			if sScans[c] > scans[c] || sLookups[c] > lookups[c] || sAsks[c] > asks[c] || sLoads[c] > loads[c] {
				t.Errorf("test %d staged: loads=%d scans=%d lookups=%d places=%d; unstaged: loads=%d scans=%d lookups=%d places=%d",
					c+1, sLoads[c], sScans[c], sLookups[c], sAsks[c], loads[c], scans[c], lookups[c], asks[c])
			}
		}
	})

	t.Run("partial fit where WarmSet fits alone but WarmSet plus Decide exceeds budget", func(t *testing.T) {
		mocks := costSetMocks(300)
		learnBudget = 16 << 20
		tr := NewTracker()
		tr.NewSet()
		fullUse := <-tr.WarmSet(srcOf(mocks))
		stageAlone := fullUse.Held + fullUse.Forms
		learnBudget = stageAlone + 64<<10
		got, _, loads, scans, lookups, asks, _, _ := runCostMocks(mocks, false)
		sGot, _, sLoads, sScans, sLookups, sAsks, use, usedAfter := runCostMocks(mocks, true)
		t.Logf("partial-fit %d B (stageAlone=%d B): staging %+v; usedAfter=%d; loads %v/%v, scans %v/%v, lookups %v/%v, places %v/%v",
			learnBudget, stageAlone, use, usedAfter, sLoads, loads, sScans, scans, sLookups, lookups, sAsks, asks)
		if use.Staged != len(mocks) || use.HeldFull || use.FormsFull {
			t.Fatalf("partial-fit fixture did not finish staging without hitting full: %+v", use)
		}
		if sGot != got {
			t.Errorf("staged set decides %q; unstaged %q", sGot, got)
		}
		if usedAfter > learnBudget {
			t.Errorf("budget used after tests %d exceeds limit %d", usedAfter, learnBudget)
		}
		for c := range got {
			if sScans[c] > scans[c] || sLookups[c] > lookups[c] || sAsks[c] > asks[c] || sLoads[c] > loads[c] {
				t.Errorf("partial-fit test %d staged: loads=%d scans=%d lookups=%d places=%d; unstaged: loads=%d scans=%d lookups=%d places=%d",
					c+1, sLoads[c], sScans[c], sLookups[c], sAsks[c], loads[c], scans[c], lookups[c], asks[c])
			}
		}
	})

	t.Run("64 MiB floor with over-budget Mongo set", func(t *testing.T) {
		learnBudget = minSetBudget
		mocks := floorMongoMocks(150)
		got, _, loads, scans, lookups, asks, _, _ := runCostMocks(mocks, false)
		sGot, _, sLoads, sScans, sLookups, sAsks, use, usedAfter := runCostMocks(mocks, true)
		t.Logf("64 MiB floor, 150 Mongo mocks: staging %+v; usedAfter=%d; loads %v/%v, scans %v/%v, lookups %v/%v, places %v/%v (staged/none)",
			use, usedAfter, sLoads, loads, sScans, scans, sLookups, lookups, sAsks, asks)
		if !use.HeldFull && !use.FormsFull {
			t.Fatalf("fixture did not exceed the 64 MiB floor during staging: %+v", use)
		}
		if sGot != got {
			t.Errorf("staged set decides %q; unstaged %q", sGot, got)
		}
		if usedAfter > learnBudget {
			t.Errorf("budget used after tests %d exceeds floor %d", usedAfter, learnBudget)
		}
		for c := range got {
			if sScans[c] > scans[c] || sLookups[c] > lookups[c] || sAsks[c] > asks[c] || sLoads[c] > loads[c] {
				t.Errorf("at 64 MiB floor, test %d staged: loads=%d scans=%d lookups=%d places=%d; unstaged: loads=%d scans=%d lookups=%d places=%d",
					c+1, sLoads[c], sScans[c], sLookups[c], sAsks[c], loads[c], scans[c], lookups[c], asks[c])
			}
		}
	})

	t.Run("default budgets 1 4 and 16 GiB", func(t *testing.T) {
		for _, limit := range []int64{1 << 30, 4 << 30, 16 << 30} {
			learnBudget = budgetOf(limit)
			for _, n := range []int{300, 3000} {
				mocks := costSetMocks(n)
				got, _, loads, scans, lookups, asks, _, _ := runCostMocks(mocks, false)
				sGot, _, sLoads, sScans, sLookups, sAsks, use, usedAfter := runCostMocks(mocks, true)
				if use.Staged != n || use.HeldFull || use.FormsFull || use.Budget != learnBudget {
					t.Fatalf("limit %d GiB, %d mocks: staging %+v; want all read, neither full, budget %d",
						limit>>30, n, use, learnBudget)
				}
				if sGot != got {
					t.Errorf("limit %d GiB, %d mocks: staged %q; unstaged %q", limit>>30, n, sGot, got)
				}
				if usedAfter > learnBudget {
					t.Errorf("limit %d GiB, %d mocks: budget used %d > %d", limit>>30, n, usedAfter, learnBudget)
				}
				for c := range got {
					if sScans[c] > scans[c] || sLookups[c] > lookups[c] || sAsks[c] > asks[c] || sLoads[c] > loads[c] {
						t.Errorf("limit %d GiB, %d mocks, test %d staged: loads=%d scans=%d lookups=%d places=%d; unstaged: loads=%d scans=%d lookups=%d places=%d",
							limit>>30, n, c+1, sLoads[c], sScans[c], sLookups[c], sAsks[c], loads[c], scans[c], lookups[c], asks[c])
					}
				}
			}
		}
	})

	t.Run("concurrent WarmSet and Decide at tight budget", func(t *testing.T) {
		learnBudget = 256 << 10
		mocks := costSetMocks(300)
		const rec, live = "ids a1b2c3d4 b2c3d4e5 c3d4e5f6 d4e5f6a7 e5f6a7b8", "ids f0e9d8c7 e9d8c7b6 d8c7b6a5 c7b6a5f4 b6a5f4e3"
		want, _, baseLoads, baseScans, baseLookups, baseAsks, _, _ := runCostMocks(mocks, false)

		tr := NewTracker()
		tr.NewSet()
		s := tr.derived
		done := tr.WarmSet(srcOf(mocks))
		var got [2]string
		var loads [2]int
		var scans, lookups, asks [2]int64
		for c := range got {
			tr.Begin()
			dc := tr.Drifts()
			ld, sc, l, a := dc.Loads(), heldScans.Load(), filterLookups.Load(), placeAsks.Load()
			got[c] = oDescribe(Decide(costCall(dc, "body.ids", rec, live, eRecAt, nil, lazyOf(dc, mocks), nil, nil)))
			loads[c] = dc.Loads() - ld
			scans[c], lookups[c], asks[c] = heldScans.Load()-sc, filterLookups.Load()-l, placeAsks.Load()-a
			tr.End()
		}
		<-done
		if got != want {
			t.Errorf("concurrent staged decides %q; unstaged %q", got, want)
		}
		if !s.inFlight.TryAcquire(readInFlight) {
			t.Errorf("concurrent WarmSet leaked readInFlight semaphore")
		} else {
			s.inFlight.Release(readInFlight)
		}
		s.mu.Lock()
		used := s.budget.used
		s.mu.Unlock()
		if used > learnBudget {
			t.Errorf("concurrent budget used %d > %d", used, learnBudget)
		}
		for c := range got {
			if scans[c] > baseScans[c] || lookups[c] > baseLookups[c] || asks[c] > baseAsks[c] {
				t.Errorf("concurrent test %d staged: loads=%d scans=%d lookups=%d places=%d; unstaged: loads=%d scans=%d lookups=%d places=%d",
					c+1, loads[c], scans[c], lookups[c], asks[c], baseLoads[c], baseScans[c], baseLookups[c], baseAsks[c])
			}
		}
	})

	t.Run("ClassifyFollowing promotion mixed-slot eviction and accounting", func(t *testing.T) {
		defer func(h int64) { heldBudget = h }(heldBudget)
		learnBudget = 64 << 10
		heldBudget = 32 << 10
		row := newHeldRow(60, 12)
		want := row.classify(noSet(NewDriftCache()))

		tr := NewTracker()
		tr.NewSet()
		s := tr.derived
		use := <-tr.WarmSet(srcOf(row.mocks))
		if !use.HeldFull {
			t.Fatalf("expected WarmSet to hit heldBudget (%d): %+v", heldBudget, use)
		}
		if !s.keepsPayloads() {
			t.Errorf("WarmSet hitting tight budget poisoned keepsPayloads()")
		}

		s.mu.Lock()
		var promotedID, evictedID string
		for id, sl := range s.held.by {
			if sl.staged&partMoments != 0 && sl.staged&partPlaces != 0 {
				if promotedID == "" {
					promotedID = id
				} else if evictedID == "" {
					evictedID = id
					break
				}
			}
		}
		if promotedID == "" || evictedID == "" {
			s.mu.Unlock()
			t.Fatal("expected at least two staged slots in s.held")
		}
		if sl := s.held.find(promotedID, partMoments); sl == nil || sl.staged&partMoments != 0 {
			s.mu.Unlock()
			t.Fatalf("call find(%q, partMoments) did not promote partMoments", promotedID)
		}
		s.held.keep(promotedID, partAt, 128, func(e *heldParts) { e.answerAt = []givenMoment{{mock: "m"}} })
		s.mu.Unlock()

		claimBytes := learnBudget - 2048
		if !setClaim(s, claimBytes) {
			t.Fatalf("setClaim(%d) failed to reclaim staged held/forms bytes", claimBytes)
		}
		s.mu.Lock()
		tierSum := s.held.used + s.words.used + s.forms.used + s.drifts.used + s.bodies.kept.used + s.views.kept.used + s.recs.used + s.refused.used
		wantBudgetUsed := tierSum + claimBytes
		gotBudgetUsed := s.budget.used
		promotedSlot := s.held.by[promotedID]
		_, evictedStillHeld := s.held.by[evictedID]
		_, evictedStillWarming := s.warming[evictedID]
		stageStopped := s.budget.stageStopped
		remStaged := s.stagedBytesLocked()
		s.mu.Unlock()

		if gotBudgetUsed != wantBudgetUsed || gotBudgetUsed > learnBudget {
			t.Errorf("after reclaiming staged bytes: budget.used=%d, want %d (<= %d)", gotBudgetUsed, wantBudgetUsed, learnBudget)
		}
		if remStaged != 0 {
			t.Errorf("expected stagedBytesLocked()==0 after full reclaim, got %d", remStaged)
		}
		if promotedSlot == nil || promotedSlot.has != (partMoments|partAt) || promotedSlot.staged != 0 || promotedSlot.p.places != nil {
			t.Errorf("promoted/mixed slot %q after reclaim: %+v; want only partMoments|partAt kept and staged places cleared", promotedID, promotedSlot)
		}
		if evictedStillHeld || evictedStillWarming {
			t.Errorf("purely staged slot %q after reclaim: held=%v warming=%v; want both removed", evictedID, evictedStillHeld, evictedStillWarming)
		}
		if !stageStopped || s.stageRoom() {
			t.Errorf("expected stageStopped=true and stageRoom()=false after reclamation (stageStopped=%v)", stageStopped)
		}
		s.mu.Lock()
		s.budget.used -= claimBytes
		s.mu.Unlock()
		if after := <-tr.WarmSet(srcOf(row.mocks)); after.Staged != use.Staged {
			t.Errorf("WarmSet after stageStopped staged %d more recordings (total %d vs %d)", after.Staged-use.Staged, after.Staged, use.Staged)
		}
		tr.Begin()
		got := row.classify(tr.Drifts())
		tr.End()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("ClassifyFollowing after tight staging learned %v, want %v", got, want)
		}
	})

	t.Run("reclaim yields staged held and forms before evicting call LRU caches and KeepRecording", func(t *testing.T) {
		learnBudget = 64 << 10
		mocks := costSetMocks(40)

		// Unstaged KeepRecording baseline: 10 recordings read twice -> 10 decodes.
		trPlain := NewTracker()
		trPlain.NewSet()
		for pass := 0; pass < 2; pass++ {
			for i := 0; i < 10; i++ {
				m := mocks[i]
				if _, err := trPlain.KeepRecording(fmt.Sprintf("k-%d", i), 256, func() (*models.Mock, error) { return m.ShallowCopy(), nil }); err != nil {
					t.Fatal(err)
				}
			}
		}
		plainDecodes := trPlain.RecordingDecodes()

		// Staged: WarmSet fills budget with staged held/forms, then KeepRecording
		// and bodyStore must reclaim staged bytes instead of starving or refusing.
		tr := NewTracker()
		tr.NewSet()
		s := tr.derived
		<-tr.WarmSet(srcOf(mocks))
		for pass := 0; pass < 2; pass++ {
			for i := 0; i < 10; i++ {
				m := mocks[i]
				if _, err := tr.KeepRecording(fmt.Sprintf("k-%d", i), 256, func() (*models.Mock, error) { return m.ShallowCopy(), nil }); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := tr.RecordingDecodes(); got > plainDecodes {
			t.Errorf("staged KeepRecording decoded %d times; unstaged %d (refused=%d)", got, plainDecodes, len(s.refused.by))
		}
		s.mu.Lock()
		refusedCount := len(s.refused.by)
		s.mu.Unlock()
		if refusedCount != 0 {
			t.Errorf("KeepRecording falsely refused %d keys due to staged bytes", refusedCount)
		}

		// Also verify call bodyStore.get reclaims staged forms/held rather than
		// evicting its own just-cached payloads, and keepRun reclaims staged
		// bytes before evicting call words.
		tr2 := NewTracker()
		tr2.NewSet()
		s2 := tr2.derived
		s2.mu.Lock()
		s2.held.keepAs("staged-1", partPlaces, 40<<10, true, func(e *heldParts) { e.places = []keyedPlace{{where: "s"}} })
		s2.words.keep("call-w", partExtra, 12<<10, func(e *wordParts) { e.extra = "call" })
		s2.mu.Unlock()
		// A real call loads a 20 KiB payload into s2.bodies: must reclaim
		// staged-1 BEFORE evicting call-w!
		bigBody := strings.Repeat("x", 20<<10)
		s2.bodies.get("b-1", "b-1", func() (*models.Mock, error) {
			return getAnswer("b-1", "http://api/b/1", `{"v":"`+bigBody+`"}`, eRecAt), nil
		})
		s2.mu.Lock()
		_, wordsKept := s2.words.by["call-w"]
		_, bodyKept := s2.bodies.kept.by["b-1"]
		_, stagedStill := s2.held.by["staged-1"]
		used2 := s2.budget.used
		s2.mu.Unlock()
		if !wordsKept || !bodyKept || stagedStill || used2 > learnBudget {
			t.Errorf("call bodyStore insert: wordsKept=%v bodyKept=%v stagedStill=%v used=%d/%d; want staged evicted first, call words & body kept",
				wordsKept, bodyKept, stagedStill, used2, learnBudget)
		}

		// keepRun reclaims staged bytes before evicting active call words.
		tr3 := NewTracker()
		tr3.NewSet()
		s3 := tr3.derived
		s3.mu.Lock()
		s3.held.keepAs("staged-run", partPlaces, 50<<10, true, func(e *heldParts) { e.places = []keyedPlace{{where: "r"}} })
		s3.words.keep("call-w3", partExtra, 10<<10, func(e *wordParts) { e.extra = "call" })
		s3.mu.Unlock()
		if !s3.keepRun(8 << 10) {
			t.Fatal("keepRun(8 KiB) failed")
		}
		s3.mu.Lock()
		_, w3Kept := s3.words.by["call-w3"]
		_, stagedRunStill := s3.held.by["staged-run"]
		used3 := s3.budget.used
		s3.mu.Unlock()
		if !w3Kept || stagedRunStill || used3 > learnBudget {
			t.Errorf("keepRun: wordsKept=%v stagedRunStill=%v used=%d/%d; want staged reclaimed first, call words kept",
				w3Kept, stagedRunStill, used3, learnBudget)
		}
		// Verify setClaim reclaiming staged bytes latches stageStopped on its own
		// (even when no per-tier cap was hit).
		trStop := NewTracker()
		trStop.NewSet()
		sStop := trStop.derived
		sStop.mu.Lock()
		sStop.held.keepAs("st-stop", partPlaces, 40<<10, true, func(e *heldParts) { e.places = []keyedPlace{{where: "x"}} })
		if sStop.budget.stageStopped {
			sStop.mu.Unlock()
			t.Fatal("stageStopped set before any call reclaimed staged bytes")
		}
		sStop.mu.Unlock()
		if !setClaim(sStop, 40<<10) {
			t.Fatal("setClaim failed to reclaim staged slot")
		}
		sStop.mu.Lock()
		stoppedByClaim := sStop.budget.stageStopped
		sStop.mu.Unlock()
		if !stoppedByClaim {
			t.Errorf("setClaim reclaiming staged bytes did not latch stageStopped")
		}
	})

	t.Run("mixed staged forms slot clears dropped form and preserves promoted form", func(t *testing.T) {
		learnBudget = 8 << 10
		s := newSetDerived()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.forms.keepAs("f-1", partDecoded, 2<<10, true, func(e *formParts) { e.d[0] = decoded{dec: "kept-form"} })
		s.forms.keepAs("f-1", partDecoded<<1, 2<<10, true, func(e *formParts) { e.d[1] = decoded{dec: "dropped-form"} })
		// Promote d[0] via call find; d[1] remains staged-only.
		if sl := s.forms.find("f-1", partDecoded); sl == nil {
			t.Fatal("expected f-1 partDecoded")
		}
		if s.forms.stagedUsed != 2<<10 {
			t.Fatalf("forms.stagedUsed = %d, want %d", s.forms.stagedUsed, 2<<10)
		}
		freed := s.forms.letGoStaged(2 << 10)
		sl := s.forms.by["f-1"]
		if freed != 2<<10 || sl == nil || sl.has != partDecoded || sl.staged != 0 || sl.p.d[0].dec != "kept-form" || sl.p.d[1].dec != "" || s.forms.stagedUsed != 0 {
			t.Errorf("after forms mixed-slot reclaim: freed=%d slot=%+v stagedUsed=%d", freed, sl, s.forms.stagedUsed)
		}
		wantSize := int64(2<<10) + slotBytes + int64(len("f-1"))
		if sl.size != wantSize || s.forms.used != wantSize || s.budget.used != wantSize {
			t.Errorf("forms accounting mismatch: sl.size=%d forms.used=%d budget.used=%d want=%d", sl.size, s.forms.used, s.budget.used, wantSize)
		}
	})

	t.Run("multi-slot per-tier cap self-eviction reclaims remaining staged slots without poisoning full", func(t *testing.T) {
		defer func(h int64) { heldBudget = h }(heldBudget)
		learnBudget = 64 << 10
		heldBudget = 4096
		s := newSetDerived()
		s.mu.Lock()
		defer s.mu.Unlock()
		// Call-demanded rec-3 (~1500 B), purely staged rec-2 (~2000 B, older),
		// purely staged rec-1 (~500 B, newer -> front of order).
		s.held.keep("rec-3", partAt, 1500-slotBytes-int64(len("rec-3")), func(e *heldParts) { e.answerAt = []givenMoment{{mock: "3"}} })
		s.held.keepAs("rec-2", partPlaces, 2000-slotBytes-int64(len("rec-2")), true, func(e *heldParts) { e.places = []keyedPlace{{where: "2"}} })
		s.held.keepAs("rec-1", partPlaces, 500-slotBytes-int64(len("rec-1")), true, func(e *heldParts) { e.places = []keyedPlace{{where: "1"}} })
		if s.held.used != 4000 || s.held.stagedUsed != 2500 {
			t.Fatalf("setup: held.used=%d stagedUsed=%d, want 4000 and 2500", s.held.used, s.held.stagedUsed)
		}
		// Call writes partAt (500 B) to rec-1: first reclaim evicts rec-1 (500 B),
		// then adding slotBytes+len("rec-1") requires reclaiming rec-2 too!
		s.held.keep("rec-1", partAt, 500, func(e *heldParts) { e.answerAt = []givenMoment{{mock: "1"}} })
		if s.held.full {
			t.Fatalf("per-tier cap self-eviction falsely poisoned s.held.full=true while rec-2 was reclaimable")
		}
		if s.held.by["rec-2"] != nil {
			t.Errorf("expected staged rec-2 to be evicted to satisfy per-tier cap")
		}
		sl := s.held.by["rec-1"]
		wantRec1 := int64(500) + slotBytes + int64(len("rec-1"))
		if sl == nil || sl.has != partAt || sl.staged != 0 || sl.size != wantRec1 {
			t.Fatalf("rec-1 after self-eviction: %+v, want has=partAt staged=0 size=%d", sl, wantRec1)
		}
		wantTotal := int64(1500) + wantRec1
		if s.held.used != wantTotal || s.budget.used != wantTotal || s.held.stagedUsed != 0 {
			t.Errorf("accounting after multi-slot self-eviction: held.used=%d budget.used=%d stagedUsed=%d want %d/%d/0",
				s.held.used, s.budget.used, s.held.stagedUsed, wantTotal, wantTotal)
		}
	})

	t.Run("staged bodyStore and warmOne obey stageStopped without evicting calls or marking warmed", func(t *testing.T) {
		learnBudget = 8 << 10
		tr := NewTracker()
		tr.NewSet()
		s := tr.derived
		// Seed a call-owned payload in s.bodies.kept and latch stageStopped.
		s.bodies.get("call-b", "call-b", func() (*models.Mock, error) {
			return getAnswer("call-b", "http://api/c", `{"v":"`+strings.Repeat("c", 4<<10)+`"}`, eRecAt), nil
		})
		s.mu.Lock()
		s.budget.stageStopped = true
		s.mu.Unlock()
		// A trailing staged bodyStore.getAs must NOT insert into s.bodies.kept
		// or evict call-b.
		s.bodies.getAs("staged-b", "staged-b", true, func() (*models.Mock, error) {
			return getAnswer("staged-b", "http://api/s", `{"v":"`+strings.Repeat("s", 5<<10)+`"}`, eRecAt), nil
		})
		s.mu.Lock()
		_, callKept := s.bodies.kept.by["call-b"]
		_, stagedBodyKept := s.bodies.kept.by["staged-b"]
		s.mu.Unlock()
		if !callKept || stagedBodyKept {
			t.Errorf("after stageStopped, staged bodyStore.getAs: callKept=%v stagedBodyKept=%v; want true/false", callKept, stagedBodyKept)
		}

		// A warmOne invoked after stageStopped must neither mark s.warming[id]=warmed
		// nor increment s.staged.
		m := costSetMocks(1)[0]
		dc := tr.newDrifts()
		dc.staging = true
		x := dc.LazyRawOf(m, false, nil)
		ch := make(chan struct{})
		s.mu.Lock()
		s.warming[x.id] = ch
		s.mu.Unlock()
		dc.warmOne(s, x, ch, true)
		s.mu.Lock()
		_, stillWarming := s.warming[x.id]
		stagedCount := s.staged
		s.mu.Unlock()
		if stillWarming || stagedCount != 0 {
			t.Errorf("aborted warmOne left warming=%v staged=%d; want false and 0", stillWarming, stagedCount)
		}
	})
}
