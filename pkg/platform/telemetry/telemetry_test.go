package telemetry

import (
	"testing"

	"github.com/keploy/go-sdk/mock"
	"github.com/keploy/go-sdk/pkg/keploy"
	mockPlatform "go.keploy.io/server/pkg/platform/fs"
	"go.uber.org/zap"
)

var (
	logger *zap.Logger
	tSrvc  *Telemetry
)

func TestMain(m *testing.M) {
	logger, _ = zap.NewProduction()
	defer logger.Sync()

	teleFS := mockPlatform.NewTeleFS()
	tSrvc = NewTelemetry(nil, true, false, true, teleFS, logger, "v0.1.0-test-telemetry")
}

func TestPing(t *testing.T) {
	ctx := mock.NewContext(mock.Config{
		Mode: keploy.MODE_RECORD,
		Name: "Ping",
	})
	tSrvc.Ping(false)
}
