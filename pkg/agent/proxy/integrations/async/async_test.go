package async

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// fakeParser is a transport-agnostic stand-in used across async tests.
type fakeParser struct {
	matches bool
	shapeOK bool
	empty   []byte
}

func (f *fakeParser) MatchesLane(_ *models.Mock, _ models.AsyncLane) bool { return f.matches }
func (f *fakeParser) MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string) {
	if f.shapeOK {
		return true, ""
	}
	return false, "shape drift"
}
func (f *fakeParser) EmptyResponse(_ models.AsyncLane) ([]byte, error)       { return f.empty, nil }
func (f *fakeParser) ResponseValueKey(*models.Mock, models.AsyncLane) string { return string(f.empty) }

// compile-time assertion the fake satisfies the interface.
var _ AsyncParser = (*fakeParser)(nil)

func TestFakeParserSatisfiesInterface(t *testing.T) {
	var p AsyncParser = &fakeParser{matches: true, shapeOK: true, empty: []byte("empty")}
	if !p.MatchesLane(nil, models.AsyncLane{}) {
		t.Fatal("MatchesLane should be true")
	}
	if ok, _ := p.MatchRequestShape(nil, nil, models.AsyncLane{}); !ok {
		t.Fatal("MatchRequestShape should be ok")
	}
	b, _ := p.EmptyResponse(models.AsyncLane{})
	if string(b) != "empty" {
		t.Fatalf("EmptyResponse = %q", b)
	}
}
