package mock

import (
	"testing"

	"go.keploy.io/server/v3/config"
)

// A record's full round keeps what it learned only on the recordings the
// record has just made, and on the pool's only when the record replaces it.
func TestLearnsOnto(t *testing.T) {
	m := &mockService{config: &config.Config{}}
	if !m.learnsOnto("o::A") || !m.learnsOnto("") {
		t.Fatal("without LearnTests every mock keeps what was learned")
	}
	m.config.Test.LearnTests = map[string]bool{"o::A": true}
	if !m.learnsOnto("o::A") || m.learnsOnto("o::B") {
		t.Fatal("only the tests the record made keep what was learned")
	}
	if m.learnsOnto("") {
		t.Fatal("a partial record never changes the pool")
	}
	m.config.Test.LearnPool = true
	if !m.learnsOnto("") {
		t.Fatal("a full record keeps what was learned on the pool")
	}
}
