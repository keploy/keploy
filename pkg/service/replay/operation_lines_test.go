package replay

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The lines a person reads of a miss name the operation an HTTP call
// carries after its call ("POST /query {query GetAppLabels}"), and a call
// of none reads as it did.
// Mutations: the mismatch heading, or describeUnclaimed, drops the
// operation.
func TestTheMissLinesNameTheOperation(t *testing.T) {
	gql := func(op string) models.UnmatchedCall {
		return models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /query", Operation: op, Destination: "api:8080"}
	}
	if got := describeUnclaimed(gql("query GetAppLabels")); got != "HTTP POST /query {query GetAppLabels} to api:8080" {
		t.Fatalf("unclaimed %q", got)
	}
	if got := describeUnclaimed(gql("")); got != "HTTP POST /query to api:8080" {
		t.Fatalf("unclaimed, no operation: %q", got)
	}
	for op, want := range map[string]string{
		"query GetAppLabels": "  Mock mismatch: [HTTP] POST /query {query GetAppLabels}  →  api:8080",
		"":                   "  Mock mismatch: [HTTP] POST /query  →  api:8080",
	} {
		out := captureStdout(t, func() {
			printMismatchReport(&models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "POST /query", Operation: op, Destination: "api:8080"})
		})
		if first, _, _ := strings.Cut(out, "\n"); first != want {
			t.Fatalf("heading %q; want %q", first, want)
		}
	}
}
