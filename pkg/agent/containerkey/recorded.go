package containerkey

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

// RecordedTally counts, over a test set's mocks, the containers each
// recorded group (one key in one recorded scope) came from.
type RecordedTally struct {
	groups map[recordedGroup]map[string]bool
	// named: a mock of the set carries a container stamp
	// (RecordedContainer), whether or not its key could be read: the set
	// was recorded naming containers.
	named bool
}

// Add counts m: a mock of a container, stamped with its key, instance and
// scope (Stamp). Any other mock counts for nothing.
func (t *RecordedTally) Add(m *models.Mock) {
	if m == nil {
		return
	}
	md := m.Spec.Metadata
	key, inst, scope := md[MetadataKey], md[InstanceKey], md[ScopeKey]
	if inst != "" {
		t.named = true
	}
	if key == "" || inst == "" {
		return
	}
	if t.groups == nil {
		t.groups = map[recordedGroup]map[string]bool{}
	}
	g := recordedGroup{key: key, scope: scope}
	if t.groups[g] == nil {
		t.groups[g] = map[string]bool{}
	}
	t.groups[g][inst] = true
}

// SetRecorded takes the tally of the test set the agent was given: its mocks
// of a key that, in one recorded scope, came from two or more containers
// (configured the same, so their key cannot tell them apart) are ambiguous
// from now on (RecordedAmbiguous), until the next set's tally.
func (k *Keyer) SetRecorded(t *RecordedTally) {
	amb := map[recordedGroup]bool{}
	for g, insts := range t.groups {
		if len(insts) > 1 {
			amb[g] = true
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.recorded = amb
	k.recordedNamed = t.named
}

// RecordingNamesContainers reports whether the test set the agent was given
// (SetRecorded) was recorded naming containers — a mock of it, of any
// destination, carries a container stamp — as the keyer of the connection
// ctx carries knows it. False with no keyer in ctx, or before any set.
func RecordingNamesContainers(ctx context.Context) bool {
	ref, ok := ctx.Value(ctxKey{}).(connRef)
	if !ok || ref.k == nil {
		return false
	}
	ref.k.mu.Lock()
	defer ref.k.mu.Unlock()
	return ref.k.recordedNamed
}

// RecordedAmbiguous reports whether m is a mock of a container key that, in
// the scope m was recorded in, answered two or more containers: m is then
// no call's by its key.
func (k *Keyer) RecordedAmbiguous(m *models.Mock) bool {
	if m == nil {
		return false
	}
	md := m.Spec.Metadata
	g := recordedGroup{key: md[MetadataKey], scope: md[ScopeKey]}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.recorded[g]
}

// RecordedContainer reports whether m came from a container destination
// under the proven switch (Stamp wrote its instance), whether or not its key
// could be read.
func RecordedContainer(m *models.Mock) bool {
	return m != nil && m.Spec.Metadata[InstanceKey] != ""
}
