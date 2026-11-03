package replayer

import (
	"context"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// literalPrefix is the request-body path of a COM_QUERY's inline literals
// (queryBodyValue), as req_body_noise and MockSpec.Clocks name them: the
// N-th literal of the statement, counted from 0, is literalPrefix+"N".
const literalPrefix = "body.query.literals."

// LiteralCall is a live COM_QUERY of record's full round, under the proven
// switch, whose statement a recording holds with other inline literals
// (the same text once its literals are masked), and whose literals that
// differ are all the recording's difference from it: no other part of its
// body (the template, a query attribute) changed. Live and Recorded hold
// every literal of the two statements by its path (literalPrefix+"N"),
// Drift the paths at which they differ, in order.
type LiteralCall struct {
	MockDb   integrations.MockMemDb
	Recorded *models.Mock
	Live     map[string]learn.Leaf
	Rec      map[string]learn.Leaf
	Drift    []string
	// Arrived is when keploy read the command whole.
	Arrived time.Time
}

// LiteralLearner decides a LiteralCall: the reads of the application's
// clock it learns at the drifted literals, by path, or nil and why it
// learns none. The call is served only where it learns a read at every
// drifted literal; it never learns a literal as noise.
type LiteralLearner func(ctx context.Context, call LiteralCall) (map[string]models.ClockOffset, string)

var literalLearner atomic.Pointer[LiteralLearner]

// RegisterLiteralLearner sets the learner record's full round asks about a
// COM_QUERY whose recording differs from it only at inline literals (nil:
// none, and the call is refused as without it).
func RegisterLiteralLearner(fn LiteralLearner) {
	if fn == nil {
		literalLearner.Store(nil)
		return
	}
	literalLearner.Store(&fn)
}

// commandCall is what the command phase read of the command matchCommand
// decides: when it read it whole, and whether it is a call of record's
// full round (OutgoingOptions.Learn).
type commandCall struct {
	arrived time.Time
	learn   bool
}

type commandCallKey struct{}

// withCommand is ctx carrying c, for matchCommand.
func withCommand(ctx context.Context, c commandCall) context.Context {
	return context.WithValue(ctx, commandCallKey{}, c)
}

// commandOf is the commandCall on ctx; its zero value where there is none.
func commandOf(ctx context.Context) commandCall {
	if ctx == nil {
		return commandCall{}
	}
	c, _ := ctx.Value(commandCallKey{}).(commandCall)
	return c
}

// literalClocks are the literals of mock's statement that the learn step
// learned as reads of the application's clock (MockSpec.Clocks), under the
// proven switch; none without it, which compares them as before it.
func literalClocks(mock *models.Mock) map[string]models.ClockOffset {
	if mock == nil || len(mock.Spec.Clocks) == 0 || !provenswitch.On() {
		return nil
	}
	var out map[string]models.ClockOffset
	for p, o := range mock.Spec.Clocks {
		if !strings.HasPrefix(p, literalPrefix) {
			continue
		}
		if out == nil {
			out = make(map[string]models.ClockOffset, len(mock.Spec.Clocks))
		}
		out[p] = o
	}
	return out
}

// clockLeftOut is leftOut with the literals of mock the gate does not
// compare as values: those the learn step learned as reads of the
// application's clock, on the recording or for this command (learned),
// which clocksHold checks instead.
func (g *strictGate) clockLeftOut(mock *models.Mock, leftOut []string) []string {
	held, learned := literalClocks(mock), g.learned[mock]
	if len(held) == 0 && len(learned) == 0 {
		return leftOut
	}
	out := append([]string(nil), leftOut...)
	for _, set := range []map[string]models.ClockOffset{held, learned} {
		for p := range set {
			out = append(out, strings.TrimPrefix(p, "body."))
		}
	}
	sort.Strings(out[len(leftOut):])
	return out
}

// clocksHold reports whether the live statement carries, at every literal
// of mock's the learn step learned as a read of the application's clock, a
// read made in its own call's span on that clock (learn.CheckClockRead),
// checked whatever its value: a value equal to its recording is a read of
// this run's clock only where the clock showed it in the span. A literal
// the statement does not carry, or a span keploy cannot tell, refuses it.
// The first refusal's reason is kept for the miss (clockWhy). Each
// recording is checked once a command.
func (g *strictGate) clocksHold(mock *models.Mock) bool {
	held := literalClocks(mock)
	if len(held) == 0 {
		return true
	}
	if ok, seen := g.clockVerdicts[mock]; seen {
		return ok
	}
	ok, why := g.checkClocks(mock, held)
	if g.clockVerdicts == nil {
		g.clockVerdicts = make(map[*models.Mock]bool)
	}
	g.clockVerdicts[mock] = ok
	if !ok && g.clockWhy == "" {
		g.clockWhy = why
	}
	return ok
}

// allowsClocks is clocksHold for a recording of the live COM_QUERY's very
// text (no literal compared differs), counted with the rejected ones, and
// named as the closest, where it is no.
func (g *strictGate) allowsClocks(mock *models.Mock) bool {
	if g.clocksHold(mock) {
		return true
	}
	g.rejected++
	if g.closestMock == "" {
		g.closestMock = mock.Name
	}
	return false
}

func (g *strictGate) checkClocks(mock *models.Mock, held map[string]models.ClockOffset) (bool, string) {
	live := liveLiterals(g.live)
	call := commandOf(g.ctx)
	span, known := learn.ClockSpanOf(g.ctx, call.arrived, mock)
	paths := make([]string, 0, len(held))
	for p := range held {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if ok, why := learn.CheckClockRead(p, held[p], live[p], span, known); !ok {
			return false, why
		}
	}
	return true, ""
}

// learnsClocks is record's full round's rule for a recording of the live
// COM_QUERY's statement whose literals differ from it (allowsLiterals'
// drift): the registered LiteralLearner decides the call where every part
// of the body that drifted is an inline literal, and the call is served
// only where it learns a read of the application's clock at each of them,
// kept for the consumed copy (learnedClocks). Without the proven switch, a
// learner, a call of the full round, or a tracker whose learn step runs
// (learn.TrackerFrom), it learns nothing, as before it.
func (g *strictGate) learnsClocks(mock *models.Mock, drift map[string][]string) bool {
	fn := literalLearner.Load()
	call := commandOf(g.ctx)
	if fn == nil || !call.learn || !provenswitch.On() || learn.TrackerFrom(g.ctx) == nil || len(drift) == 0 {
		return false
	}
	paths := make([]string, 0, len(drift))
	for p := range drift {
		p = "body." + strings.TrimPrefix(p, "body.")
		if !strings.HasPrefix(p, literalPrefix) {
			return false
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	rec := recordedLiterals(mock)
	if rec == nil {
		return false
	}
	clocks, why := (*fn)(g.ctx, LiteralCall{MockDb: g.mockDb, Recorded: mock, Live: liveLiterals(g.live), Rec: rec, Drift: paths, Arrived: call.arrived})
	for _, p := range paths {
		if _, ok := clocks[p]; !ok {
			if g.clockWhy == "" {
				g.clockWhy = why
			}
			return false
		}
	}
	if g.learned == nil {
		g.learned = make(map[*models.Mock]map[string]models.ClockOffset)
	}
	learned := make(map[string]models.ClockOffset, len(paths))
	for _, p := range paths {
		learned[p] = clocks[p]
	}
	g.learned[mock] = learned
	return true
}

// withoutClocks is noise with every literal of clocks as known noise, for
// detection: a place learned as a read of the application's clock is
// checked as one, never learned as noise.
func withoutClocks(noise map[string][]string, clocks ...map[string]models.ClockOffset) map[string][]string {
	n := 0
	for _, c := range clocks {
		n += len(c)
	}
	if n == 0 {
		return noise
	}
	out := make(map[string][]string, len(noise)+n)
	maps.Copy(out, noise)
	for _, c := range clocks {
		for p := range c {
			out[strings.TrimPrefix(p, "body.")] = []string{}
		}
	}
	return out
}

// liveLiterals are the inline literals of the live COM_QUERY b as learn
// leaves by path; nil for any other command or a statement vitess cannot
// parse.
func liveLiterals(b *mysql.PacketBundle) map[string]learn.Leaf {
	if b == nil {
		return nil
	}
	qp, ok := b.Message.(*mysql.QueryPacket)
	if !ok || qp == nil {
		return nil
	}
	return literalLeaves(qp.Query)
}

// recordedLiterals are liveLiterals of mock's recorded command.
func recordedLiterals(mock *models.Mock) map[string]learn.Leaf {
	if mock == nil || len(mock.Spec.MySQLRequests) != 1 {
		return nil
	}
	return liveLiterals(&mock.Spec.MySQLRequests[0].PacketBundle)
}

// literalLeaves are the inline literals of sql (extractQueryShape) as learn
// leaves by path: a quoted one as a string, a number as a number.
func literalLeaves(sql string) map[string]learn.Leaf {
	shape := extractQueryShape(sql)
	if !shape.ok {
		return nil
	}
	out := make(map[string]learn.Leaf, len(shape.literals))
	for i, v := range shape.literals {
		p := literalPrefix + strconv.Itoa(i)
		typ := learn.String
		if i < len(shape.numeric) && shape.numeric[i] {
			typ = learn.Number
		}
		out[p] = learn.Leaf{Path: p, Type: typ, Value: v}
	}
	return out
}
