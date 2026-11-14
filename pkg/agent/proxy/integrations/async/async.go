// Package async implements keploy's transport-agnostic async-egress engine.
// Parsers opt in to async handling by implementing AsyncParser; the proxy
// injects the shared Engine into opted-in parsers via AsyncAware.
package async

import (
	"slices"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
)

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

// LaneTyper is an optional interface of an AsyncParser that serves lanes of
// types other than the integration type it is registered under. A lane names
// an endpoint, not a wire protocol: an "http" lane's endpoint is reached over
// HTTP/1.1 or HTTP/2 as the client and the server negotiate (ALPN), so the
// HTTP/2 parser serves "http" lanes too, beside the HTTP/1 parser registered
// as "http". A parser declares it with no import of this package, by the
// method alone.
type LaneTyper interface {
	LaneTypes() []string
}

// LaneParsers is every parser of byType (keyed by the integration type each
// is registered under) that serves lanes of type base (a lane's BaseType):
// byType[base] first, then each other parser whose LaneTypes name base, in
// key order. Each serves only the calls of its own protocol (its
// MatchesLane), so a lane's call is the lane's when any of them takes it.
func LaneParsers(base string, byType map[string]AsyncParser) []AsyncParser {
	var out []AsyncParser
	if p := byType[base]; p != nil {
		out = append(out, p)
	}
	keys := make([]string, 0, len(byType))
	for k := range byType {
		if k != base {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if lt, ok := byType[k].(LaneTyper); ok && slices.Contains(lt.LaneTypes(), base) {
			out = append(out, byType[k])
		}
	}
	return out
}

// Union is one AsyncParser over the parsers of a lane type (LaneParsers), for
// a caller that asks about one mock at a time (the recorder): a mock is the
// lane's when any of them takes it, and every other question about a mock
// goes to the one that takes it. EmptyResponse, which names no mock, is the
// first's: the parser registered under the lane's own type.
func Union(ps ...AsyncParser) AsyncParser {
	if len(ps) == 1 {
		return ps[0]
	}
	return union(ps)
}

type union []AsyncParser

// pick is the parser that takes m for lane, else the first.
func (u union) pick(m *models.Mock, lane models.AsyncLane) AsyncParser {
	for _, p := range u {
		if p.MatchesLane(m, lane) {
			return p
		}
	}
	return u[0]
}

func (u union) MatchesLane(m *models.Mock, lane models.AsyncLane) bool {
	for _, p := range u {
		if p.MatchesLane(m, lane) {
			return true
		}
	}
	return false
}

func (u union) MatchRequestShape(live, recorded *models.Mock, lane models.AsyncLane) (bool, string) {
	return u.pick(live, lane).MatchRequestShape(live, recorded, lane)
}

func (u union) EmptyResponse(lane models.AsyncLane) ([]byte, error) { return u[0].EmptyResponse(lane) }

func (u union) ResponseValueKey(m *models.Mock, lane models.AsyncLane) string {
	return u.pick(m, lane).ResponseValueKey(m, lane)
}
