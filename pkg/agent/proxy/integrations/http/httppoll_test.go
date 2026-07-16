package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// Importing the http integration must register its poll kind so AsyncRecorder
// re-kinds matched httpPoll-lane egress to HttpPoll.
func TestHTTPRegistersPollKind(t *testing.T) {
	got, ok := models.PollKindFor(models.HTTP)
	if !ok || got != models.HttpPoll {
		t.Fatalf("PollKindFor(HTTP) = %q,%v want HttpPoll,true (is it registered in init()?)", got, ok)
	}
}
