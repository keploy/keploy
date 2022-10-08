package main

import (
	"fmt"
	"os"

	"go.keploy.io/server/cmd/server/cmd"
)

// "log"
// "net/http"

func main() {
	// main method to start Keploy server
	if err := cmd.RootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
