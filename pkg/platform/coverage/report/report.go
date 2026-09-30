// Package report reads the coverage report a test runner already writes, so
// `keploy mock replay` can say how much of the code the suite exercised while
// every dependency was served from the recording.
//
// It instruments nothing. The rest of pkg/platform/coverage injects an agent
// into the APPLICATION that `keploy test` drives; the mock flow wraps the
// user's own test runner, and every mainstream runner can already write a
// report (go test -coverprofile, jest/c8/nyc lcov, pytest-cov xml/lcov,
// JaCoCo). Reading that report is both less invasive and more honest: the
// number is the runner's own, the one the developer already recognises.
//
// Four formats cover those runners: the Go cover profile, LCOV, Cobertura XML
// and JaCoCo XML. The format is sniffed from the content, never trusted from
// the file name.
package report

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Summary is one coverage report reduced to the one number it supports.
type Summary struct {
	// Covered and Total are in Unit. Percent is derived from them, never
	// stored separately, so the three cannot disagree.
	Covered int64 `json:"covered" yaml:"covered"`
	Total   int64 `json:"total" yaml:"total"`
	// Unit is what was counted: "statements" for a Go profile, "lines" for
	// every other format.
	Unit string `json:"unit" yaml:"unit"`
	// Format is the report format the file was parsed as.
	Format string `json:"format" yaml:"format"`
	// Source is the report file, as the caller named it.
	Source string `json:"source" yaml:"source"`
}

// Percent is Covered/Total as a percentage; 0 for an empty report.
func (s Summary) Percent() float64 {
	if s.Total <= 0 {
		return 0
	}
	return float64(s.Covered) * 100 / float64(s.Total)
}

// Floor1 renders a percentage floored to one decimal. Floored, not rounded:
// 79.96% must not print as 80.0% next to a --min-coverage 80 that failed it.
func Floor1(p float64) string {
	return strconv.FormatFloat(float64(int64(p*10))/10, 'f', 1, 64)
}

// ErrUnknownFormat is returned for a file that is none of the four formats.
var ErrUnknownFormat = errors.New("not a recognised coverage report (Go cover profile, LCOV, Cobertura XML or JaCoCo XML)")

// maxReportBytes bounds what ParseFile will read. A JaCoCo report for a very
// large codebase is tens of megabytes. A var so a test can lower it.
var maxReportBytes int64 = 256 << 20

// ErrTooLarge is returned for a report past maxReportBytes. It is refused, not
// cut short: a truncated LCOV or cover profile parses cleanly into a partial
// count, and a partial count reported as the whole is a wrong number.
var ErrTooLarge = errors.New("the coverage report is too large to read")

// ParseFile reads and parses one report.
func ParseFile(path string) (Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return Summary{}, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > maxReportBytes {
		return Summary{}, fmt.Errorf("%s: %w (%d bytes)", path, ErrTooLarge, info.Size())
	}
	lr := &io.LimitedReader{R: f, N: maxReportBytes + 1}
	s, err := Parse(lr)
	if err == nil && lr.N <= 0 {
		// The file grew past the limit while it was being read.
		err = ErrTooLarge
	}
	if err != nil {
		return Summary{}, fmt.Errorf("%s: %w", path, err)
	}
	s.Source = path
	return s, nil
}

// Parse sniffs the format and parses the report.
func Parse(r io.Reader) (Summary, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(4096)
	trimmed := bytes.TrimLeft(head, " \t\r\n\xef\xbb\xbf")
	switch {
	case bytes.HasPrefix(trimmed, []byte("mode:")):
		return parseGoProfile(br)
	case bytes.HasPrefix(trimmed, []byte("<")):
		return parseXML(br)
	case lcovHead(trimmed):
		return parseLCOV(br)
	}
	return Summary{}, ErrUnknownFormat
}

func lcovHead(b []byte) bool {
	for _, p := range [][]byte{[]byte("TN:"), []byte("SF:")} {
		if bytes.HasPrefix(b, p) {
			return true
		}
	}
	return false
}

// parseGoProfile reads `go test -coverprofile` output:
//
//	mode: set
//	example.com/pkg/file.go:10.2,12.3 2 1
//
// A block can appear more than once (-coverpkg across several test binaries
// writes each package's blocks once per binary), so blocks are merged by
// position, as `go tool cover` does: covered if any occurrence ran.
func parseGoProfile(r io.Reader) (Summary, error) {
	type block struct {
		stmts   int64
		covered bool
	}
	blocks := map[string]*block{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if strings.HasPrefix(line, "mode:") {
				continue
			}
		}
		if strings.HasPrefix(line, "mode:") {
			// Concatenated profiles repeat the header.
			continue
		}
		// file:start,end numStmts count -- split from the right, since a path
		// may contain spaces.
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			return Summary{}, fmt.Errorf("malformed cover profile line %q", line)
		}
		count, err := strconv.ParseInt(line[sp+1:], 10, 64)
		if err != nil {
			return Summary{}, fmt.Errorf("malformed cover profile line %q", line)
		}
		rest := line[:sp]
		sp = strings.LastIndexByte(rest, ' ')
		if sp < 0 {
			return Summary{}, fmt.Errorf("malformed cover profile line %q", line)
		}
		stmts, err := strconv.ParseInt(rest[sp+1:], 10, 64)
		if err != nil {
			return Summary{}, fmt.Errorf("malformed cover profile line %q", line)
		}
		key := rest[:sp]
		b := blocks[key]
		if b == nil {
			b = &block{stmts: stmts}
			blocks[key] = b
		}
		if count > 0 {
			b.covered = true
		}
	}
	if err := sc.Err(); err != nil {
		return Summary{}, err
	}
	s := Summary{Unit: "statements", Format: "go"}
	for _, b := range blocks {
		s.Total += b.stmts
		if b.covered {
			s.Covered += b.stmts
		}
	}
	return s, nil
}

// parseLCOV reads an LCOV tracefile. Line hits (DA) are merged per file and
// line, so a tracefile that lists the same source twice -- the normal output of
// concatenating per-worker files -- counts each line once. A record with no DA
// lines falls back to its LF/LH summary.
func parseLCOV(r io.Reader) (Summary, error) {
	type rec struct {
		lines  map[int64]bool
		lf, lh int64
	}
	files := map[string]*rec{}
	var cur *rec
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "SF:"):
			name := line[3:]
			cur = files[name]
			if cur == nil {
				cur = &rec{lines: map[int64]bool{}}
				files[name] = cur
			}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "DA:"):
			parts := strings.Split(line[3:], ",")
			if len(parts) < 2 {
				continue
			}
			ln, err1 := strconv.ParseInt(parts[0], 10, 64)
			hits, err2 := strconv.ParseInt(parts[1], 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			cur.lines[ln] = cur.lines[ln] || hits > 0
		// A source listed twice with only summaries (per-worker files
		// concatenated) is one file: take the larger count rather than the
		// sum, which would count its lines twice.
		case strings.HasPrefix(line, "LF:"):
			n, _ := strconv.ParseInt(line[3:], 10, 64)
			cur.lf = max(cur.lf, n)
		case strings.HasPrefix(line, "LH:"):
			n, _ := strconv.ParseInt(line[3:], 10, 64)
			cur.lh = max(cur.lh, n)
		case line == "end_of_record":
			cur = nil
		}
	}
	if err := sc.Err(); err != nil {
		return Summary{}, err
	}
	if len(files) == 0 {
		return Summary{}, ErrUnknownFormat
	}
	s := Summary{Unit: "lines", Format: "lcov"}
	for _, f := range files {
		if len(f.lines) == 0 {
			s.Total += f.lf
			s.Covered += f.lh
			continue
		}
		for _, hit := range f.lines {
			s.Total++
			if hit {
				s.Covered++
			}
		}
	}
	return s, nil
}

// parseXML handles Cobertura (<coverage lines-valid lines-covered>) and JaCoCo
// (<report> with a report-level <counter type="LINE">). Only the root and its
// direct children are inspected; a JaCoCo report is otherwise walked without
// being held in memory.
func parseXML(r io.Reader) (Summary, error) {
	dec := xml.NewDecoder(r)
	// JaCoCo declares an external DTD. encoding/xml never resolves one, so
	// the declaration is read as a directive and ignored -- nothing is fetched.
	depth := 0
	root := ""
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Summary{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				root = t.Name.Local
				if root == "coverage" {
					return cobertura(t)
				}
				if root != "report" {
					return Summary{}, ErrUnknownFormat
				}
				continue
			}
			if depth == 2 && root == "report" && t.Name.Local == "counter" && attr(t, "type") == "LINE" {
				missed, err1 := strconv.ParseInt(attr(t, "missed"), 10, 64)
				covered, err2 := strconv.ParseInt(attr(t, "covered"), 10, 64)
				if err1 != nil || err2 != nil {
					return Summary{}, errors.New("JaCoCo report has an unreadable LINE counter")
				}
				return Summary{Covered: covered, Total: covered + missed, Unit: "lines", Format: "jacoco"}, nil
			}
			if depth >= 2 {
				// Skip the subtree: only the report-level counters matter.
				if err := dec.Skip(); err != nil {
					return Summary{}, err
				}
				depth--
			}
		case xml.EndElement:
			depth--
		}
	}
	if root == "report" {
		return Summary{}, errors.New("JaCoCo report has no report-level LINE counter")
	}
	return Summary{}, ErrUnknownFormat
}

func cobertura(t xml.StartElement) (Summary, error) {
	valid, err1 := strconv.ParseInt(attr(t, "lines-valid"), 10, 64)
	covered, err2 := strconv.ParseInt(attr(t, "lines-covered"), 10, 64)
	if err1 != nil || err2 != nil {
		// line-rate alone gives a percentage with no denominator. Refuse it:
		// every number this package reports is "N of M", never a bare ratio.
		return Summary{}, errors.New("Cobertura report has no lines-valid/lines-covered totals")
	}
	return Summary{Covered: covered, Total: valid, Unit: "lines", Format: "cobertura"}, nil
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// candidates are the paths the mainstream runners write by default, relative
// to the directory the test command runs in.
var candidates = []string{
	"coverage.out",
	"cover.out",
	"coverage.txt",
	"coverage/lcov.info",
	"lcov.info",
	"coverage.lcov",
	"coverage.xml",
	"coverage/cobertura-coverage.xml",
	"target/site/jacoco/jacoco.xml",
	"build/reports/jacoco/test/jacocoTestReport.xml",
}

var (
	// An unquoted path stops at a quote: in `sh -c 'go test -coverprofile=c.out'`
	// the closing quote belongs to the shell, not to the file name.
	goCoverprofile = regexp.MustCompile(`-coverprofile(?:=|\s+)("[^"]+"|'[^']+'|[^\s'"]+)`)
	pyCovReport    = regexp.MustCompile(`--cov-report(?:=|\s+)(xml|lcov)(?::("[^"]+"|'[^']+'|[^\s'"]+))?`)
)

// FromCommand returns the report paths a test command names explicitly: go
// test's -coverprofile, and pytest-cov's --cov-report xml/lcov (with its
// default file when no path follows the colon).
func FromCommand(cmd string) []string {
	var out []string
	for _, m := range goCoverprofile.FindAllStringSubmatch(cmd, -1) {
		out = append(out, unquote(m[1]))
	}
	for _, m := range pyCovReport.FindAllStringSubmatch(cmd, -1) {
		switch {
		case m[2] != "":
			out = append(out, unquote(m[2]))
		case m[1] == "xml":
			out = append(out, "coverage.xml")
		case m[1] == "lcov":
			out = append(out, "coverage.lcov")
		}
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// stamp is what a report file looked like before the run.
type stamp struct {
	mod  time.Time
	size int64
}

// Before is the state of every report path a run could write, taken before
// the run starts. Find compares against it.
type Before struct {
	stamps map[string]stamp
}

// paths lists every report location a run could write, in priority order:
// the explicit path, then the ones the command names, then the defaults.
func paths(dir, explicit, cmd string) []string {
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	var out []string
	if explicit != "" {
		return []string{abs(explicit)}
	}
	for _, p := range FromCommand(cmd) {
		out = append(out, abs(p))
	}
	for _, p := range candidates {
		out = append(out, abs(p))
	}
	return out
}

func stat(p string) (stamp, bool) {
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return stamp{}, false
	}
	return stamp{mod: info.ModTime(), size: info.Size()}, true
}

// Snapshot records the report files that exist before the run.
//
// Freshness is decided by comparison, not by a clock. A timestamp window --
// "modified since the run started, give or take filesystem granularity" --
// credited a run with the report the PREVIOUS run wrote a second earlier: a
// replay with no coverage flag at all quoted its predecessor's number, and a
// --min-coverage gate that should have failed passed. A report counts only if
// this run created it or changed it.
func Snapshot(dir, explicit, cmd string) Before {
	b := Before{stamps: map[string]stamp{}}
	for _, p := range paths(dir, explicit, cmd) {
		if st, ok := stat(p); ok {
			b.stamps[p] = st
		}
	}
	return b
}

// written reports whether the run created or rewrote p.
func (b Before) written(p string) (time.Time, bool) {
	now, ok := stat(p)
	if !ok {
		return time.Time{}, false
	}
	was, existed := b.stamps[p]
	return now.mod, !existed || !now.mod.Equal(was.mod) || now.size != was.size
}

// Find locates the report THIS run wrote: the explicit path when one is
// configured, else the paths the command names, else the runners' default
// locations. A file the run did not create or change describes a different
// run, and quoting it would attach someone else's number to this replay.
//
// It returns the chosen path, or "" with a reason when there is none.
func Find(dir, explicit, cmd string, before Before) (string, string) {
	all := paths(dir, explicit, cmd)
	if explicit != "" {
		if _, ok := before.written(all[0]); ok {
			return all[0], ""
		}
		if _, ok := stat(all[0]); !ok {
			return "", fmt.Sprintf("the configured coverage report %s was not written by this run", explicit)
		}
		return "", fmt.Sprintf("the configured coverage report %s was not updated by this run", explicit)
	}
	named := len(FromCommand(cmd))
	for _, p := range all[:named] {
		if _, ok := before.written(p); ok {
			return p, ""
		}
	}
	best, bestAt := "", time.Time{}
	for _, p := range all[named:] {
		if at, ok := before.written(p); ok && (best == "" || at.After(bestAt)) {
			best, bestAt = p, at
		}
	}
	if best != "" {
		return best, ""
	}
	return "", "the test command wrote no coverage report"
}
