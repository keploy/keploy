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
	//
	// The HTTP implementation fingerprints status + body only: a value signal
	// carried ONLY in a non-volatile response header (e.g. an ETag or version
	// header alongside an unchanged body) is out of scope and would be treated
	// as unchanged (collapsed). That matches the body-carries-the-version
	// contract of the config-watch lane this engine targets.
	//
	// NOTE: this is a REQUIRED addition to the exported interface — any
	// out-of-tree AsyncParser implementer must add it. Verified: the only
	// in-tree implementer is *http.HTTP (plus test fakes), and the sibling repos
	// that consume this module (keploy-enterprise, k8s-proxy) implement no
	// AsyncParser (they only declare AsyncLane config + use the env-var wire
	// contract), so this bump breaks no known consumer.
	ResponseValueKey(m *models.Mock, lane models.AsyncLane) string
}

// AsyncAware is an optional interface a parser implements so the proxy can
// hand it the shared Engine at InitIntegrations time (setter injection,
// mirroring the IntegrationsV2 capability idiom).
type AsyncAware interface {
	SetAsyncEngine(e *Engine)
}
