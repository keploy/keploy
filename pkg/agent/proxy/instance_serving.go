package proxy

import (
	"errors"
	"fmt"
	"sync"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A replay of more than one test worker serves each worker's connections
// (the kernel's instance stamp on each, structs.DestInfo.Instance) from a
// mock manager of the worker's own: its tests' views, what they consumed,
// its stateful cursors and its variables are its own, so one worker's call
// can never answer from, or use up, another worker's mock.
//
// The set is staged on the run's manager, as before, and every worker's
// manager is staged the same: each staging that reaches the run's manager
// reaches every worker's manager too, and the latest of each kind of
// staging is kept, in the order they last came, so a worker's manager that
// opens later is staged with them as it opens. Each staging reaches the
// run's manager first, which takes over the fresh mocks it is handed
// (ownForStaging): every worker's manager then stages copies of its own.
//
// From the first worker served until the run's manager is replaced (a new
// session), a connection from no worker, or from a worker whose manager was
// stopped or never made, is refused: no manager can tell which test made
// it, and none of them may answer it. Each refusal is counted as a call
// keploy cannot attribute (refuseUnserved), never only logged.

// errNoWorkerInstance refuses a connection the kernel stamped with no test
// worker while workers are served.
var errNoWorkerInstance = errors.New("the call is not part of any test worker's keploy instance, so keploy cannot tell which test made it")

// instanceNotServed refuses a connection or a call of the agent's for a
// worker that keploy does not serve (ended, or never begun).
func instanceNotServed(inst uint32) error {
	return fmt.Errorf("test worker instance %s is not served: it ended, or keploy never began serving it", agent.InstanceLabel(inst))
}

// staging is one kind of staging of the run's manager, as last done.
type staging struct {
	key   string
	apply func(*MockManager)
}

// instanceServing is the proxy's worker managers and the stagings they are
// staged with.
type instanceServing struct {
	// stageMu orders the stagings and a worker manager's start: each
	// staging reaches a manager once, whenever it starts.
	stageMu sync.Mutex
	staged  []staging
	// mu guards on, mgrs and misses, read by every connection.
	mu   sync.RWMutex
	on   bool
	mgrs map[uint32]*MockManager
	// misses is each served worker's misses window (missWindowOf).
	misses map[uint32]*missWindow
}

// stage applies a staging of kind key to the run's manager m and every
// worker's manager, and keeps it as the latest of its kind: one staging of
// each kind is kept, in the order they last came.
func (p *Proxy) stage(m *MockManager, key string, apply func(*MockManager)) {
	s := &p.instances
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	apply(m)
	s.mu.RLock()
	mgrs := make([]*MockManager, 0, len(s.mgrs))
	for _, wm := range s.mgrs {
		mgrs = append(mgrs, wm)
	}
	s.mu.RUnlock()
	for _, wm := range mgrs {
		apply(wm)
	}
	for i, st := range s.staged {
		if st.key == key {
			s.staged = append(s.staged[:i], s.staged[i+1:]...)
			break
		}
	}
	s.staged = append(s.staged, staging{key: key, apply: apply})
}

// stageOn is stage on the run's current manager; errNoMockManager when
// there is none.
func (p *Proxy) stageOn(key string, apply func(*MockManager)) error {
	m := p.getMockManager()
	if m == nil {
		return errNoMockManager
	}
	p.stage(m, key, apply)
	return nil
}

// endInstances stops serving every worker: the run's manager is replaced,
// and what was staged on it is not the next one's.
func (p *Proxy) endInstances() {
	s := &p.instances
	s.stageMu.Lock()
	s.staged = nil
	s.mu.Lock()
	mgrs := s.mgrs
	s.mgrs, s.misses, s.on = nil, nil, false
	s.mu.Unlock()
	s.stageMu.Unlock()
	for _, wm := range mgrs {
		wm.Close()
	}
}

// connManager is the manager a connection the kernel stamped with inst is
// served from: the run's while no worker is served, else inst's own.
func (p *Proxy) connManager(inst uint32) (*MockManager, error) {
	s := &p.instances
	s.mu.RLock()
	on, wm := s.on, s.mgrs[inst]
	s.mu.RUnlock()
	switch {
	case !on:
		return p.getMockManager(), nil
	case wm != nil:
		return wm, nil
	case inst == 0:
		return nil, errNoWorkerInstance
	default:
		return nil, instanceNotServed(inst)
	}
}

// instanceManager is inst's manager, nil when it is not served.
func (p *Proxy) instanceManager(inst uint32) *MockManager {
	s := &p.instances
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mgrs[inst]
}

// ServeInstance implements agent.InstanceServer: it starts serving test
// worker inst from a manager of its own, staged as the run's is, and
// returns how the agent serves inst's tests on it. While the run's manager
// serves exactly, the worker's begins so, with nothing in its pool: a call
// it makes before the agent serves it its pool answers nothing staged.
func (p *Proxy) ServeInstance(inst uint32) (agent.ExactServer, error) {
	if inst == 0 {
		return nil, errors.New("test worker instance 0 is no worker: keploy gives each worker an instance id from 1")
	}
	run := p.getMockManager()
	if run == nil {
		return nil, errNoMockManager
	}
	s := &p.instances
	s.stageMu.Lock()
	defer s.stageMu.Unlock()
	s.mu.RLock()
	_, dup := s.mgrs[inst]
	s.mu.RUnlock()
	if dup {
		return nil, fmt.Errorf("test worker instance %s is served already", agent.InstanceLabel(inst))
	}
	wm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), p.logger.With(zap.Uint32("workerInstance", inst)))
	for _, st := range s.staged {
		st.apply(wm)
	}
	if run.exactView() != nil {
		wm.ServeExactPool(models.PoolKeyNone, nil)
	}
	s.mu.Lock()
	if s.mgrs == nil {
		s.mgrs = make(map[uint32]*MockManager)
	}
	s.mgrs[inst] = wm
	if s.misses == nil {
		s.misses = make(map[uint32]*missWindow)
	}
	s.misses[inst] = &missWindow{}
	s.on = true
	s.mu.Unlock()
	return instanceExact{p: p, inst: inst}, nil
}

// StopServingInstance implements agent.InstanceServer: inst's manager goes,
// and a later connection inst's processes make is refused.
func (p *Proxy) StopServingInstance(inst uint32) error {
	s := &p.instances
	s.stageMu.Lock()
	s.mu.Lock()
	wm := s.mgrs[inst]
	delete(s.mgrs, inst)
	delete(s.misses, inst)
	s.mu.Unlock()
	s.stageMu.Unlock()
	if wm == nil {
		return instanceNotServed(inst)
	}
	wm.Close()
	return nil
}

var _ agent.InstanceServer = (*Proxy)(nil)

// instanceExact serves one test worker's tests on its own manager, as the
// proxy's own methods serve the run's.
type instanceExact struct {
	p    *Proxy
	inst uint32
}

func (s instanceExact) mgr() (*MockManager, error) {
	if m := s.p.instanceManager(s.inst); m != nil {
		return m, nil
	}
	return nil, instanceNotServed(s.inst)
}

func (s instanceExact) open(open func(*MockManager)) error {
	m, err := s.mgr()
	if err != nil {
		return err
	}
	return s.p.openExactOn(m, open)
}

func (s instanceExact) ServeExactTest(test string, owned []*models.Mock) error {
	return s.open(func(m *MockManager) { m.ServeExactTest(test, owned) })
}

func (s instanceExact) OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) error {
	return s.open(func(m *MockManager) { m.OpenExactTest(test, owned, given) })
}

func (s instanceExact) ServeExactTestConcurrent(test string, owned, cands []*models.Mock) error {
	return s.open(func(m *MockManager) { m.ServeExactTestConcurrent(test, owned, cands) })
}

func (s instanceExact) OpenExactTestConcurrent(test string, owned, cands []*models.Mock, given []models.VariableBinding) error {
	return s.open(func(m *MockManager) { m.OpenExactTestConcurrent(test, owned, cands, given) })
}

func (s instanceExact) GiveExactTest(test string, b models.VariableBinding) error {
	m, err := s.mgr()
	if err != nil {
		return err
	}
	return s.p.giveExactOn(m, test, b)
}

func (s instanceExact) ServeExactPool(key string, names []string) error {
	m, err := s.mgr()
	if err != nil {
		return err
	}
	m.ServeExactPool(key, names)
	return nil
}

func (s instanceExact) ServeExactPoolAhead(key string, names []string, ahead []*models.Mock) error {
	m, err := s.mgr()
	if err != nil {
		return err
	}
	m.ServeExactPool(key, names, ahead...)
	return nil
}

func (s instanceExact) ServeExactWorker(names []string) error {
	m, err := s.mgr()
	if err != nil {
		return err
	}
	m.ServeExactWorker(names)
	return nil
}

func (s instanceExact) RestartExactKey(key string, names []string) error {
	m, err := s.mgr()
	if err != nil {
		return err
	}
	m.RestartExactKey(key, names)
	return nil
}

func (s instanceExact) ExactTestOutcome() (string, []string, []string, bool) {
	m, err := s.mgr()
	if err != nil {
		return "", nil, nil, false
	}
	return m.ExactTestOutcome()
}

var (
	_ agent.ExactServer           = instanceExact{}
	_ agent.ExactTestOpener       = instanceExact{}
	_ agent.ExactTestGiver        = instanceExact{}
	_ agent.ExactConcurrentServer = instanceExact{}
	_ agent.ExactAheadServer      = instanceExact{}
	_ agent.ExactWorkerServer     = instanceExact{}
	_ agent.ExactKeyRestarter     = instanceExact{}
)

// refuseUnserved is the error that ends a connection no test worker's
// manager may answer (connManager's refusal), before anything answers it:
// which process made it, and where it went. The refusal is counted as a
// call keploy cannot attribute (models.CaptureLossUnattributed), which
// leaves each test open as it was made without a verdict: the call went
// unanswered, and any of them may have made it, or waited on it.
func (p *Proxy) refuseUnserved(dest *agent.NetworkAddress, dst string, why error) error {
	what := fmt.Sprintf("the latest: a connection to %s, from process %d, in no test worker's keploy instance", dst, dest.KernelPid)
	if dest.Instance != 0 {
		what = fmt.Sprintf("the latest: a connection to %s, from process %d of test worker instance %s, which keploy does not serve (the worker ended, or keploy never began serving it)", dst, dest.KernelPid, agent.InstanceLabel(dest.Instance))
	}
	p.losses.Add(models.CaptureLossUnattributed, what)
	return fmt.Errorf("keploy refused a call of process %d (test worker instance %s) to %s: %w", dest.KernelPid, agent.InstanceLabel(dest.Instance), dst, why)
}
