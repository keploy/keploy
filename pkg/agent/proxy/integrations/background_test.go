package integrations

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// backgroundStore is a MockMemDb with exact serving's background and record's
// pairing (ExactBackground, Pairing).
type backgroundStore struct {
	MockMemDb
	unused, spent, candidates []*models.Mock
	claimed                   map[*models.Mock]bool
}

func (s *backgroundStore) Background(kind models.Kind) []*models.Mock { return ofKind(s.unused, kind) }
func (s *backgroundStore) SpentBackground(kind models.Kind) []*models.Mock {
	return ofKind(s.spent, kind)
}
func (s *backgroundStore) PairingCandidates(kind models.Kind) []*models.Mock {
	return ofKind(s.candidates, kind)
}
func (s *backgroundStore) ClaimPairing(m *models.Mock) bool {
	if s.claimed[m] {
		return false
	}
	s.claimed[m] = true
	return true
}

func ofKind(ms []*models.Mock, kind models.Kind) []*models.Mock {
	var out []*models.Mock
	for _, m := range ms {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

// The helpers ask the store when it has the capability, and answer nil and
// false when it does not, or when there is no store.
func TestBackgroundAndPairingHelpers(t *testing.T) {
	a, b, c := &models.Mock{Name: "a", Kind: models.Mongo}, &models.Mock{Name: "b", Kind: models.Mongo}, &models.Mock{Name: "c", Kind: models.HTTP}
	s := &backgroundStore{unused: []*models.Mock{a, c}, spent: []*models.Mock{b}, candidates: []*models.Mock{b, c}, claimed: map[*models.Mock]bool{}}
	if got := Background(s, models.Mongo); len(got) != 1 || got[0] != a {
		t.Fatalf("Background = %v; want a", got)
	}
	if got := SpentBackground(s, models.Mongo); len(got) != 1 || got[0] != b {
		t.Fatalf("SpentBackground = %v; want b", got)
	}
	if got := PairingCandidates(s, models.HTTP); len(got) != 1 || got[0] != c {
		t.Fatalf("PairingCandidates = %v; want c", got)
	}
	if !ClaimPairing(s, b) || ClaimPairing(s, b) {
		t.Fatal("ClaimPairing does not claim b once")
	}
	for name, db := range map[string]MockMemDb{"a store without them": struct{ MockMemDb }{}, "no store": nil} {
		if Background(db, models.Mongo) != nil || SpentBackground(db, models.Mongo) != nil || PairingCandidates(db, models.Mongo) != nil || ClaimPairing(db, a) {
			t.Errorf("%s answers the background or a pairing", name)
		}
	}
}
