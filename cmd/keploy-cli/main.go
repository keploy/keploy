package main

import (
	"fmt"
	"os"

	"go.keploy.io/server/cmd/keploy-cli/cmd"
)

func main() {
	if err := cmd.RootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
