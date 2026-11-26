package record

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type noopDocStore struct{}

var _ mockDocStore = noopDocStore{}

func (noopDocStore) ReadMockDocs(context.Context, string, map[string]struct{}) (map[string][]byte, string, error) {
	return nil, "yaml", nil
}

func (noopDocStore) AppendMockDocs(context.Context, string, string, [][]byte) (int64, error) {
	return 0, nil
}

// TestSetRoll_ReadThroughDoesNotInflateCarryStateOrEvictStateMocks proves
// carried session read-through mocks never enter sr.carryState during
// evictOldestStateLocked and never evict genuine session/connection state
// mocks below maxCarryState even when >maxCarryReadThrough read-through mocks
// and >maxCarryState state mocks are written before/across rollover.
func TestSetRoll_ReadThroughDoesNotInflateCarryStateOrEvictStateMocks(t *testing.T) {
	ctx := context.Background()
	ro := &countRoller{limit: 1 << 30}
	sr := newSetRoll(zap.NewNop(), ro, noopDocStore{}, "test-set-0", nil, nil)
	if !sr.rolls() {
		t.Fatal("expected setRoll to enable rolling with noopDocStore")
	}
	cur := sr.first()
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	// 1. Write maxCarryReadThrough + 20 distinct read-through mocks first.
	for i := 0; i < maxCarryReadThrough+20; i++ {
		rt := &models.Mock{
			Version: models.Version("api.keploy.io/v1beta2"),
			Name:    fmt.Sprintf("mock-rt-%016x", i+1),
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "mocks"},
				HTTPReq: &models.HTTPReq{
					Method: models.Method("GET"),
					URL:    fmt.Sprintf("http://cfg.internal/v1/item/%d", i),
					Header: map[string]string{"Host": "cfg.internal"},
				},
				HTTPResp: &models.HTTPResp{
					StatusCode: 200,
					Body:       `{"ok":true}`,
				},
				ReqTimestampMock: t0.Add(time.Duration(i) * time.Millisecond),
				ResTimestampMock: t0.Add(time.Duration(i)*time.Millisecond + time.Millisecond),
			},
		}
		if !models.MarkReadThrough(rt) {
			t.Fatalf("MarkReadThrough failed for read-through fixture %d", i)
		}
		doc := []byte(fmt.Sprintf("name: %s\nkind: Http\n", rt.Name))
		sr.wrote(ctx, cur, rt, doc, "yaml", nil, nil)
	}

	sr.mu.Lock()
	if sr.carryReadThrough != maxCarryReadThrough {
		sr.mu.Unlock()
		t.Fatalf("carryReadThrough = %d, want capped at %d", sr.carryReadThrough, maxCarryReadThrough)
	}
	if len(sr.carryState) != 0 {
		sr.mu.Unlock()
		t.Fatalf("read-through writes must not populate carryState, got len=%d", len(sr.carryState))
	}
	sr.mu.Unlock()

	// 2. Write maxCarryState + 60 distinct genuine session/connection state mocks.
	//    Eviction MUST trigger 60 times and keep exactly maxCarryState genuine
	//    state mocks alongside maxCarryReadThrough read-through mocks.
	totalState := maxCarryState + 60
	for i := 0; i < totalState; i++ {
		st := &models.Mock{
			Version: models.Version("api.keploy.io/v1beta2"),
			Name:    fmt.Sprintf("mock-state-%d", i),
			Kind:    models.Mongo,
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"type":                     "config",
					models.MetadataLifetimeKey: models.MetadataLifetimeSession,
				},
				ReqTimestampMock: t0.Add(time.Second + time.Duration(i)*time.Millisecond),
				ResTimestampMock: t0.Add(time.Second + time.Duration(i)*time.Millisecond + time.Millisecond),
			},
			TestModeInfo: models.TestModeInfo{
				Lifetime:        models.LifetimeSession,
				LifetimeDerived: true,
			},
		}
		doc := []byte(fmt.Sprintf("name: %s\nkind: Mongo\nbody: state-payload-%d\n", st.Name, i))
		sr.wrote(ctx, cur, st, doc, "yaml", nil, nil)
	}

	sr.mu.Lock()
	defer sr.mu.Unlock()

	if len(sr.carryState) != maxCarryState {
		t.Fatalf("len(sr.carryState) = %d, want %d (read-through entries must not inflate carryState)", len(sr.carryState), maxCarryState)
	}
	var stateCount, rtCount int
	for _, c := range sr.carry {
		if c.readThrough {
			rtCount++
		} else if c.mark == nil && !c.startup {
			stateCount++
		}
	}
	if stateCount != maxCarryState {
		t.Fatalf("carried genuine state mocks = %d, want %d (read-through mocks must not evict state mocks)", stateCount, maxCarryState)
	}
	if rtCount != maxCarryReadThrough {
		t.Fatalf("carried read-through mocks = %d, want %d", rtCount, maxCarryReadThrough)
	}
	// Verify every carryState index actually points to a non-readThrough state entry.
	for k, idx := range sr.carryState {
		if idx < 0 || idx >= len(sr.carry) {
			t.Fatalf("carryState index %d out of range [0,%d)", idx, len(sr.carry))
		}
		item := sr.carry[idx]
		if item.readThrough || item.startup || item.mark != nil {
			t.Fatalf("carryState[%x] points to non-state entry %q (readThrough=%v startup=%v mark=%v)",
				k[:4], item.name, item.readThrough, item.startup, item.mark != nil)
		}
	}
}

// TestSetRoll_ReadThroughConflictPairCarriedPastCap proves a conflict sentinel
// whose base mock is already in the carry buffer is retained even once
// maxCarryReadThrough distinct base entries have been carried, while unrelated
// read-through entries and orphan conflict sentinels remain capped.
func TestSetRoll_ReadThroughConflictPairCarriedPastCap(t *testing.T) {
	ctx := context.Background()
	ro := &countRoller{limit: 1 << 30}
	sr := newSetRoll(zap.NewNop(), ro, noopDocStore{}, "test-set-0", nil, nil)
	cur := sr.first()
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	makeRT := func(path, body string, conflict bool, at time.Time) *models.Mock {
		t.Helper()
		m := &models.Mock{
			Version: models.Version("api.keploy.io/v1beta2"),
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "mocks"},
				HTTPReq: &models.HTTPReq{
					Method: models.Method("GET"),
					URL:    "http://cfg.internal" + path,
					Header: map[string]string{"Host": "cfg.internal"},
				},
				HTTPResp: &models.HTTPResp{
					StatusCode: 200,
					Body:       body,
				},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
			},
		}
		key, ok := models.ReadThroughKey(m)
		if !ok {
			t.Fatalf("ReadThroughKey failed for %s", path)
		}
		if conflict {
			m.Name = models.ReadThroughConflictMockName(key)
		} else {
			m.Name = models.ReadThroughMockName(key)
		}
		if !models.MarkReadThrough(m) {
			t.Fatalf("MarkReadThrough failed for %s", m.Name)
		}
		return m
	}

	// Fill carryReadThrough right up to maxCarryReadThrough.
	var lastPath string
	for i := 0; i < maxCarryReadThrough; i++ {
		lastPath = fmt.Sprintf("/v1/item/%d", i)
		m := makeRT(lastPath, `{"v":1}`, false, t0.Add(time.Duration(i)*time.Millisecond))
		sr.wrote(ctx, cur, m, []byte("name: "+m.Name+"\n"), "yaml", nil, nil)
	}

	// Now at cap (512): a conflict sentinel for an already-carried base MUST
	// be carried so rollover never drops conflict protection.
	conflictForCarried := makeRT(lastPath, `{"v":2}`, true, t0.Add(time.Second))
	sr.wrote(ctx, cur, conflictForCarried, []byte("name: "+conflictForCarried.Name+"\n"), "yaml", nil, nil)

	// An unrelated 513th base and an orphan conflict sentinel whose base was
	// never carried MUST still be dropped at the cap.
	extraBase := makeRT("/v1/extra", `{"v":1}`, false, t0.Add(2*time.Second))
	sr.wrote(ctx, cur, extraBase, []byte("name: "+extraBase.Name+"\n"), "yaml", nil, nil)
	orphanConflict := makeRT("/v1/extra", `{"v":2}`, true, t0.Add(3*time.Second))
	sr.wrote(ctx, cur, orphanConflict, []byte("name: "+orphanConflict.Name+"\n"), "yaml", nil, nil)

	sr.mu.Lock()
	defer sr.mu.Unlock()
	if _, ok := sr.carryNames[conflictForCarried.Name]; !ok {
		t.Fatalf("expected %s to be carried alongside its base mock even at cap %d", conflictForCarried.Name, maxCarryReadThrough)
	}
	if _, ok := sr.carryNames[extraBase.Name]; ok {
		t.Fatalf("513th unrelated base %s must remain capped", extraBase.Name)
	}
	if _, ok := sr.carryNames[orphanConflict.Name]; ok {
		t.Fatalf("orphan %s without carried base must remain capped", orphanConflict.Name)
	}
	if sr.carryReadThrough != maxCarryReadThrough {
		t.Fatalf("carryReadThrough = %d, want %d (conflict pair must not consume extra base slots)", sr.carryReadThrough, maxCarryReadThrough)
	}
}
