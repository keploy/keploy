package record

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

type recordingMode interface {
	includeIncoming() bool
	logStart(r *Recorder)
	logAgentReady(r *Recorder)
	logRecordingActive(r *Recorder)
	recordTestCase(ctx context.Context, r *Recorder, testCase *models.TestCase, testSetID string, reRecordCfg models.ReRecordCfg, insertTestErrChan chan<- error, testCount *int)
	emitTelemetry(r *Recorder, testSetID string, testCount int, mockCountMap map[string]int)
}

func (r *Recorder) recordMode() recordingMode {
	if r.config.Record.MocksOnly {
		return mocksOnlyMode{}
	}
	return testsMode{}
}
