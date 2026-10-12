package http

import (
	"bufio"
	"bytes"
	"context"
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

var sudoGrace = 2 * time.Second

func startSudo(ctx context.Context, logger *zap.Logger, cmd *exec.Cmd, again func() *exec.Cmd, logPath string, tty bool) (*exec.Cmd, <-chan error, error) {
	for try := 0; ; try++ {
		if err := agentUtils.StartCommand(cmd); err != nil {
			return nil, nil, err
		}
		waited := make(chan error, 1)
		go func(c *exec.Cmd) { waited <- c.Wait() }(cmd)
		select {
		case <-time.After(sudoGrace):
			return cmd, waited, nil
		case err := <-waited:
			said := sudoSaid(logPath)
			if said == "" {
				back := make(chan error, 1)
				back <- err
				return cmd, back, nil
			}
			if !tty || try > 0 {
				return nil, nil, fmt.Errorf("keploy's agent needs root on Linux, and sudo said: %s; keploy ran: %s", said, utils.QuoteArgs(cmd.Args))
			}
			if err := utils.CacheSudoCredentials(ctx, logger); err != nil {
				return nil, nil, fmt.Errorf("keploy's agent needs root on Linux, and sudo did not accept the password (%w); keploy ran: %s", err, utils.QuoteArgs(cmd.Args))
			}
			next := again()
			next.Stdout, next.Stderr = cmd.Stdout, cmd.Stderr
			cmd = next
		}
	}
}

func sudoSaid(logPath string) string {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "sudo: ") && strings.Contains(l, "password is required") {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
	if a.slow != nil {
		a.slow.Stop()
	}
	_ = a.agentFile.Close()
	if !stopped && !a.failed && !a.dropped {
		a.failed = true
		a.logger.Error("agent log: "+a.agentLog, log.Show)
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
