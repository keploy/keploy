package util

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitForSurvivor_RealTimerDrainsAcrossIdleWindowsThenStalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		finished := make(chan struct{})
		var delivered atomic.Int64

		done := make(chan struct{})
		start := time.Now()
		go func() {
			WaitForSurvivor(finished, delivered.Load, RelayDrainIdle)
			close(done)
		}()
		defer func() {
			close(finished)
			<-done
		}()

		// Deliver bytes across three full production idle windows (90s total),
		// each arriving before its window expires.
		step := RelayDrainIdle * 2 / 3
		rem := RelayDrainIdle - step
		for w := 0; w < 3; w++ {
			time.Sleep(step)
			synctest.Wait()
			select {
			case <-done:
				t.Fatalf("WaitForSurvivor returned early before tick %d after %v",
					w+1, time.Since(start))
			default:
			}
			delivered.Add(16 * 1024)
			time.Sleep(rem)
			synctest.Wait()
			select {
			case <-done:
				t.Fatalf("WaitForSurvivor returned early at window %d after %v while bytes were still moving",
					w+1, time.Since(start))
			default:
			}
		}

		// Go silent: WaitForSurvivor must stop once one full idle window passes
		// with delivered unchanged.
		silenceStart := time.Now()
		select {
		case <-done:
		case <-time.After(2 * RelayDrainIdle):
			t.Fatalf("WaitForSurvivor did not stall within %v of silence", 2*RelayDrainIdle)
		}
		if wait := time.Since(silenceStart); wait != RelayDrainIdle {
			t.Fatalf("WaitForSurvivor stalled after %v of silence, want %v", wait, RelayDrainIdle)
		}
		if elapsed := time.Since(start); elapsed != 4*RelayDrainIdle {
			t.Fatalf("WaitForSurvivor total elapsed = %v, want %v", elapsed, 4*RelayDrainIdle)
		}
	})
}

func TestWaitForSurvivor_RealTimerReturnsImmediatelyOnFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		finished := make(chan struct{}, 1)
		var delivered atomic.Int64

		done := make(chan struct{})
		start := time.Now()
		go func() {
			WaitForSurvivor(finished, delivered.Load, RelayDrainIdle)
			close(done)
		}()
		defer func() {
			close(finished)
			<-done
		}()

		time.Sleep(5 * time.Second)
		finished <- struct{}{}
		select {
		case <-done:
		case <-time.After(RelayDrainIdle):
			t.Fatal("WaitForSurvivor did not return after finished signaled")
		}

		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Fatalf("WaitForSurvivor returned after %v, want 5s (must not wait out %v once finished signals)",
				elapsed, RelayDrainIdle)
		}
	})
}

func TestWaitForSurvivor_ResetsFullIdleWindowAfterProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		finished := make(chan struct{})
		var delivered atomic.Int64

		done := make(chan struct{})
		start := time.Now()
		go func() {
			WaitForSurvivor(finished, delivered.Load, RelayDrainIdle)
			close(done)
		}()
		defer func() {
			close(finished)
			<-done
		}()

		// Deliver once before the first tick so the timer resets, then stay
		// silent past half of the second window: a reset shorter than idle
		// (e.g. idle/2) would stall prematurely here.
		time.Sleep(RelayDrainIdle / 3)
		delivered.Add(1024)
		time.Sleep(RelayDrainIdle)
		synctest.Wait()
		select {
		case <-done:
			t.Fatalf("WaitForSurvivor stalled early at %v; Reset must grant a full %v window",
				time.Since(start), RelayDrainIdle)
		default:
		}

		select {
		case <-done:
		case <-time.After(2 * RelayDrainIdle):
			t.Fatal("WaitForSurvivor never stalled after silence")
		}
		if elapsed := time.Since(start); elapsed != 2*RelayDrainIdle {
			t.Fatalf("WaitForSurvivor returned after %v, want %v", elapsed, 2*RelayDrainIdle)
		}
	})
}
