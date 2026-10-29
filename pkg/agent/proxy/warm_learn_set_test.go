package proxy

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// warmRow is a proxy whose manager stages a test's recordings and whose
// on-disk store keeps the whole set: the staged ones and those of later
// tests, not begun, recorded an hour after them. disk counts the store's
// reads (RangeRecordedBefore's on-disk source).
type warmRow struct {
	p    *Proxy
	mm   *MockManager
	disk atomic.Int64
}

const warmStaged, warmLater = 12, 8

func newWarmRow(t *testing.T, logger *zap.Logger) *warmRow {
	t.Helper()
	r := &warmRow{p: startedDrainProxyLogging(t, 10, logger)}
	r.p.learnTracker = learn.NewTracker()
	r.mm = NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(r.mm.Close)
	r.p.mockManager = r.mm
	now := time.Now()
	var staged, all []*models.Mock
	for i := 0; i < warmStaged+warmLater; i++ {
		at := now.Add(time.Duration(i+1) * time.Millisecond)
		if i >= warmStaged {
			at = at.Add(time.Hour)
		}
		m := r3Mock(fmt.Sprintf("mock-w%d", i), at, "POST", "/log",
			fmt.Sprintf(`{"at":"%s","n":%d,"note":"word%d other%d"}`, at.UTC().Format(time.RFC3339Nano), i, i, i), `{"ok":true}`)
		m.DeriveLifetime()
		all = append(all, m)
		if i < warmStaged {
			staged = append(staged, m)
		}
	}
	r.mm.SetMocksWithWindow(staged, nil, now, now.Add(time.Second))
	r.mm.SetRecordedBefore(func(before time.Time, visit func(*models.Mock) bool) error {
		r.disk.Add(1)
		for _, m := range all {
			if m.Spec.ReqTimestampMock.After(before) {
				continue
			}
			if !visit(m) {
				return nil
			}
		}
		return nil
	})
	return r
}

// (mongo-stuck-handler, the learn step's cost per call.) The agent reads a
// staged set's recordings for the learn step (WarmLearnSet) in record's full
// round alone, while the set's learn step is on, and there it reads every
// recording of the set: those the manager stages and those of later tests
// the on-disk store keeps. Any other session reads none of them, not one
// read of the store: keploy test's replay, keploy mock's replay and a
// retry cycle after the learn step ended (SetLearnEnded) learn nothing, so
// a staging there would read the set's recordings, on every set, for
// nothing. The gate is decided before anything is read (warmLearnSet gives
// no staging), so a session it shuts reads nothing however its goroutines
// run. With no mock manager there is no set: the call is refused.
func TestAStagedSetIsReadForTheLearnStepInRecordsFullRoundAlone(t *testing.T) {
	cases := []struct {
		name  string
		set   func(t *testing.T, r *warmRow)
		reads bool
	}{
		{"record's full round", func(t *testing.T, r *warmRow) { r.mm.SetPairing(true) }, true},
		{"the learn step taken up again", func(t *testing.T, r *warmRow) {
			r.mm.SetPairing(true)
			mustLearnEnded(t, r.p, true)
			mustLearnEnded(t, r.p, false)
		}, true},
		{"proven switch off", func(t *testing.T, r *warmRow) {
			r.mm.SetPairing(true)
			t.Setenv(provenswitch.Env, "")
		}, false},
		{"not record's full round", func(t *testing.T, r *warmRow) { r.mm.SetPairing(false) }, false},
		{"the set's learn step ended", func(t *testing.T, r *warmRow) {
			r.mm.SetPairing(true)
			mustLearnEnded(t, r.p, true)
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			r := newWarmRow(t, zap.NewNop())
			c.set(t, r)
			done, err := r.p.warmLearnSet()
			if err != nil {
				t.Fatalf("the set's staging was refused: %v", err)
			}
			if !c.reads {
				if done != nil {
					t.Fatalf("%s: the agent staged the set's recordings for a learn step that learns nothing", c.name)
				}
				if n := r.disk.Load(); n != 0 {
					t.Fatalf("%s: the agent read the on-disk store %d times for a learn step that learns nothing", c.name, n)
				}
				if err := r.p.WarmLearnSet(); err != nil {
					t.Fatalf("%s: WarmLearnSet failed: %v", c.name, err)
				}
				return
			}
			if done == nil {
				t.Fatalf("%s: the agent did not stage the set's recordings for its learn step", c.name)
			}
			u, ok := <-done
			if !ok {
				t.Fatalf("%s: the set's staging ended with nothing read", c.name)
			}
			if u.Staged != warmStaged+warmLater || u.Called != 0 {
				t.Fatalf("%s: the set's staging read %d recordings and its calls %d: want all %d (%d staged, %d of later tests on disk), none by a call",
					c.name, u.Staged, u.Called, warmStaged+warmLater, warmStaged, warmLater)
			}
			if n := r.disk.Load(); n != 1 {
				t.Fatalf("%s: the set's staging read the on-disk store %d times: want once", c.name, n)
			}
		})
	}

	t.Run("its log line", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		core, logs := observer.New(zapcore.InfoLevel)
		r := newWarmRow(t, zap.New(core))
		r.mm.SetPairing(true)
		done, err := r.p.warmLearnSet()
		if err != nil || done == nil {
			t.Fatalf("the set's staging did not start: %v", err)
		}
		r.p.logSetUse(done)
		lines := logs.FilterMessageSnippet("the learn step read the test set's recordings as it was staged").All()
		if len(lines) != 1 {
			t.Fatalf("the set's staging logged %d lines: want one", len(lines))
		}
		f := lines[0].ContextMap()
		if f["read_at_staging"] != int64(warmStaged+warmLater) || f["read_by_calls"] != int64(0) || f["set_budget_bytes"].(int64) <= 0 {
			t.Fatalf("the set's staging logged %v: want %d read at staging, none by calls, and the set's budget", f, warmStaged+warmLater)
		}
	})

	t.Run("no mock manager", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		p := startedDrainProxy(t, 10)
		p.learnTracker = learn.NewTracker()
		if done, err := p.warmLearnSet(); !errors.Is(err, errNoMockManager) || done != nil {
			t.Fatalf("with no mock manager the staging gave %v, %v: want errNoMockManager and nothing read", done, err)
		}
		if err := p.WarmLearnSet(); !errors.Is(err, errNoMockManager) {
			t.Fatalf("with no mock manager WarmLearnSet gave %v: want errNoMockManager", err)
		}
	})
}

func mustLearnEnded(t *testing.T, p *Proxy, ended bool) {
	t.Helper()
	if err := p.SetLearnEnded(ended); err != nil {
		t.Fatalf("SetLearnEnded(%v): %v", ended, err)
	}
}
