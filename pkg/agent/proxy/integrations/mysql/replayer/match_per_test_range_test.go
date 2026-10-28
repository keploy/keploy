package replayer_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// rangingStore is the real manager as a store that hands the per-test tier
// over one mock at a time (integrations.PerTestRanger), counting the mocks of
// that tier it hands the matcher: each mock of a GetPerTestMocksInWindow
// read, and each one a RangePerTestMocksInWindow walk reads.
type rangingStore struct {
	*proxy.MockManager
	handed int
}

var _ integrations.PerTestRanger = (*rangingStore)(nil)

func (s *rangingStore) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	mocks, err := s.MockManager.GetPerTestMocksInWindow()
	s.handed += len(mocks)
	return mocks, err
}

func (s *rangingStore) RangePerTestMocksInWindow(fn func(*models.Mock) bool) error {
	mocks, err := s.MockManager.GetPerTestMocksInWindow()
	if err != nil {
		return err
	}
	for _, mk := range mocks {
		s.handed++
		if !fn(mk) {
			break
		}
	}
	return nil
}

// matchThrough is diffSide.match with the command matched against db, a store
// over s's manager.
func (s *diffSide) matchThrough(ctx context.Context, db integrations.MockMemDb, req mysql.Request, eng *mocknoise.Engine, fullScan bool) string {
	resp, ok, miss, err := replayer.MatchCommandForTest(ctx, zap.NewNop(), req, db, s.dctx, eng, fullScan)
	return s.describe(resp, ok, miss, err)
}

// keyedSetReq is the command mock i of keyedSet recorded, as the app sends it
// again.
func keyedSetReq(i int) mysql.Request {
	switch i % 4 {
	case 0, 1:
		return queryReq(keyedSetText(i))
	case 2:
		return executeReq(7, fmt.Sprintf("k%d", i%500))
	default:
		return executeReq(8, fmt.Sprintf("k%d", i%500))
	}
}

// TestAWholeSetsCommandReadsTheTestsMocksOnlyUpToItsOwnRecording: one test
// owns a whole set of 1,000 or 10,000 mocks, as keploy mock replay serves a go
// test whose run recorded the set, and the app sends the set's commands again
// in recorded order. Each command is answered by its own recording, the open
// test's next unanswered mock. Through a store that hands the per-test tier
// over one mock at a time (integrations.PerTestRanger), the keyed pass reads
// that tier only up to the recording: one mock per command at either size.
// The keyed pass read the whole tier (GetPerTestMocksInWindow) for every
// command, every mock the test had not answered yet, so a command's cost grew
// with the set.
func TestAWholeSetsCommandReadsTheTestsMocksOnlyUpToItsOwnRecording(t *testing.T) {
	sizes := []int{1000, 10000}
	if testing.Short() || raceEnabled {
		sizes = []int{100, 1000}
	}
	const commands = 80
	for _, n := range sizes {
		s := newDiffSide(keyedSet(n, models.LifetimePerTest))
		s.stageNoWindow()
		s.ownWholeSet()
		db := &rangingStore{MockManager: s.mm}
		for i := 0; i < commands; i++ {
			before := db.handed
			got := s.matchThrough(context.Background(), db, keyedSetReq(i), nil, false)
			if !strings.Contains(got, "ok=true") || !strings.Contains(got, fmt.Sprintf(" m-%d:", i)) {
				t.Fatalf("n=%d command %d: not served from its own recording m-%d: %s", n, i, i, got)
			}
			if read := db.handed - before; read != 1 {
				t.Fatalf("n=%d command %d: the keyed pass read %d mocks of the per-test tier, want 1 (its own recording, the test's next one)", n, i, read)
			}
		}
		s.mm.Close()
	}
}

// TestAWholeSetServedThroughAPerTestRangerServesWhatTheFullScanServes is
// TestExactServingWholeSetServesWhatTheFullScanServes with the keyed pass
// reading the per-test tier one mock at a time (rangingStore), and stopping
// where the command settles: recorded reads, EXECUTEs whose parameters no
// recording bound (each settled by the test's own FIFO candidate, after a walk
// of the whole tier), reads of rows the set never recorded, and PREPAREs the
// connection's recorded setup answers (a mock of the connection tier, which
// the test does not own; the keyed pass settles each, reading the per-test
// tier once). Both sides serve, consume and report the same.
func TestAWholeSetServedThroughAPerTestRangerServesWhatTheFullScanServes(t *testing.T) {
	commands := 400
	if raceEnabled {
		commands = 104
	}
	for _, strict := range []bool{false, true} {
		owned := keyedSet(1000, models.LifetimePerTest)
		pool := append(owned[:len(owned):len(owned)], diffPrepareMock("prepare-a", "c1", diffQueryA, 7, diffBase.Add(-time.Millisecond)))
		ref, fast := newDiffSide(pool), newDiffSide(pool)
		var eng *mocknoise.Engine
		if strict {
			eng = replayer.StrictEngineForTest()
		}
		for _, s := range []*diffSide{ref, fast} {
			s.stageNoWindow()
			copies := make([]*models.Mock, len(owned))
			for i, m := range owned {
				copies[i] = m.DeepCopy()
			}
			s.mm.ServeExactTest("whole-set", copies)
		}
		db := &rangingStore{MockManager: fast.mm}
		ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
		prepared := 0
		for i := 0; i < commands; i++ {
			var req mysql.Request
			switch i % 8 {
			case 0:
				req = queryReq(keyedSetText(i))
			case 1:
				req = queryReq("SELECT id, name, payload FROM small WHERE id = 9999")
			case 2, 4:
				req = executeReq(7, fmt.Sprintf("k%d", i%500))
			case 3:
				req = executeReq(8, fmt.Sprintf("k%d", i%500))
			case 7:
				req = prepareReq(diffQueryA)
			default:
				req = executeReq(uint32(7+i%2), fmt.Sprintf("app-made-%d", i))
			}
			want := ref.match(ctx, req, eng, true)
			before := db.handed
			got := fast.matchThrough(ctx, db, req, eng, false)
			if want != got {
				t.Fatalf("strict=%v command %d:\n full scan:  %s\n keyed pass: %s", strict, i, want, got)
			}
			if i%8 == 7 && strings.Contains(want, "served prepare-a ") {
				prepared++
				// No recording of the test answers it: the keyed pass reads
				// the per-test tier through, then the connection tier, and
				// settles there, with no full scan reading the tier again.
				if read := db.handed - before; read > len(owned) {
					t.Fatalf("strict=%v command %d: the PREPARE read %d mocks of the per-test tier, more than the %d the test owns: it was not settled by the keyed pass", strict, i, read, len(owned))
				}
			}
		}
		if db.handed == 0 {
			t.Fatalf("strict=%v: the keyed pass never read the per-test tier", strict)
		}
		if prepared == 0 {
			t.Fatalf("strict=%v: no PREPARE was answered by the connection's recorded setup", strict)
		}
		ref.mm.Close()
		fast.mm.Close()
	}
}
