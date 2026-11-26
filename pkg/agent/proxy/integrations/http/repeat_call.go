package http

import (
	"context"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// repeatOfAnswered is, for a request no mock answered, the per-test recording
// of an earlier test of the set that already answered an identical request at
// this replay (integrations.AnsweredFinder), with when it answered; ok is
// false when there is none, when the store keeps no such history, and when
// the open test has a recording of its own under the call's request keys
// that it has not consumed (ownRecordingUnder): its diff is the miss's
// explanation then.
//
// Identical is the request as the strict comparison reads it (strictDiff),
// configured and learned noise applied, headers aside but for the
// X-Amz-Target, which names an AWS call's action: the method, destination,
// path, query and body. A header such as a trace id or a date differs
// between two calls an application makes for the same answer, and is no
// evidence that the answer it wanted differs.
func repeatOfAnswered(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise) (integrations.AnsweredCall, bool) {
	f, ok := mockDb.(integrations.AnsweredFinder)
	if !ok {
		return integrations.AnsweredCall{}, false
	}
	live := liveDestination(ctx, input)
	match := func(m *models.Mock) bool {
		return m.Spec.HTTPReq != nil && sameRequest(strictDiff(ctx, input, live, m, n))
	}
	path := ""
	if input.url != nil {
		path = input.url.Path
	}
	target := strings.Join(input.header.Values(amzTargetHeader), ",")
	keys := RequestKeysOf(input.method, path, target, n.loose())
	if ownRecordingUnder(mockDb, keys) {
		return integrations.AnsweredCall{}, false
	}
	return f.FindAnsweredEarlier(models.HTTP, keys, match)
}

// repeatNoteOf is the earlier answered recording a call's repeat note names
// (repeatOfAnswered), for a missed call and a paired one alike: none for a
// call one more than the open test recorded (extra, extraCall's count, above
// 0). That call repeats the test's own recorded call, which already answered
// it, so an earlier test's identical recording does not say why the
// application asked, and a note naming it would claim the test recorded no
// such call.
func repeatNoteOf(ctx context.Context, input *req, db integrations.MockMemDb, n strictNoise, extra int) (integrations.AnsweredCall, bool) {
	if extra > 0 {
		return integrations.AnsweredCall{}, false
	}
	return repeatOfAnswered(ctx, input, db, n)
}

// ownRecordingUnder reports whether the open test has a recording of its own
// under any of keys (models.RequestKeys) that no call of it consumed yet: a
// recording its call may be, changed. The miss then names that recording as
// its closest, with what differs, and gets no repeat note: an earlier test's
// identical recording does not explain a request the application changed, and
// a note there would hide the change behind a cache. A store whose per-test
// read fails counts as having one: no note is the side that hides nothing.
//
// A store that walks its per-test tier by key (integrations.PerTestKeyRanger)
// is read under each of keys (ownKeyIndexes), up to the first such recording,
// so a miss costs the same whatever the test or the set holds; any other, and
// one whose keyed walk fails, is read whole (ownRecordingInRead).
func ownRecordingUnder(mockDb integrations.MockMemDb, keys []string) bool {
	pr, ok := mockDb.(integrations.PerTestKeyRanger)
	if !ok {
		return ownRecordingInRead(mockDb, keys)
	}
	ix := ownKeyIndexes[0]
	if provenswitch.On() {
		ix = ownKeyIndexes[1]
	}
	// What the whole read keeps (ownRecordingInRead): not spent, and the
	// open test's own; in a store that does not tell, every recording of
	// its per-test read is (openTestOwner).
	spent, _ := mockDb.(integrations.SpentReader)
	owner, _ := mockDb.(integrations.TestOwner)
	found := false
	for _, key := range keys {
		err := pr.RangePerTestMocksWithKey(ix, key, func(m *models.Mock) bool {
			if (spent != nil && spent.Spent(m)) || (owner != nil && !owner.OwnedByOpenTest(m)) {
				return true
			}
			found = true
			return false
		})
		if found {
			return true
		}
		if err != nil {
			// A read that fails is the whole read's to answer.
			return ownRecordingInRead(mockDb, keys)
		}
	}
	return false
}

// ownKeyIndexes file the HTTP recordings of a store's per-test tier under
// their request keys (recordedRequestKeys), one index for each setting of
// the proven switch, [0] off and [1] on: the keys of a recording whose learn
// step read the application's clock in its path differ between the two
// (requestKeysIn). A call reads the index of the setting it runs under, so
// what an index files depends on the recording alone (integrations.MockIndex).
var ownKeyIndexes = [2]*integrations.MockIndex{
	{Keys: func(m *models.Mock) []string { return httpRequestKeysIn(m, false) }},
	{Keys: func(m *models.Mock) []string { return httpRequestKeysIn(m, true) }},
}

// httpRequestKeysIn is requestKeysIn of an HTTP recording; none for a mock of
// another kind, which the whole read leaves out (FilterHTTPMocks).
func httpRequestKeysIn(m *models.Mock, proven bool) []string {
	if m == nil || m.Kind != models.HTTP {
		return nil
	}
	return requestKeysIn(m, proven)
}

// ownRecordingInRead is ownRecordingUnder of a whole per-test read.
func ownRecordingInRead(mockDb integrations.MockMemDb, keys []string) bool {
	perTest, err := mockDb.GetPerTestMocksInWindow()
	if err != nil {
		return true
	}
	perTest = integrations.WithoutSpent(mockDb, perTest)
	owned := openTestOwner(mockDb, perTest)
	for _, m := range FilterHTTPMocks(perTest) {
		if m == nil || !owned(m) {
			continue
		}
		for _, k := range recordedRequestKeys(m) {
			if slices.Contains(keys, k) {
				return true
			}
		}
	}
	return false
}

// sameRequest reports whether a strict comparison's differences leave the
// request identical (repeatOfAnswered): none, or only in headers other than
// the X-Amz-Target.
func sameRequest(diffs []strictField) bool {
	target := "header." + strings.ToLower(amzTargetHeader)
	for _, d := range diffs {
		p := strings.ToLower(d.diff.Path)
		if !strings.HasPrefix(p, "header.") || p == target {
			return false
		}
	}
	return true
}

// markRepeat makes report the miss of a call identical to a, a recording an
// earlier test made that already answered at this replay: a is its closest
// mock, with no field named as differing, and the note (models.RepeatNote)
// says which recording and how long after its answer this call came. A call
// that is also a recording made elsewhere (report.NotOwned, markNotOwned)
// keeps that recording as its closest, the reason it missed, and its next
// step names both (models.RepeatNote.ExplainMiss). The replay measures the
// gaps at record and the cause they support at the set's end (markRepeatOf),
// from the set's recordings, which the agent does not hold.
//
// outside says a is a recording made outside every test
// (models.RepeatNote.OutsideTests), as the store's scope table tells it
// (madeOutsideTests) or the store's finder does (AnsweredCall.OutsideTests:
// a plain startup: recording, which no scope table need name), and hint is
// the miss's word on serving the call to its test (sessionCallHint), which
// the note keeps so that the replay's rewrite of the next step
// (markRepeatOf) keeps it too.
func markRepeat(report *models.MockMismatchReport, a integrations.AnsweredCall, outside bool, hint string) {
	m := a.Mock
	if !report.NotOwned {
		report.ClosestMock = m.Name
		report.FieldDiffs, report.Differs, report.FieldReasons, report.Diff = nil, nil, nil, ""
		report.ClosestOnAnotherRoute = false // m is this call, answered earlier
		report.ClosestMockReq = renderMockRequest(m.Spec.HTTPReq, m.Noise)
	}
	report.Repeat = models.NewRepeatNote(m.Name, a.AnsweredAt, m.Spec.ReqTimestampMock, report.At)
	report.Repeat.Background, report.Repeat.SkippedRead = a.Background, a.SkippedRead
	report.Repeat.OutsideTests = (outside || a.OutsideTests) && a.Background == "" && !a.SkippedRead
	report.Repeat.Hint = hint
	report.NextSteps = report.Repeat.ExplainMiss(report.NotOwned, report.ClosestMock, "", mismatch.RecordCommand())
}

// madeOutsideTests reports whether db says a, an earlier answered recording,
// is one made outside every test: one no test lists, by keploy mock replay's
// scope table (integrations.InPool), known only once the replay sent it.
// Under the proven switch alone: without it a test is served such a
// recording as it is between tests, so the note's word on why it was not
// (models.RepeatNote.OutsideTests) would not hold.
func madeOutsideTests(db integrations.MockMemDb, a integrations.AnsweredCall) bool {
	if a.Mock == nil || !provenswitch.On() {
		return false
	}
	pool, known := integrations.InPool(db, a.Mock)
	return pool && known
}
