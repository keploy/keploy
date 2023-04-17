package server

import (
	mockPlatform "go.keploy.io/server/pkg/platform/fs"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Output struct {
	*zap.Logger
	exportPath string
}

// ZapInit will do a normal zap initialization without check if
// an export to log exists because we need to first output if we
// can export the outputs and if any error has occurred
func ZapInit() Output {
	logConf := zap.NewDevelopmentConfig()
	logConf.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	logger, err := logConf.Build()
	if err != nil {
		panic(err)
	}
	return Output{logger, ""}
}

func (o *Output) Write(str string) {
	o.Logger.Info(str)
}

func (o *Output) ExportPath() string {
	return o.exportPath
}

func (o *Output) SetExportPath(exportPath string) {
	o.exportPath = exportPath

	if exportPath != "" {
		logExport, err := mockPlatform.OpenStream(exportPath)
		if err != nil {
			zap.Error(err)
		}
		cfg := zap.NewProductionEncoderConfig()
		cfg.EncodeTime = zapcore.ISO8601TimeEncoder

		// will make new outputs be written into logExport channel
		core := zapcore.NewCore(zapcore.NewJSONEncoder(cfg), logExport, zapcore.InfoLevel)
		o.Logger = zap.New(core)
	}
}
