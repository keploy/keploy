package http

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A filter made again on an agent docker compose started again carries the
// consumed history as it was at the call, though the replay went on writing
// to it, and though the history was never copied at the call.
func TestARecordedFilterCarriesTheHistoryAsItWasAtTheCall(t *testing.T) {
	h := models.NewConsumedHistory(0)
	h.Set(models.MockState{Name: "a", Usage: models.Updated})
	h.Set(models.MockState{Name: "b", Usage: models.Updated})
	at := map[string]models.MockState{"a": {Name: "a", Usage: models.Updated}, "b": {Name: "b", Usage: models.Updated}}

	recorded, rebuild := copyMockParams(models.MockFilterParams{TotalConsumedMocks: h.Map(), ConsumedHistory: h})
	if recorded.TotalConsumedMocks != nil || recorded.ConsumedHistory != nil {
		t.Fatal("the recorded filter holds the live history; the replay goes on writing to it")
	}
	h.Set(models.MockState{Name: "a", Usage: models.Deleted})
	h.Set(models.MockState{Name: "c", Usage: models.Updated})

	if got := rebuild(recorded).TotalConsumedMocks; !reflect.DeepEqual(got, at) {
		t.Fatalf("made again, the filter carries %v; want the history at the call, %v", got, at)
	}
}
