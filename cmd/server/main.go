package main

import (

	// "log"
	// "net/http"
	"go.keploy.io/server/server"
)

var Version = ""

func main() {
	// main method to start Keploy server
	server.Server(Version)
}
