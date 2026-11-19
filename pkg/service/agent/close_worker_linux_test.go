//go:build linux

package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// ownedSource is a proxy's owned losses (coreAgent.OwnedLossTaker), each
// taken once, and no count. As the proxy does, it keeps each worker's test
// epoch (coreAgent.PairedBytesBeginner), moved at each begin it is told and
// at each end's settle done, and stamps a close of a worker's connection
// with it as it is put.
type ownedSource struct {
	mu     sync.Mutex
	owned  []coreAgent.OwnedLoss
	epochs map[uint32]uint64
	// told is what the agent told it of a run of one test process
	// (PairedBytesOneProcess), "on" or "off", and each read of its counts
	// ("read"), in order.
	told []string
}

func (s *ownedSource) put(o coreAgent.OwnedLoss) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Every loss is stamped with its worker's epoch now, as the proxy's
	// ownKind stamps it, one that names its test too.
	o.Epoch = s.epochs[o.Worker]
	s.owned = append(s.owned, o)
}

func (s *ownedSource) move(worker uint32) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epochs == nil {
		s.epochs = map[uint32]uint64{}
	}
	s.epochs[worker]++
	return s.epochs[worker]
}

func (s *ownedSource) PairedBytesBegun(worker uint32) uint64 { return s.move(worker) }

func (s *ownedSource) PairedBytesSettled(worker uint32) { s.move(worker) }

// PairedBytesOneProcess notes what it is told and, turned on, moves worker
// 0's epoch, as the proxy does.
func (s *ownedSource) PairedBytesOneProcess(on bool) {
	s.mu.Lock()
	if on {
		s.told = append(s.told, "on")
	} else {
		s.told = append(s.told, "off")
	}
	s.mu.Unlock()
	if on {
		s.move(0)
	}
}

// toldOf is what the agent told s of a run of one test process, and its
// reads of the counts, in order, since the last toldOf; it forgets them.
func (s *ownedSource) toldOf() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.told
	s.told = nil
	return out
}

func (s *ownedSource) TakeOwnedLosses() []coreAgent.OwnedLoss {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.owned
	s.owned = nil
	return out
}

// CaptureLosses counts nothing, and notes the read in told ("read"), so a
// row can tell what the agent told the proxy before a read of the counts.
func (s *ownedSource) CaptureLosses() models.CaptureLosses {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.told = append(s.told, "read")
	return nil
}

// ownedWire and ownedInstanceWire are the record's and the replay's worker
// proxies with owned losses.
type ownedWire struct {
	*wireProxy
	*ownedSource
}

type ownedInstanceWire struct {
	*instanceWire
	*ownedSource
}

// kindOf counts the losses of kind w's evidence charges, judged (capture
// incomplete) and not (cannot capture).
func kindOf(w models.ScopeWindow, kind models.CaptureLossKind) (judged, notJudged uint64) {
	if w.Evidence == nil {
		return 0, 0
	}
	return kindCount(w.Evidence.CaptureIncomplete, kind), kindCount(w.Evidence.CannotCapture, kind)
}

// A close of one test worker's connection is charged to that worker's open
// test alone, never to another worker's open test, which never used the
// connection: an untold close leaves the one NOT JUDGED, a loss keploy saw
// fails it in a replay; the other worker's test is untouched, whichever test
// ends first (the loss taken at the other's end is kept for its own). A loss
// of a worker with no test open is the run's, charged to no test.
func TestACloseOfOneWorkersConnectionSparesAnotherWorkersTest(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, kind := range []models.CaptureLossKind{models.CaptureLossAppBytesUntold, models.CaptureLossAppBytesLost} {
			for _, oneFirst := range []bool{true, false} {
				order := "TestOne ends first"
				if !oneFirst {
					order = "TestTwo ends first"
				}
				t.Run(string(mode)+"/"+string(kind)+"/"+order, func(t *testing.T) {
					var a *Agent
					src := &ownedSource{}
					if mode == models.MODE_TEST {
						var iw *instanceWire
						a, iw, _ = instanceAgent(t, mode)
						a.Proxy = &ownedInstanceWire{instanceWire: iw, ownedSource: src}
						a.proven.pool = map[string][]string{"a": {"pa"}, "b": {"pb"}}
					} else {
						a, _ = workerAgent(t, mode, 4)
						a.Proxy = &ownedWire{wireProxy: a.Proxy.(*wireProxy), ownedSource: src}
					}
					a.proven.losses = a.markCaptureLosses(context.Background())
					w1, w2 := child(t), child(t)
					claimWorker(t, a, w1.Process.Pid, "a", 2)
					claimWorker(t, a, w2.Process.Pid, "b", 2)
					_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
					require.Nil(t, serr)
					_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
					require.Nil(t, serr)
					inst1 := a.scopeInstance("a::TestOne")
					require.NotZero(t, inst1, "premise: TestOne is a worker's test")
					require.NotEqual(t, inst1, a.scopeInstance("b::TestTwo"), "premise: the tests are two workers'")
					// Worker 1's connection, used by TestOne alone, closes.
					src.put(coreAgent.OwnedLoss{Worker: inst1, Open: true, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: "from 127.0.0.1:62766"}})
					endOne := func() {
						_, serr := a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
						require.Nil(t, serr)
					}
					endTwo := func() {
						_, serr := a.ProvenEnd(as(w2.Process.Pid), models.ScopeReq{Name: "TestTwo", Outcome: models.ScopeOutcomePassed})
						require.Nil(t, serr)
					}
					if oneFirst {
						endOne()
						// Worker 1 has no test open: a close of its
						// connection now is the run's.
						src.put(coreAgent.OwnedLoss{Worker: inst1, Open: true, Loss: models.CaptureLoss{Kind: kind, Count: 1, What: "from 127.0.0.1:62767"}})
						endTwo()
					} else {
						endTwo()
						endOne()
					}
					judged, notJudged := kindOf(windowOf(t, a, "a::TestOne"), kind)
					one := windowOf(t, a, "a::TestOne")
					if kind == models.CaptureLossAppBytesUntold {
						require.EqualValues(t, 1, notJudged, "TestOne, whose worker's connection closed untold, is not judged")
						require.Zero(t, judged)
					} else {
						require.EqualValues(t, 1, judged, "TestOne, whose worker's connection lost bytes, is charged the loss")
						if mode == models.MODE_TEST {
							require.Equal(t, models.ScopeOutcomeFailed, one.Outcome, "a loss keploy saw fails TestOne in a replay")
						}
					}
					judged, notJudged = kindOf(windowOf(t, a, "b::TestTwo"), kind)
					two := windowOf(t, a, "b::TestTwo")
					if judged != 0 || notJudged != 0 {
						t.Errorf("worker 2's TestTwo, which never used worker 1's connection, is charged %d judged and %d unjudged %s; its outcome %s", judged, notJudged, kind, two.Outcome)
					}
					require.NotEqual(t, models.ScopeOutcomeFailed, two.Outcome)
					if oneFirst {
						run, err := a.UnchargedCaptureLosses(context.Background())
						require.NoError(t, err)
						require.EqualValues(t, 1, kindCount(run, kind), "the close of a worker with no test open is the run's: %+v", run)
					}
				})
			}
		}
	}
}

// settledWire is the record's worker proxy, told each settle done
// (coreAgent.PairedBytesSettler).
type settledWire struct {
	*wireProxy
	smu     sync.Mutex
	settled []uint32
}

func (p *settledWire) PairedBytesSettled(worker uint32) {
	p.smu.Lock()
	defer p.smu.Unlock()
	p.settled = append(p.settled, worker)
}

// A test's end tells the proxy once its settle is done, for the test's own
// worker: what the settle moved on that worker's connections is the ending
// test's (the proxy's rows say how), never the next test's.
func TestAnEndTellsTheProxyItsSettleIsDoneForItsWorker(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	sw := &settledWire{wireProxy: a.Proxy.(*wireProxy)}
	a.Proxy = sw
	w1, w2 := child(t), child(t)
	claimWorker(t, a, w1.Process.Pid, "a", 2)
	claimWorker(t, a, w2.Process.Pid, "b", 2)
	_, serr := a.ProvenBegin(as(w1.Process.Pid), begin("TestOne"))
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(as(w2.Process.Pid), begin("TestTwo"))
	require.Nil(t, serr)
	inst1 := a.scopeInstance("a::TestOne")
	require.NotZero(t, inst1)
	sw.smu.Lock()
	sw.settled = nil
	sw.smu.Unlock()
	_, serr = a.ProvenEnd(as(w1.Process.Pid), models.ScopeReq{Name: "TestOne", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	sw.smu.Lock()
	defer sw.smu.Unlock()
	require.Contains(t, sw.settled, inst1, "TestOne's end told the proxy its settle was done for its worker")
	for _, w := range sw.settled {
		require.Equal(t, inst1, w, "TestOne's end told the proxy of another worker's settle: %v", sw.settled)
	}
}
