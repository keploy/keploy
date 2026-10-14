//go:build race

package reportdb

// raceEnabled: reading back a report of 75 MB takes the race runtime about
// half a minute, and several times the memory; it tests no concurrency.
const raceEnabled = true
