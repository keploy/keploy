//go:build linux

package testproc

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// LifelineEnv names, in the environment of a test process that ran
// SetLifeline, the path of its lifeline; every process it starts, and each
// of those starts in turn, inherits it.
const LifelineEnv = "KEPLOY_TEST_LIFELINE"

// UntilTheTestEnds is a shell command that runs until the test process that
// set the lifeline (SetLifeline) ends, however it ends, and no longer: cat
// reading the lifeline, which reads end-of-file only then. It stands where a
// test needs a process that runs until it is killed: a long sleep there
// outlives a test process killed with no cleanup run (a timeout's panic, a
// kill as its job ends), out of reach of a kill of its group once it is in
// a group or session of its own. `exec` makes a shell this process, `&` a
// child of it; signals it ignores stay ignored, as they would for a sleep.
// With no lifeline set, or the test process already gone, the redirection
// fails and the command ends at once: one that waits on its process fails
// at once too, rather than waiting for ever.
const UntilTheTestEnds = `cat <"$` + LifelineEnv + `"`

var lifeline struct {
	once sync.Once
	err  error
}

// SetLifeline opens this process's lifeline, once, and names its path in
// this process's environment (LifelineEnv). The lifeline is a pipe whose
// only write end this process holds, close-on-exec, and never writes: a
// process that opens it for reading (through /proc, by that path) and reads
// it waits until this process ends, by any signal too, as the kernel then
// closes the write end, and reads end-of-file. A process does not hold the
// lifeline open unless it opens it, so the descriptors of one that does not
// are as they would be with no lifeline at all.
func SetLifeline() error {
	lifeline.once.Do(func() {
		var fds [2]int
		if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC); err != nil {
			lifeline.err = fmt.Errorf("open the test's lifeline: %w", err)
			return
		}
		// Neither end is ever closed: the read end's path is what a
		// process opens, and the write end's close is this process's end.
		path := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), fds[0])
		if err := os.Setenv(LifelineEnv, path); err != nil {
			lifeline.err = fmt.Errorf("name the test's lifeline: %w", err)
		}
	})
	return lifeline.err
}
