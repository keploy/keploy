//go:build ignore

// Command learnclocks writes, for the clock-zone e2e
// (../proven-clock-zone-linux.sh), the `clocks` block of each recorded HTTP
// mock, made by the learn step's code and written onto the set's mocks by
// the code that writes what record's full round learned
// (mockdb.MockYaml.PersistLearned: learn.FoldLearned, learn.MergeLearned,
// the mock file's rewriter): a change to what learn learns of a read of the
// clock, to how it merges it, or to how the block is written, changes what
// the lane replays, so the lane breaks on it instead of on a block typed by
// hand.
//
// OSS keploy has no learn round (record's full round F, which runs every
// test a learn.FullRoundLead past its recorded instant), so this command
// plays F's run of each HTTP mock's call:
//
//   - the set's mocks are read by keploy's mock reader
//     (mockdb.MockYaml.GetTestSetMocks);
//   - each -place, a wall clock of no zone the app writes in the layout
//     given (Go's), must hold the recorded call's own reading in that layout
//     in the app's zone (that of its instant, or of a minute before);
//   - the place is written as read at R, 23:59:30 in the app's zone on the
//     mock's day, for the recording, and at R + FullRoundLead for F. The
//     recording's own values are not used: a 61.545 s lead moves a day or an
//     hour only across its boundary, so record's learn step learns such a
//     place only from a recording made in the lead's reach of one, and this
//     is the block it writes for one made at R. At R the day is one in UTC
//     and in the app's zone (+05:30), so it is learned zone: either; a
//     recording before 05:30 IST would learn the day's zone app, and one in
//     the hour after midnight cuts the hour to a day (its T00 shows no finer
//     cut): neither is exercised here. A zone whose clock jumps at midnight
//     (DST) may give F no next day: the command then fails, naming the drift;
//   - the request body is shaped and compared as the HTTP learn step shapes
//     a JSON body (learn.DecodeJSON, learn.JSONLeaves, learn.Compare), and
//     the drift is classified (learn.ClassifyIn) in the zone keploy reads
//     from the app's TZ (learn.ReadZone, named as learn.ReadProcZone names
//     it, through Tracker.DriftsOf), with no window, as a test's own call is.
//     Each place must be learned as a read of the clock (Learned.Clock), and
//     its reads go onto the mock's state as the agent's consume does
//     (models.MergeClocks, then models.MockState.Clocks);
//   - the states are written by PersistLearned under KEPLOY_PROVEN=1, as the
//     replay service writes record's full round's learning.
//
// What it does NOT run of the HTTP learn step (package http's, unexported):
// learnNoise.shape's url, query and header leaves, its shape key and media
// type (only the body's places are learned here, and both runs share the
// rest); the recorded answer's header leaves (respLeaves; the payments
// service's answer echoes nothing); keploy.yml's and the mock's noise
// (Known: the lane's mocks carry none); Pair (one candidate); the earlier
// places, the earlier recordings and what F gave the app before the call
// (ClassifyGiven's earlier, raw and given: here none, so no value is read as
// a copy of another's); learnPlan, which collects Learned.Clock as done
// here; and the replay service's filters of unread learning. The app's
// layouts (-place) are typed in the lane from the app's main.go.
//
// Run it from the keploy module's root, as one file (it is no package of the
// lane's own module, which does not require keploy):
//
//	KEPLOY_PROVEN=1 go run ./.github/workflows/test_workflow_scripts/golang/clock-zone/learnclocks/main.go \
//	    -path keploy -set test-set-0 -zone Asia/Kolkata \
//	    -place body.day=2006-01-02 -place body.hour=2006-01-02T15
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

type placeFlags map[string]string

func (p placeFlags) String() string { return fmt.Sprint(map[string]string(p)) }

func (p placeFlags) Set(v string) error {
	path, layout, ok := strings.Cut(v, "=")
	if !ok || path == "" || layout == "" {
		return fmt.Errorf("want PATH=GO-LAYOUT, got %q", v)
	}
	p[path] = layout
	return nil
}

func main() {
	path := flag.String("path", "keploy", "the recording's keploy directory")
	set := flag.String("set", "test-set-0", "the test set")
	zone := flag.String("zone", "", "the TZ the app was recorded with")
	places := placeFlags{}
	flag.Var(places, "place", "PATH=GO-LAYOUT: a wall clock of no zone the app writes there (repeat)")
	flag.Parse()
	if *zone == "" || len(places) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*path, *set, *zone, places); err != nil {
		fmt.Fprintln(os.Stderr, "learnclocks:", err)
		os.Exit(1)
	}
}

func run(path, set, zone string, places placeFlags) error {
	if !provenswitch.On() {
		return errors.New("run it under KEPLOY_PROVEN=1: record's learn step writes clock reads only under the proven switch")
	}
	// The app reads its zone from the host's files, as keploy reads a
	// process's: never from the zones built into keploy, which ReadZone
	// falls back to.
	if _, err := os.Stat(filepath.Join("/usr/share/zoneinfo", zone)); err != nil {
		return fmt.Errorf("no zone file for %q: %w", zone, err)
	}
	loc, err := learn.ReadZone("/", zone, true)
	if err != nil {
		return fmt.Errorf("read the zone %q: %w", zone, err)
	}
	pz := learn.ProcZone{Loc: loc, Name: models.ZoneNameOf(loc, time.Now())}
	ctx := context.Background()
	logger := zap.NewNop()
	db := mockdb.New(logger, path, "")
	read, err := db.GetTestSetMocks(ctx, set, time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		return fmt.Errorf("read the mocks of %s: %w", set, err)
	}
	states := map[string]models.MockState{}
	for _, m := range append(append([]*models.Mock(nil), read.AllPerTest...), read.AllSession...) {
		if m.Kind != models.HTTP {
			continue
		}
		clocks, err := learnMock(m, loc, pz, places)
		if err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
		// The consumed mock's spec, as the HTTP learn step leaves it, and
		// the state the agent sends of it.
		states[m.Name] = models.MockState{Name: m.Name, Kind: m.Kind, Clocks: models.MergeClocks(m.Spec.Clocks, clocks)}
	}
	if len(states) == 0 {
		return fmt.Errorf("%s holds no HTTP mock", set)
	}
	added, err := db.PersistLearned(ctx, set, states)
	if err != nil {
		return fmt.Errorf("write what was learned onto %s's mocks: %w", set, err)
	}
	for name := range states {
		if len(added[name]) != len(places) {
			return fmt.Errorf("%s: the writer added %v, want a read of the clock at each of %d places", name, added[name], len(places))
		}
	}
	names := make([]string, 0, len(added))
	for name := range added {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s: %s\n", name, strings.Join(added[name], ", "))
	}
	return nil
}

// learnMock is what the learn step learns of m's request body when F's run
// writes each place a FullRoundLead past the recording's, read at R, the
// end of m's day in the app's zone: the clock read learned at each place.
func learnMock(m *models.Mock, loc *time.Location, pz learn.ProcZone, places placeFlags) (map[string]models.ClockOffset, error) {
	if m.Spec.HTTPReq == nil {
		return nil, errors.New("no request")
	}
	body := m.Spec.HTTPReq.Body
	at := m.Spec.ReqTimestampMock.In(loc)
	for path, layout := range places {
		v, err := placeValue(body, path)
		if err != nil {
			return nil, err
		}
		// The app read its clock for the call just before keploy stamped
		// the call, and wrote it in its zone.
		if v != at.Format(layout) && v != at.Add(-time.Minute).Format(layout) {
			return nil, fmt.Errorf("the recorded %s %q is not the call's own reading (%s) in the layout %q in %s", path, v, at.Format(time.RFC3339Nano), layout, pz.Name)
		}
	}
	rec := time.Date(at.Year(), at.Month(), at.Day(), 23, 59, 30, 0, loc)
	f := rec.Add(learn.FullRoundLead)
	// The call reaches keploy just after the app read its clock for it.
	call := rec.Add(time.Millisecond)
	recShape, err := bodyShape(body, places, rec)
	if err != nil {
		return nil, err
	}
	fShape, err := bodyShape(body, places, f)
	if err != nil {
		return nil, err
	}
	diff := learn.Compare(fShape, recShape, learn.None)
	if diff.Structural != "" {
		return nil, fmt.Errorf("F's body differs from the recording's in its structure: %s", diff.Structural)
	}
	want := make([]string, 0, len(places))
	for path := range places {
		want = append(want, path)
	}
	sort.Strings(want)
	if strings.Join(diff.Drift, ",") != strings.Join(want, ",") {
		return nil, fmt.Errorf("drifted %v between %s and F's %s, want %v", diff.Drift, rec, f, want)
	}
	var resp []learn.Leaf
	if m.Spec.HTTPResp != nil {
		if v, ok := learn.DecodeJSON([]byte(m.Spec.HTTPResp.Body)); ok {
			leaves := map[string]learn.Leaf{}
			learn.JSONLeaves("body", v, leaves)
			for _, l := range leaves {
				resp = append(resp, l)
			}
			sort.Slice(resp, func(i, j int) bool { return resp[i].Path < resp[j].Path })
		}
	}
	tr := learn.NewTracker()
	tr.SetAppClock(func(real time.Time) (time.Time, bool) { return real, true })
	dc := tr.DriftsOf(learn.WithProcZone(context.Background(), pz))
	clocks := map[string]models.ClockOffset{}
	for _, l := range learn.ClassifyIn(dc, diff.Drift, fShape, recShape, call, nil, nil, resp, nil) {
		o, ok := l.Clock()
		if !ok {
			return nil, fmt.Errorf("%s was learned as %s, not as a read of the clock: %s", l.Recorded.Path, l.Kind, l.Why)
		}
		clocks[l.Recorded.Path] = o
	}
	if len(clocks) != len(places) {
		return nil, fmt.Errorf("learned %v, want a read of the clock at each of %v", clocks, want)
	}
	return clocks, nil
}

// placeValue is the string at path (body.<key>) of a JSON body.
func placeValue(body, path string) (string, error) {
	v, ok := learn.DecodeJSON([]byte(body))
	if !ok {
		return "", fmt.Errorf("the request body is no JSON: %q", body)
	}
	leaves := map[string]learn.Leaf{}
	learn.JSONLeaves("body", v, leaves)
	l, ok := leaves[path]
	if !ok || l.Type != learn.String {
		return "", fmt.Errorf("the request body holds no string at %s: %q", path, body)
	}
	return l.Value, nil
}

// bodyShape is the learn step's shape of a JSON body (the HTTP learn step's
// learnNoise.shape of one, its body part) with each place written at t in
// its layout, in t's zone.
func bodyShape(body string, places placeFlags, t time.Time) (learn.Shape, error) {
	v, ok := learn.DecodeJSON([]byte(body))
	if !ok {
		return learn.Shape{}, fmt.Errorf("the request body is no JSON: %q", body)
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return learn.Shape{}, fmt.Errorf("the request body is no JSON object: %q", body)
	}
	for path, layout := range places {
		key, ok := strings.CutPrefix(path, "body.")
		if !ok || strings.ContainsAny(key, ".[") {
			return learn.Shape{}, fmt.Errorf("%s is no top-level body field", path)
		}
		obj[key] = t.Format(layout)
	}
	s := learn.Shape{Fixed: map[string]string{"body": "json"}, Body: obj, Leaves: map[string]learn.Leaf{}}
	learn.JSONLeaves("body", obj, s.Leaves)
	return s, nil
}
