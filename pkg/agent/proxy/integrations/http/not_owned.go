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

// callRequestKeys are the request keys a live call asks the agent's disk
// store by (RequestKeysOf), under keploy.yml's noise n.
func callRequestKeys(input *req, n strictNoise) []string {
	path := ""
	if input.url != nil {
		path = input.url.Path
	}
	target := strings.Join(input.header.Values(amzTargetHeader), ",")
	return RequestKeysOf(input.method, path, target, n.loose())
}

// elsewhereRecording is, under exact serving (the proven switch), the staged
// recording the open view does not show that the request is, exactly
// (strictSame, under keploy.yml's noise n): another test's, any test's with
// no test open, or a worker-scope one whose place the run has not reached;
// whether or not a call of the set consumed it or a miss named it
// (integrations.FindStagedElsewhere). The request was made where it was not
// recorded, and did not change. Record's full round asks it of a request it
// would pair with a recording of its scope it drifted from, before pairing
// it; a miss's report asks it of any other request no mock answered.
//
// notOwned: no call consumed it and no miss named it, and it is named for
// this request now: record's judging reads the miss to bake the recording
// into the worker scope (design v14 S12, a2). Otherwise the request is that
// recording once more than it holds. nil for none, or when the store keeps
// no staged mocks to look in. Only the recordings whose digest may be the
// request's are compared (requestDigest). ctx carries the request's
// destination as it came (liveDestination).
//
// err: keploy could not look in all the recordings it had to (the store
// could not read one back), so whether the request is a recording made
// elsewhere is unknown; rec is nil, and nothing is named for it. A caller
// pairs, serves, learns and names nothing for such a request: it is a miss
// whose report says keploy could not look (markElsewhereUnread).
func elsewhereRecording(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise) (rec *models.Mock, notOwned bool, err error) {
	live := liveDestination(ctx, input)
	digest := liveRequestDigest(input, n)
	matched := map[string]*models.Mock{}
	q := integrations.ElsewhereQuery{
		Keys:     callRequestKeys(input, n),
		Digest:   func(m *models.Mock) any { return recordedRequestDigest(m, n) },
		DigestOf: requestDigestOf(n),
		RulesOut: func(d any) bool {
			rd, ok := d.(requestDigest)
			return ok && rd.rulesOut(digest)
		},
		Match: func(m *models.Mock) bool {
			if !strictSame(input, live, m, n) {
				return false
			}
			matched[m.Name] = m
			return true
		},
	}
	name, notOwned, err := integrations.FindStagedElsewhere(mockDb, models.HTTP, q)
	if err != nil {
		return nil, false, err
	}
	if name == "" {
		return nil, false, nil
	}
	return matched[name], notOwned, nil
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
// open view does not show that no call consumed and no miss named before
// (elsewhereRecording, notOwned): m is its closest mock, with no field named
// as differing, since none does (no field diff, nor the one-line diff the
// report kept of the recording it was read against first), and its rendered
// request is m's.
func markNotOwned(report *models.MockMismatchReport, m *models.Mock) {
	report.ClosestMock, report.NotOwned = m.Name, true
	report.FieldDiffs, report.Differs, report.Diff = nil, nil, ""
	report.ClosestMockReq = renderMockRequest(m.Spec.HTTPReq, m.Noise)
	report.NextSteps = notOwnedNextSteps(m.Name)
}

// markMadeAgain makes report the miss of a call that is m, a recording the
// open view does not show, once more than m holds: a call of the set
// consumed m, or an earlier miss was named as it (elsewhereRecording). m is
// its closest mock, with no field named as differing, since none does (as
// markNotOwned), and its rendered request is m's; it is not named as not
// owned, since m was named, or answered, already.
func markMadeAgain(report *models.MockMismatchReport, m *models.Mock) {
	report.ClosestMock, report.NotOwned = m.Name, false
	report.FieldDiffs, report.Differs, report.Diff = nil, nil, ""
	report.ClosestMockReq = renderMockRequest(m.Spec.HTTPReq, m.Noise)
	report.NextSteps = madeAgainNextSteps(m.Name)
}

// markElsewhereUnread makes report the miss of a call keploy could not tell
// from a recording made elsewhere: it could not read all the test set's
// recordings it had to look in (elsewhereRecording's err). It stands in for
// the learn step's explanation (learnMiss.explain, never called for such a
// miss) and for the recording made elsewhere a lookup would name. What the
// call is is unknown, so no learn-step reason stands for it, nor does any
// field said to differ from the open test's closest recording, which a
// reader would take for noise to add: the call may be another test's, which
// differs from none of it. report.Unread says so, and the proxy's read of
// the test's misses is then incomplete (models.ErrUnmatchedCallsUnread), so
// the test has no verdict and its set is not judged.
func markElsewhereUnread(report *models.MockMismatchReport, err error) {
	report.FieldDiffs, report.Differs, report.Diff = nil, nil, ""
	report.Unread = "keploy could not tell whether this call is a recording made elsewhere: " + err.Error()
	report.NextSteps = elsewhereUnreadNextSteps
}

// elsewhereUnreadNextSteps says what a miss on a call keploy could not tell
// from a recording made elsewhere (markElsewhereUnread) means.
const elsewhereUnreadNextSteps = "keploy could not read all the test set's recordings it had to look in to tell whether this request is one recorded elsewhere (in another test's window, or between tests), so it did not answer it, learned nothing from it, and this test has no verdict. Check the agent's log for why a recording kept on disk could not be read (a full or failing disk, the set's staging ending under load), then run the test set again."

// madeAgainNextSteps says what a miss on a call that is a recording the open
// view does not show, made once more than it holds (markMadeAgain), means.
func madeAgainNextSteps(name string) string {
	return fmt.Sprintf("This request is %s again: keploy recorded %s once, outside this test (in another test's window, or between tests), and this replay has already made that call, so no recording is left to answer it here. Nothing is learned from it, nor onto this test's own recordings of the same operation. Either the application makes this call more times than it did when it was recorded, or this test's own request now carries the values %s was recorded with; if that is expected, record the test set again.", name, name, name)
}

// notOwnedNextSteps says what a miss on a call that is a recording the open
// view does not show (markNotOwned) likely means, and what to do.
func notOwnedNextSteps(name string) string {
	return fmt.Sprintf("This request is %s, which keploy recorded where this replay did not make it: in another test's window, or between tests after a test this replay had not reached. A call the application makes on its own (a queue consumer, a scheduler) falls in whichever window its timing puts it in. keploy record's judging reads this miss and moves %s to the test set's worker scope (mappings.yaml worker:), which answers it wherever the replay makes it from its place on; record again if this replay was not keploy record's.", name, name)
}
