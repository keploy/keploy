package memoryguard

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// ForcePauseEnv is a debug knob that forces one pause of the memory guard by
// the order of ingress requests, not by memory: "<from>:<to>" pauses
// recording as the agent reads its from-th ingress HTTP/1 request (counting
// from 1) and ends the pause as it reads its to-th, so requests from to to-1
// come during the pause. The pause is the guard's own (applyPausedState): the
// recording flag and every mock manager's pressure span move together, as
// under real memory pressure. Request to is read as the pause ends, so its
// window overlaps the pause, and the recording leaves its test case out with
// those the pause overlapped. It is read once, as the agent starts
// (Start). It exists so that a test or a lane can put a pause exactly where
// it wants one, on any machine, instead of waiting for memory to run short.
// Real memory pressure while it is set still pauses and resumes recording
// as it always does, and can end the forced pause early.
const ForcePauseEnv = "KEPLOY_DEBUG_FORCE_PAUSE"

// forcedPauseKnob is a parsed ForcePauseEnv and the count of ingress requests
// read so far. mu orders each request's count with the pause it starts or
// ends, so a request counted after the from-th reads the pause.
type forcedPauseKnob struct {
	mu       sync.Mutex
	from, to uint64
	seen     uint64
	logger   *zap.Logger
}

// forcedPause is nil unless ForcePauseEnv is set, so that, unset, the knob
// costs an ingress request one atomic load, on top of the atomic add that
// counts it (IngressRequest).
var forcedPause atomic.Pointer[forcedPauseKnob]

// parseForcedPause reads a ForcePauseEnv value: two request numbers,
// 1 <= from < to.
func parseForcedPause(v string) (from, to uint64, err error) {
	a, b, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok {
		return 0, 0, fmt.Errorf("want <from>:<to>, got %q", v)
	}
	if from, err = strconv.ParseUint(strings.TrimSpace(a), 10, 64); err != nil {
		return 0, 0, fmt.Errorf("from in %q: %w", v, err)
	}
	if to, err = strconv.ParseUint(strings.TrimSpace(b), 10, 64); err != nil {
		return 0, 0, fmt.Errorf("to in %q: %w", v, err)
	}
	if from < 1 || to <= from {
		return 0, 0, fmt.Errorf("want 1 <= from < to, got %q", v)
	}
	return from, to, nil
}

// configureForcedPause sets the knob from v, the value of ForcePauseEnv, with
// its count at zero; an empty v clears it. A value it cannot read is said
// once and sets nothing.
func configureForcedPause(v string, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if strings.TrimSpace(v) == "" {
		forcedPause.Store(nil)
		return
	}
	from, to, err := parseForcedPause(v)
	if err != nil {
		forcedPause.Store(nil)
		logger.Warn("ignoring "+ForcePauseEnv+", a debug knob", zap.Error(err))
		return
	}
	forcedPause.Store(&forcedPauseKnob{from: from, to: to, logger: logger})
	logger.Warn(ForcePauseEnv+" is set: this recording pauses as if memory ran short, from its ingress request "+
		strconv.FormatUint(from, 10)+" to its request "+strconv.FormatUint(to, 10)+
		"; a debug knob, never for a recording to keep",
		zap.Uint64("from_request", from), zap.Uint64("to_request", to))
}

// IngressRequest counts one ingress request the agent has read, for each
// session's count of the requests that come during a pause (PauseTally), and
// starts or ends the forced pause (ForcePauseEnv) when it is the one that
// does. The ingress calls it as it has read a request, before it reads the
// pause for it (IsRecordingPaused). Unless the knob is set, it costs one
// atomic add and one atomic load.
func IngressRequest() {
	k := forcedPause.Load()
	if k == nil {
		ingressRequests.Add(1)
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	// Counted after the pause it starts or ends, as a request counted after
	// any pause's edge is: it reads the pause as that edge left it, so the
	// forced pause's requests during it are from to to-1, those that read it.
	defer ingressRequests.Add(1)
	k.seen++
	switch k.seen {
	case k.from:
		applyPausedState(true)
		k.logger.Info("Pausing keploy-agent recording, forced by "+ForcePauseEnv,
			zap.Uint64("ingress_request", k.seen))
	case k.to:
		applyPausedState(false)
		k.logger.Info("Cleared keploy-agent memory pressure, forced by "+ForcePauseEnv,
			zap.Uint64("ingress_request", k.seen))
	}
}

// startForcedPause is what Start does with the knob: read it from the
// environment, with its count at zero.
func startForcedPause(logger *zap.Logger) {
	configureForcedPause(os.Getenv(ForcePauseEnv), logger)
}

// SetForcedPauseForTest sets the knob as ForcePauseEnv=v does at Start, and
// returns what clears it and ends a pause it left on. Tests only.
func SetForcedPauseForTest(v string) (restore func()) {
	configureForcedPause(v, nil)
	return func() {
		forcedPause.Store(nil)
		applyPausedState(false)
	}
}
