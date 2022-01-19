package main

import (
	"log"
	"net/http"

	"go.keploy.io/server/server"
)

func main() {
	r := server.Server()
	log.Printf("connect to http://localhost:%s/ for GraphQL playground", "8081")

	err := http.ListenAndServe(":"+"8081", r)
	if err != nil {
		panic(err)
	}
}
