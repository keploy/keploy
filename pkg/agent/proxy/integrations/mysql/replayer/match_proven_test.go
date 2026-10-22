package replayer

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Under the proven model a COM_STMT_EXECUTE whose recording is in the pool
// (its query and every bound parameter exact) is answered with that
// recording, never with another recording of the same query: echo_mysql's
// seed handler reads each seeded row back by short_code in recorded order,
// and with the FIFO pick ranked above the exact one the sentinel's read was
// answered with the max row and the max's with the sentinel row, both
// recordings consumed, so the per-test mock check passed. Under exact
// serving no time window is active; the rule holds with one too, whichever
// pass of the scan finds the recording. With the proven model off the
// ranking is the switch-off path's, as it was. Mutation: rank the exact
// recording where it was (below the FIFO pick); let the window's FIFO
// candidate settle the first pass under the proven model.
func TestMatchCommand_UnderTheProvenModelAnExecuteIsAnsweredByItsRecording(t *testing.T) {
	const q = "SELECT * FROM `short_code_infos` WHERE short_code = ? ORDER BY updated_at desc"
	at := func(ms int) time.Time { return scanBase.Add(time.Duration(ms) * time.Millisecond) }
	// One PREPARE per read, each executed once: statement 10 the sentinel's,
	// 11 the max's, recorded in that order.
	pool := func() []*models.Mock {
		return []*models.Mock{
			execPrepareMock("prepare-10", q, 10),
			windowExecMock("exec-sentinel", 10, "dt-sentinel-9999-01-01T00:00:00Z", at(1)),
			execPrepareMock("prepare-11", q, 11),
			windowExecMock("exec-max", 11, "dt-max-9999-12-31T23:59:59.999999Z", at(2)),
		}
	}
	prep := func(s *mysqlDecodeCtxShim) {
		s.stmtIDToQuery[10], s.stmtIDToQuery[11] = q, q
	}
	reads := []struct {
		stmt        uint32
		param, want string
	}{
		{10, "dt-sentinel-9999-01-01T00:00:00Z", "exec-sentinel"},
		{11, "dt-max-9999-12-31T23:59:59.999999Z", "exec-max"},
		// The max's read on the sentinel's statement: still the max row.
		{10, "dt-max-9999-12-31T23:59:59.999999Z", "exec-max"},
	}
	for _, w := range []struct {
		name             string
		winStart, winEnd time.Time
	}{
		{"exact serving: no time window", time.Time{}, time.Time{}},
		{"a window that holds the other read only", at(2), at(3)},
	} {
		for _, mode := range []scanMode{modeFullScan, modeIndexed, modeFiltered} {
			for _, r := range reads {
				t.Setenv(provenswitch.Env, "1")
				got := runScan(t, mode, liveExecReq(r.stmt, r.param), nil, pool(), w.winStart, w.winEnd, nil, prep)
				if !strings.HasPrefix(got.served, r.want+"|") {
					t.Errorf("proven model, %s, %s: the read of %q on statement %d was answered with %q, want its recording %s", w.name, mode.name, r.param, r.stmt, got.served, r.want)
				}
			}
		}
	}
	// With the proven model off, exact serving's absent window ranks the
	// exact recording below the FIFO pick, as the switch-off path always
	// has: the first read is answered as before.
	t.Setenv(provenswitch.Env, "0")
	got := runScan(t, modeFullScan, liveExecReq(10, reads[0].param), nil, pool(), time.Time{}, time.Time{}, nil, prep)
	if !strings.HasPrefix(got.served, "exec-max|") {
		t.Errorf("with the proven model off the ranking changed: the sentinel's read was answered with %q, as before it was exec-max", got.served)
	}
}
