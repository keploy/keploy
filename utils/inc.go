package utils

import "sync"

type AutoInc struct {
	sync.Mutex // ensures autoInc is goroutine-safe
	id         int
}

func (a *AutoInc) Next() (id int) {
	a.Lock()
	defer a.Unlock()

	id = a.id
	a.id++
	return
}
