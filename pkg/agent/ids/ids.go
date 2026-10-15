// Package ids pairs each value a recording holds that the application made
// anew (an id, a time) with the value it made in its place in this run, and
// writes one for the other.
package ids

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Map struct {
	mu     sync.Mutex
	next   map[string]string
	prev   map[string]string
	banned map[string]bool
	rep    *swapper
	back   *swapper
	times  *Map
	first  bool
}

var Default = &Map{}

func (m *Map) Add(old, cur string) {
	if old == "" || cur == "" || old == cur || len(old) != len(cur) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.next == nil {
		m.next, m.prev, m.banned = map[string]string{}, map[string]string{}, map[string]bool{}
	}
	if m.banned[old] || m.banned[cur] {
		return
	}
	if m.first {
		if _, ok := m.next[old]; !ok {
			m.next[old] = cur
			if _, ok := m.prev[cur]; !ok {
				m.prev[cur] = old
			}
			m.rep, m.back = nil, nil
		}
		return
	}
	if was, ok := m.next[old]; ok {
		if was != cur {
			m.ban(old, was)
		}
		return
	}
	if was, ok := m.prev[cur]; ok {
		m.ban(was, cur)
		return
	}
	m.next[old], m.prev[cur] = cur, old
	m.rep, m.back = nil, nil
}

func (m *Map) ban(old, cur string) {
	delete(m.next, old)
	delete(m.prev, cur)
	m.banned[old], m.banned[cur] = true, true
	m.rep, m.back = nil, nil
}

func (m *Map) Pairs() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.next))
	for k, v := range m.next {
		out[k] = v
	}
	return out
}

func (m *Map) Empty() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.next) == 0 && (m.times == nil || m.times.Empty())
}

// AddTime pairs a time the recording holds in field with the one this run
// wrote there. A time is paired by field: one recorded second is often the
// time of several fields, which this run writes in different seconds. The
// first pair of a recorded time holds: documents written in one recorded
// second keep one time, so their recorded order holds.
func (m *Map) AddTime(field, old, cur string) {
	if field == "" {
		return
	}
	m.mu.Lock()
	if m.times == nil {
		m.times = &Map{first: true}
	}
	t := m.times
	m.mu.Unlock()
	t.Add(field+"\x00"+old, field+"\x00"+cur)
}

func (m *Map) timeMap() *Map {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.times
}

var timeField = regexp.MustCompile(`"(\w+)"\s*:\s*(?:\{\s*"\$date"\s*:\s*)?(?:\{\s*"\$numberLong"\s*:\s*)?"?(\d{19}|\d{16}|\d{13}|\d{10}|\d{4}-\d\d-\d\dT[0-9:.]+(?:Z|[+-]\d\d:\d\d))\b`)

func swapTimes(s string, pairs map[string]string) string {
	if len(pairs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range timeField.FindAllStringSubmatchIndex(s, -1) {
		field, value := s[loc[2]:loc[3]], s[loc[4]:loc[5]]
		to, ok := pairs[field+"\x00"+value]
		if !ok {
			continue
		}
		_, v, _ := strings.Cut(to, "\x00")
		b.WriteString(s[last:loc[4]])
		b.WriteString(v)
		last = loc[5]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// Rewrite writes, in s, the value this run made in place of each recorded one.
func (m *Map) Rewrite(s string) string {
	if t := m.timeMap(); t != nil {
		s = swapTimes(s, t.Pairs())
	}
	m.mu.Lock()
	if len(m.next) == 0 {
		m.mu.Unlock()
		return s
	}
	if m.rep == nil {
		m.rep = newSwapper(m.next)
	}
	rep := m.rep
	m.mu.Unlock()
	return rep.swap(s)
}

// Restore writes, in s, the recorded value in place of each one this run made.
func (m *Map) Restore(s string) string {
	if t := m.timeMap(); t != nil {
		back := map[string]string{}
		for old, cur := range t.Pairs() {
			back[cur] = old
		}
		s = swapTimes(s, back)
	}
	m.mu.Lock()
	if len(m.prev) == 0 {
		m.mu.Unlock()
		return s
	}
	if m.back == nil {
		m.back = newSwapper(m.prev)
	}
	back := m.back
	m.mu.Unlock()
	return back.swap(s)
}

func (m *Map) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next, m.prev, m.banned, m.rep, m.back, m.times = nil, nil, nil, nil, nil, nil
}

func New(pairs map[string]string) *Map {
	m := &Map{}
	for old, cur := range pairs {
		m.Add(old, cur)
	}
	return m
}

type swapper struct {
	text *strings.Replacer
	nums map[string]string
}

func newSwapper(pairs map[string]string) *swapper {
	s := &swapper{nums: map[string]string{}}
	var text []string
	for from, to := range pairs {
		if digits(from) {
			s.nums[from] = to
			continue
		}
		text = append(text, from, to)
	}
	if len(text) > 0 {
		s.text = strings.NewReplacer(text...)
	}
	return s
}

func (s *swapper) swap(in string) string {
	if s.text != nil {
		in = s.text.Replace(in)
	}
	if len(s.nums) == 0 {
		return in
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(in); {
		if !isDigit(in[i]) || (i > 0 && isWordByte(in[i-1])) {
			i++
			continue
		}
		j := i
		for j < len(in) && isDigit(in[j]) {
			j++
		}
		if to, ok := s.nums[in[i:j]]; ok && (j == len(in) || !isWordByte(in[j])) {
			b.WriteString(in[last:i])
			b.WriteString(to)
			last = j
		}
		i = j
	}
	if last == 0 {
		return in
	}
	b.WriteString(in[last:])
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isWordByte(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '_' || c == '-'
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// IsID reports whether s reads as an id an application makes anew: a
// 24-hex ObjectID or a UUID.
func IsID(s string) bool {
	switch len(s) {
	case 24:
		return hex(s)
	case 36:
		for i := 0; i < len(s); i++ {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				if s[i] != '-' {
					return false
				}
				continue
			}
			if !hex(s[i : i+1]) {
				return false
			}
		}
		return true
	}
	return false
}

func hex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isDigit(c) && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

var (
	earliest = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	latest   = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// IsTime reports whether s reads as a time an application makes anew: an
// RFC 3339 time, or Unix seconds, milliseconds, microseconds or nanoseconds,
// between 2000 and 2100.
func IsTime(s string) bool {
	if digits(s) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return false
		}
		var t time.Time
		switch len(s) {
		case 10:
			t = time.Unix(n, 0)
		case 13:
			t = time.UnixMilli(n)
		case 16:
			t = time.UnixMicro(n)
		case 19:
			t = time.Unix(0, n)
		default:
			return false
		}
		return t.After(earliest) && t.Before(latest)
	}
	if len(s) < len("2006-01-02T15:04:05Z") || s[4] != '-' || s[10] != 'T' {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return err == nil && t.After(earliest) && t.Before(latest)
}

// Pair walks rec, a recorded value, and live, the value this run sent in its
// place, and pairs in m each id and time that differs between them, place by
// place: a time by the field it is in. Values are those encoding/json decodes,
// numbers as json.Number.
func (m *Map) Pair(rec, live interface{}) {
	m.pair("", rec, live)
}

func (m *Map) pair(field string, rec, live interface{}) {
	switch r := rec.(type) {
	case map[string]interface{}:
		l, ok := live.(map[string]interface{})
		if !ok {
			return
		}
		for k, rv := range r {
			lv, ok := l[k]
			if !ok {
				continue
			}
			if strings.HasPrefix(k, "$") {
				m.pair(field, rv, lv)
			} else {
				m.pair(k, rv, lv)
			}
		}
	case []interface{}:
		l, ok := live.([]interface{})
		if !ok || len(l) != len(r) {
			return
		}
		for i := range r {
			m.pair(field, r[i], l[i])
		}
	case string:
		if l, ok := live.(string); ok {
			m.PairValue(field, r, l)
		}
	case json.Number:
		if l, ok := live.(json.Number); ok {
			m.PairValue(field, string(r), string(l))
		}
	}
}

// PairValue pairs a recorded value with the live one in its place: an id
// wherever it is, a time by its field.
func (m *Map) PairValue(field, rec, live string) {
	switch {
	case rec == live:
	case IsID(rec) && IsID(live):
		m.Add(rec, live)
	case IsTime(rec) && IsTime(live):
		m.AddTime(field, rec, live)
	}
}

// PairJSON is Pair over two JSON documents; it pairs nothing when either is
// not JSON.
func (m *Map) PairJSON(rec, live []byte) {
	r, ok := decode(rec)
	if !ok {
		return
	}
	l, ok := decode(live)
	if !ok {
		return
	}
	m.Pair(r, l)
}

func decode(b []byte) (interface{}, bool) {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	var v interface{}
	if err := d.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// PairPath pairs the ids in the segments of two URL paths with the same
// number of segments.
func (m *Map) PairPath(rec, live string) {
	r, l := strings.Split(rec, "/"), strings.Split(live, "/")
	if len(r) != len(l) {
		return
	}
	for i := range r {
		if r[i] != l[i] && IsID(r[i]) && IsID(l[i]) {
			m.Add(r[i], l[i])
		}
	}
}
