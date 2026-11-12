// The app under test: GET /label saves a lead's label with the labels
// service, PUT /labels/1 {"label":"Lead <uuid> for ci"}, a UUID made fresh
// on every call inside a text whose other words are fixed; then it answers
// {"label":"ok"} when the service answered {"saved":true}, else
// {"label":"unsaved"}, so a test passes only where its call was answered as
// recorded, and only the call shows what it wrote. WORDS_OTHER=1 makes the
// text "Evil <uuid> for attacker": the same skeleton, its other words
// changed.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

func uuid() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func main() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	http.HandleFunc("/label", func(w http.ResponseWriter, r *http.Request) {
		label := "Lead " + uuid() + " for ci"
		if os.Getenv("WORDS_OTHER") != "" {
			label = "Evil " + uuid() + " for attacker"
		}
		body, _ := json.Marshal(map[string]string{"label": label})
		saved := false
		req, err := http.NewRequest(http.MethodPut, os.Getenv("LABELS_URL")+"/labels/1", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				got, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				saved = resp.StatusCode == http.StatusOK && string(got) == `{"saved":true}`
			}
		}
		if !saved {
			fmt.Fprint(w, `{"label":"unsaved"}`)
			return
		}
		fmt.Fprint(w, `{"label":"ok"}`)
	})
	_ = http.ListenAndServe(":8191", nil)
}
