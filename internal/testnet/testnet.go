// Package testnet gives this module's tests the loopback ports they expect
// refused or free, held for the test's life. go test ./... tests
// packages side by side, and a port a test took from a listener it then
// closed is free at once: a socket of another test, in this binary or in a
// package tested beside it, can take the port and answer the dial the test
// expects refused, or hold the port the test means to listen at again.
//
// RefusedAddr's port is held for the test's life on every platform. On
// Linux it is held with a socket that never listens, at every address and
// in both families. macOS drops a SYN sent to a bound socket that is not
// listening, by XNU's source, instead of refusing it, so on macOS and
// Windows the port is held as the local end of a connection that is never
// accepted, at 127.0.0.1 (holdByConnection). SharedPort's port is held the
// same way on each platform, and the test's own listeners bind beside the
// hold: on Linux the hold and they set SO_REUSEPORT; on macOS Go's listeners
// set SO_REUSEADDR, which binds beside a connected socket; on Windows
// OpenShared's listeners set SO_REUSEADDR. CI runs these tests on Linux, and
// some packages' tests on Windows and macOS too.
//
// ReservedPort's port is for code under test that listens at it by number
// with net.Listen. On Linux it is held by a socket that sets SO_REUSEADDR
// and only binds, and Go's listeners, which set it too, bind beside it; on
// macOS it is SharedPort's. On Windows, where Go's listeners set no option
// and so bind beside no hold, it is a port below the dynamic port range,
// which no bind to port 0 and no connect is given, claimed by a file in
// os.TempDir so that no other test is given it (claimPort). UnlistedPort
// (Linux) is a port the socket tables list no socket at.
//
// BoundHere counts the sockets of this process at a port, so a test can tell
// a port its code still holds from one another process took once it was
// free; GiveAway hands a listener's socket to another process, for a test of
// that. Both work on Linux only.
//
// HoldFinalizers keeps the runtime's finalizers from running for the rest
// of a test, so a socket the code under test dropped stays open until the
// test looks. It works on every platform.
//
// The package is for tests only: nothing outside a _test.go file imports it.
package testnet

import (
	"runtime"
	"sync/atomic"
	"testing"
)

// finalizersHeld is true while a test holds the finalizers.
var finalizersHeld atomic.Bool

// finalizerParked is true while the hold's finalizer has the goroutine that
// runs finalizers.
var finalizerParked atomic.Bool

// HoldFinalizers keeps the runtime's finalizers from running for the rest of
// the test. net closes a listener or a connection in the socket's finalizer.
// The finalizer runs once a collection finds that nothing references the
// socket. So a socket the code under test left open could be closed before
// the test looks. The test's check that the code closed it would then pass.
// A collection can start at an allocation. The code under test can start one
// itself (runtime.GC). Turning the collector off does not stop runtime.GC.
// Nor does it stop a collection a memory limit (GOMEMLIMIT) starts.
//
// The runtime runs every finalizer on one goroutine, one after another. The
// hold parks that goroutine in a finalizer of its own until the test ends. No
// collection, whoever starts it, can close a dropped socket meanwhile.
//
// It holds finalizers only: a function passed to runtime.AddCleanup still
// runs. If net closed sockets that way, the hold would not keep them open. The
// finalizers are held for one test at a time: a second call, from this test
// or another, fails the test.
func HoldFinalizers(t *testing.T) {
	t.Helper()
	holdFinalizers(t, runtime.GC)
}

// holdFinalizers is HoldFinalizers that starts each collection it waits
// through with collect.
func holdFinalizers(t *testing.T, collect func()) {
	t.Helper()
	if !finalizersHeld.CompareAndSwap(false, true) {
		t.Fatal("HoldFinalizers: the finalizers are held already, by this test or another")
	}
	started, release := make(chan struct{}), make(chan struct{})
	// 64 bytes, so the tiny allocator does not put the object in a block
	// with others that keep it reachable.
	runtime.SetFinalizer(new([64]byte), func(*[64]byte) {
		finalizerParked.Store(true)
		close(started)
		<-release
		finalizerParked.Store(false)
	})
	t.Cleanup(func() {
		close(release)
		finalizersHeld.Store(false)
	})
	for {
		collect()
		select {
		case <-started:
			return
		default:
			runtime.Gosched()
		}
	}
}
