package cbshim

import (
	"context"
	"crypto/x509"
	"errors"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// resetRegistry clears any factories registered by other tests
// running in this package. cbshim's registration is process-global;
// without an explicit reset each test would inherit the previous
// test's state and produce confusing fan-out counts.
func resetRegistry(t *testing.T) {
	t.Helper()
	saved := registeredFactories
	registeredFactories = nil
	t.Cleanup(func() { registeredFactories = saved })
}

// fakeCBShim is a CBShim that records each method invocation in a
// slice keyed by name. Tests assert on the recorded call counts /
// connIDs to confirm fan-out semantics.
type fakeCBShim struct {
	mu                sync.Mutex
	registerMITM      []string
	registerReal      []string
	cleanupConnection []string
	attachToProcTree  []int
	startProcConsumer int
	closeCalled       int
	attachErr         error
	closeErr          error
}

func (f *fakeCBShim) RegisterMITM(connID string, _ []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerMITM = append(f.registerMITM, connID)
}

func (f *fakeCBShim) RegisterReal(connID string, _ []byte, _ x509.SignatureAlgorithm) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerReal = append(f.registerReal, connID)
}

func (f *fakeCBShim) CleanupConnection(connID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupConnection = append(f.cleanupConnection, connID)
}

func (f *fakeCBShim) AttachToProcessTree(rootPID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachToProcTree = append(f.attachToProcTree, rootPID)
	return f.attachErr
}

func (f *fakeCBShim) StartProcEventConsumer(_ context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startProcConsumer++
}

func (f *fakeCBShim) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalled++
	return f.closeErr
}

// TestRegisterFactory_AppendsAcrossCalls covers the core change from
// the original singleton: two RegisterFactory calls must produce two
// registered factories. Without this, blank-importing both an eBPF
// shim and an LD_PRELOAD shim would silently drop one — the symptom
// is that one of the two SCRAM-PLUS paths fails to substitute hashes
// in production and the diagnosis ("oh, only the second register
// won") is invisible from a log line.
func TestRegisterFactory_AppendsAcrossCalls(t *testing.T) {
	resetRegistry(t)

	RegisterFactory(func(*zap.Logger) (CBShim, error) { return &fakeCBShim{}, nil })
	RegisterFactory(func(*zap.Logger) (CBShim, error) { return &fakeCBShim{}, nil })

	if got, want := len(registeredFactories), 2; got != want {
		t.Fatalf("registeredFactories len = %d, want %d (append, not overwrite)", got, want)
	}
}

// Defensive: a nil Factory passed to RegisterFactory must not pollute
// the slice — NewFromFactory would otherwise panic invoking nil.
func TestRegisterFactory_IgnoresNil(t *testing.T) {
	resetRegistry(t)

	RegisterFactory(nil)

	if got := len(registeredFactories); got != 0 {
		t.Fatalf("registeredFactories len = %d, want 0 (nil factory dropped)", got)
	}
}

// Single-factory path must be unchanged: NewFromFactory returns the
// concrete impl directly, not wrapped in a composite. Pre-multi-
// factory callers (the existing enterprise eBPF cbshim) observe the
// exact same concrete type.
func TestNewFromFactory_SingleFactory_ReturnsBareImpl(t *testing.T) {
	resetRegistry(t)
	want := &fakeCBShim{}

	RegisterFactory(func(*zap.Logger) (CBShim, error) { return want, nil })

	got, err := NewFromFactory(zap.NewNop())
	if err != nil {
		t.Fatalf("NewFromFactory err = %v, want nil", err)
	}
	if got != want {
		t.Errorf("got bare-impl identity %p, want %p (single-factory path must not wrap in composite)", got, want)
	}
	if _, isComposite := got.(*composite); isComposite {
		t.Errorf("single-factory NewFromFactory returned *composite; expected bare impl for backward compat")
	}
}

// No registered factories → (nil, nil). OSS-only builds rely on this
// to detect "no cbshim available" without an error log; if it ever
// returned an error, the OSS proxy.New code would surface a scary
// log line on every startup.
func TestNewFromFactory_NoFactories_ReturnsNilNil(t *testing.T) {
	resetRegistry(t)

	got, err := NewFromFactory(zap.NewNop())
	if err != nil {
		t.Fatalf("NewFromFactory err = %v, want nil for empty registry", err)
	}
	if got != nil {
		t.Errorf("NewFromFactory cb = %v, want nil for empty registry", got)
	}
}

// Multi-factory path: NewFromFactory must wrap impls in *composite
// and fan calls out to each.
func TestNewFromFactory_TwoFactories_FansOutAllMethods(t *testing.T) {
	resetRegistry(t)
	a := &fakeCBShim{}
	b := &fakeCBShim{}

	RegisterFactory(func(*zap.Logger) (CBShim, error) { return a, nil })
	RegisterFactory(func(*zap.Logger) (CBShim, error) { return b, nil })

	cb, err := NewFromFactory(zap.NewNop())
	if err != nil {
		t.Fatalf("NewFromFactory err = %v, want nil", err)
	}
	if _, ok := cb.(*composite); !ok {
		t.Fatalf("two-factory NewFromFactory returned %T, want *composite", cb)
	}

	cb.RegisterMITM("conn-1", []byte("mitm-der"))
	cb.RegisterReal("conn-1", []byte("real-der"), x509.SHA256WithRSA)
	cb.CleanupConnection("conn-1")
	if err := cb.AttachToProcessTree(1234); err != nil {
		t.Errorf("AttachToProcessTree err = %v, want nil", err)
	}
	cb.StartProcEventConsumer(context.Background())
	if err := cb.Close(); err != nil {
		t.Errorf("Close err = %v, want nil", err)
	}

	// Each underlying impl saw every call exactly once.
	for name, impl := range map[string]*fakeCBShim{"a": a, "b": b} {
		impl.mu.Lock()
		if got, want := len(impl.registerMITM), 1; got != want {
			t.Errorf("%s.RegisterMITM count = %d, want %d", name, got, want)
		}
		if got, want := len(impl.registerReal), 1; got != want {
			t.Errorf("%s.RegisterReal count = %d, want %d", name, got, want)
		}
		if got, want := len(impl.cleanupConnection), 1; got != want {
			t.Errorf("%s.CleanupConnection count = %d, want %d", name, got, want)
		}
		if got, want := len(impl.attachToProcTree), 1; got != want {
			t.Errorf("%s.AttachToProcessTree count = %d, want %d", name, got, want)
		}
		if got, want := impl.startProcConsumer, 1; got != want {
			t.Errorf("%s.StartProcEventConsumer count = %d, want %d", name, got, want)
		}
		if got, want := impl.closeCalled, 1; got != want {
			t.Errorf("%s.Close count = %d, want %d", name, got, want)
		}
		impl.mu.Unlock()
	}
}

// A registered factory that returns (nil, nil) is a contract
// violation. In the single-factory path this surfaced as an
// errors.New; the multi-factory path must surface it the same way so
// a misregistered impl is caught at proxy.New time, not at the first
// SCRAM-PLUS handshake.
func TestNewFromFactory_FactoryReturnsNilNil_IsError(t *testing.T) {
	resetRegistry(t)
	RegisterFactory(func(*zap.Logger) (CBShim, error) { return nil, nil })

	_, err := NewFromFactory(zap.NewNop())
	if err == nil {
		t.Fatalf("NewFromFactory err = nil, want error for (nil, nil) factory")
	}
}

// Multi-factory equivalent: one good + one bad factory → whole
// NewFromFactory aborts. Otherwise a half-constructed composite
// would silently run with a missing backend and the user would only
// notice when their statically-bundled libcrypto app fails to
// SCRAM-PLUS auth.
func TestNewFromFactory_OneFactoryErrors_AbortsAll(t *testing.T) {
	resetRegistry(t)
	bad := errors.New("eBPF unsupported on this kernel")

	RegisterFactory(func(*zap.Logger) (CBShim, error) { return &fakeCBShim{}, nil })
	RegisterFactory(func(*zap.Logger) (CBShim, error) { return nil, bad })

	cb, err := NewFromFactory(zap.NewNop())
	if err == nil {
		t.Fatalf("NewFromFactory err = nil, want %v", bad)
	}
	if !errors.Is(err, bad) {
		t.Errorf("NewFromFactory err = %v, want errors.Is %v (lost the underlying cause)", err, bad)
	}
	if cb != nil {
		t.Errorf("NewFromFactory cb = %v, want nil on factory error", cb)
	}
}

// Close must run every impl's Close even if an earlier one errors —
// otherwise a failing eBPF teardown would strand the LD_PRELOAD
// impl's staged .so on disk.
func TestComposite_Close_RunsAllImplsEvenAfterError(t *testing.T) {
	a := &fakeCBShim{closeErr: errors.New("eBPF detach failed")}
	b := &fakeCBShim{}

	c := &composite{impls: []CBShim{a, b}}
	err := c.Close()
	if err == nil {
		t.Errorf("Close err = nil, want error from a")
	}
	if a.closeCalled != 1 {
		t.Errorf("a.Close not invoked")
	}
	if b.closeCalled != 1 {
		t.Errorf("b.Close not invoked — first impl's error must not short-circuit subsequent closes")
	}
}

// AttachToProcessTree fans out and surfaces the FIRST error;
// subsequent errors are intentionally dropped to keep the log signal
// readable (they typically share a root cause — missing CAP_BPF,
// PID 1 outside our namespace, etc.).
func TestComposite_AttachToProcessTree_ReturnsFirstError(t *testing.T) {
	firstErr := errors.New("first impl failed")
	secondErr := errors.New("second impl failed")
	a := &fakeCBShim{attachErr: firstErr}
	b := &fakeCBShim{attachErr: secondErr}

	c := &composite{impls: []CBShim{a, b}}
	err := c.AttachToProcessTree(1234)
	if err == nil {
		t.Fatalf("err = nil, want first-error")
	}
	if !errors.Is(err, firstErr) {
		t.Errorf("err = %v, want errors.Is %v", err, firstErr)
	}
	// Both must still have been invoked — fan-out is the whole point.
	if len(a.attachToProcTree) != 1 || len(b.attachToProcTree) != 1 {
		t.Errorf("AttachToProcessTree fan-out incomplete: a=%d b=%d, want 1 each",
			len(a.attachToProcTree), len(b.attachToProcTree))
	}
}
