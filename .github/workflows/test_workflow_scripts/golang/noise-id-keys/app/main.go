// The app under test answers maps keyed by ids, as a search service answers
// the options it found keyed by option id. Each option carries a time the
// handler reads from its clock and a fixed field (LABEL, from the
// environment), so a replay moves the times and keeps the fixed fields, and a
// replay with another LABEL changes only them.
//
//	GET /options?q=Q  forty options, each {"departAt": now, "label": LABEL-i}
//	GET /holds?q=Q    two options: one {"hold": now}, the other
//	                  {"holdReason": LABEL}, a field whose name begins with
//	                  the other's time field
//	GET /slots?q=Q    three options: two {"hold": now}, one {"price": 5}
//	                  that also holds {"hold": now} when LABEL is not
//	                  "fixed", as an option that became held does
//	GET /seen?q=Q     {"lastSeen": {id: now}} for two ids, and for a third
//	                  id when LABEL is not "fixed"
//
// The option ids are UUIDs derived from the query, so they are the same on
// every call of one request, as a mocked upstream serves them back at replay.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// optionID is the UUID-shaped id of option i of query q.
func optionID(q string, i int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", q, i)))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func answer(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	label := os.Getenv("LABEL")
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	http.HandleFunc("/options", func(w http.ResponseWriter, r *http.Request) {
		q, now := r.URL.Query().Get("q"), time.Now().UTC().Format(time.RFC3339)
		options := map[string]interface{}{}
		for i := 0; i < 40; i++ {
			options[optionID(q, i)] = map[string]interface{}{"departAt": now, "label": fmt.Sprintf("%s-%d", label, i)}
		}
		answer(w, map[string]interface{}{"query": q, "options": options})
	})
	http.HandleFunc("/holds", func(w http.ResponseWriter, r *http.Request) {
		q, now := r.URL.Query().Get("q"), time.Now().UTC().Format(time.RFC3339)
		answer(w, map[string]interface{}{"query": q, "options": map[string]interface{}{
			optionID(q, 0): map[string]interface{}{"hold": now},
			optionID(q, 1): map[string]interface{}{"holdReason": label},
		}})
	})
	http.HandleFunc("/slots", func(w http.ResponseWriter, r *http.Request) {
		q, now := r.URL.Query().Get("q"), time.Now().UTC().Format(time.RFC3339)
		third := map[string]interface{}{"price": 5}
		if label != "fixed" {
			third["hold"] = now
		}
		answer(w, map[string]interface{}{"query": q, "options": map[string]interface{}{
			optionID(q, 0): map[string]interface{}{"hold": now},
			optionID(q, 1): map[string]interface{}{"hold": now},
			optionID(q, 2): third,
		}})
	})
	http.HandleFunc("/seen", func(w http.ResponseWriter, r *http.Request) {
		q, now := r.URL.Query().Get("q"), time.Now().UTC().Format(time.RFC3339)
		seen := map[string]interface{}{optionID(q, 0): now, optionID(q, 1): now}
		if label != "fixed" {
			seen[optionID(q, 2)] = now
		}
		answer(w, map[string]interface{}{"query": q, "lastSeen": seen})
	})
	version := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Build", label)
		answer(w, map[string]interface{}{"version": label})
	}
	http.HandleFunc("/version", version)
	http.HandleFunc("/about", version)
	http.HandleFunc("/apps/", version)
	if err := http.ListenAndServe("127.0.0.1:8093", nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
