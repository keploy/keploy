package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// agentDb is learnDb as the agent's store is in keploy mock replay's round F: it knows no recorded test
// window (mock replay seeds none, so integrations.TestWindows answers known false), and the replay's scope
// table names which mocks are the pool's (integrations.PoolMembership); while no test is open the store
// serves the claim key's own pool slice (MockManager.ServeExactPool).
type agentDb struct {
	*learnDb
	slice map[string]bool
	tr    *learn.Tracker
}

func (d *agentDb) InRecordedTestWindow(time.Time) (bool, bool)              { return false, false }
func (d *agentDb) SameRecordedTestWindow(time.Time, time.Time) (bool, bool) { return false, false }
func (d *agentDb) InPool(m *models.Mock) (bool, bool)                       { return d.slice[m.Name], true }

// atgr1 round F: a spec's beforeAll creates an app named after Date.now; keploy mock replay seeds no
// recorded test window, so the learn step could not tell the beforeAll's recording was the claim key's and
// refused the call ("its closest recorded call is not one the learn step may learn onto"). Whose a call
// outside every test is, is the claim key's pool slice the store serves, by the record's mapping, never a
// time: the beforeAll's call is paired there, and the key's test that lists the roles is answered with
// this run's name.
func TestLearnMatchABeforeAllCallIsPairedAsThePools(t *testing.T) {
	h, n, opts := learnOn(t)
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.NewKey("e2e/roles.spec.ts")
	ctx := learn.WithTracker(context.Background(), tr)
	base, _ := roleDb(tr)
	db := &agentDb{learnDb: base, slice: map[string]bool{"pool-0": true}, tr: tr}
	calls := roleCalls(fRole)
	if out, miss, err := h.learnMatch(ctx, calls[0], db, n, opts); out == nil || miss != nil || err != nil {
		why := ""
		if miss != nil {
			why = miss.res.Why
		}
		t.Fatalf("the beforeAll's call was not answered: %s (err %v)", why, err)
	}
	tr.Begin()
	out, miss, _ := h.learnMatch(ctx, calls[1], db, n, opts)
	if out == nil || miss != nil || out.Spec.HTTPResp.Body != `{"data":{"roles":[{"name":"admin"},{"name":"`+fRole+`"}]}}` {
		t.Fatalf("the list: served %v miss %+v", out, miss)
	}
	if out, miss, _ := h.learnMatch(ctx, calls[2], db, n, opts); out == nil || miss != nil {
		t.Fatalf("the assign: %+v", miss)
	}
}

var _ = models.HTTP
