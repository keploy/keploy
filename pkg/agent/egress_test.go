package agent

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Every knob's line has the one form a search finds.
func TestLogRealEgressHasOneForm(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	LogRealEgress(zap.New(core), "mock.real", "these destinations", zap.Int("n", 1))
	all := logs.All()
	if len(all) != 1 || all[0].Message != RealEgressPrefix+"mock.real: these destinations" || all[0].Level != zapcore.InfoLevel {
		t.Fatalf("logged %+v", all)
	}
}
