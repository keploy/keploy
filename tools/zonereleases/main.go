// Command zonereleases writes keploy's table of IANA's zone data
// (pkg/models/zonereleases_data.go): for each zone and link name of every
// release of tzdata from 2014a to the newest IANA publishes (or -last),
// the digests (models.ZoneRulesDigest) of its offsets from 1970 to 2038 in
// each build of each release. A build is a release's main data, or, from
// 2022b, its main data with the backzone zones zone.tab lists
// (PACKRATDATA=backzone PACKRATLIST=zone.tab, as a distribution may build
// it), compiled by the release's own zic (as Go's zoneinfo.zip is) or by
// the newest release's. What `go generate ./pkg/models/` runs:
//
//	go run ../../tools/zonereleases -o zonereleases_data.go -zi testdata/tzdata.zi -sums ../../tools/zonereleases/tzdata.sha256
//
// It writes the newest release's tzdata.zi (-zi) too, which the models
// package's tests read: IANA's own list of the release's names, and the
// source keploy's reading of a tzdata.zi (models.ZoneSource) must read as
// zic compiled it.
//
// The names are the release's own: those its Zone and Link lines define
// (models.ParseZoneSource), each of which zic must have written a file of,
// and no other file but zic's own temporary ones (".zic" and six more
// letters: zic 2021c to 2022e leaves them). Every zone.tab and
// zone1970.tab name must be one of them. Each file compiled by the newest
// zic must read the offsets models.ZoneSource reads its name with from the
// same source, so keploy's reading of a root's tzdata.zi is checked on
// every release: all but a zone that keeps summer time all year
// (ZoneSource.SummerAllYear), whose slim file's footer Go reads by the UTC
// year, which ZoneSource reads as the fat file lists it (it logs each).
//
// It needs the network (https://data.iana.org/time-zones/releases/), a C
// compiler (cc), make and awk, which build each release's zic and its
// backzone data. Each tarball must hold the sha256 -sums pins for it, and
// name its release (checkRelease); a tarball -sums does not list yet is
// added to it. The
// output depends on nothing but the releases read: two runs give one file.
// -dir keeps the downloaded and built releases there for a later run.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

const releasesURL = "https://data.iana.org/time-zones/releases/"

// first is the oldest release read: older ones are a decade gone from any
// distribution still maintained.
const first = "2014a"

// mainData are the files of a release's main data that may be there,
// which zic compiles with backward (as Go's update.bash does).
var mainData = []string{"africa", "antarctica", "asia", "australasia", "europe", "northamerica", "southamerica", "pacificnew", "etcetera", "factory"}

var (
	digestTo    = time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	newsRelease = regexp.MustCompile(`(?m)^Release ([0-9]{4}[a-z]) - [0-9]{4}-[0-9]{2}-[0-9]{2}`)
	makeVersion = regexp.MustCompile(`(?m)^VERSION=\s*(\S+)\s*$`)
	// zicTemp is the name of a temporary file zic writes and renames (and
	// zic 2021c to 2022e leaves behind beside a link).
	zicTemp = regexp.MustCompile(`^\.zic[A-Za-z0-9]{6}$`)
)

// client fetches from IANA, never waiting on it for long.
var client = &http.Client{Timeout: 2 * time.Minute}

func main() {
	out := flag.String("o", "zonereleases_data.go", "the file to write")
	sumsPath := flag.String("sums", "", "the sha256 pins of the tarballs (read, and added to)")
	dir := flag.String("dir", "", "where to keep the downloaded and built releases (a temporary directory if empty)")
	last := flag.String("last", "", "the newest release to read (the newest IANA publishes if empty)")
	ziPath := flag.String("zi", "", "where to write the newest release's tzdata.zi (none if empty)")
	flag.Parse()
	if *sumsPath == "" {
		log.Fatal("-sums names no file")
	}
	work := *dir
	if work == "" {
		var err error
		if work, err = os.MkdirTemp("", "zonereleases-"); err != nil {
			log.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(work) }()
	}
	sums, err := readSums(*sumsPath)
	if err != nil {
		log.Fatal(err)
	}
	listing, err := get(releasesURL)
	if err != nil {
		log.Fatal(err)
	}
	releases, err := releasesIn(listing, *last)
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range releases {
		for _, kind := range []string{"tzdata", "tzcode"} {
			if err := fetch(work, kind+r+".tar.gz", sums); err != nil {
				log.Fatal(err)
			}
		}
		if err := checkRelease(work, r); err != nil {
			log.Fatal(err)
		}
	}
	if err := writeSums(*sumsPath, sums); err != nil {
		log.Fatal(err)
	}
	t, err := build(work, releases)
	if err != nil {
		log.Fatal(err)
	}
	src, err := t.render()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		log.Fatal(err)
	}
	if *ziPath != "" {
		zi, err := tzdataZi(work, t.last)
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(*ziPath, zi, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	log.Printf("%s: %d names of tzdata %s to %s", *out, len(t.all), t.first, t.last)
}

// get is the body of url, at most 64 MiB.
func get(url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// releasesIn is every tzdata release IANA's listing names from first on,
// up to last where it is set (which it must name), oldest first.
func releasesIn(listing []byte, last string) ([]string, error) {
	seen := map[string]bool{}
	var rs []string
	for _, m := range regexp.MustCompile(`tzdata([0-9]{4}[a-z])\.tar\.gz`).FindAllSubmatch(listing, -1) {
		if r := string(m[1]); r >= first && (last == "" || r <= last) && !seen[r] {
			seen[r] = true
			rs = append(rs, r)
		}
	}
	sort.Strings(rs)
	if len(rs) == 0 {
		return nil, fmt.Errorf("%s lists no release", releasesURL)
	}
	if last != "" && rs[len(rs)-1] != last {
		return nil, fmt.Errorf("%s lists no release %s", releasesURL, last)
	}
	return rs, nil
}

// readSums reads the pins: lines of "<sha256>  <tarball>".
func readSums(path string) (map[string]string, error) {
	sums := map[string]string{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return sums, nil
	} else if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 || len(f[0]) != 64 {
			return nil, fmt.Errorf("%s:%d: not \"<sha256>  <tarball>\"", path, n)
		}
		sums[f[1]] = f[0]
	}
	return sums, sc.Err()
}

// writeSums writes the pins, sorted.
func writeSums(path string, sums map[string]string) error {
	names := make([]string, 0, len(sums))
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	var b bytes.Buffer
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", sums[n], n)
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// fetch downloads name into work, unless it is there, and checks it
// against its pin in sums, adding one where sums has none.
func fetch(work, name string, sums map[string]string) error {
	dst := filepath.Join(work, name)
	b, err := os.ReadFile(dst)
	if errors.Is(err, fs.ErrNotExist) {
		if b, err = get(releasesURL + name); err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return pin(name, b, sums)
}

// pin checks b, the tarball name, against its pin in sums, or adds one.
func pin(name string, b []byte, sums map[string]string) error {
	sum := sha256.Sum256(b)
	got := hex.EncodeToString(sum[:])
	if want, ok := sums[name]; ok && want != got {
		return fmt.Errorf("%s: sha256 %s, but it is pinned at %s", name, got, want)
	}
	sums[name] = got
	return nil
}

// checkRelease fails unless the tarballs of release r in work are r's:
// each one's version file names r (from 2016h); before, the tzcode one's
// NEWS names r first, and the tzdata one's Makefile names r in its
// VERSION line (but 2016g's, which names none).
func checkRelease(work, r string) error {
	for _, kind := range []string{"tzcode", "tzdata"} {
		tgz := filepath.Join(work, kind+r+".tar.gz")
		files, err := readTar(tgz, "version", "NEWS", "Makefile")
		if err != nil {
			return err
		}
		var got []byte
		switch v, ok := files["version"]; {
		case ok:
			got = bytes.TrimSpace(v)
		case kind == "tzcode":
			if m := newsRelease.FindSubmatch(files["NEWS"]); m != nil {
				got = m[1]
			}
		default:
			m := makeVersion.FindSubmatch(files["Makefile"])
			if m != nil && string(m[1]) == "unknown" {
				continue
			} else if m != nil {
				got = m[1]
			}
		}
		if string(got) != r {
			return fmt.Errorf("%s names release %q, not %s", tgz, got, r)
		}
	}
	return nil
}

// readTar is the regular files of tgz of the names given.
func readTar(tgz string, names ...string) (map[string][]byte, error) {
	found := map[string][]byte{}
	err := untar(tgz, func(h *tar.Header, r io.Reader) error {
		if slices.Contains(names, h.Name) && h.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(io.LimitReader(r, 16<<20))
			found[h.Name] = b
			return err
		}
		return nil
	})
	return found, err
}

func untar(tgz string, each func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if err := each(h, tr); err != nil {
			return err
		}
	}
}

// extract unpacks a release's tzdata and tzcode into work/src-<r>.
func extract(work, r string) (string, error) {
	src := filepath.Join(work, "src-"+r)
	if _, err := os.Stat(filepath.Join(src, "Makefile")); err == nil {
		return src, nil
	}
	for _, kind := range []string{"tzdata", "tzcode"} {
		err := untar(filepath.Join(work, kind+r+".tar.gz"), func(h *tar.Header, rd io.Reader) error {
			if h.Typeflag != tar.TypeReg || strings.Contains(h.Name, "/") || strings.Contains(h.Name, "..") {
				return nil
			}
			b, err := io.ReadAll(rd)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(src, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(src, h.Name), b, 0o644)
		})
		if err != nil {
			return "", err
		}
	}
	return src, nil
}

// buildZic builds release r's zic, with today's C compilers: the C23
// attributes a release's private.h may write before "static" are written
// as GNU's, and a Makefile that lists pacificnew finds it.
func buildZic(work, r string) (string, error) {
	src, err := extract(work, r)
	if err != nil {
		return "", err
	}
	zic := filepath.Join(src, "zic")
	if _, err := os.Stat(zic); err == nil {
		return zic, nil
	}
	p := filepath.Join(src, "private.h")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	b = bytes.ReplaceAll(b, []byte("[[noreturn]]"), []byte("__attribute__((noreturn))"))
	b = bytes.ReplaceAll(b, []byte("[[reproducible]]"), []byte("__attribute__((pure))"))
	b = bytes.ReplaceAll(b, []byte("[[unsequenced]]"), []byte("__attribute__((const))"))
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return "", err
	}
	if mk, err := os.ReadFile(filepath.Join(src, "Makefile")); err == nil && bytes.Contains(mk, []byte("pacificnew")) {
		if _, err := os.Stat(filepath.Join(src, "pacificnew")); err != nil {
			if err := os.WriteFile(filepath.Join(src, "pacificnew"), nil, 0o644); err != nil {
				return "", err
			}
		}
	}
	cmd := exec.Command("make", "-C", src, "-s", "CC=cc", "CFLAGS=-O1 -w", "zic")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("make zic of tzcode%s: %v\n%s", r, err, out)
	}
	return zic, nil
}

// input is the source zic compiles for a build of release r: its main data
// and backward, or, packrat, its main.zi of PACKRATDATA=backzone
// PACKRATLIST=zone.tab (false where its Makefile has no PACKRATLIST).
func input(work, r string, packrat bool) ([]byte, bool, error) {
	src := filepath.Join(work, "src-"+r)
	if !packrat {
		var b []byte
		for _, f := range append(slices.Clone(mainData), "backward") {
			x, err := os.ReadFile(filepath.Join(src, f))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, false, err
			}
			b = append(append(b, x...), '\n')
		}
		return b, true, nil
	}
	mk, err := os.ReadFile(filepath.Join(src, "Makefile"))
	if err != nil {
		return nil, false, err
	}
	if !bytes.Contains(mk, []byte("PACKRATLIST")) {
		return nil, false, nil
	}
	zi := filepath.Join(src, "packrat.zi")
	if b, err := os.ReadFile(zi); err == nil {
		return b, true, nil
	}
	cmd := exec.Command("make", "-C", src, "-s", "PACKRATDATA=backzone", "PACKRATLIST=zone.tab", "main.zi")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, false, fmt.Errorf("make main.zi of tzdata%s: %v\n%s", r, err, out)
	}
	// Out of the way of the main data's main.zi, which tzdata.zi is made of.
	if err := os.Rename(filepath.Join(src, "main.zi"), zi); err != nil {
		return nil, false, err
	}
	b, err := os.ReadFile(zi)
	return b, true, err
}

// tzdataZi is release r's tzdata.zi, as its Makefile makes it of its main
// data: the source a distribution installs beside its zone files.
func tzdataZi(work, r string) ([]byte, error) {
	src := filepath.Join(work, "src-"+r)
	// Made again, with VERSION named: else the Makefile, finding no git,
	// reads its version file older than the files extracted after it and
	// calls the release "-dirty".
	for _, f := range []string{"version", "main.zi", "tzdata.zi"} {
		if err := os.Remove(filepath.Join(src, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	cmd := exec.Command("make", "-C", src, "-s", "VERSION="+r, "tzdata.zi")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("make tzdata.zi of tzdata%s: %v\n%s", r, err, out)
	}
	return os.ReadFile(filepath.Join(src, "tzdata.zi"))
}

// compile compiles text by zic into work/out-<r>-<zic's release>[-packrat].
func compile(work, r, zic string, packrat bool, text []byte) (string, error) {
	dst := filepath.Join(work, "out-"+r+"-"+filepath.Base(filepath.Dir(zic)))
	if packrat {
		dst += "-packrat"
	}
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	tmp := dst + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return "", err
	}
	in := tmp + ".zi"
	if err := os.WriteFile(in, text, 0o644); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(in) }()
	cmd := exec.Command(zic, "-d", tmp, in)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("zic of tzdata%s: %v\n%s", r, err, out)
	}
	return dst, os.Rename(tmp, dst)
}

// filesOf is the zone files under dir, by name, compiled from src: each of
// src's names (Zone and Link lines), and nothing else but zic's temporary
// files. An error naming each file of no name of src, and each name of no
// file, or of a zone.tab or zone1970.tab name (tabs) that is none of src's.
func filesOf(dir string, src *models.ZoneSource, tabs []string) (map[string][]byte, error) {
	files := map[string][]byte{}
	var stray []string
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if zicTemp.MatchString(filepath.Base(name)) {
			return nil
		}
		if !src.Holds(name) {
			stray = append(stray, name)
			return nil
		}
		b, err := os.ReadFile(p)
		files[name] = b
		return err
	})
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, n := range src.Names() {
		if _, ok := files[n]; !ok {
			missing = append(missing, n)
		}
	}
	for _, n := range tabs {
		if !src.Holds(n) {
			missing = append(missing, n+" (zone.tab)")
		}
	}
	sort.Strings(stray)
	if len(stray) > 0 || len(missing) > 0 {
		return nil, fmt.Errorf("%s: files of no zone or link name of the release: %q; names of no file: %q", dir, stray, missing)
	}
	return files, nil
}

// tabNames is the names zone.tab and zone1970.tab of release r list.
func tabNames(work, r string) ([]string, error) {
	var names []string
	for _, f := range []string{"zone.tab", "zone1970.tab"} {
		b, err := os.ReadFile(filepath.Join(work, "src-"+r, f))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) >= 3 && !strings.HasPrefix(f[0], "#") {
				names = append(names, f[2])
			}
		}
	}
	return names, nil
}

// table is what the generator writes: each name's digests in every build
// (all), and in the newest release's main data compiled by its own zic
// (newest).
type table struct {
	first, last string
	all         map[string]map[uint64]bool
	newest      map[string]uint64
}

// build compiles every build of every release and reads its zone files.
func build(work string, releases []string) (*table, error) {
	t := &table{first: releases[0], last: releases[len(releases)-1], all: map[string]map[uint64]bool{}, newest: map[string]uint64{}}
	lastZic, err := buildZic(work, t.last)
	if err != nil {
		return nil, err
	}
	for _, r := range releases {
		ownZic, err := buildZic(work, r)
		if err != nil {
			return nil, err
		}
		tabs, err := tabNames(work, r)
		if err != nil {
			return nil, err
		}
		for _, packrat := range []bool{false, true} {
			text, ok, err := input(work, r, packrat)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			src, err := models.ParseZoneSource(text)
			if err != nil {
				return nil, fmt.Errorf("tzdata%s (packrat %v): %w", r, packrat, err)
			}
			zics := []string{ownZic}
			if lastZic != ownZic {
				zics = append(zics, lastZic)
			}
			for _, zic := range zics {
				dir, err := compile(work, r, zic, packrat, text)
				if err != nil {
					return nil, err
				}
				files, err := filesOf(dir, src, tabs)
				if err != nil {
					return nil, err
				}
				var unread []string
				for name, b := range files {
					loc, err := time.LoadLocationFromTZData(name, b)
					if err != nil {
						return nil, fmt.Errorf("%s/%s: %w", dir, name, err)
					}
					d := models.ZoneRulesDigest(loc, digestTo)
					if t.all[name] == nil {
						t.all[name] = map[uint64]bool{}
					}
					t.all[name][d] = true
					if r == t.last && !packrat && zic == lastZic {
						t.newest[name] = d
					}
					if zic == lastZic && !src.Gives(name, d) {
						unread = append(unread, name)
					}
				}
				sort.Strings(unread)
				for _, name := range unread {
					if !src.SummerAllYear(name) {
						return nil, fmt.Errorf("tzdata%s (packrat %v), compiled by zic %s: %s reads offsets models.ZoneSource does not read it with from the release's source", r, packrat, t.last, name)
					}
					log.Printf("tzdata%s (packrat %v): %s keeps summer time all year, whose footer Go reads by the UTC year: models.ZoneSource reads it as its fat file", r, packrat, name)
				}
			}
		}
	}
	return t, nil
}

// render is the table as Go source, which depends on nothing but t.
func (t *table) render() ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by tools/zonereleases; DO NOT EDIT.\n\npackage models\n\n")
	fmt.Fprintf(&b, "// ZoneReleasesFirst and ZoneReleasesLast are the oldest and the newest\n// release of IANA's zone data zoneReleases reads.\nconst (\n\tZoneReleasesFirst = %q\n\tZoneReleasesLast  = %q\n)\n\n", t.first, t.last)
	fmt.Fprintf(&b, "// zoneReleases is each zone and link name's digests (zoneReleasesOf): its\n// zone's in ZoneReleasesLast's main data (0 for none), then those of every\n// build of every release, sorted.\nvar zoneReleases = map[string][]uint64{\n")
	names := make([]string, 0, len(t.all))
	for n := range t.all {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ds := make([]uint64, 0, len(t.all[n]))
		for d := range t.all[n] {
			ds = append(ds, d)
		}
		slices.Sort(ds)
		s := []string{fmt.Sprintf("%#016x", t.newest[n])}
		for _, d := range ds {
			s = append(s, fmt.Sprintf("%#016x", d))
		}
		fmt.Fprintf(&b, "\t%q: {%s},\n", n, strings.Join(s, ", "))
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
