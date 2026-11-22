package routes

import (
	"crypto/rand"
	"net/http"

	"go.keploy.io/server/v3/pkg/models"
)

// processInstance names this agent process in its answers
// (models.AgentInstanceHeader): drawn once, as the process starts, so every
// router the process builds answers with it, and a process started again in
// this one's place answers with another.
var processInstance = rand.Text()

// answersAs sets the agent process's instance on every answer next serves
// (models.AgentInstanceHeader). It is set before next runs, so an answer next
// writes or streams carries it whatever its status.
func answersAs(instance string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(models.AgentInstanceHeader, instance)
			next.ServeHTTP(w, r)
		})
	}
}
