package record

import (
	"context"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A RECORDING WHOSE STOP KILLED THE APP SAYS SO (P101-b-k).
//
// Under the proven switch keploy's stop of the app at the recording's end
// says when it had to kill it: the app had not exited when the stop's grace
// ran out (models.AppError.StopKilled). What the app would have done as it
// stopped, a shutdown publish or a final flush, is then not in the recording.
// A replay whose stop lets the app finish its shutdown makes those calls, and
// finds no mock for them. Judged, they would fail the set on what record's
// grace cut, which depends on the recording host's load. So the test-set's
// config says why its recording's stop killed (models.TestSet.RecordStopKilled),
// and a replay under the switch does not judge a call the app makes after
// the set's last test that no mock answered: the set is not judged, and the
// report names the recording's kill.

// noteStopKilled keeps why the app's run says its stop had to kill it.
func noteStopKilled(to *atomic.Pointer[string], appErr models.AppError) {
	if appErr.StopKilled != "" {
		why := appErr.StopKilled
		to.Store(&why)
	}
}

// configForUpdate is the test-set config store's read for a rewrite: no
// secret is read into what is written back.
type configForUpdate interface {
	ReadForUpdate(ctx context.Context, testSetID string) (*models.TestSet, error)
}

// markStopKilled writes why keploy's stop of the app at the recording's end
// had to kill it into testSetID's config (RecordStopKilled), and says so.
func (r *Recorder) markStopKilled(ctx context.Context, testSetID, why string) {
	if !provenswitch.On() {
		return
	}
	r.logger.Warn("the recording's stop had to kill the application: what it would have done as it stopped is not in the recording, so a replay of this test-set does not judge a call the application makes after the set's last test ("+provenswitch.Env+"=1)",
		zap.String("testSet", testSetID), zap.String("reason", why),
		zap.String("next_step", "make the application exit promptly on SIGINT, then record the test-set again"))
	if r.testSetConf == nil {
		utils.LogError(r.logger, nil, "keploy could not mark the test-set's recording as cut by its stop's kill: there is no test-set config store; a replay may fail it for a call the recording lacks",
			zap.String("testSet", testSetID))
		return
	}
	var conf *models.TestSet
	var err error
	if u, ok := r.testSetConf.(configForUpdate); ok {
		conf, err = u.ReadForUpdate(ctx, testSetID)
	} else {
		conf, err = r.testSetConf.Read(ctx, testSetID)
		conf = conf.WithoutSecrets()
	}
	if err == nil && conf == nil {
		conf = &models.TestSet{}
	}
	if err == nil {
		conf.RecordStopKilled = why
		err = r.testSetConf.Write(ctx, testSetID, conf)
	}
	if err != nil {
		utils.LogError(r.logger, err, "keploy could not mark the test-set's recording as cut by its stop's kill; a replay may fail it for a call the recording lacks",
			zap.String("testSet", testSetID))
	}
}
