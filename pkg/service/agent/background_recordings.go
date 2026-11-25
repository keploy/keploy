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
// or not. A staging call that names none clears the previous set's. A set
// whose recordings cannot be read is handed none: every miss stays its
// test's, as before.
func (a *Agent) seedBackgroundRecordings(params models.MockFilterParams, resident [][]*models.Mock, disk *proxyPkg.DiskMocks) {
	seeder, ok := a.Proxy.(coreAgent.BackgroundRecordingsSeeder)
	if !ok {
		return
	}
	var recs []models.BackgroundRecording
	var err error
	if len(params.BackgroundMocks) > 0 {
		recs, err = backgroundRecordingsOf(params.BackgroundMocks, params.StartupMocks, resident, disk)
	} else if params.DeriveBackgroundFromWindows && len(params.RecordedWindows) > 0 && !params.ExactStaging {
		recs, err = derivedBackgroundRecordingsOf(params.RecordedWindows, resident, disk)
	}
	if err != nil {
		a.logger.Warn("could not read the test set's background recordings; every call that misses stays its test's miss",
			zap.Int("background", len(params.BackgroundMocks)), zap.Int("windows", len(params.RecordedWindows)), zap.Error(err))
		recs = nil
	}
	seeder.SeedBackgroundRecordings(recs)
}

// derivedBackgroundRecordingsOf derives outside-window background recordings
// from windows via models.DeriveWindowMappings, then loads only those named
// recordings and their recorded spans (From) via backgroundRecordingsOf. The
// two passes preserve DiskMocks's O(window) memory bound: pass 1 retains only
// lightweight WindowedMock metadata (never full decoded *models.Mock values)
// while pass 2 keeps only the derived background mocks. Used only when the
// caller explicitly opted in via MockFilterParams.DeriveBackgroundFromWindows
// (no mappings.yaml exists and no exact plan was built). Derived recordings
// are returned with Startup=false because DeriveWindowMappings places all
// outside-window recordings in Worker.
func derivedBackgroundRecordingsOf(windows []models.TestWindow, resident [][]*models.Mock, disk *proxyPkg.DiskMocks) ([]models.BackgroundRecording, error) {
	if len(windows) == 0 {
		return nil, nil
	}
	var wm []models.WindowedMock
	seenID := map[string]bool{}
	seenName := map[string]bool{}
	see := func(mk *models.Mock) bool {
		if mk == nil {
			return true
		}
		id := learn.MockID(mk)
		if seenID[id] {
			return true
		}
		seenID[id] = true
		if mk.Name != "" && !seenName[mk.Name] {
			seenName[mk.Name] = true
			wm = append(wm, models.NewWindowedMock(mk))
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
			return nil, err
		}
	}
	if len(wm) == 0 {
		return nil, nil
	}
	derived := models.DeriveWindowMappings(windows, wm)
	if len(derived.Worker) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(derived.Worker))
	added := make(map[string]bool, len(derived.Worker))
	for _, e := range derived.Worker {
		if e.Name == "" || added[e.Name] {
			continue
		}
		added[e.Name] = true
		names = append(names, e.Name)
	}
	if len(names) == 0 {
		return nil, nil
	}
	return backgroundRecordingsOf(names, nil, resident, disk)
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
	if len(names) == 0 {
		return nil, nil
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
			return nil, err
		}
	}
	slices.SortFunc(calls, func(a, b bgCallTime) int { return a.req.Compare(b.req) })
	for i := range recs {
		recs[i].From = previousCallAt(calls, recs[i].Mock)
	}
	return recs, nil
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
