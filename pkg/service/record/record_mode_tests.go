package record

import (
	"context"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

type testsMode struct{}

func (testsMode) includeIncoming() bool {
	return true
}

func (testsMode) logStart(r *Recorder) {
	r.logger.Info("🔴 Starting Keploy recording... Please wait.")
}

func (testsMode) logAgentReady(r *Recorder) {
	r.logger.Info("🟢 Keploy agent is ready to record test cases and mocks.")
}

func (testsMode) logRecordingActive(r *Recorder) {
	r.logger.Info("🟢 Keploy is now recording test cases and mocks for your application...")
}

func (testsMode) recordTestCase(ctx context.Context, r *Recorder, testCase *models.TestCase, testSetID string, reRecordCfg models.ReRecordCfg, insertTestErrChan chan<- error, testCount *int) {
	testCase.Curl = pkg.MakeCurlCommand(testCase.HTTPReq)
	if reRecordCfg.TestSet == "" {
		if err := r.testDB.InsertTestCase(ctx, testCase, testSetID, true); err != nil {
			insertTestErrChan <- err
			return
		}
		*testCount++
		r.telemetry.RecordedTestAndMocks()
		return
	}
	r.logger.Info("🟠 Keploy has re-recorded test case for the user's application.")
}

func (testsMode) emitTelemetry(r *Recorder, testSetID string, testCount int, mockCountMap map[string]int) {
	r.telemetry.RecordedTestSuite(testSetID, testCount, mockCountMap)
}
