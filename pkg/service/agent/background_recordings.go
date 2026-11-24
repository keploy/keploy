package agent

import (
	"math"
	"slices"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// seedBackgroundRecordings hands the proxy, as a set is staged, the set's
// background recordings params names (models.MockFilterParams.BackgroundMocks)
// with their recorded spans (backgroundRecordingsOf), read from every mock
// the agent holds for the set: resident ones and those kept on disk, served
// or not, together with any declared kind: background Mongo and HTTP rules
// (models.MockFilterParams.BackgroundMongoRules, BackgroundHTTPRules). A
// staging call that names none clears the previous set's. Whenever reading
// the background recordings fails OR any named background recording is not
// found, no declared background rules are seeded (fail closed) so every
// unmatched call stays its test's miss and strict per-recording checks are
// never bypassed.
func (a *Agent) seedBackgroundRecordings(params models.MockFilterParams, resident [][]*models.Mock, disk *proxyPkg.DiskMocks) {
	recs, missing, err := backgroundRecordingsWithMissing(params.BackgroundMocks, params.StartupMocks, resident, disk)
	allowRules := true
	if err != nil {
		a.logger.Warn("could not read the test set's background recordings (mappings.yaml worker: and startup:); declared background rule fallback is disabled and every call that misses stays its test's miss",
			zap.Int("background", len(params.BackgroundMocks)), zap.Error(err))
		recs = nil
		allowRules = false
	} else if len(missing) > 0 {
		a.logger.Warn("some background recordings named by mappings.yaml worker: and startup: were not found; declared background rule fallback is disabled so every unmatched call stays its test's miss",
			zap.Strings("missing", missing))
		allowRules = false
	}
	if ruleSeeder, ok := a.Proxy.(coreAgent.BackgroundMongoRulesSeeder); ok {
		if allowRules {
			ruleSeeder.SeedBackgroundMongoRules(params.BackgroundMongoRules)
		} else {
			ruleSeeder.SeedBackgroundMongoRules(nil)
		}
	}
	if httpRuleSeeder, ok := a.Proxy.(coreAgent.BackgroundHTTPRulesSeeder); ok {
		if allowRules {
			httpRuleSeeder.SeedBackgroundHTTPRules(params.BackgroundHTTPRules)
		} else {
			httpRuleSeeder.SeedBackgroundHTTPRules(nil)
		}
	}
	if seeder, ok := a.Proxy.(coreAgent.BackgroundRecordingsSeeder); ok {
		seeder.SeedBackgroundRecordings(recs)
	}
}

// bgCallTime is what bounds a background recording's recorded span, of one
// data-plane recording: its request and answer time.
type bgCallTime struct {
	req, res time.Time
}

// backgroundRecordingsOf is, of the set's recordings (resident, and those
// disk keeps), the ones names names, each with its recorded span's start
// (models.BackgroundRecording.From): the request time of the latest
// data-plane recording sent before it, not itself, and not answered after
// it was sent, the rule learn.PreviousCall reads a call's previous call by.
// Under the proven switch each one startup names is a plain startup:
// recording (models.BackgroundRecording.Startup) when a user can declare a
// call identical to it (models.StartupDeclarable: an HTTP or HTTP/2
// recording that states its lifetime, and any of a kind a rule system
// registered) and no keploy.yaml sessionCalls entry
// names it exactly (models.Mock.DeclaredSessionCall): a declared session
// call stays the background's, and so does a recording no one can declare,
// as in v3.7.18. With the switch off none is, whatever the CLI
// sent. Each recording on disk is decoded once, and only
// the named ones are kept. nil for no names; err when disk cannot be read.
func backgroundRecordingsOf(names, startup []string, resident [][]*models.Mock, disk *proxyPkg.DiskMocks) ([]models.BackgroundRecording, error) {
	recs, _, err := backgroundRecordingsWithMissing(names, startup, resident, disk)
	return recs, err
}

// backgroundRecordingsWithMissing is backgroundRecordingsOf plus the sorted
// list of unique names in names that were not found in resident or disk.
func backgroundRecordingsWithMissing(names, startup []string, resident [][]*models.Mock, disk *proxyPkg.DiskMocks) ([]models.BackgroundRecording, []string, error) {
	if len(names) == 0 {
		return nil, nil, nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	plain := make(map[string]bool, len(startup))
	if provenswitch.On() {
		for _, n := range startup {
			plain[n] = true
		}
	}
	var recs []models.BackgroundRecording
	var calls []bgCallTime
	got := map[string]bool{}
	seen := map[string]bool{}
	see := func(mk *models.Mock) bool {
		if mk == nil {
			return true
		}
		id := learn.MockID(mk)
		if seen[id] {
			return true
		}
		seen[id] = true
		if want[mk.Name] && !got[mk.Name] {
			got[mk.Name] = true
			recs = append(recs, models.BackgroundRecording{Mock: mk, Startup: plain[mk.Name] && models.StartupDeclarable(mk) && !mk.DeclaredSessionCall()})
		}
		if !mk.IsProtocolPhase() && !mk.Spec.ReqTimestampMock.IsZero() {
			calls = append(calls, bgCallTime{req: mk.Spec.ReqTimestampMock, res: mk.Spec.ResTimestampMock})
		}
		return true
	}
	for _, list := range resident {
		for _, mk := range list {
			see(mk)
		}
	}
	if disk != nil {
		if err := disk.RangeRecordedBefore(time.Unix(0, math.MaxInt64), see); err != nil {
			return nil, nil, err
		}
	}
	var missing []string
	for n := range want {
		if !got[n] {
			missing = append(missing, n)
		}
	}
	slices.Sort(missing)
	slices.SortFunc(calls, func(a, b bgCallTime) int { return a.req.Compare(b.req) })
	for i := range recs {
		recs[i].From = previousCallAt(calls, recs[i].Mock)
	}
	return recs, missing, nil
}

// previousCallAt is the request time of rec's previous call among calls (by
// request time): the latest one sent before rec (so never rec itself, nor a
// call sent at its instant) and not answered after rec was sent (another
// flow's call, pending across its send; one with no answer time counts), as
// learn.PreviousCall.See keeps it. Zero for none, and for a rec with no
// request time (calls hold none sent at the zero time).
func previousCallAt(calls []bgCallTime, rec *models.Mock) time.Time {
	to := rec.Spec.ReqTimestampMock
	i, _ := slices.BinarySearchFunc(calls, to, func(c bgCallTime, t time.Time) int { return c.req.Compare(t) })
	for i--; i >= 0; i-- {
		if calls[i].res.After(to) {
			continue
		}
		return calls[i].req
	}
	return time.Time{}
}
