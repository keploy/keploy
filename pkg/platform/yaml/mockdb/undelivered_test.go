package mockdb

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The prune of unused mocks keeps an answer the client never read
// (models.Mock.IsUndelivered): no replay serves it, so no replay consumes it,
// and the first replay would otherwise delete what the recording says the
// dependency answered. An unconsumed answer the client read is pruned as
// before, in every mock format.
func TestThePruneKeepsAnAnswerTheClientNeverRead(t *testing.T) {
	tagged := func(m *models.Mock, tag string) *models.Mock {
		m.Spec.Metadata[models.MetadataUndeliveredKey] = tag
		return m
	}
	fixture := []*models.Mock{
		pruneMongoMock(0), // mock-0: consumed
		tagged(pruneMongoMock(1), models.MetadataUndelivered), // mock-1: never read, never served
		pruneMongoMock(2),                  // mock-2: read, not consumed: pruned
		tagged(pruneMongoMock(3), "false"), // mock-3: a tag that does not say unread: pruned
	}
	want := []string{"mock-0", "mock-1"}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			writePruneFixture(t, dir, format, fixture, "")
			ys := New(zap.NewNop(), dir, "mocks")
			if err := ys.UpdateMocks(context.Background(), pruneSet, depsConsumed("mock-0"), pruneT0.Add(time.Hour), time.Time{}); err != nil {
				t.Fatal(err)
			}
			got := readDepsSet(t, dir)
			if names := depsNamesOf(got); !reflect.DeepEqual(names, want) {
				t.Fatalf("the prune kept %v, want %v", names, want)
			}
			if !got["mock-1"].IsUndelivered() {
				t.Fatalf("the kept answer lost its tag: metadata %v", got["mock-1"].Spec.Metadata)
			}
		})
	}
}
