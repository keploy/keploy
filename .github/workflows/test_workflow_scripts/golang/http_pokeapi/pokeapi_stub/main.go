// Program pokeapi_stub serves deterministic PokeAPI responses for CI recordings.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
)

const responsePaddingSize = 10 * 1024

type locationList struct {
	Count   int              `json:"count"`
	Results []locationResult `json:"results"`
}

type locationResult struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type locationArea struct {
	Name              string             `json:"name"`
	PokemonEncounters []pokemonEncounter `json:"pokemon_encounters"`
	Padding           string             `json:"padding"`
}

type pokemonEncounter struct {
	Pokemon pokemon `json:"pokemon"`
}

type pokemon struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

var locations = []string{
	"canalave-city-area",
	"eterna-city-area",
	"pastoria-city-area",
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "address to listen on")
	flag.Parse()

	server := &http.Server{
		Addr:    *addr,
		Handler: newHandler(),
	}
	log.Printf("PokeAPI stub listening on %s", *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v2/location-area", locationAreaHandler)
	mux.HandleFunc("/api/v2/location-area/", locationAreaHandler)
	return mux
}

func locationAreaHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/api/v2/location-area/")
	if name == r.URL.Path || name == "" {
		results := make([]locationResult, 0, len(locations))
		for _, location := range locations {
			results = append(results, locationResult{
				Name: location,
				URL:  fmt.Sprintf("http://pokeapi.test/api/v2/location-area/%s", location),
			})
		}
		writeJSON(w, locationList{Count: len(results), Results: results})
		return
	}

	if !contains(locations, name) {
		http.NotFound(w, r)
		return
	}

	encounters := make([]pokemonEncounter, 0, len(locations))
	for i, pokemonName := range []string{"pikachu", "eevee", "bulbasaur"} {
		encounters = append(encounters, pokemonEncounter{Pokemon: pokemon{
			Name: pokemonName,
			URL:  fmt.Sprintf("http://pokeapi.test/api/v2/pokemon/%d", i+1),
		}})
	}
	writeJSON(w, locationArea{
		Name:              name,
		PokemonEncounters: encounters,
		Padding:           strings.Repeat("x", responsePaddingSize),
	})
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("failed to write stub response: %v", err)
	}
}
