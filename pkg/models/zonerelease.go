package models

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"sync"
	"time"
)

//go:generate go run ../../tools/zonereleases -o zonereleases_data.go -zi testdata/tzdata.zi -sums ../../tools/zonereleases/tzdata.sha256

// zoneDigestFrom and zoneDigestTo bound the offsets a zone file's digest
// (ZoneRulesDigest) reads: from 1970, where tzdata merges two zones that
// agree from it on, to 2038, as a version 1 or "fat" zone file lists no
// change of offset after 2037, where a version 2 file's footer rule goes
// on.
var (
	zoneDigestFrom = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	zoneDigestTo   = time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ZoneRulesDigest is a digest of the offsets from UTC loc reads from 1970
// up to to: its offset at 1970's first instant, then the instant of each
// change of it and the offset from there, read at every change of offset
// it makes (ZoneEnd), so two files that read one offset at every instant
// there (a slim and a fat file of one zone, of one release or of two) have
// one digest, and two that read another at any instant have two (but for a
// collision of its 64 bits of SHA-256).
func ZoneRulesDigest(loc *time.Location, to time.Time) uint64 {
	var b []byte
	last, first := 0, true
	for t := zoneDigestFrom; t.Before(to); {
		tt := t.In(loc)
		_, off := tt.Zone()
		if first || off != last {
			b = binary.LittleEndian.AppendUint64(b, uint64(t.Unix()))
			b = binary.LittleEndian.AppendUint64(b, uint64(int64(off)))
			last, first = off, false
		}
		end := ZoneEnd(tt)
		if end.IsZero() {
			break
		}
		t = end.UTC()
	}
	sum := sha256.Sum256(b)
	return binary.LittleEndian.Uint64(sum[:8])
}

// zoneReleasesOf is what zoneReleases holds of name: the digest
// (ZoneRulesDigest, to 2038) of its zone in ZoneReleasesLast's main data
// (0 where that release has no such name), and the sorted digests of it
// in every build of every release of IANA's zone data from
// ZoneReleasesFirst to ZoneReleasesLast. A release is read in the builds
// its files are found in: its main data, and, from 2022b, its main data
// with backzone's zones that zone.tab lists (PACKRATLIST=zone.tab, as a
// distribution may build it), each compiled by the release's own zic (as
// Go's zoneinfo.zip is) and by ZoneReleasesLast's. Two builds of one
// release read other offsets after 1970 for a few names (zic 2022g to
// 2023c compiled America/Ojinaga's 2022 change an hour off).
func zoneReleasesOf(name string) (newest uint64, all []uint64, ok bool) {
	z, ok := zoneReleases[name]
	if !ok || len(z) < 2 {
		return 0, nil, false
	}
	return z[0], z[1:], true
}

// ZoneReading is a zone file's offsets from 1970 to 2038 as zoneReleases
// and a ZoneSource read them (ReadZoneReleases): their digest.
type ZoneReading struct {
	all uint64
}

// Digest is the file's digest (ZoneRulesDigest, to 2038).
func (r ZoneReading) Digest() uint64 { return r.all }

// ReadZoneReleases is loc's ZoneReading.
func ReadZoneReleases(loc *time.Location) ZoneReading {
	return ZoneReading{all: ZoneRulesDigest(loc, zoneDigestTo)}
}

// OfARelease reports whether the file read the offsets from 1970 to 2038
// that some release of IANA's zone data from ZoneReleasesFirst to
// ZoneReleasesLast gives name (zoneReleases): a file of name of that
// release, whatever its build (slim or fat, its own zic or another), or of
// another name whose rules in a release are name's in one (Asia/Calcutta's
// of Asia/Kolkata, whose link it is; Asia/Urumqi's of Antarctica/Vostok,
// which read Urumqi's rules from 1970 until tzdata 2023d corrected
// Vostok's history).
func (r ZoneReading) OfARelease(name string) bool {
	_, all, ok := zoneReleasesOf(name)
	return ok && holds(all, r.all)
}

// Of reports whether a zone file of name compiled from src (the zone
// source a root's own zone data came with: its tzdata.zi) reads the file's
// offsets from 1970 to 2038: a file of name of src's release, whichever
// release that is, one later than ZoneReleasesLast among them.
func (r ZoneReading) Of(src *ZoneSource, name string) bool {
	return src != nil && src.Gives(name, r.all)
}

// Releases is the names some release of IANA's zone data from
// ZoneReleasesFirst to ZoneReleasesLast reads with the file's offsets from
// 1970 to 2038 (zoneReleases), sorted; none where no release does.
func (r ZoneReading) Releases() []string {
	zoneReleaseNames.mu.Lock()
	defer zoneReleaseNames.mu.Unlock()
	if zoneReleaseNames.m == nil {
		zoneReleaseNames.m = map[uint64][]string{}
		for name := range zoneReleases {
			_, all, _ := zoneReleasesOf(name)
			for _, d := range all {
				zoneReleaseNames.m[d] = append(zoneReleaseNames.m[d], name)
			}
		}
		for _, names := range zoneReleaseNames.m {
			sort.Strings(names)
		}
	}
	return zoneReleaseNames.m[r.all]
}

// SetZoneReleasesForTest makes zoneReleases releases (each name's newest
// digest, then its sorted digests), for rows of other packages that need a
// table no release gives; the returned func puts it back. Never called
// outside a test, nor from a parallel one: it writes it unguarded.
func SetZoneReleasesForTest(releases map[string][]uint64) (restore func()) {
	was := zoneReleases
	set := func(r map[string][]uint64) {
		zoneReleases = r
		zoneReleaseNames.mu.Lock()
		zoneReleaseNames.m = nil
		zoneReleaseNames.mu.Unlock()
	}
	set(releases)
	return func() { set(was) }
}

// zoneReleaseNames is zoneReleases by digest (ZoneReading.Releases), made
// at its first read.
var zoneReleaseNames struct {
	mu sync.Mutex
	m  map[uint64][]string
}

// holds reports whether the sorted digests ds hold d.
func holds(ds []uint64, d uint64) bool {
	i := sort.Search(len(ds), func(i int) bool { return ds[i] >= d })
	return i < len(ds) && ds[i] == d
}
