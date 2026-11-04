//go:build !linux

package stall

import (
	"testing"
	"time"
)

// Off Linux the sampling sleeps on a Go timer beside its stop (sleepFor), so
// the last release ends the sleep at once and returns when the sampling has
// ended, not at its next look an hour away; and so does the next sampling's.
func TestSampler_TheLastReleaseEndsItsSleep(t *testing.T) {
	s := &sampler{src: &fakeSource{}, tick: time.Hour}
	for run := range 2 {
		release := s.acquire()
		released := make(chan struct{})
		go func() { release(); close(released) }()
		select {
		case <-released:
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("sampling %d: the release of a sampler that looks once an hour has not returned in a minute", run)
		}
		s.mu.Lock()
		users, running := s.users, s.stop != nil
		s.mu.Unlock()
		if users != 0 || running {
			t.Fatalf("sampling %d: after its last release, %d users, sampling %v; want none, stopped", run, users, running)
		}
	}
}
