package http

import (
	"encoding/binary"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// learnKeyIndexes holds an index for each url noise a learned call read
// through (learnKeyIndexOf), by its patterns, at most noiseIndexesCap of
// them.
var learnKeyIndexes struct {
	mu sync.Mutex
	m  map[string]*integrations.MockIndex
}

// learnKeyIndexOf is the index that files each HTTP recording, under n's
// url noise, as the learn step reads it with no index of its own
// (keyedLearnEntries), and as buildLearnIndex and learnIndex.positions file
// and read a staging generation: under its method and its path with the url
// noise masked (learnPathKey); by its body, under its print
// (learnPlainPrint: learnPrintKey), or under learnUnprintedKey where the
// learn step compares its body otherwise; and, where a segment of its path
// may be excused (learnsWide), under its method and its masked path's
// segment count (learnWideKey). A recording the learn step's scope never
// places (one of no request, or one that declares variables) is filed under
// none.
func learnKeyIndexOf(n learnNoise) *integrations.MockIndex {
	cfg := urlNoiseKey(n.url)
	learnKeyIndexes.mu.Lock()
	defer learnKeyIndexes.mu.Unlock()
	if ix := learnKeyIndexes.m[cfg]; ix != nil {
		return ix
	}
	if learnKeyIndexes.m == nil || len(learnKeyIndexes.m) >= noiseIndexesCap {
		learnKeyIndexes.m = map[string]*integrations.MockIndex{}
	}
	url := learnNoise{url: n.url}
	ix := &integrations.MockIndex{Keys: func(m *models.Mock) []string { return learnKeys(m, url) }}
	learnKeyIndexes.m[cfg] = ix
	return ix
}

// learnKeys are the keys learnKeyIndexOf files m under, url noise n.
func learnKeys(m *models.Mock, n learnNoise) []string {
	if m.Kind != models.Kind(models.HTTP) || m.Spec.HTTPReq == nil || hasVariables(m) {
		return nil
	}
	rp := recordedParts(m)
	masked := n.maskedPath(rp.path)
	k := rp.method + " " + masked
	body := learnUnprintedKey(k)
	if sum, ok := learnPlainPrint(m, rp); ok {
		body = learnPrintKey(k, sum)
	}
	keys := []string{learnPathKey(k), body}
	if learnsWide(m) {
		keys = append(keys, learnWideKey(rp.method, segmentCount(masked)))
	}
	return keys
}

// The keys of learnKeyIndexOf: each kind begins with a letter of its own,
// and a method holds no space, so no two kinds, methods or paths share one.

// learnPathKey is the key of the recordings of k, a method and a masked
// path.
func learnPathKey(k string) string { return "p" + k }

// learnUnprintedKey is the key of the recordings of k whose body the learn
// step compares otherwise than by its print (learnPlainPrint).
func learnUnprintedKey(k string) string { return "u" + k }

// learnPrintKey is the key of the recordings of k whose body has the print
// sum. The print is its last 8 bytes, so two keys are one only where both k
// and sum are.
func learnPrintKey(k string, sum uint64) string {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], sum)
	return "b" + k + string(b[:])
}

// learnWideKey is the key of the recordings of method a segment of whose
// path may be excused (learnsWide), their masked path of segments segments.
func learnWideKey(method string, segments int) string {
	return "w" + method + " " + strconv.Itoa(segments)
}

// keyedLearnEntries are the entries a learned call of method, its masked
// path masked and its body of the print body where printed
// (learnLivePrint), may be, read through db's key indexes
// (learnKeyIndexOf), each tier in recorded order (inRecordedOrder), the
// per-test tier first: what learnIndex.positions gives of an index of the
// same reads, so scopeOfEntries places of them what a read's scope places
// that exactGroup may take. ok is false where the call reads the store
// instead: db is under exact serving or a stateful sequence view (whose
// index is the learn step's own, learnIndexes), reads no key, or a keyed
// read failed (the read then reports what fails).
//
// The reads are those learnReads makes (GetPerTestMocksInWindow and
// GetSessionMocks), narrowed to the keys: a recording the store let go is in
// neither, and a spent one the read holds is in both, which the scope tells
// as the read's scope tells it.
func keyedLearnEntries(db integrations.MockMemDb, n learnNoise, method, masked string, body uint64, printed bool) ([]learnEntry, bool) {
	if integrations.ExactServing(db) {
		return nil, false
	}
	if _, exact := integrations.SequenceViewOf(db); exact {
		return nil, false
	}
	pr, ok := db.(integrations.PerTestKeyRanger)
	if !ok {
		return nil, false
	}
	sr, ok := db.(integrations.SessionKeyReader)
	if !ok {
		return nil, false
	}
	ix := learnKeyIndexOf(n)
	k := method + " " + masked
	keys := []string{learnPathKey(k)}
	if printed {
		keys = []string{learnPrintKey(k, body), learnUnprintedKey(k)}
	}
	keys = append(keys, learnWideKey(method, segmentCount(masked)))
	// A recording filed under two of keys (one that learnsWide) is read
	// once in each tier.
	var tiers [2][]*models.Mock
	seen := [2]map[*models.Mock]bool{{}, {}}
	for _, key := range keys {
		for t := range tiers {
			add := func(mk *models.Mock) bool {
				if !seen[t][mk] {
					seen[t][mk] = true
					tiers[t] = append(tiers[t], mk)
				}
				return true
			}
			read := pr.RangePerTestMocksWithKey
			if t == 1 {
				read = sr.RangeSessionMocksWithKey
			}
			if err := read(ix, key, add); err != nil {
				return nil, false
			}
		}
	}
	url := learnNoise{url: n.url}
	var out []learnEntry
	for t, tier := range tiers {
		for _, m := range inRecordedOrder(tier) {
			out = append(out, learnEntry{m: m, perTest: t == 0, masked: url.maskedPath(recordedParts(m).path)})
		}
	}
	return out, true
}
