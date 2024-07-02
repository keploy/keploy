package golang

import (
	"debug/elf"
	"fmt"
	"os"
	"strings"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// CheckGoBinaryForCoverFlag checks if the given Go binary has the coverage flag enabled
// TODO: use native approach till https://github.com/golang/go/issues/67366 gets resolved
func checkGoBinaryForCoverFlag(logger *zap.Logger, cmd string) bool {
	file, err := elf.Open(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to open file")
		fmt.Fprintf(os.Stderr, "Failed to open file: %v\n", err)
		return false
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close binary file", zap.String("file", cmd))
		}
	}()

	symbols, err := file.Symbols()
	if err != nil {
		utils.LogError(logger, err, "failed to read symbols")
		return false
	}

	for _, symbol := range symbols {
		// Check for symbols that related to Go coverage instrumentation
		if strings.Contains(symbol.Name, "internal/coverage") {
			return true
		}
	}
	return false
}
