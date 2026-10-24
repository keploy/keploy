package http

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
)

// notOwnedRecording is, for a request no mock answered under exact serving
// (the proven switch), the staged recording that is this very call (the
// strict comparison, strictDiff) which the open view does not show: another
// test's, any test's with no test open, or a worker-scope one whose place the
// run has not reached (integrations.StagedFinder). The call was made where it
// was not recorded: record's judging reads it to bake the recording into the
// worker scope (design v14, a2). nil when there is none, or the store
// keeps no staged mocks to look in. ctx carries the request's destination as
// it came (liveDestination).
func notOwnedRecording(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise) *models.Mock {
	m, _ := notOwnedRecordingIn(ctx, input, mockDb, n, callRound{})
	return m
}

// callRound is where a call's first read of the round found it made
// (firstRound): the staging, and, by name, every recording that read's view
// held as its own (the open test's, in flight into it, shared with it, a
// pool view's ahead). read is false before any read, or from a store that
// reads no round.
type callRound struct {
	read  bool
	epoch uint64
	own   map[string]bool
}

// firstRound is the callRound of snap, a call's first read of the round.
func firstRound(snap integrations.RoundSnapshot) callRound {
	c := callRound{read: true, epoch: snap.At.Epoch, own: map[string]bool{}}
	for _, list := range [][]*models.Mock{snap.PerTest, snap.Startup, snap.Session, snap.SessionScoped} {
		for _, m := range list {
			if m != nil && snap.Owner(m) != integrations.OwnerNone {
				c.own[m.Name] = true
			}
		}
	}
	return c
}

// readFirstRound is a call's first read of the round from a store that reads
// it in one snapshot (integrations.RoundReader, under exact serving), for a
// call that no other read of the round decides first: the zero callRound
// from any other store.
func readFirstRound(mockDb integrations.MockMemDb) callRound {
	if snap, ok := integrations.SnapshotRound(mockDb, integrations.RoundQuery{}); ok {
		return firstRound(snap)
	}
	return callRound{}
}

// restagedNote is what a miss says when the replay staged the next test set
// while the call was decided (notOwnedRecordingIn).
const restagedNote = "keploy replay staged the next test set while it decided this request, so keploy names no recording made elsewhere for it: a request of one test set is never named as a recording of the next."

// notOwnedRecordingIn is notOwnedRecording for a call whose first read of the
// round is call (callRound; none read: notOwnedRecording). The store is
// asked now, and names each recording for one miss at most, in the staging
// it is in when it names it (FindStagedNotOwned). What it is asked must be
// of the round the call was made in, though:
//
//   - a recording the call's first read held as its view's own (call.own) is
//     no recording made elsewhere for the call, whatever view the store
//     shows now: the replay ended the call's test under it, and the call is
//     that test's, not one judging moves to the worker scope;
//   - once the replay staged another set (the store's StagingEpoch is no
//     longer the call's), no recording the store holds is of the call's set:
//     a recording of the next set named for the call would be moved there
//     by judging, and its one miss spent on a call it never made.
//
// Both are checked as the store offers each recording, the staging last,
// right before the store names one, so that a name is spent, if at all, in
// the call's staging; a staging that moves after the store named one drops
// it too. restaged then says so (restagedNote), for the miss to tell. A
// store that read a round (call.read) but cannot say its staging
// (integrations.WindowPacer) is asked nothing.
func notOwnedRecordingIn(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise, call callRound) (found *models.Mock, restaged bool) {
	moved := func() bool { return false }
	if call.read {
		p, ok := mockDb.(integrations.WindowPacer)
		if !ok {
			return nil, false
		}
		moved = func() bool { return p.StagingEpoch() != call.epoch }
	}
	live := liveDestination(ctx, input)
	// accepted is what match last accepted: the store names it unless
	// another miss spent its name first, and then offers the next.
	var accepted *models.Mock
	match := func(m *models.Mock) bool {
		// The staging is checked last, right before the store names m: it
		// only moves on, so a store at the call's staging here names m in
		// it.
		if m.Spec.HTTPReq == nil || call.own[m.Name] || len(strictDiff(input, live, m, n)) != 0 || moved() {
			return false
		}
		accepted = m
		return true
	}
	name := ""
	switch f := mockDb.(type) {
	case integrations.StagedKeyFinder:
		path := ""
		if input.url != nil {
			path = input.url.Path
		}
		target := strings.Join(input.header.Values(amzTargetHeader), ",")
		name = f.FindStagedNotOwnedByKeys(models.HTTP, RequestKeysOf(input.method, path, target, n.loose()), match)
	case integrations.StagedFinder:
		name = f.FindStagedNotOwned(models.HTTP, match)
	}
	if moved() {
		// Named in the call's staging, if at all, and the replay staged the
		// next set before the miss is told: it is no longer the call's round.
		return nil, true
	}
	if name == "" || accepted == nil || accepted.Name != name {
		return nil, false
	}
	return accepted, false
}

// amzTargetHeader names an AWS JSON-protocol call's action.
const amzTargetHeader = "X-Amz-Target"

// The request keys (models.RequestKeys) the agent's disk store indexes HTTP
// recordings by, so that a miss's diagnosis decodes only the recordings that
// may be its call. The strict comparison (strictDiff) decides; a key only
// narrows, so every call strictDiff takes for a recording asks by a key the
// recording is under:
//
//   - exact: the method, the path's segments and the X-Amz-Target; a
//     recording whose comparison tolerates no other path or target.
//   - learned: the method and number of path segments; a recording that
//     learned a path segment or its target as noise (or whose target the
//     obfuscator redacted), which a call of another path or target is.
//   - shape: the method and number of path segments; every recording. A call
//     asks by it alone when keploy.yml's noise reaches the path (url noise)
//     or the target (header noise): the recording's path and target as
//     recorded then say nothing of the call's.
//
// A path's segments are split as pathDiffs splits them (pathSegments). The
// method is compared as is, and two paths of different segment counts
// differ, learned noise or not. keploy.yml's url noise is masked within a
// path before its segments are counted: a pattern that matches across a "/"
// can make two paths of different counts the same, a call the shape key does
// not find; the miss then names no recording, and judging bakes nothing.
func requestKeyExact(method string, segs []string, target string) string {
	return "=" + method + "\x00" + strings.Join(segs, "/") + "\x00" + target
}

func requestKeyLearned(method string, segs int) string {
	return "~" + method + "\x00" + strconv.Itoa(segs)
}

func requestKeyShape(method string, segs int) string {
	return "#" + method + "\x00" + strconv.Itoa(segs)
}

// RequestKeysOf are the request keys (models.RequestKeys) an HTTP call made
// with method to path asks the agent's disk store by: with amzTarget, the
// lines of its X-Amz-Target joined by ",". loose, when keploy.yml's noise
// reaches the path or the target, asks by the shape key alone. A parser that
// matches HTTP calls of its own (enterprise's SQS) asks by them too, with
// loose set when its comparison does not hold the path and target to the
// recording's.
func RequestKeysOf(method, path, amzTarget string, loose bool) []string {
	segs := pathSegments(path)
	if loose {
		return []string{requestKeyShape(method, len(segs))}
	}
	return []string{requestKeyExact(method, segs, amzTarget), requestKeyLearned(method, len(segs))}
}

// loose reports whether keploy.yml's noise reaches a call's path or its
// X-Amz-Target (RequestKeysOf).
func (n strictNoise) loose() bool {
	return len(n.url) > 0 || n.header[strings.ToLower(amzTargetHeader)]
}

// recordedRequestKeys are a recorded HTTP mock's request keys
// (RequestKeysOf); none for one with no request.
func recordedRequestKeys(m *models.Mock) []string {
	if m == nil || m.Spec.HTTPReq == nil {
		return nil
	}
	r := m.Spec.HTTPReq
	path, _ := recordedURL(r)
	segs := pathSegments(path)
	method := string(r.Method)
	// The target as headerDiffs reads it: of the header's spellings, the
	// first in sorted order.
	target, hasTarget := "", false
	for _, name := range sortedStringKeys(r.Header) {
		if strings.EqualFold(name, amzTargetHeader) {
			target, hasTarget = r.Header[name], true
			break
		}
	}
	k := strictKnown{learned: m.Spec.ReqBodyNoise, values: util.NewNoiseChecker(m.Noise)}
	learned := hasTarget && (k.learnedAt("header."+strings.ToLower(amzTargetHeader), "") || k.redacted(target))
	for i := range segs {
		if learned {
			break
		}
		learned = k.learnedAt("url.path."+strconv.Itoa(i), "")
	}
	keys := []string{requestKeyShape(method, len(segs))}
	if learned {
		return append(keys, requestKeyLearned(method, len(segs)))
	}
	return append(keys, requestKeyExact(method, segs, target))
}

func init() {
	models.RegisterRequestKeys(models.HTTP, recordedRequestKeys)
}

// markNotOwned makes report the miss of a call that is m, a recording the
// open view does not show (notOwnedRecording): m is its closest mock, with no
// field named as differing, since none does, and its rendered request is m's.
func markNotOwned(report *models.MockMismatchReport, m *models.Mock) {
	report.ClosestMock, report.NotOwned = m.Name, true
	report.FieldDiffs, report.Differs = nil, nil
	report.ClosestMockReq = renderMockRequest(m.Spec.HTTPReq, m.Noise)
	report.NextSteps = notOwnedNextSteps(m.Name)
}

// notOwnedNextSteps says what a miss on a call that is a recording the open
// view does not show (notOwnedRecording) likely means, and what to do.
func notOwnedNextSteps(name string) string {
	return fmt.Sprintf("This request is %s, which keploy recorded where this replay did not make it: in another test's window, or between tests after a test this replay had not reached. A call the application makes on its own (a queue consumer, a scheduler) falls in whichever window its timing puts it in. keploy record's judging reads this miss and moves %s to the test set's worker scope (mappings.yaml worker:), which answers it wherever the replay makes it from its place on; record again if this replay was not keploy record's.", name, name)
}
