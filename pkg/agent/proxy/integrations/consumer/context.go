package consumer

import (
	"context"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// The Gate and the Recorder ride the parser context, the same way the
// mock-mismatch reporter and the per-session syncMock manager already do.
// Context carriage rather than a package global is what lets one process run
// several capture or replay sessions at once — the enterprise multi-app
// reader runs one manager per application — without one session's triggers
// reaching another's worker.
//
// Both lookups return nil when nothing was installed, and every method on
// both types is nil-safe, so a parser can call them unconditionally. That
// matters more than it looks: a parser built against this SPI must keep
// working against an agent that predates it.

type gateKey struct{}

// WithGate returns a child of ctx carrying g. A nil gate is ignored so
// callers can pass through unconditionally.
func WithGate(ctx context.Context, g *Gate) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, gateKey{}, g)
}

// GateFromContext returns the Gate carried by ctx, or nil.
func GateFromContext(ctx context.Context) *Gate {
	if ctx == nil {
		return nil
	}
	g, _ := ctx.Value(gateKey{}).(*Gate)
	return g
}

type recorderKey struct{}

// WithRecorder returns a child of ctx carrying r. A nil recorder is ignored.
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecorderFromContext returns the Recorder carried by ctx, or nil.
func RecorderFromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(recorderKey{}).(*Recorder)
	return r
}

// ProtocolOf derives the projector/deliverer registry key for a mock from its
// Kind: "Kafka" -> "kafka".
//
// Kind is already the mock's protocol family, so this introduces no second
// vocabulary and nothing here has to know which families exist. A parser
// registers under the same lower-cased spelling and the two cannot drift,
// because both sides derive it from the same constant.
func ProtocolOf(m *models.Mock) string {
	if m == nil {
		return ""
	}
	return strings.ToLower(string(m.Kind))
}
