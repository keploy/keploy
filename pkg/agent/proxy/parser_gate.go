package proxy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// debugParserGateEnv names a directory that gates every recorded connection's
// parser (relay.Config.ReaderGate): the parser of the n-th connection the
// proxy records (from 1) gets none of its traffic until a file named n, or
// "all", is in it. It exists for end-to-end tests that need parsers to reach
// their traffic in an order of the test's choosing, by events and not by
// time; unset, the proxy gates nothing.
const debugParserGateEnv = "KEPLOY_DEBUG_PARSER_GATE"

// parserGates are the gates of debugParserGateEnv: a gate opens on the event
// of its file being created in the directory, not on a clock. A directory it
// cannot watch opens none: a gate that opened anyway would let a test that
// orders parsers by them pass without the order it set up.
type parserGates struct {
	dir    string
	logger *zap.Logger
	seq    atomic.Uint64

	mu      sync.Mutex
	watched bool
	watcher *fsnotify.Watcher
	opened  map[string]bool
	waiting map[string][]chan struct{}
}

// cannotWatchGates is said, at ERROR, when the gate directory cannot be
// watched: no gate opens.
const cannotWatchGates = debugParserGateEnv + ": cannot watch the gate directory; no gate opens, and the parsers behind them get no traffic"

// debugParserGate reads debugParserGateEnv, and says so at WARN when it is
// set: a parser held back no one asked for must never go unnoticed. Nil when
// it is not set.
func debugParserGate(logger *zap.Logger) *parserGates {
	dir := strings.TrimSpace(os.Getenv(debugParserGateEnv))
	if dir == "" {
		return nil
	}
	if logger != nil {
		logger.Warn(debugParserGateEnv+" is set: the parser of the n-th recorded connection gets its traffic only once a file named n, or \"all\", is in this directory. It is a test hook; unset it for real recordings",
			zap.String("dir", dir))
	}
	g := &parserGates{dir: dir, logger: logger, opened: map[string]bool{}, waiting: map[string][]chan struct{}{}}
	// Watched from the proxy's start: a directory it cannot watch is said
	// before any connection waits on a gate.
	g.mu.Lock()
	g.watchLocked()
	g.mu.Unlock()
	return g
}

// parserGate is the next recorded connection's reader gate under
// debugParserGateEnv, nil when the hook is not set.
func (p *Proxy) parserGate(ctx context.Context, logger *zap.Logger, dst net.Conn) <-chan struct{} {
	if p.parserGates == nil {
		return nil
	}
	return p.parserGates.next(ctx, logger, dst)
}

// next numbers the next recorded connection and returns its gate: closed once
// its file, or "all", is in the directory. It stops waiting once ctx, the
// context the connection is recorded under, is done.
func (g *parserGates) next(ctx context.Context, logger *zap.Logger, dst net.Conn) <-chan struct{} {
	n := strconv.FormatUint(g.seq.Add(1), 10)
	if logger != nil {
		dest := ""
		if dst != nil && dst.RemoteAddr() != nil {
			dest = dst.RemoteAddr().String()
		}
		logger.Info(debugParserGateEnv+": a recorded connection's parser waits for its gate", zap.String("gate", n), zap.String("destination", dest))
	}
	open := make(chan struct{})
	g.mu.Lock()
	g.watchLocked()
	if g.opened[n] || g.opened["all"] {
		g.mu.Unlock()
		close(open)
		return open
	}
	g.waiting[n] = append(g.waiting[n], open)
	g.mu.Unlock()
	// A file created before the watch began is found by the scan in
	// watchLocked; one created since, by its event. Either way after this
	// connection is waiting, so it is told.
	context.AfterFunc(ctx, func() { g.forget(n, open) })
	return open
}

// watchLocked starts watching the directory, once: every file already in it
// opens its gate, and every file created in it from then on. Caller holds mu.
func (g *parserGates) watchLocked() {
	if g.watched {
		return
	}
	g.watched = true
	w, err := fsnotify.NewWatcher()
	if err == nil {
		if err = w.Add(g.dir); err != nil {
			_ = w.Close()
		}
	}
	if err != nil {
		if g.logger != nil {
			g.logger.Error(cannotWatchGates, zap.String("dir", g.dir), zap.Error(err),
				zap.String("next_step", "create the directory "+debugParserGateEnv+" names, on a filesystem that reports file events (inotify), before the recording starts; or unset it"))
		}
		return
	}
	g.watcher = w
	if entries, err := os.ReadDir(g.dir); err == nil {
		for _, e := range entries {
			g.opened[e.Name()] = true
		}
	}
	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write) != 0 {
					g.openGate(filepath.Base(ev.Name))
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				// An event lost (the queue overflowed) may be a gate's
				// file: said, so a gate that never opens is explained.
				if g.logger != nil {
					g.logger.Warn(debugParserGateEnv+": the gate directory's watch reported an error; a gate's file may have been missed", zap.String("dir", g.dir), zap.Error(err))
				}
			}
		}
	}()
}

// close stops watching the gate directory: no gate opens after it. Nil-safe.
func (g *parserGates) close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.watcher != nil {
		_ = g.watcher.Close()
		g.watcher = nil
	}
}

// openGate opens the gate named name: one connection's, or every one's
// ("all").
func (g *parserGates) openGate(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.opened[name] = true
	for n, chans := range g.waiting {
		if n != name && name != "all" {
			continue
		}
		for _, c := range chans {
			close(c)
		}
		delete(g.waiting, n)
	}
}

// forget stops a connection that ended from waiting for its gate.
func (g *parserGates) forget(n string, open chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	chans := g.waiting[n]
	for i, c := range chans {
		if c == open {
			g.waiting[n] = append(chans[:i], chans[i+1:]...)
			break
		}
	}
	if len(g.waiting[n]) == 0 {
		delete(g.waiting, n)
	}
}
