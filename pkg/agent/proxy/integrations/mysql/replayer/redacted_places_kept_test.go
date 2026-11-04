package replayer

import (
	"fmt"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// keptMock is a recording of the commands reqs under the patterns noise.
func keptMock(noise []string, reqs ...mysql.Request) *models.Mock {
	m := &models.Mock{Name: "kept", Kind: models.MySQL, Noise: noise}
	m.Spec.MySQLRequests = reqs
	return m
}

func keptQuery(sql string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{Message: &mysql.QueryPacket{Query: sql}}}
}

func keptPrepare(sql string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{Message: &mysql.StmtPreparePacket{Query: sql}}}
}

// keptExecute is an EXECUTE of the stored statement sql binding vals.
func keptExecute(sql string, vals ...string) mysql.Request {
	prms := make([]mysql.Parameter, 0, len(vals))
	for _, v := range vals {
		prms = append(prms, mysql.Parameter{Type: 254, Value: v})
	}
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Meta:    map[string]string{mysql.MetaQuery: sql},
		Message: &mysql.StmtExecutePacket{Parameters: prms},
	}}
}

// keptExecuteBytes is keptExecute binding each value as bytes, as a blob
// or a binary string is bound.
func keptExecuteBytes(sql string, vals ...string) mysql.Request {
	r := keptExecute(sql)
	msg := r.PacketBundle.Message.(*mysql.StmtExecutePacket)
	for _, v := range vals {
		msg.Parameters = append(msg.Parameters, mysql.Parameter{Type: 252, Value: []byte(v)})
	}
	return r
}

func keptChunk(data string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{Message: &mysql.StmtSendLongDataPacket{Data: []byte(data)}}}
}

// A recording's places are kept by what they are read from: two recordings
// that differ in any one part read have each their own places, whichever is
// read first, and a recording read again, or a copy of it, is not read
// again.
func TestMySQLKeptPlacesAreKeptByEveryPartTheyAreReadFrom(t *testing.T) {
	keyed := func(k string) string { return "SELECT name FROM t WHERE k = '" + k + "'" }
	stored := func(k string) string { return "SELECT name FROM t WHERE k = '" + k + "' AND id = ?" }
	for _, tc := range []struct {
		name string
		a, b *models.Mock
	}{
		{"the patterns", keptMock(redactedNoise, keptQuery(keyed(redactedPlaceholder))),
			keptMock([]string{"^nomatch$"}, keptQuery(keyed(redactedPlaceholder)))},
		{"a COM_QUERY's statement", keptMock(redactedNoise, keptQuery(keyed(redactedPlaceholder))),
			keptMock(redactedNoise, keptQuery(keyed("other")))},
		{"a COM_STMT_PREPARE's statement", keptMock(redactedNoise, keptPrepare(keyed(redactedPlaceholder))),
			keptMock(redactedNoise, keptPrepare(keyed("other")))},
		{"an EXECUTE's stored statement", keptMock(redactedNoise, keptExecute(stored(redactedPlaceholder), "1")),
			keptMock(redactedNoise, keptExecute(stored("other"), "1"))},
		{"an EXECUTE's bound value", keptMock(redactedNoise, keptExecute(stored("a"), redactedPlaceholder)),
			keptMock(redactedNoise, keptExecute(stored("a"), "other"))},
		{"an EXECUTE's second bound value", keptMock(redactedNoise, keptExecute(stored("a"), "a", redactedPlaceholder)),
			keptMock(redactedNoise, keptExecute(stored("a"), "a", "other"))},
		{"an EXECUTE's bound bytes", keptMock(redactedNoise, keptExecuteBytes(stored("a"), redactedPlaceholder)),
			keptMock(redactedNoise, keptExecuteBytes(stored("a"), "Oth3rValue1234"))},
		{"an EXECUTE's bound bytes' bounds", keptMock(redactedNoise, keptExecuteBytes(stored("a"), redactedPlaceholder, "Q")),
			keptMock(redactedNoise, keptExecuteBytes(stored("a"), redactedPlaceholder[:12], redactedPlaceholder[12:]+"Q"))},
		// The same text bound, split into two values at another byte.
		{"an EXECUTE's bound values' bounds", keptMock(redactedNoise, keptExecute(stored("a"), redactedPlaceholder, "Q")),
			keptMock(redactedNoise, keptExecute(stored("a"), redactedPlaceholder[:12], redactedPlaceholder[12:]+"Q"))},
		{"a streamed chunk's data", keptMock(redactedNoise, keptChunk(redactedPlaceholder)),
			keptMock(redactedNoise, keptChunk("other"))},
		// The same bytes streamed as one chunk and as two, the second's
		// kind written between them.
		{"a streamed chunk's bounds", keptMock([]string{"^xdy$"}, keptChunk("xdy")),
			keptMock([]string{"^xdy$"}, keptChunk("x"), keptChunk("y"))},
		// The same text, read as a statement and as a chunk.
		{"a command's kind", keptMock(redactedNoise, keptQuery(redactedPlaceholder)),
			keptMock(redactedNoise, keptChunk(redactedPlaceholder))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantA, wantB := readPlaces(tc.a), readPlaces(tc.b)
			if reflect.DeepEqual(wantA, wantB) {
				t.Fatalf("precondition: the two recordings read the same places %+v", wantA)
			}
			for _, order := range [][2]*models.Mock{{tc.a, tc.b}, {tc.b, tc.a}} {
				ForgetPlacesForTest()
				placesOf(order[0])
				for _, m := range []*models.Mock{tc.a, tc.b} {
					want := wantA
					if m == tc.b {
						want = wantB
					}
					if got := placesOf(m); !reflect.DeepEqual(got, want) {
						t.Errorf("read after the other recording, placesOf = %+v; want its own places %+v", got, want)
					}
				}
			}
			ForgetPlacesForTest()
			before := PlacesReadsForTest()
			placesOf(tc.a)
			placesOf(tc.a)
			placesOf(tc.a.DeepCopy())
			if n := PlacesReadsForTest() - before; n != 1 {
				t.Errorf("a recording, read again and as a copy, was read for its places %d time(s); want 1", n)
			}
		})
	}
	// A command with no message adds no place, whatever its kind.
	t.Run("commands with no message", func(t *testing.T) {
		m := keptMock(redactedNoise, keptQuery(keyed(redactedPlaceholder)),
			mysql.Request{PacketBundle: mysql.PacketBundle{Message: (*mysql.QueryPacket)(nil)}},
			mysql.Request{PacketBundle: mysql.PacketBundle{Message: (*mysql.StmtPreparePacket)(nil)}},
			mysql.Request{PacketBundle: mysql.PacketBundle{Message: (*mysql.StmtExecutePacket)(nil)}},
			mysql.Request{PacketBundle: mysql.PacketBundle{Message: (*mysql.StmtSendLongDataPacket)(nil)}})
		ForgetPlacesForTest()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("reading a recording with commands of no message panics: %v", r)
			}
		}()
		if got, want := placesOf(m), readPlaces(keptMock(redactedNoise, keptQuery(keyed(redactedPlaceholder)))); !reflect.DeepEqual(got, want) {
			t.Errorf("placesOf = %+v; want %+v", got, want)
		}
	})
	ForgetPlacesForTest()
	before := PlacesReadsForTest()
	if p := placesOf(keptMock(nil, keptQuery(keyed(redactedPlaceholder)))); p.redactsAny() {
		t.Errorf("a recording with no patterns has places %+v; want none", p)
	}
	if n := PlacesReadsForTest() - before; n != 0 {
		t.Errorf("a recording with no patterns was read for its places %d time(s); want 0", n)
	}
}

// The places kept hold at most their bound, by the bytes each holds: a new
// recording's places drop others until they fit, a recording's places kept
// again replace their own, below the bound and at it, and places larger
// than the bound are not kept.
func TestMySQLKeptPlacesHoldAtMostTheirBound(t *testing.T) {
	p := readPlaces(keptMock(redactedNoise, keptQuery("SELECT name FROM t WHERE k = '"+redactedPlaceholder+"'")))
	size := p.size()
	k := keptPlaces{byKey: map[placesDigest]keptEntry{}, max: 3 * size}
	check := func(step string, wantLen int, mustHold ...placesDigest) {
		t.Helper()
		sum := 0
		for _, e := range k.byKey {
			sum += e.size
		}
		if k.bytes != sum || k.bytes > k.max {
			t.Errorf("%s: kept bytes %d, entries sum to %d, bound %d", step, k.bytes, sum, k.max)
		}
		if len(k.byKey) != wantLen {
			t.Errorf("%s: %d places kept; want %d", step, len(k.byKey), wantLen)
		}
		for _, key := range mustHold {
			if got, ok := k.get(key); !ok || !reflect.DeepEqual(got, p) {
				t.Errorf("%s: the places of %v are not kept", step, key)
			}
		}
	}
	keys := []placesDigest{{1, 1}, {2, 2}, {3, 3}, {4, 4}}
	k.put(keys[0], p, size)
	k.put(keys[0], p, size)
	check("one place kept twice, the bound not reached", 1, keys[0])
	for i := 1; i < 3; i++ {
		k.put(keys[i], p, size)
	}
	check("three places of the bound's three", 3, keys[0], keys[1], keys[2])
	k.put(keys[0], p, size)
	check("one of them kept again", 3, keys[0], keys[1], keys[2])
	k.put(keys[3], p, size)
	check("a fourth", 3, keys[3])
	k.put(placesDigest{5, 5}, p, k.max+1)
	if _, ok := k.get(placesDigest{5, 5}); ok {
		t.Errorf("places larger than the bound are kept")
	}
	check("places larger than the bound", 3, keys[3])
}

// A pool's places are summed into maps of the sum's own: summing them, or
// adding one recording's places to another's (or), leaves each recording's
// kept places as it redacted them, and the places of a pool of recordings
// already read sum into a few maps, not one or two for each recording.
func TestMySQLPoolPlacesLeaveEachRecordingsKeptPlacesAsTheyWere(t *testing.T) {
	ForgetPlacesForTest()
	t.Cleanup(ForgetPlacesForTest)
	keyed := func(a, b string) string { return "SELECT name FROM t WHERE a = '" + a + "' AND b = '" + b + "'" }
	const stmt = "SELECT 1 FROM t WHERE id = ? AND org = ?"
	a := keptMock(redactedNoise, keptQuery(keyed(redactedPlaceholder, "x")), keptExecute(stmt, redactedPlaceholder, "1"))
	b := keptMock(redactedNoise, keptQuery(keyed("x", redactedPlaceholder)), keptExecute(stmt, "1", redactedPlaceholder))
	wantA, wantB := readPlaces(a), readPlaces(b)
	if len(wantA.literals) != 1 || len(wantA.params) != 1 || reflect.DeepEqual(wantA.literals, wantB.literals) || reflect.DeepEqual(wantA.params, wantB.params) {
		t.Fatalf("precondition: each recording redacts one literal and one bound value, the two at other positions: %+v, %+v", wantA, wantB)
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("summing places panics: %v", r)
			}
		}()
		if sum := poolPlaces([]*models.Mock{a, b}); len(sum.literals) != 2 || len(sum.params) != 2 {
			t.Errorf("the pool's places are %+v; want both literals and both bound values", sum)
		}
		_ = placesOf(a).or(placesOf(b))
		_ = placesOf(b).or(placesOf(a))
	}()
	if got := placesOf(a); !reflect.DeepEqual(got, wantA) {
		t.Errorf("after the sums, the first recording's places are %+v; want %+v", got, wantA)
	}
	if got := placesOf(b); !reflect.DeepEqual(got, wantB) {
		t.Errorf("after the sums, the second recording's places are %+v; want %+v", got, wantB)
	}

	const n = 1000
	pool := make([]*models.Mock, 0, n)
	for i := 0; i < n; i++ {
		pool = append(pool, keptMock(redactedNoise,
			keptQuery(fmt.Sprintf("SELECT v FROM t%d WHERE k = '%s'", i, redactedPlaceholder)),
			keptExecute(stmt, redactedPlaceholder, "1")))
	}
	poolPlaces(pool)
	if allocs := testing.AllocsPerRun(10, func() { poolPlaces(pool) }); allocs >= n/10 {
		t.Errorf("summing the places of %d recordings read before allocates %.0f objects; want a few maps, not one for each recording", n, allocs)
	}
}

// A bound value of bytes is read into a recording's digest as it is, not
// copied: a blob costs no copy of itself each time the recording that binds
// it answers (the answer notes its places, placesOf).
func TestMySQLPlacesKeyReadsBoundBytesInPlace(t *testing.T) {
	const stmt = "SELECT name FROM t WHERE k = ?"
	blob := keptMock(redactedNoise, keptExecuteBytes(stmt, string(make([]byte, 64<<10))))
	small := keptMock(redactedNoise, keptExecuteBytes(stmt, "x"))
	if a, b := testing.AllocsPerRun(20, func() { placesKey(blob) }), testing.AllocsPerRun(20, func() { placesKey(small) }); a > b {
		t.Errorf("the digest of a recording binding 64 KiB of bytes allocates %v times, of one binding a byte %v; want no more", a, b)
	}
	if placesKey(blob) == placesKey(small) {
		t.Errorf("two recordings binding other bytes have one digest")
	}
}
