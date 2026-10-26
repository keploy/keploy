package mysql

// ResetWireCache empties the encode cache and sets its budget, for a test; the
// func it returns empties the cache again and restores the budget it replaced.
func ResetWireCache(budget int64) (restore func()) {
	wireCache.mu.Lock()
	defer wireCache.mu.Unlock()
	old := wireCache.budget
	wireCache.evictTo(0)
	wireCache.budget = budget
	return func() { ResetWireCache(old) }
}
