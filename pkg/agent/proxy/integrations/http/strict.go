package http

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
)

// Strict HTTP matching under the proven switch (design v14 row S1; §9's
// "every call keploy mocks is held"): a recorded request answers a live one
// only when it went to the same place.
//
// The place is the authority the client sent: the Host header, or the URL's
// host for a request without one, with its port. Two upstreams behind one
// address (a gateway or LocalStack routing by virtual host) are two places,
// and so are two services that share a path ("/v1/charge" on two hosts). The
// authority is compared as sent, lowercased: "example.com" and
// "example.com:80", or a name with a trailing dot, are not one place. The
// same app and configuration send the same bytes in record and replay; a
// configuration that changes them misses, naming the host.
//
// The container a request reached is not compared yet. Its key (package
// containerkey) is recorded on the mock, but it numbers an image's
// containers by their first use, which concurrent first uses can swap. Until
// the key names a container by its own configuration instead, a container
// reached on a port Docker publishes at random (Testcontainers) is another
// authority each run, and its recordings miss under the switch.

// destination is where an HTTP request went, as the strict matcher compares
// it: its authority, lowercased.
type destination struct {
	host string
}

// liveDestination is where the live request goes: the authority it sent.
func liveDestination(input *req) destination {
	d := destination{}
	if input.header != nil {
		d.host = input.header.Get("Host")
	}
	if d.host == "" && input.url != nil {
		d.host = input.url.Host
	}
	d.host = strings.ToLower(d.host)
	return d
}

// recordedDestination is where a recorded request went: its recorded
// authority (models.Mock.RecordedDestination). A recording whose authority
// cannot be read (no Host, a path-only URL: an HTTP/1.0 call without one) is
// at "", which only a live call that sent none matches.
func recordedDestination(m *models.Mock) destination {
	d := destination{}
	if host, ok := m.RecordedDestination(); ok {
		d.host = strings.ToLower(host)
	}
	return d
}

// sameDestination reports whether a recorded request went where the live one
// goes: to the same authority.
func sameDestination(live, rec destination) bool {
	return live.host == rec.host
}

// destinationDiff is the field difference a report shows when the closest
// recording went elsewhere, nil when it went to live: "host", the authority.
func destinationDiff(live destination, m *models.Mock) []models.MockFieldDiff {
	rec := recordedDestination(m)
	if sameDestination(live, rec) {
		return nil
	}
	return []models.MockFieldDiff{{Path: "host", Kind: models.DiffKindValueChanged, Expected: rec.host, Actual: live.host}}
}

// sameDestinationMocks are the mocks recorded for live's destination.
func sameDestinationMocks(live destination, mocks []*models.Mock) []*models.Mock {
	var out []*models.Mock
	for _, m := range mocks {
		if m != nil && m.Spec.HTTPReq != nil && sameDestination(live, recordedDestination(m)) {
			out = append(out, m)
		}
	}
	return out
}

// strictServe answers the live request, under the proven switch, with the
// first recording in pool that is this very call (strictDiff), in the pool's
// order: the test's own mocks first. A request recorded several times answers
// with each of its recordings in turn (strictCursorPick). There is no lenient
// pass, no closest match and no fuzzy fallback (design v14 row S1): a call
// no recording is answers nothing, and the miss names what differs from the
// closest recording (diag.closest, diag.differs). retry is true when another
// connection took the recording first; comparedPool is the set the miss
// report says was compared.
func (h *HTTP) strictServe(ctx context.Context, input *req, pool, comparedPool []*models.Mock, mockDb integrations.MockMemDb, n strictNoise, statefulMocks bool) (served *models.Mock, diag *matchDiag, retry bool, err error) {
	live := liveDestination(input)
	var fits []*models.Mock
	var closest *models.Mock
	var closestDiffs []strictField
	routeMatched := false
	for _, m := range pool {
		if ctx.Err() != nil {
			return nil, nil, false, ctx.Err()
		}
		d := strictDiff(input, live, m, n)
		if len(d) == 0 {
			fits = append(fits, m)
			continue
		}
		// The closest recording: one of the same route (method, destination,
		// path) first, then the one with the fewest differences, the first in
		// the pool's order on a tie.
		route := routeMatches(d)
		if closest == nil || (route && !routeMatched) || (route == routeMatched && len(d) < len(closestDiffs)) {
			closest, closestDiffs, routeMatched = m, d, route
		}
	}
	if len(fits) == 0 {
		phase := models.MatchPhaseStrict
		if !routeMatched {
			phase = models.MatchPhaseSchema
		}
		diag = &matchDiag{phase: phase, candidates: len(comparedPool), pool: comparedPool, live: &live, closest: closest, differs: closestDiffs}
		if closest != nil && routeMatched {
			diag.schemaMatched = []*models.Mock{closest}
		}
		return nil, diag, false, nil
	}
	picked, commit := h.strictCursorPick(fits, mockDb, statefulMocks)
	served, claimed, err := h.claim(ctx, picked, mockDb, nil, nil)
	if err != nil {
		return nil, nil, false, err
	}
	if !claimed {
		return nil, nil, true, nil
	}
	if commit != nil {
		commit()
	}
	return served, nil, false, nil
}

// routeMatches reports whether a recording that differs by diffs is of the
// live request's route: the same method, destination (host) and path.
func routeMatches(diffs []strictField) bool {
	for _, d := range diffs {
		switch d.diff.Path {
		case "method", "host", "path":
			return false
		}
	}
	return true
}

// strictCursorPick chooses, among the recordings that are this call (fits, in
// the pool's order), the one that answers: the first, or, when it is a
// stateful recording (ConsumeCursorSaturate), the first of the stateful
// recordings that are this call, in record order, not served yet, and the
// last once all were (servedPick, the variable pass's rule). Each recording
// carries its own served mark, so another caller's calls (other credentials:
// other recordings) never move it, and a call whose set of recordings grows
// carries on. commit marks it served once the answer is; nil when no mark
// applies.
func (h *HTTP) strictCursorPick(fits []*models.Mock, mockDb integrations.MockMemDb, statefulMocks bool) (*models.Mock, func()) {
	first := fits[0]
	if !statefulMocks || first.TestModeInfo.Consume != models.ConsumeCursorSaturate {
		return first, nil
	}
	cs, ok := mockDb.(integrations.MockCursor)
	if !ok {
		return first, nil
	}
	group := make([]*models.Mock, 0, len(fits))
	for _, m := range fits {
		if m.TestModeInfo.Consume == models.ConsumeCursorSaturate {
			group = append(group, m)
		}
	}
	sort.SliceStable(group, func(i, j int) bool { return recordedBefore(group[i], group[j]) })
	i, commit := servedPick(group, cs)
	return group[i], commit
}

// strictMismatchReport is the miss report of a call the strict comparison
// answered with nothing: diffed against its closest recording with the fields
// strictServe found differ, and Differs naming them without their values,
// which is what keploy's reason for the failed test names ("(body.amount
// differs)"). Only the fields the strict comparison found differ are named:
// a field it excused, or found equal, is not.
func strictMismatchReport(request *http.Request, liveBody []byte, dest, actualKey string, diag *matchDiag, comparedDests []string, phase string, candidateCount int) *models.MockMismatchReport {
	closest := diag.closest
	diffs := fieldDiffs(diag.differs)
	differs := differsOf(diag.differs)
	redactFieldDiffs(diffs, closest.Noise)
	r := mismatch.NewReport(mismatch.ProtocolHTTP, actualKey).
		WithDestination(dest).
		WithComparedDestinations(comparedDests).
		WithPhase(phase, candidateCount).
		WithClosest(closest.Name, diffs).
		WithRenderedRequests(
			renderMockRequest(closest.Spec.HTTPReq, closest.Noise),
			renderLiveRequest(request, liveBody, closest.Noise),
		).Build()
	if r != nil {
		r.Differs = differs
	}
	return r
}

// differsOf is the names of the fields that differ, as a reason names them
// (each strictField's shape: no part that may be a value), in the strict
// comparison's order, each once.
func differsOf(fields []strictField) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		if f.shape != "" && !seen[f.shape] {
			seen[f.shape] = true
			out = append(out, f.shape)
		}
	}
	return out
}
