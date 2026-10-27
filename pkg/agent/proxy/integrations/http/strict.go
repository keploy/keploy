package http

import (
	"context"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
)

// Strict HTTP matching under the proven switch (design v14 row S1; §9's
// "every call keploy mocks is held"): a recorded request answers a live one
// only when it went to the same place.
//
// The place is the container the request reached, by its container key
// (containerkey: repository, container port and the container's own
// configuration), when the live call and the recording both reached a
// container: a container on a random published port (Testcontainers) is the
// same place across runs while its address is not, and a container of
// another version of the image is the same dependency. Behind one container
// the hostname the client sent must agree too (a gateway or LocalStack
// routing by virtual host), unless either side reached it by an IP literal
// or as localhost, which say nothing of where it routes.
//
// A container's key names no recording where it cannot tell containers
// apart: two containers configured the same, both used in one scope, live
// (containerkey.Withheld) or in the recording (RecordedAmbiguous); or a
// container whose configuration could not be read. Such a call matches no
// recording of a container, and such a recording answers no call, by key or
// by address: Docker can give a container this run the port another one had
// in the recording, so an address says nothing of which container it is.
// (A recording made without keys, which names no container, is compared by
// its authority, as every call to it is.)
//
// Otherwise (a recording made without keys, a destination that is no
// container) the place is the authority the client sent: the Host header,
// or the URL's host for a request without one, with its port, lowercased.
// Two upstreams behind one address are two places, and so are two services
// that share a path. "example.com" and "example.com:80", or a name with a
// trailing dot, are not one place: the same app and configuration send the
// same bytes in record and replay.

// destination is where an HTTP request went, as the strict matcher compares
// it.
type destination struct {
	// host is the authority, lowercased.
	host string
	// container: the request reached a container (live: the keyer named its
	// connection's destination; recorded: the mock carries its container's
	// instance). key is that container's key, "" when it names no
	// recording: withheld live, ambiguous or unreadable in the recording.
	container bool
	key       string
	// legacy is, live, the container's key of the legacy form
	// (containerkey.LegacyKeyFromContext), which a mock recorded before
	// secrets were left out of fingerprints names it by: compared with
	// (containerkey.SameKey), never shown.
	legacy string
}

// liveDestKey carries, on a request's context, its destination as decode
// read it when the request came (withLiveDestination).
type liveDestKey struct{}

// withLiveDestination is ctx carrying d, the destination of the request it is
// matched for: every later reading of it (each recording compared, the
// variable pass, the miss report) sees the one read once, when the request
// came, in the scope open then.
func withLiveDestination(ctx context.Context, d destination) context.Context {
	return context.WithValue(ctx, liveDestKey{}, d)
}

// liveDestination is where the live request goes: the one decode read when
// the request came (withLiveDestination), or else the container key of its
// connection's destination (containerkey.StateFromContext: a use of the
// container in the scope open now) and the authority it sent.
func liveDestination(ctx context.Context, input *req) destination {
	if d, ok := ctx.Value(liveDestKey{}).(destination); ok {
		return d
	}
	d := destination{}
	switch key, st := containerkey.StateFromContext(ctx); st {
	case containerkey.Keyed:
		d.container, d.key, d.legacy = true, key, containerkey.LegacyKeyFromContext(ctx)
	case containerkey.Withheld:
		d.container = true
	}
	if input.header != nil {
		d.host = input.header.Get("Host")
	}
	if d.host == "" && input.url != nil {
		d.host = input.url.Host
	}
	d.host = strings.ToLower(d.host)
	return d
}

// recordedDestination is where a recorded request went: its container, by
// its key unless the recording's key is ambiguous (two containers configured
// the same answered one scope of it), and its recorded authority
// (models.Mock.RecordedDestination). A recording whose authority cannot be
// read (no Host, a path-only URL: an HTTP/1.0 call without one) is at "",
// which only a live call that sent none matches.
func recordedDestination(m *models.Mock) destination {
	d := destination{container: containerkey.RecordedContainer(m)}
	if d.container && !containerkey.Default.RecordedAmbiguous(m) {
		if key, ok := containerkey.Recorded(m); ok {
			if _, err := containerkey.Parse(key); err == nil {
				d.key = key
			}
		}
	}
	if host, ok := m.RecordedDestination(); ok {
		d.host = strings.ToLower(host)
	}
	return d
}

// sameDestination reports whether a recorded request went where the live one
// goes: to the same container, under the same virtual host, when both
// reached a container; to the same authority when neither did, when only the
// recording did and it names its container by a key (a fixed-port
// dependency CI runs natively), or when only the live call did (a recording
// made without keys, which says nothing of containers). A recording whose
// container its key cannot name (ambiguous, or unreadable) answers no call:
// by address it could be any container Docker gave that address.
func sameDestination(live, rec destination) bool {
	switch {
	case rec.container && rec.key == "":
		return false
	case live.container && rec.container:
		return live.key != "" && containerkey.SameKey(rec.key, live.key, live.legacy) && sameVirtualHost(live.host, rec.host)
	}
	return live.host == rec.host
}

// sameVirtualHost reports whether two authorities name the same host behind
// one container: their hostnames, without ports (the key names the port), are
// equal, or both are addresses (an IP literal or localhost), which name no
// virtual host: a container reached at another address or random port.
func sameVirtualHost(a, b string) bool {
	ha, hb := hostOnly(a), hostOnly(b)
	return ha == hb || (isAddress(ha) && isAddress(hb))
}

// isAddress reports whether a host is an address, not a name: empty, an IP
// literal, or localhost. A name under .localhost (api.localhost) is a name:
// a gateway on this host routes by it.
func isAddress(h string) bool {
	if h == "" || h == "localhost" {
		return true
	}
	_, err := netip.ParseAddr(h)
	return err == nil
}

// destinationDiff is the field difference a report shows when the closest
// recording went elsewhere, nil when it went to live: "container" when the
// recording's container is named by no key, or both reached a container and
// the keys differ (or the live one's names none); "host" when the authority
// (or, behind one container, the virtual host) does.
func destinationDiff(live destination, m *models.Mock) []models.MockFieldDiff {
	rec := recordedDestination(m)
	if sameDestination(live, rec) {
		return nil
	}
	diff := func(path, expected, actual string) []models.MockFieldDiff {
		return []models.MockFieldDiff{{Path: path, Kind: models.DiffKindValueChanged, Expected: expected, Actual: actual}}
	}
	// The container differs: the recording's names none by its key, or both
	// reached one and their keys differ (or the live one's names none).
	if rec.container && (rec.key == "" || live.container && !containerkey.SameKey(rec.key, live.key, live.legacy)) {
		expected, actual := rec.key, live.key
		if expected == "" {
			expected = "a container the recording does not name by its configuration (another configured the same answered its scope, or its configuration was not read)"
		}
		switch {
		case !live.container:
			actual = "no container: the call went to " + live.host
		case actual == "":
			actual = "a container keploy does not name by its configuration (another configured the same is in use, or its configuration was not read)"
		}
		return diff("container", expected, actual)
	}
	return diff("host", rec.host, live.host)
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
	live := liveDestination(ctx, input)
	fits, near, err := strictCompare(ctx, input, live, pool, n)
	if err != nil {
		return nil, nil, false, err
	}
	closest, closestDiffs, routeMatched := near.mock, near.diffs, near.route
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
	// A stateful walk is fixed by every recording the store stages that is
	// this very call, those spent included (servedWalk).
	member := func(m *models.Mock) bool {
		return !hasVariables(m) && string(m.Spec.HTTPReq.Method) == input.method && len(strictDiff(input, live, m, n)) == 0
	}
	picked, commit, answered := h.strictCursorPick(fits, ownedBy(ctx), mockDb, statefulMocks, member)
	if answered != nil {
		return nil, &matchDiag{phase: models.MatchPhaseExhausted, candidates: len(comparedPool), pool: comparedPool, live: &live, answered: answered}, false, nil
	}
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

// nearest is a recording the strict comparison found closest to a call: the
// fields in which it differs, and whether it is of the call's route.
type nearest struct {
	mock  *models.Mock
	diffs []strictField
	route bool
}

// closerThan reports whether c is a closer recording than o: one of the same
// route (method, destination, path) first, then the one with the fewer
// differences. On a tie it is not, so the first one compared stays closest.
func (c nearest) closerThan(o nearest) bool {
	if o.mock == nil {
		return c.mock != nil
	}
	return c.mock != nil && ((c.route && !o.route) || (c.route == o.route && len(c.diffs) < len(o.diffs)))
}

// strictCompare compares the live request with each recording of pool, in
// order (strictDiff): fits are those that are this very call, and near is
// the closest of the others (closerThan), its mock nil when there is none.
func strictCompare(ctx context.Context, input *req, live destination, pool []*models.Mock, n strictNoise) (fits []*models.Mock, near nearest, err error) {
	for _, m := range pool {
		if ctx.Err() != nil {
			return nil, nearest{}, ctx.Err()
		}
		d := strictDiff(input, live, m, n)
		if len(d) == 0 {
			fits = append(fits, m)
			continue
		}
		if c := (nearest{mock: m, diffs: d, route: routeMatches(d)}); c.closerThan(near) {
			near = c
		}
	}
	return fits, near, nil
}

// routeMatches reports whether a recording that differs by diffs is of the
// live request's route: the same method, destination (host or container)
// and path.
func routeMatches(diffs []strictField) bool {
	for _, d := range diffs {
		switch d.diff.Path {
		case "method", "host", "container", "path":
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
// carries on. Under the background rules the open test's own recordings
// (own) and the set's background are two bands (statefulBand). Under exact
// serving the walk is servedWalk's, over the recordings the store stages
// that member says are this call too (spent ones: stagedGroup); answered,
// non-nil, is then the call's miss. commit marks the answer served once the
// answer is; nil when no mark applies.
func (h *HTTP) strictCursorPick(fits []*models.Mock, own func(*models.Mock) bool, mockDb integrations.MockMemDb, statefulMocks bool, member func(*models.Mock) bool) (*models.Mock, func(), *answeredSequence) {
	first := fits[0]
	if !statefulMocks || first.TestModeInfo.Consume != models.ConsumeCursorSaturate {
		return first, nil, nil
	}
	_, exact := integrations.SequenceViewOf(mockDb)
	cs, ok := mockDb.(integrations.MockCursor)
	if !ok && !exact {
		return first, nil, nil
	}
	group := make([]*models.Mock, 0, len(fits))
	for _, m := range fits {
		if m.TestModeInfo.Consume == models.ConsumeCursorSaturate {
			group = append(group, m)
		}
	}
	group, cs, bg, ofBand := statefulBand(first, group, own, mockDb, cs)
	sort.SliceStable(group, func(i, j int) bool { return recordedBefore(group[i], group[j]) })
	i, commit, answered := bandPick(group, cs, bg, mockDb, statefulMember(member, ofBand))
	if answered != nil {
		return nil, nil, answered
	}
	return group[i], commit, nil
}

// statefulMember is, for stagedGroup, a stateful recording member says is
// the call's, of the band (ofBand); nil when member is.
func statefulMember(member, ofBand func(*models.Mock) bool) func(*models.Mock) bool {
	if member == nil {
		return nil
	}
	return func(m *models.Mock) bool {
		return m.TestModeInfo.Consume == models.ConsumeCursorSaturate && ofBand(m) && member(m)
	}
}

// bandPick is servedPick over a band (statefulBand) in record order, with
// cs; under exact serving it is bandWalk's, over one view. answered is
// servedWalk's miss.
func bandPick(group []*models.Mock, cs integrations.MockCursor, bg bool, mockDb integrations.MockMemDb, member func(*models.Mock) bool) (int, func(), *answeredSequence) {
	if sv, exact := integrations.SequenceViewOf(mockDb); exact {
		return bandWalk(group, cs, bg, sv, mockDb, member)
	}
	i, commit := servedPick(group, cs)
	return i, commit, nil
}

// bandWalk is servedWalk over a band (statefulBand) under exact serving,
// fixed by the recordings the store stages that member says are the band's
// call too (stagedGroup), spent ones included: the open test's band over
// sv, from the test's recorded window on; the background's (bg) on the
// background's cursor (cs) over every one of them, its sequence one wherever
// its calls land and whichever test is open. So once the background's last
// recording can answer no more (a worker scope's answers once in the
// staging) the call is a miss, never an earlier recording again. In a test,
// a band of the set's worker scope (sv.Worker: every recording of it) is
// walked the same way: on the cursor the calls between tests advance
// (sv.Between) over every reached recording of it the store stages, by
// placement, with no window. A worker-scope recording belongs to no test:
// record files one in the worker scope when no test's window was open as it
// was recorded, at its place in the gap it was made in, and an idle answer
// wherever it was recorded; so its call is answered the same whether it lands
// in a test or in the gap beside it, and the set's end finds none it reached
// unanswered for want of a window. A test's own recordings are never walked
// here (a test view serves them ConsumeReuse, each once), so under today's
// rules a band in a test is the worker scope's; one that holds any other
// recording keeps the test's window. Each worker-scope recording is spent as
// it answers, so today the cursor's served marks only restate the spends:
// what the band changes is the window. Between tests the walk holds only the
// recordings the gap's view can serve (sv.Gap: its pool slice's, the reached
// worker scope's, the next test's in flight): a test's own recording of the
// call never answers there, so once the gap's own have each answered, the
// last of them answers again while it is live (a beforeAll's page that reads
// once more than recorded), never a later test's that the gap cannot use.
func bandWalk(group []*models.Mock, cs integrations.MockCursor, bg bool, sv integrations.SequenceView, mockDb integrations.MockMemDb, member func(*models.Mock) bool) (int, func(), *answeredSequence) {
	switch {
	case bg:
		sv = integrations.SequenceView{Cursor: cs}
	case sv.Worker != nil && allOf(group, sv.Worker):
		worker := sv.Worker
		sv = integrations.SequenceView{Cursor: sv.Between}
		if member != nil {
			of := member
			member = func(m *models.Mock) bool { return worker(m) && of(m) }
		}
	case sv.Test == "" && sv.Gap != nil:
		if member != nil {
			gap, of := sv.Gap, member
			member = func(m *models.Mock) bool { return gap(m) && of(m) }
		}
	}
	return servedWalk(group, stagedGroup(mockDb, group, member), sv)
}

// allOf reports whether is holds for every recording of group, and group has
// one.
func allOf(group []*models.Mock, is func(*models.Mock) bool) bool {
	for _, m := range group {
		if !is(m) {
			return false
		}
	}
	return len(group) > 0
}

// statefulBand is, of group (the stateful recordings that are a call, first
// among them, the one the pool's order puts first), those of first's band, the
// cursor their served marks are kept in, and whether a recording is of the
// band (ofBand). Under the background rules
// (integrations.BackgroundCursorOf) the open test's own recordings (own) and
// the set's background are two bands, never one sequence: when
// first is the test's own, only its own answer, with cs as before (each
// answers once, in recorded order), so no
// background recording recorded earlier answers the test's own call; else
// only the background's, with the background's cursor, the one its calls
// between tests advance, so its sequence is one wherever its calls land (bg
// true). Under today's rules, and with no owner known, group and cs are as
// given, and every recording is of the band.
func statefulBand(first *models.Mock, group []*models.Mock, own func(*models.Mock) bool, mockDb integrations.MockMemDb, cs integrations.MockCursor) ([]*models.Mock, integrations.MockCursor, bool, func(*models.Mock) bool) {
	bg, s12 := integrations.BackgroundCursorOf(mockDb)
	if !s12 || own == nil {
		return group, cs, false, func(*models.Mock) bool { return true }
	}
	firstOwn := own(first)
	ofBand := func(m *models.Mock) bool { return own(m) == firstOwn }
	band := make([]*models.Mock, 0, len(group))
	for _, m := range group {
		if ofBand(m) {
			band = append(band, m)
		}
	}
	if firstOwn {
		return band, cs, false, ofBand
	}
	return band, bg, true, ofBand
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
