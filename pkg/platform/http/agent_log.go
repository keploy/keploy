package http

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"

	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

type agentOut struct {
	mu   sync.Mutex
	file io.Writer
	term io.Writer
	line []byte
}

func (w *agentOut) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.file.Write(p)
	w.line = append(w.line, p...)
	for {
		i := bytes.IndexByte(w.line, '\n')
		if i < 0 {
			break
		}
		if bytes.HasPrefix(w.line, []byte("keploy: ")) {
			_, _ = w.term.Write(w.line[:i+1])
		}
		w.line = w.line[i+1:]
	}
	if len(w.line) > 64<<10 {
		w.line = nil
	}
	return len(p), nil
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
	a.agentLog = f.Name()
	if abs, err := filepath.Abs(f.Name()); err == nil {
		a.agentLog = abs
	}
	w := &agentOut{file: f, term: log.PrimarySink()}
	return w, w
}

func (a *AgentClient) sayAgentLog() {
	if a.agentLog == "" {
		return
	}
	a.sayLog.Do(func() {
		a.logger.Error("agent log: "+a.agentLog, log.Show)
	})
}
