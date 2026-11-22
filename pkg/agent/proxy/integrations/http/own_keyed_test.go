package http

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// ownKeysDb is a per-test tier (perTest) a store reads whole or by key
// (integrations.PerTestKeyRanger): by key, the tier's recordings the index
// files under it, in the tier's order. It reports spent the recordings spent
// holds, and fails every keyed read with keyErr. wholeReads counts the
// whole reads. It does not tell which recordings are the open test's own;
// ownersDb does.
type ownKeysDb struct {
	*mockMemDb
	perTest    []*models.Mock
	spent      map[*models.Mock]bool
	keyErr     error
	wholeReads int
}

func (d *ownKeysDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	d.wholeReads++
	return d.perTest, nil
}

func (d *ownKeysDb) Spent(m *models.Mock) bool { return d.spent[m] }

func (d *ownKeysDb) RangePerTestMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	if d.keyErr != nil {
		return d.keyErr
	}
	for _, m := range d.perTest {
		if slices.Contains(ix.Keys(m), key) && !fn(m) {
			return nil
		}
	}
	return nil
}

// ownersDb is ownKeysDb in a store that tells the open test's own
// recordings (integrations.TestOwner): those others does not hold.
type ownersDb struct {
	*ownKeysDb
	others map[*models.Mock]bool
}

func (d *ownersDb) OwnedByOpenTest(m *models.Mock) bool { return !d.others[m] }

var ownSegs = []string{"a", "b", "1", "2"}

// ownRandomPath is a path of one to three segments of ownSegs.
func ownRandomPath(r *rand.Rand) string {
	segs := make([]string, 1+r.Intn(3))
	for i := range segs {
		segs[i] = ownSegs[r.Intn(len(ownSegs))]
	}
	return "/" + strings.Join(segs, "/")
}

// ownRandomRecording is a recording of a random method and path, now and
// then with an X-Amz-Target, a path segment learned as noise or as a read of
// the clock (which files it under the learned key with the proven switch on
// only), recorded with its host, of another kind than HTTP, or of no request.
func ownRandomRecording(r *rand.Rand, i int) *models.Mock {
	method := []string{"GET", "POST"}[r.Intn(2)]
	path := ownRandomPath(r)
	url := path
	if r.Intn(2) == 0 {
		url = "http://svc.local:8080" + path + "?q=1"
	}
	m := httpMock(fmt.Sprintf("rec-%d", i), method, url)
	segs := strings.Count(path, "/")
	switch r.Intn(4) {
	case 0:
		m.Spec.HTTPReq.Header = map[string]string{[]string{"X-Amz-Target", "x-amz-target"}[r.Intn(2)]: []string{"S.Get", "S.Put"}[r.Intn(2)]}
	case 1:
		m.Spec.ReqBodyNoise = map[string][]string{fmt.Sprintf("url.path.%d", r.Intn(segs)): {}}
	case 2:
		m.Spec.Clocks = map[string]models.ClockOffset{fmt.Sprintf("url.path.%d", r.Intn(segs)): {}}
	}
	switch r.Intn(10) {
	case 0:
		m.Kind = models.Mongo
	case 1:
		m.Spec.HTTPReq = nil
	}
	return m
}

// A store that walks its per-test tier by key is asked, for a miss's repeat
// note, only for the recordings under the call's request keys
// (ownRecordingUnder), and answers as the whole read does
// (ownRecordingInRead), with the proven switch on and off: recordings spent
// or of another test count in neither, nor do recordings of another kind
// than HTTP; a recording whose path holds a learned read of the clock is
// filed as the switch reads it. The keyed check never reads the tier whole.
func TestAKeyedOwnRecordingCheckAnswersAsTheWholeRead(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	hits := map[bool]int{}
	for _, proven := range []bool{false, true} {
		t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
		for round := 0; round < 300; round++ {
			d := &ownKeysDb{mockMemDb: &mockMemDb{}, spent: map[*models.Mock]bool{}}
			others := map[*models.Mock]bool{}
			for i, n := 0, r.Intn(8); i < n; i++ {
				m := ownRandomRecording(r, i)
				d.perTest = append(d.perTest, m)
				switch r.Intn(5) {
				case 0:
					d.spent[m] = true
				case 1:
					others[m] = true
				}
			}
			var db interface {
				integrations.MockMemDb
				integrations.PerTestKeyRanger
			} = d
			if r.Intn(2) == 0 {
				db = &ownersDb{ownKeysDb: d, others: others}
			}
			for call := 0; call < 6; call++ {
				method := []string{"GET", "POST"}[r.Intn(2)]
				target := []string{"", "S.Get", "S.Put"}[r.Intn(3)]
				keys := RequestKeysOf(method, ownRandomPath(r), target, r.Intn(4) == 0)
				want := ownRecordingInRead(db, keys)
				d.wholeReads = 0
				got := ownRecordingUnder(db, keys)
				if got != want {
					t.Fatalf("proven %v, round %d, call %d (keys %q): the keyed check says %v, the whole read %v", proven, round, call, keys, got, want)
				}
				if d.wholeReads != 0 {
					t.Fatalf("proven %v, round %d, call %d: the keyed check read the per-test tier whole %d times", proven, round, call, d.wholeReads)
				}
				hits[want]++
			}
		}
	}
	if hits[true] < 100 || hits[false] < 100 {
		t.Fatalf("precondition: the calls found an own recording %d times and none %d times; want both often", hits[true], hits[false])
	}
}

// The index a keyed check reads files what the switch in force reads
// (ownKeyIndexes): a recording whose path segment is a learned read of the
// clock is under its learned key with the switch on, under its recorded path
// with it off; a mock of another kind, or none, is under no key.
func TestTheOwnKeyIndexFilesARecordingAsTheSwitchInForceReadsIt(t *testing.T) {
	m := httpMock("clocked", "GET", "/v1/u/1700000000")
	m.Spec.Clocks = map[string]models.ClockOffset{"url.path.2": {}}
	learned, exact := requestKeyLearned("GET", 3), requestKeyExact("GET", []string{"v1", "u", "1700000000"}, "")
	if keys := ownKeyIndexes[1].Keys(m); !slices.Contains(keys, learned) || slices.Contains(keys, exact) {
		t.Fatalf("switch on: filed under %q, want the learned key %q and not the exact %q", keys, learned, exact)
	}
	if keys := ownKeyIndexes[0].Keys(m); slices.Contains(keys, learned) || !slices.Contains(keys, exact) {
		t.Fatalf("switch off: filed under %q, want the exact key %q and not the learned %q", keys, exact, learned)
	}
	for _, proven := range []bool{false, true} {
		t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
		if got, want := recordedRequestKeys(m), ownKeyIndexes[map[bool]int{false: 0, true: 1}[proven]].Keys(m); !slices.Equal(got, want) {
			t.Fatalf("proven %v: the recording's keys %q, its index's %q", proven, got, want)
		}
	}
	other := httpMock("mongo", "GET", "/v1/u")
	other.Kind = models.Mongo
	for i, ix := range ownKeyIndexes {
		if keys := ix.Keys(other); len(keys) != 0 {
			t.Fatalf("index %d files a mock of another kind under %q", i, keys)
		}
		if keys := ix.Keys(nil); len(keys) != 0 {
			t.Fatalf("index %d files no mock under %q", i, keys)
		}
	}
}

// A keyed per-test read that fails counts as an own recording, as a whole
// read that fails does: no note is the side that hides nothing.
func TestAFailedKeyedOwnRecordingCheckKeepsTheNoteOff(t *testing.T) {
	d := &ownKeysDb{mockMemDb: &mockMemDb{}, keyErr: errors.New("read failed")}
	if !ownRecordingUnder(d, RequestKeysOf("GET", "/a", "", false)) {
		t.Fatal("a failed keyed read counts as no own recording; want one")
	}
	if d.wholeReads != 0 {
		t.Fatalf("a failed keyed read read the tier whole %d times", d.wholeReads)
	}
}
