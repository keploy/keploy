package matcher

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// BodyNoiseCovering names the entry that covers a body place as the
// comparison applies noise: the test case's whole-body noise, a global "*",
// then the test case's keys before the global ones; a key without a dot names
// a field at any depth, a dotted key a path, and an entry over a field covers
// everything under it. Array indices are no part of a path.
func TestBodyNoiseCovering(t *testing.T) {
	keys := []string{"orders", "id"} // body.orders[0].id
	for _, c := range []struct {
		name    string
		tcNoise map[string][]string
		global  map[string]map[string][]string
		want    string
	}{
		{"nothing", nil, nil, ""},
		{"the test case's whole body", map[string][]string{"body": {}}, nil, "body (this test case)"},
		{"a global *", nil, map[string]map[string][]string{"body": {"*": {"*"}}}, "body.* (globalNoise)"},
		{"a global field at any depth", nil, map[string]map[string][]string{"body": {"ID": {}}}, "body.ID (globalNoise)"},
		{"a global ancestor", nil, map[string]map[string][]string{"body": {"orders": {}}}, "body.orders (globalNoise)"},
		{"a global path", nil, map[string]map[string][]string{"body": {"orders.id": {}}}, "body.orders.id (globalNoise)"},
		{"a global sibling", nil, map[string]map[string][]string{"body": {"status": {}, "orders.status": {}}}, ""},
		{"the test case's field before the global one", map[string][]string{"body.id": {}}, map[string]map[string][]string{"body": {"orders": {}}}, "body.id (this test case)"},
		{"the test case's path", map[string][]string{"body.orders.id": {}}, nil, "body.orders.id (this test case)"},
		{"a header entry covers no body place", map[string][]string{"header.id": {}}, map[string]map[string][]string{"header": {"id": {}}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, BodyNoiseCovering(keys, c.tcNoise, c.global, zap.NewNop()))
		})
	}
}

// A dotted key names its place as the comparison applies it: as a substring
// of the place's path, wherever in the path it stands (orders.id covers
// data.orders[0].id, and so does rs.id), not only from the root. A prefix
// rule missed a nested order's id, and a use there moved onto another order
// passed unrefused.
func TestBodyNoiseCovering_ADottedKeyAnywhereInThePath(t *testing.T) {
	keys := []string{"data", "orders", "id"} // body.data.orders[0].id
	for _, c := range []struct {
		entry, want string
	}{
		{"orders.id", "body.orders.id (globalNoise)"},
		{"rs.id", "body.rs.id (globalNoise)"},
		{"data.orders", "body.data.orders (globalNoise)"},
		{"orders.status", ""},
	} {
		t.Run(c.entry, func(t *testing.T) {
			global := map[string]map[string][]string{"body": {c.entry: {}}}
			assert.Equal(t, c.want, BodyNoiseCovering(keys, nil, global, zap.NewNop()))
			assert.Equal(t, c.want != "", noiseCoversInTheComparison(t, c.entry), "the comparison applies the entry the same way")
		})
	}
}

// noiseCoversInTheComparison reports whether the comparison ignores a change
// of data.orders[0].id under the global body entry.
func noiseCoversInTheComparison(t *testing.T, entry string) bool {
	t.Helper()
	exp := map[string]interface{}{"data": map[string]interface{}{"orders": []interface{}{map[string]interface{}{"id": "a", "status": "paid"}}}}
	act := map[string]interface{}{"data": map[string]interface{}{"orders": []interface{}{map[string]interface{}{"id": "b", "status": "paid"}}}}
	res, err := JSONDiffWithNoiseControl(NewValidatedJSON(exp, act), map[string][]string{entry: {}}, false, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return res.IsExact()
}
