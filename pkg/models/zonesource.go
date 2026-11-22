package models

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ZoneSource is a release of IANA's zone data (tzdata) as its source text
// gives it: the Rule, Zone and Link lines zic compiles into the release's
// zone files. Every release ships them in one file, tzdata.zi, which
// distributions install beside the compiled files (Debian, Ubuntu, Fedora,
// RHEL: /usr/share/zoneinfo/tzdata.zi, built from the same data as those
// files, backzone's zones and the rearguard form included where the
// distribution builds them). ZoneSource reads each zone's offsets from UTC
// as zic compiles them (Digest), so a zone file can be checked against
// the release it came with, whichever release that is.
type ZoneSource struct {
	version string
	rules   map[string][]srcRule
	zones   map[string][]srcLine
	links   map[string]string
	// Each rule's, zone's and link's own lines, for Extract.
	rtext, ztext map[string][]string
	ltext        map[string]string

	// byZone is each zone's digests (Digests), made at their first read.
	mu     sync.Mutex
	byZone map[string][]uint64
}

// srcRule is one Rule line.
type srcRule struct {
	from, to      int64 // years; to is srcMaxYear for "max"
	hiWasNum      bool
	month         time.Month
	dayCode       int // dayOfMonth, dayAtLeast or dayAtMost
	day           int
	wday          time.Weekday
	tod           int64 // seconds into the day, local per todStd and todUT
	todStd, todUT bool
	save          int64
	isdst         bool
	letters       string
}

// srcLine is one line of a Zone, the first or a continuation.
type srcLine struct {
	stdoff    int64
	rule      string // the Rule name, or the line's own SAVE where no Rule has it
	save      int64  // the line's own SAVE (rule names no Rule)
	isdst     bool
	format    string
	spec      byte // '%s' or '%z' in format: 's' or 'z', else 0
	hasUntil  bool
	untilYear int64
	until     int64 // the UNTIL instant as local seconds (rpytime)
	untilStd  bool
	untilUT   bool
	rules     []srcRule
	ruled     bool
}

const (
	dayOfMonth = iota
	dayAtLeast
	dayAtMost
)

// srcMaxYear stands for a Rule's "max" TO year.
const srcMaxYear = int64(1) << 40

// srcLastYear is the last year whose changes ZoneSource reads: the year a
// digest's span (zoneDigestTo) ends in.
const srcLastYear = 2038

var (
	srcLineCodes = []string{"Rule", "Zone", "Link"}
	srcLeapCodes = []string{"Leap", "Expires"}
	srcMonths    = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	srcWdays     = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	srcLasts     = []string{"last-Sunday", "last-Monday", "last-Tuesday", "last-Wednesday", "last-Thursday", "last-Friday", "last-Saturday"}
)

// srcWord is the index in words of word, as zic's byword reads it: an
// exact match ignoring ASCII case, else the one word word is a prefix of;
// -1 for none or several.
func srcWord(word string, words []string) int {
	for i, w := range words {
		if strings.EqualFold(word, w) {
			return i
		}
	}
	found := -1
	for i, w := range words {
		if len(word) > 0 && len(word) <= len(w) && strings.EqualFold(word, w[:len(word)]) {
			if found >= 0 {
				return -1
			}
			found = i
		}
	}
	return found
}

// srcFields splits a source line into its fields as zic's getfields does:
// separated by white space, up to a '#', a quoted part kept whole, and a
// field of "-" alone read as empty.
func srcFields(line string) ([]string, error) {
	var out []string
	i := 0
	for {
		for i < len(line) && isSrcSpace(line[i]) {
			i++
		}
		if i >= len(line) || line[i] == '#' {
			return out, nil
		}
		var f []byte
		for i < len(line) && line[i] != '#' && !isSrcSpace(line[i]) {
			if line[i] != '"' {
				f = append(f, line[i])
				i++
				continue
			}
			i++
			j := strings.IndexByte(line[i:], '"')
			if j < 0 {
				return nil, errors.New("odd number of quotation marks")
			}
			f = append(f, line[i:i+j]...)
			i += j + 1
		}
		s := string(f)
		if s == "-" {
			s = ""
		}
		out = append(out, s)
	}
}

func isSrcSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\f' || c == '\n' || c == '\r' || c == '\v'
}

// srcHMS reads "[-]h[:mm[:ss[.frac]]]" as seconds, as zic's gethms does
// (fractions rounded to the even second); "" is zero.
func srcHMS(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	sign := int64(1)
	t := s
	if t[0] == '-' {
		sign, t = -1, t[1:]
	}
	parts := strings.SplitN(t, ":", 3)
	num := func(p string) (int64, bool) {
		if p == "" {
			return 0, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		n, err := strconv.ParseInt(p, 10, 64)
		return n, err == nil
	}
	hh, ok := num(parts[0])
	if !ok {
		return 0, fmt.Errorf("invalid time %q", s)
	}
	var mm, ss int64
	frac := ""
	if len(parts) > 1 {
		if mm, ok = num(parts[1]); !ok || mm >= 60 {
			return 0, fmt.Errorf("invalid time %q", s)
		}
	}
	if len(parts) > 2 {
		sp := parts[2]
		if d := strings.IndexByte(sp, '.'); d >= 0 {
			sp, frac = sp[:d], sp[d+1:]
			if _, ok := num(frac); !ok {
				return 0, fmt.Errorf("invalid time %q", s)
			}
		}
		if ss, ok = num(sp); !ok || ss > 60 {
			return 0, fmt.Errorf("invalid time %q", s)
		}
	}
	if frac != "" {
		tenths := int64(frac[0] - '0')
		rest := strings.TrimLeft(frac[1:], "0")
		even := int64(0)
		if ss%2 == 0 && rest == "" {
			even = 1
		}
		if tenths >= 5+even {
			ss++
		}
	}
	return sign * (hh*3600 + mm*60 + ss), nil
}

// srcSave reads a SAVE field: its time, and whether it is summer time
// (a "d" or "s" suffix says so, else a SAVE other than zero is).
func srcSave(s string) (int64, bool, error) {
	dst := -1
	if s != "" {
		switch s[len(s)-1] {
		case 'd':
			dst, s = 1, s[:len(s)-1]
		case 's':
			dst, s = 0, s[:len(s)-1]
		}
	}
	save, err := srcHMS(s)
	if err != nil {
		return 0, false, err
	}
	if dst < 0 {
		return save, save != 0, nil
	}
	return save, dst == 1, nil
}

// srcRuleOf reads a rule's years, month, day and time of day, as zic's
// rulesub does (an UNTIL is one too, of the year "only").
func srcRuleOf(r *srcRule, lo, hi, month, day, tod string) error {
	m := srcWord(month, srcMonths)
	if m < 0 {
		return fmt.Errorf("invalid month name %q", month)
	}
	r.month = time.Month(m + 1)
	if tod != "" {
		switch tod[len(tod)-1] | 0x20 {
		case 's':
			r.todStd, tod = true, tod[:len(tod)-1]
		case 'w':
			tod = tod[:len(tod)-1]
		case 'g', 'u', 'z':
			r.todStd, r.todUT, tod = true, true, tod[:len(tod)-1]
		}
	}
	var err error
	if r.tod, err = srcHMS(tod); err != nil {
		return err
	}
	if srcWord(lo, []string{"minimum"}) == 0 {
		r.from = 1900
	} else if r.from, err = strconv.ParseInt(lo, 10, 64); err != nil {
		return fmt.Errorf("invalid starting year %q", lo)
	}
	switch srcWord(hi, []string{"maximum", "only"}) {
	case 0:
		r.to = srcMaxYear
	case 1:
		r.to = r.from
	default:
		if r.to, err = strconv.ParseInt(hi, 10, 64); err != nil {
			return fmt.Errorf("invalid ending year %q", hi)
		}
		r.hiWasNum = true
	}
	if r.from > r.to {
		return fmt.Errorf("starting year %d after ending year %d", r.from, r.to)
	}
	maxDay := [12]int{31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[r.month-1]
	d := day
	if len(d) > 4 && strings.EqualFold(d[:4], "last") {
		if d[4] == '-' {
			if w := srcWord(d, srcLasts); w >= 0 {
				r.dayCode, r.wday, r.day = dayAtMost, time.Weekday(w), maxDay
				return nil
			}
		} else if w := srcWord(d[4:], srcWdays); w >= 0 {
			r.dayCode, r.wday, r.day = dayAtMost, time.Weekday(w), maxDay
			return nil
		}
		return fmt.Errorf("invalid day %q", day)
	}
	num := d
	if i := strings.IndexAny(d, "<>"); i >= 0 {
		r.dayCode = dayAtLeast
		if d[i] == '<' {
			r.dayCode = dayAtMost
		}
		if i+1 >= len(d) || d[i+1] != '=' {
			return fmt.Errorf("invalid day %q", day)
		}
		w := srcWord(d[:i], srcWdays)
		if w < 0 {
			return fmt.Errorf("invalid weekday in %q", day)
		}
		r.wday, num = time.Weekday(w), d[i+2:]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 || n > maxDay {
		return fmt.Errorf("invalid day %q", day)
	}
	r.day = n
	return nil
}

// srcDay is the day number (days since 1970-01-01) of r's day in year y,
// as zic's rpytime finds it.
func srcDay(r *srcRule, y int64) int64 {
	d := r.day
	if r.month == time.February && d == 29 && !isLeapYear(y) {
		// lastSun in February: "Sun<=29" of a year of 28 days.
		d = 28
	}
	day := time.Date(int(y), r.month, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
	if r.dayCode == dayOfMonth {
		return day
	}
	wd := time.Weekday(((day % 7) + 7 + 4) % 7) // 1970-01-01 was a Thursday
	for wd != r.wday {
		if r.dayCode == dayAtLeast {
			day++
			wd = (wd + 1) % 7
		} else {
			day--
			wd = (wd + 6) % 7
		}
	}
	return day
}

func isLeapYear(y int64) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

// rpytime is the instant of r in year y in local seconds: its day's start
// plus its time of day, read by its suffix (todStd, todUT).
func (r *srcRule) rpytime(y int64) int64 { return srcDay(r, y)*86400 + r.tod }

// ParseZoneSource reads the Rule, Zone and Link lines of IANA's zone
// source text: a release's tzdata.zi, or the files zic compiles it from
// (africa, europe, backward, backzone, ...) put one after another. Leap
// and Expires lines (the leapseconds file's) are skipped. An error where a
// line is no such line, or a name is a Zone and a Link both, or twice.
func ParseZoneSource(text []byte) (*ZoneSource, error) {
	s := &ZoneSource{rules: map[string][]srcRule{}, zones: map[string][]srcLine{}, links: map[string]string{},
		rtext: map[string][]string{}, ztext: map[string][]string{}, ltext: map[string]string{}}
	var cont string // the Zone a continuation line belongs to
	for n, raw := range strings.Split(string(text), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if s.version == "" && strings.HasPrefix(line, "# version ") {
			s.version = strings.TrimSpace(strings.TrimPrefix(line, "# version "))
		}
		f, err := srcFields(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		if len(f) == 0 {
			continue
		}
		if cont != "" {
			if len(f) < 3 || len(f) > 7 {
				return nil, fmt.Errorf("line %d: a Zone continuation line of %d fields", n+1, len(f))
			}
			zl, err := srcZoneLine(f)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			if prev := s.zones[cont]; prev[len(prev)-1].until >= zl.until && zl.hasUntil {
				return nil, fmt.Errorf("line %d: a Zone continuation line ending no later than the line before", n+1)
			}
			s.zones[cont] = append(s.zones[cont], zl)
			s.ztext[cont] = append(s.ztext[cont], line)
			if !zl.hasUntil {
				cont = ""
			}
			continue
		}
		switch srcWord(f[0], srcLineCodes) {
		case 0:
			if len(f) != 10 {
				return nil, fmt.Errorf("line %d: a Rule line of %d fields", n+1, len(f))
			}
			if f[1] == "" || strings.ContainsAny(f[1][:1], " +-0123456789") {
				return nil, fmt.Errorf("line %d: invalid rule name %q", n+1, f[1])
			}
			if f[4] != "" {
				return nil, fmt.Errorf("line %d: rule year type %q", n+1, f[4])
			}
			var r srcRule
			if r.save, r.isdst, err = srcSave(f[8]); err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			if err := srcRuleOf(&r, f[2], f[3], f[5], f[6], f[7]); err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			r.letters = f[9]
			s.rules[f[1]] = append(s.rules[f[1]], r)
			s.rtext[f[1]] = append(s.rtext[f[1]], line)
		case 1:
			if len(f) < 5 || len(f) > 9 {
				return nil, fmt.Errorf("line %d: a Zone line of %d fields", n+1, len(f))
			}
			name := f[1]
			if _, ok := s.zones[name]; ok {
				return nil, fmt.Errorf("line %d: zone %s defined twice", n+1, name)
			}
			zl, err := srcZoneLine(f[2:])
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			s.zones[name] = []srcLine{zl}
			s.ztext[name] = []string{line}
			if zl.hasUntil {
				cont = name
			}
		case 2:
			if len(f) != 3 || f[1] == "" {
				return nil, fmt.Errorf("line %d: a Link line of %d fields", n+1, len(f))
			}
			// As zic makes them: a later Link of a name replaces an
			// earlier one, and a Link replaces a Zone of its name
			// (zic writes the links after the zones).
			s.links[f[2]] = f[1]
			s.ltext[f[2]] = line
		default:
			if srcWord(f[0], srcLeapCodes) < 0 {
				return nil, fmt.Errorf("line %d: a line of unknown type %q", n+1, f[0])
			}
			// The leapseconds file's lines: no zone's.
		}
	}
	if cont != "" {
		return nil, fmt.Errorf("zone %s: its continuation line is missing", cont)
	}
	for name, lines := range s.zones {
		for i := range lines {
			zl := &lines[i]
			if rs, ok := s.rules[zl.rule]; ok && zl.rule != "" {
				zl.rules, zl.ruled = rs, true
				continue
			}
			var err error
			if zl.save, zl.isdst, err = srcSave(zl.rule); err != nil {
				return nil, fmt.Errorf("zone %s: rules %q: no Rule lines, and %w", name, zl.rule, err)
			}
			if zl.spec == 's' {
				return nil, fmt.Errorf("zone %s: %%s in a zone line of no rules", name)
			}
		}
	}
	for name := range s.links {
		if _, ok := s.resolve(name); !ok {
			return nil, fmt.Errorf("link %s: its target %s is no zone", name, s.links[name])
		}
	}
	return s, nil
}

// srcZoneLine reads a Zone line's fields after its name (STDOFF RULES
// FORMAT [UNTIL...]), or a continuation line's.
func srcZoneLine(f []string) (srcLine, error) {
	var zl srcLine
	var err error
	if zl.stdoff, err = srcHMS(f[0]); err != nil {
		return zl, err
	}
	zl.rule, zl.format = f[1], f[2]
	if i := strings.IndexByte(zl.format, '%'); i >= 0 {
		if i+1 >= len(zl.format) || zl.format[i+1] != 's' && zl.format[i+1] != 'z' || strings.Contains(zl.format[i+1:], "%") || strings.Contains(zl.format, "/") {
			return zl, fmt.Errorf("invalid abbreviation format %q", zl.format)
		}
		zl.spec = zl.format[i+1]
	}
	if len(f) > 3 {
		zl.hasUntil = true
		get := func(i int, dflt string) string {
			if len(f) > i {
				return f[i]
			}
			return dflt
		}
		var u srcRule
		if err := srcRuleOf(&u, f[3], "only", get(4, "Jan"), get(5, "1"), get(6, "0")); err != nil {
			return zl, err
		}
		zl.untilYear, zl.until, zl.untilStd, zl.untilUT = u.from, u.rpytime(u.from), u.todStd, u.todUT
	}
	return zl, nil
}

// resolve is the Zone name names, through its links.
func (s *ZoneSource) resolve(name string) (string, bool) {
	for i := 0; i < 64; i++ {
		t, ok := s.links[name]
		if !ok {
			_, ok := s.zones[name]
			return name, ok
		}
		name = t
	}
	return "", false
}

// Version is the release the source names in its "# version" line
// (tzdata.zi's first), "" where it names none.
func (s *ZoneSource) Version() string { return s.version }

// Names is every zone and link name of the source, sorted.
func (s *ZoneSource) Names() []string {
	names := make([]string, 0, len(s.zones)+len(s.links))
	for n := range s.zones {
		if _, ok := s.links[n]; !ok {
			names = append(names, n)
		}
	}
	for n := range s.links {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Holds reports whether the source names name, a zone or a link.
func (s *ZoneSource) Holds(name string) bool {
	_, ok := s.resolve(name)
	return ok
}

// srcType is a local time type of a compiled zone, as zic's addtype keys
// one in a file of no fat data: its offset, summer time, abbreviation.
type srcType struct {
	off   int64
	isdst bool
	abbr  string
}

type srcTrans struct {
	at  int64
	typ int
}

// abbrOffset is zic's abbroffset: %z's text of off.
func abbrOffset(off int64) string {
	sign := byte('+')
	if off < 0 {
		off, sign = -off, '-'
	}
	s, m, h := off%60, off/60%60, off/3600
	b := []byte{sign, byte('0' + h/10), byte('0' + h%10)}
	if m|s != 0 {
		b = append(b, byte('0'+m/10), byte('0'+m%10))
		if s != 0 {
			b = append(b, byte('0'+s/10), byte('0'+s%10))
		}
	}
	return string(b)
}

// abbr is zic's doabbr of the line: its abbreviation for a time of save,
// letters the rule's (none: noLetters).
func (zl *srcLine) abbr(letters *string, isdst bool, save int64) string {
	if i := strings.IndexByte(zl.format, '/'); i >= 0 {
		if isdst {
			return zl.format[i+1:]
		}
		return zl.format[:i]
	}
	l := "%s"
	switch {
	case zl.spec == 'z':
		l = abbrOffset(zl.stdoff + save)
	case letters == nil:
	case letters == &noLetters:
		return ""
	default:
		l = *letters
	}
	if i := strings.IndexByte(zl.format, '%'); i >= 0 {
		return zl.format[:i] + l + zl.format[i+2:]
	}
	return zl.format
}

// noLetters is zic's disable_percent_s.
var noLetters = "\x00"

// srcZone is a zone as zic's outzone compiles it: its local time types,
// its changes of type (unsorted, as outzone adds them), the type of the
// times before the first, and what a slim file keeps of them (zic's
// default since 2020b): the changes up to tzStart, after which a reader
// takes the file's footer rule (the last line's rules of the "max" year,
// tz), never the type of the last change kept.
type srcZone struct {
	types     []srcType
	tts       []srcTrans
	def       int
	tzStart   int64
	dontMerge int // the index in tts of the change at tzStart zic keeps whatever its type, or -1
	tz        *srcFooter
}

// srcFooter is the rule a slim file's footer (its TZ string) gives the
// times after its last change: one summer and one standard rule, each
// running to the "max" year, applied every year, or one offset (nil
// rules).
type srcFooter struct {
	line     *srcLine
	dst, std *srcRule
}

// compileZone is lines as zic compiles them (srcZone).
func compileZone(lines []srcLine) srcZone {
	var z srcZone
	addType := func(t srcType) int {
		for i, u := range z.types {
			if u == t {
				return i
			}
		}
		z.types = append(z.types, t)
		return len(z.types) - 1
	}
	minYear, maxYear := int64(1970), int64(1970)
	upd := func(y int64) {
		if y < minYear {
			minYear = y
		}
		if y > maxYear {
			maxYear = y
		}
	}
	for i := range lines {
		if i < len(lines)-1 {
			upd(lines[i].untilYear)
		}
		for _, r := range lines[i].rules {
			upd(r.from)
			if r.hiWasNum {
				upd(r.to)
			}
		}
	}
	if maxYear < srcLastYear {
		maxYear = srcLastYear
	}
	defType := -1
	// nonTZLim is the last change no footer rule gives (outzone's
	// nonTZlimtime): one of a line that ends, or of a rule that does.
	nonTZLim, nonTZLimType, haveNonTZ := int64(0), -1, false
	noteNonTZ := func(at int64, typ int) {
		if !haveNonTZ || nonTZLim < at {
			nonTZLim, nonTZLimType, haveNonTZ = at, typ, true
		}
	}
	var startTime int64
	for i := range lines {
		zl := &lines[i]
		save := int64(0)
		useStart := i > 0
		useUntil := i < len(lines)-1
		stdoff := zl.stdoff
		startOff := stdoff
		startBuf, startSet := "", false
		if !zl.ruled {
			save = zl.save
			typ := addType(srcType{stdoff + save, zl.isdst, zl.abbr(nil, zl.isdst, save)})
			if useStart {
				z.tts = append(z.tts, srcTrans{startTime, typ})
				noteNonTZ(startTime, typ)
				useStart = false
			} else {
				defType = typ
			}
		} else {
			todo := make([]bool, len(zl.rules))
			temp := make([]int64, len(zl.rules))
			for year := minYear; year <= maxYear; year++ {
				if useUntil && year > zl.untilYear {
					break
				}
				for j := range zl.rules {
					r := &zl.rules[j]
					todo[j] = year >= r.from && year <= r.to
					if todo[j] {
						temp[j] = r.rpytime(year)
					}
				}
				for {
					var untilTime int64
					if useUntil {
						untilTime = zl.until
						if !zl.untilUT {
							untilTime -= stdoff
						}
						if !zl.untilStd {
							untilTime -= save
						}
					}
					k, ktime := -1, int64(0)
					for j := range zl.rules {
						if !todo[j] {
							continue
						}
						if jtime := zl.rules[j].utc(temp[j], stdoff, save); k < 0 || jtime < ktime {
							k, ktime = j, jtime
						}
					}
					if k < 0 {
						break
					}
					r := &zl.rules[k]
					todo[k] = false
					if useUntil && ktime >= untilTime {
						if !startSet && stdoff+r.save == startOff {
							startBuf, startSet = zl.abbr(&r.letters, r.isdst, r.save), true
						}
						break
					}
					save = r.save
					if useStart && ktime == startTime {
						useStart = false
					}
					if useStart {
						if ktime < startTime {
							startOff = stdoff + save
							startBuf, startSet = zl.abbr(&r.letters, r.isdst, r.save), true
							continue
						}
						if !startSet && startOff == stdoff+save {
							startBuf, startSet = zl.abbr(&r.letters, r.isdst, r.save), true
						}
					}
					typ := addType(srcType{stdoff + r.save, r.isdst, zl.abbr(&r.letters, r.isdst, r.save)})
					if defType < 0 && !r.isdst {
						defType = typ
					}
					z.tts = append(z.tts, srcTrans{ktime, typ})
					if useUntil || r.to != srcMaxYear {
						noteNonTZ(ktime, typ)
					}
				}
			}
		}
		if useStart {
			isdst := startOff != stdoff
			if !startSet {
				startBuf = zl.abbr(&noLetters, isdst, save)
			}
			typ := addType(srcType{startOff, isdst, startBuf})
			if defType < 0 && !isdst {
				defType = typ
			}
			z.tts = append(z.tts, srcTrans{startTime, typ})
		}
		if useUntil {
			startTime = zl.until
			if !zl.untilStd {
				startTime -= save
			}
			if !zl.untilUT {
				startTime -= stdoff
			}
		}
	}
	if defType < 0 {
		defType = 0
	}
	z.def = defType
	z.dontMerge = -1
	z.tz = footerOf(&lines[len(lines)-1])
	if z.tz == nil {
		// No footer zic can write: it lists every change (do_extend).
		z.tzStart = math.MaxInt64
		return z
	}
	z.tzStart = math.MaxInt64
	for _, t := range z.tts {
		if (!haveNonTZ || nonTZLim < t.at) && t.at < z.tzStart {
			z.tzStart = t.at
		}
	}
	if z.tzStart == math.MaxInt64 {
		z.tzStart = nonTZLim
	}
	for i, t := range z.tts {
		if t.at == z.tzStart && (nonTZLimType != t.typ || z.tz.dst != nil) {
			z.dontMerge = i
		}
	}
	return z
}

// utc is the instant of a rule's local time t (rpytime) in a line of
// standard offset stdoff whose time saved before it is save.
func (r *srcRule) utc(t, stdoff, save int64) int64 {
	off := stdoff
	if r.todUT {
		off = 0
	}
	if !r.todStd {
		off += save
	}
	return t - off
}

// footerOf is the footer zic writes of a zone whose last line is zl
// (stringzone): its one standard and one summer rule of the "max" year,
// or none where the line has no rules or none of that year (one offset);
// nil where zic can write no footer of its rules (do_extend).
func footerOf(zl *srcLine) *srcFooter {
	f := &srcFooter{line: zl}
	if !zl.ruled {
		return f
	}
	for i := range zl.rules {
		r := &zl.rules[i]
		if r.to != srcMaxYear {
			continue
		}
		last := &f.std
		if r.isdst {
			last = &f.dst
		}
		if *last != nil {
			return nil
		}
		*last = r
	}
	if (f.std == nil) != (f.dst == nil) {
		// One rule of the "max" year: zic's special cases (a permanent
		// summer time) read as the explicit changes do.
		return &srcFooter{line: zl}
	}
	return f
}

// srcOffsets is a reading of a zone from 1970 to 2038: its offset at
// 1970's first instant, then each change of offset.
type srcOffsets struct {
	first int64
	at    []int64
	off   []int64
}

func (o *srcOffsets) add(at, off int64) {
	if n := len(o.off); n > 0 && o.off[n-1] == off || n == 0 && o.first == off {
		return
	}
	o.at, o.off = append(o.at, at), append(o.off, off)
}

// keep is writezone's optimization of tts sorted: a change whose local
// time is no later than the one before it drops that one, the later
// change taking its place, and a change to the type in force is dropped
// (unless dontMerge, the index in tts of one zic keeps).
func keep(types []srcType, tts []srcTrans, dontMerge int) []srcTrans {
	idx := make([]int, len(tts))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return tts[idx[a]].at < tts[idx[b]].at })
	out := make([]srcTrans, 0, len(tts))
	for _, i := range idx {
		t := tts[i]
		if n := len(out); n > 0 {
			t2 := 0
			if n > 1 {
				t2 = out[n-2].typ
			}
			if t.at+types[out[n-1].typ].off <= out[n-1].at+types[t2].off {
				if t.typ == t2 {
					out = out[:n-1]
				} else {
					out[n-1].typ = t.typ
				}
				continue
			}
		}
		if n := len(out); n == 0 || i == dontMerge || types[out[n-1].typ] != types[t.typ] {
			out = append(out, t)
		}
	}
	return out
}

// readings are the offsets from 1970 to 2038 a reader (Go, the C library)
// reads of the zone from a fat file of it (every change listed) and from a
// slim one (changes up to tzStart, then the footer's rule): one reading
// where the two agree, as they do but where a line begins inside its
// rules' summer time with no rule of its own before it (Africa/Windhoek
// of tzdata 2014a to 2016j: a slim file reads summer time where the fat
// one reads standard).
func (z srcZone) readings() []srcOffsets {
	from, to := zoneDigestFrom.Unix(), zoneDigestTo.Unix()
	read := func(tts []srcTrans, footerFrom int64) srcOffsets {
		var o srcOffsets
		o.first = z.types[z.def].off
		i := 0
		for ; i < len(tts) && tts[i].at <= from && tts[i].at < footerFrom; i++ {
			o.first = z.types[tts[i].typ].off
		}
		for ; i < len(tts) && tts[i].at < to && tts[i].at < footerFrom; i++ {
			o.add(tts[i].at, z.types[tts[i].typ].off)
		}
		if footerFrom >= to {
			return o
		}
		// From the last change on, the footer's rule.
		cs := z.tz.changes(footerFrom, to)
		for j, c := range cs {
			if c.at >= to {
				break
			}
			if c.at <= from {
				o.first = c.off
				continue
			}
			if j == 0 || c.at <= footerFrom {
				// The footer's offset at the last change.
				o.add(max(footerFrom, from), c.off)
				continue
			}
			o.add(c.at, c.off)
		}
		return o
	}
	fat := read(keep(z.types, z.tts, -1), math.MaxInt64)
	if z.tz == nil || z.tz.dst == nil {
		return []srcOffsets{fat}
	}
	var kept []srcTrans
	dm := -1
	for i, t := range z.tts {
		if t.at <= z.tzStart {
			if i == z.dontMerge {
				dm = len(kept)
			}
			kept = append(kept, t)
		}
	}
	slimTTs := keep(z.types, kept, dm)
	last := int64(math.MaxInt64)
	if n := len(slimTTs); n > 0 {
		last = slimTTs[n-1].at
	}
	slim := read(slimTTs, last)
	if slices.Equal(slim.at, fat.at) && slices.Equal(slim.off, fat.off) && slim.first == fat.first {
		return []srcOffsets{fat}
	}
	return []srcOffsets{fat, slim}
}

// srcChange is a change of offset a footer's rule makes.
type srcChange struct{ at, off int64 }

// changes are the footer's offsets from from to to: the offset in force at
// from (at no later than from), then each change after it, its two rules
// applied every year (a footer knows no first year).
func (f *srcFooter) changes(from, to int64) []srcChange {
	y0 := time.Unix(from, 0).UTC().Year() - 1
	y1 := time.Unix(to, 0).UTC().Year()
	stdoff := f.line.stdoff
	save := f.std.save
	var out []srcChange
	for y := int64(y0); y <= int64(y1); y++ {
		rs := []*srcRule{f.std, f.dst}
		ts := []int64{rs[0].rpytime(y), rs[1].rpytime(y)}
		done := []bool{false, false}
		for n := 0; n < 2; n++ {
			k, kt := -1, int64(0)
			for j := range rs {
				if done[j] {
					continue
				}
				if jt := rs[j].utc(ts[j], stdoff, save); k < 0 || jt < kt {
					k, kt = j, jt
				}
			}
			done[k] = true
			save = rs[k].save
			out = append(out, srcChange{kt, stdoff + save})
		}
	}
	// The offset in force at from: the last change at or before it.
	i := sort.Search(len(out), func(i int) bool { return out[i].at > from })
	if i == 0 {
		return out
	}
	return out[i-1:]
}

// digest is ZoneRulesDigest's digest of the reading.
func (o srcOffsets) digest() uint64 {
	var b []byte
	b = binary.LittleEndian.AppendUint64(b, uint64(zoneDigestFrom.Unix()))
	b = binary.LittleEndian.AppendUint64(b, uint64(o.first))
	for i := range o.at {
		b = binary.LittleEndian.AppendUint64(b, uint64(o.at[i]))
		b = binary.LittleEndian.AppendUint64(b, uint64(o.off[i]))
	}
	sum := sha256.Sum256(b)
	return binary.LittleEndian.Uint64(sum[:8])
}

// Digests are the digests (ZoneRulesDigest, from 1970 to 2038) of the
// offsets a zone file of the zone or link name compiled from this source
// reads: one, or two where its fat and its slim file read apart
// (srcZone.readings); false where the source names no such zone.
func (s *ZoneSource) Digests(name string) ([]uint64, bool) {
	z, ok := s.resolve(name)
	if !ok {
		return nil, false
	}
	s.mu.Lock()
	ds, ok := s.byZone[z]
	s.mu.Unlock()
	if ok {
		return ds, true
	}
	for _, o := range compileZone(s.zones[z]).readings() {
		ds = append(ds, o.digest())
	}
	s.mu.Lock()
	if s.byZone == nil {
		s.byZone = map[string][]uint64{}
	}
	s.byZone[z] = ds
	s.mu.Unlock()
	return ds, true
}

// Gives reports whether a zone file of name compiled from this source
// reads the offsets whose digest is d (Digests).
func (s *ZoneSource) Gives(name string, d uint64) bool {
	ds, _ := s.Digests(name)
	return slices.Contains(ds, d)
}

// SummerAllYear reports whether the zone of name keeps summer time all
// year from its last change on: its last line's latest rule (by its last
// year, then its day) is a summer one, with no standard rule of the "max"
// year beside it, or the line's own SAVE is summer time. zic writes the
// footer of such a zone as summer time from each 1 January to each 31
// December, which Go reads by the UTC year (with a gap of standard time at
// each UTC new year in a zone east of UTC: Africa/Freetown of tzdata
// 2022b's backzone); Digests reads it as its fat file lists it.
func (s *ZoneSource) SummerAllYear(name string) bool {
	z, ok := s.resolve(name)
	if !ok {
		return false
	}
	zl := &s.zones[z][len(s.zones[z])-1]
	if !zl.ruled {
		return zl.isdst
	}
	var last [2]*srcRule
	for i := range zl.rules {
		r := &zl.rules[i]
		k := 0
		if r.isdst {
			k = 1
		}
		if c := ruleCmp(last[k], r); c < 0 {
			last[k] = r
		} else if c == 0 {
			return false
		}
	}
	return ruleCmp(last[1], last[0]) > 0
}

// ruleCmp orders two rules by the last instant they apply in, as zic's
// rule_cmp: by their last year, then their month and day (two of the
// "max" year are equal); no rule is before any.
func ruleCmp(a, b *srcRule) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case a.to != b.to:
		if a.to < b.to {
			return -1
		}
		return 1
	case a.to == srcMaxYear:
		return 0
	case a.month != b.month:
		return int(a.month) - int(b.month)
	}
	return a.day - b.day
}

// Holding is the names whose offsets from 1970 to 2038 the source gives
// digest d (Digests), sorted; none where it gives them none.
func (s *ZoneSource) Holding(d uint64) []string {
	var names []string
	for _, n := range s.Names() {
		if s.Gives(n, d) {
			names = append(names, n)
		}
	}
	return names
}

// Extract is the source text of name alone: the version line, the Link
// lines from name to its zone, the zone's lines and the Rule lines they
// use, which ParseZoneSource reads back to the same Digests of name. nil
// where the source names no such zone.
func (s *ZoneSource) Extract(name string) []byte {
	z, ok := s.resolve(name)
	if !ok {
		return nil
	}
	var b bytes.Buffer
	if s.version != "" {
		fmt.Fprintf(&b, "# version %s\n", s.version)
	}
	seen := map[string]bool{}
	for _, zl := range s.zones[z] {
		if zl.ruled && !seen[zl.rule] {
			seen[zl.rule] = true
			for _, l := range s.rtext[zl.rule] {
				b.WriteString(l + "\n")
			}
		}
	}
	for _, l := range s.ztext[z] {
		b.WriteString(l + "\n")
	}
	for n := name; n != z; n = s.links[n] {
		b.WriteString(s.ltext[n] + "\n")
	}
	return b.Bytes()
}
