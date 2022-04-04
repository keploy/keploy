package main

import (
	"go.keploy.io/server/server"
	"log"
	"net/http"
	// "github.com/go-chi/chi"
)

func main() {
	r := server.Server()
	log.Printf("connect to http://localhost:%s/ for GraphQL playground", "8081")
	err := http.ListenAndServe(":"+"8081", r)
	if err != nil {
		panic(err)
	}
}
