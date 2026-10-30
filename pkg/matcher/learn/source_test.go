package learn

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// WordFrom reads a value whose every word changed (a test's whole response
// that came back with other data) in work linear in the value's length: each
// side's words are read once as the value's drift is made, never again for
// each word that changed. Read with four times the words, the value takes
// about four times the bytes to read, not sixteen (what reading the other
// side again for each changed word takes). The bytes are counted, not timed:
// the row holds on a loaded box.
func TestWordFrom_ReadsAValueWhoseEveryWordChangedInLinearWork(t *testing.T) {
	if reflect.ValueOf(readings[asciiReading]).Pointer() != reflect.ValueOf(isWord).Pointer() {
		t.Fatalf("readings[asciiReading] is not isWord: clockWord would read the other side in another reading")
	}
	m := &models.Mock{Name: "mock-3", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: models.Method("GET"), URL: "http://config:8080/boot"},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: `{"tenant":"tenantA7K29Q"}`}}}
	value := func(prefix string, n int) string {
		var b strings.Builder
		b.WriteString(`{"words":"`)
		for w := 0; w < n; w++ {
			fmt.Fprintf(&b, "%s%05d ", prefix, w)
		}
		b.WriteString(`"}`)
		return b.String()
	}
	read := func(n int) uint64 {
		rec, live := value("rec", n), value("got", n)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		w, err := NewDriftCache().WordFrom(rec, live, m)
		runtime.ReadMemStats(&after)
		if err != nil || w != "" {
			t.Fatalf("%d words: WordFrom = %q, %v; mock-3's answer holds none of the value's words", n, w, err)
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	// Both sizes are past maxMovedCells' table, so neither compares the
	// word lists in one.
	const small = 600
	one, four := read(small), read(4*small)
	t.Logf("%d changed words: %d bytes; %d: %d bytes", small, one, 4*small, four)
	if four > 8*one {
		t.Fatalf("reading %d changed words took %d bytes, %d words %d: the work grows with the square of the value", 4*small, four, small, one)
	}
}
