// Package consumer is the agent-side runtime of Keploy's consumer testing
// contract: the delivery gate, the projector SPI and the unit recorder.
//
// WHAT THIS PACKAGE IS FOR. `keploy record -c "./worker"` against a real
// broker should turn each consumed message into a test case, and
// `keploy test -c "./worker"` should replay it with NO broker: the recorded
// poll response is delivered as the trigger and what the worker produced is
// asserted. The three pieces here are the protocol-neutral half of that:
//
//   - the Gate (gate.go) decides WHEN a recorded payload may reach the app,
//     and collects what the app produced inside that window;
//   - the projector registry (this file) is the SPI a protocol parser
//     implements to turn its own wire payloads into models.EffectView;
//   - the Recorder (recorder.go) turns a stream of role-tagged mocks into
//     consumer units and mints one test case per unit.
//
// NOTHING HERE IS PROTOCOL-AWARE. OSS ships no Kafka, Pulsar or SQS parser —
// pkg/agent/proxy/integrations contains async, generic, http and mysql only —
// so nothing in this repository can drive this package end to end. That is
// deliberate, not an oversight: the contract, the gate and the judge are the
// part a user must be able to read before trusting a green consumer suite, so
// they live in OSS, and the protocol decoders live where they already are.
// Until an enterprise parser registers a Deliverer and a Projector, this
// package is inert. Its merge gate is its unit tests plus the in-repo fake
// projector under ./consumerfake, not user-visible behaviour.
//
// See keploy-consumer-design-v2.md §0 (the seven decisions), §3 (record flow),
// §4 (replay flow) and §5 (the correctness contract).
package consumer

import (
	"fmt"
	"sort"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Projector is the SPI a protocol parser implements so this package can see
// its payloads without knowing anything about its wire format.
//
// Project decodes ONE mock into zero or more protocol-neutral views:
//
//   - a poll/push delivery ("the trigger") normally yields one view whose
//     Records says how many protocol records the frame carried; an EMPTY poll
//     response yields ZERO views, and that is how the Recorder recognises an
//     idle poll;
//   - something the worker produced ("an effect") yields one view PER RECORD,
//     so completion arithmetic counts records rather than requests. Batching
//     (linger.ms / batch.size and their equivalents) makes requests-per-record
//     nondeterministic between runs, so a request-count rule flakes by
//     construction.
//
// REFUSE, DO NOT GUESS. A projector that cannot fully model a payload must
// stamp models.DecodedOpaque on the view (or return an error) rather than
// emit a best-effort decode. Comparing a misparse against the same misparse
// is a silent PASS, which is strictly worse than a named refusal — see
// design §5, false-pass row 5.
//
// A Projector is called from parser goroutines and must be safe for
// concurrent use. It must not retain the mock it is handed.
type Projector interface {
	Project(m *models.Mock) ([]models.EffectView, error)
}

// ProjectorFunc adapts a plain function to Projector.
type ProjectorFunc func(m *models.Mock) ([]models.EffectView, error)

// Project implements Projector.
func (f ProjectorFunc) Project(m *models.Mock) ([]models.EffectView, error) { return f(m) }

// registration is one entry in the projector registry.
//
// THE ENTRY IS A POINTER SO UNREGISTERING CANNOT PANIC. The obvious
// implementation stores the Projector interface directly and has the
// unregister closure compare `projectors[protocol] == p`. Comparing two
// interface values panics at run time when their dynamic type is not
// comparable, and a func type is not comparable — so that version panicked for
// exactly the callers ProjectorFunc exists to serve. Comparing the pointer
// identity of a per-registration struct is total: it is safe for every
// Projector implementation, and it still means "unregister MY registration,
// not whatever replaced it".
type registration struct{ p Projector }

// projectors is the process-wide protocol -> Projector registry. Parsers
// register from their package init, exactly like database/sql drivers.
var (
	projectorsMu sync.RWMutex
	projectors   = map[string]*registration{}
)

// RegisterProjector registers p as the projector for protocol, and returns a
// function that unregisters it again.
//
// It PANICS on a duplicate registration for the same protocol. A registry that
// silently kept the first (or silently kept the last) would turn a genuine
// double-registration — two parsers built for the same protocol linked into
// one binary — into a recording whose payloads are decoded by whichever
// package's init happened to run first. That is a coin flip deciding what a
// test asserts. Failing at process start is the only honest answer, and it is
// the same contract sql.Register and http.Handle use.
//
// The returned unregister function exists for tests, which install a fake
// projector around a single case and must not leak it into the next one.
// Production callers register from init and discard it.
func RegisterProjector(protocol string, p Projector) func() {
	if protocol == "" {
		panic("consumer: RegisterProjector called with an empty protocol")
	}
	if p == nil {
		panic("consumer: RegisterProjector called with a nil projector for protocol " + protocol)
	}
	projectorsMu.Lock()
	defer projectorsMu.Unlock()
	if _, dup := projectors[protocol]; dup {
		panic("consumer: a projector is already registered for protocol " + protocol)
	}
	reg := &registration{p: p}
	projectors[protocol] = reg
	return func() {
		projectorsMu.Lock()
		defer projectorsMu.Unlock()
		if projectors[protocol] == reg {
			delete(projectors, protocol)
		}
	}
}

// ProjectorFor returns the projector registered for protocol.
func ProjectorFor(protocol string) (Projector, bool) {
	projectorsMu.RLock()
	defer projectorsMu.RUnlock()
	reg, ok := projectors[protocol]
	if !ok {
		return nil, false
	}
	return reg.p, true
}

// RegisteredProtocols lists the protocols that have a projector, sorted. It
// exists so a refusal can name what IS available instead of only what is not.
func RegisteredProtocols() []string {
	projectorsMu.RLock()
	defer projectorsMu.RUnlock()
	out := make([]string, 0, len(projectors))
	for k := range projectors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrNoProjector reports that no parser has registered a projector for a
// protocol. It is a REFUSAL, never a fallback: guessing at a payload we have
// no decoder for is how a misparse becomes a silent pass.
type ErrNoProjector struct {
	Protocol   string
	Registered []string
}

func (e *ErrNoProjector) Error() string {
	return fmt.Sprintf("no consumer projector registered for protocol %q (registered: %v)", e.Protocol, e.Registered)
}

// ErrProjectorPanic reports that a projector panicked. It carries the
// recovered value so the failure is named rather than folded into "no
// effects observed", which would read as an application regression.
type ErrProjectorPanic struct {
	Protocol string
	Value    any
}

func (e *ErrProjectorPanic) Error() string {
	return fmt.Sprintf("consumer projector for protocol %q panicked: %v", e.Protocol, e.Value)
}

// Project decodes m with the projector registered for protocol, with the
// projector's panics contained.
//
// PANIC CONTAINMENT, AND WHY NOT utils.Recover. The design says a projector
// panic must score the test INTERNAL_FAILURE and "never kill the agent", and
// names utils.Recover as the wrapper. utils.Recover cannot do that:
// utils.Recover -> utils.Stop -> utils.ExecCancel cancels the process-wide
// context, which tears down the whole keploy run. Using it here would turn a
// decoder bug in ONE mock into an aborted recording or an aborted test run —
// the opposite of the stated intent, and a much worse failure than the one it
// is guarding.
//
// What this does instead is the same shape as pkg/agent/proxy/util's
// RecoverWithoutClose, which is this repository's panic handler for exactly
// this situation (a parser goroutine that must not take anything else down
// with it): report through utils.HandleRecovery — the shared core that both
// utils.Recover and RecoverWithoutClose funnel into, giving the identical
// Sentry capture and stack-trace log — and then convert the panic into an
// error. RecoverWithoutClose itself is not reusable here only because it
// swallows the recovered value, and the caller needs it to name the failure;
// its own doc comment prescribes this wrapper ("wrap this in a named-return
// defer that sets err").
func Project(logger *zap.Logger, protocol string, m *models.Mock) (views []models.EffectView, err error) {
	p, ok := ProjectorFor(protocol)
	if !ok {
		return nil, &ErrNoProjector{Protocol: protocol, Registered: RegisteredProtocols()}
	}
	if m == nil {
		return nil, fmt.Errorf("consumer: cannot project a nil mock for protocol %q", protocol)
	}
	defer func() {
		if r := recover(); r != nil {
			views = nil
			err = &ErrProjectorPanic{Protocol: protocol, Value: r}
			if logger != nil {
				logger.Error("recovered from a panic inside a consumer projector",
					zap.String("protocol", protocol),
					zap.Any("panic", r),
					zap.String("next_step", "the caller turns this into a named refusal on the affected test rather than silently skipping the payload; file the panic with the parser owner using the Sentry issue just captured"),
				)
				utils.HandleRecovery(logger, r, "Recovered from panic")
			}
		}
	}()
	return p.Project(m)
}
