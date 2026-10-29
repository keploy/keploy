package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sync"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// ComposeChildOutput keeps, in the app-output tail (apptail.go), what the
// containers of keploy's own compose stack print when keploy runs
// `docker compose -f - up` as a child process: the path of a build without
// the compose library (darwin, windows, a build without the composelib tag).
// So such a build, too, takes an app's recent output from what compose
// attached to, cut, and not from `docker logs`, and the stack's containers
// need no log driver that keeps their output on the docker host's disk.
//
// compose prints each line a container prints as `<name><padding> | <line>`,
// on its stdout for the container's stdout and on its stderr for the
// container's stderr, where name is the container as compose names it in its
// output (its container_name, else <service>-<number>) and the padding lines
// the names up. Its own lines (progress, "Attaching to", "exited with code")
// carry no such prefix and are not kept. It is the line format of compose's
// log formatter, the one the compose library's consumer reproduces
// (composeLogConsumer), with its colour codes when compose colours it.
//
// Each StartComposeChildOutput is one Up: it starts a new run of the
// project's tails, as ComposeRunner.Up does, so the tails hold that Up's
// output alone, and a line an earlier Up's child still prints is not kept.
type ComposeChildOutput struct {
	project *composetypes.Project
	run     uint64
}

// StartComposeChildOutput loads content as `docker compose` will, from
// workingDir (the current directory when "") with envFiles (the command's
// --env-file values) under projectName (compose's own precedence when ""),
// to know the project's name and its services, and
// starts a new run of that project's tails. It reads no daemon. Call it before
// the child starts, and hand each of the child's output streams its own
// Stream.
func StartComposeChildOutput(ctx context.Context, content []byte, projectName, workingDir string, envFiles []string) (*ComposeChildOutput, error) {
	if len(content) == 0 {
		return nil, fmt.Errorf("compose content is empty")
	}
	absWorkingDir, err := composeWorkingDir(workingDir)
	if err != nil {
		return nil, err
	}
	project, err := loadComposeProject(ctx, content, projectName, absWorkingDir, envFiles)
	if err != nil {
		return nil, err
	}
	return &ComposeChildOutput{project: project, run: appOutput.startRun(project.Name)}, nil
}

// Project is the compose project the child's output is kept under: the name
// AppOutputTail and ServiceOutputTail take.
func (o *ComposeChildOutput) Project() string {
	if o == nil || o.project == nil {
		return ""
	}
	return o.project.Name
}

// Stream is a writer for one of the child's output streams, as keploy copies
// it to its console, already cut (utils.ChildConsole): each line that carries
// a container's prefix is kept in that container's tail, cut again on the
// way. Each stream wants its own: a Stream holds the line it has not yet seen
// the end of. Its writes never fail.
func (o *ComposeChildOutput) Stream() io.Writer {
	return &composeChildStream{out: o}
}

// composeLinePrefix is compose's prefix of a container's line: the colour
// code compose may open it with, the container's name as compose names
// containers (a docker container name), the padding, "| ", and the colour
// reset that may close it.
var composeLinePrefix = regexp.MustCompile(`^(?:\x1b\[[0-9;]*m)*([a-zA-Z0-9][a-zA-Z0-9_.-]*) +\| (?:\x1b\[0m)?`)

const (
	// composePrefixMax is the most of a line read for compose's prefix. A
	// prefix is a container name (at most a few hundred bytes), padded to the
	// longest name compose attached to, with a colour code; a line whose
	// first composePrefixMax bytes hold none carries none.
	composePrefixMax = 1 << 10
	// composeLineMax is the most of one line's text a stream holds: the most
	// a tail keeps of a line (appTailMaxContainerBytes). Of a longer line it
	// keeps the end, as the tail does, and the rest counts as dropped.
	composeLineMax = appTailMaxContainerBytes
	// composeLineBuf is the most buffer a stream takes for one line's text.
	composeLineBuf = composeLineMax + composeLineMax/4
	// composeLineKeep is the buffer a stream keeps between lines; one that
	// a long line grew past it is let go.
	composeLineKeep = 64 << 10
)

// composeChildStream is one stream of the child's output, split into lines.
type composeChildStream struct {
	mu  sync.Mutex
	out *ComposeChildOutput
	// head is the line's start while its prefix is not yet read.
	head []byte
	// name is the container the line is of, once its prefix is read; skip
	// is set instead when the line carries no prefix.
	name string
	skip bool
	// text is the line's text after the prefix, at most composeLineMax
	// bytes of its end held, and lost the bytes of its start let go.
	text []byte
	lost int64
}

func (w *composeChildStream) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for rest := p; len(rest) > 0; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			w.take(rest)
			break
		}
		w.take(rest[:i])
		w.end()
		rest = rest[i+1:]
	}
	return len(p), nil
}

// take adds b, a piece of the line with no line end in it.
func (w *composeChildStream) take(b []byte) {
	switch {
	case w.skip:
		return
	case w.name == "":
		room := composePrefixMax - len(w.head)
		if len(b) < room {
			w.head = append(w.head, b...)
			return
		}
		w.head = append(w.head, b[:room]...)
		w.readPrefix()
		if w.skip {
			return
		}
		b = b[room:]
	}
	// b is one write, of any length. It is compared with the room the line
	// has left before any sum is made, so every size below is plainly made of
	// values composeLineBuf bounds (CodeQL's allocation-size check needs to
	// see that; the text staying within composeLineBuf is not enough for it).
	if len(b) <= cap(w.text)-len(w.text) {
		w.text = append(w.text, b...)
		return
	}
	if room := composeLineBuf - len(w.text); len(b) <= room {
		// The line with b fits within the bound. need is its length,
		// len(w.text)+len(b), worked out from the room so that no sum is
		// made. The buffer is grown by hand, to twice its size held to
		// composeLineBuf, or to need when that is more, so append's own
		// growth never takes more.
		need := composeLineBuf - (room - len(b))
		c := 2 * cap(w.text)
		if c > composeLineBuf {
			c = composeLineBuf
		}
		if c < need {
			c = need
		}
		grown := make([]byte, len(w.text), c)
		copy(grown, w.text)
		w.text = append(grown, b...)
		return
	}
	// Past the bound: keep the line's last composeLineMax bytes, in a buffer
	// of composeLineBuf, and let the start go. The quarter left free is what
	// the next pieces fill before the start is let go again, so the copy is
	// paid once per quarter of the bound, not once per write.
	kept := make([]byte, 0, composeLineBuf)
	if len(b) >= composeLineMax {
		// b alone holds the line's last composeLineMax bytes.
		w.lost += int64(len(w.text)) + int64(len(b)-composeLineMax)
		kept = append(kept, b[len(b)-composeLineMax:]...)
	} else {
		// b and the text's last composeLineMax-len(b) bytes before it. The
		// text holds more than that, since the line is past the bound.
		over := len(w.text) - (composeLineMax - len(b))
		w.lost += int64(over)
		kept = append(kept, w.text[over:]...)
		kept = append(kept, b...)
	}
	w.text = kept
}

// readPrefix reads compose's prefix off head: the container's name, and the
// line's text after it; or skip when head carries none.
func (w *composeChildStream) readPrefix() {
	m := composeLinePrefix.FindSubmatchIndex(w.head)
	if m == nil {
		w.skip = true
		w.head = w.head[:0]
		return
	}
	w.name = string(w.head[m[2]:m[3]])
	w.text = append(w.text[:0], w.head[m[1]:]...)
	w.head = w.head[:0]
}

// end ends the line: a container's line goes to its tail, cut again.
func (w *composeChildStream) end() {
	if !w.skip && w.name == "" {
		w.readPrefix()
	}
	if !w.skip {
		text := w.text
		if over := len(text) - composeLineMax; over > 0 {
			w.lost += int64(over)
			text = text[over:]
		}
		o := w.out
		appOutput.record(o.project.Name, o.run, w.name, serviceOfContainer(o.project, w.name), string(text), w.lost)
	}
	w.name, w.skip, w.lost = "", false, 0
	w.text = w.text[:0]
	if cap(w.text) > composeLineKeep {
		w.text = nil
	}
}
