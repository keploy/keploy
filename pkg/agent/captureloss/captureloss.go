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

// Add counts one loss of kind; what names it (never a value the app sent),
// and hosts are the host names what names whole, each where what writes it,
// with the tag the agent's lines name it by (models.LossText): a log line of
// keploy's writes each by its shape and tag there, and reads nothing else in
// what as a host (models.CaptureLoss.Hosts). Every host what names is
// listed: one left out would be logged whole.
func (c *Counter) Add(kind models.CaptureLossKind, what string, hosts ...models.LossHost) {
	c.AddN(kind, 1, what, hosts...)
}

// AddN counts n losses of kind at once, as Add does one: a count the agent
// reads whole, such as the kernel's drops at the proxy's listener in one
// test's window. Zero counts nothing.
func (c *Counter) AddN(kind models.CaptureLossKind, n uint64, what string, hosts ...models.LossHost) {
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
	// The latest loss's What, with the hosts it names: this agent lists
	// them (HostsListed).
	l.SetWhatOf(models.CaptureLoss{What: what, Hosts: append(models.LossHosts(nil), hosts...), HostsListed: true})
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
