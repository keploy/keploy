package testnet

import (
	"net"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// droppedListener opens a listener at 127.0.0.1 and returns its port. It
// keeps no reference to the listener, as code that leaks one does.
func droppedListener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	return ln.Addr().(*net.TCPAddr).Port
}

// While a test holds the finalizers, the hold's finalizer has the goroutine
// that runs them. A listener nothing references then stays open through
// collections, ones the test starts included.
func TestHoldFinalizersKeepsADroppedListenerOpenThroughACollection(t *testing.T) {
	HoldFinalizers(t)
	require.True(t, finalizerParked.Load(), "HoldFinalizers returned before its finalizer parked")
	if _, known := BoundHere(t, 0); !known {
		t.Skip("BoundHere cannot tell here whose socket is at a port")
	}
	port := droppedListener(t)
	for range 3 {
		runtime.GC()
		runtime.Gosched()
	}
	n, _ := BoundHere(t, port)
	require.Equal(t, 1, n, "sockets of this process at the dropped listener's port %d after collections: 1 is the listener, still open", port)
}

// The hold returns only once its finalizer has the goroutine that runs
// finalizers, however many collections that takes. A finalizer the test sets
// first has that goroutine until the hold's eighth collection. A hold that
// stops after fewer collections returns before its finalizer parks.
func TestHoldFinalizersWaitsThroughCollectionsUntilItsFinalizerParks(t *testing.T) {
	gateStarted, gateOpen := make(chan struct{}), make(chan struct{})
	var open sync.Once
	t.Cleanup(func() { open.Do(func() { close(gateOpen) }) })
	runtime.SetFinalizer(new([64]byte), func(*[64]byte) {
		close(gateStarted)
		<-gateOpen
	})
	for parked := false; !parked; {
		runtime.GC()
		select {
		case <-gateStarted:
			parked = true
		default:
			runtime.Gosched()
		}
	}
	collections := 0
	holdFinalizers(t, func() {
		runtime.GC()
		collections++
		if collections == 8 {
			open.Do(func() { close(gateOpen) })
		}
	})
	require.True(t, finalizerParked.Load(), "the hold returned before its finalizer parked")
}
