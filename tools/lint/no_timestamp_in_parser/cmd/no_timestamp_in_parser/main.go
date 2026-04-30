// Command no_timestamp_in_parser runs the notimestampinparser analyzer as a
// standalone binary. It is a thin singlechecker.Main wrapper so developers can
// invoke the rule locally without golangci-lint:
//
//	go run ./tools/lint/no_timestamp_in_parser/cmd/no_timestamp_in_parser ./...
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	notimestampinparser "go.keploy.io/server/v3/tools/lint/no_timestamp_in_parser"
)

func main() {
	singlechecker.Main(notimestampinparser.Analyzer)
}
