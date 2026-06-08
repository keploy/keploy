package tls

import (
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestMITMPublishHook_FiresOnceWithCorrectArgs verifies that after
// the hook is installed, CertForClient invokes it exactly once with
// the freshly-minted leaf DER and a connID derived from the source
// port of hello.Conn. Pinned regression coverage for the cbshim
// rendezvous wiring — without this, a refactor that drops the
// publishMITM call inside CertForClient would compile cleanly and
// pass every other tls test, but silently break SCRAM-PLUS through
// the MITM.
func TestMITMPublishHook_FiresOnceWithCorrectArgs(t *testing.T) {
	resetCertCacheForTest()
	resetHookForTest(t)
	logger := zap.NewNop()
	caKey, caCert := helperCA(t)

	type capture struct {
		connID string
		der    []byte
	}
	var captured []capture
	var mu sync.Mutex
	SetMITMPublishHook(func(connID string, mitmDER []byte) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, capture{
			connID: connID,
			der:    append([]byte(nil), mitmDER...),
		})
	})

	hello, cleanup, err := helperClientHello("api.wise-sandbox.example")
	if err != nil {
		t.Fatalf("helperClientHello: %v", err)
	}
	defer cleanup()
	// Source port comes from the kernel-assigned RemoteAddr of the
	// accepted loopback connection — observe it now to assert against.
	tcpAddr, ok := hello.Conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("hello.Conn.RemoteAddr is %T, want *net.TCPAddr", hello.Conn.RemoteAddr())
	}
	wantConnID := strconv.Itoa(tcpAddr.Port)

	cert, err := CertForClient(logger, hello, caKey, caCert, time.Time{})
	if err != nil {
		t.Fatalf("CertForClient: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatalf("CertForClient returned empty cert")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("MITMPublishHook fired %d times, want 1", len(captured))
	}
	if got := captured[0].connID; got != wantConnID {
		t.Errorf("connID = %q, want %q", got, wantConnID)
	}
	if string(captured[0].der) != string(cert.Certificate[0]) {
		t.Errorf("published DER differs from CertForClient's leaf DER (len got %d, want %d)",
			len(captured[0].der), len(cert.Certificate[0]))
	}
}

// TestMITMPublishHook_NoCallWhenHookNil verifies that with no hook
// installed (the OSS default + the post-shutdown state),
// CertForClient still works — no nil-deref, valid cert returned.
// Pinned regression coverage for the publishMITM nil-snapshot
// guard.
func TestMITMPublishHook_NoCallWhenHookNil(t *testing.T) {
	resetCertCacheForTest()
	resetHookForTest(t)
	logger := zap.NewNop()
	caKey, caCert := helperCA(t)

	hello, cleanup, err := helperClientHello("api.no-hook.example")
	if err != nil {
		t.Fatalf("helperClientHello: %v", err)
	}
	defer cleanup()

	cert, err := CertForClient(logger, hello, caKey, caCert, time.Time{})
	if err != nil {
		t.Fatalf("CertForClient: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatalf("CertForClient returned empty cert")
	}
}

// TestMITMPublishHook_RaceWithToggling stresses the publishMITM
// nil-snapshot pattern: one goroutine churns the hook on/off as
// fast as it can while many goroutines mint certs through
// CertForClient. The local snapshot inside publishMITM must
// prevent any nil-deref panic and must keep -race happy.
//
// Catches regressions of the form:
//
//	if MITMPublishHook != nil { MITMPublishHook(...) }
//
// which compiles + passes single-threaded tests but races under
// concurrent SetCBShim(nil) during shutdown. Without the snapshot,
// a hook clear between the nil-check and the call panics.
func TestMITMPublishHook_RaceWithToggling(t *testing.T) {
	resetCertCacheForTest()
	resetHookForTest(t)
	logger := zap.NewNop()
	caKey, caCert := helperCA(t)

	stop := make(chan struct{})
	var toggler sync.WaitGroup
	toggler.Add(1)
	go func() {
		defer toggler.Done()
		on := func(_ string, _ []byte) {}
		for {
			select {
			case <-stop:
				return
			default:
			}
			SetMITMPublishHook(on)
			SetMITMPublishHook(nil)
			// Yield to the scheduler so the toggler doesn't starve
			// the worker goroutines doing expensive cert mints.
			// Without this, on a low-CPU CI runner the toggler can
			// pin a core and bloat the test wall-clock.
			runtime.Gosched()
		}
	}()

	const workers = 8
	const itersPerWorker = 25
	var wg sync.WaitGroup
	var fatal atomic.Bool
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < itersPerWorker; i++ {
				if fatal.Load() {
					return
				}
				host := "api.toggle-" + strconv.Itoa(w) + "-" + strconv.Itoa(i) + ".example"
				hello, cleanup, err := helperClientHello(host)
				if err != nil {
					t.Errorf("helperClientHello: %v", err)
					fatal.Store(true)
					return
				}
				cert, err := CertForClient(logger, hello, caKey, caCert, time.Time{})
				cleanup()
				if err != nil {
					t.Errorf("CertForClient: %v", err)
					fatal.Store(true)
					return
				}
				if cert == nil {
					t.Errorf("nil cert")
					fatal.Store(true)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	toggler.Wait()
}

// resetHookForTest snapshots and clears the MITM publish hook for
// the duration of the test, restoring the original on cleanup.
// Tests MUST use this rather than touching the hook directly so
// they don't bleed state into each other (the storage is
// package-level).
func resetHookForTest(t *testing.T) {
	t.Helper()
	hookMu.RLock()
	prev := mitmPublishHook
	hookMu.RUnlock()
	SetMITMPublishHook(nil)
	t.Cleanup(func() { SetMITMPublishHook(prev) })
}
