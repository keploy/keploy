package golang

import (
	"debug/elf"
	"slices"
	"strings"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// checkForCoverFlag checks if the given Go binary has the coverage flag enabled if one argument
// else check if -cover flag is passed or not
// TODO: use native approach till https://github.com/golang/go/issues/67366 gets resolved
func checkForCoverFlag(logger *zap.Logger, cmd string) bool {
	cmdFields := strings.Fields(cmd)
	i := 0
	var part string
	for i, part = range cmdFields {
		if !strings.Contains(part, "=") {
			break
		}
	}
	if cmdFields[i] == "go" && len(cmdFields) > 1 {
		if slices.Contains(cmdFields, "-cover") {
			return true
		}
		logger.Warn("cover flag not found in command, skipping coverage calculation")
		return false
	}
	file, err := elf.Open(cmdFields[i])
	if err != nil {
		utils.LogError(logger, err, "failed to open file, skipping coverage calculation")
		return false
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close binary file", zap.String("file", cmd))
		}
	}()

	symbols, err := file.Symbols()
	if err != nil {
		utils.LogError(logger, err, "failed to read symbols, skipping coverage calculation")
		return false
	}

	for _, symbol := range symbols {
		// Check for symbols that related to Go coverage instrumentation
		if strings.Contains(symbol.Name, "internal/coverage") {
			return true
		}
	}
	logger.Warn("go binary was not build with -cover flag", zap.String("file", cmd))
	return false
}
