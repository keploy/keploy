package ids

import (
	"strings"
	"sync"
)

type Map struct {
	mu   sync.Mutex
	next map[string]string
	prev map[string]string
	rep  *strings.Replacer
}

var Default = &Map{}

func (m *Map) Add(old, cur string) {
	if old == "" || cur == "" || old == cur || len(old) != len(cur) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.next == nil {
		m.next, m.prev = map[string]string{}, map[string]string{}
	}
	if _, ok := m.next[old]; ok {
		return
	}
	if _, ok := m.prev[cur]; ok {
		return
	}
	m.next[old], m.prev[cur] = cur, old
	m.rep = nil
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

func (m *Map) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next, m.prev, m.rep = nil, nil, nil
}

func New(pairs map[string]string) *Map {
	m := &Map{}
	for old, cur := range pairs {
		m.Add(old, cur)
	}
	return m
}
