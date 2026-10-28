//go:build linux || darwin

package utils

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// plantedConsoleValue is what the test cutter cuts; never a real secret.
const plantedConsoleValue = "planted-console-s3cr3t"

// consoleCutter is a log redactor whose child-output cut takes out
// plantedConsoleValue.
type consoleCutter struct{}

func (consoleCutter) RedactEntry(*zapcore.Entry)       {}
func (consoleCutter) RedactField(*zapcore.Field)       {}
func (consoleCutter) RedactEncoded(text string) string { return text }
func (consoleCutter) CutsChildOutput() bool            { return true }
func (consoleCutter) RedactChildOutput(line string) string {
	return strings.ReplaceAll(line, plantedConsoleValue, "***")
}

// HoldChildOutput holds the end of text that may start plantedConsoleValue.
func (consoleCutter) HoldChildOutput(text string) int {
	for k := len(plantedConsoleValue) - 1; k > 0; k-- {
		if strings.HasSuffix(text, plantedConsoleValue[:k]) {
			return k
		}
	}
	return 0
}

func cutChildOutput(t *testing.T) {
	t.Helper()
	keployLog.SetRedactor(consoleCutter{})
	t.Cleanup(func() { keployLog.SetRedactor(nil) })
}

// console is what a pipe standing in for os.Stdout or os.Stderr received.
type console struct {
	mu   sync.Mutex
	got  bytes.Buffer
	more chan struct{}
	done chan struct{}
}

func collect(r *os.File) *console {
	c := &console{more: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			c.mu.Lock()
			c.got.Write(buf[:n])
			c.mu.Unlock()
			select {
			case c.more <- struct{}{}:
			default:
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *console) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got.String()
}

// waitFor waits until c received s, or its pipe ended without it.
func (c *console) waitFor(t *testing.T, s string) {
	t.Helper()
	for !strings.Contains(c.String(), s) {
		select {
		case <-c.more:
		case <-c.done:
			if !strings.Contains(c.String(), s) {
				t.Fatalf("the console ended without %q: %q", s, c.String())
			}
		}
	}
}

// captureConsole points os.Stdout and os.Stderr at pipes for the test, and
// returns them and a func that restores both and returns what each received.
func captureConsole(t *testing.T) (outW, errW *os.File, read func() (stdout, stderr string)) {
	t.Helper()
	outW, errW, out, errOut := captureConsoles(t)
	return outW, errW, func() (string, string) {
		_ = outW.Close()
		_ = errW.Close()
		<-out.done
		<-errOut.done
		return out.String(), errOut.String()
	}
}

func captureConsoles(t *testing.T) (outW, errW *os.File, out, errOut *console) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	t.Cleanup(func() { os.Stdout, os.Stderr = oldOut, oldErr })
	return outW, errW, collect(outR), collect(errR)
}

func noCancel(_ *exec.Cmd) func() error { return func() error { return nil } }

func TestChildConsoleIsKeploysOwnFileWhenNothingIsCut(t *testing.T) {
	keployLog.SetRedactor(nil)
	cmd := exec.Command("true")
	c, err := SetChildConsole(cmd)
	if err != nil {
		t.Fatal(err)
	}
	c.Started()
	c.Exited()
	if cmd.Stdout != io.Writer(os.Stdout) || cmd.Stderr != io.Writer(os.Stderr) {
		t.Fatalf("the child gets %T/%T, want keploy's own os.Stdout and os.Stderr", cmd.Stdout, cmd.Stderr)
	}
	cutChildOutput(t)
	cmd = exec.Command("true")
	c, err = SetChildConsole(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Exited()
	defer c.Started()
	for _, w := range []io.Writer{cmd.Stdout, cmd.Stderr} {
		f, ok := w.(*os.File)
		if !ok || f == os.Stdout || f == os.Stderr {
			t.Fatalf("the child gets %T while the redactor cuts child output, want a pipe as an *os.File (so Wait waits on the process alone)", w)
		}
	}
}

func TestExecuteCommandCutsWhatTheChildPrints(t *testing.T) {
	cutChildOutput(t)
	_, _, read := captureConsole(t)
	// The stderr line is left unended: it reaches the console at the flush
	// after Wait.
	script := `printf 'DB_PASSWORD=%s\n' '` + plantedConsoleValue + `'; printf 'last %s' '` + plantedConsoleValue + `' >&2`
	cmdErr := ExecuteCommand(context.Background(), zap.NewNop(), script, Native, noCancel, time.Second, nil, nil)
	stdout, stderr := read()
	if cmdErr.Err != nil {
		t.Fatalf("ExecuteCommand: %v", cmdErr.Err)
	}
	if want := "DB_PASSWORD=***\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	if want := "last ***"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
}

// pipeName is the "pipe:[inode]" /proc names f's pipe by.
func pipeName(t *testing.T, f *os.File) string {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	return "pipe:[" + strconv.FormatUint(uint64(st.Ino), 10) + "]"
}

func TestExecuteCommandHandsTheChildKeploysOwnStdoutWhenNothingIsCut(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd/1"); err != nil {
		t.Skip("no /proc to read the child's stdout from")
	}
	for _, tc := range []struct {
		name     string
		cut      bool
		inherits bool
	}{
		{"nothing to cut", false, true},
		{"values to cut", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keployLog.SetRedactor(nil)
			if tc.cut {
				cutChildOutput(t)
			}
			outW, _, read := captureConsole(t)
			want := pipeName(t, outW)
			cmdErr := ExecuteCommand(context.Background(), zap.NewNop(), `readlink /proc/self/fd/1`, Native, noCancel, time.Second, nil, nil)
			stdout, _ := read()
			if cmdErr.Err != nil {
				t.Fatalf("ExecuteCommand: %v", cmdErr.Err)
			}
			if got := strings.TrimSpace(stdout); (got == want) != tc.inherits {
				t.Fatalf("the child's stdout is %q, keploy's is %q; want the child to inherit it: %v", got, want, tc.inherits)
			}
		})
	}
}

func TestExecuteCommandsTerminalCopyIsCut(t *testing.T) {
	cutChildOutput(t)
	_, _, read := captureConsole(t)
	cmd := exec.Command("sh", "-c", `printf 'token=%s\n' '`+plantedConsoleValue+`'; printf 'tail %s' '`+plantedConsoleValue+`'`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmdErr := executeWithPTY(context.Background(), zap.NewNop(), cmd)
	stdout, _ := read()
	if cmdErr.Type == Init {
		t.Skipf("no terminal to run the command in: %v", cmdErr.Err)
	}
	if cmdErr.Err != nil {
		t.Fatalf("executeWithPTY: %v", cmdErr.Err)
	}
	if strings.Contains(stdout, plantedConsoleValue) {
		t.Fatalf("the terminal copy carries the value: %q", stdout)
	}
	// The terminal ends the first line with "\r\n"; the unended tail
	// reaches the console before the master side is closed, though the
	// command had exited before the kernel moved it there.
	if want := "token=***\r\ntail ***"; stdout != want {
		t.Fatalf("the terminal copy is %q, want %q", stdout, want)
	}
}

// TestExecuteCommandDoesNotWaitOnAProcessTheChildLeftRunning: a script that
// starts a server in the background (`./mock-server &`) and exits has ended
// for keploy, though the server holds its output; the script's lines were
// written out by then, its unended last one too (Exited), and the server's
// lines still reach the console cut.
func TestExecuteCommandDoesNotWaitOnAProcessTheChildLeftRunning(t *testing.T) {
	cutChildOutput(t)
	fifo := filepath.Join(t.TempDir(), "release")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Held open for read and write, the fifo never blocks an open, and a
	// line written to it waits there for the background process. Whatever
	// happens, it is let go: the line is written, and once the test lets go
	// of the fifo too a read there ends.
	gate, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_, _ = gate.WriteString("go\n")
		}
	}
	t.Cleanup(func() {
		release()
		_ = gate.Close()
	})
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outW.Close(); _ = outR.Close() })
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = errW.Close(); _ = errR.Close() })
	_ = collect(errR)
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	t.Cleanup(func() { os.Stdout, os.Stderr = oldOut, oldErr })

	// The background process opens the fifo before the script goes on, so
	// it holds it when the line comes. The script's last line is left
	// unended on what may start the value, so the writer holds that part:
	// only Exited's drain writes it out, as the pipe cannot end while the
	// background process holds it.
	heldTail := "held " + plantedConsoleValue[:8]
	script := `exec 3<'` + fifo + `'; (read x <&3; printf '\nbg %s\n' '` + plantedConsoleValue + `') & exec 3<&-; printf 'fg %s\n%s' '` + plantedConsoleValue + `' '` + heldTail + `'`
	// Were Wait waiting on the pipe the background process holds, it would
	// return ErrWaitDelay once the delay is up. Waiting on the process alone,
	// it never starts the delay: no time is measured here.
	cmdErr := ExecuteCommand(context.Background(), zap.NewNop(), script, Native, noCancel, time.Second, nil, nil)
	if cmdErr.Err != nil {
		t.Fatalf("ExecuteCommand: %v", cmdErr.Err)
	}
	// A mark written now comes after all that was written before: the pipe
	// keeps order, and the background process writes nothing until let go.
	const mark = "--returned--\n"
	if _, err := outW.WriteString(mark); err != nil {
		t.Fatal(err)
	}
	out := collect(outR)
	out.waitFor(t, mark)
	if early, _, _ := strings.Cut(out.String(), mark); early != "fg ***\n"+heldTail {
		t.Fatalf("keploy's stdout held %q when ExecuteCommand returned, want the script's lines cut, its unended last one whole", early)
	}
	release()
	out.waitFor(t, "bg ***\n")
	if strings.Contains(out.String(), plantedConsoleValue) {
		t.Fatalf("the console carries the value: %q", out.String())
	}
}

func TestChildOutputCopyDrainsWithoutWaitingForTheEnd(t *testing.T) {
	cutChildOutput(t)
	for _, keep := range []bool{true, false} {
		t.Run(strconv.FormatBool(keep), func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			var dst syncBuffer
			cp := StartChildOutputCopy(&dst, r, keep)
			if _, err := w.WriteString("a " + plantedConsoleValue + "\ntail " + plantedConsoleValue); err != nil {
				t.Fatal(err)
			}
			// w is still open: Drain writes out what the pipe holds, the
			// unended tail too, and returns.
			cp.Drain()
			if got, want := dst.String(), "a ***\ntail ***"; got != want {
				t.Fatalf("after Drain the console has %q, want %q", got, want)
			}
			if !keep {
				<-cp.Done()
				return
			}
			select {
			case <-cp.Done():
				t.Fatal("a copy that keeps on stopped at Drain")
			default:
			}
			if _, err := w.WriteString("\nmore " + plantedConsoleValue + "\n"); err != nil {
				t.Fatal(err)
			}
			_ = w.Close()
			<-cp.Done()
			if got, want := dst.String(), "a ***\ntail ***\nmore ***\n"; got != want {
				t.Fatalf("at the pipe's end the console has %q, want %q", got, want)
			}
		})
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestChildConsoleCopiesEndWithTheChild: once keploy's copy of the write ends
// is closed (Started), each copy ends when the child closes its own.
func TestChildConsoleCopiesEndWithTheChild(t *testing.T) {
	cutChildOutput(t)
	_, _, read := captureConsole(t)
	cmd := exec.Command("sh", "-c", `printf 'x %s\n' '`+plantedConsoleValue+`'`)
	c, err := SetChildConsole(cmd)
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Start()
	c.Started()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	c.Exited()
	if len(c.copies) != 2 {
		t.Fatalf("%d copies, want one each for stdout and stderr", len(c.copies))
	}
	for _, cp := range c.copies {
		<-cp.Done()
	}
	if stdout, _ := read(); stdout != "x ***\n" {
		t.Fatalf("stdout %q", stdout)
	}
}

// gatedConsole holds the copy in its first write until released, so a test
// knows what the copy has not read. With refill set, each write once
// drainReq is closed puts a line back into the pipe, as a process the child
// left running that writes on would.
type gatedConsole struct {
	mu       sync.Mutex
	b        bytes.Buffer
	entered  chan struct{}
	release  chan struct{}
	first    bool
	refill   *os.File
	drainReq <-chan struct{}
	refills  int
}

const maxRefills = 1000

func newGatedConsole() *gatedConsole {
	return &gatedConsole{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedConsole) Write(p []byte) (int, error) {
	g.mu.Lock()
	if !g.first {
		g.first = true
		g.mu.Unlock()
		close(g.entered)
		<-g.release
		g.mu.Lock()
	}
	defer g.mu.Unlock()
	g.b.Write(p)
	if g.refill != nil && g.refills < maxRefills {
		select {
		case <-g.drainReq:
			g.refills++
			_, _ = g.refill.WriteString("more\n")
		default:
		}
	}
	return len(p), nil
}

func (g *gatedConsole) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.b.String()
}

// drainWhileHeld writes first and, while the copy is held writing it out,
// rest, then drains: the copy is let go only once Drain has set its
// deadline, so rest is in the pipe, unread, when the drain begins.
func drainWhileHeld(t *testing.T, cp *ChildOutputCopy, g *gatedConsole, w *os.File, first, rest string) {
	t.Helper()
	if _, err := w.WriteString(first); err != nil {
		t.Fatal(err)
	}
	<-g.entered
	if _, err := w.WriteString(rest); err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		cp.Drain()
		close(drained)
	}()
	<-cp.drainReq
	close(g.release)
	<-drained
}

func TestChildOutputCopyDrainsWhatItHadNotRead(t *testing.T) {
	cutChildOutput(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	g := newGatedConsole()
	// Not kept on: the copy stops at the drain, so only the drain can have
	// written the rest.
	cp := StartChildOutputCopy(g, r, false)
	drainWhileHeld(t, cp, g, w, "a "+plantedConsoleValue+"\n", "b "+plantedConsoleValue+"\ntail "+plantedConsoleValue)
	<-cp.Done()
	if got, want := g.String(), "a ***\nb ***\ntail ***"; got != want {
		t.Fatalf("after Drain the console has %q, want %q", got, want)
	}
}

// TestChildOutputCopyDrainReadsNoMoreThanThePipeHeld: a process the child
// left running that writes on as fast as keploy's console takes it cannot
// keep Drain, and so ExecuteCommand, from returning.
func TestChildOutputCopyDrainReadsNoMoreThanThePipeHeld(t *testing.T) {
	cutChildOutput(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	g := newGatedConsole()
	cp := StartChildOutputCopy(g, r, false)
	g.mu.Lock()
	g.refill, g.drainReq = w, cp.drainReq
	g.mu.Unlock()
	drainWhileHeld(t, cp, g, w, "a\n", "b\n")
	<-cp.Done()
	g.mu.Lock()
	refills := g.refills
	g.mu.Unlock()
	if refills >= maxRefills {
		t.Fatalf("Drain read on while the pipe was refilled (%d lines), want it to stop at what the pipe held", refills)
	}
	if got := g.String(); !strings.HasPrefix(got, "a\nb\n") {
		t.Fatalf("the console has %q, want what the pipe held first", got)
	}
}

// TestChildConsoleGivesOnePipeWhenStdoutIsStderr: on one console file the
// child's stdout and stderr lines keep their order.
func TestChildConsoleGivesOnePipeWhenStdoutIsStderr(t *testing.T) {
	cutChildOutput(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	t.Cleanup(func() { os.Stdout, os.Stderr = oldOut, oldErr })
	out := collect(r)
	cmd := exec.Command("sh", "-c", `printf 'one\n'; printf 'two\n' >&2; printf 'three %s\n' '`+plantedConsoleValue+`'`)
	c, err := SetChildConsole(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Stdout != cmd.Stderr || len(c.copies) != 1 {
		t.Fatalf("the child gets %d copies, stdout and stderr one: %v; want one pipe for both", len(c.copies), cmd.Stdout == cmd.Stderr)
	}
	err = cmd.Start()
	c.Started()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	c.Exited()
	const mark = "--exited--\n"
	if _, err := w.WriteString(mark); err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, mark)
	if early, _, _ := strings.Cut(out.String(), mark); early != "one\ntwo\nthree ***\n" {
		t.Fatalf("the console has %q, want the lines in the order the child wrote them", early)
	}
}
