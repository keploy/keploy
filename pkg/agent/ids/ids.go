package ids

import (
	"strings"
	"sync"
)

type Map struct {
	mu     sync.Mutex
	next   map[string]string
	prev   map[string]string
	banned map[string]bool
	rep    *strings.Replacer
	back   *strings.Replacer
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
	return len(m.next) == 0
}

func (m *Map) Rewrite(s string) string {
	m.mu.Lock()
	if len(m.next) == 0 {
		m.mu.Unlock()
		return s
	}
	if m.rep == nil {
		pairs := make([]string, 0, 2*len(m.next))
		for old, cur := range m.next {
			pairs = append(pairs, old, cur)
		}
		m.rep = strings.NewReplacer(pairs...)
	}
	rep := m.rep
	m.mu.Unlock()
	return rep.Replace(s)
}

// Restore is Rewrite the other way: each id this run made in s is put back as
// the recording's, so a call that carries one (a find by an _id the app's
// insert just made) is matched against the recording that carried the
// recorded id.
func (m *Map) Restore(s string) string {
	m.mu.Lock()
	if len(m.prev) == 0 {
		m.mu.Unlock()
		return s
	}
	if m.back == nil {
		pairs := make([]string, 0, 2*len(m.prev))
		for cur, old := range m.prev {
			pairs = append(pairs, cur, old)
		}
		m.back = strings.NewReplacer(pairs...)
	}
	back := m.back
	m.mu.Unlock()
	return back.Replace(s)
}

func (m *Map) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next, m.prev, m.banned, m.rep, m.back = nil, nil, nil, nil, nil
}

func New(pairs map[string]string) *Map {
	m := &Map{}
	for old, cur := range pairs {
		m.Add(old, cur)
	}
	return m
}
