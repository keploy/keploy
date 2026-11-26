package replay

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// runFailedReason is the stop reason when every test set ran to completion
// without halting early, but one or more test sets failed. Never
// runCompletedReason ("replay completed successfully").
const runFailedReason = "replay finished with test failures"

// stopReasonInput carries the post-loop verdict state so finalStopReason
// names the real cause whenever a run does not succeed and never blames tests
// when no test failed (e.g. unread evidence or earlier-set losses).
type stopReasonInput struct {
	testRunResult    bool
	verifiedNothing  bool
	failedTests      int
	notJudgedSets    []string
	hasEarlierCalls  bool
	hasEarlierLosses bool
	keepAliveErr     *models.AppError
}

// finalStopReason settles the run's stop reason after all sets and post-loop
// verdict checks have run: a failed or unjudged run never logs "replay
// completed successfully", and never blames "test failures" when no test failed.
func finalStopReason(stopReason string, in stopReasonInput) string {
	if stopReason != runCompletedReason {
		return stopReason
	}
	if in.keepAliveErr != nil {
		return fmt.Sprintf("user application failed under --keep-app-alive: %v", *in.keepAliveErr)
	}
	if in.verifiedNothing {
		return "replay finished with no tests verified"
	}
	if in.testRunResult {
		return runCompletedReason
	}
	if in.failedTests > 0 {
		return runFailedReason
	}
	if len(in.notJudgedSets) > 0 {
		sets := append([]string(nil), in.notJudgedSets...)
		sort.Strings(sets)
		return fmt.Sprintf("replay finished: test-set %s not judged (keploy could not read all evidence)", strings.Join(sets, ", "))
	}
	if in.hasEarlierLosses {
		return "replay finished: an earlier test-set was not judged (capture losses found after it ended)"
	}
	if in.hasEarlierCalls {
		return "replay finished: an earlier test-set had unmatched calls decided after it ended"
	}
	return "replay finished: one or more test-sets did not pass"
}

type wholeSetCallSig struct {
	protocol string
	method   string
	path     string
}

type wholeSetEntry struct {
	name       string
	target     string
	recordedAt time.Time
}

type recordedTestWindow struct {
	name  string
	start time.Time
	end   time.Time
}

// wholeSetMockIndex searches the ENTIRE test set's recorded mocks (not just
// the current test window's compared subset) so miss next steps and failure
// blame lines can definitively name the test and mock that recorded a missed
// call. The signature index and window table are built lazily via sync.Once on
// the first miss so clean runs pay zero cost and OnDiskView summaries are read
// at most once per mock.
type wholeSetMockIndex struct {
	lookup    map[string]mockDisplayInfo
	owners    map[string][]string
	testCases []*models.TestCase

	once    sync.Once
	bySig   map[wholeSetCallSig][]wholeSetEntry
	windows []recordedTestWindow
	// windowOwner caches the test window that recorded each mock (computed
	// once inside sync.Once) so owner lookups never rescan all test cases.
	windowOwner map[string]string
}

func newWholeSetMockIndex(
	mockLookup map[string]mockDisplayInfo,
	expectedMappings map[string][]models.MockEntry,
	exactOwners map[string][]string,
	testCases []*models.TestCase,
) *wholeSetMockIndex {
	owners := map[string][]string{}
	addOwner := func(mockName, testName string) {
		if mockName == "" || testName == "" {
			return
		}
		if !slices.Contains(owners[mockName], testName) {
			owners[mockName] = append(owners[mockName], testName)
		}
	}
	for testName, entries := range expectedMappings {
		for _, e := range entries {
			addOwner(e.Name, testName)
		}
	}
	for mockName, tests := range exactOwners {
		for _, testName := range tests {
			addOwner(mockName, testName)
		}
	}
	for _, tests := range owners {
		sort.Strings(tests)
	}
	return &wholeSetMockIndex{
		lookup:    mockLookup,
		owners:    owners,
		testCases: testCases,
	}
}

func (idx *wholeSetMockIndex) ensureBuilt() {
	idx.once.Do(func() {
		idx.windows = make([]recordedTestWindow, 0, len(idx.testCases))
		for _, tc := range idx.testCases {
			if tc == nil || tc.Name == "" {
				continue
			}
			reqAt, respAt := recordReqResTimestamps(tc)
			if reqAt.IsZero() {
				continue
			}
			if respAt.IsZero() {
				respAt = reqAt
			}
			idx.windows = append(idx.windows, recordedTestWindow{name: tc.Name, start: reqAt, end: respAt})
		}

		sorted := make([]string, 0, len(idx.lookup))
		for name := range idx.lookup {
			sorted = append(sorted, name)
		}
		sort.Strings(sorted)

		idx.bySig = make(map[wholeSetCallSig][]wholeSetEntry, len(sorted))
		idx.windowOwner = make(map[string]string, len(sorted))

		for _, name := range sorted {
			info := idx.lookup[name]
			at := info.recordedAtOf()
			if !at.IsZero() {
				for _, w := range idx.windows {
					if !at.Before(w.start) && !at.After(w.end) {
						idx.windowOwner[name] = w.name
						break
					}
				}
			}
			method, path, okSig := normalizeCallSignature(info.protocol, info.summaryOf())
			if !okSig {
				continue
			}
			sig := wholeSetCallSig{
				protocol: strings.ToLower(strings.TrimSpace(info.protocol)),
				method:   method,
				path:     path,
			}
			idx.bySig[sig] = append(idx.bySig[sig], wholeSetEntry{
				name:       name,
				target:     strings.ToLower(strings.TrimSpace(info.targetOf())),
				recordedAt: at,
			})
		}
	})
}

// isOwnedBy reports whether mockName belongs to testName (by explicit mapping /
// exact plan ownership, or by recorded timestamp window when no explicit
// mapping exists).
func (idx *wholeSetMockIndex) isOwnedBy(mockName, testName string) bool {
	if idx == nil || mockName == "" || testName == "" {
		return false
	}
	if slices.Contains(idx.owners[mockName], testName) {
		return true
	}
	idx.ensureBuilt()
	return idx.windowOwner[mockName] == testName
}

// hasAnyOwner reports whether any test in the set owns mockName (either by
// mapping/plan ownership or by recorded timestamp window).
func (idx *wholeSetMockIndex) hasAnyOwner(mockName string) bool {
	if idx == nil || mockName == "" {
		return false
	}
	if len(idx.owners[mockName]) > 0 {
		return true
	}
	idx.ensureBuilt()
	return idx.windowOwner[mockName] != ""
}

// otherOwnerOf returns a test other than currentTest that owns mockName, or ""
// when only currentTest or no test owns it.
func (idx *wholeSetMockIndex) otherOwnerOf(mockName, currentTest string) string {
	if idx == nil || mockName == "" {
		return ""
	}
	for _, t := range idx.owners[mockName] {
		if t != currentTest {
			return t
		}
	}
	idx.ensureBuilt()
	if w := idx.windowOwner[mockName]; w != "" && w != currentTest {
		return w
	}
	return ""
}

// normalizeCallSignature normalizes a call summary such as
// "POST http://auth.internal:8080/v3/oauth/token?x=1" or "POST /v3/oauth/token"
// into ("POST", "/v3/oauth/token") for cross-set matching.
func normalizeCallSignature(protocol, summary string) (method, path string, ok bool) {
	s := strings.TrimSpace(summary)
	if s == "" {
		return "", "", false
	}
	if !strings.EqualFold(protocol, "HTTP") && !strings.EqualFold(protocol, string(models.HTTP)) {
		return strings.ToLower(strings.TrimSpace(protocol)), s, true
	}
	parts := strings.Fields(s)
	if len(parts) < 2 {
		return "", "", false
	}
	method = strings.ToUpper(parts[0])
	rawTarget := parts[1]
	if strings.Contains(rawTarget, "://") {
		if u, err := url.Parse(rawTarget); err == nil && u.Path != "" {
			rawTarget = u.Path
		}
	}
	if i := strings.IndexByte(rawTarget, '?'); i >= 0 {
		rawTarget = rawTarget[:i]
	}
	if rawTarget == "" {
		rawTarget = "/"
	}
	return method, rawTarget, true
}

// lookupCall searches the whole mock set for a recorded mock matching call
// outside currentTest's own recordings. outsideAnyTest is true ONLY when the
// matching mock has no owner test and falls in no test's recorded window.
// exactOwner is true ONLY when the matcher already supplied real owner evidence
// (RecordedIn or NotOwned+ClosestMock); heuristic endpoint-signature matches
// return exactOwner=false so callers never state them as fact or overwrite
// ClosestMock/RecordedIn.
func (idx *wholeSetMockIndex) lookupCall(call models.UnmatchedCall, currentTest string) (mockName, recordedIn string, outsideAnyTest, exactOwner, ok bool) {
	if idx == nil {
		return "", "", false, false, false
	}
	if call.RecordedIn != "" {
		return call.ClosestMock, call.RecordedIn, false, true, true
	}
	if call.NotOwned && call.ClosestMock != "" {
		other := idx.otherOwnerOf(call.ClosestMock, currentTest)
		return call.ClosestMock, other, other == "" && !idx.hasAnyOwner(call.ClosestMock), true, true
	}
	wantMethod, wantPath, valid := normalizeCallSignature(call.Protocol, call.ActualSummary)
	if !valid {
		return "", "", false, false, false
	}
	idx.ensureBuilt()
	sig := wholeSetCallSig{
		protocol: strings.ToLower(strings.TrimSpace(call.Protocol)),
		method:   wantMethod,
		path:     wantPath,
	}
	entries := idx.bySig[sig]
	if len(entries) == 0 {
		return "", "", false, false, false
	}
	wantDest := strings.ToLower(strings.TrimSpace(call.Destination))

	var fallbackMock, fallbackOwner string
	var fallbackOutside bool
	for _, e := range entries {
		otherOwner := idx.otherOwnerOf(e.name, currentTest)
		// Never attribute currentTest's own mock as recorded in another test
		// or as recorded outside any test window.
		if otherOwner == "" && idx.isOwnedBy(e.name, currentTest) {
			continue
		}
		outside := otherOwner == "" && !idx.hasAnyOwner(e.name)
		destMatches := wantDest == "" || e.target == wantDest || strings.Contains(e.target, wantDest)
		if destMatches {
			if otherOwner != "" {
				return e.name, otherOwner, false, false, true
			}
			if fallbackMock == "" {
				fallbackMock, fallbackOwner, fallbackOutside = e.name, otherOwner, outside
			}
			continue
		}
		if fallbackMock == "" && otherOwner != "" {
			fallbackMock, fallbackOwner, fallbackOutside = e.name, otherOwner, false
		}
	}
	if fallbackMock != "" {
		return fallbackMock, fallbackOwner, fallbackOutside, false, true
	}
	return "", "", false, false, false
}

// isOwnPayloadDiff reports whether call is a payload/header diff against a
// candidate mock rather than a cross-test missing-candidate call. A call
// whose closest mock is on another route (ClosestOnAnotherRoute) is none,
// whoever owns that mock: the nearest other route is often one of the
// test's own, and the lookup never names the test's own mocks. A call whose
// destination is what differs (a host or container diff, or out of scope) is
// left as it was: the lookup's fallback can name a recording of its path on
// another upstream, which says nothing of a call whose host changed.
func (idx *wholeSetMockIndex) isOwnPayloadDiff(c models.UnmatchedCall, currentTest string) bool {
	if c.ClosestOnAnotherRoute && !diffsNameADestination(c.FieldDiffs) && c.DestinationScope != models.DestinationScopeNotInComparedSet {
		return false
	}
	if len(c.FieldDiffs) > 0 || len(c.Differs) > 0 || c.MatchPhase == models.MatchPhaseBody {
		return true
	}
	if !c.NotOwned && c.ClosestMock != "" && idx.isOwnedBy(c.ClosestMock, currentTest) {
		return true
	}
	return false
}

// enrichCalls resolves each counting unmatched call against the whole mock set
// so NextSteps names where a matching call or endpoint was recorded across the
// set, skipping payload diffs against mocks already owned by currentTest.
// ClosestMock is never overwritten, and RecordedIn is populated strictly from
// real owner evidence (exactOwner == true).
func (idx *wholeSetMockIndex) enrichCalls(calls []models.UnmatchedCall, currentTest string) {
	if idx == nil || len(calls) == 0 {
		return
	}
	for i := range calls {
		c := &calls[i]
		if !c.CountsAgainstTest() {
			continue
		}
		if idx.isOwnPayloadDiff(*c, currentTest) {
			continue
		}
		mockName, recordedIn, outsideAnyTest, exactOwner, found := idx.lookupCall(*c, currentTest)
		if !found {
			continue
		}
		if exactOwner && recordedIn != "" && c.RecordedIn == "" {
			c.RecordedIn = recordedIn
		}
		c.NextSteps = formatWholeSetNextStep(c.NextSteps, mockName, recordedIn, outsideAnyTest, exactOwner)
		if c.Whole != nil {
			w := *c.Whole
			w.NextStep = c.NextSteps
			c.Whole = &w
		}
	}
}

func formatWholeSetNextStep(existing, mockName, recordedIn string, outsideAnyTest, exactOwner bool) string {
	lead := "A call to the same endpoint was recorded in"
	if exactOwner {
		lead = "This call was recorded in"
	}
	var recordedClause string
	switch {
	case recordedIn != "" && mockName != "":
		recordedClause = fmt.Sprintf("%s %s (%s).", lead, recordedIn, mockName)
	case recordedIn != "":
		recordedClause = fmt.Sprintf("%s %s.", lead, recordedIn)
	case mockName != "" && outsideAnyTest:
		recordedClause = fmt.Sprintf("%s %s (outside any test window).", lead, mockName)
	case mockName != "":
		recordedClause = fmt.Sprintf("%s %s.", lead, mockName)
	default:
		return existing
	}

	const condAnother = "If another test recorded this call and every test may make it"
	if strings.Contains(existing, condAnother) {
		return strings.Replace(existing, condAnother, recordedClause+" If every test may make it", 1)
	}
	const condOutside = "If the application made this call outside every test"
	if recordedIn != "" && strings.Contains(existing, condOutside) {
		return strings.Replace(existing, condOutside, recordedClause+" If every test may make it", 1)
	}
	if recordedIn != "" && strings.Contains(existing, recordedIn) && (mockName == "" || strings.Contains(existing, mockName)) {
		return existing
	}
	if strings.TrimSpace(existing) == "" {
		return recordedClause
	}
	return recordedClause + " " + existing
}

// formatVetoedFailureMessage builds the per-test failure log line that leads
// with the real cause (response status change and any unmatched outgoing call
// with where it was recorded), followed by the mapping divergence note.
func formatVetoedFailureMessage(testCase *models.TestCase, resp any, calls []models.UnmatchedCall) string {
	var b strings.Builder
	switch {
	case testCase != nil && testCase.Kind == models.HTTP:
		if httpResp, ok := resp.(*models.HTTPResp); ok && httpResp != nil {
			if testCase.HTTPResp.StatusCode != httpResp.StatusCode {
				fmt.Fprintf(&b, "response %d -> %d", testCase.HTTPResp.StatusCode, httpResp.StatusCode)
			} else {
				fmt.Fprintf(&b, "response differed (status %d)", httpResp.StatusCode)
			}
		} else {
			b.WriteString("response differed")
		}
	default:
		b.WriteString("response differed")
	}

	var first *models.UnmatchedCall
	counting := 0
	for i := range calls {
		if !calls[i].CountsAgainstTest() {
			continue
		}
		counting++
		if first == nil {
			first = &calls[i]
		}
	}
	if first != nil {
		fmt.Fprintf(&b, "; the call %s got no recorded answer", models.UnmatchedCallLogName(*first))
		switch {
		case first.RecordedIn != "":
			fmt.Fprintf(&b, " (recorded in %s)", first.RecordedIn)
		case first.ClosestMock != "":
			fmt.Fprintf(&b, " (closest mock %s)", first.ClosestMock)
		}
		if counting > 1 {
			fmt.Fprintf(&b, " (+%d more)", counting-1)
		}
	}
	b.WriteString("; mock mapping diverged (kept FAILED)")
	return b.String()
}

// failedMismatchNextStep derives the end-of-run next_step for a failed run
// from the actual unmatched calls and mock differences recorded during the run
// rather than printing a static noise-only hint.
func failedMismatchNextStep(failures []TestFailure) string {
	var hasRecordedElsewhere, hasUnmatchedNoCandidate, hasPayloadDrift bool
	for _, f := range failures {
		if f.FailureReason != models.ErrMockNotFound || f.MismatchReport == nil {
			continue
		}
		r := f.MismatchReport
		if !r.CountsAgainstTest() {
			continue
		}
		if r.RecordedIn != "" || r.NotOwned {
			hasRecordedElsewhere = true
		}
		// Field diffs against another route's recording
		// (ClosestOnAnotherRoute) or another upstream's (out of scope) are
		// no drift of the call's own: the call got no recorded answer.
		if !r.ClosestOnAnotherRoute && r.DestinationScope != models.DestinationScopeNotInComparedSet &&
			(len(r.FieldDiffs) > 0 || len(r.Differs) > 0 ||
				r.MatchPhase == models.MatchPhaseBody ||
				strings.Contains(r.MatchPhase, "body") ||
				strings.Contains(r.MatchPhase, "header")) {
			hasPayloadDrift = true
		} else if r.MatchPhase == models.MatchPhaseSchema ||
			r.MatchPhase == models.MatchPhaseNoMocks ||
			r.DestinationScope == models.DestinationScopeNotInComparedSet ||
			r.MatchPhase == models.MatchPhaseExhausted {
			hasUnmatchedNoCandidate = true
		}
	}

	switch {
	case hasRecordedElsewhere && hasPayloadDrift:
		return "Review the mismatch summary below: declare shared calls recorded in another test under sessionCalls in keploy.yaml, and add drifting dynamic fields as noise (test.globalNoise) or re-record the test-set with 'keploy record'."
	case hasRecordedElsewhere:
		return "One or more unmatched calls were recorded in another test (see the summary below). If every test may make those calls (for example auth tokens or shared config reads), declare them under sessionCalls in keploy.yaml, or re-record the test-set with 'keploy record'."
	case hasUnmatchedNoCandidate && !hasPayloadDrift:
		return "One or more outgoing calls got no recorded answer (see the summary below). Declare shared calls (such as auth tokens or config reads) under sessionCalls in keploy.yaml, or re-record the test-set with 'keploy record'."
	case hasPayloadDrift:
		return "Add drifting dynamic fields as noise (test.globalNoise); if the request structure changed, re-record the test-set with 'keploy record'."
	default:
		return "A failed test keeps its recorded mapping (--update-test-mapping updates only a passing test's); to remap tests whose mapping is stale, run with --disable-mapping --update-test-mapping, or re-record the test-set with 'keploy record'."
	}
}
