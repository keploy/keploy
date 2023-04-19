package fs

import (
	"go.uber.org/zap"
	"os"
)

func DefineExportFile(path string, out *zap.Logger) *os.File {

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		out.Error("Error creating file "+path, zap.Error(err))
	}
	return file
}
