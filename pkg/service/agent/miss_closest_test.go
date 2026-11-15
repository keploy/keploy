package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// A failed test's reason names, on each miss's line, the recording the call
// was compared with, and says once that a field it names with no reason of
// its own was compared with its recorded value. Where it names less than
// every call or every field, it says which file holds the whole evidence.
// A judging replay learns nothing, so its misses carry no reason of record's
// full round: before this, such a reason named three fields and no recording.
func TestAFailedTestsReasonNamesTheClosestRecordingAndWhereEveryFieldIs(t *testing.T) {
	reason := func(misses ...models.UnmatchedCall) string {
		return evidenceReason("api::TestJobs", &models.ScopeEvidence{Misses: misses}, nil, time.Time{}, nil)
	}
	whole := missesListedWhole(models.ReplayTestsFile)
	since := "body.since was learned as clock, a read of the application's clock, and the call sends no value there: it changed"
	miss := func(closest string, differs []string, reasons ...models.FieldReason) models.UnmatchedCall {
		return sanitizedMiss(models.UnmatchedCall{Protocol: "MongoDB", Destination: "127.0.0.1:27017", ClosestMock: closest,
			Differs: differs, FieldReasons: reasons})
	}

	// The recording is named; its one field was compared as recorded.
	one := reason(miss("mock-12", []string{"body.amount"}))
	require.Contains(t, one, "a MongoDB call to 127.0.0.1:27017 had no recorded answer (closest recording mock-12: body.amount differs). "+comparedText)
	require.NotContains(t, one, whole, "every field is named: no file is pointed to")

	// No recording named: the line is today's.
	bare := reason(miss("", []string{"body.amount"}))
	require.Contains(t, bare, "had no recorded answer (body.amount differs). "+comparedText)
	require.NotContains(t, bare, "closest recording")

	// A field with a reason of its own: its reason says why, and nothing
	// says it was compared as recorded.
	reasoned := reason(miss("mock-3", []string{"body.since"}, models.FieldReason{Field: "body.since", Why: since}))
	require.Contains(t, reasoned, "had no recorded answer (closest recording mock-3: body.since differs: "+since+").")
	require.NotContains(t, reasoned, comparedText)
	// A reason with no field said to differ is named after its recording too.
	onlyWhy := reason(miss("mock-4", nil, models.FieldReason{Field: "body.since", Why: since}))
	require.Contains(t, onlyWhy, "had no recorded answer (closest recording mock-4: "+since+").")
	require.NotContains(t, onlyWhy, comparedText)
	// One field reasoned, one not: the other was compared as recorded.
	mixed := reason(miss("mock-5", []string{"body.since", "body.q"}, models.FieldReason{Field: "body.since", Why: since}))
	require.Contains(t, mixed, "(closest recording mock-5: body.since and body.q differ: "+since+"). "+comparedText)
	// A miss with neither a field nor a reason says nothing of a recording.
	none := reason(miss("mock-6", nil))
	require.Contains(t, none, "a MongoDB call to 127.0.0.1:27017 had no recorded answer.")
	require.NotContains(t, none, "mock-6")
	require.NotContains(t, none, comparedText)

	// More fields than a line names: the file with every one is named.
	four := reason(miss("mock-7", []string{"body.a", "body.b", "body.c", "body.d"}))
	require.Contains(t, four, "(closest recording mock-7: body.a, body.b, body.c and 1 more differ). "+comparedText+" "+whole)
	three := reason(miss("mock-8", []string{"body.a", "body.b", "body.c"}))
	require.NotContains(t, three, whole)
	require.Equal(t, "Every call that missed, its closest recording and each field it differs in are under this test's evidence.misses in the set's last-replay-tests.yaml.", whole)

	// More calls than the reason lists: the file is named, and a field
	// compared as recorded only on a line left out is not said to be.
	var six []models.UnmatchedCall
	for range 5 {
		six = append(six, miss("mock-9", []string{"body.since"}, models.FieldReason{Field: "body.since", Why: since}))
	}
	six = append(six, miss("mock-10", []string{"body.q"}))
	many := reason(six...)
	require.Contains(t, many, "; and 1 more. "+whole)
	require.NotContains(t, many, comparedText, "the one line with a field compared as recorded is left out")
	require.NotContains(t, many, "mock-10")
	// Five lines: every call is listed, the fifth compared as recorded.
	five := reason(six[1:]...)
	require.Contains(t, five, "(closest recording mock-10: body.q differs). "+comparedText)
	require.NotContains(t, five, whole)
	// What keploy could not capture comes first and takes a line: the fifth
	// miss is then left out, and what it names is not said.
	lost := evidenceReason("api::TestJobs", &models.ScopeEvidence{Misses: six[1:],
		CaptureIncomplete: models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}}}, nil, time.Time{}, nil)
	require.Contains(t, lost, "; and 1 more. "+whole)
	require.NotContains(t, lost, comparedText)

	// A miss record's full round gave a reason for says why by it, not by
	// the compared line.
	round := miss("mock-11", []string{"body.q"})
	round.Reason = "request_changed"
	full := reason(round)
	require.Contains(t, full, "(closest recording mock-11: body.q differs).\n")
	require.NotContains(t, full, comparedText)

	// An extra call is said by its count: no field, no compared line, no file.
	extra := miss("mock-13", []string{"body.a", "body.b", "body.c", "body.d"})
	extra.Kind, extra.Recorded = models.MissExtraCall, 1
	counted := reason(extra)
	require.Contains(t, counted, "was called 2 times, recorded 1")
	require.NotContains(t, counted, comparedText)
	require.NotContains(t, counted, whole)
	// A miss made before the test began, and one made as it ended, name
	// their fields as a miss in the test does.
	began := time.Unix(100, 0)
	early := miss("mock-14", []string{"body.a", "body.b", "body.c", "body.d"})
	early.At = began.Add(-time.Second)
	late := miss("mock-15", []string{"body.a", "body.b", "body.c", "body.d"})
	late.Kind, late.At = models.MissOutsideTest, began.Add(time.Second)
	for _, m := range []models.UnmatchedCall{early, late} {
		r := evidenceReason("api::TestJobs", &models.ScopeEvidence{Misses: []models.UnmatchedCall{m}}, nil, began, nil)
		require.Contains(t, r, "(closest recording "+m.ClosestMock+": body.a, body.b, body.c and 1 more differ). "+comparedText+" "+whole)
	}

	// A record's reason names the record's file, a replay's the replay's.
	ev := &models.ScopeEvidence{Misses: []models.UnmatchedCall{four0()}}
	for mode, file := range map[models.Mode]string{models.MODE_RECORD: models.RecordTestsFile, models.MODE_TEST: models.ReplayTestsFile} {
		a := &Agent{config: &config.Config{}}
		a.config.Agent.Mode = mode
		got := a.evidenceText("api::TestJobs", ev, time.Time{})
		require.Contains(t, got, "in the set's "+file+".", "mode %s", mode)
		require.Equal(t, 1, strings.Count(got, "tests.yaml"), "mode %s", mode)
	}
}

// four0 is a miss with four fields: more than its line names.
func four0() models.UnmatchedCall {
	return sanitizedMiss(models.UnmatchedCall{Protocol: "MongoDB", ClosestMock: "mock-1", Differs: []string{"body.a", "body.b", "body.c", "body.d"}})
}
