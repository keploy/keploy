//go:build linux

// Package ownthread runs a function on an OS thread of its own, which the Go
// runtime ends once the function returns: for work that leaves the thread in
// a state no other goroutine may run in, such as another mount namespace.
package ownthread

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// Run runs f, from a new goroutine, locked to a thread that is not the
// process's main thread, and never unlocked: the runtime ends a thread whose
// goroutine exits locked, and starts no thread from a locked one, so nothing
// else ever runs on it. Not the main thread: the runtime parks that one for
// good instead of ending it, and it is the thread /proc/self reads. A
// goroutine that finds itself on the main thread holds it while another
// locks a thread, which then cannot be the main one, and lets it go once that
// one is locked. Run returns at once; f runs on.
func Run(f func()) {
	go func() {
		runtime.LockOSThread()
		if unix.Gettid() != unix.Getpid() {
			f()
			return
		}
		locked := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			close(locked)
			f()
		}()
		<-locked
		runtime.UnlockOSThread()
	}()
}
