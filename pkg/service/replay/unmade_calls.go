package replay

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// unmadeCallReason tags TestFailure rows representing a recorded test-driven
// outgoing call that was not observed during a passing test's window in
// default mode (and was not consumed anywhere else in the test set). Kept
// separate from ErrMockNotFound so GetFailures() and existing mapping/mismatch
// tests remain untouched while PrintOutgoingDiffTable renders both in one
// unified, deduplicated OUTGOING CALL DIFFS table.
const unmadeCallReason = models.ParserErrorType("unmade_recorded_call")

const unmadeCallFieldsText = "not seen in this test's window"

// SetUnmadeOutgoingCalls replaces the recorded-but-unmade outgoing-call rows
// for (testSetID, testID). Calling with an empty slice clears the entry so a
// --retry-passing cycle that recovers leaves no stale row.
func (tfs *TestFailureStore) SetUnmadeOutgoingCalls(testSetID, testID string, rows []TestFailure) {
	if tfs == nil {
		return
	}
	key := [2]string{testSetID, testID}
	tfs.mu.Lock()
	defer tfs.mu.Unlock()
	if len(rows) == 0 {
		delete(tfs.unmadeCalls, key)
		return
	}
	if tfs.unmadeCalls == nil {
		tfs.unmadeCalls = make(map[[2]string][]TestFailure)
	}
	cp := make([]TestFailure, len(rows))
	copy(cp, rows)
	tfs.unmadeCalls[key] = cp
}

// GetUnmadeOutgoingCalls returns all recorded-but-unmade outgoing-call rows in
// deterministic (TestSetID, TestID) order.
func (tfs *TestFailureStore) GetUnmadeOutgoingCalls() []TestFailure {
	if tfs == nil {
		return nil
	}
	tfs.mu.Lock()
	defer tfs.mu.Unlock()
	if len(tfs.unmadeCalls) == 0 {
		return nil
	}
	keys := make([][2]string, 0, len(tfs.unmadeCalls))
	for k := range tfs.unmadeCalls {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	var out []TestFailure
	for _, k := range keys {
		out = append(out, tfs.unmadeCalls[k]...)
	}
	return out
}

// PruneConsumedUnmadeOutgoingCalls removes any unmade outgoing-call rows for
// testSetID whose recorded mock WAS consumed anywhere during the test set
// (e.g., an async / linger-batched Kafka Produce flushed after the HTTP
// response was written).
func (tfs *TestFailureStore) PruneConsumedUnmadeOutgoingCalls(testSetID string, consumed map[string]models.MockState) {
	if tfs == nil || len(consumed) == 0 {
		return
	}
	tfs.mu.Lock()
	defer tfs.mu.Unlock()
	for key, rows := range tfs.unmadeCalls {
		if key[0] != testSetID {
			continue
		}
		kept := rows[:0]
		for _, row := range rows {
			if row.MismatchReport != nil && row.MismatchReport.ClosestMock != "" {
				if _, wasConsumed := consumed[row.MismatchReport.ClosestMock]; wasConsumed {
					continue
				}
			}
			kept = append(kept, row)
		}
		if len(kept) == 0 {
			delete(tfs.unmadeCalls, key)
		} else {
			tfs.unmadeCalls[key] = kept
		}
	}
}

// isTestDrivenOutgoingMock reports whether m represents a recorded outgoing
// call driven by a test request (as opposed to startup/session handshakes,
// carry-over, broker message deliveries, idle/undelivered answers, or
// background consumer-group/broker coordination calls such as Kafka
// OffsetFetch, OffsetCommit, JoinGroup, SyncGroup, Heartbeat, Metadata, etc.).
// Used identically for both mapped and unmapped sets so default-mode reporting
// never produces false alarms and stays consistent across replay modes.
func isTestDrivenOutgoingMock(m *models.Mock) bool {
	if m == nil || m.Name == "" {
		return false
	}
	if isReusableTierMock(m) || models.IsCarryOver(m) || models.IsMessageDelivery(m) || m.IsUndelivered() {
		return false
	}
	wm := models.NewWindowedMock(m)
	if !wm.TestMock || wm.Idle {
		return false
	}
	if m.Kind == models.KAFKA {
		return isKafkaProduceMock(m)
	}
	return true
}

const kafkaProduceAPIKey = 0

// isKafkaProduceMock reports whether a Kafka mock is a Produce call (API key 0).
// Consumer-group and broker coordination calls (OffsetFetch, OffsetCommit,
// JoinGroup, SyncGroup, Heartbeat, FindCoordinator, Metadata, ApiVersions,
// ListOffsets, Fetch, etc.) fire on background consumer timers and can land
// inside a test's recorded window purely by timing, so they are never treated
// as test-driven outgoing calls.
func isKafkaProduceMock(m *models.Mock) bool {
	if m == nil || m.Kind != models.KAFKA {
		return false
	}
	if md := m.Spec.Metadata; md != nil {
		if api := strings.TrimSpace(md["kafka_api_name"]); api != "" {
			return strings.EqualFold(api, "Produce")
		}
		if api := strings.TrimSpace(md["apiKey"]); api != "" {
			return strings.EqualFold(api, "Produce") || api == "0"
		}
	}
	for _, p := range m.Spec.GenericRequests {
		for _, msg := range p.Message {
			data := strings.TrimSpace(msg.Data)
			if data == "" {
				continue
			}
			var frame struct {
				APIKeyName string `json:"apiKeyName"`
				Header     struct {
					APIKey *int `json:"apiKey"`
				} `json:"header"`
			}
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				continue
			}
			if frame.APIKeyName != "" {
				return strings.EqualFold(frame.APIKeyName, "Produce")
			}
			if frame.Header.APIKey != nil {
				return *frame.Header.APIKey == kafkaProduceAPIKey
			}
		}
	}
	return false
}

// filterTestDrivenUnconsumed keeps only unconsumed mocks that represent
// test-driven outgoing calls according to lookup (same rule for mapped and
// unmapped sets).
func filterTestDrivenUnconsumed(missing []models.UnconsumedMock, lookup map[string]mockDisplayInfo) []models.UnconsumedMock {
	if len(missing) == 0 {
		return nil
	}
	out := make([]models.UnconsumedMock, 0, len(missing))
	for _, m := range missing {
		info, ok := lookup[m.Name]
		if ok && !info.testDriven {
			continue
		}
		out = append(out, m)
	}
	return out
}

// deriveRecordedWindowExpected derives per-test expected mocks from the
// recorded test windows and loaded per-test mocks for unmapped / auto-replay
// runs (where mappings.yaml does not exist yet). Only test-driven mocks
// unambiguously inside a single non-overlapping test window are included.
func deriveRecordedWindowExpected(
	testCases []*models.TestCase,
	mocks []*models.Mock,
	backgroundExcluded []string,
) map[string][]models.MockEntry {
	if len(testCases) == 0 || len(mocks) == 0 {
		return nil
	}
	excluded := make(map[string]bool, len(backgroundExcluded))
	for _, n := range backgroundExcluded {
		excluded[n] = true
	}
	windows := recordedTestWindows(testCases)
	if len(windows) == 0 {
		return nil
	}
	windowed := make([]models.WindowedMock, 0, len(mocks))
	for _, m := range mocks {
		if m == nil || m.Name == "" || excluded[m.Name] {
			continue
		}
		if !isTestDrivenOutgoingMock(m) {
			continue
		}
		wm := models.NewWindowedMock(m)
		if wm.At.IsZero() {
			continue
		}
		windowed = append(windowed, wm)
	}
	if len(windowed) == 0 {
		return nil
	}
	derived := models.DeriveWindowMappings(windows, windowed)
	out := make(map[string][]models.MockEntry, len(derived.Tests))
	for _, tc := range derived.Tests {
		var unambiguous []models.MockEntry
		for _, e := range tc.Mocks {
			if e.Group != "" {
				continue
			}
			unambiguous = append(unambiguous, e)
		}
		if len(unambiguous) > 0 {
			out[tc.ID] = unambiguous
		}
	}
	return out
}

// buildUnmadeOutgoingCallRows builds TestFailure rows for recorded test-driven
// outgoing calls that went unobserved during a passing test's window in
// default mode.
func buildUnmadeOutgoingCallRows(
	testSetID, testID string,
	missing []models.UnconsumedMock,
	lookup map[string]mockDisplayInfo,
) []TestFailure {
	missing = filterTestDrivenUnconsumed(missing, lookup)
	if len(missing) == 0 {
		return nil
	}
	rows := make([]TestFailure, 0, len(missing))
	for _, m := range missing {
		info := lookup[m.Name]
		proto := strings.TrimSpace(info.protocol)
		if proto == "" {
			proto = strings.TrimSpace(m.Kind)
		}
		summary := strings.TrimSpace(info.summaryOf())
		if proto != "" && len(summary) > len(proto) && strings.EqualFold(summary[:len(proto)], proto) && summary[len(proto)] == ' ' {
			summary = strings.TrimSpace(summary[len(proto)+1:])
		}
		if summary == "" || strings.EqualFold(summary, proto) {
			if m.Name != "" {
				summary = m.Name
			}
		}
		target := strings.TrimSpace(info.targetOf())
		rows = append(rows, TestFailure{
			TestSetID:     testSetID,
			TestID:        testID,
			FailureReason: unmadeCallReason,
			MismatchReport: &models.MockMismatchReport{
				Protocol:      proto,
				ActualSummary: summary,
				Destination:   target,
				ClosestMock:   m.Name,
			},
		})
	}
	return rows
}

// unmadeFromWindowExpected computes missing recorded outgoing calls for an
// unmapped (auto-replay) passing test against its recorded-window expectations.
// Kept in a helper so RunTestSet retains its single buildDepResults call site.
func unmadeFromWindowExpected(
	expected []models.MockEntry,
	consumed []models.MockState,
	reusable map[string]bool,
	kindByName map[string]models.Kind,
	lookup map[string]mockDisplayInfo,
) []models.UnconsumedMock {
	return buildDepResults(expected, consumed, true, reusable, kindByName, lookup).Missing
}

// filterUnmadeNotCoveredByDrifted drops any unmade outgoing-call row whose
// recorded mock is already named as ClosestMock on a drifted outgoing-call row
// for the same test, so a drifted call is printed once (with its field diff)
// rather than twice.
func filterUnmadeNotCoveredByDrifted(unmade, drifted []TestFailure) []TestFailure {
	if len(unmade) == 0 {
		return nil
	}
	covered := make(map[[3]string]bool, len(drifted))
	for _, f := range drifted {
		if f.MismatchReport != nil && f.MismatchReport.ClosestMock != "" {
			covered[[3]string{f.TestSetID, f.TestID, f.MismatchReport.ClosestMock}] = true
		}
	}
	out := make([]TestFailure, 0, len(unmade))
	for _, u := range unmade {
		if u.MismatchReport != nil && u.MismatchReport.ClosestMock != "" {
			if covered[[3]string{u.TestSetID, u.TestID, u.MismatchReport.ClosestMock}] {
				continue
			}
		}
		out = append(out, u)
	}
	return out
}

// unmadeCallDescriptions returns a compact, deduplicated summary list of
// unmade recorded outgoing calls for structured Warn logs.
func unmadeCallDescriptions(rows []TestFailure) []string {
	if len(rows) == 0 {
		return nil
	}
	counts := make(map[string]int, len(rows))
	order := make([]string, 0, len(rows))
	for _, f := range rows {
		callText, _ := formatOutgoingCallText(f.MismatchReport)
		label := fmt.Sprintf("%s/%s: %s", f.TestSetID, f.TestID, callText)
		if counts[label] == 0 {
			order = append(order, label)
		}
		counts[label]++
	}
	sort.Strings(order)
	out := make([]string, 0, len(order))
	for _, label := range order {
		if n := counts[label]; n > 1 {
			out = append(out, fmt.Sprintf("%s (x%d)", label, n))
		} else {
			out = append(out, label)
		}
	}
	return out
}

// withoutMessageDeliveries filters broker message deliveries out of startup
// states passed to setStartupMocks so an early delivery consumed before the
// first test is never baked into mappings.yaml's STARTUP section.
func withoutMessageDeliveries(states []models.MockState, lookup map[string]mockDisplayInfo) []models.MockState {
	if len(states) == 0 || len(lookup) == 0 {
		return states
	}
	out := make([]models.MockState, 0, len(states))
	for _, m := range states {
		if lookup[m.Name].messageDelivery {
			continue
		}
		out = append(out, m)
	}
	return out
}
