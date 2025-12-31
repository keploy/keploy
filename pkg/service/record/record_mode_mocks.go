package record

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

type mocksOnlyMode struct{}

func (mocksOnlyMode) includeIncoming() bool {
	return false
}

func (mocksOnlyMode) logStart(r *Recorder) {
	r.logger.Info("🔴 Starting Keploy mock recording... Please wait.")
}

func (mocksOnlyMode) logAgentReady(r *Recorder) {
	r.logger.Info("🟢 Keploy agent is ready to record mocks.")
}

func (mocksOnlyMode) logRecordingActive(r *Recorder) {
	r.logger.Info("🟢 Keploy is now recording mocks for your application...")
}

func (mocksOnlyMode) recordTestCase(_ context.Context, _ *Recorder, _ *models.TestCase, _ string, _ models.ReRecordCfg, _ chan<- error, _ *int) {
}

func (mocksOnlyMode) emitTelemetry(r *Recorder, _ string, _ int, mockCountMap map[string]int) {
	r.telemetry.RecordedMocks(mockCountMap)
}
