package utils

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	keployLog "go.keploy.io/server/v3/utils/log"
)

// ChildConsole is the stdout and stderr of a child process keploy starts.
//
// When the active redactor cuts nothing from child output
// (log.CutsChildOutput) they are keploy's own os.Stdout and os.Stderr: the
// child inherits keploy's files, as it always has, and sees a terminal when
// keploy has one. Otherwise each is the write end of a pipe keploy owns, whose
// read end is copied to keploy's file through a log.ChildOutputWriter, so
// each line the child prints reaches it cut. The child gets the pipe as an
// *os.File, so cmd.Wait waits on the process alone, never on a process it
// left running that still holds the pipe (`./mock-server &` in a script): the
// copy goes on until the last holder closes it.
//
// When os.Stdout and os.Stderr are one file (a terminal, one CI log), the
// child gets one pipe for both, so its lines keep their order there.
//
// A keploy agent's console (SetAgentConsole) is piped while a run log is open
// too (keployLog.RunLogWriter, keploy mock replay's): each of its lines,
// cut, goes to the run log or to keploy's file by its level.
//
// The choice is made when it is set: a value registered after the child
// started is not cut from its output. A child writing to a pipe may buffer
// its output in blocks and drop its colour, as it does under any pipe. Once
// keploy has exited (killed first, say), the child, an agent mid-teardown
// included, and any process it left running on the pipe die of SIGPIPE at
// their next write, and nohup does not move their output to nohup.out, as
// under any pipe.
type ChildConsole struct {
	parentEnds []*os.File
	copies     []*ChildOutputCopy
	// runLogs are the run log's writers the copies write to: each holds a
	// last line with no line break until Exited flushes it.
	runLogs []io.Writer
}

// SetChildConsole sets cmd's Stdout and Stderr to keploy's own, through a
// ChildConsole. Call Started once cmd.Start has returned, whatever it
// returned, and Exited once cmd.Wait has.
func SetChildConsole(cmd *exec.Cmd) (*ChildConsole, error) {
	return setChildConsole(cmd, false)
}

// SetAgentConsole is SetChildConsole for a keploy agent, whose lines go, while
// a run log is open, to the run log or to keploy's own by their level
// (keployLog.RunLogWriter).
func SetAgentConsole(cmd *exec.Cmd) (*ChildConsole, error) {
	return setChildConsole(cmd, true)
}

func setChildConsole(cmd *exec.Cmd, runLog bool) (*ChildConsole, error) {
	c := &ChildConsole{}
	out, err := c.file(os.Stdout, runLog)
	if err != nil {
		return nil, err
	}
	if out != os.Stdout && sameFile(os.Stdout, os.Stderr) {
		cmd.Stdout, cmd.Stderr = out, out
		return c, nil
	}
	errOut, err := c.file(os.Stderr, runLog)
	if err != nil {
		c.Started()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = out, errOut
	return c, nil
}

// file is what the child gets for dst: dst itself, or a pipe copied to it cut,
// through the run log's writer where runLog is set and a run log is open.
func (c *ChildConsole) file(dst *os.File, runLog bool) (*os.File, error) {
	var to io.Writer = dst
	if runLog {
		to = keployLog.RunLogWriter(dst)
	}
	split := to != io.Writer(dst)
	if !keployLog.CutsChildOutput() && !split {
		return dst, nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("make the pipe the child's output is cut through: %w", err)
	}
	c.parentEnds = append(c.parentEnds, w)
	if split {
		c.runLogs = append(c.runLogs, to)
	}
	cp := StartChildOutputCopy(to, r, true)
	c.copies = append(c.copies, cp)
	go func() {
		<-cp.Done()
		_ = r.Close()
	}()
	return w, nil
}

// sameFile says whether a and b are one open file.
func sameFile(a, b *os.File) bool {
	ai, err := a.Stat()
	if err != nil {
		return false
	}
	bi, err := b.Stat()
	return err == nil && os.SameFile(ai, bi)
}

// Started closes keploy's copy of the pipes' write ends, so that each copy
// ends when the child, and every process it left holding the pipe, has
// closed it. Call it once cmd.Start has returned, whatever it returned.
func (c *ChildConsole) Started() {
	if c == nil {
		return
	}
	for _, w := range c.parentEnds {
		_ = w.Close()
	}
	c.parentEnds = nil
}

// Exited writes out, cut, everything the child wrote before it exited, its
// last line too if it left it unended, and returns: it does not wait for a
// process the child left running (whose output goes on being copied). Call it
// once cmd.Wait has returned.
func (c *ChildConsole) Exited() {
	if c == nil {
		return
	}
	for _, cp := range c.copies {
		cp.Drain()
	}
	for _, w := range c.runLogs {
		_ = keployLog.FlushRunLogWriter(w)
	}
}

// ChildOutputCopy copies what a child process prints, from the read end of
// its pipe or the master side of its terminal, to keploy's console, through
// a log.ChildOutputWriter: each line is cut of what the active redactor cuts
// when it is written, so a value registered after the child started is cut
// too, and with nothing to cut the bytes are the child's.
type ChildOutputCopy struct {
	src       *os.File
	dst       io.Writer
	flush     func()
	keep      bool
	drainOnce sync.Once
	canDrain  bool
	drainReq  chan struct{}
	drained   chan struct{}
	done      chan struct{}
}

// StartChildOutputCopy starts copying src to dst until src ends (EOF, or EIO
// once a terminal's last slave is closed), or until Drain, after which it
// goes on only when keep is set. Its caller closes src. A write to dst that
// fails is dropped, and the copy goes on: a child is never stopped by
// keploy's console going away.
//
// A terminal's master side is not drained: creack/pty leaves it in blocking
// mode, where a deadline does not wake a read already blocked on it. Its
// copy is waited on (Done) before it is closed, and ends at EIO, once the
// kernel has moved to it all the command wrote.
func StartChildOutputCopy(dst io.Writer, src *os.File, keep bool) *ChildOutputCopy {
	cw := keployLog.NewChildOutputWriter(dropErrors{dst})
	c := &ChildOutputCopy{
		src:      src,
		dst:      cw,
		flush:    func() { _ = cw.Flush() },
		keep:     keep,
		drainReq: make(chan struct{}),
		drained:  make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.run()
	return c
}

// Drain writes out all src holds now, flushes the line held unended, and
// returns. Call it once the child has exited: what it wrote is then all in
// src. It reads no more than src held when it began, so a process the child
// left running that writes on cannot keep it from returning. Without
// deadlines on src (no poller, as on Windows) it returns at once and the
// copy goes on to src's end.
func (c *ChildOutputCopy) Drain() {
	c.drainOnce.Do(func() {
		// Not a time budget: a deadline in the past is how a read blocked
		// on src is woken to drain it. It is set before drainReq is closed,
		// so a copy that sees drainReq closed reads no more but the drain.
		c.canDrain = c.src.SetReadDeadline(time.Unix(1, 0)) == nil
		close(c.drainReq)
	})
	if !c.canDrain {
		return
	}
	select {
	case <-c.drained:
	case <-c.done:
	}
}

// Done is closed once the copy has stopped.
func (c *ChildOutputCopy) Done() <-chan struct{} { return c.done }

func (c *ChildOutputCopy) run() {
	defer close(c.done)
	buf := make([]byte, 32<<10)
	for {
		n, err := c.src.Read(buf)
		if n > 0 {
			_, _ = c.dst.Write(buf[:n])
		}
		if err == nil {
			continue
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// Only Drain sets a deadline, and it closes drainReq next.
			<-c.drainReq
			drainNow(c.src, buf, c.dst)
			c.flush()
			close(c.drained)
			if !c.keep {
				return
			}
			_ = c.src.SetReadDeadline(time.Time{})
			continue
		}
		c.flush()
		return
	}
}

// dropErrors writes to w and reports every write whole.
type dropErrors struct{ w io.Writer }

func (d dropErrors) Write(p []byte) (int, error) {
	_, _ = d.w.Write(p)
	return len(p), nil
}
