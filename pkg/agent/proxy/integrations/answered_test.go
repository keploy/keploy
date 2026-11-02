package integrations

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// With no listener, or after one is removed, an answer written tells no one;
// a listener is told each mock, never a nil one.
func TestAnswerWrittenTellsOnlyAListener(t *testing.T) {
	AnswerWritten(&models.Mock{Name: "mock-1"})
	var told []string
	SetAnswerWritten(func(m *models.Mock) { told = append(told, m.Name) })
	AnswerWritten(&models.Mock{Name: "mock-2"})
	AnswerWritten(nil)
	SetAnswerWritten(nil)
	AnswerWritten(&models.Mock{Name: "mock-3"})
	if len(told) != 1 || told[0] != "mock-2" {
		t.Fatalf("the listener was told %v, want [mock-2]", told)
	}
}
