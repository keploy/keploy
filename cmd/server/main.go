package main

import (

	// "log"
	// "net/http"
	"go.keploy.io/server/server"
)

// Version will be injected during go build with ldflag
var Version = ""

func main() {
	// main method to start Keploy server
	server.Server(Version)
}
