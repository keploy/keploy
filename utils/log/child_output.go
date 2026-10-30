package log

import (
	"bytes"
	"io"
	"sync"
)

// ChildOutputRedactor is a Redactor that also cuts what the processes keploy
// starts print on their way to keploy's console: an app, docker compose and
// the containers it attaches to, an agent. It is optional. A Redactor without
// it leaves child output as the child wrote it, which is what keploy did
// before it existed: the redactor's pass over log lines (RedactEncoded) is
// keploy's, and may rewrite text an app's own lines are read for.
//
// Child output is cut a line at a time, and in pieces of a line not yet
// ended, so a value is cut only where it sits inside one line: a value that
// holds a line end (a PEM key, a JSON document) is cut only by the forms the
// implementation registers for its lines.
//
//   - CutsChildOutput says whether it has anything to cut now.
//   - RedactChildOutput cuts one line, or a piece of one HoldChildOutput let
//     go, given without its line end.
//   - HoldChildOutput is how many bytes at the end of text, a line not yet
//     ended, are held until more of it comes: those that may start a value
//     the next bytes complete, and any whole value they would leave split. So
//     what comes before them is cut on its own, by RedactChildOutput, as the
//     whole line would be. 0 lets all of text go.
//
// All three are called from any goroutine.
type ChildOutputRedactor interface {
	CutsChildOutput() bool
	RedactChildOutput(line string) string
	HoldChildOutput(text string) int
}

// cuttingChildOutput is the active redactor when it cuts child output now.
func cuttingChildOutput() ChildOutputRedactor {
	c, _ := loadRedactor().(ChildOutputRedactor)
	if c == nil || !c.CutsChildOutput() {
		return nil
	}
	return c
}

// CutsChildOutput says whether the active redactor cuts anything from what a
// child process prints, now.
func CutsChildOutput() bool {
	return cuttingChildOutput() != nil
}

// RedactChildOutput is one line a child process printed (without its line
// end), cut by the active redactor, or as it is when that cuts nothing from
// child output now.
func RedactChildOutput(line string) string {
	if c := cuttingChildOutput(); c != nil {
		return c.RedactChildOutput(line)
	}
	return line
}

// holdChildOutput is the active redactor's HoldChildOutput, kept within
// text, or 0 when it cuts nothing from child output now.
func holdChildOutput(text string) int {
	c := cuttingChildOutput()
	if c == nil {
		return 0
	}
	h := c.HoldChildOutput(text)
	if h < 0 {
		return 0
	}
	if h > len(text) {
		return len(text)
	}
	return h
}

// maxHeldLine is the most of one unended line a ChildOutputWriter holds. A
// held piece that reaches it is written out cut as it stands, so a child that
// never ends a line cannot grow keploy's memory: only a value longer than it
// can be split there, and is then not cut.
const maxHeldLine = 64 << 10

// ChildOutputWriter writes what a child process prints to dst, cut by
// RedactChildOutput. A line ends at '\n' or at '\r' (a terminal's redrawn
// progress line ends at '\r' alone), and is written with its line end as it
// came. Of a line not yet ended it writes at once all that HoldChildOutput
// lets go, so a prompt or a "Listening..." shows as it comes, and holds the
// rest until the line goes on, ends, or Flush: so a value split across two
// writes is still cut.
//
// It is safe for concurrent use, though each stream wants its own writer: one
// line is held per writer.
type ChildOutputWriter struct {
	mu   sync.Mutex
	dst  io.Writer
	held []byte
}

// NewChildOutputWriter returns a ChildOutputWriter onto dst.
func NewChildOutputWriter(dst io.Writer) *ChildOutputWriter {
	return &ChildOutputWriter{dst: dst}
}

// Write takes p, and writes out every line it ends and what HoldChildOutput
// lets go of the line it leaves unended. Its count is of p's bytes taken,
// written or held: a piece whose write to dst failed is dropped, and counted
// taken, so a caller that writes the rest again repeats none of it.
func (w *ChildOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	taken := 0
	for taken < len(p) {
		rest := p[taken:]
		i := bytes.IndexAny(rest, "\r\n")
		if i < 0 {
			room := maxHeldLine - len(w.held)
			if len(rest) < room {
				w.held = append(w.held, rest...)
				return len(p), w.writeUnended()
			}
			// Full: what may start a value is still held, unless it is all
			// of it.
			w.held = append(w.held, rest[:room]...)
			taken += room
			err := w.writeUnended()
			if err == nil && len(w.held) >= maxHeldLine {
				err = w.writeHeld(len(w.held), nil)
			}
			if err != nil {
				return taken, err
			}
			continue
		}
		w.held = append(w.held, rest[:i]...)
		taken += i + 1
		if err := w.writeHeld(len(w.held), rest[i:i+1]); err != nil {
			return taken, err
		}
	}
	return len(p), nil
}

// Flush writes out the line held unended, cut. Call it once the child's
// output has ended.
func (w *ChildOutputWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.held) == 0 {
		return nil
	}
	return w.writeHeld(len(w.held), nil)
}

// writeUnended writes out what HoldChildOutput lets go of the held, unended
// line.
func (w *ChildOutputWriter) writeUnended() error {
	n := len(w.held) - holdChildOutput(string(w.held))
	if n == 0 {
		return nil
	}
	return w.writeHeld(n, nil)
}

// writeHeld writes the first n held bytes, cut, then end, and keeps the rest
// held.
func (w *ChildOutputWriter) writeHeld(n int, end []byte) error {
	line := RedactChildOutput(string(w.held[:n]))
	w.held = append(w.held[:0], w.held[n:]...)
	_, err := io.WriteString(w.dst, line+string(end))
	return err
}
