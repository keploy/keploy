// Package captureloss counts the calls the agent intercepted and could not
// record or mock (design v14 §3.2, rule 12: capture is never silent): a TLS
// client that rejected keploy's certificate, a STARTTLS upgrade keploy could
// not make, and each later kind. The agent adds these counts to the ones its
// hooks keep (Agent.CaptureLosses), and under the proven switch every test
// that ends after a loss is charged with it -- but for a test's own kinds
// (models.CaptureLossKind.PerTest), charged to the test they happened in.
package captureloss

import (
	"sort"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// Counter counts losses by kind, each with the latest one's example. Counts
// only ever grow. The zero value is ready to use.
type Counter struct {
	mu     sync.Mutex
	byKind map[models.CaptureLossKind]*models.CaptureLoss
}

// Add counts one loss of kind; what names it (never a value the app sent).
func (c *Counter) Add(kind models.CaptureLossKind, what string) {
	c.AddN(kind, 1, what)
}

// AddN counts n losses of kind at once, as Add does one: a count the agent
// reads whole, such as the kernel's drops at the proxy's listener in one
// test's window. Zero counts nothing.
func (c *Counter) AddN(kind models.CaptureLossKind, n uint64, what string) {
	if n == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKind == nil {
		c.byKind = make(map[models.CaptureLossKind]*models.CaptureLoss)
	}
	l := c.byKind[kind]
	if l == nil {
		l = &models.CaptureLoss{Kind: kind}
		c.byKind[kind] = l
	}
	l.Count += n
	l.What = what
}

// Snapshot is every kind's count, sorted by kind.
func (c *Counter) Snapshot() models.CaptureLosses {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(models.CaptureLosses, 0, len(c.byKind))
	for _, l := range c.byKind {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}
