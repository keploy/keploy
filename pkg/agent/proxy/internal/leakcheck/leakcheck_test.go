package leakcheck

import (
	"context"
	"runtime"
	"testing"
)

// A goroutine is counted by the wait it is parked in; one on its way out
// (runnable, running, in a syscall) or briefly in a lock is not.
func TestParkedReadsTheWaitInTheHeader(t *testing.T) {
	for header, want := range map[string]bool{
		"goroutine 7 [chan receive]:":                       true,
		"goroutine 7 [chan receive, 2 minutes]:":            true,
		"goroutine 7 [chan receive (nil chan)]:":            true,
		"goroutine 7 [chan send]:":                          true,
		"goroutine 7 [select]:":                             true,
		"goroutine 7 [select, locked to thread]:":           true,
		"goroutine 7 [IO wait, 5 minutes]:":                 true,
		"goroutine 7 [sync.WaitGroup.Wait]:":                true,
		"goroutine 7 [sync.Cond.Wait]:":                     true,
		"goroutine 7 gp=0x4000102380 m=nil [chan send]:":    true,
		"goroutine 7 [runnable]:":                           false,
		"goroutine 7 [running]:":                            false,
		"goroutine 7 [syscall]:":                            false,
		"goroutine 7 [semacquire]:":                         false,
		"goroutine 7 [sync.Mutex.Lock]:":                    false,
		"goroutine 7 [GC assist wait]:":                     false,
		"goroutine 7 gp=0x4000102380 m=3 mp=0x1 [running]:": false,
	} {
		if got := parked(header + "\nmain.f()\n"); got != want {
			t.Errorf("parked(%q) = %v, want %v", header, got, want)
		}
	}
}

//go:noinline
func parkOnAChannel(ch chan struct{}, done chan struct{}) {
	defer close(done)
	<-ch
}

func TestParkedCountsAGoroutineWaitingOnAChannel(t *testing.T) {
	ch, done := make(chan struct{}), make(chan struct{})
	go parkOnAChannel(ch, done)
	// It parks once it reaches the receive; go test's -timeout guards a hang.
	for n, _ := Parked("leakcheck.parkOnAChannel"); n != 1; n, _ = Parked("leakcheck.parkOnAChannel") {
		runtime.Gosched()
	}
	close(ch)
	<-done
	if n, _ := Parked("leakcheck.nothingByThisName"); n != 0 {
		t.Fatalf("Parked counted %d goroutines of a function nothing runs", n)
	}
}

// What context.AfterFunc and context.WithCancel register is counted until
// it is stopped, and Cancel runs what is left.
func TestContextCountsRegistrationsUntilTheyStop(t *testing.T) {
	ctx := NewContext(context.Background())
	stop := context.AfterFunc(ctx, func() {})
	child, cancelChild := context.WithCancel(ctx)
	if got := ctx.Live(); got != 2 {
		t.Fatalf("Live() = %d after an AfterFunc and a WithCancel, want 2", got)
	}
	if !stop() {
		t.Fatal("stopping the AfterFunc reported it had already run")
	}
	cancelChild()
	if got := ctx.Live(); got != 0 {
		t.Fatalf("Live() = %d after both stopped, want 0", got)
	}
	if child.Err() == nil {
		t.Fatal("the child is not cancelled after its cancel")
	}

	ran := make(chan struct{})
	context.AfterFunc(ctx, func() { close(ran) })
	ctx.Cancel()
	<-ran
	if ctx.Err() == nil || ctx.Live() != 0 {
		t.Fatalf("after Cancel: Err() = %v, Live() = %d", ctx.Err(), ctx.Live())
	}
}
