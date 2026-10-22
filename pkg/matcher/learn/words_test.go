package learn

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// Until words are learned, nothing is admitted by its words and no leaf is
// judged whole: each fails closed, with its reason.
func TestTheWordsContractFailsClosed(t *testing.T) {
	e := models.WordsEntry{V: models.WordsVersion, Digest: "9f2c41d07a3be815", Layout: "4be1c09a5d3e7f21", Slots: []models.WordSlot{{Seg: 0}}}
	if ok, why := WordsAdmit("ORD-1 ref=a1", "ORD-1 ref=b2", e); ok || why == "" {
		t.Errorf("WordsAdmit = %v, %q: want a refusal with its reason", ok, why)
	}
	if ok, why := WordsAdmit("same", "same", e); ok || why == "" {
		t.Errorf("WordsAdmit of an equal value = %v, %q: want a refusal with its reason", ok, why)
	}
	for _, unread := range []models.WordsEntry{{Digest: e.Digest, Layout: e.Layout, Slots: e.Slots}, {V: models.WordsVersion + 1}} {
		if ok, why := WordsAdmit("ORD-1 ref=a1", "ORD-1 ref=b2", unread); ok || why != unread.Unreadable() {
			t.Errorf("WordsAdmit of an entry this build cannot read = %v, %q: want a refusal saying %q", ok, why, unread.Unreadable())
		}
	}
	if whole, why := WholeDrift("a1b2", "c3d4", nil); whole || why == "" {
		t.Errorf("WholeDrift = %v, %q: want a refusal with its reason", whole, why)
	}
}
