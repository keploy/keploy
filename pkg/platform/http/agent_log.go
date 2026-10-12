package http

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	agentUtils "go.keploy.io/server/v3/pkg/platform/http/utils"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

const (
	maxFactLine    = 64 << 10
	slowAgentStart = 20 * time.Second
)

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
		case errors.Is(err, bufio.ErrBufferFull) || len(line) >= maxFactLine:
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
	if len(l) > maxFactLine {
		t.long(l)
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
	if warnLevel.Match(sgr.ReplaceAll(l[:min(len(l), 4096)], nil)) {
		t.warns++
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

var needsPTY = agentUtils.NeedsPTY

func (a *AgentClient) sudoRoute(elevates, cached bool) (bool, bool, error) {
	if !elevates || cached {
		return false, cached, nil
	}
	if needsPTY() {
		return true, false, nil
	}
	if !a.mock || a.conf.Debug {
		return false, false, nil
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return false, false, fmt.Errorf("keploy's agent needs root on Linux and sudo is not installed; run keploy as root: %s", utils.QuoteArgs(append([]string{"keploy"}, os.Args[1:]...)))
	}
	return false, false, nil
}

func (a *AgentClient) sayLog() {
	a.failed = true
	a.logger.Error("agent log: "+a.agentLog, log.Show)
	raw, err := os.ReadFile(a.agentLog)
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "sudo:") || strings.HasPrefix(l, "sudo-rs:") {
			again := utils.QuoteArgs(append([]string{"keploy"}, os.Args[1:]...))
			if exe, err := os.Executable(); err == nil {
				again = utils.QuoteArgs(append([]string{"sudo", "-E", "env", "PATH=" + os.Getenv("PATH"), exe}, os.Args[1:]...))
			}
			a.logger.Error(l, log.Show)
			a.logger.Info("NEXT  run with sudo:  "+again, log.Show)
		}
		return
	}
}

func (a *AgentClient) watchStart() {
	if a.agentLog == "" {
		return
	}
	a.logMu.Lock()
	defer a.logMu.Unlock()
	a.slow = time.AfterFunc(slowAgentStart, func() {
		a.logMu.Lock()
		defer a.logMu.Unlock()
		if a.dropped {
			return
		}
		a.named = true
		a.logger.Info(fmt.Sprintf("keploy's agent container has not started after %s; docker's output, including any image download, is in %s", slowAgentStart, a.agentLog), log.Show)
	})
}

func (a *AgentClient) agentReady() {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.slow != nil {
		a.slow.Stop()
	}
}

func (a *AgentClient) sayAgentLog() {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.agentLog == "" || a.failed || a.dropped {
		return
	}
	a.sayLog()
}

func (a *AgentClient) endAgentLog(stopped bool) {
	if a.tail == nil {
		return
	}
	warns := a.tail.end()
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if a.slow != nil {
		a.slow.Stop()
	}
	_ = a.agentFile.Close()
	if !stopped && !a.failed && !a.dropped {
		a.sayLog()
	}
	if a.failed || a.dropped || a.named || !stopped {
		return
	}
	if warns > 0 {
		what := fmt.Sprintf("%d warnings or errors", warns)
		if warns == 1 {
			what = "1 warning or error"
		}
		a.logger.Info("keploy's agent logged "+what+"; its log: "+a.agentLog, log.Show)
		return
	}
	a.dropped = true
	_ = os.Remove(a.agentFile.Name())
}
