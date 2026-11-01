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
//
// The store is asked of call, the round the request's first read found it
// made in (integrations.CallRound, marked before any matcher read the
// store): it names no recording that read's view held as its own, and none
// once the replay staged another set, which restaged then says
// (restagedNote), for the miss to tell. The zero CallRound names nothing.
func notOwnedRecording(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise, call integrations.CallRound) (found *models.Mock, restaged bool) {
	live := liveDestination(ctx, input)
	// accepted is what match last accepted: the store names it unless
	// another miss spent its name first, and then offers the next.
	var accepted *models.Mock
	match := func(m *models.Mock) bool {
		if m.Spec.HTTPReq == nil || len(strictDiff(ctx, input, live, m, n)) != 0 {
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
		name, restaged = f.FindStagedNotOwnedByKeys(models.HTTP, call, RequestKeysOf(input.method, path, target, n.loose()), match)
	case integrations.StagedFinder:
		name, restaged = f.FindStagedNotOwned(models.HTTP, call, match)
	}
	if restaged || name == "" || accepted == nil || accepted.Name != name {
		return nil, restaged
	}
	return accepted, false
}

// restagedNote is what a miss says when the replay staged a test set again
// while the call was decided: its matcher, held to the call's first read
// (integrations.HoldToRound), found nothing to answer it with, or its lookup
// of a recording made elsewhere (notOwnedRecording) named none. The next
// set, or the same set for a replaced agent, which the store does not tell
// apart.
const restagedNote = "keploy replay staged a test set again while it decided this request, so keploy answered it with no recording and names none made elsewhere for it: a request is never answered with, nor named as, a recording of a staging made after it began."

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
	// A read of the application's clock learned in the path or the target
	// is a value made anew each run there, filed as learned noise is.
	k := strictKnown{learned: m.Spec.ReqBodyNoise, values: util.NewNoiseChecker(m.Noise), clocks: clocksOf(m)}
	learned := hasTarget && (k.learnedAt("header."+strings.ToLower(amzTargetHeader), "") || k.redacted(target))
	for i := range segs {
		if learned {
			break
		}
		learned = k.learnedAt("url.path."+strconv.Itoa(i), "")
	}
	if !learned && hasVariables(m) {
		learned = variableKeyed(m, len(segs))
	}
	keys := []string{requestKeyShape(method, len(segs))}
	if learned {
		return append(keys, requestKeyLearned(method, len(segs)))
	}
	return append(keys, requestKeyExact(method, segs, target))
}

// variableKeyed reports whether a variable of m (variablePlaces) sits where
// the exact request key reads (one of its segs path segments, or its
// X-Amz-Target), or at a place keploy cannot read. A call of m holds a value
// made anew each run there, so m is filed under the learned key, as for
// learned noise there: filed under its recorded value, no call that carries
// another would ask for it, and the learn step's check that a call is exactly
// a recording made elsewhere (exactlyElsewhere) would pass it over.
func variableKeyed(m *models.Mock, segs int) bool {
	at, err := variablePlaces(m)
	if err != nil {
		return true
	}
	if at["header."+strings.ToLower(amzTargetHeader)] {
		return true
	}
	for i := 0; i < segs; i++ {
		if at["url.path."+strconv.Itoa(i)] {
			return true
		}
	}
	return false
}

func init() {
	models.RegisterRequestKeys(models.HTTP, recordedRequestKeys)
}

// markNotOwned makes report the miss of a call that is m, a recording the
// open view does not show (notOwnedRecording): m is its closest mock, with no
// field named as differing, since none does, and its rendered request is m's.
// call is where the call's first read found it made: a call keploy mock
// replay made (no set staged, models.UnroutedUnstaged) is told the mapping
// edit (notOwnedMappingEdit), never record's judging, which keploy mock has
// none of.
func markNotOwned(report *models.MockMismatchReport, m *models.Mock, call integrations.CallRound) {
	report.ClosestMock, report.NotOwned = m.Name, true
	report.FieldDiffs, report.Differs, report.FieldReasons = nil, nil, nil
	report.ClosestMockReq = renderMockRequest(m.Spec.HTTPReq, m.Noise)
	report.NextSteps = notOwnedNextSteps(m.Name)
	if call.Unrouted() == models.UnroutedUnstaged {
		report.NextSteps = notOwnedMappingEdit(m.Name)
	}
}

// notOwnedMappingEdit says what a miss on a call that is a recording the open
// view does not show means under keploy mock replay, which marks no set and
// routes none: the test framework's begin and end set each test's window,
// and a call made outside its recording's window is a mapping edit the
// reader makes (worker:), as keploy mock replay's end says it.
func notOwnedMappingEdit(name string) string {
	return fmt.Sprintf("This request is %s, which the test set's mappings.yaml lists where this replay did not make it: for another test, whose window the call was recorded in, or in worker: from a place this replay had not reached. keploy mock does not decide whose a call is: each test's begin and end set its window. If the call is the application's own work (a queue consumer, a scheduler), move %s to worker: in mappings.yaml, which answers it in any test's window or between tests and requires it of none; if it is the test's own, the test made it outside the window it was recorded in.", name, name)
}

// notOwnedNextSteps says what a miss on a call that is a recording the open
// view does not show (notOwnedRecording) likely means, and what to do.
func notOwnedNextSteps(name string) string {
	return fmt.Sprintf("This request is %s, which keploy recorded where this replay did not make it: in another test's window, or between tests after a test this replay had not reached. A call the application makes on its own (a queue consumer, a scheduler) falls in whichever window its timing puts it in. keploy record's judging reads this miss and moves %s to the test set's worker scope (mappings.yaml worker:), which answers it wherever the replay makes it from its place on; record again if this replay was not keploy record's.", name, name)
}
