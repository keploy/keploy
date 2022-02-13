package main

import (
	"log"
	"net/http"
	"go.keploy.io/server/server"
	// "github.com/go-chi/chi"
)

func main() {
	r := server.Server()
	log.Printf("connect to http://localhost:%s/ for GraphQL playground", "8081")
	err := http.ListenAndServe(":"+"8082", r)
	if err != nil {
		panic(err)
	}
}
