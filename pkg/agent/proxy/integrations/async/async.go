// Package async implements keploy's transport-agnostic async-egress engine.
// Parsers opt in to async handling by implementing AsyncParser; the proxy
// injects the shared Engine into opted-in parsers via AsyncAware.
package async

import "go.keploy.io/server/v3/pkg/models"

// AsyncParser is the capability interface a protocol parser implements to
// participate in async egress. The engine holds only *models.Mock and
// models.AsyncLane, delegating every protocol decision here — this is what
// keeps the engine transport-agnostic.
type AsyncParser interface {
	// MatchesLane reports whether the egress (recorded mock at record time,
	// or a live request wrapped as a mock at replay) belongs to the lane.
	MatchesLane(m *models.Mock, lane models.AsyncLane) bool

	// MatchRequestShape compares a live request against a recorded async
	// mock's request, treating lane.VolatileParams as noise. ok=false with a
	// human-readable detail on drift.
	MatchRequestShape(live, recorded *models.Mock, lane models.AsyncLane) (ok bool, detail string)

	// EmptyResponse returns the parser's "no data yet" keep-alive payload for
	// the lane, served when no async mock is armed. Always synthesizable.
	EmptyResponse(lane models.AsyncLane) ([]byte, error)

	// ResponseValueKey returns a stable fingerprint of the VALUE a recorded
	// async response conveys, so the recorder can detect when the value changes
	// across poll cycles (identical value => identical key). Volatile/no-signal
	// fields (e.g. Date headers) are excluded.
	ResponseValueKey(m *models.Mock, lane models.AsyncLane) string
}

// AsyncAware is an optional interface a parser implements so the proxy can
// hand it the shared Engine at InitIntegrations time (setter injection,
// mirroring the IntegrationsV2 capability idiom).
type AsyncAware interface {
	SetAsyncEngine(e *Engine)
}
