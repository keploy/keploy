package http

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

const maxFactLine = 64 << 10

var (
	factLine  = []byte("keploy: ")
	sgr       = regexp.MustCompile("\x1b\\[[0-9;]*m")
	warnLevel = regexp.MustCompile(`\t(WARN|ERROR|DPANIC|PANIC|FATAL)\t|^panic: |^fatal error: `)
)

type agentTail struct {
	term  io.Writer
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	warns int
	skip  bool
}

func tailAgentLog(path string, term io.Writer, every time.Duration) *agentTail {
	t := &agentTail{term: term, stop: make(chan struct{}), done: make(chan struct{})}
	go t.run(path, every)
	return t
}

func (t *agentTail) run(path string, every time.Duration) {
	defer close(t.done)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, maxFactLine)
	var line []byte
	stopping := false
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		switch {
		case err == nil:
			t.line(line)
			line = line[:0]
		case errors.Is(err, bufio.ErrBufferFull):
			if len(line) >= maxFactLine {
				t.long(line)
				line = line[:0]
			}
		case stopping:
			if len(line) > 0 {
				t.line(append(line, '\n'))
			}
			return
		default:
			select {
			case <-t.stop:
				stopping = true
			case <-time.After(every):
			}
		}
	}
}

func (t *agentTail) line(l []byte) {
	if t.skip {
		t.skip = false
		return
	}
	if bytes.HasPrefix(l, factLine) {
		_, _ = t.term.Write(l)
	}
	if warnLevel.Match(sgr.ReplaceAll(l, nil)) {
		t.warns++
	}
}

func (t *agentTail) long(l []byte) {
	if t.skip {
		return
	}
	t.skip = true
	if bytes.HasPrefix(l, factLine) {
		_, _ = t.term.Write(append(l[:maxFactLine:maxFactLine], "...\n"...))
	}
}

func (t *agentTail) end() int {
	t.once.Do(func() { close(t.stop) })
	<-t.done
	return t.warns
}

func (a *AgentClient) agentOutput() (io.Writer, io.Writer) {
	if !a.mock || a.conf.Debug {
		return os.Stdout, os.Stderr
	}
	f, err := log.OpenAgentLog()
	if err != nil {
		a.logger.Debug("could not open the agent log; the agent writes to the console", zap.Error(err))
		return os.Stdout, os.Stderr
	}
	a.agentFile = f
	a.agentLog = f.Name()
	if abs, err := filepath.Abs(f.Name()); err == nil {
		a.agentLog = abs
	}
	a.tail = tailAgentLog(f.Name(), log.PrimarySink(), 100*time.Millisecond)
	return f, f
}

func (a *AgentClient) sayAgentLog() {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.agentLog == "" || a.failed || a.dropped {
		return
	}
	a.failed = true
	a.logger.Error("agent log: "+a.agentLog, log.Show)
}

func (a *AgentClient) endAgentLog(stopped bool) {
	if a.tail == nil {
		return
	}
	warns := a.tail.end()
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.failed || a.dropped || !stopped {
		return
	}
	if warns > 0 {
		what := fmt.Sprintf("%d warnings", warns)
		if warns == 1 {
			what = "1 warning"
		}
		a.logger.Info("keploy's agent logged "+what+"; its log: "+a.agentLog, log.Show)
		return
	}
	a.dropped = true
	_ = a.agentFile.Close()
	_ = os.Remove(a.agentFile.Name())
}
