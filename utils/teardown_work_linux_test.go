//go:build linux

package utils

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// wedgedStage begins a stage of teardown work on a goroutine of its own and
// wedges it: its thread asleep in a system call nothing ever completes, as a
// uprobe removal waits on a lock the kernel never releases. The stage ends
// when the test does.
func wedgedStage(t *testing.T) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil { // blocking: the read sleeps in the kernel
		t.Fatal(err)
	}
	begun, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		end := BeginTeardownWork()
		close(begun)
		var b [1]byte
		_, _ = syscall.Read(fds[0], b[:])
		end()
	}()
	<-begun
	t.Cleanup(func() {
		_, _ = syscall.Write(fds[1], []byte{0})
		<-ended
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
	})
}

// A stage of teardown work wedged in the middle is not reported busy for
// ever: once it has stopped making progress, the checks made during it count
// as checks without progress, and the CLI kills an agent whose teardown takes
// no step across its fixed count of them. Reported busy for as long as it was
// in progress, a stage the kernel never finished kept the agent from ever
// being killed.
func TestAWedgedStageOfTeardownWorkIsNotReportedBusy(t *testing.T) {
	defer func(b time.Duration) { teardownBeat = b }(teardownBeat)
	teardownBeat = 5 * time.Millisecond
	path := filepath.Join(t.TempDir(), "teardown.json")
	ctx, begin := context.WithCancel(context.Background())
	startReporter(t, ctx, pathSink(path), nil)
	begin()
	wedgedStage(t)

	awaitStallChecks(t, path)
}

// awaitStallChecks returns once the reports at path show checks the CLI
// counts as checks without progress -- beats whose report takes no step and
// is not busy -- several in a row, counted in the agent's beats, not by any
// clock; it fails the test if hundreds of beats pass without.
func awaitStallChecks(t *testing.T, path string) {
	t.Helper()
	const stalls, within = 10, 400
	first := awaitReport(t, path, func(TeardownProgress) bool { return true })
	var quiet *TeardownProgress
	for {
		p := awaitReport(t, path, func(p TeardownProgress) bool { return p.Beat > first.Beat })
		first = p
		switch {
		case p.Busy:
			quiet = nil
		case quiet == nil || p.Seq != quiet.Seq:
			quiet = &p
		case p.Beat-quiet.Beat >= stalls:
			return // checks without progress, as a wedged stage's must be
		}
		if p.Beat >= within {
			t.Fatalf("after %d beats a wedged stage of teardown work is still reported as progressing (last report %+v): the CLI never counts a check made during it, and never kills the agent", p.Beat, p)
		}
	}
}

// A process the stage's thread had already started before the stage began --
// for another goroutine that ran on it -- is not the stage's: its running is
// no progress of the stage, and a wedged stage is still seen as one.
func TestAStageIsNotCreditedWithAnEarlierChildOfItsThread(t *testing.T) {
	if _, err := os.Stat("/proc/thread-self/children"); err != nil {
		t.Skipf("this kernel lists no thread's own children (%v): every child of the agent counts, by design", err)
	}
	defer func(b time.Duration) { teardownBeat = b }(teardownBeat)
	teardownBeat = 5 * time.Millisecond
	path := filepath.Join(t.TempDir(), "teardown.json")
	ctx, begin := context.WithCancel(context.Background())
	startReporter(t, ctx, pathSink(path), nil)
	begin()

	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	begun, ended := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(ended)
		runtime.LockOSThread() // the child is this thread's
		defer runtime.UnlockOSThread()
		child := busyChild() // runs on and on, while the test binary does
		if err := child.Start(); err != nil {
			begun <- err
			return
		}
		defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
		end := BeginTeardownWork()
		begun <- nil
		var b [1]byte
		_, _ = syscall.Read(fds[0], b[:]) // wedged
		end()
	}()
	if err := <-begun; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = syscall.Write(fds[1], []byte{0})
		<-ended
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
	})
	awaitStallChecks(t, path)
}

// A stage of teardown work that keeps making progress is reported busy, and
// each check that finds it so is a step: never a check without progress.
func TestAProgressingStageOfTeardownWorkIsReportedBusy(t *testing.T) {
	defer func(b time.Duration) { teardownBeat = b }(teardownBeat)
	teardownBeat = 5 * time.Millisecond
	path := filepath.Join(t.TempDir(), "teardown.json")
	ctx, begin := context.WithCancel(context.Background())
	startReporter(t, ctx, pathSink(path), nil)
	begin()
	stop, ended := make(chan struct{}), make(chan struct{})
	begun := make(chan error, 1)
	go func() {
		defer close(ended)
		end := BeginTeardownWork()
		defer end()
		// A CA tool at work, which the stage runs: always running, or
		// waiting to run, whatever this process's goroutines do -- with one
		// CPU for them, a goroutine of its own would be descheduled for whole
		// beats, its thread asleep.
		tool := busyChild()
		if err := tool.Start(); err != nil {
			begun <- err
			return
		}
		defer func() { _ = tool.Process.Kill(); _ = tool.Wait() }()
		begun <- nil
		<-stop
	}()
	if err := <-begun; err != nil {
		t.Fatal(err)
	}
	defer func() { close(stop); <-ended }()

	start := awaitReport(t, path, func(p TeardownProgress) bool { return p.Busy })
	// Across many checks, every report is busy, and the steps keep coming.
	last := start
	for last.Beat < start.Beat+50 {
		p := awaitReport(t, path, func(p TeardownProgress) bool { return p.Beat > last.Beat })
		if !p.Busy && p.Seq == last.Seq {
			t.Fatalf("a progressing stage of teardown work was reported as a check without progress: %+v after %+v", p, last)
		}
		last = p
	}
	if last.Seq <= start.Seq {
		t.Fatalf("no step across %d checks of a progressing stage (%+v, then %+v)", last.Beat-start.Beat, start, last)
	}
}

// TestEscalationWhileWedgedHelper is not a test of its own: with
// KEPLOY_TEST_ESCALATION_WEDGED set it is an agent whose stop has begun, in
// the middle of a stage of teardown work wedged for good.
func TestEscalationWhileWedgedHelper(t *testing.T) {
	dir := os.Getenv("KEPLOY_TEST_ESCALATION_WEDGED")
	if dir == "" {
		return
	}
	helperExitsWithParent()
	teardownBeat = 10 * time.Millisecond
	stopping, stop := context.WithCancel(context.Background())
	stop() // its stop has begun
	DieOnEscalation(stopping)
	wedgedStage(t)
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		os.Exit(3)
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}

// The escalation signal -- the CLI's kill of an agent it judged to have
// stopped making progress, and the only kill there is of an agent sudo runs
// as root -- does not wait for a stage of teardown work wedged in the middle:
// it kills once the stage has stopped making progress, as it kills between
// two stages. Waiting for its end, it waited for ever, and the root agent
// outlived the keploy that stopped it.
func TestTheEscalationDoesNotWaitForAWedgedStage(t *testing.T) {
	dir := t.TempDir()
	helper, exited, out := startHelper(t, "TestEscalationWhileWedgedHelper", "KEPLOY_TEST_ESCALATION_WEDGED="+dir)
	hung := time.After(time.Minute) // only turns a hang into a failure
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("the helper ended before it was ready: %v\n%s", err, out.String())
		case <-hung:
			t.Fatalf("the helper was never ready\n%s", out.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := helper.Process.Signal(EscalationSignal); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Minute): // the stage is checked every 10 ms: a minute is a hang
		t.Fatalf("the escalation never killed an agent whose teardown work is wedged\n%s", out.String())
	}
	if st := helper.ProcessState.Sys().(syscall.WaitStatus); !st.Signaled() || st.Signal() != syscall.SIGKILL {
		t.Fatalf("the helper ended with %v, want it killed (SIGKILL)\n%s", helper.ProcessState, out.String())
	}
}

// TestEscalationWhilePausingHelper is not a test of its own: with
// KEPLOY_TEST_ESCALATION_PAUSING set it is an agent whose stop has begun, in
// the middle of a stage of teardown work that keeps making progress in
// spurts. The stage is at work until the test writes "sent" (the escalation
// is pending); then it sleeps in the kernel for a third of wedgedAfter of its
// checks and is woken, pausingRounds times -- more checks than wedgedAfter in
// all, twice over. It writes "rounds" after the last pause, and ends the
// stage once the test writes "end".
func TestEscalationWhilePausingHelper(t *testing.T) {
	dir := os.Getenv("KEPLOY_TEST_ESCALATION_PAUSING")
	if dir == "" {
		return
	}
	helperExitsWithParent()
	teardownBeat = 10 * time.Millisecond
	stopping, stop := context.WithCancel(context.Background())
	stop() // its stop has begun
	DieOnEscalation(stopping)
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil { // blocking: the read sleeps in the kernel
		os.Exit(3)
	}
	// atWorkUntil returns once the test has written name, the stage's thread
	// waking every millisecond meanwhile: at work, as the kernel sees it.
	atWorkUntil := func(name string) {
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	const pausingRounds = 7
	begun := make(chan struct{})
	go func() {
		end := BeginTeardownWork()
		close(begun)
		// At work until the escalation is pending: a pause begun earlier
		// would be counted from whenever the test got round to sending it.
		atWorkUntil("sent")
		var b [1]byte
		for i := 0; i < pausingRounds; i++ {
			_, _ = syscall.Read(fds[0], b[:]) // a pause: asleep until woken
		}
		if err := os.WriteFile(filepath.Join(dir, "rounds"), nil, 0o600); err != nil {
			os.Exit(3)
		}
		atWorkUntil("end")
		end()
	}()
	<-begun
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		os.Exit(3)
	}
	// stageAt returns the stage's count of quiet checks, and the teardown's
	// step count at its last check, once done holds for them: waiting on the
	// work's own events (a check), not a clock.
	stageAt := func(done func(quiet, seq uint64) bool) (quiet, seq uint64) {
		for {
			teardownWorkMu.Lock()
			quiet, seq = 0, 0
			for w := range teardownWorks {
				quiet, seq = w.quiet, w.lastSeq
			}
			changed := workChanged
			teardownWorkMu.Unlock()
			if done(quiet, seq) {
				return quiet, seq
			}
			<-changed
		}
	}
	// A pause lasts wedgedAfter/3 quiet checks counted from the check that
	// saw the last wake, and a wake is waited for by that check's step:
	// neither is undone by a later check, however late this goroutine runs.
	// (Waited for by the count's 0, a wake could be missed -- the next check,
	// in the next pause, counting on before this goroutine ran -- and the
	// pause then never ended.)
	var woken uint64 // the quiet count when the last wake was seen
	for i := 0; i < pausingRounds; i++ {
		_, asleep := stageAt(func(q, _ uint64) bool { return q >= woken+wedgedAfter/3 })
		_, _ = syscall.Write(fds[1], []byte{0}) // woken: its next check finds it scheduled
		woken, _ = stageAt(func(_, seq uint64) bool { return seq != asleep })
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}

// The escalation waits for a stage of teardown work that keeps making
// progress, also one that sleeps in the kernel between its spurts -- a
// removal waiting on a lock the kernel does release: it is wedged only once
// it has been quiet across wedgedAfter checks in a row, not across that many
// checks in all, nor at its first quiet one. Killed in a pause, such a
// removal is cut short and leaks its breakpoint.
func TestTheEscalationWaitsForAStageThatPausesButProgresses(t *testing.T) {
	dir := t.TempDir()
	helper, exited, out := startHelper(t, "TestEscalationWhilePausingHelper", "KEPLOY_TEST_ESCALATION_PAUSING="+dir)
	awaitHelperFile(t, dir, "ready", exited, out)
	if err := helper.Process.Signal(EscalationSignal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sent"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Fails the test if the escalation killed it in a pause.
	awaitHelperFile(t, dir, "rounds", exited, out)
	if err := os.WriteFile(filepath.Join(dir, "end"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatalf("the escalation did not kill the agent once its stage ended\n%s", out.String())
	}
	if st := helper.ProcessState.Sys().(syscall.WaitStatus); !st.Signaled() || st.Signal() != syscall.SIGKILL {
		t.Fatalf("the helper ended with %v, want it killed (SIGKILL)\n%s", helper.ProcessState, out.String())
	}
}

// ProcessProgress tells a process that sleeps from one that runs: a cat
// blocked reading its stdin has made no progress between two looks, and has
// once it has been woken to copy a line; one that has exited has changed too.
func TestProcessProgressTellsASleepingProcessFromARunningOne(t *testing.T) {
	cmd := exec.Command("cat")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("no cat: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	out := bufio.NewReader(outPipe)
	// Up and asleep in its read: its first line copied, then nothing more.
	if _, err := in.Write([]byte("up\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := out.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	moved := ProcessProgress(cmd.Process.Pid)
	if !moved() {
		t.Fatal("the first look reported no progress")
	}
	// Once it sleeps in its read again (S, with nothing to wake it), a look
	// takes in what it did on its way there, and the next one sees nothing
	// new.
	stat := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/stat"
	for {
		b, err := os.ReadFile(stat)
		if err != nil {
			t.Fatal(err)
		}
		if st, _, _, ok := parseTaskStat(b); ok && st == 'S' {
			break
		}
	}
	moved()
	if moved() {
		t.Fatal("a process asleep in a read was reported making progress")
	}
	if _, err := in.Write([]byte("again\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := out.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if !moved() {
		t.Fatal("a process that was woken to copy a line was reported making no progress")
	}
}
