package http

import (
	"context"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// earlierRecording is, under exact serving, for a request no mock answered
// that is no recording made elsewhere (notOwnedRecording named none) and no
// call more than its test recorded (extraCall), the recording the miss names
// as Earlier (models.MockMismatchReport.Earlier): the latest one another
// test made of the request's method and path, to its destination, before
// the earliest of the open test's own recordings of them that has a request
// time. Test windows follow one another, so that is an earlier test's
// window, which the replay passed without making the call there: when that
// test was skipped, the test's own call likely changed because the earlier
// one's write is missing (a check-and-set version, a row's state), which
// judging reads.
//
// nil when the test recorded no such call (the request is not one of its
// own), when keploy cannot place the test's own (none has a request time),
// when no other test recorded one before them, or when the replay staged
// another set while the call was decided (restaged). A recording with no
// request time is never named: keploy cannot place it.
//
// It reads the staged recordings the open view does not show, as
// notOwnedRecording does (integrations.StagedKeyFinder, StagedFinder), so
// one an earlier call consumed (its test made the call at this replay) is
// never named. It claims none: its match keeps the latest it is shown and
// accepts none, so the store spends no name on it, and a later miss that is
// such a recording still names it (notOwnedRecording). ctx carries the
// request's destination as it came (liveDestination).
func earlierRecording(ctx context.Context, input *req, db integrations.MockMemDb, n strictNoise, call integrations.CallRound) *models.Mock {
	if input.url == nil {
		return nil
	}
	live, path := liveDestination(ctx, input), input.url.Path
	same := func(m *models.Mock) bool {
		if m == nil || m.Spec.HTTPReq == nil || string(m.Spec.HTTPReq.Method) != input.method {
			return false
		}
		p, _ := recordedURL(m.Spec.HTTPReq)
		return p == path && sameDestination(live, recordedDestination(m))
	}
	var first time.Time
	for _, m := range ownHTTPRecordings(db) {
		if at := m.Spec.ReqTimestampMock; same(m) && !at.IsZero() && (first.IsZero() || at.Before(first)) {
			first = at
		}
	}
	if first.IsZero() {
		return nil
	}
	var latest *models.Mock
	keep := func(m *models.Mock) bool {
		if at := m.Spec.ReqTimestampMock; same(m) && !at.IsZero() && at.Before(first) && (latest == nil || at.After(latest.Spec.ReqTimestampMock)) {
			latest = m
		}
		return false
	}
	restaged := false
	switch f := db.(type) {
	case integrations.StagedKeyFinder:
		target := strings.Join(input.header.Values(amzTargetHeader), ",")
		_, restaged = f.FindStagedNotOwnedByKeys(models.HTTP, call, RequestKeysOf(input.method, path, target, n.loose()), keep)
	case integrations.StagedFinder:
		_, restaged = f.FindStagedNotOwned(models.HTTP, call, keep)
	}
	if restaged {
		return nil
	}
	return latest
}

// ownHTTPRecordings is the open test's own HTTP recordings, answered
// (integrations.ExactConsumedReader) or not (the per-test read, of which the
// open test's own, openTestOwner). None when the per-test read fails.
func ownHTTPRecordings(db integrations.MockMemDb) []*models.Mock {
	perTest, err := db.GetPerTestMocksInWindow()
	if err != nil {
		return nil
	}
	owned := openTestOwner(db, perTest)
	var out []*models.Mock
	for _, m := range FilterHTTPMocks(perTest) {
		if m != nil && owned(m) {
			out = append(out, m)
		}
	}
	if er, ok := db.(integrations.ExactConsumedReader); ok {
		out = append(out, FilterHTTPMocks(er.ConsumedTestMocks())...)
	}
	return out
}
