/*
 * The idea of this module is to serve as a centralized output service for Keploy
 */

package service

import (
	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/pkg/platform/fs"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"os"
	"path/filepath"
)

type Output struct {
	zap  *zap.Logger
	pp   *pp.PrettyPrinter
	file *os.File
}

// OutInit will do a normal zap initialization without check if
// an export to log exists because we need to first output if we
// can export the outputs and if any error has occurred.
// Also, it will initialize pp.
func OutInit() Output {
	out := Output{nil, nil, nil}
	out.zapInit(zapcore.InfoLevel)
	out.pp = pp.New()
	return out
}

func (out *Output) zapInit(level zapcore.Level) {
	logConf := zap.NewDevelopmentConfig()
	logConf.Level = zap.NewAtomicLevelAt(level)
	logger, err := logConf.Build()
	if err != nil {
		panic(err)
	}

	out.zap = logger
}

func (out *Output) SetZapLevel(level zapcore.Level) {
	if out.IsExport() {
		out.setZapExport(level)
	} else {
		out.zapInit(level)
	}
}

func (out *Output) IsExport() bool {
	if out.file != nil {
		return true
	}
	return false
}

// ExportOutput will be used to define a path to logs be exported
func (out *Output) ExportOutput(path string, level zapcore.Level) {

	if path != "" {
		out.zap.Info("Output being redirected to: " + path)
		// works on unix or windows
		cleanPath := filepath.Clean(path)

		out.file = fs.DefineExportFile(cleanPath, out.zap)
		out.setZapExport(level)    // pipe zap
		os.Stdout = out.file       // pipe fmt...
		out.pp.SetOutput(out.file) // pipe pp

	} else {
		out.zap.Info("Export path empty. Outputing at terminal")
	}

}

func (out *Output) setZapExport(level zapcore.Level) {
	cfg := zap.NewProductionEncoderConfig()
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder

	// Create a new io.Writer that writes to the log file
	fileWriter := zapcore.AddSync(out.file)

	// will make new outputs be written into logExport channel
	core := zapcore.NewCore(zapcore.NewJSONEncoder(cfg), fileWriter, level)
	out.zap = zap.New(core)
}

func (out *Output) GetZap() *zap.Logger {
	return out.zap
}

func (out *Output) GetPP() *pp.PrettyPrinter {
	return out.pp
}
