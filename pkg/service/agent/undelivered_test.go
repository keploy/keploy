package agent

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A test's own mocks, as the agent's exact view takes them from its mapping,
// hold no answer the client never read, even when a mapping lists one (one
// written before the tag, or by another writer): no replay serves it, so the
// test is never charged with it as unconsumed.
func TestATestsOwnMocksHoldNoAnswerTheClientNeverRead(t *testing.T) {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	mock := func(name string, ms int, unread bool) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.Kind("Kafka"), Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: at.Add(time.Duration(ms) * time.Millisecond),
		}}
		if unread {
			m.MarkUndelivered()
		}
		return m
	}
	byName := map[string]*models.Mock{}
	for _, m := range []*models.Mock{mock("read-1", 10, false), mock("unread", 20, true), mock("read-2", 30, false)} {
		byName[m.Name] = m
	}
	got := testMocks([]string{"read-1", "unread", "read-2"}, byName)
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	if len(names) != 2 || names[0] != "read-1" || names[1] != "read-2" {
		t.Fatalf("a test's own mocks = %v; want [read-1 read-2]", names)
	}
}

// A refused answer the client never read is no recorded call its test is
// judged for (refusedOf), as a refused protocol-phase mock is not; a refused
// answer the client read is one.
func TestARefusedAnswerTheClientNeverReadChargesNoTest(t *testing.T) {
	read := &models.Mock{Name: "read", Kind: models.Kind("Kafka"), Spec: models.MockSpec{Metadata: map[string]string{"type": "mocks"}}}
	unread := &models.Mock{Name: "unread", Kind: models.Kind("Kafka"), Spec: models.MockSpec{Metadata: map[string]string{"type": "mocks"}}}
	unread.MarkUndelivered()
	s := &ClientMockStorage{}
	s.noteRefused(read)
	s.noteRefused(unread)
	if !s.refused["read"] || s.refused["unread"] {
		t.Fatalf("refused test mocks %v; want the answer read alone", s.refused)
	}
}
